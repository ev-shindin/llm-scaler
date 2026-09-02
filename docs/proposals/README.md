# Proposals

Design notes for work that is not built yet, or is built and still moving. A
proposal here is **not** a description of current behaviour — check its status
header before trusting it, and prefer the guides and reference docs for how the
system works today.

## Warm capacity

Read the review first; the implementation design says what was built.

- **[Fast model loading, from first principles](fast-model-loading.md)** —
  **start here.** Restates the problem commercially before technically: the
  competitor is a spare replica, not doing nothing, so the pool has exactly one
  thing to sell — one slot covering many models. Sizes that with a loss model,
  names the assumption that can end the project (spike correlation), and lists
  the measurements to take before any code is written.
- **[Warm pool implementation](fast-model-loading-implementation.md)** — how to
  build it: objects, the supervisor API, how a woken model joins its
  InferencePool, the cache policy, the RBAC increase a readiness gate needs, and
  a three-week breakdown for phase 1.
- **[Warm pool: open questions](warm-pool-open-questions.md)** — the three
  things the pool raises that are bigger than a bug fix: the optimizer cannot
  tell a pool replica from a serving one, the pool's vLLM version belongs to FMA
  rather than to us, and what either costs to change. Also records what HAS been
  decided, so it is not re-litigated.
- **[FMA post-mortem](fma-post-mortem.md)** — Fast Model Actuation was the
  earlier route to the same goal. This records what was tried, the measurements
  that killed it, and the four things in this repo that are still FMA and must
  not be swept up with it — starting with the pool's supervisor, which IS FMA's
  launcher. It replaces eight documents removed on 2026-08-27; they are in git
  history.

- **[Warm pool: configuration surface](warm-pool-configuration.md)** — which
  knobs the pool needs and where they live. The pool is a Deployment and its
  settings are annotations on it; three flags were deleted as facts the cluster
  already states.
- **[Retained pools](warm-pool-retained.md)** — built. Holding several large
  models on one set of GPUs, and the rule deciding which one stays awake: the
  model under more pressure, never a coin-flip between two that both want to
  scale.
- **[Warming an engine that spans Pods](warm-pool-lws.md)** — implemented.
  Multi-node sleep and wake on a LeaderWorkerSet, demonstrated without Ray.
- **[Sleep level 1 or 2](warm-pool-sleep-levels.md)** — measured. The pool
  implements level 1 only; this is the case for that, and what level 2 would
  cost if host RAM ever becomes the binding constraint.
- **[Transferring weights instead of reading them](warm-pool-weight-transfer.md)**
  — investigated, nothing built. What the shipped machinery can and cannot do,
  and which of it is worth building.

## Scaling and actuation

- **[WVA as a KEDA external scaler](wva-keda-external-scaler.md)** — the actuation
  design: how WVA drives KEDA, and the ScalingPolicy tiers.
- **[Scale-from-zero: the missing signal](scale-from-zero-missing-signal.md)** —
  why a parked model needs a push, and where it comes from.
- **[Priority scoping](priority-scoping.md)** — parked. Read before redesigning.

- **[WVA as a KEDA external scaler: the argument](wva-external-scaler-proposal.md)**
  — the shorter framing of the same design: compute the target, let KEDA
  actuate, and why that beats publishing a metric.
- **[The alternative framing](wva-external-scaler-alternative.md)** — the reply
  to "WVA as a metric shop", keeping the capacity model in WVA.

## Engine and analyzers

- **[Analyzer metric interface](analyzer-metric-interface.md)**
- **[SGLang backend](sglang-backend.md)**
- **[What counts as a serving replica](what-counts-as-a-serving-replica.md)** —
  the count WVA derives capacity from means "Pods that reported metrics", is
  used as though it meant "Pods taking traffic", and the gap produced three
  symptoms investigated as unrelated bugs.

## Product and lifecycle

- **[Capacity-planner positioning](capacity-planner-positioning.md)** — where a
  cluster capacity planner sits relative to WVA.
