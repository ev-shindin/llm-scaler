# Workload-Variant-Autoscaler documentation

WVA is llm-d's variant autoscaler: it decides how many replicas of each model
variant should run, and drives KEDA to make it so. This directory holds the
guides, reference and design notes.

New here? Start with **[Install WVA in a namespace](guides/install-in-namespace/)**,
then **[After the install](deployment/operations.md)**.

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
- **[Autoscale a Fast Model Actuation stack](guides/fma/)** — FMA namespaces:
  scraping launchers, what the plan targets, sizing from the launcher pool

## Operations and reference

- **[Configuration](deployment/configuration.md)** — every variable the installer
  reads
- **[After the install](deployment/operations.md)** — what to watch, the Grafana
  dashboard and who owns it, first-line troubleshooting
- **[Deployment methods](deployment/install-methods.md)** — installer, kustomize,
  and per-platform entry points
- **[GPU limiter](deployment/gpu-limiter.md)** — bounding WVA by real accelerators
- **[Scaling policy configuration](developer-guide/scaling-policy-config.md)** —
  thresholds, tiers, scale-to-zero, limiters
- **[Unified configuration system](developer-guide/configuration.md)** —
  configuration reference for all components
- **[Metrics and health](developer-guide/metrics-health-monitoring.md)** — exposed
  metrics and health endpoints
- **[Prometheus integration](developer-guide/prometheus.md)**
- **[Quota limiter](developer-guide/quota-limiter.md)** — operator-declared
  per-accelerator GPU caps
- **[GPU capacity accounting](developer-guide/gpu-capacity-accounting.md)** — what
  the GPU budget means, and three ways it over-states free capacity
- **[Troubleshooting](developer-guide/troubleshooting.md)**

## Concepts and design

- **[Architecture](https://llm-d.ai/docs/architecture/advanced/autoscaling)** —
  where WVA sits among llm-d's autoscaling paths
- **[Modeling and optimization](design/modeling-optimization.md)** — queueing
  models and the optimization algorithm
- **[External scaler design](design/wva-external-scaler-proposal.md)** — how WVA
  drives KEDA, and why
- **[Saturation engine (v2)](user-guide/v2-saturation-engine.md)** — the analyzer
  that decides saturation
- **[Throughput analyzer](developer-guide/throughput-analyzer.md)**
- **[Queue-model analyzer](developer-guide/slo-queuemodel.md)** — SLO-aware
  queueing model
- **[Pod scraping source](developer-guide/pod-scraping-source.md)** — direct pod
  metric scraping
- **[Multi-analyzer pipeline](developer-guide/multi-analyzer-pipeline.md)**
- **[Controller behavior](design/controller-behavior.md)** — event handling and
  reconciliation. **Outdated**; read the external-scaler design first.

### Proposals

Design notes for work that is not built yet, or built and still moving. See
**[proposals/](proposals/)** for the full set; the FMA cluster is the largest:

- **[Warm-pool design](proposals/fma-warm-pool-design.md)** — a shared pool of
  GPU-holding Pods so capacity arrives in seconds instead of ~41 s
- **[FMA-aware attribution](proposals/fma-aware-attribution.md)** — how WVA
  measures an FMA variant whose engine runs in a Pod no ScaledObject owns
- **[Requests to Fast Model Actuation](proposals/fma-upstream-requests.md)** —
  findings and change requests for the FMA project

## Developing

- **[Development setup](developer-guide/development.md)**
- **[Testing](developer-guide/testing.md)** — unit, envtest and e2e
- **[Debugging](developer-guide/debugging.md)**
- **[Benchmark internals](developer-guide/benchmark-guide.md)** — the OpenShift
  step-by-step for single- and multi-model benchmark runs. For the normal path use
  the **[Benchmark WVA guide](guides/benchmarking/)**
- **[Benchmark reference](benchmark.md)** — harness options and what each knob does
- **[Example k2 decision report](benchmark-k2-decisions-example.md)** — what the
  capacity-decision log looks like on a real run
- **[Contributing](../CONTRIBUTING.md)**

## Elsewhere in the repo

- [Main README](../)
- [Kubernetes deployment](../deploy/kubernetes/)
- [OpenShift deployment](../deploy/openshift/)
- [Local development with the kind emulator](../deploy/kind-emulator/)

## Need help?

- [Troubleshooting](developer-guide/troubleshooting.md)
- [Open an issue](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues)
- Community meetings
