# Traffic shape shifts: a systematic treatment

**Status:** proposal, 2026-09-21. Evidence from the shape-swap benchmark, three
passes of one trace on one Kubernetes cluster (below).

## The problem in one sentence

The saturation analyzer learns per-request cost as a function of the
workload's *shape* -- input length `I` and output length `O` -- and a shift
in shape is exactly the moment its learned figures are stale and its
measurements are noisiest. Three passes of the 8000/1000 → 1000/6000 trace
each failed at the switch for a different mechanism, and each mechanism was
the same representation failing: throughput in **requests per second**,
learned under **coarse output-length buckets**, keyed **per replica**.

| pass | image | what the floor did at the switch | overall p95 / p99 TTFT | switch 20-25 min p95 | 36-38 min p95 |
|---|---|---|---|---|---|
| 1 | #77-#80 | fresh replicas *borrowed* the 1000-token shape's mu (4.38 vs 1.4 live); target 10 → 4 under a growing backlog, flapping for ten minutes | 85 / 181 s | 28.6 s | 0.08 s |
| 2 | + #84 (own readings outvote borrowed) | fresh replicas' *own* readings sat in another bucket (`long` 3.08 vs `xxlong` 1.42); median 2.94; released 10 → 3 where 4-5 was needed | 77 / 164 s | 8.1 s | 32 s |
| 3 | + #85 (one bucket per role) | the fleet's average output crossed 6000 into the empty `huge` bucket as the batch drained; its first readings were the **drain burst** (2.7-3.5 req/s against 1.3-1.5 sustained); released 10 → 3 | 56 / 107 s | 1.05 s | 31 s |

Each fix removed the mechanism it targeted and left the next one standing.
The point of this document is to stop there and change the representation.

## What the engine measures, and what it means under a shift

Per decode replica, from the raw scrapes of pass 3, 16 s apart:

| state | requests/s | generation tokens/s | running sequences |
|---|---|---|---|
| full: waiting > 0, running at the engine's ceiling | 0.75 - 1.06 | 8,000 - 10,000 | 256 |
| the drain: the batch admitted at the switch finishing together | **3.5 - 5.3** | 6,000 - 7,600, falling | 150 → 65 |
| lightly loaded, minutes later | 0.5 - 0.7 | 3,300 | 8 - 12 |

Two things follow.

1. **Requests per second is not a property of a replica.** It is the token
   rate divided by the output length, and on a drain it bursts by 4-5x
   because sequences admitted together finish together -- with nothing
   about the replica's capacity having changed. A window that keeps the max
   (the right choice against the under-read of a replica that has just
   filled) keeps the burst too. The file header of `throughput_floor.go`
   names this as open; pass 3 is its measurement.
2. **Tokens per second is not shape-invariant either, but its dependence
   is known.** At saturation a replica generates `N x s(N)` tokens/s, where
   `N` is the number of running sequences and `s(N)` the per-sequence
   decode speed at that batch size (~40 tok/s at 256 here, ~400 at 8: the
   GPU is throughput-bound at large batch and latency-bound at small).
   `N` at saturation is what the KV allows for the shape --
   `min(max_num_seqs, KV_capacity / (I + O))` -- and both terms are
   quantities the analyzer already learns (`k1`, the engine parameters,
   the shape). Phase 1 of the trace (8000/1000) saturated at ~3,100
   tokens/s because 9000-token sequences fit ~100 at a time; phase 2
   (1000/6000) at ~10,000 because 7000-token sequences fit 256 -- a factor
   of three between two shapes, predictable from `I + O`.

So the model is:

```
mu_tok(N)  = N x s(N)                     learned: s at two or three batch sizes
N(I, O)    = min(max_num_seqs, k1 / (I + O))
mu_req     = mu_tok(N(I, O)) / O          for ANY (I, O), including one never seen saturated
```

This replaces the bucket table, the nearest-bucket borrow, and the
per-shape windows with one learned curve per (model, accelerator, role),
and it prices a shape the fleet has never been saturated under -- the
case every bucket scheme handles by guessing.

## The shape as a first-class signal

Today the analyzer's only shape input is each replica's own average output
length over its recent completions, folded into a bucket. That is a lagging
signal (a completion ends `O x ITL` after the request arrived: ~60 s at
6000 tokens), a noisy one (a fresh replica's first completions are the
short requests, which finish first), and a one-dimensional one (`I` is
not represented at all, though it decides prefill time, KV per request
and the batch size above).

Two sources give the fleet's shape **before completions do**:

- **Arriving input length** from the scheduler queue: the EPP reports queue
  size and queue bytes; bytes / size is the prompt length of what is
  *arriving now*. Pass 1's log has it every cycle (`scheduler-queue-demand`,
  `eppQueueBytes` / `eppQueueSize` = 5.8 KB ≈ 1000 tokens within a minute
  of the switch from 8000). Prefill's cost per request follows from it
  directly.
- **Output length in progress** from the engines. vLLM does not expose
  per-request tokens in flight, but two fleet-level signals move within a
  scrape of the switch: `generation_tokens_total` keeps rising at the
  saturated rate while `request_success_total` stalls (a 6000-token batch
  completes nothing for its first ~60 s), and `tokensInUse` climbs with no
  completions to release it. Tokens generated since the running count last
  rose, over the running count, is a lower bound on the `O` of what is in
  flight; `tokens completed / requests completed` over the last window is
  the `O` of what just finished, weighted by rate as #85 does.

With those, the analyzer carries a fleet shape `(I, O)` per role per cycle
(#85's `fleetOutputLength` is the `O` half of this), and can raise a
**shape-change event** when either moves by a bucket's worth: from that
cycle, (a) the previous shape's residency and throughput figures are
discounted, not trusted; (b) the fleet is *held* -- no release -- until the
new shape has a saturated reading of its own or the model above prices it;
(c) the log says so, once. On all three passes the analyzer's first
decision after the switch came 20-60 s behind it and the jump to 10 some
75 s later, because it waited for completions; the release to 3 on passes
2 and 3 came from trusting a figure the switch had made stale. Both are
the *absence* of this event.

## The two directions, and what each breaks

| shift | what changes physically | what the analyzer sees late | failure observed |
|---|---|---|---|
| **`O` up** (1000 → 6000) | every in-flight request lives 6x longer; residency and the queue balloon before any completion says why; per-replica requests/s falls 6x | completions, 60 s+ later; a mu learned under the old `O` | a decision 20-60 s behind the switch; mu from the old shape (passes 1-3) |
| **`O` down** (6000 → 1000) | residency collapses; requests/s rises 6x; the fleet is over-provisioned | occupancy falls at once -- this direction the analyzer reads well | over-hold (the floor's old mu prices the new load high) -- costs money, not TTFT |
| **`I` up** (1000 → 8000) | prefill time per request ~8x; KV per admitted request up; decode's batch size down ~2.5x | prefill saturates while decode's occupancy is not yet high; the queue is at the gateway, attributed to decode | prefill under-ordered (on this trace prefill never scaled: one replica, 1 → 1, on every pass) |
| **`I` down** (8000 → 1000) | prefill relaxes; decode can batch more | fine | over-hold on prefill |

The queue-at-the-gateway attribution is worth its own line: on a P/D fleet
the EPP queue is charged to decode (its KV) and dropped from prefill (#76,
the right call for a decode-bound queue), but under an `I`-up shift the
queue *is* prefill's. The arriving input length above is the discriminator:
a queue whose prompts are long while decode has free KV is prefill's
backlog.

## The throughput analyzer already has most of this

`internal/engines/analyzers/throughput` carries a `ShapeTracker` over
`(IL, OL, hit rate)` with tolerance-based change detection, an ITL model
`ITL(k) = A·k + B` fitted over KV utilization (the `s(N)` above, in a better
coordinate), a token-rate supply `μ_dec` verified against the observed
generation-token rate, and a scheduler-queue pricing in tokens to drain.
It lacks V2's capacity learning, role attribution and hold discipline.
Items 1 and 2 below are therefore not new code but a merge of the two
analyzers' halves -- [engine-structure.md](engine-structure.md) says how,
and why the structure comes first.

## What to build, in order

1. **`mu` in tokens.** Record the saturated generation-token rate and the
   running count per saturated replica; fit `s(N)` (two points suffice: the
   ceiling and the low-batch speed); derive `mu_req` for the fleet's `(I, O)`
   from `k1` and the engine's `max_num_seqs`. Keep #84/#85 as guards on the
   window that remains for the ceiling. The drain no longer records a burst
   (tokens/s falls on a drain), and no bucket boundary exists at 6000.
   *Replaces:* `outputBuckets`, `nearestSaturatedThroughput`, the per-shape
   `saturatedThroughput` windows.
2. **Fleet shape `(I, O)`** per role per cycle from the two early sources,
   and the **shape-change event** with its hold and discount. *Closes:* the
   wait for completions after a switch, the release under a stale shape,
   and the prefill attribution under `I`-up.
3. **The benchmark matrix.** The two traces we have are two cells:
   `O` up with `I` down, twice. The matrix is `O` up / `O` down / `I` up /
   `I` down / both, at two arrival rates, each cold and warm, scored by the
   fixed table this document uses (windowed p95 at 120 s and 300 s, the
   target path, hold/gate/sticky, GPU-minutes over the load window, replica
   starts). `hack/benchmark/compare_runs.sh` becomes that scorecard, and a
   trace generator that takes `(I, O, rate)` per phase replaces hand-made
   `.jsonl` files.

Items 1 and 2 are each one PR against `saturation_v2`; item 3 is benchmark
tooling and can proceed in parallel. None of them changes the actuation
path or the ScaledObject; the HPA policy question (#55) is orthogonal and
stays a backstop.

## What this does not cover

- The cold-ramp overshoot-then-collapse at the start of every pass (target
  to 5-7, then 2 once the ramp's transient queue drains) is a cold-controller
  cost, not a shape problem; pass B (warm) of the same trace is its
  measurement, and it has not been run on this trace.
- Replica start time is out of scope here and no longer the lever: every
  start on passes 2 and 3 was 55-69 s with the seeded node-local cache (#83).
- Tokens-per-second as a *capacity* is decode's; prefill's cost is in input
  tokens per second, and item 1 needs a prefill counterpart (prompt tokens
  processed per second at saturation, from `prompt_tokens_total`), keyed by
  `I` alone.
