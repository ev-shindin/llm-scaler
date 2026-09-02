# Proposal: WVA as a KEDA External Scaler

> **Status:** Draft for discussion (untracked working doc) · **Date:** 2026-08-04
> **Author:** (WVA maintainers) · **Area:** actuation / autoscaling integration

---

## 1. Summary

Run WVA as a **KEDA external scaler**: WVA implements KEDA's `ExternalScaler` gRPC service, and `ScaledObject`s reference it via an `external` (or `external-push`) trigger. WVA's global optimizer produces the scaling decision; KEDA/HPA carry it out.

The design keeps WVA's decision path unchanged (its own collector + optimizer) and uses KEDA purely for **actuation**, with an opt-in **managed** mode in which WVA additionally owns a ScaledObject's *envelope* (min/max) and *behavior* fields and can perform **urgent GPU reclamation** (lower the ceiling to free GPUs fast) — while an **unmanaged** ScaledObject is scaled by WVA's metric but keeps its externally-configured envelope.

Discovery is **call-driven**: WVA learns which ScaledObjects to act on from the gRPC calls KEDA makes to it — no cluster-wide `list`/`watch` of ScaledObjects.

## 2. Motivation

WVA today emits `wva_desired_replicas` / `wva_desired_ratio` metrics that reach an HPA via `prometheus-adapter` (external-metrics) or a KEDA `prometheus` trigger. Making WVA a first-class **external scaler**:

- Removes the `prometheus-adapter` dependency for the WVA path.
- Gives WVA a **per-object config channel** (`scalerMetadata`) and a **scale-to-zero** signal (`IsActive`) it doesn't get cleanly today.
- Enables an opt-in **managed** mode where WVA can shape the HPA envelope/behavior and react to emergencies (**urgent ceiling** — free GPUs fast) that the steady-state metric path is too slow to handle.

## 3. Goals / Non-goals

**Goals**
- WVA delivers its scaling decision as a KEDA external scaler (metric path).
- Opt-in **managed** ownership of a ScaledObject's `min/maxReplicaCount` and `advanced…behavior`, via **Server-Side Apply (SSA)** field ownership.
- **Urgent ceiling** to free GPUs quickly for reallocation — lower `maxReplicaCount` to bypass HPA's scale-down stabilization.
- **Call-driven** discovery — no cluster-wide ScaledObject informer.

**Non-goals**
- Replacing WVA's collector or optimizer — their ingestion/decision *role* is unchanged (queries are optimized, not replaced — §7.9, §10).
- Taking over KEDA's responsibilities (HPA creation + `ScaledObject.status` remain KEDA's).
- Owning ScaledObjects that don't reference WVA.
- Writing arbitrary status onto ScaledObjects (the protocol has no such field — see §9).

## 4. Background: the KEDA external-scaler contract

WVA implements the `ExternalScaler` gRPC service (`externalscaler.proto`). KEDA calls four RPCs:

| RPC | KEDA asks | WVA returns |
|---|---|---|
| `GetMetricSpec(ScaledObjectRef)` | metric + target for this object | `metricName`, `targetSize` |
| `GetMetrics(GetMetricsRequest)` | current value | `metricValue` |
| `IsActive(ScaledObjectRef)` | active? (scale to/from zero) | bool |
| `StreamIsActive(ScaledObjectRef)` | (push) activation stream | stream of bool |

The **only inbound payload** is:

```proto
message ScaledObjectRef {
  string name = 1;
  string namespace = 2;
  map<string,string> scalerMetadata = 3;   // the trigger's metadata block, verbatim
}
message GetMetricsRequest { ScaledObjectRef scaledObjectRef = 1; string metricName = 2; }
```

KEDA does **not** pass current replicas, pod/vLLM metrics, HPA state, or ScaledObject status. WVA gathers all telemetry itself. KEDA's HPA then computes `desiredReplicas = ceil(metricValue / targetSize)`, **clamped to `[minReplicaCount, maxReplicaCount]`**, subject to `behavior` (stabilization + policies). (Newer KEDA adds float variants `targetSizeFloat`/`metricValueFloat`.)

Trigger example:

```yaml
triggers:
  - type: external            # or external-push
    metadata:
      scalerAddress: wva-scaler.wva-system.svc:9090   # KEDA's own key, never read by WVA
      modelID: llama-3-70b     # REQUIRED — grouping key for every multi-variant decision
      variantCost: "2.5"       # optional — per-replica cost for cost-aware optimization
      scalingPolicy: interactive   # optional — named, reusable tier
      # wvaOwnership: "true"   # PLANNED, not yet shipped (see §6)
```

`inferencePool` and `engine` are **not** metadata — both are inferred (see §7.2).

## 5. Actuation: three mechanisms, three timescales

Each mechanism writes **different fields**, so they compose with single-writer-per-field semantics. KEDA still owns the HPA and `ScaledObject.status`.

| Concern | Mechanism | Cadence | Fields |
|---|---|---|---|
| **Steady-state count** | External-scaler **metric** → HPA `ceil(value/target)` | per poll (~30s) | none on ScaledObject |
| **Envelope / behavior** (managed only) | **SSA-apply** owned fields | occasional (policy change / drift) | `min/maxReplicaCount`, `advanced…behavior`, `fallback` |
| **Urgent ceiling** (managed only) | Lower the **ceiling** (`maxReplicaCount`), then relax | event | `maxReplicaCount` |

**Steady state.** WVA's optimizer output is encoded as `metricValue`/`targetSize`; HPA computes the count within `[min,max]`. WVA writes nothing on the ScaledObject.

**Envelope/behavior.** In managed mode WVA SSA-applies only the fields it owns (dedicated field manager, e.g. `wva`). This sets the bounds the metric operates within and tunes responsiveness (e.g., short scale-up stabilization for latency-critical targets).

**Urgent ceiling (free GPUs fast).** The urgent lever is the **ceiling**, because HPA's stabilization is asymmetric: scale-up `stabilizationWindowSeconds` defaults to **0** (the metric path already scales *up* fast), but scale-down defaults to **300s** (HPA waits ~5 min before removing replicas). When WVA needs to **reclaim GPUs** — a higher-priority workload is starving under GPU contention, or the quota/fair-share optimizer wants to rebalance — it **lowers `maxReplicaCount`** to force an immediate scale-down that **bypasses the scale-down stabilization window**, freeing GPUs now for reallocation. It then relaxes `max` back up once the metric reflects the new steady state. Two safety properties: (1) lowering `max` cannot push a target below its own `minReplicaCount` (HPA still honors `min`), so min-availability floors stay safe; (2) because WVA's metric already reflects the reduced demand, relaxing `max` does **not** cause a rebound — the ceiling patch merely *accelerates* the drop the metric would eventually produce, never fights it. (Urgent scale-*up* is rarely needed since the metric path is already fast upward; a symmetric floor patch on `minReplicaCount` is available if ever required.)

## 6. Managed vs. unmanaged (the `wvaOwnership` flag)

A per-ScaledObject flag in the trigger `metadata` (delivered as `scalerMetadata`, also readable from the spec):

- **Unmanaged** (`wvaOwnership` absent/false): WVA scales the object **only via the metric**. It **reads** `min/max` + `behavior` and treats them as **hard constraints** (user/GitOps owns them). If WVA's desired count exceeds `max`, it is capped and **reports/logs** rather than overriding.
- **Managed** (`wvaOwnership: "true"`): WVA additionally **owns** `min/max` + `behavior` via SSA, and may **urgent-lower the ceiling** to free GPUs. If desired exceeds `max`, WVA **raises `max`** (it owns it). Managed↔unmanaged transitions are seen on the next call.

### Ownership semantics
- Ownership = **authoritative writer of specific fields** via an SSA **field manager**, *not* `ownerReferences`.
- Do **not** add a controller `ownerReference` to a user-created ScaledObject (it would GC-couple the user's object to WVA's lifecycle). Reserve `ownerReferences` for ScaledObjects WVA **generates** itself.
- KEDA still reconciles the ScaledObject → HPA and owns `.status` regardless.

### GitOps drift
If a managed ScaledObject is Argo/Flux-managed, WVA's SSA writes to `min/max`/`behavior` are drift. Resolve by either: GitOps `ignoreDifferences` on the WVA-owned fields (the SSA field manager makes this precise), **or** have WVA **generate** the ScaledObject so it is the end-to-end source of truth.

## 7. Identity & configuration model

**No `VariantAutoscaling` CRD, no discovery/variant labels, no ScaledObject annotations.** All identity and per-target config flows through the trigger **metadata** (`scalerMetadata`); all scaling *policy* flows through **layered ConfigMaps**; everything else is **inferred** from the workload.

### 7.1 The scale unit: the ScaledObject replaces the "variant"

A ScaledObject has exactly one `scaleTargetRef`, but an **InferencePool spans many Deployments/LWS**. Therefore:

- Deploy **one ScaledObject per Deployment/LWS**. The ScaledObject is the atomic scale unit that **replaces the old "variant"** — WVA's internal identity is `(namespace, ScaledObject)`.
- `inferencePool` in metadata is a **grouping key**, not a single actuation handle. WVA keys its registry by ScaledObject and builds a **secondary index by pool** for pool-level/global reasoning (GPU budget, fair-share). ***N ScaledObjects → 1 pool.***
- The InferencePool is routing; the scale target is the backing Deployment/LWS (`inference_pool_ready_pods` is an *outcome*, not a scale handle).

### 7.2 Trigger metadata (per-target facts)

```yaml
triggers:
  - type: external                       # or external-push
    metadata:
      scalerAddress: wva-scaler.wva-system.svc:9090
      modelID: llama-3-70b               # REQUIRED — grouping key; a variant that cannot be
                                         # attributed to a model cannot be optimized against one
      variantCost: "2.5"                 # per-replica cost — naturally per-accelerator (this SO = one accelerator)
      scalingPolicy: interactive         # named, reusable tier (no model identity inside)
      # wvaOwnership: "true"             # PLANNED — managed vs unmanaged (see §6)
      # inferencePool, engine: NOT set here — inferred (see below)
      # role: NOT set here — inferred from the workload (see §7.4); optional override only
```

**Shipped surface.** `modelID` is the only required key; `variantCost` and
`scalingPolicy` are optional. `wvaOwnership` is designed but not yet
implemented. The `managed` annotation has no successor and needs none: KEDA only
calls a scaler its trigger names, so **being called is what being managed
means**.
`scaleTargetRef` is **not** in metadata — it is the ScaledObject's own spec field, which WVA reads.

### 7.3 Placement principle

> **Metadata = per-*target* facts. ConfigMap = per-*model*/tier policy. Inferred = workload-derivable facts.**

| Attribute | Where | Scope |
|---|---|---|
| `modelID` (required), `scalingPolicy` ref | **metadata** | per target |
| `wvaOwnership` | **metadata** — *planned, not yet shipped* | per target |
| `inferencePool`, `engine` | **inferred** — see the update below | per target |
| `variantCost` | **metadata** | per target — per-accelerator by construction |
| `role`, accelerator type, GPU/replica | **inferred** from `scaleTargetRef` workload | per target |
| `priority` (tier default), scale thresholds, SLO targets, `scaleToZero` | **named policy CM** | per tier (reusable) |
| `limiters`/`quota`, `enableRescale`, GPU budget, fallbacks | **default/namespace CM** | cluster / tenant |

> **Update (2026-08-11):** `scaleToZero` is now `{enabled, retentionPeriod}` on the
> scaling entry and is the only per-model surface for it — the separate
> `wva-model-scale-to-zero-config` ConfigMap is gone. It belongs in the **named
> policy** row: whether a tier is allowed to idle to zero, and how long it must be
> idle first, are tier properties, not per-target facts. `WVA_SCALE_TO_ZERO`
> remains the deployment-level switch.

> **Update (2026-08-11): `inferencePool` is not in metadata, and should not be.**
> It is derived by matching the workload's pod-template labels against each
> InferencePool's selector (`Datastore.PoolGetFromLabels`), which is strictly
> better than declaring it: a declared pool can disagree with the selector that
> actually decides membership, and then WVA reasons about a queue the workload is
> not in. The same argument retires `engine`, read from the container image and
> args. `modelID` stays required — it is the grouping key for every multi-variant
> decision, and a workload parked at zero has no pods and no metrics to infer it
> from, which is precisely when it is needed.
>
> The rule this follows: **ask only for what cannot be observed.** Anything
> derivable is derived, so it cannot drift from reality.

- **Role** is inferred from the workload: primary = engine args (`--disaggregation-mode prefill|decode`, parsed by `deployment_parser`); fallback = pod-template label `llm-d.ai/role`. Optional metadata override only for ambiguous `both`.
- **Accelerator type / GPU count** from the `scaleTargetRef` pods' resource requests + node GPU type (existing discovery). **Cost** stays in metadata (per-accelerator); a future iteration can source it from OpenCost without changing shape.

### 7.4 Metric attribution without a variant label

`vllm:`/`sglang:` metrics are keyed by **`model_name` only** — no pool/variant/per-deployment label. If two Deployments in a pool serve the *same* model, their series are indistinguishable by label. Since the design forbids labels, attribution is **topological, not label-based**:

- Query serving metrics by `model_name` (grouped by `pod`), then **map each `pod` → its `scaleTargetRef` via ownerReferences** — the existing **Locator** (`pod-to-managed-scaler-locator`). No variant label needed.
- WVA already **GETs the ScaledObject** (for `min/max/behavior`); the same GET yields `scaleTargetRef`, whose pods define the attribution set.
- **Heterogeneous engines** in a pool are handled because `engine` is per-ScaledObject (each Deployment runs one engine); pool-level EPP metrics are engine-agnostic.

### 7.5 Layered configuration & reusable policies

Policies are **identity-free** (no `model_name`) — a named policy is a reusable **scaling tier** (`interactive`, `standard`, `batch`) referenced by many models. Resolution reuses the existing `Merge()` (scalar override), applied across layers, most-specific wins:

```text
SO metadata            (per-target: cost, engine, pool, ownership, optional priority override)
  ▼ overrides
named ScalingPolicy    (per-tier: thresholds, default priority, analyzer selection)   ← metadata.scalingPolicy
  ▼ overrides
namespace default CM   (per-tenant: budget/quota, rescale/limiter, defaults)           [optional, add later]
  ▼ overrides
global default CM      (cluster: fallback thresholds, GPU budget, defaultPolicy)
```

- **Priority** = tier default in the policy + **optional per-SO metadata override** (metadata > policy). Priority tracks the tier, so sharing across a tier's models is correct.
- **Budget-scope knobs** (`limiters`, `enableRescale`, GPU budget) live in the **default/namespace layer**, not the reusable policy — they are cluster/namespace-scoped (the code already reads `EnableRescale` "only from the default entry"; this formalizes it). A tenant must not be able to grant themselves capacity by naming a policy, so a `limiters:` block anywhere but the cluster layer is ignored and reported.

  > **Update (2026-08-11):** the `enableLimiter` flag no longer exists. The `limiters:` list is the sole switch — declaring a limiter is what turns limiting on, and declaring none means nothing is limited. Two knobs meant a quota could be declared and silently not enforced. See `docs/reference/quota-limiter.md`.
- **Emit the resolved effective policy** per SO (`wva_effective_policy_*` metric / structured log) — layered config is hard to debug without a "which value won" readout; annotations are disallowed, so use a metric/log.
- **Start with two layers** (global default + named policy); add the namespace layer only when multi-tenancy demands it.

**Three rules the layering needs to be safe (added 2026-08-11).** Each closes a
way the resolution above can go wrong silently:

- **A model's variants must resolve to ONE policy.** WVA scales a model and the
  optimizer distributes replicas across its variants, so two ScaledObjects of the
  same model naming different tiers would have the optimizer balancing them under
  conflicting thresholds. Treat it as a config error: report it and resolve
  deterministically rather than letting it half-apply. (Different models sharing a
  tier is the normal case and stays fine — that is the point of tiers.)
- **An unknown policy name must be reported, not absorbed.** Falling back to the
  global default is the right behaviour, but doing it quietly makes a typo look
  like a working configuration. Warn with the name that did not resolve.
- **`priority` is tier-scoped, not per-target.** The metadata override above is a
  capability leak: priority is *how contention is resolved*, so a tenant raising
  their own is self-granting — the same class of problem as quota, which this
  design already keeps off the tenant surface. Either keep priority in the policy
  only, or clamp a metadata override to at most the policy's value. It cannot be
  a free per-target field.

### 7.6 Analyzers: a cluster catalog, selected by policy

WVA has **two kinds of analyzers that share one output contract**, so a policy selects and weights them uniformly:

- **Internal analyzers** — compiled-in **Go** (the saturation-v2 token/capacity model, rate-anchored k2, queueing-model, throughput). Rich logic that is *not* expressible as a single PromQL (engine-arg parsing, capacity store, rate anchoring). Referenced by name; built-in, **not** defined in a ConfigMap.
- **External analyzers** — config-driven **PromQL** per **PR #1444** (follow-up **#1455**): each defines a **`query`** (the Demand `D`) and a per-replica **`threshold`** — a constant, or a `thresholdQuery` for measured capacity (the Target `P`). Added in the catalog CM **with no code change**.

**The shared contract is `D/P`.** Every analyzer — internal or external — yields a Demand and a per-replica Target; `D/P` is a replica count. This is the unifier: it's the collector query pair, the composite-scoring input, *and* the external-scaler metric (`Demand → metricValue`, `Target → targetSize`, HPA computes `ceil(D/P)`).

**Policies select/weight; they don't define.** Levels:

| Level | Owns | Reusable across |
|---|---|---|
| **Built-in registry** (Go) | *internal* analyzer implementations, by name | — |
| **Analyzer catalog** (cluster CM `wva-analyzers`) | *external* analyzer definitions: `name → {query, threshold\|thresholdQuery, activationThreshold, metricType, role, engines}` | all models & tiers |
| **Named policy** (tier CM) | *selection + weighting* of internal **and** external by name: `[{name, enabled, score, per-analyzer thresholds}]` | models in a tier |
| **ScaledObject metadata** | identity + per-target facts | one target |

**Name resolution:** a policy entry's `name` resolves against the **built-in registry first (internal), then the catalog (external)**; an optional `source: internal|external` disambiguates/validates. So `saturation` (internal Go) and `ttft-slo` (external PromQL) sit side by side in the same policy, combined by the same composite score.

**External analyzers use KEDA scaler field names directly** — an external analyzer *is* essentially a KEDA `prometheus` trigger, so a KEDA user reads it with no translation. The fields:

- **`query`** — PromQL for the current (total) signal (the Demand).
- **`threshold`** — per-replica target as a **constant** (the common case, e.g. `threshold: "0.5"`), *or* **`thresholdQuery`** — a PromQL when per-replica capacity must be measured.
- **`activationThreshold`** — scale-to/from-zero gate.
- **`metricType`** — `AverageValue` (default): HPA scales relative to replica count.

Desired replicas = `ceil(query / threshold)`, so a static per-replica target needs **no query at all**. (Internal Go analyzers compute the per-replica target in code — e.g. saturation's token capacity — the dynamic case; external analyzers cover both constant and measured targets without code.) *Impl note for #1455:* pin the HPA `metricType` (AverageValue divides the metric by replica count) so the `query/threshold` arithmetic is exact.

The catalog **merges with the collector query registry** — the Go-registered templates (`saturation.go`, `rate_capacity.go`, …) become the built-in default catalog, extensible via config with no code change. The same collector optimizations apply: write catalog queries with a `{{scope}}` placeholder expanding to `model_name=~"…"` + grouped `by (…)` (one query across all managed models), and precompute heavy expressions as **recording rules**.

**Backend = query variant, not a separate analyzer.** An analyzer is a *concept* (saturation, ttft-slo); vLLM vs SGLang (vs future engines) are per-engine query **bodies** of that concept, selected by the SO's `engine`. This keeps policies backend-agnostic — a policy enables `saturation`, never `saturation-vllm` — and mirrors the existing `registerForEngine` pattern (same logical query, per-engine template). It also keeps a **mixed-engine pool comparable**: every target's `saturation` means the same thing regardless of engine, so pool fair-share compares like with like. Engine-*agnostic* analyzers (built on EPP/GIE metrics like `inference_pool_*` / `inference_objective_*`) use a single body with no `engines:` map; if a SO's engine has no body for an engine-specific analyzer, that analyzer is skipped for it (logged). The output metric stays labeled by the analyzer *concept* (`wva_analyzer_demand{analyzer, namespace, model, role?}`) — an `engine` label is optional for observability only, not a new analyzer identity.

**Examples**

```yaml
# ConfigMap: wva-analyzers   (cluster — EXTERNAL analyzer DEFINITIONS only; internal ones are built-in Go)
# Fields mirror KEDA scaler vocabulary: query (=Demand D), threshold / thresholdQuery (=per-replica Target P),
# activationThreshold, metricType. {{scope}}=model/ns selector, {{group}}=group-by set, {{pool}}=pool name.
# An analyzer is a CONCEPT; backend = per-engine query VARIANT selected by the SO's `engine`.
analyzers:
  ttft-slo:                  # per-replica target is a CONSTANT (KEDA `threshold`) — no target query needed
    role: fromPod            # llm-d.ai/role pod-template label (NOT a metric label)
    metricType: AverageValue
    activationThreshold: "0"
    engines:
      vllm:
        query: sum by ({{group}}) (max_over_time(vllm:time_to_first_token_seconds_sum{ {{scope}} }[1m]))
        threshold: "0.5"     # per-replica TTFT-SLO target (seconds)
      sglang:
        query: sum by ({{group}}) (max_over_time(sglang:time_to_first_token_seconds_sum{ {{scope}} }[1m]))
        threshold: "0.5"
  pool-queue:                # engine-AGNOSTIC; per-replica target MEASURED via a query (thresholdQuery)
    query:          sum by ({{group}}) (max_over_time(inference_pool_average_queue_size{name="{{pool}}"}[1m]))
    thresholdQuery: avg by ({{group}}, pod) (<per-replica queue capacity>)
```
```yaml
# ConfigMap: wva-policy-interactive   (TIER — selection + weights; identity-free, no model_name)
scaleUpThreshold: 0.85     # WVA composite no-change band (not a KEDA field)
scaleDownBoundary: 0.70
priority: 10
analyzers:
  - name: saturation       # INTERNAL (built-in Go token/capacity analyzer)
    enabled: true
    score: 1.0
  - name: ttft-slo         # EXTERNAL (PromQL, defined in the wva-analyzers catalog)
    enabled: true
    score: 0.5
    threshold: "0.4"        # KEDA-compatible per-analyzer override (tighter TTFT for interactive)
```
```yaml
# ConfigMap: wva-defaults   (GLOBAL default + budget scope)
limiters:                    # declaring one is what enables limiting
  - type: gpu-inventory
enableRescale: true
gpuBudget: 128
defaultPolicy: standard      # used when a SO names no policy
scaleUpThreshold: 0.85       # fallback if a policy omits it
scaleDownBoundary: 0.70
```

**Composition caveat:** with multiple analyzers, **compose them inside WVA** (weighted/composite `score` → one `D/P` per ScaledObject) and return a **single** scaler metric. Do **not** map each analyzer to a separate KEDA trigger — HPA takes the *max* across triggers, which is not the weighted composite. Keep #1444's guards: `P ≤ 0` divide-by-zero, `orZero`, bare selectors + `{{scope}}` for arbitrary expressions.

### 7.7 Simplified policy schema (vs. current `SaturationScalingConfig`)

Prune the schema, keep the plumbing (`Merge()` / `ApplyDefaults()` / `Validate()` are reused as the layering + defaults + validation primitives):

- **Delete** `ModelID`, `Namespace` — identity moves to metadata.
- **Move** `EnableLimiter`, `EnableRescale`, GPU budget → the default/namespace layer (budget scope, not per-tier policy).
- **Drop** V1 thresholds (`KvCacheThreshold`, `QueueLengthThreshold`, `KvSpareTrigger`, `QueueSpareTrigger`) and the `AnalyzerName`-vs-`Analyzers` duality **if V2-only**; else isolate behind a legacy struct.
- **Keep** `priority` (tier default), the composite no-change band `scaleUpThreshold` / `scaleDownBoundary` (WVA-specific — no KEDA equivalent), and `analyzers` — each entry `{name, enabled, score}` plus **KEDA-compatible per-analyzer overrides** `threshold` / `activationThreshold`; `name` now points at a catalog (external) or built-in (internal) analyzer.

### 7.8 Worked example: an interactive pool (prefill + decode, vLLM)

One InferencePool `chat-pool` serving `llama-3-70b`, disaggregated into two Deployments — `chat-prefill` and `chat-decode` — both vLLM. This shows *N ScaledObjects → 1 pool*, role inference, and a policy mixing an **internal** and an **external** analyzer.

```yaml
# ── Two ScaledObjects, one per Deployment/LWS (the pool is a grouping key, not a scale handle) ──
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: { name: chat-prefill, namespace: chat }
spec:
  scaleTargetRef: { name: chat-prefill }        # the Deployment WVA scales (from SPEC, not metadata)
  minReplicaCount: 1
  maxReplicaCount: 8
  triggers:
    - type: external
      metadata:
        scalerAddress: wva-scaler.wva-system.svc:9090
        modelID: llama-3-70b
        variantCost: "2.5"                       # per-replica (H100) → per-accelerator by construction
        scalingPolicy: interactive
        # wvaOwnership: "true"                   # PLANNED, not yet shipped
        # pool + engine NOT set → inferred (pod-template labels vs the InferencePool
        #   selector; engine from the container image and args)
        # role NOT set → inferred = prefill (from `--disaggregation-mode prefill`)
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: { name: chat-decode, namespace: chat }
spec:
  scaleTargetRef: { name: chat-decode }
  minReplicaCount: 2
  maxReplicaCount: 16
  triggers:
    - type: external
      metadata:
        scalerAddress: wva-scaler.wva-system.svc:9090
        modelID: llama-3-70b
        variantCost: "2.5"
        scalingPolicy: interactive
        # wvaOwnership: "true"                   # PLANNED, not yet shipped
        # role inferred = decode
```

Both reference the **same reusable `interactive` policy** (§7.6 examples), which selects the **internal** `saturation` analyzer + the **external** `ttft-slo` analyzer; the catalog defines only the external one; `wva-defaults` supplies budget scope + fallbacks.

**Flow per cycle:**
1. KEDA calls the scaler for each SO → WVA registers `{chat/chat-prefill, chat/chat-decode}` in its TTL registry, both indexed under pool `chat-pool`.
2. WVA `GET`s each ScaledObject → `scaleTargetRef`, `min/max`, `behavior`; resolves its pods via ownerReferences (**Locator**); infers `role` from engine args.
3. Collector runs **internal `saturation`** (Go token/capacity model) and **external `ttft-slo`** (PromQL, vLLM body) over `llama-3-70b` metrics, attributed to each SO's pods.
4. Composite score (`interactive`: `saturation` 1.0, `ttft-slo` 0.5) → one `D/P` per SO.
5. Scaler returns `metricValue=D`, `targetSize=P`; each HPA scales its Deployment within `[min,max]`.
6. Under GPU contention, `enableRescale` and the declared `limiters` (from `wva-defaults`, priority-weighted) reallocate; an **urgent ceiling** lowers `maxReplicaCount` on a lower-priority pool to free GPUs immediately (§5).

### 7.9 Collector: efficient shared queries

Analyzer queries **are** the collector queries (§7.6), so optimizing them *is* optimizing the collector. The direction is **stay on Prometheus, improve the PromQL** — not replace the collector (KEDA can't be it, §12).

**Where it stands.** WVA's queries already aggregate across pods in one PromQL per signal — e.g. `max by (instance, pod)(max_over_time(vllm:kv_cache_usage_perc{…}[1m]))` — i.e. the KEDA-style "query the aggregator, don't scrape pods" is already there. But today each template bakes `model_name="{{.modelID}}"`, so a query fires **once per managed model** → `O(queries × models)` per cycle.

**Win #1 — one query across all managed models (`O(queries)`).** Drop the single-model filter; select the managed set with a regex (or fetch-all + filter in Go); add `model_name`/`namespace` to the group-by; fan out by `(namespace, model_name)` in Go. This is exactly what the catalog's `{{scope}}` / `{{group}}` placeholders expand to:

```promql
# per-model (today, ×M):
max by (instance, pod)         (max_over_time(vllm:kv_cache_usage_perc{namespace="ns1",model_name="A"}[1m]))
# collapsed (once):
max by (namespace, model_name, instance, pod)
                               (max_over_time(vllm:kv_cache_usage_perc{namespace=~"ns1|ns2",model_name=~"A|B|C"}[1m]))
```
Note the group-by drops `llm_d_ai_variant` (variant is gone, §7.1) and keeps `pod` — pods are attributed to their ScaledObject via the **Locator** (ownerReferences, §7.4), not a label.

**Win #2 — recording rules.** Push the heavy expressions (rates, `max_over_time`, histogram averages) into Prometheus recording rules; WVA reads a cheap precomputed series:
```yaml
- record: wva:kv_usage:max_by_pod
  expr: max by (namespace, model_name, instance, pod) (max_over_time(vllm:kv_cache_usage_perc[1m]))
```
Per-cycle Prometheus cost then stays small and flat regardless of fleet size. (Trade-off: ships Prometheus rules + inherits the eval-interval delay.)

**Win #3 — dedup & cache.** Fetch each raw metric **once** (e.g. `vllm:kv_cache_usage_perc` is consumed by several analyzers) and share it; give static/info metrics (`vllm:cache_config_info`) a **long cache TTL** rather than re-querying every cycle. WVA's source registry already caches — just tier the TTLs (seconds for load, minutes+ for config).

**Label-surface constraints (§7.4, and the label-surface reference).** `vllm:`/`sglang:` metrics are keyed by `model_name` only (no pool/variant label), so serving analyzers key on `model_name` + Locator attribution — you *cannot* re-key them on the pool. Engine-agnostic analyzers may use EPP `inference_pool_*` (keyed by pool `name`, **no namespace** — GIE #2309, watch cross-namespace collisions).

**Scale lever (optional).** If query volume/cardinality outgrows a single Prometheus, swap in a Prometheus-compatible store (**VictoriaMetrics** single-node) with no query changes; or read pre-aggregated pool signals from **EPP** directly. These are drop-in, not part of the core proposal.

---

## 8. Discovery: call-driven, no cluster watch

- The gRPC call **is** the registration. KEDA only calls WVA for ScaledObjects whose trigger targets WVA; each call self-registers that object (`name`, `namespace`, `scalerMetadata`). Objects that don't reference WVA are never seen.
- WVA keeps an **in-memory registry** `{ns/name → scalerMetadata, cached min/max/behavior, last-seen}`.
- **No `list`/`watch`.** RBAC is `get`/`patch` on ScaledObjects **by name** only.
- **TTL / age-out.** Without a watch there are no delete events; entries are dropped after several missed poll intervals. This is safe because WVA sets **no `ownerReferences`/finalizers** on user objects — nothing to clean up when an object disappears.

### Reads: cached bounds + behavior (decision inputs)
WVA needs current `min/max` + `behavior` to decide (predict clamping, respect per-target `max` in global fair-share, place the urgent ceiling, judge whether the metric path is fast enough). So each registry entry is **enriched by a targeted `GET`** of that one ScaledObject, cached, refreshed on-call or on a TTL. Reads are **per-object on the called set** — still no informer.

- **Writes are blind SSA** (declarative; no read-then-diff needed).
- **Reads are for decisions:** in unmanaged mode they are *constraints*; in managed mode they are WVA's own values (read mainly to detect drift/SSA conflicts).

### Global-optimizer completeness
WVA's cross-model cost/quota/fair-share needs the full managed set, which holds because:
1. The registry **converges within one KEDA `pollingInterval`** (KEDA calls every trigger).
2. WVA's **telemetry/workload discovery (collector)** is a *separate plane* from the ScaledObject registry — global optimization keys off telemetry WVA already scrapes; the registry only says *where to deliver* each decision.

## 9. What WVA cannot do through this contract

- **Push status onto the ScaledObject:** the protocol has no status field. KEDA reflects scaler *behavior* into `ScaledObject.status` (`Active`/`Ready`/`Fallback` conditions, `health` failure counts, events), but WVA cannot author it. WVA surfaces its own decisions via its existing `wva_*` metrics / CR-status / events.
- **Receive metrics from KEDA:** the channel is one-way (WVA → KEDA). WVA can *read back* KEDA's `keda_scaler_metrics_value` or the external-metrics API, but that is a re-export of upstream signals WVA already reads directly — not useful as an input.
- **Reuse KEDA as a collector:** KEDA scalers are single-query threshold checks wired into KEDA's own loop, not a general telemetry API. WVA keeps its own collector (multi-source, per-model/role aggregation, engine-arg parsing).

## 10. Relationship to WVA's existing components

- **Collector / optimizer:** decision-making stays in WVA (reads Prometheus/EPP directly) — *not* replaced by KEDA (§12). The collector's **queries are optimized** (§7.9): one grouped query across all managed models instead of per-model, optional recording rules, and dedup/caching. Its ingestion *role* is unchanged; its query *shape* improves.
- **`ScaledObjectReconciler`:** today it discovers/reads managed objects (annotation `llm-d.ai/managed`). This proposal narrows it to **call-driven** interaction and adds **conditional SSA ownership** + the **urgent-ceiling** path.
- **Actuator:** the external-scaler gRPC server is a new delivery surface alongside (or replacing) the metric-emission path.

## 11. RBAC

- ScaledObjects: `get`, `patch` (SSA) — **no `list`/`watch`**.
- HPA (optional, for status/current-replicas): `get`.
- gRPC service exposed for KEDA to reach (Service + NetworkPolicy).

## 12. Alternatives considered

1. **KEDA `prometheus` trigger on `wva_desired_replicas`** (reuses KEDA's Prometheus read; drops `prometheus-adapter`). Simpler, but no `scalerMetadata` channel, no `IsActive` ownership hook, and no clean place for managed/urgent. Good default for pure metric delivery; this proposal is the superset.
2. **`prometheus-adapter` → HPA external metrics** (status quo). Extra component; no scale-to-zero; no managed mode.
3. **WVA patches the Deployment scale subresource directly** (bypassing HPA/KEDA). Fights HPA; loses stabilization/behavior; rejected except for scale-from-zero edge cases already handled by the direct actuator.
4. **KEDA as WVA's collector / metrics source** (reuse KEDA scalers to gather telemetry, WVA reads `keda_scaler_metrics_value` or `external.metrics.k8s.io`). **Rejected — structurally unsupported.** KEDA has **no metrics-only mode as of 2026 (v2.20)**: `scaleTargetRef` is mandatory (the direct ask, discussion #2762, is rejected by-design; #1281 only produced OTel observability push-metrics; #470 — single external-metrics adapter per cluster — is a standing blocker, open Oct 2025). The only path is a dummy-Deployment + `autoscaling.keda.sh/paused` hack read via the external-metrics API — fragile, object-sprawling, and scalar-per-trigger (unfit for WVA's joined per-model/role telemetry). WVA keeps its own collector; KEDA is actuation-only. Sources: https://github.com/kedacore/keda/discussions/2762 · https://github.com/kedacore/keda/issues/1281 · https://github.com/kedacore/keda/issues/470 · https://keda.sh/docs/2.20/reference/scaledobject-spec/

## 13. Risks / open questions

- **Two write paths:** ensure the metric and any spec patches never both drive the *count* — the metric drives the count *within* WVA-owned `[min,max]`; urgent only lowers the ceiling. Keep the metric consistent with the urgent ceiling so relaxing `max` doesn't cause a rebound. Note the actuated scale-down terminates replicas (in-flight requests) — rely on graceful termination / PodDisruptionBudgets.
- **KEDA validating webhook** may reject some spec changes (e.g., conflicting `scaleTargetRef`) — confirm the owned-field set is webhook-safe.
- **Poll cadence vs urgency:** urgent override mitigates KEDA `pollingInterval` lag, but an object WVA has *never been called about* cannot be urgent-rescaled (acceptable — it isn't wired to WVA).
- **Registry TTL tuning:** age-out threshold vs KEDA `pollingInterval`; avoid dropping live entries.
- **Float vs int metric** across KEDA versions (`metricValueFloat`).
- **Confirm exact `externalscaler.proto` fields and `keda_scaler_metrics_value` labels** against the target KEDA version before implementation.

## 14. Sources to verify against implementation

- KEDA external scalers (concept + proto): https://keda.sh/docs/latest/concepts/external-scalers/
- KEDA ScaledObject spec (min/max, `advanced…behavior`, fallback, pollingInterval): https://keda.sh/docs/latest/reference/scaledobject-spec/
- KEDA Prometheus scaler (alternative delivery): https://keda.sh/docs/latest/scalers/prometheus/
- Kubernetes Server-Side Apply (field managers): https://kubernetes.io/docs/reference/using-api/server-side-apply/
- HPA algorithm / behavior (stabilization, policies): https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/
- WVA analyzer-metric-interface proposal (D/P contract, config-driven PromQL analyzers): https://github.com/llm-d/llm-d-workload-variant-autoscaler/pull/1444 (merged) · follow-up issue #1455
- Current policy struct being simplified: `internal/config/saturation_scaling.go` (`SaturationScalingConfig`)
