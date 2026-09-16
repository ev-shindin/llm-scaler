# What a warm pool buys, measured

[← Bridge a scale-up with a warm pool](README.md)

Two models behind one gateway, bursting out of phase, measured three ways with
the same traffic: autoscaling alone (`nopool`), autoscaling with a shared
one-Pod warm pool (`pool`), and no pool but a floor of two replicas per model
(`floor`). Two traffic shapes: **dense** bursts, where one model is always
bursting, and **sparse** bursts separated by long quiet stretches, where a
held floor is paid for mostly idle. One run of each arm, on CoreWeave H200s,
2026-09-16/17. The scenario and every guard that keeps the arms comparable are
in the [benchmarking guide](../../guides/benchmarking/two-model-warm-pool.md);
the numbers below are that report's, unedited.

## The setup

| | |
| --- | --- |
| models | `unsloth/Meta-Llama-3.1-8B-Instruct` (A), `Qwen/Qwen3-8B` (B), one InferencePool + EPP each, 1 × H200 per replica, `max-num-seq 32` |
| load | inference-perf, Poisson open-loop, 1000 in / 500 out tokens, synthetic prompts (no shared prefix — [why that matters](../../guides/benchmarking/two-model-warm-pool.md#the-prompts-share-no-prefix)) |
| rates | each model at 3 rps, bursting to 9 (A) or 8 (B) rps; A and B never burst at once |
| ceiling | every model 1–4 replicas in every arm; the cluster could place 8 |
| scrape | vLLM metrics every 10 s ([why](../../guides/warm-pool/operating.md)) |

| arm | what it is | the insurance it pays |
| --- | --- | --- |
| `nopool` | autoscaling alone | none — every rise pays a cold model load |
| `pool` | the same, plus a **one-Pod** warm pool (reserve 0) with both models resident | 1 accelerator held for the whole run |
| `floor` | no pool, each model held at **2 or more** replicas | 2 extra replicas held for the whole run |

**Why one Pod.** A two-Pod pool with the default reserve of one was run first
(2026-09-16, 30 s scrape). Its own log showed `lent` never above **1** for the
whole run: the second Pod was reserve throughout, and the pool held 4 620 GPU-s
to lend 525. With anti-phase bursts one Pod does the same work at half the
cost, so that is the pool measured here; the two-Pod run is kept in the
results directory as `three-arms/`.

## Dense bursts: one model is always bursting

Two minutes at 3 rps each, then four 8-minute phases in which A and B take
turns bursting, with 90 s both-low bands between; 2310 s in all, 24 420
requests per arm. The falling model still holds its replicas when the other
rises, so the fleet never returns to two GPUs.

![Dense: p95 (bar) and p50 (tick) TTFT per rise window, per arm](figures/dense-ttft-rises.svg)

| rise | nopool p50 / p95 | pool p50 / p95 | floor p50 / p95 |
| --- | ---: | ---: | ---: |
| Qwen @120 s | 107 / 4 972 ms | 67 / 122 ms | 74 / 127 ms |
| Llama @690 s | 100 / 6 703 ms | 62 / 104 ms | 63 / 101 ms |
| Qwen @1260 s | 97 / 5 644 ms | 67 / 173 ms | 76 / 127 ms |
| Llama @1830 s | 6 143 / 17 231 ms | 62 / 370 ms | 62 / 101 ms |

Whole run p50 / p95 / p99 — Llama: nopool 59 / 10 037 / 16 909, pool 61 / 100
/ 401, floor 60 / 96 / 123. Qwen: nopool 69 / 3 966 / 5 644, pool 66 / 120 /
175, floor 67 / 119 / 152. Failures: none in any arm.

![Dense: accelerators held over the run](figures/dense-fleet-timeline.svg)

![Dense: accelerator-seconds per arm](figures/dense-gpu-seconds.svg)

| arm | GPU-seconds | of which the pool | lent | peak GPUs | short of its own fleet |
| --- | ---: | ---: | ---: | ---: | ---: |
| nopool | 10 933 | – | – | 6 | 5 % of samples, longest 20 s |
| pool | 10 358 (−5 %) | 2 310 | 467 | 6 | 2 %, longest 14 s |
| floor | **9 240** (−15 %) | – | – | 4 | never |

## Sparse bursts: long quiet stretches between them

Two minutes at 3 rps, then 300 s bursts separated by 900 s with both models at
3 rps, two cycles; 4020 s in all, 30 720 requests per arm. Every rise starts
from one replica per model, and between bursts the fleet is back at two GPUs.

![Sparse: p95 (bar) and p50 (tick) TTFT per rise window, per arm](figures/sparse-ttft-rises.svg)

| rise | nopool p50 / p95 | pool p50 / p95 | floor p50 / p95 |
| --- | ---: | ---: | ---: |
| Qwen @120 s | 153 / 4 906 ms | 73 / 149 ms | 75 / 127 ms |
| Llama @1320 s | 2 281 / 13 459 ms | 62 / 112 ms | 63 / 104 ms |
| Qwen @2520 s | 670 / 5 827 ms | 69 / 129 ms | 77 / 127 ms |
| Llama @3720 s | 1 551 / 8 491 ms | 61 / 776 ms | 62 / 100 ms |

Whole run p50 / p95 / p99 — Llama: nopool 61 / 8 376 / 13 103, pool 60 / 94 /
571, floor 54 / 87 / 111. Qwen: nopool 69 / 3 954 / 5 644, pool 68 / 116 /
159, floor 64 / 110 / 141. Failures: pool 3 × `503` (below), the others none.

![Sparse: accelerators held over the run](figures/sparse-fleet-timeline.svg)

![Sparse: accelerator-seconds per arm](figures/sparse-gpu-seconds.svg)

| arm | GPU-seconds | of which the pool | lent | peak GPUs | short of its own fleet |
| --- | ---: | ---: | ---: | ---: | ---: |
| nopool | **13 489** | – | – | 5 | 3 % of samples, longest 13 s |
| pool | 14 446 (+7 %) | 4 020 | 479 | 5 | 1 %, longest 7 s |
| floor | 16 080 (+19 %) | – | – | 4 | never |

## Reading it

**The pool does what it claims, in both shapes.** Every rise goes from 4–17 s
at p95 to 0.1–0.8 s — within 25 ms of the floor on five of the eight rises —
and the models' own replica-seconds fall in both shapes, because the bridge
lets each scale-up be smaller and shorter. In the sparse shape that is
visible as nopool's cost: every cold rise drives the autoscaler to three or
four replicas and holds them through the 300 s scale-down window, so
autoscaling alone averages 3.4 GPUs where the pool arm's models average 2.6.

**Whether it is worth its Pod depends on the traffic shape.** With dense
bursts the fleet never returns to two GPUs, so a floor of two replicas per
model costs *less* than either alternative (it never scales, and it never
overshoots) while serving every rise at ~100 ms — the pool is the wrong
insurance there. With quiet stretches the ordering is the one the pool exists
for: it costs 7 % more than autoscaling alone and 10 % less than the floor,
for rises the floor barely beats.

**The arithmetic behind the 10 %.** Two models and a one-Pod pool hold
2 + 1 = 3 accelerators in the quiet, against a floor's 4, so the pool's
advantage over the floor caps at 25 % as the quiet fraction of the run
approaches 1. Each burst keeps a model at two replicas for its own length plus
~300 s of stabilization, so with 300 s bursts and 900 s quiet the quiet
fraction is only about half. Twenty percent needs ≥ 3000 s of quiet per burst; the
lever that reaches 30 % and beyond is more models sharing the same Pod, not
more quiet.

**What is not settled.** Four scale-up events per arm, one run each, no
confidence interval; the direction is consistent across all eight rises and
both shapes, the margins are one run's. The pool arm's `503`s (3 here, 4 in
the two-Pod run) are the pool proxy answering `no model is awake in this Pod`
during the ~1.6 s between a bridge's proxy being cleared at hand-back and the
EPP dropping the Pod; the fix — drain readiness before the upstream — is in
`fix/warmpool-return-503` and was not deployed on this cluster.

**The decision lag is the signal, not the controller.** From each rate step
to the scale-up decision (which is also what triggers the lend): 25 / 56 / 56
/ 87 s at a 30 s scrape, 15 / 60 / 60 / 76 s at 10 s. The controller acts
within ~7 s of Prometheus holding the sample; the rest is the queue-based
demand signal, which cannot move until the engine's 32 sequence slots are
full. A leading signal — arrival rate on a shorter window, or slot occupancy —
is the next lever, and a controller change.

## Measure it on your cluster

Everything above comes from one make-target family and runs against any
llm-d install with two models and a shared model cache:

```bash
export BENCHMARK_NAMESPACE=<your namespace>
export MAX_REPLICAS=4 POOL_REPLICAS=1 POOL_RESERVE=0
make benchmark-two-model-preflight      # placeable accelerators, one accelerator kind
make benchmark-two-model-standup        # both stacks, one gateway
make benchmark-two-model-verify
make benchmark-two-model-reset && make benchmark-two-model-run ARM=nopool
make benchmark-two-model-reset && make benchmark-two-model-run ARM=floor
make benchmark-two-model-pool-create && make benchmark-two-model-warm
make benchmark-two-model-reset && make benchmark-two-model-run ARM=pool
make benchmark-two-model-pool-delete
make benchmark-two-model-report         # report.md, report.json, the three SVGs
make benchmark-two-model-teardown
```

The dense shape is the default; the sparse one is `PHASE_SECONDS=300
OVERLAP_SECONDS=900 CYCLES=2`. The report refuses rather than tabulates when
the arms are not comparable — different ceilings, unissued arrivals, a driver
that queued, a router that pinned a model to one engine, a sampled series with
holes — and the plots are drawn from `report.json` by
`hack/benchmark/two_model_plots.py` with nothing but the Python standard
library, so they regenerate anywhere. Change the models, rates and shape with
the variables listed in the
[benchmarking guide](../../guides/benchmarking/two-model-warm-pool.md#running-it),
and re-measure both ends of the burst when you do.
