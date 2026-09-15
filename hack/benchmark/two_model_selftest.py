#!/usr/bin/env python3
"""
Self-test for the two-model anti-phase scenario: the schedule, the arrival plan,
the rise windows, the report's arithmetic and every one of its refusals --
executed rather than read.

It exists because each claim this scenario makes rests on a small function
nobody looks at twice, and every one of them fails SILENTLY:

  * `build_schedule` putting both models on the high rate in one phase still
    completes and still prints a table -- describing an IN-PHASE run as an
    anti-phase one, the opposite of the thing being measured.
  * `plan_arrivals` drawing from one shared stream gives the two ARMS different
    traffic under an identical --seed, which is the one thing an A/B must not do.
  * `rise_windows` marking the wrong phases turns the headline number into
    steady-state TTFT under a heading that says rise.
  * `pct` off by one rank inflates both arms, unequally.

Run by hack/check-two-model-scenario.sh; no cluster, no network.
"""

import io
import json
import math
import os
import sys
import tempfile

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import harness_results as harness    # noqa: E402
import two_model_load as load        # noqa: E402
import two_model_profile as profile  # noqa: E402
import two_model_report as report    # noqa: E402

FAIL = 0
CASES = 0


def case(name):
    global CASES
    CASES += 1
    return name


def fail(msg):
    global FAIL
    FAIL += 1
    print("FAIL %s" % msg)


def ok(msg):
    print("ok   %s" % msg)


# ---------------------------------------------------------------------------
# the schedule
# ---------------------------------------------------------------------------
case("schedule shape")
sched = load.build_schedule(phase_seconds=480, cycles=2, low_rps=2, high_rps=12, lead_in=120)
if len(sched) != 5:
    fail("schedule has %d phases, expected 1 lead-in + 2*2 = 5" % len(sched))
elif sched[0] != (0, 120, 2, 2):
    fail("lead-in is %s, expected both models at the LOW rate for 120s -- without it the "
         "first burst measures a cold stack, not a scale-up" % (sched[0],))
elif sched[-1][1] != 120 + 4 * 480:
    fail("schedule ends at %ds, expected %ds" % (sched[-1][1], 120 + 4 * 480))
else:
    ok("lead-in then 2 cycles of 2 phases, contiguous, ending at 2040s")

case("anti-phase is structural")
both_high = [i for i, p in enumerate(sched[1:], 1) if p[2] == 12 and p[3] == 12]
same = [i for i, p in enumerate(sched[1:], 1) if p[2] == p[3]]
if both_high:
    fail("both models are at the HIGH rate in phase(s) %s -- an IN-PHASE run, which the "
         "report would describe as anti-phase" % both_high)
elif same:
    fail("models share a rate in phase(s) %s after the lead-in" % same)
else:
    ok("after the lead-in, exactly one model is high in every phase")

case("phases are contiguous")
gaps = [(sched[i][1], sched[i + 1][0]) for i in range(len(sched) - 1)
        if sched[i][1] != sched[i + 1][0]]
if gaps:
    fail("phase boundaries do not meet: %s. A gap is unoffered load; an overlap "
         "double-counts arrivals." % gaps)
else:
    ok("every phase starts where the previous one ended")

case("a rate boundary belongs to the phase it opens")
if load.rate_at(sched, 119.999) != (2, 2):
    fail("just before the boundary: %s, expected the lead-in's (2, 2)" % (load.rate_at(sched, 119.999),))
elif load.rate_at(sched, 120.0) != (2, 12):
    fail("at the boundary: %s, expected phase 1's (2, 12)" % (load.rate_at(sched, 120.0),))
elif load.rate_at(sched, sched[-1][1]) != (None, None):
    fail("past the end the rate is not (None, None), so the driver would not stop")
else:
    ok("a boundary opens its phase, and the end stops the driver")

case("a single cycle still bursts both models")
one = load.build_schedule(phase_seconds=60, cycles=1, low_rps=1, high_rps=5, lead_in=0)
if len(one) != 2 or not (one[0][3] == 5 and one[1][2] == 5):
    fail("cycles=1 did not give each model exactly one burst: %s" % (one,))
else:
    ok("cycles=1 gives each model one burst, with no lead-in")

# ---------------------------------------------------------------------------
# the arrival plan -- the part that makes the two arms the same experiment
# ---------------------------------------------------------------------------
case("the same seed gives the same arrivals")
p1 = load.plan_arrivals(sched, "a", 1729)
p2 = load.plan_arrivals(sched, "a", 1729)
if p1 != p2:
    fail("two plans from the same seed differ: the two ARMS would get different traffic, "
         "which is the one thing an A/B comparison must not do")
elif not p1:
    fail("the plan is empty")
else:
    ok("arrivals are reproducible from the seed (%d for model A)" % len(p1))

case("the two models draw from independent streams")
pa = load.plan_arrivals(sched, "a", 1729)
pb = load.plan_arrivals(sched, "b", 1730)
if [t for t, _ in pa[:20]] == [t for t, _ in pb[:20]]:
    fail("both models got identical arrival times; they share an RNG stream")
else:
    ok("each model has its own stream, so one model's timing cannot shift the other's")

case("the first arrival is not at zero")
if pa[0][0] <= 0:
    fail("model A's first arrival is at t=%s; both models firing at t=0 opens the run with "
         "a synchronised pair it never repeats" % pa[0][0])
else:
    ok("the first arrival comes after a drawn gap (t=%.2fs)" % pa[0][0])

case("arrivals are ordered and inside the schedule")
times = [t for t, _ in pa]
if times != sorted(times):
    fail("arrivals are not monotonic")
elif times[-1] >= sched[-1][1]:
    fail("an arrival at %.1fs lands past the end of the schedule (%ds)" % (times[-1], sched[-1][1]))
else:
    ok("arrivals are ordered and all inside the run")

case("the arrival RATE follows the phase")
# Model A: low (2 rps) in the lead-in and phases 1 and 3, high (12) in 2 and 4.
lead = [t for t in times if t < 120]
burst = [t for t in times if 600 <= t < 1080]      # phase 2: A is high
lead_rate = len(lead) / 120.0
burst_rate = len(burst) / 480.0
if not (1.0 < lead_rate < 3.5):
    fail("the lead-in ran at %.2f rps, not the 2 it asks for" % lead_rate)
elif not (9.0 < burst_rate < 15.0):
    fail("model A's burst ran at %.2f rps, not the 12 it asks for" % burst_rate)
else:
    ok("measured %.1f rps in the lead-in and %.1f rps in the burst" % (lead_rate, burst_rate))

case("every arrival carries the phase it belongs to")
bad = [(t, ph) for t, ph in pa if load.phase_index_at(sched, t) != ph]
if bad:
    fail("%d arrivals carry the wrong phase index, e.g. %s" % (len(bad), bad[0]))
else:
    ok("each arrival is tagged with the phase it falls in")

# ---------------------------------------------------------------------------
# rise windows -- which are STAGES, because the harness reports a latency
# distribution per stage and nothing finer.
# ---------------------------------------------------------------------------
meta_stages = profile.schedule_json(profile.stage_map(sched, 90))
meta_sched = meta_stages

# ---------------------------------------------------------------------------
# the overlap band -- what keeps the two models' bursts from running into each
# other when they cross a boundary at different moments
# ---------------------------------------------------------------------------
case("a both-low band sits between consecutive bursts")
banded = profile.build_schedule(480, 2, 3, 9, 120, overlap=30)
bursts = [i for i, p in enumerate(banded) if p[2] != p[3]]
between = [(banded[x][1], banded[y][0]) for x, y in zip(bursts, bursts[1:])]
gaps = [b - a for a, b in between]
if not bursts:
    fail("the schedule has no bursts at all")
elif any(g != 30 for g in gaps):
    fail("consecutive bursts are separated by %s, not the 30s band. A stage ends when "
         "its requests DRAIN and the bursting model drains slower, so without a band "
         "the two models burst at once for however far they have drifted -- which a "
         "pool can only half serve, and which would be recorded as the pool failing"
         % gaps)
else:
    ok("%d bursts, each pair separated by a 30s both-low band" % len(bursts))

case("the band is at the LOW rate, so it is a dip in demand and not a rise")
band_phases = [p for i, p in enumerate(banded) if p[2] == p[3] and i > 0]
if not band_phases:
    fail("no band phases were produced")
elif any(p[2] != 3 for p in band_phases):
    fail("a band runs at %s, not the low rate. Above it the band would be a burst of "
         "its own and the fleet would scale for it" % [p[2] for p in band_phases])
else:
    ok("every band runs both models at the low rate")

case("no band is wasted immediately after the lead-in")
# The lead-in already is a both-low band; a second one would only be dead time
# before the first burst.
if banded[1][2] == banded[1][3]:
    fail("a band was inserted straight after the lead-in: %s. The lead-in already holds "
         "both models low" % (banded[1],))
else:
    ok("the first burst follows the lead-in directly")

case("with no band the schedule is exactly what it was")
if profile.build_schedule(480, 2, 3, 9, 120, overlap=0) != \
        load.build_schedule(phase_seconds=480, cycles=2, low_rps=3, high_rps=9, lead_in=120):
    fail("overlap=0 changed the schedule, so the band is not an addition but a rewrite "
         "and no stored run is comparable to a new one")
else:
    ok("overlap=0 reproduces the original schedule exactly")

case("a band is never cut into a rise window")
band_stages = profile.stage_map(banded, 90)
starts = [st[0] for st in band_stages]
if len(set(starts)) != len(starts):
    fail("two stages start at the same second: %s" % starts)
elif any((st[1] - st[0]) == 90 and st[2] == st[3] for st in band_stages[1:]):
    fail("a both-low band was cut at the rise window. Nothing rises into it, so the cut "
         "only spends a stage: %s" % band_stages)
else:
    ok("only phases a model rises into are cut")

case("every phase after the lead-in opens with a rise-window stage")
bounds = [(st["start"], st["end"]) for st in meta_stages]
if bounds[0] != (0, 120):
    fail("the lead-in was cut up: %s. Nothing rises into it, and splitting it only "
         "spends stages." % (bounds[0],))
elif (120, 210) not in bounds or (210, 600) not in bounds:
    fail("phase 1 was not cut at the rise window: %s. TTFT is reported per stage, so an "
         "uncut phase averages a 90s scale-up into 8 minutes of steady state." % bounds)
elif sum(e - s for s, e in bounds) != sched[-1][1]:
    fail("the stages do not tile the schedule: %s. Load was added or dropped by the cut, "
         "which is not what a cut is for." % bounds)
else:
    ok("each phase opens with its own rise-window stage, and the stages tile the run")

case("rise stages follow the model, not the stage index")
ia = report.rise_stages(meta_stages, "a")
ib = report.rise_stages(meta_stages, "b")
starts_a = [meta_stages[i]["start"] for i in ia]
starts_b = [meta_stages[i]["start"] for i in ib]
if starts_a != [600, 1560]:
    fail("model A rises at %s, expected the stages where A goes 2->12" % starts_a)
elif starts_b != [120, 1080]:
    fail("model B rises at %s, expected the stages where B goes 2->12" % starts_b)
else:
    ok("each model's rises are the stages where ITS OWN rate rose")

case("a rise stage is the window, not the whole phase")
for i in ia:
    st = meta_stages[i]
    if st["end"] - st["start"] != 90:
        fail("the rise stage at %ds is %ds long, not the 90s rise window: a scale-up "
             "averaged over a whole phase is reported as steady state"
             % (st["start"], st["end"] - st["start"]))
        break
else:
    ok("a rise stage is exactly the rise window")

case("a model that never rises yields no rise stage")
if report.rise_stages([{"start": 0, "end": 100, "rate_a": 2, "rate_b": 2}], "a"):
    fail("a flat schedule produced a rise stage")
else:
    ok("no rise, no stage")

# ---------------------------------------------------------------------------
# the report's arithmetic
# ---------------------------------------------------------------------------
case("percentiles are nearest-rank, not a rank high")
bad = []
for n in (1, 2, 20, 100, 600):
    vals = [float(i) for i in range(1, n + 1)]
    for q in (50, 95, 99):
        got = report.pct(vals, q)
        want = vals[max(0, min(n - 1, int(math.ceil(q / 100.0 * n)) - 1))]
        if got != want:
            bad.append((n, q, got, want))
if bad:
    fail("%d percentile(s) off, e.g. n=%d q=%d gave %s not %s -- `round(x+0.5)` hits "
         "banker's rounding and returns one rank HIGH, inflating both arms unequally"
         % (len(bad), bad[0][0], bad[0][1], bad[0][2], bad[0][3]))
elif report.pct([], 50) is not None:
    fail("an empty set produced a percentile; the table would print a number for a model "
         "that served nothing")
else:
    ok("percentiles land on the nearest rank, and an empty set has none")

case("GPU-seconds integrate the sample forward")
# 3 GPUs for 10s then 5 for 10s = 80. The last sample spans no time.
series = [(0, 3, 0, 0), (10, 5, 0, 0), (20, 5, 0, 0)]
total, gapped = report.integrate(series, 1)
if total != 80.0:
    fail("integrate gave %s GPU-seconds, expected 80 (3x10 + 5x10)" % total)
elif report.integrate([(0, 3, 0, 0)], 1)[0] is not None:
    fail("a single sample produced a total; one sample spans no time")
else:
    ok("GPU-seconds integrate forward, and one sample is not a duration")

case("a hole in the sampling is reported, not hidden")
holed = [(0, 4, 0, 0), (120, 4, 0, 0)]
_, gapped = report.integrate(holed, 1, gap_limit=30)
if gapped != 120:
    fail("a 120s hole between samples was not reported (%s). A failed `kubectl get` writes "
         "no line, so the previous sample is forward-filled across it." % gapped)
else:
    ok("a sampling hole longer than the limit is reported")

case("pool Pods: idle, lent, and always inside the total")
with tempfile.TemporaryDirectory() as d:
    p = os.path.join(d, "gpus.jsonl")
    with open(p, "w") as fh:
        fh.write(json.dumps({"ts": 1000, "pods": [
            {"name": "pool-1", "gpus": 1, "pool": "p", "model": "", "serving": ""},
            {"name": "decode-1", "gpus": 1, "pool": "", "model": "m", "serving": "true"}]}) + "\n")
        fh.write(json.dumps({"ts": 1010, "pods": [
            {"name": "pool-1", "gpus": 1, "pool": "p", "model": "m", "serving": "true"},
            {"name": "decode-1", "gpus": 1, "pool": "", "model": "m", "serving": "true"}]}) + "\n")
    ser = report.gpu_series(p, 1000)
    if ser[0][0] != 0 or ser[1][0] != 10:
        fail("samples were not aligned to the run's origin: %s" % (ser,))
    elif ser[0][3] != 0:
        fail("an IDLE pool Pod was counted as lent: %s" % (ser[0],))
    elif ser[1][3] != 1:
        fail("a LENT pool Pod was not counted: %s" % (ser[1],))
    elif ser[1][1] != 2:
        fail("total GPUs is %s; the pool's own must be INCLUDED or the pool arm looks free"
             % ser[1][1])
    else:
        ok("samples align to t0; idle and lent are told apart; the pool is in the total")

case("a failed request is not a served one")
rows = [
    {"model": "a", "t_rel": 1.0, "error": ""},
    {"model": "a", "t_rel": 2.0, "error": "ClientPayloadError: torn after 3 frames"},
    {"model": "a", "t_rel": 3.0, "error": "TimeoutError: deadline"},
]
served, failed = report.served_and_failed(rows, "a")
if len(served) != 1 or len(failed) != 2:
    fail("%d served / %d failed, expected 1/2. Counting a torn response as a success lets "
         "a saturating arm shed its worst requests while its percentiles improve."
         % (len(served), len(failed)))
else:
    ok("only a request that did not fail counts as served")

case("a window with no successes has no TTFT, not a zero one")
w = harness.window_from({"successes": {"count": 0}, "failures": {"count": 12},
                         "load_summary": {}})
if w["p95"] is not None or w["p50"] is not None:
    fail("a window where everything failed reported a TTFT of %s. Zero would make the "
         "worst window in the run look like the best." % w["p95"])
elif w["failed"] != 12:
    fail("the failures were lost: %s" % w)
else:
    ok("a window that served nothing reports no latency")

# ---------------------------------------------------------------------------
# the refusals
# ---------------------------------------------------------------------------
BASE_ROWS = [{"model": "a", "phase": 1, "t_sched": 130.0, "queue_delay": 0.01,
              "ttft": 0.1, "ttft_send": 0.1, "total": 1.0, "tokens": 200,
              "want_tokens": 200, "done": True, "status": 200, "error": None}]
BASE_META = {"schedule": meta_sched, "input_tokens": 1000, "output_tokens": 200,
             "model_a": "A", "model_b": "B", "seed": 1729,
             "planned": 100, "issued": 100, "t0": 1000}


BUDGET_N = {"arm": "nopool", "max_replicas_per_model": 3, "pool_replicas": 0,
            "gpus_per_replica": 1}
BUDGET_P = {"arm": "pool", "max_replicas_per_model": 2, "pool_replicas": 2,
            "gpus_per_replica": 1}


WORK_OK = {"pod-1": 5000.0, "pod-2": 4800.0}


def run_report(meta_a, meta_b, rows_a=None, rows_b=None,
               budget_a=None, budget_b=None, work_a=None, work_b=None):
    """report.main over two fixture directories; returns its exit code."""
    d = tempfile.mkdtemp()
    works = {"nopool": WORK_OK if work_a is None else work_a,
             "pool": WORK_OK if work_b is None else work_b}
    for name, meta, rows, bud in (("nopool", meta_a, rows_a or BASE_ROWS, BUDGET_N if budget_a is None else budget_a),
                                  ("pool", meta_b, rows_b or BASE_ROWS, BUDGET_P if budget_b is None else budget_b)):
        sub = os.path.join(d, name)
        os.makedirs(sub)
        with open(os.path.join(sub, "requests.jsonl"), "w") as fh:
            for r in rows:
                fh.write(json.dumps(r) + "\n")
        with open(os.path.join(sub, "meta.json"), "w") as fh:
            json.dump(meta, fh)
        if bud != "omit":
            with open(os.path.join(sub, "budget.json"), "w") as fh:
                json.dump(bud, fh)
        w = works[name]
        if w != "omit":
            # before is empty, so after IS the work done during the arm.
            with open(os.path.join(sub, "podwork.before"), "w") as fh:
                for pod in w:
                    fh.write("%s 0\n" % pod)
            with open(os.path.join(sub, "podwork.after"), "w") as fh:
                for pod, v in w.items():
                    fh.write("%s %.0f\n" % (pod, v))
    # The table goes nowhere: these cases are about the RETURN CODE, and a full
    # report printed into the middle of a check makes its own `ok` lines
    # unfindable.
    keep = sys.stdout
    sys.stdout = open(os.devnull, "w")
    try:
        return report.main(["--nopool", os.path.join(d, "nopool"),
                            "--pool", os.path.join(d, "pool")])
    finally:
        sys.stdout.close()
        sys.stdout = keep


case("arms that ran different scenarios are refused")
other = dict(BASE_META); other["output_tokens"] = 400
if run_report(BASE_META, other) == 0:
    fail("the report compared a 200-token arm against a 400-token one")
else:
    ok("arms that did not run the same scenario are refused")

case("a different seed is a different experiment")
other = dict(BASE_META); other["seed"] = 7
if run_report(BASE_META, other) == 0:
    fail("the report compared arms whose traffic was drawn from different seeds")
else:
    ok("a seed difference is refused -- the arms did not send the same requests")

case("an arm whose driver could not keep up is refused")
starved = dict(BASE_META); starved["issued"] = 80
if run_report(BASE_META, starved) == 0:
    fail("the report accepted an arm that issued 80 of 100 arrivals; its TTFT describes "
         "the loader, not the cluster")
else:
    ok("an arm with unissued arrivals is refused")

case("an arm whose own queueing is material is refused")
slow = [dict(BASE_ROWS[0], queue_delay=2.5) for _ in range(10)]
if run_report(BASE_META, dict(BASE_META), rows_b=slow) == 0:
    fail("the report compared an arm carrying 2.5s of its OWN queueing inside every TTFT")
else:
    ok("driver queueing above the limit is refused")

case("an arm thinned by the loader's OWN network errors is refused")
# Measured on CoreWeave: a new connection per request meant a DNS lookup per
# request, and at ~14 rps for 34 minutes the resolver gave out -- gaierror on
# half of all requests, in both arms. The table was produced anyway, over the
# survivors, and read as a result.
lossy = ([dict(BASE_ROWS[0]) for _ in range(90)]
         + [dict(BASE_ROWS[0], ttft=None, error="gaierror: [Errno -3] Temporary failure")
            for _ in range(10)])
if run_report(BASE_META, dict(BASE_META), rows_b=lossy) == 0:
    fail("the report tabulated an arm that lost 10%% of its requests to client-side DNS "
         "failures; those are the loader's network, not the cluster's")
else:
    ok("client-side network loss above the threshold is refused")

case("a few client-side errors do not void a run")
few = ([dict(BASE_ROWS[0]) for _ in range(199)]
       + [dict(BASE_ROWS[0], ttft=None, error="gaierror: blip")])
if run_report(BASE_META, dict(BASE_META), rows_b=few) != 0:
    fail("one client-side error in 200 voided the run; the threshold is too tight to ever pass")
else:
    ok("an occasional client-side error does not void the run")

case("prompt prefixes are distinct across groups")
# THE DEFECT THAT INVALIDATED THREE RUNS. One shared prefix hands every request
# to whichever replica cached it first, because llm-d's EPP weights the
# prefix-cache scorer above queue depth. Measured: 6,000,692 prompt tokens on one
# replica and ZERO on every other, including an awake warm-pool Pod.
groups = load.build_prefix_groups(1729, 32, 1000)
firsts = {g.split()[0] for g in groups}
if len(groups) != 32:
    fail("asked for 32 prefix groups and got %d" % len(groups))
elif len(set(groups)) != 32:
    fail("the prefix groups are not distinct: %d unique of 32" % len(set(groups)))
elif len(firsts) < 5:
    fail("the groups share a first token (%d distinct openings of 32): prefix-cache "
         "scoring keys on the leading tokens, so near-identical openings recreate the "
         "lock-in" % len(firsts))
elif load.build_prefix_groups(1729, 32, 1000) != groups:
    fail("the groups are not reproducible from the seed, so the two arms would send "
         "different traffic")
else:
    ok("32 distinct prefix groups, %d distinct openings, reproducible from the seed" % len(firsts))

case("one group reproduces the lock-in, and is the documented degenerate case")
one = load.build_prefix_groups(1729, 1, 1000)
if len(one) != 1:
    fail("--prefix-groups 1 produced %d groups" % len(one))
else:
    ok("--prefix-groups 1 is a single shared prefix, the shape that caused the lock-in")

case("a run where one engine did all the work is refused")
# The guard that was missing entirely. Capacity that is never routed to cannot
# affect TTFT, so a table built from such a run describes a one-replica fleet.
one_busy = dict(BASE_META)
rc = run_report(BASE_META, one_busy,
                work_a={"pod-a": 1000.0, "pod-b": 900.0, "pod-c": 850.0},
                work_b={"pod-x": 6000692.0, "pod-y": 0.0, "pod-z": 0.0})
if rc == 0:
    fail("the report tabulated an arm in which one engine did every prompt token and "
         "the other replicas did none")
else:
    ok("an arm where only one engine did any work is refused")

case("a run with no per-engine capture is refused")
if run_report(BASE_META, dict(BASE_META), work_a="omit", work_b="omit") == 0:
    fail("the report ran with no podwork capture, so nothing established that the load "
         "reached more than one replica")
else:
    ok("a missing per-engine work capture is refused")

case("work spread across replicas is accepted")
if run_report(BASE_META, dict(BASE_META),
              work_a={"pod-a": 1000.0, "pod-b": 900.0},
              work_b={"pod-x": 1000.0, "pod-y": 950.0}) != 0:
    fail("a run with work spread over two engines was refused; the guard is too tight "
         "to ever pass")
else:
    ok("work spread across replicas is accepted")

case("a missing meta is refused rather than assumed")
d = tempfile.mkdtemp()
for name in ("nopool", "pool"):
    sub = os.path.join(d, name)
    os.makedirs(sub)
    with open(os.path.join(sub, "requests.jsonl"), "w") as fh:
        fh.write(json.dumps(BASE_ROWS[0]) + "\n")
if report.main(["--nopool", os.path.join(d, "nopool"), "--pool", os.path.join(d, "pool")]) == 0:
    fail("the report ran with no meta.json, so nothing checked that the arms match")
else:
    ok("a missing meta.json is refused")

case("an arm allowed more cluster is refused")
fat = dict(BUDGET_P); fat["max_replicas_per_model"] = 3      # pool AND the full ceiling
if run_report(BASE_META, dict(BASE_META), budget_b=fat) == 0:
    fail("the report compared an arm that held a pool AND the same per-model ceiling. That "
         "is a bigger cluster, not a pool, and it wins on TTFT for a reason that has "
         "nothing to do with lending.")
else:
    ok("a pool arm that was allowed more accelerators is refused")

case("an arm with no recorded budget is refused")
if run_report(BASE_META, dict(BASE_META), budget_b="omit") == 0:
    fail("the report ran with no budget.json, so nothing established that the two arms had "
         "the same ceiling")
else:
    ok("a missing replica budget is refused")

case("matching arms do produce a table")
rc = run_report(BASE_META, dict(BASE_META))
if rc != 0:
    fail("two identical arms were refused (rc=%d); every refusal above would then pass for "
         "the wrong reason" % rc)
else:
    ok("identical arms are compared")

case("equal low and high rates are refused by the loader")
if load.main(["--endpoint-a=http://x/a/v1/completions", "--endpoint-b=http://x/b/v1/completions",
              "--model-a=a", "--model-b=b", "--low-rps=5", "--high-rps=5",
              "--out=/tmp/unused.jsonl"]) != 2:
    fail("--low-rps equal to --high-rps was accepted; the run would be flat while the "
         "report describes it phase by phase")
else:
    ok("a flat 'burst' is refused")

case("one URL for both models is refused")
if load.main(["--endpoint-a=http://x/a/v1/completions", "--endpoint-b=http://x/a/v1/completions",
              "--model-a=a", "--model-b=b", "--out=/tmp/unused.jsonl"]) != 2:
    fail("both models were pointed at one URL. llm-d routes a multi-model stack by PATH "
         "prefix, so every request would reach one stack and the other model's rows would "
         "be a full run of 404s")
else:
    ok("the two models must have their own endpoints")

# ---------------------------------------------------------------------------
# the inference-perf profiles -- load is generated by the llm-d harness, so
# what is checked here is that the two profiles it is handed are MIRRORED, and
# that each one addresses its own stack.
# ---------------------------------------------------------------------------
PROF_ARGS = ["--model-a=unsloth/Meta-Llama-3.1-8B-Instruct", "--model-b=Qwen/Qwen3-8B",
             "--endpoint-a=http://gw.ns.svc.cluster.local:80/llama-31-8b",
             "--endpoint-b=http://gw.ns.svc.cluster.local:80/qwen3-8b"]


def render(role, extra=()):
    keep = sys.stdout
    sys.stdout = io.StringIO()
    try:
        rc = profile.main(["--emit", "profile", "--role", role] + PROF_ARGS + list(extra))
        return rc, sys.stdout.getvalue()
    finally:
        sys.stdout = keep


case("the profile generator builds the same schedule the loader did")
if profile.build_schedule(480, 2, 2, 12, 120) != load.build_schedule(
        phase_seconds=480, cycles=2, low_rps=2, high_rps=12, lead_in=120):
    fail("the harness profiles would run a different schedule from the one the report "
         "buckets against, so every rise window would mark the wrong requests")
else:
    ok("one schedule shape, whichever generator drives it")

case("the two ladders are mirrored, phase for phase")
sc = profile.build_schedule(480, 2, 3, 9, 120)
sa = profile.stages_for(sc, "a")
sb = profile.stages_for(sc, "b")
if [d for _, d in sa] != [d for _, d in sb]:
    fail("the two ladders have different stage DURATIONS, so the models drift out of "
         "phase as the run goes on: A=%s B=%s" % (sa, sb))
elif any(ra == rb for (ra, _), (rb, _) in list(zip(sa, sb))[1:]):
    fail("a burst phase puts both models on the same rate; that is an in-phase run "
         "reported as an anti-phase one: A=%s B=%s" % (sa, sb))
else:
    ok("mirrored ladders over identical boundaries")

case("the input budget is split, not invented")
s, q = profile.split_input(1000)
if s + q != 1000:
    fail("system_prompt_len + question_len = %d, not the 1000 input tokens asked for; "
         "the two arms would be priced at a length neither ran" % (s + q))
elif s <= q:
    fail("the shared prefix (%d) is no longer than the unique part (%d); with little to "
         "cache the router has nothing to spread on" % (s, q))
else:
    ok("input tokens split %d shared + %d unique" % (s, q))

case("base_url carries the stack prefix and no route")
rc, text = render("a")
doc = yaml.safe_load(text) if rc == 0 else {}
url = (doc.get("server") or {}).get("base_url", "")
if rc != 0:
    fail("the profile did not render (rc=%d)" % rc)
elif "/v1" in url:
    fail("base_url is %r. inference-perf appends the route itself, so this becomes "
         "/v1/completions/v1/completions -- a 404 from the gateway on every request" % url)
elif not url.endswith("/llama-31-8b"):
    fail("base_url %r does not end at model A's path prefix; llm-d routes a multi-model "
         "stack by path, so the requests would reach the wrong stack" % url)
else:
    ok("base_url is the gateway plus the stack prefix")

case("streaming and ignore_eos are both on")
if not (doc.get("api") or {}).get("streaming"):
    fail("streaming is off, so there is no first-token time and the whole scenario "
         "measures nothing")
elif not (doc.get("server") or {}).get("ignore_eos"):
    fail("ignore_eos is off: each model would generate as many tokens as it felt like, "
         "so the two arms would differ in work done per request")
else:
    ok("TTFT is observable and the output length is fixed")

case("the two models get different data streams")
_, text_b = render("b")
doc_b = yaml.safe_load(text_b)
if doc["data"]["shared_prefix"]["seed"] == doc_b["data"]["shared_prefix"]["seed"]:
    fail("both models draw the same questions at the same instants from one seed")
else:
    ok("each model has its own data seed")

case("THE TWO MODELS' BURSTS NEVER OVERLAP, BY DEFAULT")
# The invariant the whole scenario rests on. Checked on what the generator
# emits with NO arguments, because a band that only appears when someone passes
# --overlap is not a guarantee: setting the default to 0 removed every band
# from the schedule and every case that passed overlap=30 explicitly still
# passed.
_, text_d = render("a")
_, text_d_b = render("b")
la = yaml.safe_load(text_d)["load"]["stages"]
lb = yaml.safe_load(text_d_b)["load"]["stages"]
hi = max(s["rate"] for s in la)
both_high = [i for i, (x, y) in enumerate(zip(la, lb))
             if x["rate"] == hi and y["rate"] == hi]
if len(la) != len(lb):
    fail("the two ladders have %d and %d stages: they are not the same schedule"
         % (len(la), len(lb)))
elif both_high:
    fail("stage(s) %s put BOTH models at the high rate. The scenario is that their peaks "
         "do not coincide -- one pool covering many models is exactly the claim that "
         "fails if they burst together" % both_high)
else:
    bands = [i for i, (x, y) in enumerate(zip(la, lb))
             if x["rate"] == y["rate"] and i > 0]
    if not bands:
        fail("no both-equal band anywhere in the default schedule. The two models cross a "
             "stage boundary at different moments -- the bursting one drains slower -- so "
             "without a band the bursts run into each other by however far they drift")
    else:
        ok("no stage has both models high, and %d band(s) separate the bursts by default"
           % len(bands))

case("a flat 'burst' is refused by the profile generator")
rc, _ = render("a", ["--low-rps=5", "--high-rps=5"])
if rc != 2:
    fail("--low-rps equal to --high-rps was accepted; no scale-up happens in either arm "
         "while the report describes the run rise by rise")
else:
    ok("a flat rate is refused")

case("per-request reporting is off")
# It stores the raw SSE text of every chunk of every response: 1.2 GB for an
# ELEVEN MINUTE run, which kubectl cp (exec+tar) truncated -- losing the arm at
# the collection step after all its accelerators had been spent. Nothing needs
# it: there are no token timestamps in it, and the counts and failure breakdown
# live in the 7 KB stage files.
_, text_r = render("a")
rl = yaml.safe_load(text_r)["report"]["request_lifecycle"]
if rl.get("per_request"):
    fail("per_request reporting is on. The file reached 1.2 GB for an eleven-minute run "
         "and kubectl cp truncated it, which loses the arm after its accelerators are "
         "already spent")
elif not rl.get("per_stage") or not rl.get("summary"):
    fail("per_stage or summary reporting is off: %s. Those files are where every latency "
         "in the report comes from" % rl)
else:
    ok("stage and summary reports on, the 1.2 GB per-request file off")

case("stages run back to back, with no sleep between them")
# `interval` is the SLEEP BETWEEN STAGES, not a metrics interval: the load
# generator does `if self.stageInterval: await sleep(self.stageInterval)` after
# each stage drains. Measured at 30 -- the value the shipped guide profiles use
# -- that is 30s of zero load immediately before every rise stage, so queues
# drain and the autoscaler sees an idle fleet in the seconds before the burst
# this scenario exists to measure.
_, text_i = render("a")
doc_i = yaml.safe_load(text_i)
if doc_i["load"].get("interval", 0) != 0:
    fail("load.interval is %s. That is a sleep between stages, not a metrics interval: "
         "every rise would be preceded by that many seconds of silence, and the scale-up "
         "would be measured from an idle fleet" % doc_i["load"].get("interval"))
else:
    ok("no sleep between stages; a phase boundary is a change of rate, not a pause")

case("a rise window as long as the phase is refused")
rc, _ = render("a", ["--rise-window=480", "--phase-seconds=480"])
if rc != 2:
    fail("--rise-window equal to the phase was accepted, so no phase gets cut and there "
         "is no rise stage. This harness reports latency per stage only, so every "
         "scale-up would be averaged into eight minutes of steady state and reported "
         "under a heading that says rise")
else:
    ok("a rise window that cuts nothing is refused")

case("a single shared prefix is refused")
rc, _ = render("a", ["--prefix-groups=1"])
if rc != 2:
    fail("one prefix group was accepted. The shipped llm-d profile weights the "
         "prefix-cache scorer highest, so the first replica to cache that prefix takes "
         "every later request and the run measures a one-replica fleet")
else:
    ok("a degenerate single-prefix dataset is refused")

# ---------------------------------------------------------------------------
# the harness -> report conversion
#
# These fixtures are the SHAPE inference-perf v0.6.1 actually writes, read off a
# real run on CoreWeave: per-request records with no token times at all, and a
# `time_to_first_token` distribution per stage. A fixture invented from the
# newer schema would make every case here pass against code that cannot read a
# single real run.
# ---------------------------------------------------------------------------
def stage_doc(n, failed=0, p50=0.05, p95=0.09, delay_p95=0.004):
    latency = {}
    if n:
        latency["time_to_first_token"] = {"median": p50, "p95": p95, "p99": p95 * 1.2,
                                          "max": p95 * 1.5, "min": p50 * 0.8}
    return {"load_summary": {"count": n + failed, "requested_rate": 3.0,
                             "achieved_rate": 2.98,
                             "schedule_delay": {"median": 0.0005, "p95": delay_p95}},
            "successes": {"count": n, "latency": latency},
            "failures": {"count": failed}}


def rec(start, error=None, out_tokens=497):
    """One per-request record, in the shape this harness version writes."""
    return {"start_time": start, "end_time": start + 2.5,
            "info": {"input_tokens": 999,
                     "response_metrics": {"output_tokens": out_tokens,
                                          "response_chunks": ["{}"]}},
            "error": error}


def harness_dir(records, stages, summary=True):
    d = tempfile.mkdtemp()
    with open(os.path.join(d, "per_request_lifecycle_metrics.json"), "w") as fh:
        json.dump(records, fh)
    for i, st in enumerate(stages):
        with open(os.path.join(d, "stage_%d_lifecycle_metrics.json" % i), "w") as fh:
            json.dump(st, fh)
    if summary:
        with open(os.path.join(d, "summary_lifecycle_metrics.json"), "w") as fh:
            json.dump(stage_doc(len(records), 0), fh)
    return d


case("TTFT comes from the stage the harness measured, not from the rows")
w = harness.window_from(stage_doc(120, failed=3, p50=0.048, p95=0.062))
if w["n"] != 120 or w["failed"] != 3:
    fail("counts are %s; successes and failures are separate fields and must not be pooled" % w)
elif abs(w["p50"] - 0.048) > 1e-9 or abs(w["p95"] - 0.062) > 1e-9:
    fail("TTFT read back as p50=%s p95=%s, not the distribution the harness wrote"
         % (w["p50"], w["p95"]))
else:
    ok("a window's latency is the generator's own time_to_first_token block")

case("rows carry no latency, because this harness records none")
rows = harness.rows_from([rec(10655087.75)], "a", 10655087.75)
if "ttft" in rows[0]:
    fail("a row carries a `ttft` field: there are no per-request token times in this "
         "version, so any value there was invented")
elif rows[0]["t_rel"] != 0.0 or rows[0]["output_tokens"] != 497:
    fail("the row did not read the record: %s" % rows[0])
else:
    ok("rows carry counts and errors only")

case("inference-perf's own error_type survives into the row")
rows = harness.rows_from([rec(1.0, error={"error_type": "ClientConnectorDNSError",
                                          "error_msg": "Temporary failure in name resolution"})],
                         "a", 1.0)
if not rows[0]["error"].startswith("ClientConnectorDNSError"):
    fail("the error came through as %r; the report classifies client-side failures by "
         "the leading token, and a mangled type is a failure it cannot see"
         % rows[0]["error"])
else:
    ok("the generator's own error_type leads the row's error")

STAGES_FIXTURE = [{"start": 0, "end": 90, "rate_a": 1, "rate_b": 1},
                  {"start": 90, "end": 200, "rate_a": 1, "rate_b": 1}]
sched_file = os.path.join(tempfile.mkdtemp(), "schedule.json")
with open(sched_file, "w") as fh:
    json.dump(STAGES_FIXTURE, fh)


class _Args(object):
    pass


def convert_args(a, b):
    args = _Args()
    args.results_a, args.results_b, args.schedule = a, b, sched_file
    args.out = os.path.join(tempfile.mkdtemp(), "requests.jsonl")
    args.t0, args.arm, args.model_a, args.model_b = 1789464860.0, "pool", "A", "B"
    args.overlap = 30
    args.input_tokens, args.output_tokens = 1000, 500
    args.seed, args.prefix_groups = 1729, 32
    return args


two_stages = [stage_doc(24, failed=1), stage_doc(117, failed=2, delay_p95=0.02)]
dir_a = harness_dir([rec(2000.0), rec(2100.0)], two_stages)
dir_b = harness_dir([rec(2050.0), rec(2150.0)], two_stages)
conv_rows, conv_meta = harness.convert(convert_args(dir_a, dir_b))

case("the run's origin is the driver's barrier, not the harness's clock")
if conv_meta["t0"] != 1789464860.0:
    fail("t0 is %s. inference-perf's start_time is a MONOTONIC clock -- measured at "
         "10655087.75, an uptime -- so using it would put every GPU sample tens of "
         "thousands of hours away from the run." % conv_meta["t0"])
else:
    ok("t0 is the wall-clock barrier the driver set")

case("one window per stage, in the schedule's order")
if len(conv_meta["windows"]["a"]["stages"]) != len(STAGES_FIXTURE):
    fail("%d windows for %d stages: stage N of the report would not be stage N of the run"
         % (len(conv_meta["windows"]["a"]["stages"]), len(STAGES_FIXTURE)))
elif conv_meta["windows"]["a"]["stages"][1]["n"] != 117:
    fail("the stages came back out of order or misread: %s"
         % conv_meta["windows"]["a"]["stages"])
else:
    ok("every stage of the schedule has its own window")

case("a run that stopped short of the schedule is refused")
short = harness_dir([rec(1.0)], [stage_doc(10)])
if harness.main(["--results-a", short, "--results-b", short,
                 "--schedule", sched_file, "--t0", "1",
                 "--out", os.path.join(tempfile.mkdtemp(), "r.jsonl")]) != 2:
    fail("an arm that wrote fewer stages than the profile asked for was accepted; the "
         "windows the report describes would not be the windows that ran")
else:
    ok("a short run is refused, not padded")

case("the driver's own queueing is the WORST it reported, not the average")
if abs(conv_meta["queue_delay_p95"] - 0.02) > 1e-9:
    fail("queue_delay_p95 is %s, not the 0.02 one stage reported. A driver that kept up "
         "on average while falling behind through one burst was late exactly where it "
         "mattered." % conv_meta["queue_delay_p95"])
else:
    ok("driver queueing is the worst stage, from the generator that would be at fault")

case("a missing percentile never falls back to a LOWER one")
if harness.pick_percentile({"median": 0.001, "p99": 0.5}) != 0.5:
    fail("p95 was unavailable and something other than a higher percentile was used; "
         "reading p50 where p95 was meant passes the very runs the guard exists to stop")
elif harness.pick_percentile({"median": 0.001}) is not None:
    fail("only the median was available and it was used as p95")
elif harness.pick_percentile({"median": 0.007}, 50) != 0.007:
    fail("the median is written as `median` by this generator and was not found")
else:
    ok("the fallback is upward or nothing, and `median` is p50")

case("a run with NO per-request file still converts")
# The profile turns per-request reporting off: the file stores the raw SSE text
# of every chunk of every response and reached 1.2 GB for an eleven-minute run,
# which kubectl cp truncated -- losing the arm at collection after all its
# accelerators had been spent.
bare_a = harness_dir([], two_stages)
bare_b = harness_dir([], two_stages)
for d in (bare_a, bare_b):
    os.remove(os.path.join(d, "per_request_lifecycle_metrics.json"))
bare_rows, bare_meta = harness.convert(convert_args(bare_a, bare_b))
if bare_meta["issued"] != 2 * (24 + 1 + 117 + 2):
    fail("issued is %s, not the generator's own per-stage counts. With no rows to count, "
         "a length would be zero and read as a cluster that answered nothing"
         % bare_meta["issued"])
elif not bare_meta["windows"]["a"]["stages"]:
    fail("the windows were lost along with the per-request file")
else:
    ok("counts and windows come from the stage files, with no per-request file at all")

case("loss is counted from the generator's own failure labels")
lossy_meta = dict(BASE_META)
lossy_meta["failures_by_label"] = {"a": {"Connection Error": 20}, "b": {"Connection Error": 5}}
lossy_meta["issued"] = 420
if run_report(BASE_META, lossy_meta) == 0:
    fail("an arm that lost 25 of 420 requests to connection failures was compared. With "
         "per-request reporting off there are no rows to count, and counting zero would "
         "clear the guard silently -- which is the exact failure it exists for")
else:
    ok("failure labels are counted when there are no rows")

case("an HTTP failure is the cluster's, not the driver's")
served_meta = dict(BASE_META)
served_meta["failures_by_label"] = {"a": {"HTTP 500": 20}, "b": {}}
served_meta["issued"] = 420
if run_report(BASE_META, served_meta) != 0:
    fail("500s from the model voided the arm. A response that arrived is the cluster "
         "answering, which is the thing being measured, not a driver that could not ask")
else:
    ok("a request the model answered badly is not counted as one that never arrived")

case("drift is measured from the generator's own per-stage elapsed time")
drifted = {"windows": {
    "a": {"overall": {}, "stages": [{"elapsed": 100.0}, {"elapsed": 100.0}]},
    "b": {"overall": {}, "stages": [{"elapsed": 108.0}, {"elapsed": 112.0}]}}}
d = report.phase_drift({"meta": drifted})
if d is None or abs(d - 20.0) > 1e-9:
    fail("drift came out as %s, not the 20s the two models' cumulative stage times "
         "differ by. Drift accumulates: each stage ends when ITS requests drain, and "
         "the bursting model drains slower" % d)
else:
    ok("drift is the worst cumulative divergence, not the last one")

case("drift wider than the band is refused")
wide = dict(BASE_META)
wide["overlap_seconds"] = 5
wide["windows"] = drifted["windows"]
if run_report(BASE_META, wide) == 0:
    fail("the two models drifted 20s apart with only a 5s band between bursts, so for "
         "15s BOTH were bursting -- the one thing an anti-phase run must not contain, "
         "and a pool asked for two models at once can serve one")
else:
    ok("drift the band cannot absorb is refused")

case("drift inside the band is fine")
narrow = dict(BASE_META)
narrow["overlap_seconds"] = 30
narrow["windows"] = drifted["windows"]
if run_report(BASE_META, narrow) != 0:
    fail("a 20s drift against a 30s band was refused; the band exists precisely to "
         "absorb that")
else:
    ok("drift the band absorbs does not void the run")

case("a run with no measured driver queueing is refused")
no_qd = dict(BASE_META)
bare = [dict(r) for r in BASE_ROWS]
for r in bare:
    r.pop("queue_delay", None)
if run_report(BASE_META, no_qd, rows_b=bare) == 0:
    fail("an arm reporting no driver queueing at all was compared. Rows with no such "
         "field percentile to 0.0 and clear the guard silently, which is exactly the "
         "failure the guard is for")
else:
    ok("unmeasured driver queueing is a refusal, not a zero")

case("the error type that ACTUALLY occurred is recognised as the driver's")
# Not a hypothetical: a real run lost 25 of 420 requests to
# ClientConnectorDNSError -- the load Pod's istio sidecar was not ready when
# the app container started issuing -- and an exact "ClientConnectorError" in
# the list did not match it, so 6% driver-side loss was charged to the cluster
# and the table printed.
if not report.is_client_side("ClientConnectorDNSError: Temporary failure in name resolution"):
    fail("ClientConnectorDNSError was not recognised as client-side. The aiohttp connector "
         "family has DNS, SSL and certificate variants; a list that names only the base "
         "class silently lets the driver's own failures count as the cluster's")
elif report.is_client_side("ClientResponseError: 500"):
    fail("a server's response error was charged to the driver")

case("aiohttp's client-side failures are recognised as the driver's")
if not report.is_client_side("ClientConnectorError: cannot connect"):
    fail("inference-perf raises aiohttp errors, and one that is not recognised as "
         "client-side becomes a cluster failure the loss guard cannot see")
elif report.is_client_side("HTTP 500: internal error"):
    fail("a server error was charged to the driver")
else:
    ok("driver-side and cluster-side failures are told apart")

case("a run thinned by aiohttp errors is refused")
lossy = [dict(BASE_ROWS[0], error="ClientConnectorError: cannot connect", ttft=None)
         for _ in range(10)] + [dict(BASE_ROWS[0]) for _ in range(20)]
if run_report(BASE_META, dict(BASE_META), rows_b=lossy) == 0:
    fail("an arm that lost a third of its requests to the generator's own sockets was "
         "compared over the survivors")
else:
    ok("client-side loss voids the arm whichever generator produced it")

print("")
if FAIL:
    print("two-model self-test FAILED (%d of %d cases)" % (FAIL, CASES))
    sys.exit(1)
print("two-model self-test OK (%d cases)" % CASES)
