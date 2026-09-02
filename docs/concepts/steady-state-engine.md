# The steady-state engine

The Workload Variant Autoscaler (WVA) is a **global optimizer**: each cycle it builds a
capacity model for your inference workloads and computes how many replicas each one should
have. It does not patch replica counts directly — it emits a target that HPA or KEDA actuate.
This guide explains, at a high level, the **steady-state engine**: the terms it uses, the
metrics it reads, the thresholds that govern it, and the algorithm that turns those into replica
targets.

It is aimed at operators tuning and observing WVA. For the full configuration field reference and
the EPP coordination workflow, see
[Saturation Scaling Configuration](../reference/scaling-policy.md); for the
complete metrics catalog see
[Metrics & Health Monitoring](../reference/metrics.md).

## Terminology

| Term | Meaning |
|---|---|
| **Model** | A served model (a `modelID`, e.g. `Qwen/Qwen3-0.6B`) in a namespace. The unit WVA makes a scaling decision for. |
| **Variant** | One deployable configuration of a model — a Deployment/LeaderWorkerSet, typically pinned to an accelerator type (and, for disaggregated serving, a role). A model may have several variants (e.g. A100 vs H100, or prefill vs decode). |
| **Replica** | One running pod of a variant. |
| **Role** | For prefill/decode-**disaggregated** serving, a variant serves the `prefill` or `decode` stage; non-disaggregated variants are `both`. |
| **EPP** | *Endpoint Picker* — the request router in the llm-d inference scheduler ([Gateway API Inference Extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension)). EPP spreads requests across replicas; WVA decides how many replicas exist. |
| **Capacity / supply** | How much work a replica (or the whole model) can serve, measured in **KV-cache tokens**. |
| **Demand** | How much work the model currently needs to serve, also in KV-cache tokens. |

## Overview

WVA has one saturation analyzer: the token-based engine described here. The
earlier percentage-based analyzer has been removed, so there is nothing to select
between — what follows is simply how saturation analysis works.

| Aspect | **Token-based analyzer** |
|---|---|
| Reasons in | Absolute **KV-cache tokens** (supply vs. demand) |
| Scale-up size | a division: `ceil(required tokens / per-replica capacity)` |
| Multi-replica jumps | native |
| Heterogeneous accelerators | first-class (per-variant capacity) |
| Zero-replica sizing | supported (capacity derived from deployment args) |
| GPU-constrained fair-share | supported (with the limiter) |

It builds an explicit capacity model in units of KV-cache tokens, so a single
decision can jump from, say, 2 → 5 replicas, or size a variant that currently has
**zero** replicas.

### Configuring it

The analyzer runs unconditionally. The shipped saturation-scaling ConfigMap names
it explicitly on the global `default` entry, which is the recommended form:

```yaml
analyzers:
  - name: saturation
    score: 1.0
```

The legacy `analyzerName: "saturation"` spelling is still accepted. Per-model and
per-namespace entries can override *thresholds*.

## How it works

Each cycle, for every model, the engine runs four core steps (plus an optional fifth when the GPU
limiter is enabled).

### 1. Per-replica capacity = the tighter of two ceilings

A replica can't serve past whichever wall it hits first — **memory** or **compute** — so its
usable capacity is the **smaller** of two ceilings, both in KV-cache tokens:

- **Memory ceiling** = `KV_token_capacity × kvCacheThreshold`.
  `KV_token_capacity` (= `num_gpu_blocks × block_size`) is the pod's physical KV-cache size,
  read from `vllm:cache_config_info` (or derived from the deployment's engine args when the
  metric is absent). `kvCacheThreshold` (default **0.80**) reserves headroom — only 80% of the
  cache is treated as usable.
- **Compute ceiling** = how many concurrent tokens the GPU can actually *process* before
  scheduling throughput — not memory — becomes the bottleneck (common for long-generation
  workloads). The engine estimates it from live queue behavior when a replica is queue-saturated,
  otherwise from recent history, otherwise from the deployment's batch/sequence limits. *(The
  detailed estimation chain is in the [developer guide](../reference/scaling-policy.md).)*

A variant's per-replica capacity is the **median** of its ready replicas' capacities (median so
one outlier pod doesn't skew it). A variant with no ready replicas borrows capacity derived from
its deployment args or from a compatible sibling variant — this is what lets the engine size a
zero-replica variant.

### 2. Demand — the tokens the model needs to serve

Demand is also in KV-cache tokens and sums three sources:

```text
demand = tokens_in_use          (live: kv_cache_usage × capacity)
       + local_queue_tokens     (requests waiting on the pod × avg input tokens)
       + scheduler_queue_tokens  (requests still queued upstream in the EPP flow-control layer,
                                   before they reach any pod)
```

The upstream scheduler-queue term accounts for load that hasn't landed on a pod yet; its
input-token portion is discounted by the observed prefix-cache hit rate when available.

### 3. The scaling signal

The engine aggregates to model level:

- **`TotalSupply`** = Σ (ready replicas × per-replica capacity)
- **`TotalAnticipatedSupply`** = Σ ((ready + **pending**) replicas × per-replica capacity)
- **`TotalDemand`** = the demand above

We want demand to sit at the target utilization of supply — so the supply we *need* is
`demand ÷ scaleUpThreshold` (e.g. at 0.85, demand should be 85% of supply). That gives two
signals:

```text
RequiredCapacity  = max(0, TotalDemand / scaleUpThreshold  − TotalAnticipatedSupply)   → scale up
SpareCapacity     = max(0, TotalSupply  − TotalDemand / scaleDownBoundary)             → scale down
```

- **`RequiredCapacity > 0`** → the supply we need exceeds what's already in flight → **scale up**.
  Because *anticipated* supply counts pending replicas, an in-flight scale-up shrinks
  `RequiredCapacity` — this avoids counting the same scale-up twice.
- **`SpareCapacity > 0`** → even after inflating demand to the `scaleDownBoundary`, supply is
  left over → **scale-down is safe**.
- Neither → **no change**. The band between the two thresholds is a stable no-change region;
  note WVA has no time-based stabilization window yet, so keeping the band wide is what reduces
  flapping.

> The engine also reports a model `utilization = TotalDemand / TotalSupply` for observability, but the
> decision is driven by `RequiredCapacity`/`SpareCapacity` above — **not** by comparing that
> ratio to the thresholds. (This `SpareCapacity` token signal is distinct from the
> `wva_spare_capacity` *metric*, which is a 0–1 headroom fraction — see [Metrics](#metrics).)

### 4. Replica target

```text
scale-up replicas   = ceil(RequiredCapacity / per_replica_capacity)
scale-down replicas = floor(SpareCapacity   / per_replica_capacity)
```

Scale-down respects each variant's `minReplicas`, removes highest-cost replicas first, and (for
prefill/decode-disaggregated models) keeps the two roles coupled so the model stays functional.

### 5. (Optional) GPU-constrained fair-share

When a `limiters:` entry is declared, the desired counts are capped by actually-available GPUs,
fair-shared across competing models weighted by each model's `priority`. With no limiter declared,
scale-up is unconstrained and the optimizer simply minimizes cost. Priority weighting applies
**only** in limiter mode.

## Thresholds

Set these in the saturation-scaling ConfigMap (see the
[configuration reference](../reference/scaling-policy.md) for structure and
per-model/per-namespace overrides).

| Key | Default | Range | Effect |
|---|---|---|---|
| `kvCacheThreshold` | `0.80` | 0–1 | Fraction of physical KV cache treated as usable → sets the **memory ceiling**. Lower = more headroom. **Agree with EPP — [see below](#aligning-thresholds-with-epp).** |
| `queueLengthThreshold` | `5` | ≥ 0 | Local waiting-queue depth at/above which a replica is "queue-saturated," letting its current token load set the **compute ceiling**. **Agree with EPP — [see below](#aligning-thresholds-with-epp).** |
| `scaleUpThreshold` | `0.85` | (0, 1], `> scaleDownBoundary` | Target peak utilization. Drives **`RequiredCapacity`**. Lower = scale up earlier. |
| `scaleDownBoundary` | `0.70` | (0, 1], `< scaleUpThreshold` | Utilization floor for safe scale-down. Drives **`SpareCapacity`**. The gap to `scaleUpThreshold` is the no-change band. |
| `limiters` | `[]` | list | Zero or more `{type: gpu-inventory\|quota, ...}` entries. Declaring one turns on step 5 (GPU-constrained fair-share) and arms the scale-from-zero capacity check; declaring none leaves both off. |
| `priority` | `1.0` | ≥ 0 | Relative weight when fair-sharing scarce GPUs (**limiter mode only**). |
| `analyzerName` / `analyzers` | `""` / — | — | Selects the saturation analyzer (`"saturation"`) and, via the `analyzers` list, per-analyzer scores and threshold overrides. |



**Tuning guidance**
- Keep `scaleUpThreshold` and `scaleDownBoundary` apart (e.g. 0.85 / 0.70): a narrow gap risks
  flapping, a wide gap makes scaling sluggish.
- Lower `kvCacheThreshold` if you see pressure near the KV-cache limit — and mirror it in EPP.
- Set values globally (`default`) and override per model (`{modelID}#{namespace}`).

### Aligning thresholds with EPP

`kvCacheThreshold` and `queueLengthThreshold` define **what "a saturated endpoint" means**, and
they should be **agreed with EPP** (the request router). EPP and WVA read the **same two
model-server metrics** — `vllm:kv_cache_usage_perc` and `vllm:num_requests_waiting` — but act at
different layers: EPP routes and, under load, queues/sheds requests as endpoints approach
saturation; WVA decides when the pool is saturated and adds replicas. EPP's Saturation Detector
exposes two thresholds that map one-to-one to WVA's:

| EPP (`saturationDetector` / `utilization-detector`) | Default | WVA | Default |
|---|---|---|---|
| `kvCacheUtilThreshold` | `0.8` | `kvCacheThreshold` | `0.80` |
| `queueDepthThreshold` | `5` | `queueLengthThreshold` | `5` |

If the two disagree, routing and scaling fight each other:
- **WVA higher than EPP** → EPP queues requests in flow-control (raising latency, and inflating
  WVA's own `scheduler_queue` demand term) while WVA still sees headroom → scale-up lags.
- **WVA lower than EPP** → WVA scales up while EPP is still comfortably balancing → over-provisioning.

**Guidance:** set WVA's `kvCacheThreshold` = EPP's `kvCacheUtilThreshold` and `queueLengthThreshold`
= EPP's `queueDepthThreshold`, and mirror any change on both sides. This repo configures EPP's
scorers (`queue-scorer`, `kv-cache-utilization-scorer`) in `deploy/lib/epp-flow-control.values.yaml`.
See the developer guide's
[Coordinating with InferenceScheduler (EPP)](../reference/scaling-policy.md#best-practices-coordinating-with-inferencescheduler-end-point-picker)
for the full apply/verify workflow.

**References**
- EPP EndpointPicker configuration (`saturationDetector` fields, `headroom`, `metricsStalenessThreshold`): <https://gateway-api-inference-extension.sigs.k8s.io/guides/epp-configuration/config-text/>
- EPP model-server metrics protocol (queue size, KV-cache utilization): <https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/docs/proposals/003-model-server-protocol/README.md>

## Metrics

### Input — what the engine reads from your inference engines

Scraped from vLLM and the EPP scheduler. If these are missing or stale, the engine degrades or skips the
model — check the health metrics below.

| Metric | Type | Used for |
|---|---|---|
| `vllm:cache_config_info` (`num_gpu_blocks`, `block_size`) | info gauge | KV token capacity → **memory ceiling** |
| `vllm:kv_cache_usage_perc` | gauge | live tokens-in-use → **demand** |
| `vllm:num_requests_waiting` | gauge | local queue → **compute-ceiling** trigger + demand |
| `vllm:request_prompt_tokens_sum` / `_count` | histogram | average **input** tokens per request |
| `vllm:request_generation_tokens_sum` / `_count` | histogram | average **output** tokens per request |
| `vllm:prefix_cache_hits` / `vllm:prefix_cache_queries` | counters | prefix-cache hit rate (discounts input-token demand) — *optional* |
| `inference_extension_flow_control_queue_size` / `_bytes` | gauges (EPP) | upstream scheduler-queue demand — *optional* |

Deployment engine args (`--max-num-seqs`, `--gpu-memory-utilization`, `--block-size`,
`--max-model-len`, `--max-num-batched-tokens`, `--num-gpu-blocks-override`, …) are read from the
Deployment/LeaderWorkerSet — **not** Prometheus — and feed the derived-capacity path.

### Output — WVA metrics to observe the engine's decisions

| Metric | Type | Meaning |
|---|---|---|
| `wva_saturation_utilization` | gauge | Utilization = demand / capacity (0–1). |
| `wva_required_capacity` | gauge | > 0 ⇒ scale-up needed; the value is the KV-cache **token** deficit (per-role for P/D-disaggregated models). `unit=continuous`. |
| `wva_spare_capacity` | gauge | > 0 ⇒ safe scale-down headroom; the value is the KV-cache **token** surplus — the companion to `wva_required_capacity`. |
| `wva_kv_cache_tokens_used` / `_capacity` | gauges | Total KV tokens in use vs. total capacity across replicas. |
| `wva_desired_replicas` / `wva_current_replicas` | gauges | What the engine wants vs. what is running. |
| `wva_desired_ratio` | gauge | desired / current — the value HPA/KEDA actuate on. |
| `wva_replica_scaling_total` | counter | Scale actions, labeled by `direction` and `reason`. |
| `wva_config_info` | gauge | Active `analyzer_name` + feature flags — confirm the saturation analyzer is selected. |
| `wva_config_optimization_interval_seconds` | gauge | Decision cadence. |
| `wva_models_processed` | gauge | Models handled last cycle. |
| `wva_available_gpus` | gauge | GPUs available (limiter input). |
| `wva_metrics_freshness_status`, `wva_metrics_pods_discovered`, `wva_metrics_collection_errors_total` | gauges / counter | **Input health** — check these first when scaling looks stuck. |

## Operating the engine

### Verify the engine is active

Check `wva_config_info` — the active engine appears in the `analyzer_name` label:

```promql
wva_config_info{analyzer_name="saturation"}   # == 1 when the saturation analyzer is running
```

If it shows a different `analyzer_name` (or the metric is absent), it is not selected — confirm
the ConfigMap's `default` entry sets `analyzerName: "saturation"`.

### Why isn't it scaling?

Work down this list:

1. **No inputs.** `wva_metrics_pods_discovered == 0`, or `wva_metrics_freshness_status{status="stale"|"missing"}` — WVA isn't getting fresh metrics. Check pod discovery / scraping.
2. **Collection errors.** `wva_metrics_collection_errors_total` climbing — a query is failing (labeled by `query_type`).
3. **Not saturated.** `wva_saturation_utilization` is below `scaleUpThreshold` and `wva_required_capacity == 0` — expected no-change.
4. **Scale-up already in flight.** Pending replicas count toward *anticipated* supply, so `wva_required_capacity` stays `0` until they become ready.
5. **Capped by GPUs.** With a limiter declared, `wva_available_gpus` at/near 0 (or namespace quota exhausted) blocks scale-up even when demand warrants it.
6. **Actuation.** `wva_desired_ratio > 1` but replicas don't change → the HPA/KEDA object consuming the signal isn't acting; check the actuator, not WVA.

## Example configuration

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-scaling-policy-config
  namespace: workload-variant-autoscaler-system   # WVA controller namespace
data:
  default: |
    analyzerName: "saturation"   # select the saturation analyzer
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    limiters:
      - type: gpu-inventory
    priority: 1.0
  # optional per-model override. model_id and namespace in the BODY are what bind
  # it to a model — the key is only a label, and must be a legal ConfigMap key
  # ([-._a-zA-Z0-9] only, so no "/" or "#"). An entry that names no model is
  # registered as a named policy tier instead.
  my-model.my-namespace: |
    model_id: my-model
    namespace: my-namespace
    scaleUpThreshold: 0.90       # let this model run hotter before scaling
```

## See also

- [Saturation Scaling Configuration](../reference/scaling-policy.md) — full field reference, per-model/namespace resolution, and the EPP coordination workflow.
- [Metrics & Health Monitoring](../reference/metrics.md) — complete metrics catalog and health endpoints.
- [Monitoring](../reference/monitoring.md) — dashboards and observing WVA.
- [Quota Limiter](../reference/quota-limiter.md) — per-accelerator GPU caps used by the fair-share step.
