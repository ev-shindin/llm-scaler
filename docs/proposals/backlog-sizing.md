# Sizing a backlog: what the shape-swap run showed

**Status:** analysis of one run, three fixes landed on `fix/shape-swap-overprovision`
(the third of them a new floor), and one open design question with a proposed
direction. Nothing here has been re-run on a cluster; every "would have" below
is arithmetic on the values the controller logged, and says so.

Run: `biran-20260915-102548-571/results/guidellm-1789457189-u8bjb3_1` on the
`benchmark-data/shape-swap-6k1000-1k4000-rps6-2026-09-15` branch. P/D
disaggregated Qwen3-0.6B on H200, one variant per role, `kvCacheThreshold`
0.8, `queueLengthThreshold` 5, `scaleUpThreshold` 0.85, `scaleDownBoundary`
0.7, optimize interval 15s, cost-aware (unlimited) optimizer. Load: guidellm
replay at a constant 6 req/s, 6000 in / 1000 out for 18.3 minutes, then
1000 in / 4000 out for 18.3 minutes. Controller code: `llm-scaler/main` at
`67229a01` (every logged `file:line` matches it).

## What was seen

The decode target's desired replica count over the run, with the count of
replicas actually Ready:

```
07:26  1/1        load starts
07:29  3/1        one decode replica saturates
07:30  7/2        peak order; 6 Ready by 07:33
07:35  6/6 -> 5 -> 3
07:38  1/1        back to one, at the same 6 req/s
07:39  2/1        the one replica saturates again
07:43  9/3        second peak; only 3 ever became Ready
07:47  8/3 -> 7 -> 6 -> 5 -> 4 -> 3
07:57  2/2        phase 2 (1000/4000) on two replicas, service time climbs 22s -> 81s
08:04  6/2        run ends; 6/3 two minutes later
```

Prefill: 1 -> 4 (07:30) -> 1 (07:38) -> 6 (07:42) -> 1 (07:53). Its own KV
occupancy never exceeded 0.9 of one replica.

Two things are wrong with this and they are not the same thing. The **peaks**
(7, 9) are three to four times what the load needed. The **troughs** (1, 2)
are below it, and each trough caused the next peak. Median TTFT for the run
was 103ms and p95 was 45s: the p95 is the two saturation episodes.

## What one replica can do

The controller's capacity model reasons in KV tokens: memory-bound k1
(0.8 x 1.16M = 930k per replica) and compute-bound k2, learned as the
occupancy seen when the queue was saturated (1.0-1.1M here). Neither is a
throughput. From the decode pod that lived through the whole run, per minute
(`vllm:request_inference_time_seconds` and `request_generation_tokens`
deltas, `svc.py` in the session scratch):

| time | fleet | batch | ITL | W (service time) | completions/s |
|---|---|---|---|---|---|
| 07:28 | 1 decode | 158 | 27 ms | 26.6 s | 5.4 |
| 07:29 | 1 | 178 | 50 ms | 37.1 s | 3.3 (KV 100%, preempting) |
| 07:34 | 6 | 1 | 2.0 ms | 2.0 s | 1.05 |
| 07:47-07:56 | 3 (phase 2) | 40 | 5 ms | 20 s | 2.1 |
| 08:02 | 2 (phase 2) | 218 | 17 ms | 65 s | 2.5 |

So one decode replica sustains ~5.4 req/s at 6000/1000 and ~2.5 req/s at
1000/4000. At 6 req/s the fleet needs **2 decode replicas in phase 1 and 3
in phase 2**. That is the yardstick for everything below. It also shows why
the analyzer could not see it: service time varies 25x with the batch, so
any quantity built from the W of the moment (occupancy, or Little's law on
that W) describes the fleet, not the load.

## Where each decision came from

Every number is from the `analyzer-result` / `scheduler-queue-demand` /
`replica-capacity-decision` lines. The engine orders
`ceil((D / 0.85 - anticipated) / P)` replicas per role.

**07:30:23, decode 4 -> 7.** Demand 4,039,843 = resident KV 1,149,823 +
local queue 959,000 (137 waiting x 7000) + EPP flow-control queue 1,931,020
(213 requests, priced from bytes at 4 bytes/token plus 213 x 500 output
tokens -- see fix 1 for the 500). Anticipated supply was 3 x 929,792 although
the target had 4 replicas (fix 2). Ordered: +3.

Decomposed against the yardstick of 2:

| demand counted | replicas the engine sizes to |
|---|---|
| resident KV only | 2 (already ordered at 07:27:38) |
| + local engine queue | 3 |
| + EPP flow-control queue | 6 |
| + anticipated-supply undercount | 7 |

**07:30:23, prefill 2 -> 4.** Demand 2,359,897 = own KV 535,484 + EPP queue
1,824,413. The EPP queue's *input* tokens are charged to prefill in full
(`estimateSchedulerQueueDemand`): 213 queued prompts as 213 x 6000 tokens of
prefill KV held simultaneously. Without that term prefill never exceeds 2
in the run, and reaches 2 only for a minute at 07:42 when its own occupancy
peaked at 824k -- KV held *for decode*, blocks awaiting transfer while decode
had none free, which more prefill replicas do not help.

**07:42:08-07:42:54, decode 6 -> 8 -> 9, prefill 4 -> 6.** The same
composition with a deeper EPP queue (355-408 requests, 3.5-3.7M tokens).

**07:33-07:38, the trough.** Six decode replicas Ready, occupancy 100-140k
tokens for the model (each replica at batch ~1), no queue anywhere. Spare
capacity 5.4M tokens = five replicas; ordered 7 -> 2, then 1. The arrival
floor bound at 07:35:08 -- at 88k tokens against 85k occupancy, because W
had fallen to 2.0s. The HPA (biran's 50%/120s, 180s stabilization) spread
the descent over three minutes; the controller's own recommendation was 1.
The load then saturated that replica within 70 seconds.

**07:54-07:57, the second trough.** Three replicas in phase 2, occupancy
~400k, floor 550-640k (W 19s), ordered 3 -> 2. At 2 the batch climbed to
256 per replica and W to 81s; the P1 observation at 08:03 read k2 = 800k and
the run ended with an order for 6.

## Fixed on this branch

1. **Prefill readings in the model-level averages** (`30e2f7d2`). A prefill
   replica's output length is ~1 and its service time ~0.11s, by role. The
   unweighted mean over both roles halved the output length charged per
   queued request (500 vs 1000 -- the number above), and the floor's lower
   median read prefill's W whenever prefill replicas equalled or outnumbered
   decode ones, which on this run was the whole 1/1 window at 07:38-07:39.
   The completion-rate fallback for lambda also counted every P/D request
   twice. All three now read the decode side.

   *On the run:* the queue charge grows by 213 x 500 tokens at the peaks (a
   quarter of a replica -- the direction is up, and this fix is about
   correctness, not the over-provisioning); the floor is unaffected except
   in the 1/1 window, where it was already too small to matter (see 3).

2. **A Ready-but-unscraped replica counted as absent** (`963149d9`). The
   analyzer takes supply from the rows that reported and pending from the
   target's not-ready count, and a replica that turned Ready between the
   last scrape and the cycle is in neither. Anticipated supply undercounted
   by one, the shortfall ordered one more, and it stayed.

   *On the run:* 07:30:23 orders 6 instead of 7. One occurrence in 231 cycles.

3. **The throughput floor** (`2da5576c`). The P1 moment that records k2 now
   also records the replica's completion rate under saturation, and each
   role's demand is floored at `(lambda / mu) x P`. Details and properties in
   the [demand floors guide](../developer-guide/saturation-demand-floor.md).

   *On the run, by arithmetic:* mu = 5.4 at 07:29 (the max over the window;
   the mean of the first three readings, 5.4, 3.3, 3.5, would be 4.1). At six
   replicas the floor is 6/5.4 x 930k = 1.03M tokens; decode's spare is
   5.58M - 1.03M/0.7 = 4.1M, four replicas, so 6 -> 2 rather than 6 -> 1; at
   2 the spare is 1.86M - 1.47M = 0.39M, less than one replica: **held at
   2.** No second saturation, no second peak. Only the two replicas that had
   served phase 1 carry a mu (the four new ones report no completions yet and
   read the `short` bucket); two is enough, the floor takes the median over
   the replicas that have one.

   Phase 2 is weaker. 1000/4000 shares the `long` output bucket with
   6000/1000, so the window still holds the 5.4 readings when the scale-down
   3 -> 2 happens at 07:57, and the floor holds 2, not 3. The P1 observations
   from 08:03 record mu = 2.5, and once they have pushed the phase-1 readings
   out of the 10-sample window -- about five cycles at two replicas -- the
   floor is 6/2.5 = 2.4 replicas' worth and holds 3. A key that told the two
   shapes apart would catch it at 07:57; the bucket boundaries are
   `ShortOutputThreshold` / `MediumOutputThreshold` in `constants.go`.

   What this does not change: the peaks. The floor only raises, and at the
   peaks the queue charge is already far above it.

## Open: a backlog is charged as residency

The peaks come from one convention. `waitingQueueDemand` and
`estimateSchedulerQueueDemand` price every queued request at its full KV
footprint (input + output), and the engine sizes the fleet to hold all of it
at once. The code says this is deliberate: "under-provisioning decode
capacity causes preemption and recompute thrash, which costs more than a
spare replica." On this run it ordered five spare replicas at the first
peak and seven at the second, each taking 145-160s to become Ready -- and a
backlog of 350 requests at 6 req/s arriving is 58 seconds of arrivals, which
two replicas at 5.4 req/s each clear in about two minutes and three in one.
The flow-control queue was empty by 07:31:08 with two replicas Ready; the
five that became Ready at 07:32-07:33 found nothing to drain and were
removed as spare over the next five minutes.

A backlog needs **throughput**, not simultaneous residency. With mu on
record, the replicas needed to clear a backlog of B requests within T
seconds while keeping up with lambda is

```
N = (lambda + B / T) / mu
```

For B = 350, T = 60s, mu = 5.4: N = 2.2 -> 3 (through the 0.85 headroom, 3).
Against the 7 and 9 that were ordered. T is a policy: how long a request may
sit in the queue before capacity that is not yet running is ordered for it.
It should not be shorter than the replica start time, because a replica
ordered to clear a backlog in less time than it takes to start clears
nothing.

Proposed direction, not built:

- Keep the resident-KV term as is. It is measured, and it is what k1 bounds.
- Keep the local engine queue's residency charge for decode: those requests
  are admitted or about to be, and the preemption argument applies to them.
- Replace the EPP flow-control queue's charge with the throughput form above,
  per role, using the role's mu when one is on record and falling back to
  today's residency charge when none is (a fleet never seen saturated has no
  mu, and the residency charge is at least an opinion).
- **Stop charging the EPP queue's input tokens to prefill as residency.** A
  prefill replica holds a prompt's KV for its prefill time plus the wait for
  decode to pull it -- ~0.1s when decode is healthy -- and the only thing that
  makes it hold more is decode being saturated, which more prefill replicas
  do not fix. On this run every prefill replica beyond the first was ordered
  by this term. Prefill's mu is never observed (its queue does not fill), so
  its EPP share would be the fallback until it is; the fallback should be
  bounded by prefill's own occupancy, not the whole queue.

A second, smaller question in the same area: cold decode replicas report
`avgOutputTokens` 0 for their first minutes and read k2 from the `short`
bucket (807k on this run against 1.0-1.1M in `long`), so a fleet mid-scale-up
is priced at a lower P than the one it will have. Keying a replica with no
completions on its variant's bucket rather than its own would close it.

## Also seen, not the analyzer's

- **The order was unattainable.** The second peak asked for 9 decode + 6
  prefill; the fleet never had more than 9 Ready pods in the run (6 + 3 at
  07:33), one decode pod created at 07:39:43 was never Ready, and the run
  ended with 6 desired and 3 Ready. The cost-aware optimizer is unlimited by
  design; the physical limiter that would have capped this was not enabled.
  The benchmark's replica-status report shows desired and Ready but not
  Pending, so the gap reads as slow starts rather than as a cluster that was
  full.
- **The HPA policy in the run** was 50%/120s with a 180s stabilization window
  (biran's `fix/scaledown-defaults`, unmerged), reconstructed from the desired
  transitions. It slowed the descent; it could not change the recommendation
  it was descending to.
- **The EPP queue's per-request size** was priced from bytes at 4 bytes/token
  (7.3MB / 213 requests = 8,565 tokens against a 6000-token prompt). The
  count x avgInput estimate was closer and lost to the max. 43% high, on a
  term that is itself the open question above.
