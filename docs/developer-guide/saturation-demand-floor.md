# The demand floors

Why the saturation analyzer floors its demand at what the offered load requires,
what each floor is and is not invariant to, and which of the claims here are
measured versus assumed.

There are two floors. The **arrival-rate floor** (`arrival_demand.go`) prices
the load by Little's law from the service time the engines report. The
**throughput floor** (`throughput_floor.go`) prices it from the completion rate
a replica was seen to sustain while saturated. The second exists because the
first turned out not to have the property it was built for; the measurement
that showed it is in [What it is not invariant to](#what-it-is-not-invariant-to).

Code: `internal/engines/analyzers/saturation_v2/arrival_demand.go` and
`internal/engines/analyzers/saturation_v2/throughput_floor.go`.

## The problem

Saturation's demand is **occupancy**: resident KV plus the waiting-queue
footprint. Both are states of the *fleet* rather than properties of the *load*,
and both shrink as capacity grows — resident KV because residence time falls, the
queue because it drains. The signal that sizes the fleet is therefore a function
of the fleet, and once the fleet is adequate the signal decays toward zero and
takes the replica count with it.

Measured on an H100 at 14 QPS (run `biran-20260822-021153-340`): demand fell from
7.5M tokens to 152k as ten replicas drained a 910-deep queue, and the target
followed from the ceiling down to one, mid-load. The arithmetic was right; the
input stopped describing the workload once the workload was being served.

## The floor

By Little's law:

```
L      = λ × W                requests concurrently in service
demand = L × (avgIn + avgOut) the KV they hold
demand = max(occupancy, that) never lowers, only raises
```

λ is the arrival rate the scheduler reports; the token counts are the request
shape. Neither moves when replicas are added, which is what makes them usable
where occupancy is not.

`W` is the per-request **service** time with the queue wait removed. End-to-end
latency will not do: it climbs when the fleet is behind and falls when it catches
up, which is the capacity-dependent term the floor exists to exclude.

## What it is not invariant to

`W` is not invariant to the fleet, and the effect is not small. Inter-token
latency rises with the batch a replica runs, and service time is output length
times ITL, so `W` tracks how many replicas are sharing the load. Measured on one
decode pod across the shape-swap P/D run (`biran-20260915-102548-571`; H200,
Qwen3-0.6B, a constant 6 req/s at a constant 6000 in / 1000 out):

| replicas sharing the load | batch on this pod | ITL | `W` |
|---|---|---|---|
| 6 | ~1 | 2.0 ms | 2.0 s |
| 2-3 | 40-78 | 4-6 ms | 15-22 s |
| 1 | 136-178 | 18-50 ms | 16-52 s |

A 25x range in `W` at the same load and the same shape. `lambda x W x tokens`
with the `W` of the moment is therefore current occupancy restated -- `L = lambda
W` is an identity, not an estimate -- and on that run the floor sat within 3-40%
of occupancy on every cycle it bound. At six replicas it read 88k tokens against
85k occupancy, one tenth of a replica, and authorised the scale-down to one; the
same load saturated that one replica within a minute and the controller went to
nine. The loop ran twice in 38 minutes.

An earlier version of this section said `W` is bounded below by the uncontended
cost of the work, so the floor cannot decay toward zero the way occupancy does.
That is true and does not help: the uncontended `W` is exactly what a fleet that
is far too large measures, so the bound holds nothing up. What the arrival floor
still does is damp the ramp -- a starved fleet measures a long `W` and asks for
more, in the same shape occupancy does -- and refuse a scale-down that a
misreported *zero* occupancy would otherwise permit. Holding a fleet at the size
the load needs is the throughput floor's job.

## The throughput floor

What a replica can do does not move with the fleet: its completion rate when it
is saturated. With the queue over `queueLengthThreshold` the engine is completing
requests as fast as it can for this shape, so the P1 moment that records k2
(occupancy under a saturated queue) also records the replica's completion rate,
`mu`, under the same history key. The fleet then needs `lambda / mu` replicas to
keep up, which in the analyzer's (demand, per-replica capacity) contract is a
demand of

```
floor_role = (lambda / mu_role) x P_role      tokens
```

applied per role on a P/D fleet (every request passes through both roles, so
each must keep up with all of it) and on the total otherwise. On the run above,
`mu` read 5.4 req/s at 6000/1000 and 2.5 req/s at 1000/4000 against 6 req/s
arriving: 2 and 3 decode replicas, which is where the fleet in fact held
whenever it was left at those sizes.

Properties, each with a spec in `throughput_floor_test.go`:

- **Invariant to fleet size.** `mu` is a per-replica constant, so the floor is
  the same at one replica and at six.
- **A max over the window, not a mean.** A saturated completion rate can only
  under-read its capacity -- in the first minute after a replica fills, the
  requests completing are the few admitted first; under KV pressure preemption
  drops it further -- and it cannot over-read, since nothing completes faster
  than the engine runs. The run's readings for one replica were 5.4, 3.3 and
  3.5 while it was demonstrably completing 5.4. The floor divides by `mu`, so a
  mean of under-reads would order replicas that are not needed.
- **A hold, not an order.** The floor is capped at `scaleUp x anticipated
  supply` for the role -- the largest demand the engine's `RC = D/scaleUp -
  anticipated` turns into nothing, with `scaleUp` read the way the engine
  reads it (`AnalyzerThresholds`, so a per-analyzer override applies to both).
  Above that it would stop holding the fleet
  and start growing it, and with a `mu` that under-read it would keep growing
  it every cycle. Scale-up stays with occupancy and the queue, which read well
  while a fleet is behind; the worst a bad `mu` can do is refuse one scale-down.
- **Ready pods and own replicas only.** The collector leaves a not-Ready pod's
  completion rate in place (only its timing is dropped), and a bridge's rate is
  the pool's, not the variant's.
- **Silent for a role never seen saturated.** Prefill, in practice: its queue
  is rarely the one that fills. No opinion rather than a guess, on the same
  principle as the arrival floor.

Both floors only ever raise, and the throughput floor is applied last. A floor
that binds logs `throughput-demand-floor` with `demandBeforeFloor` (what
demand stood at when it was applied -- possibly already raised by the arrival
floor, so not always occupancy), `arrivalRate`, `saturatedThroughput`,
`perReplicaCapacity`, `replicasImplied` and `heldAtFleet`; the per-replica `replica-capacity-decision` line carries
`saturatedThroughput` every cycle, 0 until saturation has been observed.

What it does not do: size a **backlog**. A queue of 350 requests is charged by
the analyzer as 350 requests' worth of resident KV -- the whole backlog held at
once -- which on the run above was three to four replicas on top of the two the
load needed, ordered against pods that take 150s to start and arrive after the
backlog is gone. That is the analyzer's deliberate convention (see
`waitingQueueDemand`), and it is the open question in
[Sizing a backlog](../proposals/backlog-sizing.md).

Two further properties of `W` worth knowing:

- It is **completion-weighted and lags**. `avg_service_time` is
  `rate(sum)/rate(count)` — a mean over requests *completing* in the window. When
  the mix shifts toward longer requests, the completions in-window are the short
  ones that started recently, so `W` under-reads while the fleet fills with
  expensive work. "W tracks current load" is weaker than it sounds.
- A prefix-cache-heavy window does not collapse it: cache hits skip prefill, but
  decode dominates service time (0.055s against 24.5s measured), so `W` barely
  moves.

## Why it binds more often than "floor" suggests

`avgIn + avgOut` prices a request at its **peak** — a request holds its input for
its whole life while its output accumulates, so the KV it occupies averaged over
its lifetime is nearer `avgIn + avgOut/2`. The peak is this analyzer's existing
convention (see `waitingQueueDemand`, which calls I+O "a request's KV footprint at
its LAST decode step, not its mean"), so against a mean-measuring occupancy the
floor sits about 11% high and binds routinely rather than only during a collapse.

**The 11.3× gap observed on that run is not explained.** It is far larger than the
11% convention bias, and an earlier draft of this document proposed that occupancy
is a point-in-time gauge while the floor derives from windowed means. That is
wrong in both premise and direction: occupancy's KV usage is
`max_over_time(vllm:kv_cache_usage_perc[1m])`
(`internal/collector/registration/saturation.go`), windowed over the *same* minute,
and it takes the **peak** while the floor's inputs are means — which biases
occupancy *upward* and so predicts the opposite sign. What accounts for the
remaining gap is an open question, and until it is answered the floor is
correcting a discrepancy whose cause is unknown.

## Interaction with the conceded-replica clamp

A separate change caps supply at the replica count an in-flight scale-down has
already committed to (`steadystate.clampReplicaCountToScaleTarget`). Whether it is
present depends on the branch: it is NOT on `feat/arrival-rate-demand-floor`, and
it IS on `feat/clamp-plus-floor`, where the two run together for the first time.
Check before relying on anything below.

If both land they push the same way — the clamp lowers supply, the floor raises
demand, and `RequiredCapacity = demand/scaleUp − anticipatedSupply` responds to
both. Worked on the numbers from the run above (floor 1,722,000, `scaleUp` 0.85,
`prc` 550,758, an in-flight scale-down at `curr=3` against 5 still-reporting
replicas): each alone leaves RC at zero, while both together give RC ≈ 373,608,
or **about two-thirds of a replica** — enough to ask for one back and reverse the
scale-down in progress.

That is the intended damping rather than a defect, but it is **emergent**: neither
change produces it alone, so neither branch's tests can catch it. It needs an
integration test at whatever point both mechanisms are present.

## Claims in this document, and their status

| Claim | Status |
|---|---|
| Demand fell 7.5M → 152k as the queue drained | Measured, run `biran-20260822-021153-340` |
| `W ≈ avgOut × ITL` reproduces measured service time to 0.2% | Measured (24.60s vs 24.66s), decode-dominated shape only |
| Prefill was 0.055s of 24.6s | Measured, same run |
| Peak-vs-mean convention biases the floor ~11% high | Arithmetic from the request shape |
| Observed floor/occupancy gap of 11.3× | Measured; **cause unexplained** |
| `W` inflates under contention | **Measured**: 2.0s at six replicas, 16-52s at one, same load and shape, run `biran-20260915-102548-571` |
| The arrival floor tracks occupancy rather than holding the fleet | Measured, same run: floor within 3-40% of occupancy on every binding cycle; scale-down to one authorised at 88k tokens |
| `mu` = 5.4 req/s at 6000/1000, 2.5 req/s at 1000/4000 on an H200 | Measured, same run, from the pod's own completion counters |
| The throughput floor holds the fleet at 2 and 3 decode replicas on that run | Arithmetic on logged values; **not** replayed on a cluster |
| Clamp + floor gives RC ≈ two-thirds of a replica | Arithmetic on logged values; **not** reproduced on a cluster |
