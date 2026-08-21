# Proposals

Design notes for work that is not built yet, or is built and still moving. A
proposal here is **not** a description of current behaviour — check its status
header before trusting it, and prefer the guides and reference docs for how the
system works today.

## Fast Model Actuation and warm capacity

The largest cluster, and the one most likely to be read out of order. Read the
review first; the others are the reasoning and the measurements behind it.

- **[Fast model loading, from first principles](fast-model-loading.md)** —
  **start here.** Restates the problem commercially before technically: the
  competitor is a spare replica, not doing nothing, so the pool has exactly one
  thing to sell — one slot covering many models. Sizes that with a loss model,
  names the assumption that can end the project (spike correlation), and lists
  the measurements to take before any code is written.
- **[Warm-pool design](fma-warm-pool-design.md)** — superseded on framing and
  economics by the review above; still the reference for per-step timings, the
  measured prerequisites and the sequencing detail. A shared pool
  of GPU-holding Pods so capacity for any model arrives in seconds instead of
  ~41 s. Configuration, per-step timings with measured provenance, what is reused
  from FMA, and five measured prerequisites.
- **[Launcher-owned GPUs (exploration)](fma-launcher-owned-warm-pool.md)** — how
  that design was arrived at, and why each alternative was rejected on evidence.
  Its own recommendations were revised repeatedly; the design document wins where
  they conflict.
- **[Shared warm pool, policy in WVA](fma-shared-warm-pool.md)** — measured
  sleeper economics, `--sleeper-limit` as the binding constraint, and why
  allocation belongs in WVA.
- **[What the FMA fork must fix](fma-fork-problem-statement.md)** — the defects in
  today's dual-pods path, with Fix 1 as landed.
- **[Requests to Fast Model Actuation](fma-upstream-requests.md)** — findings and
  change requests for the FMA project, with measurements.
- **[FMA-aware attribution](fma-aware-attribution.md)** — how WVA measures an FMA
  variant whose engine runs in a Pod no ScaledObject owns.
- **[Warm pool priced by WVA](fma-warm-pool-wva.md)** — the no-FMA-change pricing
  design.

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
