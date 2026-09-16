# The throughput floor

Why the saturation analyzer floors its demand at what the offered load requires,
what the floor is and is not invariant to, which of the claims here are measured
versus assumed -- and why the floor it replaced, built on Little's law, was
retired.

Code: `internal/engines/analyzers/saturation_v2/throughput_floor.go`.

## The problem

Saturation's demand is **occupancy**: resident KV plus the waiting-queue
footprint. Both are states of the *fleet* rather than properties of the *load*,
and both shrink as capacity grows -- resident KV because residence time falls, the
queue because it drains. The signal that sizes the fleet is therefore a function
of the fleet, and once the fleet is adequate the signal decays toward zero and
takes the replica count with it.

Measured on an H100 at 14 QPS (run `biran-20260822-021153-340`): demand fell from
7.5M tokens to 152k as ten replicas drained a 910-deep queue, and the target
followed from the ceiling down to one, mid-load. And on an H200 at 6 QPS (run
`biran-20260915-102548-571`): six replicas measured 100k tokens of occupancy
for the whole model, the target went to one, the one replica saturated within
a minute, and the loop ran twice in 38 minutes.

## The floor

What a replica can do does not move with the fleet: its completion rate when it
is saturated. With the queue over `queueLengthThreshold` the engine is completing
requests as fast as it can for this shape, so the P1 moment that records k2
(occupancy under a saturated queue) also records the replica's completion rate,
`mu`, under the same history key. The fleet then needs `lambda / mu` replicas to
keep up, which in the analyzer's (demand, per-replica capacity) contract is a
demand of

```
floor_role = (lambda / mu_role) x P_role      tokens
demand     = max(occupancy, floor)            never lowers, only raises
```

applied per role on a P/D fleet (every request passes through both roles, so
each must keep up with all of it) and on the total otherwise. `lambda` is the
scheduler's arrival rate, or the generating replicas' completion rate when the
scheduler reports none.

Properties, each with a spec in `throughput_floor_test.go`:

- **Invariant to fleet size.** `mu` is a per-replica constant, so the floor is
  the same at one replica and at six.
- **A max over the window, not a mean.** A saturated completion rate can only
  under-read its capacity -- in the first minute after a replica fills, the
  requests completing are the few admitted first; under KV pressure preemption
  drops it further -- and it cannot over-read, since nothing completes faster
  than the engine runs. One run's readings for one replica were 5.4, 3.3 and
  3.5 req/s while it was demonstrably completing 5.4. The floor divides by
  `mu`, so a mean of under-reads would order replicas that are not needed.
- **A hold, not an order.** The floor is capped at `scaleUp x anticipated
  supply` for the role, with `scaleUp` read the way the engine reads it
  (`AnalyzerThresholds`, so a per-analyzer override applies to both) -- the
  largest demand the engine's `RC = D/scaleUp - anticipated` turns into
  nothing. Above that it would stop holding the fleet and start growing it,
  and with a `mu` that under-read it would keep growing it every cycle.
  Scale-up stays with occupancy and the queues, which read well while a fleet
  is behind; the worst a bad `mu` can do is refuse one scale-down.
- **Ready pods and own replicas only.** The collector leaves a not-Ready pod's
  completion rate in place (only its timing is dropped), and a bridge's rate is
  the pool's, not the variant's.
- **Silent for a role never seen saturated.** Prefill, in practice: its queue
  is rarely the one that fills. No opinion rather than a guess.
- **Keyed by output length**, in factor-of-two buckets above 500 tokens
  (`classifyOutputLength`). `mu` falls roughly with output length, and two
  shapes sharing a bucket share a window whose max is the shorter shape's --
  which held a fleet at the shorter shape's size while the longer one was
  served. Measured on the shape-swap benchmark, where 1000 and 4000 tokens
  shared one bucket; they no longer do.

A floor that binds logs `throughput-demand-floor` with `demandBeforeFloor`,
`arrivalRate`, `saturatedThroughput`, `perReplicaCapacity`, `replicasImplied`
and `heldAtFleet`; the per-replica `replica-capacity-decision` line carries
`saturatedThroughput` every cycle, 0 until saturation has been observed.

## What it is not invariant to

`mu` is learned only at saturation, and saturation is defined by the queue.
A replica that is falling behind by growing its running batch rather than its
queue -- admitting everything while its inter-token latency climbs -- has not
saturated by that definition and records nothing. Measured on the shape-swap
benchmark's second phase (1000 in / 4000 out at 6 req/s): two decode replicas
took their batch from 75 to 229 against a `max-num-seqs` of 256 over four
minutes, ITL from 5.8 to 19.9 ms, completing 5.5 req/s between them against 6
arriving, with `waiting` never above 1. Until the batch reaches the engine's
own ceiling and a queue forms, the floor has no `mu` for that shape and holds
whatever the previous shape's was.

That window closes on its own -- the batch does reach the ceiling -- provided
nothing orders a replica first and drains the batch before saturation is
observed. Which is what the retired floor did.

## Why the Little's-law floor was retired

The first version of this file built a floor from the load directly: `lambda x
W x (avgIn + avgOut)`, with `W` the per-request service time the engines
report. The premise was that `W` is a property of the load. It is not: service
time is output length times inter-token latency, and ITL grows with the batch a
replica runs, so `W` tracks how many replicas share the load. Measured on one
decode pod across a run at a constant 6 req/s and a constant 6000 in / 1000 out:

| replicas sharing the load | batch on this pod | ITL | `W` |
|---|---|---|---|
| 6 | ~1 | 2.0 ms | 2.0 s |
| 2-3 | 40-78 | 4-6 ms | 15-22 s |
| 1 | 136-178 | 18-50 ms | 16-52 s |

A 25x range at the same load and shape, so `lambda x W x tokens` with the `W`
of the moment is current occupancy restated (`L = lambda W` is an identity). On
the way down that made it useless: at six replicas it read 88k tokens against
85k occupancy and let the fleet go to one. On the way up it made it harmful,
and that is the measurement that retired it. In the shape-swap benchmark's
second phase, with the throughput floor holding two replicas:

```
13:25:47  W=36s   floor 1.29M -> ordered the 3rd replica   (occupancy 0.75M)
13:28:32  W=77s   floor 2.50M -> ordered the 4th           (occupancy 1.37M)
13:31-34  W=15s   floor 0.40M -> scale-down to 2           (occupancy 0.22M)
13:37:47  W=57s   floor 1.65M -> the 3rd again
13:40:32  W=76s   floor 2.41M -> the 4th again
```

At two replicas the load "needed" 2.5M tokens; at four, 450k. Each fleet size
justified the move to the other, with a ten-minute period. And every one of
those orders landed *before* the two replicas reached saturation, so the
throughput floor never got the `mu` that would have held three -- the floor
that oscillated was also the one preventing the floor that would not.

Its stated purpose, damping the collapse, is what the throughput floor does
with a term that does not move. What it did beyond that -- ordering replicas on
an inflated `W` -- is what occupancy and the queues do already, later and for
the right reason. So it was removed rather than corrected; there is no `W` that
is a property of the load.

## Claims in this document, and their status

| Claim | Status |
|---|---|
| Demand fell 7.5M -> 152k as the queue drained | Measured, run `biran-20260822-021153-340` |
| Six replicas read 100k tokens; target went to one; re-saturated in a minute | Measured, run `biran-20260915-102548-571` |
| `W` ranges 25x with the batch at one load and shape | Measured, same run, one pod's own counters |
| `mu` = 5.4 req/s at 6000/1000, ~2.75 at 1000/4000 on an H200 | Measured, that run and the 2026-09-16 re-run |
| The throughput floor held two decode replicas through phase 1 | Measured, 2026-09-16 re-run (`throughput-demand-floor` at 13:12-13:25) |
| Two replicas grew their batch 75 -> 229 with `waiting` <= 1 in phase 2 | Measured, same re-run, pod counters |
| The retired floor ordered every phase-2 scale-up, at `W` 36-77s | Measured, same re-run, controller log |
| Retiring it and splitting the buckets holds phase 2 at three replicas | Arithmetic on logged values; **not yet re-run** |
