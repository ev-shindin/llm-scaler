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
- **A prefill saturation records only while decode is not saturated, and
  prefill is held meanwhile.** A prefill request is done when decode admits
  it and pulls its KV, so with a decode replica over its queue threshold what
  prefill shows -- the KV of finished prompts held until the pull, arrivals
  the scheduler's flow control releases in bursts, a completion rate that is
  decode's admission rate -- is metered by decode: decode's saturation seen
  from upstream. Measured on the shape-swap benchmark's cold pass
  (2026-09-18): the one prefill replica read queue 30 and 357 800 resident
  tokens for the four cycles both decode replicas were over the threshold
  (+220..+265 s after load start), was priced at k2 = 357 800 against a k1
  of 919 859 and `mu` = 5.57 req/s, and its own occupancy read 100 % of that
  k2 and ordered a second prefill replica on the spot. The `mu`, under the
  6 req/s offered, then held both (`lambda / mu` 1.08 at the median, 0.98
  to 1.17 between the 10th and 90th percentile of the cycles) for the
  remaining 33 minutes while their resident KV read at most 54k -- and
  nothing could correct either figure, since a prefill fleet of two never
  saturates again. The correlation is what was measured; the path is not
  settled (prefill's KV sat at 31 % of its cache on those rows, so it was
  not block-starved). The analyzer now leaves such a reading unrecorded
  (`P1-obs-downstream` on the `k2-decision` line; k2 falls through to
  history or k1, and no `mu` is written), and clamps prefill's demand for
  the cycle into the band where the engine neither orders nor releases,
  `[scaleDown x supply, scaleUp x anticipated supply]`
  (`prefill-demand-held`): against a small prefill k2 learned on some
  earlier, genuine saturation the held KV alone would read as several
  replicas, and against k1 on a fleet of two it reads as a release (537 800
  on 1 839 718 is 29 %). What the floor of the band buys is not a re-order
  avoided -- a replica released mid-episode would stay released, prefill
  reads near zero after one -- but the burst that ends an episode: the
  scheduler's flow control releases what it held in one go (124 requests,
  744k prompt tokens against a 919k k1 on the measured run), and it lands
  on prefill first. A genuine prefill order is deferred for as long as any
  decode replica's one-minute peak reads full and queued, plus the memory
  below: 195 s and two decode starts replayed on the measured cold pass,
  135 s and one on the warm, unbounded while decode is capped and cannot
  grow. A prefill bottleneck
  reduces decode's arrivals, so the two saturate at once only when decode
  is short at prefill's completion rate. "Decode saturated" is a decode
  replica full AND queued -- resident KV at its k1, queue at the
  threshold -- not the queue alone: vLLM counts a request waiting for its
  remote KV in `num_requests_waiting`, so a fleet whose transfer keeps as
  many in flight as the threshold (a large model over a slow link) would
  read saturated every cycle on the queue alone, and prefill would then
  never record and, held, never be ordered. Full is decode unable to
  allocate the blocks a pull needs, which the transfer pipeline does not
  produce. The test is remembered for the collector's row window
  (`DecodeSaturationMemory`, one minute): on the measured episode decode's
  occupancy dropped under k1 on the fourth cycle while its queue and
  prefill's row -- a one-minute max, repeated to the token -- had not
  moved, and that row would otherwise have recorded. A decode bound by its
  sequence ceiling before its KV does not gate; that is a prefill
  mislearned low, which costs money, where a prefill that cannot be
  ordered costs latency. Newer vLLM splits the waiting count by reason
  (`vllm:num_requests_waiting_by_reason`, `reason="capacity"` for the
  scheduler's own queue); reading that for both this test and P1 itself is
  the follow-up, once the vLLM the stack ships is known to carry it.
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
| A prefill saturation under a saturated decode, recorded as prefill's, ordered a second prefill replica and held it 33 minutes at near-zero resident KV | Measured, run `guidellm-1789743465-ckjz7y_1` (cold), full controller log of the pass (`wva_controller_full.log` beside the results; the harness's own copy starts at 15:09:46): prefill `P1-obs` at 15:01:33-15:02:18 with both decode replicas at queue 36-81, then `replicasImplied` for prefill 1.08 at the median (p10-p90 0.98-1.17, above 1.0 on 86 % of 133 cycles) through 15:35 |
| The persisted prefill `mu` alone ordered a second prefill replica at +51 s on the warm pass that followed, at zero resident KV | Measured, run `guidellm-1789746634-pov4xp_1`: `throughput-demand-floor` for prefill at 15:51:35, `arrivalRate` 5.5, `saturatedThroughput` 5.57, `residentDemand` 0, `replicasImplied` 0.99 |
| Left unrecorded, the same cycle prices prefill at k1 with no order | Replayed from the run's rows in `downstream_saturation_test.go` |
| With the gate, the hold and the memory, prefill stays at one replica on every pass; the hold engages only while decode is full and queued | Measured, runs `guidellm-1789760128-zphd9d_1` (cold: 11 `prefill-demand-held` cycles at +130..+340 s, 0 `P1-obs-downstream`, all-pod 137.9 GPU-min against 167.8), `guidellm-1789768002-eb450o_1` (cold again: 9 and 0, 142.1, p95 241 ms) and `guidellm-1789763936-mr6h9x_1` (warm: 9 and 0, 132.9 against 168.0, p95 194 ms) |
