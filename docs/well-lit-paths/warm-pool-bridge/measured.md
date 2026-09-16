# What a warm pool buys, measured

[← Bridge a scale-up with a warm pool](README.md)

Two models behind one gateway, bursting out of phase, measured three ways with
the same traffic: autoscaling alone, autoscaling with a shared warm pool, and
no pool but a floor of two replicas per model. One run of each, on CoreWeave
H200s, 2026-09-16. The scenario and every guard that keeps the arms comparable
are in the [benchmarking guide](../../guides/benchmarking/two-model-warm-pool.md);
the numbers below are that report's, unedited.

## The setup

| | |
| --- | --- |
| models | `unsloth/Meta-Llama-3.1-8B-Instruct` (A), `Qwen/Qwen3-8B` (B), one InferencePool + EPP each, 1 × H200 per replica, `max-num-seq 32` |
| load | inference-perf, Poisson open-loop, 1000 in / 500 out tokens, synthetic prompts (no shared prefix — [why that matters](../../guides/benchmarking/two-model-warm-pool.md#the-prompts-share-no-prefix)) |
| shape | 2 min lead-in at 3 rps each, then four 8-minute phases: A and B take turns bursting (A to 9 rps, B to 8 rps) with 90 s both-low bands between; 2310 s in all |
| ceiling | every model 1–4 replicas in every arm; the cluster could place 8 |

| arm | what it is | the insurance it pays |
| --- | --- | --- |
| `nopool` | autoscaling alone | none — every rise pays a cold model load |
| `pool` | the same, plus a 2-Pod warm pool with both models resident | 2 accelerators held for the whole run |
| `floor` | no pool, each model held at **2 or more** replicas | 2 extra replicas held for the whole run |

## Time to first token in each rise window

The first 240 s after a model's rate goes up — one row per scale-up event, and
that is the sample size: four events per arm, one run each.

![p95 (bar) and p50 (tick) TTFT per rise window, per arm](figures/ttft-rises.svg)

| rise | nopool p50 / p95 | pool p50 / p95 | floor p50 / p95 |
| --- | ---: | ---: | ---: |
| Qwen @120 s | 91 / 4302 ms | 70 / 146 ms | 74 / 127 ms |
| Llama @690 s | 2241 / 10623 ms | 64 / 2301 ms | 63 / 101 ms |
| Qwen @1260 s | 4250 / 8841 ms | 71 / 146 ms | 76 / 127 ms |
| Llama @1830 s | 2999 / 12626 ms | 63 / 361 ms | 62 / 101 ms |

Whole run, p50 / p95 / p99: Llama nopool 58 / 9662 / 12496, pool 61 / 107 /
2068, floor 60 / 96 / 123; Qwen nopool 65 / 5675 / 8787, pool 67 / 123 / 213,
floor 67 / 119 / 152. Failures: nopool 0, floor 0, pool 4 × `503` (see below).

## What each arm held

![Accelerators held over the run, per arm, with the bursts shaded](figures/fleet-timeline.svg)

![Accelerator-seconds per arm: model replicas, pool lent, pool held idle](figures/gpu-seconds.svg)

| arm | GPU-seconds | of which the pool | lent | peak GPUs | short of its own fleet |
| --- | ---: | ---: | ---: | ---: | ---: |
| nopool | 10 350 | – | – | 6 | 4 % of samples, longest 29 s |
| pool | 12 393 | 4 620 | 525 | 7 | 3 %, longest 58 s |
| floor | **9 240** | – | – | 4 | never |

## Reading it

**The pool does what it claims.** Every rise went from 4–12 s at p95 to
0.15–2.3 s, and the models' own replica-seconds fell from 10 350 to 7 773
because the bridge let each scale-up be smaller and shorter.

**And at this ratio it still loses to over-provisioning.** A floor of two
replicas per model served every rise at ~100–130 ms p95, never needed a third
replica (9 240 GPU-s is exactly 4 accelerators × 2310 s), and was cheaper than
*both* other arms: 11 % below autoscaling alone — which overshoots to three
replicas and holds them through the 300 s scale-down window — and 25 % below
the pool, whose two Pods cost 4 620 GPU-s to lend 525.

The reason is arithmetic, not the pool's mechanics. With **two models and a
two-Pod pool**, the pool holds exactly as many spare accelerators as the floor
does, warms them less directly (a bridge arrives 40–90 s into a rise and hands
back when the real replica is Ready), and the models still pay for their own
scale-ups on top. A pool is insurance that gets cheaper per model as more
models share it; two sharing two is the ratio at which it cannot win.

**When the pool is the right answer** — and this measurement does not cover it:

- many models on few Pods: eight models sharing two Pods holds 2 spare
  accelerators, where a floor holds 8;
- bursts a cheap floor cannot absorb: a model that goes from one replica to
  four needs the bridge whichever way it is provisioned;
- models too large to keep a spare replica of at all, which is
  [the retained pool path](../retained-pool/).

**What is not settled.** Four scale-up events per arm, one run each, no
confidence interval; the direction is consistent across all four rises and
both models, the margins are one run's. The pool arm's four `503`s were the
pool proxy answering `no model is awake in this Pod` during the ~1.6 s between
a bridge's proxy being cleared at hand-back and the EPP dropping the Pod — a
defect in the return sequence, not in the lend, and fixable by draining
readiness before the upstream.

## Measure it on your cluster

Everything above comes from one make target family and runs against any
llm-d install with two models and a shared model cache:

```bash
export BENCHMARK_NAMESPACE=<your namespace>
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

The report refuses rather than tabulates when the arms are not comparable —
different ceilings, unissued arrivals, a driver that queued, a router that
pinned a model to one engine, a sampled series with holes — and the plots are
drawn from `report.json` by `hack/benchmark/two_model_plots.py` with nothing
but the Python standard library, so they regenerate anywhere. Change the models,
rates and shape with the variables listed in the
[benchmarking guide](../../guides/benchmarking/two-model-warm-pool.md#running-it),
and re-measure both ends of the burst when you do.
