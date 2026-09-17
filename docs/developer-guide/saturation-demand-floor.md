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
floor_role = ((lambda + B_role / T) / mu_role) x P_role    tokens
demand     = max(resident KV, floor)                        never lowers, only raises
```

applied per role on a P/D fleet (every request passes through both roles, so
each must keep up with all of it) and on the total otherwise. `lambda` is the
scheduler's arrival rate, or the generating replicas' completion rate when the
scheduler reports none. `B` is the role's backlog -- the requests waiting in
its engines' queues plus the scheduler's -- and `T` the drain target
(`BacklogDrainSeconds`, 60): the fleet asked for clears the backlog in about
`T` while keeping up with `lambda`. For a role with a `mu`, the queues enter
the model this way and their residency charge (every queued request at its
full KV footprint, as if all had to be resident at once) comes out; a role
with no `mu` keeps the residency charge, except prefill's share of the
scheduler queue, which is dropped -- a prefill replica holds a prompt for its
prefill time plus the hand-off, and the only thing that makes it hold longer
is decode being saturated, which more prefill replicas do not fix.

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
- **An order as well as a hold, and the order comes first.** The first
  version was capped at the fleet's own size, so that scale-up stayed with
  occupancy and the queues. Measured on three runs of the shape-swap trace,
  that put the second replica's order at +69-78 s after load start, from
  occupancy crossing k1 -- and the lone replica, at 94% of its saturated rate
  from the first cycle, tipped into preemption at +91 s on two of them and
  +144 s on the third. With a 60-100 s start the replica landed after the
  tip, and the queue that built in between was sized as five to seven extra
  replicas. `lambda / mu = 0.94` through the engine's scale-up headroom orders
  that replica in the first cycle `lambda` is measured. There is no cap; the
  bound is the formula, which does not move as replicas are added. What a
  `mu` that under-read costs is bounded by the under-read, and the window's
  max corrects it upward at the next saturation.
- **Two readings may hold but not order**, and for those the cap at
  `scaleUp x anticipated supply` stays (`heldAtFleet`, `heldWhy` on the log
  line). A `mu` *borrowed* from a neighbouring bucket is wrong in a known
  direction and, from a longer shape, would over-order. A window with a
  *single* reading is the first cycle's under-read (3.67 against a true 7.13
  on the run), and an order on it over-provisions in a way that removes the
  saturation which would have recorded the second, corrected reading
  (`MinThroughputSamplesToOrder`, 2). Neither changed the measured runs:
  every order on them came from a window with several readings of its own.
- **A backlog is throughput, not residency.** 350 queued requests at 6 req/s
  arriving are 58 s of arrivals; two replicas at 5.4 req/s each clear them in
  about two minutes and three in one. Charged as resident KV they were five
  replicas, each arriving after the queue was gone. `B / T` prices them as
  what they are.
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
  shared one bucket; they no longer do. A bucket with no reading borrows the
  nearest bucket's under the same key prefix until it has its own -- without
  that, the first cycle of a new shape had no floor at all, and a
  three-replica fleet was sized to one from 400k tokens of occupancy. The
  per-replica log line names the bucket the figure came from
  (`saturatedThroughputBucket`).

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
| Splitting the buckets with no fallback loses the floor on a new shape (3 -> 1) | Measured, run `guidellm-1789577844-0smu2m_1` |
| Retired floor + split buckets + borrow: phase 2 goes 2 -> 3 -> 4 and holds, `mu` = 2.97 learned at the one saturation | Measured, run `guidellm-1789581140-kb3q2v_1` |
| The learning saturation costs one window at 4.8 s p95 TTFT | Measured, same run, engine histograms per 5-minute window |
| The second replica's order came from occupancy at +69-78 s on three runs; the lone replica tipped at +91 / +91 / +144 s | Measured, runs `wm9k0y_1`, `ydqs8h_1`, `t1pclo_1`, the first replica's own counters |
| A 350-request backlog charged as residency ordered five extra replicas that arrived after it was gone | Measured, run `biran-20260915-102548-571` |
| `(lambda + B/T) / mu` orders the second replica at +62 s and sizes the first-ramp backlog at 3 replicas | Measured, run `guidellm-1789645863-l32fnc_1` (warm controller) and `guidellm-1789642083-9yngog_1` (cold) |
| The first reading at a saturation under-reads and the window's max corrects it | Measured, same cold run: 3.67, 5.23, 7.13 req/s on three consecutive saturated cycles |
