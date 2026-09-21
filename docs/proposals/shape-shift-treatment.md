# Traffic shape shifts: a systematic treatment

**Status:** proposal, 2026-09-21, revised after review. Evidence from the
shape-swap benchmark, three passes of one trace on one Kubernetes cluster
(below). The structure work that has to land first is
[PR #87](https://github.com/ev-shindin/llm-scaler/pull/87).

## The problem in one sentence

The saturation analyzer learns per-request cost as a function of the
workload's *shape* -- input length `I` and output length `O` -- and a shift
in shape is exactly the moment its learned figures are stale and its
measurements are noisiest. Three passes of the 8000/1000 → 1000/6000 trace
each failed at the switch for a different mechanism, and each mechanism was
the same representation failing: throughput in **requests per second**,
learned under **coarse output-length buckets**, keyed **per replica**.

Two TTFT definitions appear below and they are not interchangeable. The
**client-side** figures are guidellm's, from the request's send to its first
token, and include the wait in the EPP scheduler queue. The **engine-side**
figures are differenced from vLLM's `time_to_first_token_seconds`
histogram per pod and exclude that wait; they say what the *engines* saw.
Under a backlog the two differ by the queue's depth.

| pass | image | what the floor did at the switch | client p95 / p99, whole pass | client p95, requests arriving 20-25 min | engine p95, 20-25 min | engine p95, 36-38 min |
|---|---|---|---|---|---|---|
| 1 | #77-#80 | fresh replicas *borrowed* the 1000-token shape's mu (4.38 vs 1.4 live); target 10 → 4 under a growing backlog, flapping for ten minutes | 85 / 181 s | 190 s | 28.6 s | 0.08 s |
| 2 | + #84 (own readings outvote borrowed) | fresh replicas' *own* readings sat in another bucket (`long` 3.08 vs `xxlong` 1.42); median 2.94; released 10 → 3 where 4-5 was needed | 77 / 164 s | 171 s | 8.1 s | 32 s |
| 3 | + #85 (one bucket per role) | the **drain burst** -- the batch admitted at the switch finishing together at 2.7-3.5 req/s against 1.3-1.5 sustained -- was recorded as mu (in an empty bucket and, when the fleet's bucket flapped back, on top of an established one), and the floor alternated between the two; released 10 → 3 | 56 / 107 s | 114 s | 1.05 s | 31 s |

Each fix removed the mechanism it targeted and left the next one standing;
what users saw at the switch improved by 1.7x across the three, not by an
order of magnitude. The point of this document is to stop there and change
the representation.

## What the engine measures, and what it means under a shift

Per decode replica, from the raw scrapes of pass 3 (16 s apart), pods
`64wnb` and `886hx`, 21:17-21:27:

| state | requests/s | generation tokens/s | running sequences | KV utilization |
|---|---|---|---|---|
| full: waiting > 0, running at the engine's ceiling | 0.6 - 1.06 | 8,000 - 9,200 | 256 | 0.85 - 0.98 |
| a fresh batch just admitted, nothing completed yet | 0 (six scrapes) | up to 11,000 | ~150 | 0.4 - 0.8 |
| the drain: the switch's batch finishing together | **3.5 - 5.3** | 6,600 - 7,700 | 150 → 65 | falling |
| lightly loaded, minutes later | 0.5 - 0.75 | 3,300 - 3,900 | 9 - 12 | low |

Three things follow.

1. **Requests per second is not a property of a replica.** It is the token
   rate divided by the output length, and on a drain it bursts by 4-5x
   because sequences admitted together finish together -- with nothing
   about the replica's capacity having changed. A window that keeps the max
   (the right choice against the under-read of a replica that has just
   filled) keeps the burst too. The `recordSaturatedThroughput` comment in
   `throughput_floor.go` already measured this (6.67 req/s on the last
   saturated cycle against ~5.0 sustained, cold pass of 2026-09-19) and
   proposes the fix below in so many words -- "a mu priced from the
   generation-token rate over the bucket's output length, which does not
   burst on a drain"; pass 3 is the first measurement where the burst
   reached the floor's decision.
2. **Tokens per second does not burst on a drain, but it over-reads on a
   fresh batch.** A batch just admitted generates at 11,000 tokens/s with
   zero completions and moderate KV, against 8,000-9,200 sustained when the
   replica is full; a max window on tokens/s keeps the fresh-batch figure,
   a 10-24 % over-read that under-orders. So the invariant is not tokens/s
   either -- it is the per-sequence decode speed as a function of load.
3. **Per-sequence speed is a function of KV utilization, not of batch
   size.** The same pod at ~150 running sequences generated 73 tokens/s per
   sequence with the KV at 0.42 and 45 with it at 0.63. Fitting inter-token
   latency against KV utilization `k` across every scrape of the window
   (9 to 256 running, fresh and aged): `ITL(k) = 30.7 ms · k + 1.4 ms`, mean
   residual 4 %, worst 15 %. That is the throughput analyzer's model
   (`ITL(k) = A·k + B`, `internal/engines/analyzers/throughput/itl_model.go`)
   exactly, and its `μ_dec = N(k) / ITL(k)` is the token rate for any load.

So the model is the throughput analyzer's, made the floor's:

```
ITL(k)       = A·k + B                     learned: two coefficients, A (contention) and B (hardware)
KV_req(I,O)  = I + O/2                     the time-averaged footprint of one request (ages uniform on [0, O]
                                           in steady state) -- what estimateCapacityFromParams and the
                                           throughput analyzer's KVreq already use; I + O is the footprint
                                           of a synchronized batch, the switch case, and is the over-admission
                                           the engine then preempts (pass 3: 239 preemptions, running 256 → 75)
N(I,O,k)     = min(max_num_seqs, k · k1 / KV_req(I,O))
mu_tok(k)    = N(I,O,k) / ITL(k)           tokens/s one replica sustains at load k
mu_req       = mu_tok(k_sat) / O           the request rate the floor prices with, for ANY (I, O)
```

With `k1` = 930,508 tokens on this card, `KV_req` = 233 at 1000/6000 and
109 at 8000/1000; at the saturation `k` the analyzer already uses, the
first admits 256 (the engine's `max_num_seqs` binds) and the second ~100 --
the phase-1 fleet saturated at 89-136 running and 2.3-3.2k tokens/s, the
phase-2 fleet at 256 and 8-9k, and the formula gives the factor between
them from `I + O/2`. This replaces the bucket table, the nearest-bucket
borrow and the per-shape windows with one learned line per (model,
accelerator, role), and it prices a shape the fleet has never been
saturated under -- the case every bucket scheme handles by guessing.

Two things the current code does that this must keep or replace, and the
proposal must say which:

- **The k2 history key uses the same buckets** (`historyKey` →
  `classifyOutputLength`, for compute-bound capacity as well as for the
  throughput window). k2 stays per replica and stays bucketed until the
  refactor gives it the fleet shape too; this proposal changes the
  *throughput* key only. `outputBuckets` is not deleted by item 1; it
  loses its throughput use.
- **The hold rules.** A mu the fleet has measured (two spaced saturated
  readings, `MinThroughputSamplesToOrder`) may order; a borrowed one may
  only hold. Under the model, a `mu_req` for a shape never seen saturated
  is a *derived* figure: it inherits the ITL line's trust (A and B fitted
  on this card, on any shape) and orders only when the line has been
  verified against the observed generation-token rate for this shape's
  `k` (the throughput analyzer's GPS check); until then it holds. That
  keeps the existing discipline and states it in the new terms.
- **`estimateCapacityFromParams` already carries a different `N(I, O)`**
  (`min(S, B·O/(I+O))`, the batched-tokens bound). It is a derived-capacity
  fallback for k2; it stays, and the two bounds are reconciled by taking
  the smaller when both apply.

## The shape as a first-class signal

Today the analyzer's only shape input is each replica's own average output
length over its recent completions, folded into a bucket. That is a lagging
signal (a completion ends `O x ITL` after the request arrived: 150-190 s at
6000 tokens on a full replica, ITL 25-32 ms), a noisy one (a fresh
replica's first completions are the short requests, which finish first),
and a one-dimensional one (`I` is not represented at all, though it decides
prefill time, KV per request and the batch size above).

Two sources give the fleet's shape **before completions do**, each with a
condition:

- **Arriving input length** from the scheduler queue: the EPP reports queue
  size and queue bytes; bytes / size is the prompt length of what is
  *queued now* (5.8 KB/request ≈ 1000 tokens on this trace; 45.6 KB at
  8000, the same 5.7 bytes per token). It exists only while a scheduler
  queue exists: on pass 1 the switch was at 11:42:20 and the first non-zero
  bytes came at 11:44:22, and the field is zero on 375 of 410 cycles. So it
  is early relative to completions (which came another minute later) but
  not relative to the queue -- a fleet with headroom has no arriving-shape
  signal until it loses the headroom. Prefill's cost per request follows
  from it directly when it is there.
- **Output length in progress** from the engines. vLLM does not expose
  per-request tokens in flight, but two fleet-level signals move within a
  scrape of the switch: `generation_tokens_total` keeps rising while
  `request_success_total` stalls (the second batch of pass 3 completed
  nothing for 95 s), and `tokensInUse` climbs with no completions to
  release it. Tokens generated since the running count last rose, over the
  running count, is a lower bound on the `O` of what is in flight;
  `tokens completed / requests completed` over the last window is the `O`
  of what just finished, weighted by rate as #85 does.

With those, the analyzer carries a fleet shape `(I, O)` per role per cycle
(#85's `fleetOutputLength` is the `O` half; the throughput analyzer's
`ShapeTracker` is the tolerance-based change detector, per variant today,
per role in the target), and can raise a **shape-change event** when either
moves past the tolerance: from that cycle, (a) the previous shape's
residency and throughput figures are discounted, not trusted; (b) the fleet
is *held* -- no release -- until the new shape has a saturated reading of
its own or the model above prices it; (c) the log says so, once. Measured
from the trace's first 1000/6000 request, the analyzer's first decision
after the switch came 105 s later on all three passes (2 → 3 at +1205 s
against the switch at +1100 s) and the jump to 10 another 75 s after that,
because it waited for completions; the release to 3 on passes 2 and 3 came
from trusting a figure the switch had made stale. Both are the *absence*
of this event.

## The two directions, and what each breaks

Both traces run so far shift `O` **up** and `I` **down** at the same time
(8000/1000 → 1000/6000, 6000/1000 → 1000/4000), so the evidence cannot
separate the two; the table says which rows are measured and which are
what the model predicts and no run has tested.

| shift | what changes physically | what the analyzer sees late | status |
|---|---|---|---|
| **`O` up** (1000 → 6000) with `I` down | every in-flight request lives 6x longer; residency and the queue balloon before any completion says why; per-replica requests/s falls 6x | completions, 150 s+ later; a mu learned under the old `O` | **measured**: a decision 105 s behind the switch; mu from the old shape (passes 1-3) |
| **`O` down** (6000 → 1000) | residency collapses; requests/s rises 6x; the fleet is over-provisioned | occupancy falls at once -- this direction the analyzer reads well | predicted: an over-hold from the old, low mu -- costs money, not TTFT. Not run |
| **`I` up** (1000 → 8000) | prefill time per request ~8x; KV per admitted request up; decode's batch size down ~2.5x | prefill saturates while decode's occupancy is not yet high; the queue is at the gateway, attributed to decode | predicted: prefill under-ordered. Not run. (Prefill did stay at one replica on every pass, but those passes shift `I` *down*; and prefill never recorded a saturation at all -- its one queued minute was gated by the decode-downstream rule -- so nothing about prefill's sizing has been measured yet) |
| **`I` down** (8000 → 1000) | prefill relaxes; decode can batch more | fine | measured only as the confounded half of the first row |

The queue-at-the-gateway attribution is worth its own line: on a P/D fleet
the EPP queue is charged to decode (its KV) and dropped from prefill (#76,
the right call for a decode-bound queue), but under an `I`-up shift the
queue *is* prefill's. The arriving input length above is the discriminator:
a queue whose prompts are long while decode has free KV is prefill's
backlog. **Prefill's own cost model is not designed here.** Two facts stand
in the way and are recorded so the design does not skip them: prefill's
saturation is never recorded while decode is saturated (the downstream
rule in `analyzer.go`), which on a P/D fleet under a shift is nearly
always; and at 16 s scrapes a small model's prefill reads zero prompt
tokens/s on half the intervals and 50-156k on the others, so no sampling
moment for a prefill throughput is defined yet. That is a separate design
note, and item 2 below is built for decode first.

## What to build, in order

The throughput analyzer already has the `ShapeTracker`, the ITL model with
its two-tier fit and the GPS verification; the saturation analyzer has the
capacity learning, the role attribution and the hold discipline. Items 1
and 2 are a merge of those halves, not new code, and PR #87 says how the
code has to be arranged for that merge to be small.

1. **The floor's mu from the ITL line.** Record `(k, ITL)` per saturated
   replica into the throughput analyzer's window (one window per (model,
   accelerator, role)); price the floor with `mu_req = N(I,O,k_sat) /
   ITL(k_sat) / O` for the fleet's `(I, O)`; keep the hold rule above.
   Neither the drain (requests/s) nor a fresh batch (tokens/s) enters the
   fit: `ITL = N / tok-rate` is the same on both. *Replaces:* the
   throughput window's bucket key, `nearestSaturatedThroughput`, the
   per-shape `saturatedThroughput` windows. *Keeps:* `outputBuckets` for
   k2, the sample-spacing rule, the GPS check.
2. **Fleet shape `(I, O)`** per role per cycle from the two early sources
   with their conditions, and the **shape-change event** with its hold and
   discount. *Closes:* the wait for completions after a switch, the release
   under a stale shape, and the prefill attribution under `I`-up -- decode
   first; prefill's cost model is the separate note above.
3. **The benchmark matrix.** The two traces we have are one cell (`O` up
   with `I` down, twice). The matrix is `O` up / `O` down / `I` up / `I`
   down / both, at two arrival rates, each cold and warm, scored by the
   fixed table this document uses -- both TTFT definitions, named; the
   target path; hold/gate/sticky; GPU-minutes over the load window; replica
   starts. `hack/benchmark/gen_shape_trace.py` already generates a phased
   `(I, O, rate)` trace and produced the 1000/6000 one; what is missing is
   the scorecard as a repo tool (the per-window scripts used for this
   document live outside the repo today) and the colleague's
   `report.py`/results bundle as its base (PR #87).

Items 1 and 2 are each one PR against the analyzer packages; item 3 is
benchmark tooling and can proceed in parallel. None of them changes the
actuation path or the ScaledObject; the HPA policy question (#55) is
orthogonal and stays a backstop.

## What this does not cover

- The cold-ramp overshoot-then-collapse at the start of every pass (target
  to 5-7, then 2 once the ramp's transient queue drains) is a cold-controller
  cost, not a shape problem; pass B (warm) of the same trace is its
  measurement, and it has not been run on this trace.
- Replica start time is out of scope here and no longer the lever: every
  start on passes 2 and 3 was 55-69 s with the seeded node-local cache (#83).
- Prefill's throughput model, per the section above.
