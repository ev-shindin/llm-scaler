# Workload-Variant-Autoscaler documentation

WVA decides how many replicas of each model variant should run, and drives KEDA
to make it so. It reads what your inference engines are doing, computes a target
per model each cycle, and answers KEDA's external scaler with it.

| You want to | Start here |
| --- | --- |
| Solve a specific problem | **[Well-lit paths](well-lit-paths/)** — one page per scenario |
| Get it installed | **[Install WVA in a namespace](guides/install-in-namespace/)** |
| Look something up | **[Reference](#reference--what-you-set-and-read)** |
| Understand a decision it made | **[Concepts](#concepts--how-wva-decides)** |
| Change WVA itself | **[Developing WVA](#developing-wva)** |

## Well-lit paths — start from the problem

Documented, tested and benchmarked recipes. Each page names the suites and the
benchmark scenario behind it, and says which leg is short when one is.

| Path | For |
| --- | --- |
| [Scale a model on saturation](well-lit-paths/scale-on-saturation/) | the default: replica counts that follow real serving load |
| [Scale to zero, and get back](well-lit-paths/scale-to-zero/) | releasing an idle model's accelerators, and waking it |
| [Bridge a scale-up with a warm pool](well-lit-paths/warm-pool-bridge/) | spikes that arrive faster than a replica loads |
| [Hold several large models on one set of GPUs](well-lit-paths/retained-pool/) | more large models than hardware to run them — **experimental** |
| [Bound a fleet by real GPUs](well-lit-paths/bound-by-gpus/) | a shared cluster where `maxReplicaCount` is not a real ceiling |
| [One model, two accelerator variants](well-lit-paths/accelerator-variants/) | letting the optimizer pick the cost-efficient hardware |
| [A P/D-disaggregated model](well-lit-paths/pd-disaggregation/) | prefill and decode scaled apart — **experimental** |

## Guides — the steps

Every guide runs one task from nothing to working, in the same shape: *Overview →
Prerequisites → Installation → Verification → Cleanup → Configuration*. The
commands are generated from each guide's `guide.yaml`, so they cannot drift from
what they document. Index and conventions: **[guides/](guides/)**.

**Installing** — [in a namespace](guides/install-in-namespace/) (the common
case) · [cluster-wide](guides/install-cluster-wide/) · [a small llm-d model
first](guides/install-small-model/), if nothing is serving yet

**Administering** — [cluster-admin setup for a
namespace](guides/admin-cluster-setup/) · [bound every WVA by real
GPUs](guides/admin-gpu-bounding/)

**Exercising** — [scale to zero](guides/scale-to-zero/) · [warm
pool](guides/warm-pool/) · [test against a full llm-d
stack](guides/testing-with-llm-d/) · [benchmark WVA](guides/benchmarking/)

## Reference — what you set and read

- **[Configuration](reference/configuration.md)** — every variable the installer reads, and which settings need a restart
- **[After the install](reference/operations.md)** — verifying it worked, and first-line troubleshooting
- **[Watching what WVA decides](reference/monitoring.md)** — the dashboard, who owns it, and the metrics that answer specific questions
- **[The cycle log](reference/cycle-log.md)** — the two lines WVA emits per cycle, their fields and reason codes: the page that answers "why did it scale?"
- **[Scaling policy](reference/scaling-policy.md)** — thresholds, tiers, scale-to-zero, limiters
- **[Preparing a workload](reference/workload-preparation.md)** — the model cache, draining before scale-down, `make workload-patch`
- **[Install methods](reference/install-methods.md)** — installer, kustomize, and per-platform entry points
- **[The GPU limiter](reference/gpu-limiter.md)** and **[the quota limiter](reference/quota-limiter.md)** — bounding WVA by real accelerators, and by declared caps
- **[Metrics and health](reference/metrics.md)** · **[Prometheus integration](reference/prometheus.md)**
- **[SGLang backend](reference/sglang-backend.md)** — auto-detected per variant; nothing to configure
- **[Troubleshooting](reference/troubleshooting.md)**

## Concepts — how WVA decides

- **[The steady-state engine](concepts/steady-state-engine.md)** — what it measures, and how a measurement becomes a replica count
- **[GPU capacity accounting](concepts/gpu-capacity-accounting.md)** — what the GPU budget means, and three ways it over-states free capacity
- **[Modeling and optimization](concepts/modeling-and-optimization.md)** — the queueing model and the optimization algorithm
- **[Architecture](https://llm-d.ai/docs/architecture/advanced/autoscaling)** — where WVA sits among llm-d's autoscaling paths

## Developing WVA

- **[Development setup](developer-guide/development.md)** · **[Testing](developer-guide/testing.md)** · **[Debugging](developer-guide/debugging.md)**
- **[Multi-analyzer pipeline](developer-guide/multi-analyzer-pipeline.md)** — how analyzers are registered, run and scored
- **[Throughput analyzer](developer-guide/throughput-analyzer.md)** · **[saturation demand floor](developer-guide/saturation-demand-floor.md)** · **[pod scraping source](developer-guide/pod-scraping-source.md)**
- **[Analyzer checklists](developer-guide/analyzer-checklists.md)** — what a new analyzer must show before it graduates
- **[Benchmark internals](developer-guide/benchmark-guide.md)** · **[two-variant benchmark](developer-guide/two-variant-wva-benchmark.md)** · **[recorded results](developer-guide/benchmark-results.md)** · **[an example k2 decision report](developer-guide/benchmark-k2-decisions-example.md)**
- **[Release process](developer-guide/release-process.md)** · **[Contributing](../CONTRIBUTING.md)**

## Design notes

- **[proposals/](proposals/)** — work not built yet, or built and still moving. The warm pool's argument and implementation design live here, as does the [FMA post-mortem](proposals/fma-post-mortem.md) explaining what the pool inherited.
- **[comparison/](comparison/)** — how WVA's autoscaling compares with Dynamo, Mooncake, SGLang and the hosted platforms.
- **[plans/](plans/)** — agent plans and the specs behind them.

## Elsewhere in the repo

- [Main README](../) · [Kubernetes deployment](../deploy/kubernetes/) · [OpenShift deployment](../deploy/openshift/) · [kind emulator, for local development](../deploy/kind-emulator/)

## Need help?

- [Troubleshooting](reference/troubleshooting.md) first, then [after the install](reference/operations.md)
- [Open an issue](https://github.com/ev-shindin/llm-scaler/issues)
