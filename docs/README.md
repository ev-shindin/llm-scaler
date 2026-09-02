# Workload-Variant-Autoscaler documentation

WVA is llm-d's variant autoscaler: it decides how many replicas of each model
variant should run, and drives KEDA to make it so. This directory holds the
guides, reference and design notes.

New here? Start with **[Install WVA in a namespace](guides/install-in-namespace/)**,
then **[After the install](reference/operations.md)**.

## Guides — the task-shaped path

Every guide follows the same shape: *Overview → Prerequisites → Installation
Instructions → Verification → Cleanup → Configuration*. Index and conventions in
**[guides/](guides/)**.

### Installing

- **[Install WVA in a namespace](guides/install-in-namespace/)** — the common case
- **[Install WVA for the whole cluster](guides/install-cluster-wide/)** — one
  controller watching every namespace
- **[Cluster-admin setup for a namespace](guides/admin-cluster-setup/)** — what an
  admin does once, so a tenant can install without cluster rights

### After installing

- **[Scale a model to zero, and get it back](guides/scale-to-zero/)**
- **[Bridge a scale-up with a warm pool](guides/warm-pool/)** — hold models
  loaded and asleep on held GPUs, so a scale-up serves while its own replica
  is still loading
- **[Bound every WVA by real GPUs](guides/admin-gpu-bounding/)** — the GPU limiter
- **[Test WVA against a full llm-d stack](guides/testing-with-llm-d/)**
- **[Benchmark WVA](guides/benchmarking/)** — the supported benchmark path

## Operations and reference

- **[Configuration](reference/configuration.md)** — every variable the installer
  reads
- **[After the install](reference/operations.md)** — verifying the install and
  first-line troubleshooting
- **[Watching what WVA decides](reference/monitoring.md)** — the Grafana
  dashboard and who owns it, the metrics that answer specific questions, the logs
- **[Preparing a workload to be scaled](reference/workload-preparation.md)** —
  the model cache, draining before scale-down, `make workload-patch`
- **[Deployment methods](reference/install-methods.md)** — installer, kustomize,
  and per-platform entry points
- **[GPU limiter](reference/gpu-limiter.md)** — bounding WVA by real accelerators
- **[Scaling policy configuration](reference/scaling-policy.md)** —
  thresholds, tiers, scale-to-zero, limiters
- **[Metrics and health](reference/metrics.md)** — exposed
  metrics and health endpoints
- **[Prometheus integration](reference/prometheus.md)**
- **[Quota limiter](reference/quota-limiter.md)** — operator-declared
  per-accelerator GPU caps
- **[GPU capacity accounting](concepts/gpu-capacity-accounting.md)** — what
  the GPU budget means, and three ways it over-states free capacity
- **[Troubleshooting](reference/troubleshooting.md)**

## Concepts and design

- **[Architecture](https://llm-d.ai/docs/architecture/advanced/autoscaling)** —
  where WVA sits among llm-d's autoscaling paths
- **[Modeling and optimization](concepts/modeling-and-optimization.md)** — queueing
  models and the optimization algorithm
- **[External scaler design](proposals/wva-external-scaler-proposal.md)** — how WVA
  drives KEDA, and why
- **[The steady-state engine](concepts/steady-state-engine.md)** — what it
  measures, and how a measurement becomes a replica count
- **[Throughput analyzer](developer-guide/throughput-analyzer.md)**
- **[Pod scraping source](developer-guide/pod-scraping-source.md)** — direct pod
  metric scraping
- **[Multi-analyzer pipeline](developer-guide/multi-analyzer-pipeline.md)**

### Proposals

Design notes for work that is not built yet, or built and still moving. See
**[proposals/](proposals/)** for the full set:

- **[Fast model loading](proposals/fast-model-loading.md)** — the argument for
  the warm pool that shipped, and **[the implementation
  design](proposals/fast-model-loading-implementation.md)** for what was built
- **[FMA post-mortem](proposals/fma-post-mortem.md)** — what Fast Model Actuation
  was, what was measured, why it was dropped, and what of it is still load-bearing
  here (the pool runs FMA's launcher)

## Developing

- **[Development setup](developer-guide/development.md)**
- **[Testing](developer-guide/testing.md)** — unit, envtest and e2e
- **[Debugging](developer-guide/debugging.md)**
- **[Benchmark internals](developer-guide/benchmark-guide.md)** — the OpenShift
  step-by-step for single- and multi-model benchmark runs. For the normal path use
  the **[Benchmark WVA guide](guides/benchmarking/)**
- **[Benchmark results](developer-guide/benchmark-results.md)** — the recorded
  runs a new analyzer is measured against
- **[Example k2 decision report](developer-guide/benchmark-k2-decisions-example.md)** — what the
  capacity-decision log looks like on a real run
- **[Contributing](../CONTRIBUTING.md)**

## Elsewhere in the repo

- [Main README](../)
- [Kubernetes deployment](../deploy/kubernetes/)
- [OpenShift deployment](../deploy/openshift/)
- [Local development with the kind emulator](../deploy/kind-emulator/)

## Need help?

- [Troubleshooting](reference/troubleshooting.md)
- [Open an issue](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues)
- Community meetings
