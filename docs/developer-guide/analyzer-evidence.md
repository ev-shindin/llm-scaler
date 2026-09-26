# Analyzer evidence

What the scaling signals were measured to do, and on which pass. The code
states the rule it follows; this file is why that rule and not another one,
with the run behind each figure so a reader can disagree with the evidence
rather than with an assertion.

Every entry names its date and its run. A figure without one of those is not
evidence, and does not belong here.

The stands: CoreWeave 8 x H200 nodes, Qwen3-0.6B on a P/D fleet, guidellm
driving a replayed trace at a constant arrival rate, the shape switching once
mid-run. `k1` (the memory-bound per-replica capacity the analyzer prices with)
is 930,508 tokens there; the engine's physical KV capacity is 1,163,136.

## The throughput floor

### Occupancy alone under-sizes a fleet that is keeping up

*2026-09-15, run `biran-20260915-102548-571`, 6 req/s at 6000/1000.*

Occupancy -- resident KV plus the waiting queues -- is a state of the fleet,
not a property of the load, and it falls as replicas are added. So is anything
built from a per-request cost measured on the current fleet: the service time
the engines report is ITL x output length, and ITL grows with the batch a
replica is running. Measured on one decode pod across the run:

| replicas sharing the load | batch | ITL | W (service time) |
|---|---|---|---|
| 6 | ~1 | 2.0 ms | 2.0 s |
| 1 | 136-178 | 18-50 ms | 16-52 s |

A 25x range in W at the same load, so `lambda x W x tokens` with the W of the
moment is current occupancy restated. The arrival-rate floor built that way
read 88k tokens at six replicas and authorised the scale-down to one; on the
way up it did the opposite harm, pricing the load at the inflated W of a fleet
that was behind, ordering replicas before any had saturated, and releasing
them once they had brought W back down -- a 2-4 replica oscillation with a
ten-minute period on the second phase (`docs/proposals/backlog-sizing.md`).

What a replica can do does not move with the fleet: its completion rate when
saturated. On the same run, mu read 5.4 req/s at 6000/1000 and 2.5 req/s at
1000/4000 against 6 req/s arriving -- 2 and 3 decode replicas, which is where
the fleet held when it was left alone at those sizes.

### The floor is not capped at the fleet's own size

*Four passes, 2026-09-16 to 09-19.*

The first version was capped, so that it could hold a fleet but never order
one, and scale-up stayed with occupancy and the queues. That left the order
too late: a lone replica at 6 req/s against a mu of 5.4-6.4 is at 94 % of what
it can do from the first cycle and tips into preemption at 75-145 s, while
occupancy crossed k1 at +69-78 s on every run -- which, with a 60-100 s
replica start, lands the second replica after the tip on most of them.
Uncapped, `lambda / mu` = 0.94 replicas orders it in the first cycle lambda is
measured, some 50 s earlier. What a mu that under-reads costs is bounded by
the under-read; what a late order cost was five to seven replicas at the first
ramp of every run.

Uncapped is safe, not merely better. What the floor can order is
`(lambda + B / T) / mu` by construction -- fixed by the offered load and by
what one replica does when saturated, neither of which moves as replicas are
added. That is the property occupancy lacks, and it is why removing the cap
does not reintroduce the ratchet the cap was there to prevent.

### A backlog is throughput, not residency

*Same stand.* Occupancy charged every queued request at its full KV footprint,
as if all of them had to be resident at once, and the engine sized the fleet
to hold them: 350 queued requests became five to seven extra replicas, each
arriving after the queue was gone. Priced as work to drain -- B requests
within T seconds on top of lambda arriving -- the same backlog is one more
replica.

`BacklogDrainSeconds` is 60 because a drain target shorter than a replica's
start orders capacity that drains nothing, and a replica on these stands takes
60-100 s to become Ready.

### Prefill's share of the scheduler queue is dropped

*Same stand.* A prefill replica holds a prompt's KV for its prefill time plus
the hand-off to decode; the only thing that makes it hold more is decode being
saturated, which more prefill replicas do not fix. Every prefill replica
beyond the first at the first ramp was ordered by that term, and none of them
prefilled anything a single one could not.

## The hold rules

### Two readings, not one, before a reading may order

*Shape-swap trace.* The first reading at a saturation under-reads: a 1-minute
rate on a replica that has been full for 20 s counts a third of a minute's
completions. Three consecutive saturated scrape pairs, 60-90 s apart, read
**3.67, then 5.23, then 7.13 req/s**. An order on the first over-provisions in
a way that removes the saturation which would have corrected it, so
`MinThroughputSamplesToOrder` is 2 -- and the two readings must come from two
rate windows, not the same window read twice.

Letting a single reading order one replica was tried twice and dropped. Against
the anticipated supply it ordered a fourth replica at a phase switch whose
third was still starting; against the running supply it was a ratchet, since
nothing remembered that the reading had already ordered, so once the ordered
replica reported, the same reading ordered the next. Across four passes the one
replica bought a single cycle over the plain hold -- a third replica 30 s ahead
of occupancy, on an under-read that then ran unneeded for 35 minutes -- and the
hold's own cost, replayed, is 15-45 s on the cold ramp's next replica.

### A single reading may order behind a standing queue

*2026-09-25, run 24 pass B, the 8k1000 -> 1k6000 trace at 6 req/s, with the
arrival rate already corrected.*

`MinThroughputSamplesToOrder` held decode at 2 replicas from t+75 s to t+135 s
while the scheduler queue went **32 to 217**. The floor's own figure through
those four cycles was **2.51, 2.67, 2.73 and 3.06** replicas: it wanted a third
and was not allowed to ask. The fleet reached 6 at t+225 and the queue peaked
at 282; the run's whole TTFT tail is that window, p95 26.5 s against a p50 of
114 ms.

The hold's argument has two halves: an order on a first under-read
over-provisions, **and** the over-provisioned fleet then never saturates again
to record the second reading that would have corrected it. The second half
fails while the scheduler is holding work -- the queue keeps the fleet
saturated until it drains, so the correcting reading arrives either way.

A first version also withheld the figure while any replica was starting, on
the reasoning that the attempt above over-ordered "at a phase switch whose
third was still starting". **Run 26 measured that version inert**: zero
firings across a full pass, four single-sample holds in both arms, a queue peak
of 359 against 380. Through every hold cycle the decode deployment had
`spec=2` and `ready=1`, so `PendingReplicas` was 1 and the test blocked the
rule -- a start outstanding is what a ramp is, so it excluded the case it was
written for.

It was redundant besides. A phase switch sets `staleShape`, which the first
condition below already blocks. And the pacing it imitated exists a layer up:
the engine computes `RC = max(0, TotalDemand / scaleUp - TotalAnticipatedSupply)`,
subtracting the supply already on its way, so it cannot re-order what is in
flight.

What makes the climb safe is not pacing but invariance: `(lambda + backlog /
drain) / mu` is fixed by the load and the queue, not by the fleet, so repeated
firings converge on it instead of ratcheting past it -- the same property the
floor's package comment rests on -- and the rule stops firing when the queue
drains. Measured over five cycles with a replica driven pending to ready
throughout: the floor reads 1,818,582 tokens every time, 1.95 replicas, and
does not grow.

Two conditions:

  - **The SCHEDULER's queue, not the merged backlog, and worth more than
    both a second of arrivals and one replica-second of service.** Against
    `lambda` alone the test degenerates as the load falls -- at 0.1 req/s one
    stray request is ten seconds of arrivals, which is jitter. vLLM counts a request waiting for its remote KV in
    `num_requests_waiting`, so a large model over a slow link keeps six or more
    there at all times while decode admits fine -- no further replica drains
    those. A request in the scheduler's queue has not been dispatched to any
    pod at all. The first version of this rule used the merged backlog and
    broke the spec that pins exactly that case.

### A borrowed reading never outvotes a replica's own

*2026-09-20, the 1000/6000 shape-swap trace, cycles 11:45:22-11:47:22.*

A fresh replica's first completions are its short requests -- they finish
first, and a replica with none yet reads an output length of 0 -- so its key
lands in a short bucket with no reading and borrows the previous shape's mu.
Measured: the borrowed figure was **4.38 req/s** from 1000-token outputs while
the replica that had been saturated under the new 6000-token shape read
**1.4-1.7** of its own. Two fresh replicas out of three put 4.38 at the median:
the backlog of 441 requests read as 2 replicas' worth instead of 5, the floor
fell from 10 M tokens to 3 M in one cycle, the target went from 10 to 4, and
the backlog kept growing (256 -> 642) under the figure that said it would not.
The target then swung 10 <-> 4 for ten minutes.

## The capacity windows

### The mu window reads the median, not the maximum

*2026-09-22 rerun, phase 1.*

`Max` was right while mu was a **completion** rate: that error is one-sided,
because a saturated replica's completion rate under-reads while it fills, so
the largest reading is the best estimate of what it sustains. The argument did
not survive the move to a **generation-token** rate, which bursts rather than
under-reads. Measured over the rerun's first phase, the per-replica rate ran
**4,028 min / 7,548 median / 11,663 max**, while the fleet's own total sat at
33,175 tokens/s against a demanded 36,000 -- the typical reading was the true
one and the peak was half again above it.

Read with `Max`, the window ratcheted to a mu of 3.6 req/s and the demand floor
asked for 1.6 replicas where about 8 were needed. The fleet's average
per-replica rate over that window, 4,538 tokens/s, prices mu at 0.76, inside
the 0.61-0.85 the fleet's own queueing implies. The window reads `Median`.

## The arrival rate

### The arrival rate is a dispatch rate while the queue is building

*2026-09-24, the 8k1000 -> 1k6000 shape-swap trace at a constant 6 req/s, both
passes of run 23.*

`QueryModelArrivalRate` read
`rate(inference_extension_scheduler_attempts_total{status="success"}[1m])`.
That counter increments when the scheduler PLACES a request on a pod, so while
the fleet is saturated it measures what the fleet could take, not what the
workload offered. Against the flow-control enqueue counter, which increments
when a request is accepted, over the ramp of one pass:

| t+ | placements (what sized the fleet) | enqueues (what arrived) |
|---|---|---|
| 120 s | 5.38 | 6.10 |
| 180 s | 3.82 | 5.56 |
| **240 s** | **1.66** | **6.16** |
| 300 s | 4.88 | 5.90 |
| 360 s | 5.22 | 5.38 |
| 420 s | 6.50 | 6.50 |
| 480 s | 5.80 | 5.80 |

A **3.7x under-read at the worst moment**, with the scheduler queue at 234
and climbing (it peaks at 356 one sample later) -- and the two agree to the digit from 420 s. The queue itself
drained earlier, at about 270 s; placements stay low for the two samples after
that because the counter is still working through the drain burst, not because
the queue is still full. The error is not noise:
it is largest exactly when the fleet is furthest behind, which is when this
figure orders capacity.

What it cost on that run, arithmetically: `replicasImplied` is
`(arrivalRate + backlog / drainSeconds) / mu`, and at the 1.66 sample that is
`(1.66 + 605/60) / 2.8651` = **4.10**, which is what the log says. Substituting
the arrival rate that was really offered, 6.16, gives **5.67**.

So the 3.7x under-read did not become a 3.7x under-order. The backlog term was
supplying 10.08 req/s against the arrival term's 1.66 and had already absorbed
most of the error; what the under-read cost was **1.6 replicas, one replica
after the ceiling**. That is worth fixing -- one decode replica through a ramp
is the difference the rest of this section describes -- but the floor is not
blind while the queue is growing, because the queue itself is an input. The fleet stalled at 5 decode replicas through
minutes 4 and 5 while the scheduler queue climbed to its peak of 356 at
t+255 s; it was 3 by t+270 and 0 from t+285, so the queue is empty for the rest
of the twenty-minute pass. The run's whole TTFT tail is those first four and a
half minutes -- p95 44.7 s and p99 70.3 s over the pass, against a p50 of
112 ms.

The sibling pass of the same run drew slightly better numbers, held 6 replicas
instead of 5 at the matched offset, peaked at 284 queued instead of 356, and
landed p95 22.4 s. Both passes reached 7 eventually. Same
code, same trace, same hour; the difference is which side of a replica the
under-read happened to fall on.

`deriv(queue_size)` was tried as a correction -- placements plus queue growth
should be arrivals -- and rejected. Measured against the enqueue counter it was
wrong by up to 8.28 req/s and went NEGATIVE (-0.20, -2.38 req/s) at the two
samples where the queue was draining fastest, which is worse than the
under-read it was meant to repair.

## How to add to this file

One section per decision, with the date, the run identifier and the numbers
the decision rests on. Put the rule in the code and the evidence here, and
link from the code to the section rather than restating it: a comment that
carries a measurement has to be re-verified every time the code around it is
edited, and in practice is not.
