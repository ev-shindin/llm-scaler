#!/usr/bin/env python3
"""
two_model_report.py -- compare the pool and nopool arms of the two-model
anti-phase run.

It reports four things, and the last two decide the question.

  1. TTFT over the whole run, per model. Least interesting: most of a run is
     steady state, where a pool does nothing but hold accelerators, so a pool
     that works well shows up here only faintly.
  2. TTFT in each RISE WINDOW, one row per rise rather than pooled. There are
     only two rises per model per arm, and each one is a SINGLE scale-up event:
     the hundreds of requests inside it are not independent samples of anything.
     Pooling them into one percentile produces a number that looks like n=700
     and carries the weight of n=1. Printed separately, with counts, so the
     reader can see how thin the evidence is.
  3. Accelerator-seconds, INCLUDING the pool's own. A pool arm that wins on
     TTFT while holding extra accelerators for the whole run has not won; it
     has spent.
  4. Whether the comparison is admissible at all -- same schedule, every
     arrival issued, and the DRIVER's own queueing small enough not to be the
     number being compared.

Refusals rather than a table, where the run cannot support one. The arms are
separated by tens of minutes and an edit in between is easy; a table that
silently compares a 2-cycle run against a 3-cycle one is worse than no table.
"""

import argparse
import json
import math
import os
import sys


def load_rows(path):
    rows = []
    with open(path) as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                rows.append(json.loads(line))
            except ValueError:
                continue
    return rows


def load_meta(d):
    p = os.path.join(d, "meta.json")
    if not os.path.exists(p):
        return None
    with open(p) as fh:
        return json.load(fh)


def pct(values, q):
    """Nearest-rank percentile: the smallest value at or above q% of the set.

    `ceil`, not `round(x + 0.5)`. The latter hits banker's rounding whenever
    q/100*n lands on an odd integer and returns one rank too HIGH -- measured:
    n=2 p50, n=20 p95, n=100 p95 and p99 all came back a rank high, which
    inflates both arms and not by the same amount.
    """
    if not values:
        return None
    s = sorted(values)
    k = max(0, min(len(s) - 1, int(math.ceil(q / 100.0 * len(s))) - 1))
    return s[k]


def fmt_ms(v):
    return "-" if v is None else "%.0f" % (v * 1000.0)


def rise_windows(schedule, model_key, window):
    """(start, end) for each phase where THIS model's rate went up.

    Keyed on the rate rising rather than on the phase index, so a schedule with
    a different lead-in or an odd cycle count still marks the right windows --
    and a model that never rises yields nothing rather than silently marking
    phase 1.
    """
    out = []
    prev = None
    key = "rate_a" if model_key == "a" else "rate_b"
    for ph in schedule:
        rate = ph[key]
        if prev is not None and rate > prev:
            out.append((ph["start"], min(ph["start"] + window, ph["end"])))
        prev = rate
    return out


def served_and_failed(rows, model_key):
    served = [r for r in rows if r["model"] == model_key and r.get("ttft") is not None
              and not r.get("error")]
    failed = [r for r in rows if r["model"] == model_key
              and (r.get("ttft") is None or r.get("error"))]
    return served, failed


def summarize(rows, model_key, lo=None, hi=None):
    served, failed = served_and_failed(rows, model_key)
    if lo is not None:
        served = [r for r in served if lo <= r["t_sched"] < hi]
        failed = [r for r in failed if lo <= r["t_sched"] < hi]
    t = [r["ttft"] for r in served]
    return {
        "n": len(served), "failed": len(failed),
        "p50": pct(t, 50), "p95": pct(t, 95), "p99": pct(t, 99),
        "max": max(t) if t else None,
    }


def failure_kinds(rows, model_key):
    kinds = {}
    _, failed = served_and_failed(rows, model_key)
    for r in failed:
        e = (r.get("error") or "unknown").split(":")[0].split(" ")[0]
        kinds[e] = kinds.get(e, 0) + 1
    return kinds


def gpu_series(path, t0):
    """[(t_offset, total, pool, lent)] from the sampler, aligned to the run.

    Samples carry an absolute epoch `ts`; the offset is computed here against
    the loader's own t0. Aligning in the sampler instead would require it to
    know when the load Pod actually started, which it cannot.
    """
    out = []
    if not os.path.exists(path):
        return out
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        try:
            s = json.loads(line)
        except ValueError:
            continue
        ts = s.get("ts")
        if ts is None:
            continue
        total = pool = lent = 0
        for p in s.get("pods", []):
            g = p.get("gpus") or 0
            total += g
            if p.get("pool"):
                pool += g
                # A pool Pod that has been LENT carries the borrowing model's
                # labels; an idle one does not. That is the lend, observed from
                # the Pod rather than from a counter that may not be scrapable
                # from where the driver runs.
                if p.get("model") or p.get("serving") == "true":
                    lent += g
        out.append((ts - t0, total, pool, lent))
    out.sort(key=lambda r: r[0])
    return out


def integrate(series, index, gap_limit=None):
    """Accelerator-seconds: each sample held until the next.

    Returns (total, seconds_spanned_by_gaps). A failed `kubectl get` writes no
    sample, so the previous one is forward-filled across the hole -- reported
    rather than hidden, because a long hole is a run nobody should quote.
    """
    if len(series) < 2:
        return None, 0.0
    total = 0.0
    gapped = 0.0
    for i in range(len(series) - 1):
        dt = series[i + 1][0] - series[i][0]
        if dt <= 0:
            continue
        if gap_limit and dt > gap_limit:
            gapped += dt
        total += series[i][index] * dt
    return total, gapped


def schedule_signature(meta):
    if not meta:
        return None
    return json.dumps({
        "schedule": meta.get("schedule"),
        "input_tokens": meta.get("input_tokens"),
        "output_tokens": meta.get("output_tokens"),
        "model_a": meta.get("model_a"),
        "model_b": meta.get("model_b"),
        "seed": meta.get("seed"),
    }, sort_keys=True)


def load_budget(d):
    p = os.path.join(d, "budget.json")
    if not os.path.exists(p):
        return None
    with open(p) as fh:
        return json.load(fh)


def budget_problem(a, b):
    """The arms must not differ in how much cluster they were allowed.

    The whole cost argument for a warm pool is that insurance LOWERS your
    maximum fleet. An arm that holds a pool AND the same per-model ceiling is
    not this pool being measured, it is a bigger cluster being measured -- and
    it wins on TTFT for a reason that has nothing to do with lending.
    """
    ba, bb = a.get("budget"), b.get("budget")
    if not ba or not bb:
        return ("neither arm recorded its replica budget (budget.json), so nothing "
                "establishes that the pool arm was not simply allowed more cluster")
    if bb.get("pool_replicas", 0) <= 0:
        return "the pool arm recorded no pool Pods; it was not the pool arm"
    peak_n = 2 * ba["max_replicas_per_model"] * ba["gpus_per_replica"]
    peak_p = (2 * bb["max_replicas_per_model"] * bb["gpus_per_replica"]
              + bb["pool_replicas"] * bb["gpus_per_replica"])
    if peak_p > peak_n:
        return ("the pool arm could reach %d accelerators against the nopool arm's %d. "
                "It was allowed more cluster, not just faster cluster."
                % (peak_p, peak_n))
    return None


def pod_work(d):
    """{pod: prompt tokens done during the arm}, from the before/after captures.

    Empty when the captures are missing, which is reported rather than assumed
    to be fine.
    """
    def read(name):
        p = os.path.join(d, name)
        out = {}
        if not os.path.exists(p):
            return None
        for line in open(p):
            parts = line.split()
            if len(parts) == 2:
                try:
                    out[parts[0]] = float(parts[1])
                except ValueError:
                    pass
        return out

    before, after = read("podwork.before"), read("podwork.after")
    if before is None or after is None:
        return None
    work = {}
    for pod, end in after.items():
        work[pod] = max(0.0, end - before.get(pod, 0.0))
    return work


def routing_problem(arm):
    """Did the load actually reach more than one replica?

    THE GUARD THAT WAS MISSING. Measured on CoreWeave: one Qwen replica did
    6,000,692 prompt tokens while every other replica the autoscaler added did
    ZERO -- including an awake, Ready warm-pool Pod that was in the EPP's own
    backend list. With one shared prompt prefix, llm-d's prefix-cache scorer is
    the highest-weighted scorer in the shipped profile, so the first replica to
    cache the prefix wins every later request regardless of its queue, and the
    others never receive one. Capacity cannot help when it is never used, and
    every TTFT and GPU number from such a run describes a one-replica fleet.
    """
    work = arm.get("work")
    if work is None:
        return ("the %s arm has no per-engine work capture, so nothing establishes that the "
                "load reached more than one replica" % arm["name"])
    busy = {p: v for p, v in work.items() if v > 0}
    if len(busy) <= 1 and len(work) > 1:
        top = max(work.items(), key=lambda kv: kv[1]) if work else ("?", 0)
        return ("in the %s arm only %d engine(s) did any work at all, out of %d seen: %s did "
                "%.0f prompt tokens and the rest did none. Added capacity was never routed to, "
                "so this run measures a one-replica fleet."
                % (arm["name"], len(busy), len(work), top[0], top[1]))
    total = sum(work.values())
    if total > 0 and len(busy) > 1:
        share = max(work.values()) / total
        if share > 0.95:
            return ("in the %s arm one engine did %.0f%% of all prompt tokens; the other "
                    "replicas were effectively unused, so added capacity cannot have affected "
                    "TTFT" % (arm["name"], 100 * share))
    return None


# Failures that happened on the DRIVER's side of the wire. Two families,
# because the load generator changed: the stdlib names the earlier bespoke
# loader raised, and the aiohttp ones inference-perf raises. A name missing
# from this list does not become a cluster failure -- it becomes a failure the
# guard cannot see, which is why the list is explicit and not a catch-all.
_CLIENT_SIDE = (
    # stdlib
    "gaierror", "ConnectionRefusedError", "ConnectionResetError", "OSError",
    "TimeoutError", "socket.timeout",
    # aiohttp / inference-perf
    "ClientConnectorError", "ClientConnectionError", "ClientOSError",
    "ServerDisconnectedError", "ServerTimeoutError", "ClientPayloadError",
    "asyncio.TimeoutError", "ConnectionTimeoutError",
)


def is_client_side(err):
    return bool(err) and str(err).startswith(_CLIENT_SIDE)


def driver_queueing(arm):
    """p95 of the load generator's own scheduling delay, in seconds.

    Two sources, in order of preference, because the generator changed and a
    stored run from either is still readable:

      1. `queue_delay_p95` in the meta -- what inference-perf measured about
         itself and reported in `load_summary.schedule_delay`.
      2. a per-row `queue_delay`, which the earlier bespoke loader recorded.

    Returns None when NEITHER exists. That is a refusal, not a zero: rows with
    no such field would otherwise percentile to 0.0 and clear the guard
    silently, which is the exact shape of the failure the guard is for.
    """
    meta = arm.get("meta") or {}
    reported = meta.get("queue_delay_p95")
    if isinstance(reported, (int, float)):
        return float(reported)
    measured = [r["queue_delay"] for r in arm["rows"] if "queue_delay" in r]
    if measured:
        return pct(measured, 95)
    return None


def admissible(a, b, max_queue_delay):
    """Everything that makes the two arms comparable. Returns a list of reasons."""
    problems = []
    for arm in (a, b):
        rp = routing_problem(arm)
        if rp:
            problems.append(rp)
    bp = budget_problem(a, b)
    if bp:
        problems.append(bp)
    if a["meta"] is None or b["meta"] is None:
        problems.append("one arm has no meta.json, so nothing can check that the two "
                        "runs used the same schedule")
        return problems
    if schedule_signature(a["meta"]) != schedule_signature(b["meta"]):
        diffs = []
        for k in ("input_tokens", "output_tokens", "model_a", "model_b", "seed"):
            if a["meta"].get(k) != b["meta"].get(k):
                diffs.append("%s: nopool=%s pool=%s" % (k, a["meta"].get(k), b["meta"].get(k)))
        if a["meta"].get("schedule") != b["meta"].get("schedule"):
            diffs.append("the schedule itself (phases, rates or durations)")
        problems.append("the arms did not run the same scenario -- " + "; ".join(diffs))
    for arm in (a, b):
        m = arm["meta"]
        if m.get("issued", 0) < m.get("planned", 0):
            problems.append("the %s arm issued %d of %d planned arrivals: the DRIVER was "
                            "the limit, not the cluster" % (arm["name"], m.get("issued"),
                                                            m.get("planned")))
        # CLIENT-SIDE network failures are the loader's, not the cluster's, and
        # a run thinned by them is not a measurement of anything. Measured here:
        # a new connection per request meant a DNS lookup per request, and at
        # ~14 rps for 34 minutes the cluster resolver gave out -- `gaierror` on
        # roughly HALF of all requests, in both arms. The table was produced
        # anyway, over the survivors, and looked like a result.
        rows = arm["rows"]
        netfail = sum(1 for r in rows if is_client_side(r.get("error")))
        arm["netfail"] = netfail
        if rows and netfail > 0.02 * len(rows):
            problems.append("the %s arm lost %d of %d requests to CLIENT-SIDE network errors "
                            "(%.0f%%) -- the loader's own DNS or sockets, not the models. "
                            "What is left is the survivors of that, not the scenario."
                            % (arm["name"], netfail, len(rows), 100.0 * netfail / len(rows)))
        qd = driver_queueing(arm)
        arm["queue_p95"] = qd
        if qd is None:
            # Not "probably fine". Every TTFT below contains the driver's own
            # delay, and with no measurement of it there is nothing that says
            # the difference between the arms is the cluster's.
            problems.append("the %s arm reports no driver queueing at all -- neither per "
                            "request nor in the generator's own summary -- so nothing "
                            "establishes that the latencies below are the cluster's"
                            % arm["name"])
        elif qd > max_queue_delay:
            problems.append("the %s arm's own queueing reached %.0f ms at p95 (limit %.0f): "
                            "that is the loader's latency, not the cluster's, and it is "
                            "inside every TTFT below"
                            % (arm["name"], qd * 1000, max_queue_delay * 1000))
    return problems


def main(argv):
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--nopool", required=True)
    p.add_argument("--pool", required=True)
    p.add_argument("--model-a", default="A")
    p.add_argument("--model-b", default="B")
    p.add_argument("--rise-window", type=int, default=90)
    p.add_argument("--max-queue-delay", type=float, default=0.25,
                   help="p95 driver queueing above which the arms are not comparable")
    p.add_argument("--gap-limit", type=float, default=30.0,
                   help="a GPU sampling hole longer than this is reported")
    args = p.parse_args(argv)

    arms = []
    for d, name in ((args.nopool, "nopool"), (args.pool, "pool")):
        meta = load_meta(d)
        arms.append({
            "name": name, "dir": d, "meta": meta,
            "rows": load_rows(os.path.join(d, "requests.jsonl")),
            "gpus": gpu_series(os.path.join(d, "gpus.jsonl"), (meta or {}).get("t0", 0)),
            "budget": load_budget(d),
            "work": pod_work(d),
            "queue_p95": None,
        })
    a, b = arms

    problems = admissible(a, b, args.max_queue_delay)
    if problems:
        print("ERROR: these two arms cannot be compared:", file=sys.stderr)
        for pr in problems:
            print("  - " + pr, file=sys.stderr)
        return 2

    schedule = a["meta"]["schedule"]
    windows = {"a": rise_windows(schedule, "a", args.rise_window),
               "b": rise_windows(schedule, "b", args.rise_window)}

    print("")
    print("# Two models, anti-phase bursts, warm pool on and off")
    print("")
    print("- model A: `%s`" % args.model_a)
    print("- model B: `%s`" % args.model_b)
    print("- schedule: %d phases, %ds total; rise window %ds"
          % (len(schedule), schedule[-1]["end"], args.rise_window))
    for k, label in (("a", args.model_a), ("b", args.model_b)):
        print("- %s rises at: %s" % (label,
              ", ".join("%ds" % s for s, _ in windows[k]) or "never"))
    print("- driver queueing (p95): nopool %s ms, pool %s ms"
          % (fmt_ms(a["queue_p95"]), fmt_ms(b["queue_p95"])))
    print("")

    print("## Time to first token, whole run (ms)")
    print("")
    print("| model | arm | served | failed | p50 | p95 | p99 | max |")
    print("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |")
    for key, label in (("a", args.model_a), ("b", args.model_b)):
        for arm in (a, b):
            s = summarize(arm["rows"], key)
            print("| %s | %s | %d | %d | %s | %s | %s | %s |" % (
                label, arm["name"], s["n"], s["failed"],
                fmt_ms(s["p50"]), fmt_ms(s["p95"]), fmt_ms(s["p99"]), fmt_ms(s["max"])))
    print("")
    kinds = {(k, arm["name"]): failure_kinds(arm["rows"], k)
             for k in ("a", "b") for arm in (a, b)}
    if any(kinds.values()):
        print("Failures by kind (a request that did not deliver every token it asked for "
              "is a failure, not a fast success -- counting torn streams as successes "
              "lets a saturating arm improve its own percentiles):")
        print("")
        for (k, armname), kk in sorted(kinds.items()):
            if kk:
                label = args.model_a if k == "a" else args.model_b
                print("- %s / %s: %s" % (label, armname,
                      ", ".join("%s=%d" % (n, c) for n, c in sorted(kk.items()))))
        print("")

    print("## Time to first token, per rise (ms)")
    print("")
    print("Each row is ONE scale-up event. The requests inside a window are not "
          "independent samples of it.")
    print("")
    print("| model | rise at | arm | served | failed | p50 | p95 | max |")
    print("| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: |")
    for key, label in (("a", args.model_a), ("b", args.model_b)):
        for lo, hi in windows[key]:
            for arm in (a, b):
                s = summarize(arm["rows"], key, lo, hi)
                print("| %s | %ds | %s | %d | %d | %s | %s | %s |" % (
                    label, lo, arm["name"], s["n"], s["failed"],
                    fmt_ms(s["p50"]), fmt_ms(s["p95"]), fmt_ms(s["max"])))
    print("")

    print("## Accelerators")
    print("")
    for arm in (a, b):
        bud = arm["budget"]
        if bud:
            print("- %s: each model capped at %d replicas, pool %d Pod(s) -- ceiling %d accelerators"
                  % (arm["name"], bud["max_replicas_per_model"], bud.get("pool_replicas", 0),
                     2 * bud["max_replicas_per_model"] * bud["gpus_per_replica"]
                     + bud.get("pool_replicas", 0) * bud["gpus_per_replica"]))
    print("")
    print("| arm | GPU-seconds (all) | of which pool | peak GPUs | pool lent (GPU-s) | sampling holes |")
    print("| --- | ---: | ---: | ---: | ---: | ---: |")
    totals = {}
    for arm in (a, b):
        ser = arm["gpus"]
        if len(ser) < 2:
            print("| %s | - | - | - | - | no samples |" % arm["name"])
            totals[arm["name"]] = None
            continue
        tot, gapped = integrate(ser, 1, args.gap_limit)
        poolgpu, _ = integrate(ser, 2)
        lent, _ = integrate(ser, 3)
        totals[arm["name"]] = tot
        print("| %s | %.0f | %.0f | %d | %.0f | %s |" % (
            arm["name"], tot, poolgpu, max(r[1] for r in ser), lent,
            "-" if gapped == 0 else "%.0fs" % gapped))
    print("")

    print("## What this says")
    print("")
    for key, label in (("a", args.model_a), ("b", args.model_b)):
        for lo, hi in windows[key]:
            sn = summarize(a["rows"], key, lo, hi)
            sp = summarize(b["rows"], key, lo, hi)
            if sn["p95"] is None or sp["p95"] is None:
                print("- **%s**, rise at %ds: one arm served nothing in the window." % (label, lo))
                continue
            delta = (sn["p95"] - sp["p95"]) * 1000.0
            print("- **%s**, rise at %ds: p95 TTFT %.0f ms %s with the pool "
                  "(%s -> %s ms; n=%d vs %d served, %d vs %d failed)."
                  % (label, lo, abs(delta), "lower" if delta > 0 else "higher",
                     fmt_ms(sn["p95"]), fmt_ms(sp["p95"]),
                     sn["n"], sp["n"], sn["failed"], sp["failed"]))
    gt_n, gt_p = totals.get("nopool"), totals.get("pool")
    if gt_n and gt_p:
        diff = gt_p - gt_n
        if diff > 0:
            print("- The pool arm spent **%.0f more GPU-seconds** (%.1f%%). Note this "
                  "EXCLUDES the pool's warm-up, which happens before the run starts, so "
                  "it understates the cost of holding one."
                  % (diff, 100.0 * diff / gt_n))
        else:
            print("- The pool arm spent **%.0f fewer GPU-seconds** (%.1f%%): each model "
                  "held less standing headroom because the pool covered its rises."
                  % (-diff, 100.0 * -diff / gt_n))
    else:
        print("- GPU-seconds are missing for at least one arm, so the cost side of this "
              "comparison is **not** established; the TTFT numbers alone do not settle it.")
    # The limit of the evidence, printed every time, because a table invites a
    # conclusion the sample size does not support.
    n_rises = len(windows["a"]) + len(windows["b"])
    print("")
    print("**How far this goes.** %d scale-up events per arm, one run each, no repetition "
          "and no confidence interval. A difference of the same order as the spread "
          "between the two rises of the same model is not a result. To claim a direction, "
          "repeat the pair and check that the sign is stable." % n_rises)
    print("")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
