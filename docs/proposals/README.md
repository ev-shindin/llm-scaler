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

## Scaling and actuation

- **[WVA as a KEDA external scaler](wva-keda-external-scaler.md)** — the actuation
  design: how WVA drives KEDA, and the ScalingPolicy tiers.
- **[Scale-from-zero: the missing signal](scale-from-zero-missing-signal.md)** —
  why a parked model needs a push, and where it comes from.
- **[Priority scoping](priority-scoping.md)** — parked. Read before redesigning.

## Engine and analyzers

- **[Analyzer metric interface](analyzer-metric-interface.md)**
- **[SGLang backend](sglang-backend.md)**

## Product and lifecycle

- **[Capacity-planner positioning](capacity-planner-positioning.md)** — where a
  cluster capacity planner sits relative to WVA.
- **[Deprecate the VariantAutoscaling CRD](deprecate-va-crd.md)**
