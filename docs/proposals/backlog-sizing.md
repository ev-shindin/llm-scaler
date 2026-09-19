# Sizing a backlog: what the shape-swap run showed

**Status:** analysis of one run, three fixes landed on `fix/shape-swap-overprovision`
(the third of them a new floor), one open design question with a proposed
direction -- and a re-run of the same trace with the fixes, in
[Re-run with the fixes](#re-run-with-the-fixes-2026-09-16-kermit) at the end.
Every "would have" in the analysis sections is arithmetic on the values the
controller logged, and says so; the re-run is measured.

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

   Phase 2 is weaker, and the re-run below showed the reason to be different
   from the one first written here (a shared output bucket). See
   [Phase 2, investigated](#phase-2-investigated).

   What this does not change: the peaks. The floor only raises, and at the
   peaks the queue charge is already far above it.

## A backlog is charged as residency -- built on `fix/backlog-by-throughput`

What follows is the analysis that led to it and the direction it proposed;
the implementation is in `throughput_floor.go` and described in [the
throughput floor](../developer-guide/saturation-demand-floor.md). Two things
changed against the direction below, both from measurement: the local engine
queue is priced by throughput too, not only the scheduler's (on the first
ramp it was 180 of the 380 queued requests, and the same argument applies);
and the floor now orders as well as holds, because the second replica's
order was the late one -- see the developer guide.

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
  bounded by prefill's own occupancy, not the whole queue. (It did fill,
  once, under a saturated decode -- see the 2026-09-18 section at the end.)

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

## Re-run with the fixes, 2026-09-16, kermit

The same trace (`shape_6k1000_1k4000_rps6_20m.jsonl`, seed 42: 6693 + 6595 =
13288 requests, exactly the counts Ofer's summary implies), the same
scenario (`guides/pd-disaggregation`, Qwen3-0.6B, one variant per role, H200),
the same scaling policy (0.8 / 5 / 0.85 / 0.7, 15s) and the same HPA
behaviour as the original run (scale-up 100%/5s, window 0; scale-down
50%/120s, window 180s -- Ofer's `fix/scaledown-defaults`, applied by patching
the two ScaledObjects), against the image built from this branch at
`367bd76b`. CoreWeave **kermit** rather than waldorf, which was in use; both
are 8 x H200 nodes, and k1 came out identical (929,894 / 919,449).
Results: `evgensh-20260916-kermit-shapeswap/results/guidellm-1789563884-wm9k0y_1`
(not committed; 500MB of raw scrapes).

What differs from the original and is NOT the code: the controller started
with no k2 history (Ofer's carried `historyWindowLen: 8` from earlier runs);
a decode pod took 90s to Ready here against 145-160s there, so the first
replica was alone for less time; kermit had 44 free GPUs, so nothing the
controller asked for was refused; and the routing sidecar is pinned to
v0.9.0 (see the scenario file -- `latest` had moved past the chart pin and
every decode pod crash-looped on the first standup).

### Decode replicas over the run

```
                  original (waldorf, 09-15)      this branch (kermit, 09-16)
load starts       1                              1
first saturation  3 -> 7  (one replica alone)    2 -> 3  (46 queued for one cycle)
phase 1 steady    7 -> 1  (collapse, 07:35-38)   3 -> 2, HELD at 2 for 11 min
re-saturation     1 -> 9  (second peak)          none
phase 2           3 -> 2 -> 6 at the end         2 -> 3 -> 4 -> 2 -> 3 -> 4
prefill           1 -> 4 -> 1 -> 6 -> 1          1 throughout
max ordered       9 decode + 6 prefill           4 decode + 1 prefill
```

The throughput floor is what held phase 1. From the log at 13:12:46, with
three replicas Ready and occupancy at 136k tokens (one seventh of a replica,
the reading that took the original fleet to one):

```
throughput-demand-floor  role=decode  demandBeforeFloor=135695  flooredTo=1184659
                         arrivalRate=6.2  saturatedThroughput=4.87
                         replicasImplied=1.27  heldAtFleet=false
```

mu was recorded at 4.87 req/s from the one P1 observation at 13:08 (the pod's
first saturated minute -- Ofer's pod, saturated for longer, reached 5.4), so
the floor asked for 1.27 replicas' worth and the scale-down stopped at 2.
The arrival floor bound in the same cycles at 134k tokens, occupancy restated,
exactly as the measurement in the guide predicts.

Phase 2 oscillated 2 -> 3 -> 4 -> 2 -> 3 -> 4 with a ten-minute period,
against 2 -> 6 at the end of the original. [Phase 2, investigated](#phase-2-investigated)
is what the log says about it.

### What the load saw

| | original (waldorf) | this branch (kermit) |
|---|---|---|
| requests / errors | 13288 / 0 | 13288 / 0 |
| TTFT p50 / p95 / p99 | 103 ms / **45.4 s** / -- | 112 ms / **218 ms** / 3.1 s |
| ITL p50 / p95 | 5.4 / 39.7 ms | 12.2 / 21.2 ms |
| request latency p50 / p95 | 21.6 / 90.4 s | 17.0 / 80.1 s |
| concurrency p50 / mean | 134 / 200 | 105 / 179 |
| ready replicas, both roles, mean / max | 4.05 / 9 | 3.32 / 5 |
| decode ordered, max | 9 (6 ever Ready) | 4 (4 Ready) |
| GPU-minutes (avg replicas x run) | -- | 90.7 |

The p95 TTFT is the two saturation episodes in the original and the absence
of a second one here. The higher p50 ITL here is the fleet running at a
higher batch on fewer replicas -- two instead of six for most of phase 1 --
which is the point: the same load served on a third of the GPUs, with the
p95 tail 200x lower.

### Also found while running it

- **`guides/pd-disaggregation` could not stand up on main.** The scenario
  overrode the routing sidecar to `latest`, `patch_harness.sh` pins the
  modelservice chart to v0.4.15, and the two had drifted apart: every decode
  pod crash-looped on `unknown flag: --connector`. Pinned to v0.9.0, the
  sidecar the chart pin was made for, with the reasoning in the scenario.
- The harness's final `kubectl cp` of the 500MB results directory died with
  `read message: %!w(<nil>)` on this cluster, and the run was reported FAILED
  after 3108s of successful load. The results were still on the workload PVC;
  streamed off as a gzip in 40MB chunks with per-chunk checksums, which
  worked first time. Worth a retry-with-chunks in the harness.
- fozzie, the emptier CoreWeave cluster, could not create ANY pod from a
  Deployment on 2026-09-16: its kueue pod webhook denied every one with
  `Deployment.apps ... not found` (a newer kueue revision was crash-looping
  beside the old one). Not touched; noted so nobody spends an hour on it.

## Phase 2, investigated

The controller log of the re-run, cycle by cycle, with the two decode pods'
own counters beside it (per minute, from the raw scrapes):

```
time      N  KVinUse   W(s)  arrival floor   throughput floor   decision       pod batch  ITL    completions
13:24:32  2   514k     22     910k           1.18M (mu 4.87)    hold 2          75       5.8 ms  2.1 /s each
13:25:47  2   750k     36    1.29M           --                 order 3rd
13:26:10  2   993k     50    1.79M           --                 (3rd booting)  184      15.1     2.4
13:28:32  2  1.37M     77    2.50M           --                 order 4th      229      19.9     2.75
13:29:32  3  1.94M     84    2.14M           --                 hold 4
13:31:02  3   885k     43    1.27M           --                 4 -> 3
13:32:02  4   560k     26     731k           1.08M (mu 4.87)    4 -> 2
13:34:42  2   ...                                               (HPA: 4 -> 2 in one 50% step)
13:37:47  2   994k     57    1.65M           --                 order 3rd      178      14.9
13:40:32  3  1.37M     76    2.41M           --                 order 4th      223      18.9
13:43:02  4   316k     47     580k           --                 4 -> 1 (load ended)
```

Two things the earlier explanation got wrong.

**mu was never re-learned, and the bucket is not why.** P1 fires on
`waiting >= queueLengthThreshold`, and nothing waited: the two replicas took
their batch from 75 to 229 (against `max-num-seqs` 256) admitting everything,
while completions per pod went 2.1 -> 2.75 req/s -- 5.5 between them against
6 arriving. They were falling behind inside the running batch, not in the
queue, and by the queue's definition never saturated. The `mu` column is
phase 1's 4.87 throughout because there was no phase-2 reading to mask, in
any bucket. The batch would have reached the ceiling and queued within
another minute or two; it never got there, because:

**Every scale-up in phase 2 was the arrival floor's.** The occupancy
threshold for a third replica is D > 0.85 x 1.86M = 1.58M; occupancy was
0.75M at 13:25:47 and 1.37M at 13:28:32. What crossed the line was the
Little's-law floor at the inflated W of a fleet that was behind: 1.29M at
W=36s, 2.50M at W=77s. Those replicas arrived, took W back to 15s, the same
floor read 400-450k, and the fleet went back to two -- where W climbed again.
Two self-consistent states, each ordering the other. And because the orders
landed before the batch reached the ceiling, the saturation that would have
taught the throughput floor `mu = 2.75` never happened.

What was done (branch `fix/phase2-oscillation`):

- **The arrival-rate floor is retired.** Its purpose -- damping the collapse
  -- is what the throughput floor does with a term that does not move with
  the fleet; what it did beyond that was order replicas on a W it had no
  business pricing with. `docs/developer-guide/saturation-demand-floor.md`
  keeps the measurement.
- **Output-length buckets are a factor of two wide above 500 tokens** (1500 /
  3000 / 6000), so that when phase 2 does saturate its first `mu` is not
  masked by phase 1's max in a shared window.

- **A bucket with no reading borrows the nearest bucket's** until it has
  its own. Found by the re-run of the two changes above without it (run
  `guidellm-1789577844-0smu2m_1`): the new shape landed in an empty `xxlong`,
  the floor vanished, and a three-replica fleet was sized to one from 400k
  tokens of occupancy -- worse than the shared bucket it replaced. The
  borrowed figure is wrong in a known direction (a shorter shape's mu holds
  too few, which the fleet corrects by saturating once; a longer shape's
  holds too many, which the cap bounds) and lasts until the first reading
  of the bucket's own, which then takes over unmasked.

### Measured, 2026-09-16, kermit

Three runs of the same trace on the same day, same stack and HPA policy,
same cluster; the second decode replica's start time against the load
differed run to run (97 s, ~140 s, ~140 s), which sets how deep the first
ramp goes and is not the code.

```
                       PR #62 image          split, no borrow       split + borrow + no
                       (arrival floor)       (no arrival floor)     arrival floor
                       wm9k0y_1              0smu2m_1               kb3q2v_1
first ramp, decode     1 -> 3                1 -> 9 (3 Ready)       1 -> 6 (3 Ready)
phase 1 steady         held 2                held 2                 held 2
shape change           2->3->4->2->3->4      3 -> 1 (collapse),     2 -> 3 -> 4, HELD
                       (arrival floor)       then 1 -> 5            (mu learned once)
p95 TTFT, min 0-5      0.44 s                34 s                   8.4 s
p95 TTFT, min 20-25    0.23 s                0.04 s                 4.8 s
p95 TTFT, min 30-35    0.20 s                88.7 s                 0.04 s
```

The cycle-by-cycle of the last run's second phase: target 2 from 18:10
(borrowed mu 6.5, the first ramp's deeper saturation this time); the two
replicas' batch grows with nothing waiting; occupancy crosses the threshold
and orders a third at 18:15:29; the batch reaches the ceiling, `waiting`
reads 16-18, and P1 records `mu = 2.97` for `xxlong` at 18:17:44 together
with the shape's own k2 (806k, against 1.16M for the 1000-token shape); the
saturation moment's occupancy plus queue orders a fourth at 18:18:29 (the
ramp, again -- the residency charge); and the target then holds until the
load ends at 18:29. Three were Ready throughout; the fourth waited on a GPU
the cluster did not have, so with a free cluster the settle would have been
4 -> 3 and held (spare at four is 0.9 of a replica, at three 0.13).

What the last column costs against the first: one saturation per new shape,
4.8 s p95 for one five-minute window, to learn the shape's throughput by
reaching it. What it buys: a fleet that does not fall below what a known
shape needs and is not scaled down for looking idle -- the first column's
0.23 s came from a floor that ordered replicas early on an inflated service
time and released them again, five replicas' worth of churn per ten minutes.
Learning a shape's throughput before its queue forms (from the batch
approaching the engine's ceiling, or from ITL growth) would remove the
episode and is the natural next step; it needs `num_requests_running` per
replica, which the collector does not read today.

## The prefill side of the same convention, 2026-09-18

With the queues priced as throughput (`fix/backlog-by-throughput`) the first
ramp is 1 -> 2 -> 3 on every cold run since, and the second phase 2 -> 3 -> 4
held. What the cold pass of 2026-09-18 (run `guidellm-1789743465-ckjz7y_1`,
same trace, same Kubernetes cluster and HPA policy, controller restarted
before the pass) added was a **prefill** replica that the earlier runs never
ordered, and kept for the rest of the run:

```
+85 s    decode 1 -> 2 (lambda / mu)                 ready +168
+190 s   decode 2 -> 3                               ready +281
+220 s   prefill 1 -> 2                              ready +331, held to the end
+280 s   decode 3 -> 2, applied +470
...      prefill stays 2 through +2231
```

Decode GPU-minutes came out lower than the run before (95.7 against 105.8,
one fewer flap at the second peak) and the all-pod figure higher (167.8
against 144.6): the second prefill replica cost 33 GPU-minutes (Ready at
+331 s to the end of the load) and prefilled nothing a single one could not
-- both prefill pods read 0 running, 0 waiting and under 3 % KV on 95 % of
the harness scrapes after +330 s.

Where it came from, cycle by cycle from the controller log:

```
15:01:03  decode sjldp P1-obs  inUse 1 039 474  queue 35    prefill P4-k1  inUse 385 067  queue 0
          decode fvntl P1-obs  inUse   970 475  queue 46
15:01:33  decode sjldp P1-obs  inUse 1 039 474  queue 81    prefill P1-obs inUse 357 800  queue 30  mu 4.77
15:01:48  ...                                                prefill P1-obs inUse 357 800  queue 30
15:02:03  ...                                                prefill P1-obs inUse 357 800  queue 30  mu 5.57
15:02:18  decode sjldp P1-obs  inUse   589 249  queue 81    prefill P1-obs inUse 357 800  queue 30
15:02:33  decode sjldp P2-hist inUse   208 407  queue 3     prefill P2-hist inUse  54 150  queue 0
```

The prefill replica saturated -- by the analyzer's definition, queue over
the threshold with tokens resident -- exactly and only while both decode
replicas were saturated at their k1. A prefill request is done when decode
admits it and pulls its KV; with decode full, the prefill engine held 385k
tokens of finished prompts awaiting a pull that takes ~0.1 s when decode is
healthy (its KV peaked at 39 % on the harness scrapes around +130 s and
read under 3 % on 95 % of them), its arrivals came from the scheduler's
flow control in bursts (the EPP queue read 4, 124, 51, 49, 107, 0 over
these cycles), a queue of 30 stood behind them, and its completion rate was
decode's admission rate. Which of the held blocks and the bursts put the 30
in the queue is not settled -- prefill's KV was at 31 % of its cache, so it
was not block-starved -- and does not matter to the reading: every one of
those numbers is decode's. Recorded as prefill's: k2 = 357 800 against a k1
of 919 859, so the one replica's own occupancy read 100 % and `roleRC` for
prefill went to 63 141 at 15:01:33 -- the order; and `mu` = 5.57 req/s,
below the 6 req/s offered, so `lambda / mu` read 1.08 replicas at the
median for the rest of the run (0.98-1.17 between the 10th and 90th
percentile, above 1.0 on 86 % of the 133 `throughput-demand-floor` lines
for prefill after the episode; the arrival rate jitters) while
`residentDemand` on the same lines read 0-54k. Two prefill replicas never
saturate again, so no later reading could displace either figure: the
window keeps a max, and the history evicts after 24 h. (The rows above are
in the full controller log of the pass, `wva_controller_full.log` beside
the results; the harness's own copy starts at 15:09:46.) The warm pass that
followed on the same controller (run `guidellm-1789746634-pov4xp_1`) shows
what a persisted figure does on its own: at +51 s, with `residentDemand` 0
on every prefill line and no decode replica anywhere near saturation, the
floor read `lambda / mu` = 5.5 / 5.57 = 0.99 replicas -- 98.8 % of the one
prefill replica's 357 800 -- and ordered the second before the decode
fleet had ordered its own; both prefill replicas then ran to the end.

This is the convention the throughput floor removed for the queues,
surfacing one layer down: a backlog that belongs to decode, charged to
prefill -- not as residency this time, but as prefill's *capacity* and
*throughput*, which persist. The fix is on the admission side, where the
k2 capacity model already puts it (`docs/plans/analyzers/k2-capacity-model.md`:
sample admission must be strict, because persistence makes a bad sample
permanent): a prefill P1 reading taken in a
cycle where any decode replica is over the queue threshold is left
unrecorded (`P1-obs-downstream`), for both k2 and `mu`. Decode's own reading
in the same cycle records as before. A prefill bottleneck reduces decode's
arrivals, so the two saturate at once only when decode is short at
prefill's completion rate; prefill's reading then waits for decode to
recover. On the run's rows, the gated cycle prices prefill at k1 with a
demand of 357 800 + 30 x 6000 = 537 800 -- 58 % of one replica, no order
(`downstream_saturation_test.go` replays it).

The demand side of the same cycle is decode's too, and the gate alone
leaves it acting on the decision. Review found the two cases: a prefill k2
learned earlier on a genuine saturation is small, as a compute bound is
(one step's prompts plus the hand-off -- 80 000, say), and against it the
537 800 that decode's backlog puts on prefill is `RC = 537 800 / 0.85 -
80 000`, seven extra prefill replicas ordered on every decode saturation of
a fleet whose prefill was ever the bottleneck and released through the
scale-down window when decode recovers; and against k1 on a fleet of two,
537 800 on 1 839 718 is 29 %, one replica removed while decode is
saturated. (The release can only happen on the gated cycles where decode's
own order is already pending -- while decode is ordering, the optimizer
runs no scale-down at all -- and a replica released then would stay
released: prefill read 54k, 12k, 0 after the episode. What the floor of
the band buys is the burst that ends an episode: the scheduler's flow
control releases what it held in one go, 124 requests and 744k prompt
tokens against a 919k k1 on this run, and it lands on prefill first.) So
on a gated cycle prefill's demand is clamped into the band where the
engine neither orders nor releases, `[scaleDown x supply, scaleUp x
anticipated supply]`, and the cycle logs `prefill-demand-held` with the
figure it found and the one it left. Prefill keeps what it has until
decode's numbers are its own again. What that costs: a genuine prefill
order -- both roles short at once -- is deferred for as long as any decode
replica's one-minute peak reads full and queued, plus a minute of memory
(below): replayed on this run's cold pass the episode runs +115 s to
+310 s, 195 s and two decode starts, a minute of it the memory; the warm
pass's 135 s across one; the phase switch, where decode queued without
filling, none at all. Unbounded while decode is capped and cannot grow.
The cycle decode recovers, prefill's own occupancy and floor stand.

The decode test, and why it is what it is:

- It is a decode replica full AND queued -- resident KV at its k1, queue
  at the threshold -- not the queue alone. vLLM counts a decode request
  waiting for its remote KV in `num_requests_waiting` (it sits in the
  scheduler's waiting queue as `WAITING_FOR_REMOTE_KVS` until the transfer
  lands), so a fleet whose KV transfer keeps as many in flight as the
  threshold -- a 70B-class model at 6 req/s over 10 GbE is 1-2 s and
  ~2 GB per prompt, and the collector takes the minute's peak -- reads
  decode saturated every cycle on the queue alone. With the gate that is
  a prefill that never records; with the hold it is a prefill that can
  never be ordered, on a fleet where prefill may be the one that is
  short. Full is decode unable to allocate the blocks a pull needs, which
  the transfer pipeline does not produce; the measured cycles satisfy it
  (970k and 1 039k against decode k1s of 929 894 and 930 508). The same
  in-flight rows make decode's own P1 fire every cycle, which is the older
  problem and not this one; newer vLLM splits the count by reason
  (`vllm:num_requests_waiting_by_reason`, `reason="capacity"`), and
  reading that for both is the follow-up once the vLLM the stack ships is
  known to carry it.
- The test is remembered for the collector's row window
  (`DecodeSaturationMemory`, one minute). The KV bound alone fails on this
  run's own rows: the Prometheus windows are not aligned, and on the fourth
  cycle decode's occupancy had dropped under k1 (589k and 825k) while its
  queue (65, 81) and prefill's row -- a one-minute max, repeated to the
  token -- had not moved; that row would have recorded. With the memory
  the episode's last cycle is still decode's.
- A decode bound by its sequence ceiling before its KV does not gate; that
  is a prefill mislearned low, which costs money, where a prefill that
  cannot be ordered costs latency.
- The four "readings" are one Prometheus sample seen four times (the rows
  repeat to the token), which is what let a single saturated moment clear
  `MinThroughputSamplesToOrder`. The gate makes it moot for prefill; for
  decode the repeated rows are the same under-read repeated, and the
  window's max is unaffected.

Replayed on the logged rows, the gate would have been on for 13 of the
cold pass's 210 cycles and 9 of the warm pass's, one contiguous episode
each.

### Re-run with the gate, the hold and the memory, 2026-09-18

Same trace, cluster, stack and HPA policy; image built from this branch at
`860d174f`; controller restarted before the cold pass. Runs
`guidellm-1789760128-zphd9d_1` (cold) and `guidellm-1789763936-mr6h9x_1`
(warm, same controller).

```
                          run 8 cold      run 9 cold      run 9 cold, 2nd  run 8 warm      run 9 warm
prefill replicas          2 from +331 s   1 throughout    1 throughout     2 from +51 s    1 throughout
all-pod GPU-min           167.8           137.9           142.1            168.0           132.9
decode GPU-min            95.7            99.1            103.4            93.1            94.2
2nd decode ordered/Ready  +85 / +168      +70 / +221      +77 / +165       +66 / +153      +48 / +133
first-ramp peak           3               4               3                3               3
TTFT p95 / p99            7.9 s / 25.2 s  25.3 s / 49.8 s 241 ms / 14.1 s  243 ms / 17.3 s 194 ms / 3.3 s
p95, minutes 0-5          5.0 s           21.5 s          2.9 s            5.8 s           0.59 s
p95, minutes 20-25        4.1 s           4.4 s           0.21 s           0.22 s          0.22 s
P1-obs-downstream lines   --              0               0                --              0
prefill-demand-held       --              11 (+130..+340) 9 (+137..+257)   --              9
```

Prefill never left one replica on any pass: 30-35 GPU-minutes per pass
against run 8, and the warm pass's p95 the lowest of the series. The gate
was never needed -- prefill showed no saturated row on any pass -- and the
hold was: decode was full and queued for 15 cycles of the first cold pass
(+130..+340 s), and on four of them (+175..+220 s) prefill's resident KV
read 830 300 with NO queue -- 72 % of its cache, sixty-odd finished prompts
awaiting a pull -- which is 90 % of k1, above the scale-up threshold. The
hold capped it at 0.85 x k1; without it a second prefill replica would
have been ordered at +175 s, as run 8's was at +220 s, and for the same
reason. The other seven logged cycles lifted 6k-349k to the band floor
(the four in between, at 710k, sat inside the band and needed nothing).
On the warm pass and the second cold pass every held figure was below the
floor (0-463k). Note what the 830k-with-no-queue rows say about the
mechanism question above: the held blocks are real on their own, queue or
no queue; whether the queue of 30 on run 8 was theirs or a burst's is
still the open part.

The cold pass's first ramp is the pod-start variance on record
([the workload preparation reference](../reference/workload-preparation.md), "The rest of the start path"), not the analyzer: the second
decode replica was ordered at +70 s, as on every cold pass (+70..+85), and
took 129 s from creation to Ready (created 19:36:58, containers started
14 s later, Ready 19:39:07) against 66 s for run 8's -- same image on the
node, same volume. The lone replica tipped at its usual ~+90 s, the backlog ran
two minutes instead of one, and the throughput floor priced it as a fourth
replica at +265 s and released it at +355 s. Nothing here touches decode's
demand or a pod's start; the phase switch at +1200 s, where the same
paths run without a start, is 4.4 s against 4.1 and 3.3-3.5 s (the P/D
well-lit path's 3.3 and the 3.5 here are the same run 7 window anchored at
the harness start and at the first scrape). The second
cold pass (run `guidellm-1789768002-eb450o_1`, controller restarted, 0
history lines before the load) drew a 67 s start for its second decode
replica, Ready at +165 s, and its first window is 2.9 s at p95 -- the
lowest cold ramp of the series (7.0-7.3, 5.0 and 21.5 s before it) -- with
prefill again at one and the hold again on the cycles decode was full and
queued. Its decode GPU-minutes are the highest of the cold passes (103.4)
for a reason outside this change: the decode target chattered 3, 2, 3, 2
from +272 s to +1023 s and the HPA's window kept the third replica until
+1202 s, the descent the sticky scale-down (`WVA_STICKY_SCALE_DOWN`, on by
default since it merged) exists for; the branch this image was built from
does not include it. A run on the merged tree is in progress and will be
added here.

The re-run did not get the signals that separate held blocks from bursts
at a prefill QUEUE peak (prefill's `num_requests_running`, its KV at the
peak, the EPP flow-control queue): prefill never queued, so there was no
peak to read. What it did show is the held-block half on its own, above.
