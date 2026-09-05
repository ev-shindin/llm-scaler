#!/usr/bin/env python3
"""Extract the V2 saturation analyzer's internal k1/k2 capacity decisions from
the controller logs within a given results dir's run window, and render a
markdown report of which fallback tier fired, when, and why.

Reads eight log lines:
  - k2-decision                    (saturation_v2) per replica, per cycle:
                                    which of the four k2 priority tiers fired
                                    (observed / historical / derived /
                                    fallback-to-k1)
  - replica-capacity-decision      (saturation_v2) per replica, per cycle:
                                    k1 vs k2, which bound won, demand/queue
                                    inputs
  - replica-capacity-skipped       (saturation_v2) vllm:cache_config_info is
                                    absent AND no capacity-store record covers
                                    the replica: it contributes no capacity
  - replica-capacity-store-fallback
                                    (saturation_v2) vllm:cache_config_info is
                                    absent but a capacity-store record does
                                    cover the replica
  - variant-capacity-source        (saturation_v2) zero-replica variant:
                                    compatible-variant borrow or no-data
  - zero-replica-capacity-estimate (saturation_v2) zero-replica variant:
                                    live / derived / stored-fallback estimate
  - scheduler-queue-demand         (saturation_v2) per model, per cycle: the
                                    EPP flow-control queue's token demand
  - Applied saturation decision via shared cache
                                    (steadystate) the actual, post-enforcement
                                    target replica count for the variant this
                                    cycle

k2-decision, replica-capacity-decision and replica-capacity-store-fallback are
logged at V(logging.DEFAULT), which is the verbosity the shipped deployment
runs at (cmd/main.go defaults -v to logging.DEFAULT). Started with -v=1 or
lower, the controller suppresses them and this report comes back empty.
replica-capacity-skipped is at Info and survives any -v, since it reports a
replica contributing no capacity at all rather than a routine decision.

Output:
  metrics/processed/k2_decisions.json   (raw per-event records)
  metrics/reports/k2_decision_report.md (human-readable summary)

Usage
-----
  python hack/benchmark/dump_k2_decisions.py \
      <results>/<treatment>_<i> -n NAMESPACE
"""
import argparse
import json
import re
import subprocess
import sys
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path

try:
    import yaml
except ImportError:
    print("ERROR: PyYAML required. pip install pyyaml", file=sys.stderr)
    sys.exit(1)

# Tab-delimited zap console encoding: ts, level, caller, message, json-fields.
# Matched on message name only (not file:line) — see dump_wva_target_timeseries.py
# for why pinning the caller path is the wrong thing to do here. The message
# itself is matched up to the next tab rather than restricted to
# lowercase-hyphen text, since "Applied saturation decision via shared cache"
# is a plain sentence, not one of this package's own kebab-case message names.
LOG_LINE = re.compile(
    r'^(?P<ts>\S+)\t\S+\t\S+\t(?P<msg>[^\t]+)\t(?P<json>\{.*\})$'
)

DECISION_MSG = "Applied saturation decision via shared cache"
K2_MSG = "k2-decision"
RC_MSG = "replica-capacity-decision"
SQ_MSG = "scheduler-queue-demand"
# The arrival-rate demand floor. FLOOR_MSG fires only when the floor actually
# raises demand, so its presence in a dump marks the cycles where the scaling
# decision came from the offered load rather than from measured occupancy --
# which is the first thing to check when a target looks higher than the fleet
# appears to warrant. FLOOR_NA_MSG is its counterpart: the floor could not be
# computed at all, and names the input that was missing.
FLOOR_MSG = "arrival-demand-floor"
FLOOR_NA_MSG = "arrival-demand-floor unavailable"

MESSAGES = {
    K2_MSG,
    RC_MSG,
    SQ_MSG,
    FLOOR_MSG,
    FLOOR_NA_MSG,
    "replica-capacity-skipped",
    "replica-capacity-store-fallback",
    "variant-capacity-source",
    "zero-replica-capacity-estimate",
    DECISION_MSG,
}

# Cycle-clustering window, in seconds. See assign_cycles(). The default sits
# comfortably below the shipped GLOBAL_OPT_INTERVAL (15s) and above the spread
# of a single cycle's own log lines; resolve_cycles() narrows it when the run
# used a shorter interval.
DEFAULT_CYCLE_GAP = 3.0

# The floor resolve_cycles() will not narrow past. Log timestamps are
# second-granularity, so below 1s a cycle whose lines straddle a second boundary
# gets split apart again -- the exact bug the clustering exists to prevent.
MIN_CYCLE_GAP = 1.0

# Messages that occur at most once per optimize cycle, mapped to the field tuple
# naming the thing they are "once per". A repeat inside one cluster proves the
# clustering merged several cycles. k2-decision is absent on purpose: it is per
# replica but carries no pod field, so two replicas of one variant legitimately
# produce two indistinguishable events in a single cycle.
CYCLE_UNIQUE_KEYS = {
    RC_MSG: ("variant", "pod"),
    "replica-capacity-skipped": ("variant", "pod"),
    "replica-capacity-store-fallback": ("variant", "pod"),
    SQ_MSG: ("modelID",),
    "variant-capacity-source": ("variant",),
    "zero-replica-capacity-estimate": ("variant",),
    DECISION_MSG: ("variant",),
}


def md_table(headers, rows):
    """Renders a markdown table with every column padded to its widest cell,
    so the raw .md source lines up visually in a plain-text viewer, not just
    when rendered. Returns a list of lines to extend into the report."""
    str_rows = [[str(c) for c in row] for row in rows]
    widths = [len(h) for h in headers]
    for row in str_rows:
        for i, cell in enumerate(row):
            widths[i] = max(widths[i], len(cell))

    def fmt_row(cells):
        return "| " + " | ".join(cell.ljust(w) for cell, w in zip(cells, widths)) + " |"

    lines = [fmt_row(headers), "|" + "|".join("-" * (w + 2) for w in widths) + "|"]
    lines.extend(fmt_row(row) for row in str_rows)
    return lines


# Legend for the abbreviated codes in the detail table's "Bound" and "Decision"
# columns — kept short so each row fits on one line.
BOUND_LEGEND = "k1=memory-bound won, k2=compute-bound won"
DECISION_LEGEND = "DN = the controller decided N replicas (post scale-to-zero/min-replica enforcement)"


def format_bound(bound_by):
    return {"k1-memory": "k1", "k2-compute": "k2"}.get(bound_by, bound_by)


def variant_role(variant):
    """"decode" or "prefill" from a variant name like
    "qwen-qwe-...-decode-wva", or None if neither appears (e.g. an
    aggregated/non-disaggregated deployment). Used to pick this variant's own
    figure out of scheduler-queue-demand's per-role breakdown rather than the
    pool-wide one, which is always the decode leg's number (see build_cycle_row)."""
    if "decode" in variant:
        return "decode"
    if "prefill" in variant:
        return "prefill"
    return None


def parse_iso(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


def assign_cycles(events, gap_seconds):
    """Groups timestamp-sorted events into optimize cycles, returning a list of
    per-cycle event lists.

    The controller stamps logs at whole-second precision (controller-runtime's
    RFC3339 time encoder), and one optimize cycle emits all of its lines inside
    a fraction of a second. Joining on exact timestamp equality therefore
    usually works — and silently produces garbage when it does not: a cycle
    that happens to straddle a second boundary splits into two rows, each with
    N halved, k1/k2 blank, demand totals split, and no applied decision. In the
    report that is indistinguishable from a real half-idle cycle.

    Clustering on the gap between consecutive events removes the boundary
    sensitivity: inside a cycle that gap is 0-1s, between cycles it is the
    optimize interval (15s by default), so any threshold between the two
    separates them cleanly.
    """
    cycles = []
    prev = None
    for e in events:
        ts = parse_iso(e["_ts"])
        if prev is None or (ts - prev).total_seconds() > gap_seconds:
            cycles.append([])
        cycles[-1].append(e)
        prev = ts
    return cycles


def cycles_merged(cycles):
    """True if any cluster holds two events that can only happen once per
    optimize cycle -- proof the clustering gap exceeded the optimize interval.

    Keying on every once-per-cycle message rather than on scheduler-queue-demand
    alone is what makes this work at all: that line is only logged when
    input.SchedulerQueue is non-nil, and the collector returns nil whenever the
    EPP flow-control metrics are absent. On a cluster without them the queue
    line never appears, so a detector keyed on it could never fire.
    """
    for cycle in cycles:
        counts = Counter(
            (e["_msg"],) + tuple(e.get(f) for f in CYCLE_UNIQUE_KEYS[e["_msg"]])
            for e in cycle if e["_msg"] in CYCLE_UNIQUE_KEYS
        )
        if counts and max(counts.values()) > 1:
            return True
    return False


def resolve_cycles(events, gap_seconds):
    """Clusters events into optimize cycles, narrowing the gap until no cluster
    holds two events that can only happen once per cycle.

    GLOBAL_OPT_INTERVAL ships at 15s but may legally go as low as 1s
    (config.MinOptimizationInterval), so no single gap is right for every
    cluster and a fixed default would silently merge cycles on a fast one.
    Narrowing stops at MIN_CYCLE_GAP; at a 1s interval the cycles are simply not
    separable at this timestamp resolution, and that is reported rather than
    papered over.
    """
    gap = gap_seconds
    cycles = assign_cycles(events, gap)
    while cycles_merged(cycles) and gap > MIN_CYCLE_GAP:
        gap = max(MIN_CYCLE_GAP, gap / 2.0)
        cycles = assign_cycles(events, gap)

    if gap != gap_seconds:
        print(f"NOTE: --cycle-gap={gap_seconds}s spanned more than one optimize cycle; "
              f"narrowed to {gap}s.", file=sys.stderr)
    if cycles_merged(cycles):
        print(f"WARNING: even --cycle-gap={gap}s cannot separate this run's optimize cycles "
              "(GLOBAL_OPT_INTERVAL at or below the 1s timestamp resolution?). Each row below "
              "may cover more than one cycle.", file=sys.stderr)
    return cycles


def build_cycle_row(cycle, variant):
    """Aggregates one optimize cycle's events into a single row for `variant`,
    totalled across every replica of it that reported that cycle — not one row
    per replica. Returns None when this variant did not report in this cycle."""
    rcs = [e for e in cycle if e["_msg"] == RC_MSG and e.get("variant") == variant]
    k2s = [e for e in cycle if e["_msg"] == K2_MSG and e.get("variant") == variant]
    if not rcs and not k2s:
        return None

    # scheduler-queue-demand is model-level, not per-variant, so join it on the
    # modelID this variant's own events reported rather than on whichever queue
    # event happens to share the cycle. With several models in one log window
    # that is the difference between this model's number and another model's.
    model_ids = {e.get("modelID") for e in rcs + k2s}
    sq = next((e for e in cycle
               if e["_msg"] == SQ_MSG and e.get("modelID") in model_ids), None)

    # The applied-decision line's "variant" field is "namespace/name" (a
    # composite cache key — see steadystate/engine.go), while every
    # saturation_v2 event uses the bare name. Strip the namespace prefix so
    # both sides join on the same key.
    decision = next((e for e in cycle
                     if e["_msg"] == DECISION_MSG
                     and e.get("variant", "").rsplit("/", 1)[-1] == variant), None)

    priorities = [k.get("priority", "?") for k in k2s]
    # k1 and k2 are shared across replicas of one variant unless their
    # history-bucket key differs (rare within one cycle) — show the most
    # common value rather than every replica's copy.
    k1_common = Counter(r.get("k1MemoryBound") for r in rcs).most_common(1)
    k2_common = Counter(r.get("k2ComputeBound") for r in rcs).most_common(1)
    bound_counts = Counter(format_bound(r.get("boundBy", "?")) for r in rcs)

    # scheduler-queue-demand's top-level estimatedTokens is the whole model's
    # flow-control queue, which in a P/D split is dominated by one leg (in
    # practice always equal to byRole.decode) -- applying it to BOTH the
    # prefill and decode tables silently double-counts the same tokens into
    # each variant's own EPPq/TotalDemand as if each owned that backlog
    # independently. byRole carries the real per-leg split when present.
    role = variant_role(variant)
    by_role = (sq.get("byRole") or {}) if sq else {}
    if role is not None and role in by_role:
        epp_queue = by_role[role] or 0
    else:
        epp_queue = (sq.get("estimatedTokens") if sq else 0) or 0
    replica_demand_total = sum(r.get("replicaDemand", 0) or 0 for r in rcs)
    ts = min(e["_ts"] for e in rcs + k2s)
    target = decision.get("target") if decision else None

    return {
        "ts": ts,
        "time_short": ts.split("T")[1].split("+")[0] if "T" in ts else ts,
        "n": max(len(rcs), len(k2s)),
        "priority_label": ",".join(sorted(set(priorities))) if priorities else "?",
        "k1": k1_common[0][0] if k1_common else "?",
        "k2": k2_common[0][0] if k2_common else "?",
        "bound_label": ",".join(sorted(bound_counts)) if bound_counts else "?",
        "tokens_in_use": sum(r.get("tokensInUse", 0) or 0 for r in rcs),
        "local_queue": sum(r.get("localQueueDemand", 0) or 0 for r in rcs),
        "epp_queue": epp_queue,
        "total_demand": replica_demand_total + epp_queue,
        "decision": f"D{target}" if target is not None else "?",
    }


def load_workload_shape_line(results_dir, run_meta):
    """One-line workload summary ("6000/1000 → 1000/4000 in/out tokens   |
    12m/phase   |   RPS 4 throughout") for a trace-replay shape-swap run, or
    None for anything else (this repo's other scenario shapes, or a replay
    run whose *.params.yaml can't be found).

    Mirrors plot_two_variant_pipeline.py's _load_experiment_metadata /
    _format_workload_line -- kept as an independent copy rather than a
    shared import, matching this directory's existing convention of
    self-contained dump/plot scripts. If you change one, change the other.
    """
    workload_name = run_meta.get("harness_workload")
    if not workload_name:
        return None
    # guidellm doesn't copy its rendered profile into results/<treatment>_<i>/
    # (only inference-perf does) -- it stays at
    # <run_dir>/workload/profiles/<harness>/<name>, one level above results_dir.
    candidates = [results_dir / workload_name]
    harness_name = run_meta.get("harness_name")
    if harness_name:
        candidates.append(
            results_dir.parent.parent / "workload" / "profiles" / harness_name / workload_name
        )
    workload = None
    for wp in candidates:
        if wp.is_file():
            try:
                workload = yaml.safe_load(wp.read_text()) or {}
            except Exception:
                workload = None
            break
    if not workload or "spec" not in workload:
        return None
    profile = (workload["spec"].get("profile") or {})
    if profile.get("kind") != "replay":
        return None

    data0 = (workload["spec"].get("data") or [{}])[0]
    stem = Path(data0.get("path", "")).stem
    if not stem:
        return None
    params_path = (
        Path(__file__).resolve().parent / ".." / ".." /
        "test" / "benchmark" / "scenarios" / f"{stem}.params.yaml"
    ).resolve()
    if not params_path.is_file():
        return None
    try:
        params = yaml.safe_load(params_path.read_text()) or {}
    except Exception:
        return None

    phases = [{
        "rate": p.get("rate_rps"),
        "duration": p.get("duration_s"),
        "input_tokens": (p.get("input_tokens") or {}).get("mean"),
        "output_tokens": (p.get("output_tokens") or {}).get("mean"),
    } for p in params.get("phases") or []]
    if not phases:
        return None

    parts = []
    shape_str = " → ".join(
        f"{p['input_tokens']:g}/{p['output_tokens']:g}"
        for p in phases if p.get("input_tokens") is not None
    )
    if shape_str:
        parts.append(f"{shape_str} in/out tokens")
    durations = {p["duration"] for p in phases if p.get("duration")}
    if len(durations) == 1:
        d = durations.pop()
        parts.append(f"{d / 60:g}m/phase" if d >= 60 else f"{d:g}s/phase")
    rates = {p["rate"] for p in phases if p.get("rate") is not None}
    if len(rates) == 1:
        parts.append(f"RPS {rates.pop():g} throughout")
    elif rates:
        parts.append("RPS " + " → ".join(f"{p.get('rate'):g}" for p in phases))
    return "   |   ".join(parts) if parts else None


def fetch_logs(namespace, since_seconds, results_dir=None):
    """Returns the controller's logs, or exits non-zero.

    Prefers a captured tail file (metrics/../wva_controller.log, written by
    `make benchmark-run` via tail_wva_logs.sh from the moment the run
    started) over a live `kubectl logs` call: the controller's own log
    buffer is bounded by kubelet's fixed per-container rotation size, not by
    run length, so anything past the first few minutes is commonly gone by
    the time this runs after the fact. The captured file has no such limit.

    A kubectl failure — expired token, wrong namespace, no matching pods —
    must not reach the report as an empty log window; that reads as "the
    controller never logged anything" and sends whoever holds the report off
    to check the image.
    """
    if results_dir is not None:
        captured = Path(results_dir) / "wva_controller.log"
        if captured.is_file():
            text = captured.read_text()
            if text.strip():
                print(f"Using captured log tail: {captured}", file=sys.stderr)
                return text
            print(f"{captured} exists but is empty — falling back to live kubectl logs.",
                  file=sys.stderr)

    cmd = ["kubectl", "logs", "-n", namespace,
           "-l", "app.kubernetes.io/name=workload-variant-autoscaler",
           f"--since={since_seconds}s", "--tail=200000"]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        print(f"ERROR: {' '.join(cmd)} failed (exit {proc.returncode}):", file=sys.stderr)
        print(proc.stderr.strip(), file=sys.stderr)
        sys.exit(1)
    if not proc.stdout.strip():
        print(f"ERROR: no controller logs in namespace {namespace!r} over the last "
              f"{since_seconds}s. Check the -n namespace and that the "
              "workload-variant-autoscaler pod is running.", file=sys.stderr)
        sys.exit(1)
    return proc.stdout


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir", help="Path to .../results/<treatment>_<i>")
    ap.add_argument("-n", "--namespace", required=True)
    ap.add_argument("--cycle-gap", type=float, default=DEFAULT_CYCLE_GAP,
                    help="Seconds of silence separating two optimize cycles. Narrowed "
                         f"automatically (down to {MIN_CYCLE_GAP}s) if it turns out to span more "
                         f"than one. Default: {DEFAULT_CYCLE_GAP}")
    args = ap.parse_args()

    if args.cycle_gap < MIN_CYCLE_GAP:
        print(f"ERROR: --cycle-gap must be at least {MIN_CYCLE_GAP}s. Below that, a cycle whose "
              "log lines straddle a second boundary is split into separate rows.",
              file=sys.stderr)
        sys.exit(1)

    rd = Path(args.results_dir).resolve()
    meta_path = rd / "run_metadata.yaml"
    if not meta_path.is_file():
        print(f"ERROR: run_metadata.yaml not found in {rd}", file=sys.stderr)
        sys.exit(1)
    meta = yaml.safe_load(meta_path.read_text())

    start = parse_iso(meta["harness_start"])
    stop = parse_iso(meta["harness_stop"])

    now = datetime.now(timezone.utc)
    since_seconds = int((now - start).total_seconds()) + 90

    logs = fetch_logs(args.namespace, since_seconds, results_dir=rd)

    events = []
    for line in logs.splitlines():
        m = LOG_LINE.match(line)
        if not m or m.group("msg") not in MESSAGES:
            continue
        try:
            ts_dt = parse_iso(m.group("ts"))
            if ts_dt < start or ts_dt > stop:
                continue
            d = json.loads(m.group("json"))
        except (ValueError, json.JSONDecodeError):
            continue
        d["_msg"] = m.group("msg")
        d["_ts"] = ts_dt.isoformat()
        events.append(d)

    events.sort(key=lambda e: e["_ts"])

    processed_dir = rd / "metrics" / "processed"
    processed_dir.mkdir(parents=True, exist_ok=True)
    json_out = processed_dir / "k2_decisions.json"
    json_out.write_text(json.dumps({"events": events}, indent=2))

    workload_line = load_workload_shape_line(rd, meta)
    report = render_report(events, start, stop, args.cycle_gap, workload_line)
    reports_dir = rd / "metrics" / "reports"
    reports_dir.mkdir(parents=True, exist_ok=True)
    md_out = reports_dir / "k2_decision_report.md"
    md_out.write_text(report)

    print(f"Wrote {json_out} ({len(events)} events)")
    print(f"Wrote {md_out}")


def render_report(events, start, stop, cycle_gap, workload_line=None):
    lines = []
    lines.append("# K1/K2 Capacity Decision Report")
    lines.append("")
    if workload_line:
        lines.append(workload_line)
    lines.append(f"Window: {start.isoformat()} -> {stop.isoformat()}")
    lines.append(f"Total events captured: {len(events)}")
    lines.append("")

    k2_events = [e for e in events if e["_msg"] == K2_MSG]
    variants = sorted({e.get("variant", "?") for e in k2_events})

    cycles = resolve_cycles(events, cycle_gap)

    for variant in variants:
        rows = [r for r in (build_cycle_row(c, variant) for c in cycles) if r]
        if not rows:
            continue

        lines.append(f"## Variant: {variant}")
        lines.append("")
        lines.append("One row per optimize cycle, totalled across every ready replica of this "
                     "variant that cycle (N). KVinUse/LocalQ/EPPq/TotalDemand are all in tokens; "
                     "Priority lists every k2 tier that fired across N replicas this cycle "
                     "(P1-obs=observed, P2-hist=historical average, P3-k2=derived from deployment "
                     "args, P4-k1=no signal, memory-bound only). Time is HH:MM:SS on the run date "
                     "above.")
        lines.append("")
        lines.append(f"Legend — Bound: {BOUND_LEGEND}.  Decision: {DECISION_LEGEND}.")
        lines.append("")

        detail_rows = [[
            r["time_short"], r["n"], r["priority_label"], r["k2"], r["k1"], r["bound_label"],
            r["tokens_in_use"], r["local_queue"], r["epp_queue"], r["total_demand"], r["decision"],
        ] for r in rows]
        lines.extend(md_table(
            ["Time", "N", "Priority", "k2", "k1", "Bound", "KVinUse", "LocalQ", "EPPq",
             "TotalDemand", "Decision"],
            detail_rows))
        lines.append("")

    if not k2_events:
        lines.append("_No k1/k2 decision events found in the run window. The two per-replica "
                     "lines are logged at V(logging.DEFAULT): check that the controller was not "
                     "started with -v=1 or lower, and that its image includes the k1/k2 "
                     "logging._")
        lines.append("")

    return "\n".join(lines)


if __name__ == "__main__":
    main()
