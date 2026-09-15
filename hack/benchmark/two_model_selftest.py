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

import json
import math
import os
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import two_model_load as load      # noqa: E402
import two_model_report as report  # noqa: E402

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
# rise windows
# ---------------------------------------------------------------------------
meta_sched = [{"start": s, "end": e, "rate_a": ra, "rate_b": rb} for s, e, ra, rb in sched]

case("rise windows follow the model, not the phase index")
wa = report.rise_windows(meta_sched, "a", 90)
wb = report.rise_windows(meta_sched, "b", 90)
if wa != [(600, 690), (1560, 1650)]:
    fail("model A's rise windows are %s, expected the phases where A goes 2->12" % wa)
elif wb != [(120, 210), (1080, 1170)]:
    fail("model B's rise windows are %s, expected the phases where B goes 2->12" % wb)
else:
    ok("each model's windows are the phases where ITS OWN rate rose")

case("a rise window never runs past its phase")
w = report.rise_windows(meta_sched, "a", 10_000)
if w[0][1] != 1080:
    fail("an over-long window escaped its phase: %s. It would cover the NEXT phase, where "
         "the rate fell, and report steady state as a rise." % w)
else:
    ok("a window longer than the phase is clipped to it")

case("a model that never rises yields no window")
if report.rise_windows([{"start": 0, "end": 100, "rate_a": 2, "rate_b": 2}], "a", 60):
    fail("a flat schedule produced a rise window")
else:
    ok("no rise, no window")

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

case("a torn stream is a failure, not a fast success")
rows = [
    {"model": "a", "t_sched": 1.0, "ttft": 0.1, "error": None, "tokens": 200},
    {"model": "a", "t_sched": 2.0, "ttft": 0.1, "error": "torn after 3 frames", "tokens": 3},
    {"model": "a", "t_sched": 3.0, "ttft": None, "error": "deadline", "tokens": 0},
]
s = report.summarize(rows, "a")
if s["n"] != 1 or s["failed"] != 2:
    fail("a truncated stream was counted as served (%s). Counting it as a success lets a "
         "saturating arm shed its worst requests while its percentiles improve." % s)
else:
    ok("only a complete response counts as served")

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


def run_report(meta_a, meta_b, rows_a=None, rows_b=None,
               budget_a=None, budget_b=None):
    """report.main over two fixture directories; returns its exit code."""
    d = tempfile.mkdtemp()
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

print("")
if FAIL:
    print("two-model self-test FAILED (%d of %d cases)" % (FAIL, CASES))
    sys.exit(1)
print("two-model self-test OK (%d cases)" % CASES)
