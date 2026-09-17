#!/usr/bin/env python3
"""
two_model_report.py -- compare the arms of the two-model
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

     Every latency here is the LOAD GENERATOR's own per-stage distribution.
     inference-perf v0.6.1 records no per-request token times, so nothing can
     be re-cut into a window after the run -- which is why the profile makes
     each rise window a stage of its own. The report reads what the harness
     measured; it does not recompute it.
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


def rise_stages(schedule, model_key):
    """Indices of the stages where THIS model's rate went UP.

    Stages, not phases: inference-perf reports `time_to_first_token` per stage
    and nothing finer, and the profile cuts the opening `--rise-window` seconds
    of every phase into its own stage precisely so a rise IS one. The index is
    what addresses the harness's own distribution for that window.

    Keyed on the rate rising rather than on position, so a schedule with a
    different lead-in or an odd cycle count still marks the right stages -- and
    a model that never rises yields nothing rather than silently marking the
    first one.
    """
    out = []
    prev = None
    key = "rate_a" if model_key == "a" else "rate_b"
    for i, st in enumerate(schedule):
        rate = st[key]
        if prev is not None and rate > prev:
            out.append(i)
        prev = rate
    return out


_EMPTY_WINDOW = {"n": 0, "failed": 0, "p50": None, "p95": None, "p99": None, "max": None}


def window_of(arm, model_key, index=None):
    """The harness's own distribution for one window of one model.

    `index` None means the whole run. Returns None when the arm's meta carries
    no such window -- a refusal upstream, never a zero here.

    A window that IS present but missing a field reads as absent for that field
    and prints as `-`. A KeyError here would take down the whole report over one
    field of one window, after the run had already been paid for.
    """
    w = ((arm.get("meta") or {}).get("windows") or {}).get(model_key)
    if not w:
        return None
    if index is None:
        found = w.get("overall")
    else:
        stages = w.get("stages") or []
        found = stages[index] if 0 <= index < len(stages) else None
    if found is None:
        return None
    full = dict(_EMPTY_WINDOW)
    full.update(found)
    return full


def served_and_failed(rows, model_key):
    """Rows for one model, split on whether the request failed.

    NO LATENCY: inference-perf v0.6.1 records no per-request token times, so
    rows carry counts and error types only. Every TTFT in this report comes
    from the harness's own per-stage distributions -- see `window_of`.
    """
    served = [r for r in rows if r["model"] == model_key and not r.get("error")]
    failed = [r for r in rows if r["model"] == model_key and r.get("error")]
    return served, failed


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
        if ts is None or s.get("failed"):
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


def clip_series(series, t_end):
    """The samples inside [0, t_end], with the boundaries held exactly.

    The sampler starts before the start barrier and stops after the ladder,
    so the raw series carries ~7 minutes of the standing fleet at each end --
    and in the pool arm those minutes include the pool's Pods. MEASURED on
    the first valid A/B: the pool arm's series began 404s before the load, at
    4 accelerators against nopool's 2, which charged the pool ~800 GPU-s it
    did not spend during the run and inflated its excess from +39% to +44%.
    Accelerator-seconds are the run's, so the window is the schedule's.

    The sample straddling each boundary is carried to the boundary at its own
    value, so the integral covers exactly [0, t_end] with nothing dropped and
    nothing invented.
    """
    if not series or t_end is None:
        return series
    out = []
    prev = None
    for row in series:
        t = row[0]
        if t < 0:
            prev = row
            continue
        if t > t_end:
            break
        if not out and prev is not None and t > 0:
            out.append((0.0,) + tuple(prev[1:]))
        out.append(row)
        prev = row
    if out and out[-1][0] < t_end and prev is not None:
        out.append((float(t_end),) + tuple(prev[1:]))
    elif not out and prev is not None:
        # every sample was outside the window: hold the last one across it
        out = [(0.0,) + tuple(prev[1:]), (float(t_end),) + tuple(prev[1:])]
    return out


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
        # The dataset is part of the traffic: a shared_prefix arm against a
        # synthetic one compares a one-replica fleet with a spread one.
        "data": meta.get("data"),
    }, sort_keys=True)


def load_budget(d):
    p = os.path.join(d, "budget.json")
    if not os.path.exists(p):
        return None
    with open(p) as fh:
        return json.load(fh)


def budget_problem(a, b):
    """The arms must have been allowed the same per-model ceiling.

    Every model gets the same maxReplicaCount in every arm -- as much as it
    asks for, up to what the cluster can place -- and the pool sits ON TOP of
    that in the pool arm. The cost of the insurance is not imposed through the
    ceiling; it is MEASURED, in accelerator-seconds, which is the number the
    report prices each arm on. What this guard refuses is an arm whose models
    were capped differently from nopool's: that arm's TTFT differs for a reason
    that has nothing to do with the insurance under test.
    """
    ba, bb = a.get("budget"), b.get("budget")
    if not ba or not bb:
        return ("the %s arm or the nopool arm did not record its replica budget "
                "(budget.json), so nothing establishes that it was not simply allowed "
                "more cluster" % b["name"])
    if b["name"].startswith("pool") and bb.get("pool_replicas", 0) <= 0:
        return "the pool arm recorded no pool Pods; it was not the pool arm"
    if b["name"].startswith("floor") and bb.get("min_replicas_per_model", 0) <= ba.get("min_replicas_per_model", 1):
        return ("the floor arm's floor (%s per model) is no higher than the nopool "
                "arm's (%s); it was not the floor arm"
                % (bb.get("min_replicas_per_model"), ba.get("min_replicas_per_model", 1)))
    if (bb["max_replicas_per_model"] != ba["max_replicas_per_model"]
            or bb["gpus_per_replica"] != ba["gpus_per_replica"]):
        return ("the %s arm capped each model at %d replicas of %d accelerator(s) against "
                "the nopool arm's %d of %d: its models were allowed a different ceiling, "
                "so its TTFT differs for a reason that is not the insurance under test."
                % (b["name"], bb["max_replicas_per_model"], bb["gpus_per_replica"],
                   ba["max_replicas_per_model"], ba["gpus_per_replica"]))
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


def deployment_of(pod):
    """The Deployment a Pod belongs to: its name minus the ReplicaSet hash and
    the Pod suffix. `qwen-...-decode-74bd9cbc64-2hxzj` -> `qwen-...-decode`."""
    parts = pod.rsplit("-", 2)
    return parts[0] if len(parts) == 3 else pod


def routing_problem(arm):
    """Did the load actually reach the capacity each MODEL was given?

    PER MODEL, and that is the whole point. Pooling the two models hides the
    failure completely: measured on CoreWeave, the busiest engine held 48.9% of
    ALL traffic -- comfortably under any threshold worth setting -- while per
    model it held 99.0% and 98.7%. Every replica either arm added during that
    run served between 0.3% and 2.0%. Both arms were one-replica-per-model
    fleets wearing the aggregate as a disguise, and the report compared them.

    Pool Pods are judged apart. A lent Pod serves whichever model borrowed it,
    so it belongs to no one Deployment, and folding it into a model's group
    would credit that model with capacity it did not have.

    THE GUARD THAT WAS MISSING, AND THEN WAS MISSING AGAIN. llm-d's shipped
    scheduling profile weights the prefix-cache scorer highest, so the first
    replica to cache a prefix keeps winning regardless of its queue. Capacity
    cannot help when it is never used, and every TTFT and accelerator number
    from such a run describes a fleet that was never really built.
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

    groups = {}
    for pod, v in work.items():
        key = "the warm pool" if "warm-pool" in pod else deployment_of(pod)
        groups.setdefault(key, []).append((pod, v))

    for key, pods in sorted(groups.items()):
        if key == "the warm pool" or len(pods) < 2:
            continue
        total = sum(v for _, v in pods)
        if total <= 0:
            continue
        _, top = max(pods, key=lambda kv: kv[1])
        share = top / total
        if share > 0.95:
            return ("in the %s arm one engine of %s served %.1f%% of that MODEL's prompt "
                    "tokens across %d replicas -- the others were effectively unused. The "
                    "model scaled and the traffic did not follow, so its added capacity "
                    "cannot have affected TTFT."
                    % (arm["name"], key, 100 * share, len(pods)))
    return None


# Failures where NO RESPONSE WAS EVER RECEIVED. Two families, because the load
# generator changed: the stdlib exception names the earlier bespoke loader
# raised, and the labels inference-perf groups its failures under.
#
# The label form is deliberately about the OUTCOME rather than the blame. A
# "Connection Error" can be the driver's sockets or the cluster refusing, and
# at 6% either way the arm is not measuring what it claims -- so the guard
# fires on "the request never got a response", which is what can actually be
# told from the data.
#
# A name missing from this list does not become a cluster failure -- it becomes
# a failure the guard cannot see, which is why the list is explicit and not a
# catch-all.
_CLIENT_SIDE = (
    # stdlib
    "gaierror", "ConnectionRefusedError", "ConnectionResetError", "OSError",
    "TimeoutError", "socket.timeout",
    # aiohttp / inference-perf. `ClientConnector` is a PREFIX on purpose: the
    # family has DNS, SSL and certificate variants, and the one that actually
    # occurred was ClientConnectorDNSError -- which an exact
    # "ClientConnectorError" did not match, so 25 of 420 requests (6%, three
    # times the threshold) were charged to the cluster and the table printed.
    "ClientConnector", "ClientConnectionError", "ClientOSError",
    "ServerDisconnectedError", "ServerTimeoutError", "ClientPayloadError",
    "asyncio.TimeoutError", "ConnectionTimeoutError",
)


# inference-perf's own grouping labels for failures that never reached the
# model. Matched case-insensitively and by substring: these are display strings,
# not identifiers.
_NO_RESPONSE_LABELS = ("connection error", "timeout", "client error")


def is_client_side(err):
    if not err:
        return False
    text = str(err)
    if text.startswith(_CLIENT_SIDE):
        return True
    low = text.lower()
    return any(label in low for label in _NO_RESPONSE_LABELS)


def lost_requests(arm):
    """(count, {label: count}) of requests that never received a response.

    From the generator's own per-label failure breakdown when there is one --
    with per-request reporting off there are no rows to count, and counting
    zero would clear the guard silently. Falls back to the rows for a stored
    run from the earlier loader.
    """
    by_label = ((arm.get("meta") or {}).get("failures_by_label")) or {}
    if by_label:
        kinds = {}
        for labels in by_label.values():
            for label, n in labels.items():
                kinds[label] = kinds.get(label, 0) + n
        lost = sum(n for label, n in kinds.items() if is_client_side(label))
        return lost, kinds
    kinds = {}
    for r in arm["rows"]:
        err = r.get("error")
        if err:
            kinds[str(err).split(":")[0]] = kinds.get(str(err).split(":")[0], 0) + 1
    lost = sum(1 for r in arm["rows"] if is_client_side(r.get("error")))
    return lost, kinds


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


def shortfall(path, t0=None, t_end=None):
    """(samples, short_samples, worst, longest_short_seconds), inside the run.

    How far, and for how long CONTINUOUSLY, an arm fell short of the fleet its
    own Deployments asked for.

    The duration is what decides, not the fraction. A Deployment is short for as
    long as a new replica takes to boot, and on a healthy run with four
    scale-ups that is ~12% of the samples -- which a fraction-based threshold
    calls starvation. It is the opposite: that gap is the phenomenon this whole
    scenario exists to measure, and it is exactly what a warm pool bridges.
    Refusing it would refuse every run where the pool had anything to do.

    What is NOT normal is a gap that never closes. The run this guard was
    written for stayed short for THIRTY MINUTES.

    A Pod that is Pending holds no accelerator and serves nothing, so an arm
    whose Deployments scaled to 3 while one replica ran is measuring a
    one-replica fleet. Measured on CoreWeave exactly that: KEDA actuated
    correctly within four minutes of the first burst, four Pods went Pending and
    eight by the end, and the arm ran its whole schedule on one replica per
    model. The other arm, an hour later, got everything it asked for. Nothing
    compared the two fleets, so the report would have called the difference a
    warm-pool result.

    `budget.json` establishes what each arm was ALLOWED. This establishes what
    it actually got.
    """
    if not os.path.exists(path):
        return 0, 0, 0, 0.0
    total = short = worst = 0
    longest = 0.0
    run_start = None
    last_ts = None
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        try:
            s = json.loads(line)
        except ValueError:
            continue
        want = s.get("desired")
        if not want:
            continue
        # Only the run itself: the sampler also covers the preload and the
        # report-writing tail, when the fleet is standing still by design.
        if t0 is not None and s.get("ts") is not None:
            off = s["ts"] - t0
            if off < 0 or (t_end is not None and off > t_end):
                continue
        running = {}
        for p in s.get("pods", []):
            if p.get("pool"):
                continue
            for dep in want:
                if p.get("name", "").startswith(dep + "-"):
                    running[dep] = running.get(dep, 0) + 1
        gap = sum(max(0, n - running.get(dep, 0)) for dep, n in want.items())
        total += 1
        ts = s.get("ts")
        if gap > 0:
            short += 1
            worst = max(worst, gap)
            if run_start is None:
                run_start = ts
            if ts is not None and run_start is not None:
                longest = max(longest, ts - run_start)
        else:
            run_start = None
        last_ts = ts
    return total, short, worst, longest


def burst_overlap(arm):
    """Seconds during which BOTH models were at their high rate.

    THE thing an anti-phase run must not contain, measured directly rather than
    through a proxy. Each model's real wall-clock stage boundaries are
    reconstructed from its OWN per-stage elapsed times -- the two do not cross a
    boundary together, because a stage ends when its in-flight requests drain
    and the bursting model drains slower -- and the high-rate intervals are
    intersected.

    Cumulative drift was the previous measure and it overstates: measured on one
    run, 59s of drift but only 32s where the bursts truly overlapped, because
    part of the divergence is absorbed by the band exactly as intended. What
    matters is the intersection, not the offset.

    Returns None when the generator reported no per-stage timing.
    """
    meta = arm.get("meta") or {}
    schedule = meta.get("schedule") or []
    windows = meta.get("windows") or {}
    spans = {}
    for role in ("a", "b"):
        stages = (windows.get(role) or {}).get("stages") or []
        if len(stages) != len(schedule) or not stages:
            return None
        t = 0.0
        out = []
        for st, w in zip(schedule, stages):
            e = w.get("elapsed")
            if not isinstance(e, (int, float)):
                return None
            out.append((t, t + e, st["rate_a"] if role == "a" else st["rate_b"]))
            t += e
        spans[role] = out
    # PER MODEL. A single global max would stop seeing model B's bursts the
    # moment the two models are given different rates -- 9 and 8, say -- and the
    # overlap would silently read as zero for the very configuration that makes
    # the two models comparable.
    bursts = {}
    for role in ("a", "b"):
        rates = [r for _, _, r in spans[role]]
        if not rates:
            return None
        high = max(rates)
        if high <= min(rates):
            # This model never bursts, so it cannot overlap with anything.
            bursts[role] = []
            continue
        bursts[role] = [(s, e) for s, e, r in spans[role] if r == high]
    total = 0.0
    for s1, e1 in bursts["a"]:
        for s2, e2 in bursts["b"]:
            total += max(0.0, min(e1, e2) - max(s1, s2))
    return total


def phase_drift(arm):
    """How far the two models' stage boundaries diverge, in seconds.

    A stage ends when its in-flight requests drain, and the BURSTING model
    drains slower -- so the two ladders do not cross a boundary together, and
    the divergence changes sign as the burst moves from one model to the other.
    Measured on CoreWeave: per-stage drains of 4-16s, peak divergence 6.1s.

    That divergence is time when both models are bursting at once, which is the
    one thing an anti-phase run must not contain: a pool asked for two models
    simultaneously can serve one, and it would be recorded as the pool failing
    at the thing being measured. The overlap band exists to absorb it; this is
    what checks the band was big enough.

    Returns None when the generator reported no per-stage timing.
    """
    windows = ((arm.get("meta") or {}).get("windows")) or {}
    stages = {k: (windows.get(k) or {}).get("stages") or [] for k in ("a", "b")}
    if not stages["a"] or len(stages["a"]) != len(stages["b"]):
        return None
    worst = 0.0
    run = {"a": 0.0, "b": 0.0}
    for sa, sb in zip(stages["a"], stages["b"]):
        if not isinstance(sa.get("elapsed"), (int, float)) or \
           not isinstance(sb.get("elapsed"), (int, float)):
            return None
        run["a"] += sa["elapsed"]
        run["b"] += sb["elapsed"]
        worst = max(worst, abs(run["a"] - run["b"]))
    return worst


def admissible(arms, max_queue_delay, max_short=300.0, max_overlap=0.0,
               max_gap=0.05, args_gap_limit=30.0):
    """Everything that makes the arms comparable. Returns a list of reasons.

    arms[0] is the nopool baseline; every other arm is judged against it.
    """
    problems = []
    a = arms[0]
    for arm in arms:
        rp = routing_problem(arm)
        if rp:
            problems.append(rp)
    for b in arms[1:]:
        bp = budget_problem(a, b)
        if bp:
            problems.append(bp)
    if any(arm["meta"] is None for arm in arms):
        problems.append("an arm has no meta.json, so nothing can check that the "
                        "runs used the same schedule")
        return problems
    for b in arms[1:]:
        if schedule_signature(a["meta"]) != schedule_signature(b["meta"]):
            diffs = []
            for k in ("input_tokens", "output_tokens", "model_a", "model_b", "seed", "data"):
                if a["meta"].get(k) != b["meta"].get(k):
                    diffs.append("%s: nopool=%s %s=%s" % (k, a["meta"].get(k), b["name"], b["meta"].get(k)))
            if a["meta"].get("schedule") != b["meta"].get("schedule"):
                diffs.append("the schedule itself (phases, rates or durations)")
            problems.append("the nopool and %s arms did not run the same scenario -- %s"
                            % (b["name"], "; ".join(diffs)))
    for arm in arms:
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
        netfail, kinds = lost_requests(arm)
        arm["netfail"] = netfail
        arm["failure_kinds"] = kinds
        issued = m.get("issued") or len(arm["rows"])
        if issued and netfail > 0.02 * issued:
            problems.append("the %s arm lost %d of %d requests that never received a "
                            "response at all (%.0f%%) -- connection or timeout failures, "
                            "not answers from the models. What is left is the survivors "
                            "of that, not the scenario."
                            % (arm["name"], netfail, issued, 100.0 * netfail / issued))
        sched = m.get("schedule") or []
        total, short, worst, longest = shortfall(
            os.path.join(arm["dir"], "gpus.jsonl"), m.get("t0"),
            sched[-1]["end"] if sched else None)
        arm["short"] = (total, short, worst, longest)
        if longest > max_short:
            problems.append("the %s arm went %.0fs CONTINUOUSLY short of the fleet its own "
                            "Deployments asked for -- up to %d replica(s) Pending at once, "
                            "against a %.0fs allowance for a replica to boot. Pending "
                            "replicas hold no accelerator and serve nothing, so this arm "
                            "measured a smaller fleet than it was allowed."
                            % (arm["name"], longest, worst, max_short))
        # A SAMPLED SERIES WITH HOLES IS NOT A MEASUREMENT. Accelerator-seconds
        # are half of what this scenario reports -- insurance is a cost claim --
        # and they are integrated from the sampler, so a hole is silently priced
        # as whatever the sample before it held. Measured: one arm lost 587s of a
        # 2310s run to failed polls, came out at 1654 accelerator-seconds against
        # the other's 14284, and the report printed "the pool arm spent 763% more"
        # with a note about holes rather than refusing.
        ser = arm["gpus"]
        if len(ser) >= 2:
            span = ser[-1][0] - ser[0][0]
            _, gapped = integrate(ser, 1, args_gap_limit)
            arm["coverage_gap"] = (gapped, span)
            if span > 0 and gapped > max_gap * span:
                problems.append("the %s arm's accelerator sampling has %.0fs of holes in a "
                                "%.0fs window (%.0f%%). Accelerator-seconds are integrated "
                                "from those samples, so a hole is priced as whatever the "
                                "sample before it held -- and the cost half of this "
                                "comparison is what the pool is being judged on."
                                % (arm["name"], gapped, span, 100.0 * gapped / span))
        drift = phase_drift(arm)
        arm["drift"] = drift
        overlap = burst_overlap(arm)
        arm["overlap"] = overlap
        if overlap is not None and overlap > max_overlap:
            problems.append("in the %s arm BOTH models were bursting at once for %.0fs "
                            "(limit %.0fs). One pool covering many models is the claim "
                            "under test, and it rests on their peaks not coinciding -- a "
                            "pool asked for two models at once can serve one, and would "
                            "be recorded as failing. Raise OVERLAP_SECONDS above the "
                            "%.0fs the two ladders drifted apart."
                            % (arm["name"], overlap, max_overlap, drift or 0.0))
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


def arm_ceiling(bud):
    return (2 * bud["max_replicas_per_model"] * bud["gpus_per_replica"]
            + bud.get("pool_replicas", 0) * bud["gpus_per_replica"])


def main(argv):
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--nopool", required=True, help="the baseline arm's results directory")
    p.add_argument("--pool", help="the warm-pool arm's results directory")
    p.add_argument("--floor", help="the over-provisioned arm's results directory: no pool, "
                                   "every model held at a floor of replicas for the whole run")
    p.add_argument("--arm", action="append", default=[], metavar="NAME=DIR",
                   help="any further arm, e.g. pool1=<dir> for a differently sized pool. A "
                        "name starting with 'pool' is held to the pool arm's rules, one "
                        "starting with 'floor' to the floor arm's")
    p.add_argument("--model-a", default="A")
    p.add_argument("--model-b", default="B")
    p.add_argument("--max-queue-delay", type=float, default=0.25,
                   help="p95 driver queueing above which the arms are not comparable")
    p.add_argument("--max-shortfall", type=float, default=300.0,
                   help="seconds an arm may go CONTINUOUSLY short of the fleet its "
                        "Deployments asked for. A scale-up is short for as long as the "
                        "new replica takes to boot -- that gap is the phenomenon under "
                        "study, not a defect")
    p.add_argument("--max-overlap", type=float, default=0.0,
                   help="seconds both models may be bursting at once")
    p.add_argument("--max-sampling-gap", type=float, default=0.05,
                   help="fraction of an arm's window that may be missing from the "
                        "accelerator sampling before its GPU-seconds mean nothing")
    p.add_argument("--gap-limit", type=float, default=30.0,
                   help="a GPU sampling hole longer than this is reported")
    p.add_argument("--json", help="also write every number the tables hold to this file, "
                                  "for the plots and for anyone comparing runs")
    args = p.parse_args(argv)

    extra = []
    for spec in args.arm:
        if "=" not in spec:
            print("--arm takes NAME=DIR, got %r" % spec, file=sys.stderr)
            return 2
        name, d = spec.split("=", 1)
        if name in ("nopool", "pool", "floor") or not name:
            print("--arm name %r collides with a built-in arm; pick another" % name, file=sys.stderr)
            return 2
        extra.append((d, name))
    if not args.pool and not args.floor and not extra:
        print("nothing to compare nopool against: give --pool, --floor and/or --arm", file=sys.stderr)
        return 2
    arms = []
    for d, name in [(args.nopool, "nopool"), (args.pool, "pool"), (args.floor, "floor")] + extra:
        if not d:
            continue
        meta = load_meta(d)
        sched = (meta or {}).get("schedule") or []
        arms.append({
            "name": name, "dir": d, "meta": meta,
            "rows": load_rows(os.path.join(d, "requests.jsonl")),
            "gpus": clip_series(gpu_series(os.path.join(d, "gpus.jsonl"), (meta or {}).get("t0", 0)),
                                sched[-1]["end"] if sched else None),
            "budget": load_budget(d),
            "work": pod_work(d),
            "queue_p95": None,
        })
    a = arms[0]
    others = arms[1:]

    problems = admissible(arms, args.max_queue_delay, args.max_shortfall,
                          args.max_overlap, args.max_sampling_gap, args.gap_limit)
    if problems:
        print("ERROR: these arms cannot be compared:", file=sys.stderr)
        for pr in problems:
            print("  - " + pr, file=sys.stderr)
        return 2

    schedule = a["meta"]["schedule"]
    rises = {"a": rise_stages(schedule, "a"), "b": rise_stages(schedule, "b")}
    out = {"models": {"a": args.model_a, "b": args.model_b}, "schedule": schedule,
           "rises": rises, "arms": {}}

    print("")
    print("# Two models, anti-phase bursts: %s" % " vs ".join(arm["name"] for arm in arms))
    print("")
    print("- model A: `%s`" % args.model_a)
    print("- model B: `%s`" % args.model_b)
    print("- schedule: %d stages, %ds total. Every latency below is the load "
          "generator's OWN per-stage distribution -- this harness records no "
          "per-request token times, so a rise window has to BE a stage."
          % (len(schedule), schedule[-1]["end"]))
    for k, label in (("a", args.model_a), ("b", args.model_b)):
        print("- %s rises at: %s" % (label,
              ", ".join("%ds" % schedule[i]["start"] for i in rises[k]) or "never"))
    print("- driver queueing (p95): %s"
          % ", ".join("%s %s ms" % (arm["name"], fmt_ms(arm["queue_p95"])) for arm in arms))
    for arm in arms:
        tot, sh, wst, lng = arm.get("short", (0, 0, 0, 0.0))
        if tot:
            print("- %s: short of the requested fleet in %.0f%% of samples, longest "
                  "continuous stretch %.0fs%s -- a scale-up is short until the new "
                  "replica boots, which is the gap a pool exists to bridge"
                  % (arm["name"], 100.0 * sh / tot, lng,
                     " (up to %d Pending)" % wst if wst else ""))
    band = (a["meta"] or {}).get("overlap_seconds")
    for arm in arms:
        if arm.get("overlap") is not None:
            print("- %s: both models bursting at once for %.0fs" % (arm["name"], arm["overlap"]))
    print("- the two models drifted at most %s apart, against a %s band of both-low "
          "between bursts (a stage ends when its requests drain, and the bursting "
          "model drains slower)"
          % (" / ".join("%s %.0fs" % (arm["name"], arm["drift"])
                        if arm.get("drift") is not None else "%s -" % arm["name"]
                        for arm in arms),
             "%ds" % band if band is not None else "(unrecorded)"))
    print("")

    print("## Time to first token, whole run (ms)")
    print("")
    print("| model | arm | served | failed | p50 | p95 | p99 | max |")
    print("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |")
    for key, label in (("a", args.model_a), ("b", args.model_b)):
        for arm in arms:
            w = window_of(arm, key)
            rec = out["arms"].setdefault(arm["name"], {"whole": {}, "rises": {}})
            if w is None:
                print("| %s | %s | - | - | - | - | - | - |" % (label, arm["name"]))
                continue
            rec["whole"][key] = w
            print("| %s | %s | %d | %d | %s | %s | %s | %s |" % (
                label, arm["name"], w["n"], w["failed"],
                fmt_ms(w["p50"]), fmt_ms(w["p95"]), fmt_ms(w["p99"]), fmt_ms(w["max"])))
    print("")
    if any(arm.get("failure_kinds") for arm in arms):
        print("Failures by kind, as the load generator grouped them. A request that never "
              "received a response is a failure, not a fast success -- counting one as a "
              "success lets a saturating arm improve its own percentiles:")
        print("")
        for arm in arms:
            kk = arm.get("failure_kinds") or {}
            if kk:
                print("- %s: %s" % (arm["name"],
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
        for i in rises[key]:
            lo = schedule[i]["start"]
            for arm in arms:
                w = window_of(arm, key, i)
                if w is None:
                    print("| %s | %ds | %s | - | - | - | - | - |" % (label, lo, arm["name"]))
                    continue
                out["arms"][arm["name"]]["rises"]["%s@%d" % (key, lo)] = w
                print("| %s | %ds | %s | %d | %d | %s | %s | %s |" % (
                    label, lo, arm["name"], w["n"], w["failed"],
                    fmt_ms(w["p50"]), fmt_ms(w["p95"]), fmt_ms(w["max"])))
    print("")

    print("## Accelerators")
    print("")
    for arm in arms:
        bud = arm["budget"]
        if bud:
            floor = bud.get("min_replicas_per_model")
            print("- %s: each model %s %d replicas, pool %d Pod(s) -- ceiling %d accelerators"
                  % (arm["name"],
                     "held between %d and" % floor if floor else "capped at",
                     bud["max_replicas_per_model"], bud.get("pool_replicas", 0),
                     arm_ceiling(bud)))
    print("")
    print("| arm | GPU-seconds (all) | of which pool | peak GPUs | pool lent (GPU-s) | sampling holes |")
    print("| --- | ---: | ---: | ---: | ---: | ---: |")
    totals = {}
    for arm in arms:
        ser = arm["gpus"]
        rec = out["arms"].setdefault(arm["name"], {"whole": {}, "rises": {}})
        rec["budget"] = arm["budget"]
        rec["gpu_series"] = ser
        if len(ser) < 2:
            print("| %s | - | - | - | - | no samples |" % arm["name"])
            totals[arm["name"]] = None
            continue
        tot, gapped = integrate(ser, 1, args.gap_limit)
        poolgpu, _ = integrate(ser, 2)
        lent, _ = integrate(ser, 3)
        totals[arm["name"]] = tot
        rec["gpu_seconds"] = {"all": tot, "pool": poolgpu, "lent": lent,
                              "peak": max(r[1] for r in ser)}
        print("| %s | %.0f | %.0f | %d | %.0f | %s |" % (
            arm["name"], tot, poolgpu, max(r[1] for r in ser), lent,
            "-" if gapped == 0 else "%.0fs" % gapped))
    print("")

    print("## What this says")
    print("")
    for b in others:
        what = "with %s" % ("the pool" if b["name"] == "pool" else "the floor" if b["name"] == "floor" else b["name"])
        for key, label in (("a", args.model_a), ("b", args.model_b)):
            for i in rises[key]:
                lo = schedule[i]["start"]
                sn = window_of(a, key, i) or {"p95": None, "n": 0, "failed": 0}
                sp = window_of(b, key, i) or {"p95": None, "n": 0, "failed": 0}
                if sn["p95"] is None or sp["p95"] is None:
                    print("- **%s**, rise at %ds: one arm served nothing in the window." % (label, lo))
                    continue
                delta = (sn["p95"] - sp["p95"]) * 1000.0
                print("- **%s**, rise at %ds: p95 TTFT %.0f ms %s %s "
                      "(%s -> %s ms; n=%d vs %d served, %d vs %d failed)."
                      % (label, lo, abs(delta), "lower" if delta > 0 else "higher", what,
                         fmt_ms(sn["p95"]), fmt_ms(sp["p95"]),
                         sn["n"], sp["n"], sn["failed"], sp["failed"]))
        gt_n, gt_p = totals.get("nopool"), totals.get(b["name"])
        if gt_n and gt_p:
            diff = gt_p - gt_n
            if diff > 0:
                print("- The %s arm spent **%.0f more GPU-seconds** than nopool (%.1f%%).%s"
                      % (b["name"], diff, 100.0 * diff / gt_n,
                         " Note this EXCLUDES the pool's warm-up, which happens before the "
                         "run starts, so it understates the cost of holding one."
                         if b["name"].startswith("pool") else ""))
            else:
                print("- The %s arm spent **%.0f fewer GPU-seconds** than nopool (%.1f%%)."
                      % (b["name"], -diff, 100.0 * -diff / gt_n))
        else:
            print("- GPU-seconds are missing for the nopool or %s arm, so the cost side of "
                  "that comparison is **not** established; the TTFT numbers alone do not "
                  "settle it." % b["name"])
    # The comparison the third arm exists for: two kinds of insurance, priced
    # against each other on what they bought.
    by_name = {arm["name"]: arm for arm in arms}
    if "pool" in by_name and "floor" in by_name and totals.get("pool") and totals.get("floor"):
        gp, gf = totals["pool"], totals["floor"]
        worse = compared = 0
        for key in ("a", "b"):
            for i in rises[key]:
                wp = window_of(by_name["pool"], key, i)
                wf = window_of(by_name["floor"], key, i)
                if not (wp and wf and wp["p95"] is not None and wf["p95"] is not None):
                    continue
                compared += 1
                if wp["p95"] > wf["p95"]:
                    worse += 1
        print("- **Pool against floor**: the pool arm spent %.0f GPU-seconds and the floor "
              "arm %.0f (%+.1f%% for the pool); the pool's rise p95 was higher than the "
              "floor's in %d of the %d rises both served."
              % (gp, gf, 100.0 * (gp - gf) / gf, worse, compared))
    # The limit of the evidence, printed every time, because a table invites a
    # conclusion the sample size does not support.
    n_rises = len(rises["a"]) + len(rises["b"])
    print("")
    print("**How far this goes.** %d scale-up events per arm, one run each, no repetition "
          "and no confidence interval. A difference of the same order as the spread "
          "between the two rises of the same model is not a result. To claim a direction, "
          "repeat the pair and check that the sign is stable." % n_rises)
    print("")
    if args.json:
        with open(args.json, "w") as fh:
            json.dump(out, fh, indent=1, default=float)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
