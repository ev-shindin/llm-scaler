# What a warm pool buys, measured

[← Bridge a scale-up with a warm pool](README.md)

Two models behind one gateway, bursting out of phase, measured three ways with
the same traffic: autoscaling alone (`nopool`), autoscaling with a shared
one-Pod warm pool (`pool`), and no pool but a floor of two replicas per model
(`floor`). Bursts are separated by long quiet stretches, so a held floor is
paid for mostly idle — the shape a pool exists for. One run of each arm, on
CoreWeave H200s, 2026-09-18, with the controller's sticky scale-down on (below).
The scenario and every guard that keeps the arms comparable are in the
[benchmarking guide](../../guides/benchmarking/two-model-warm-pool.md); the
numbers below are that report's, unedited.

## The setup

| | |
| --- | --- |
| models | `unsloth/Meta-Llama-3.1-8B-Instruct` (A), `Qwen/Qwen3-8B` (B), one InferencePool + EPP each, 1 × H200 per replica, `max-num-seq 32` |
| load | inference-perf, Poisson open-loop, 1000 in / 500 out tokens, synthetic prompts (no shared prefix — [why that matters](../../guides/benchmarking/two-model-warm-pool.md#the-prompts-share-no-prefix)) |
| rates | each model at 3 rps, bursting to 9 (A) or 8 (B) rps; A and B never burst at once |
| schedule | two minutes at 3 rps, then 300 s bursts separated by 900 s with both models at 3 rps, two cycles; 4020 s and 30 720 requests per arm |
| ceiling | every model 1–4 replicas in every arm; the cluster could place 8 |
| scrape | vLLM metrics every 10 s, models and pool alike ([why](../../guides/warm-pool/operating.md)); the scenario sets it, nothing is patched by hand |
| hand-back | the pool proxy drains before it is cleared (`warmpool-proxy:v11`), so a returning Pod cannot answer `503` in the second before the EPP drops it |
| scale-down | `WVA_STICKY_SCALE_DOWN=true`: a published scale-down is held against demand noise until the scale-up threshold says otherwise, so the fleet actually descends between bursts ([why](#why-the-sticky-scale-down)) |

| arm | what it is | the insurance it pays |
| --- | --- | --- |
| `nopool` | autoscaling alone | none — every rise pays a cold model load |
| `pool` | the same, plus a **one-Pod** warm pool (reserve 0) with both models resident | 1 accelerator held for the whole run |
| `floor` | no pool, each model held at **2 or more** replicas | 2 extra replicas held for the whole run |

**Why one Pod.** The two models never burst at once, so at most one scale-up
is in flight at any moment and one warm Pod covers every rise; a second Pod
would sit in reserve for the whole run and double the insurance for nothing.
A pool sized for the number of models that can rise *together* is the rule;
here that number is one.

## Time to first token

Every rise starts from one replica per model, and between bursts the fleet is
back at two GPUs.

![p95 (bar) and p50 (tick) TTFT per rise window, per arm](figures/ttft-rises.svg)

| rise | nopool p50 / p95 | pool p50 / p95 | floor p50 / p95 |
| --- | ---: | ---: | ---: |
| Qwen @120 s | 1 871 / 8 281 ms | 71 / 831 ms | 71 / 127 ms |
| Llama @1320 s | 1 932 / 5 154 ms | 61 / 112 ms | 59 / 95 ms |
| Qwen @2520 s | 1 580 / 8 338 ms | 74 / 154 ms | 76 / 127 ms |
| Llama @3720 s | 2 152 / 8 839 ms | 61 / 140 ms | 59 / 92 ms |

Whole run p50 / p95 / p99 — Llama: nopool 56 / 5 616 / 8 124, pool 56 / 89 /
169, floor 50 / 82 / 101. Qwen: nopool 67 / 6 995 / 8 484, pool 66 / 117 /
375, floor 62 / 109 / 140. Failures: none, in any arm — 30 720 of 30 720
served three times over.

## Accelerators

![Accelerators held over the run](figures/fleet-timeline.svg)

![Accelerator-seconds per arm](figures/gpu-seconds.svg)

| arm | GPU-seconds | of which the pool | lent | peak GPUs | short of its own fleet |
| --- | ---: | ---: | ---: | ---: | ---: |
| nopool | **12 129** | – | – | 5 | 3 % of samples, longest 13 s |
| pool | 14 138 (+17 %) | 4 003 | 421 | 4 | never |
| floor | 16 080 (+33 %) | – | – | 4 | never |

The pool's accelerator is charged for the whole run, lent or idle — that is
the 4 003 — so the pool arm's models themselves spent 10 135 GPU-s against
nopool's 12 129: the bridge lets every scale-up be smaller and shorter, and
the pool's own accelerator costs twice what that saves.

## Reading it

**The pool does what it claims.** Every rise goes from 5–8.8 s at p95 to
0.1–0.8 s — within 50 ms of the floor on three of the four rises — and the
models' own replica-seconds fall by 16 %, because the bridge lets each
scale-up be smaller and shorter (the fleet peaks at 4 GPUs instead of 5, and
is never short of what it asked for). Every cold rise in nopool drives the
autoscaler to three or four replicas and holds them through the 300 s
scale-down window, so autoscaling alone averages 3.0 GPUs where the pool
arm's models average 2.5.

**What it costs, honestly.** Autoscaling alone is the cheapest arm, at
12 129 GPU-s, and pays for it with 5–9 s rises. The pool is **17 % more** —
its accelerator is held for the whole run and lent for 421 s of it, so the
insurance costs about twice what the smaller scale-ups save — and the floor
is **33 % more**. Against the floor it replaces, the pool serves the same
rises within tens of milliseconds for **12 % less**.

**The arithmetic behind the 12 %.** Two models and a one-Pod pool hold
2 + 1 = 3 accelerators in the quiet, against a floor's 4, so the pool's
advantage over the floor caps at 25 % as the quiet fraction of the run
approaches 1. Each burst keeps a model at two replicas for its own length plus
~300 s of stabilization, so with 300 s bursts and 900 s quiet the quiet
fraction is only about half. Twenty percent needs ≥ 3000 s of quiet per
burst; the lever that reaches 30 % and beyond is more models sharing the same
Pod, not more quiet. Conversely, when one model is always bursting the fleet
never returns to two GPUs and a floor is the cheaper insurance — the pool is
for traffic with quiet in it.

**What is not settled.** Four scale-up events per arm, one run each, no
confidence interval; the direction is consistent across all four rises, the
margins are one run's. The lend follows the scale-up decision, and in the
pool arm's own samples the bridge was first seen lent 33 / 81 / 38 / 143 s
after the four rate steps — the decision waits on a queue-based signal that
cannot move until the engine's sequence slots fill, and that lag, not the
pool, is where the Qwen rise at 120 s (p95 831 ms against the floor's 127)
comes from. The lever there is the demand signal
([operating guide](../../guides/warm-pool/operating.md)).

## Why the sticky scale-down

The scale-down rule is stateless: every 15 s the target is recomputed from
scratch as the smallest replica count whose utilization stays under the
scale-down boundary. A model idling at 3 rps on two replicas sits right at
that boundary with one replica, and its demand moves ±15 % from one cycle
to the next — so the target flips 1, 2, 1, 2, … and KEDA's HPA, which takes
the **maximum** of the published values over its 300 s window, keeps the
second replica for as long as the chatter lasts. Measured here before the
fix, in two runs: Qwen held two replicas through the whole 900 s quiet band
(about 1 GPU × 3 000 s in the arm meant to show what autoscaling alone
costs), and whether it happened to be at one or two replicas when the next
burst came decided between a 12.6 s and a 125 ms rise on the same schedule.

With `WVA_STICKY_SCALE_DOWN=true` the controller keeps publishing a
scale-down it has already published until demand at that count would reach
the scale-*up* threshold — the same two thresholds, applied to the value
KEDA acts on. In this run the hold fired 33 times, every burst started from
one replica per model, and the floor arm's cost is 4 GPUs × 4020 s to the
second. The numbers above are the ones to compare a pool against; without
the switch, autoscaling alone is dearer by whatever the chatter happens to
hold, and the comparison is luck.

## Measure it on your cluster

Everything above comes from one make-target family and runs against any
llm-d install with two models and a shared model cache. The scenario carries
the 10 s scrape, the ceiling and the pool shape come from the variables, and
the controller is whatever `IMG` names (the default is this repository's
`main`). The sticky scale-down is a switch on the controller, set after the
standup and before anything is measured:

```bash
export BENCHMARK_NAMESPACE=<your namespace>
export MAX_REPLICAS=4 POOL_REPLICAS=1 POOL_RESERVE=0
export PHASE_SECONDS=300 OVERLAP_SECONDS=900 CYCLES=2
make benchmark-two-model-preflight      # placeable accelerators, one accelerator kind
make benchmark-two-model-standup        # both stacks, one gateway
kubectl -n $BENCHMARK_NAMESPACE set env deploy/wva-controller-manager WVA_STICKY_SCALE_DOWN=true
kubectl -n $BENCHMARK_NAMESPACE rollout status deploy/wva-controller-manager
make benchmark-two-model-verify
make benchmark-two-model-reset && make benchmark-two-model-run ARM=nopool
make benchmark-two-model-reset && make benchmark-two-model-run ARM=floor
make benchmark-two-model-pool-create && make benchmark-two-model-warm
make benchmark-two-model-reset && make benchmark-two-model-run ARM=pool
make benchmark-two-model-pool-delete
make benchmark-two-model-report         # report.md, report.json, the three SVGs
make benchmark-two-model-teardown
```

The report refuses rather than tabulates when the arms are not comparable —
different ceilings, unissued arrivals, a driver that queued, a router that
pinned a model to one engine, a sampled series with holes — and the plots are
drawn from `report.json` by `hack/benchmark/two_model_plots.py` with nothing
but the Python standard library, so they regenerate anywhere. Change the
models, rates and shape with the variables listed in the
[benchmarking guide](../../guides/benchmarking/two-model-warm-pool.md#running-it),
and re-measure both ends of the burst when you do.
