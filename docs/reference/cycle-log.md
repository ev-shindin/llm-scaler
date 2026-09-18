# WVA Cycle Log

The WVA saturation engine emits two structured INFO log lines per reconcile
cycle per model. These lines are the primary observability instrument for
understanding what the analyzer and optimizer computed without enabling
verbose debug logging.

---

## Log lines

### `analyzer-result`

Emitted once per analyzer that ran for a model, immediately after the
universal threshold post-step has been applied. The values reflect what the
optimizer actually receives.

```json
{
  "level": "info",
  "msg": "analyzer-result",
  "modelID": "my-model",
  "namespace": "default",
  "analyzer": "saturation",
  "supply": 658534,
  "demand": 1041047,
  "util": 1.58,
  "rc": 0,
  "sc": 50000,
  "scaleUpThreshold": 1.1,
  "scaleDownBoundary": 0.7,
  "variants": [
    {"name": "primary", "prc": 1152000, "role": "both", "reason": "P3-k2"},
    {"name": "v2",      "prc":  403391, "role": "both", "reason": "P1-obs"}
  ]
}
```

| Field | Description |
|---|---|
| `modelID` | WVA model ID (unique within a namespace) |
| `namespace` | Kubernetes namespace |
| `analyzer` | Analyzer name, e.g. `"saturation"`, `"throughput"` |
| `supply` | Total token supply across ready replicas (readyCount × perReplicaCapacity) |
| `demand` | Total token demand. Not purely observed: for saturation V2 it is the sum of three terms — resident KV tokens, a role-aware projection of requests waiting in each replica's local engine queue, and a model-level, prefix-cache-discounted projection of requests still queued upstream in llm-d flow control (`SchedulerQueue`, not attributed to any variant). See [scaling-policy-config.md](scaling-policy.md) |
| `util` | `demand / supply`; > 1.0 means the model is over capacity |
| `rc` | Required capacity signal (post-threshold): > 0 triggers scale-up |
| `sc` | Spare capacity signal (post-threshold): > 0 permits scale-down |
| `scaleUpThreshold` | Scale-up threshold resolved for this analyzer (from config) |
| `scaleDownBoundary` | Scale-down boundary resolved for this analyzer (from config) |
| `variants[].name` | Variant name |
| `variants[].prc` | Per-replica capacity in analyzer units (tokens for saturation) |
| `variants[].role` | Resolved P/D role: `prefill`, `decode`, or `both`. Renders as `both` both when the scale target has no `llm-d.ai/role` label and when the analyzer does not populate the role at all. Saturation V2 charges waiting requests by this role, so it is needed to interpret `demand` |
| `variants[].reason` | How the variant's capacity was computed (see below) |

If an analyzer does not compute per-variant capacity, `variants` is an empty
array. Multiple `analyzer-result` lines appear when more than one analyzer is
enabled; each has the same `modelID`/`namespace` and its own `analyzer` field.

### `scaling-decision`

Emitted once per model after the optimizer has produced its final per-variant
replica targets.

```json
{
  "level": "info",
  "msg": "scaling-decision",
  "modelID": "my-model",
  "namespace": "default",
  "decisions": [
    {"name": "primary", "curr": 1, "tgt": 2, "action": "ScaleUp"},
    {"name": "v2",      "curr": 1, "tgt": 1, "action": "NoChange"}
  ]
}
```

| Field | Description |
|---|---|
| `modelID` | WVA model ID |
| `namespace` | Kubernetes namespace |
| `decisions[].name` | Variant name |
| `decisions[].curr` | Current replica count at the time of this cycle |
| `decisions[].tgt` | Target replica count chosen by the optimizer |
| `decisions[].action` | `ScaleUp`, `ScaleDown`, or `NoChange` |

---

## Reason values (`reason` field)

The `reason` field in `analyzer-result` variants is set by each analyzer to
describe how it computed the variant's per-replica capacity. It is free text;
the saturation V2 analyzer sets one of these values:

| Reason | Meaning |
|---|---|
| `P0-store` | capacity came from the **capacity store** (no live replicas) |
| `P1-obs` | k2 came from **observed** tokens-in-use (queue was saturated), blended into the same rolling average `P2-hist` reads -- a single noisy cycle gets 1/N weight rather than taking over outright |
| `P2-hist` | k2 came from the **historical** rolling average |
| `P3-k2` | k2 was **derived** from deployment parameters (vLLM model args). Never fires for a **prefill** variant: the formula assumes a real per-request output length, which prefill's own avgOutput (~0-1, it hands off before generating anything) collapses to just the batch-token budget echoed back -- not a derived signal |
| `P4-k1` | k2 was unavailable; **fell back** to k1 (memory-bound capacity). For prefill this is the common case, not a degraded one -- see `P3-k2` above |

Two further values appear in the `k2-decision` line's `priority` field but never
as a variant's `reason`, because no capacity comes from either:

| value | meaning |
| --- | --- |
| `P1-obs-invalid` | an observation was discarded for exceeding the KV cache's **physical** ceiling, i.e. a scrape artifact. Note the bound is the ceiling, not k1: k1 is the ceiling times `kvCacheThreshold` (0.80 by default), so occupancy between the two is legitimate and is kept. The analyzer falls through to the next priority |
| `P1-obs-downstream` | a **prefill** replica's saturated queue was left unrecorded because a **decode** replica was over its queue threshold in the same cycle. A prefill request is done only when decode admits it and pulls its KV, so with decode saturated what prefill shows -- held KV, a queue, a completion rate -- is metered by decode: decode's saturation seen from upstream, not a reading of prefill. Neither k2 nor the saturated throughput (`throughput-demand-floor`) is recorded from it; the analyzer falls through to the next priority, and prefill's demand for the cycle is held where it neither orders nor releases (`prefill-demand-held`). A prefill bottleneck reduces decode's arrivals, so both saturate at once only when decode is short at prefill's completion rate; prefill's reading then waits for decode to recover |
| `no-data` | no ready replicas, no stored record, no compatible variant — capacity is 0 this cycle (normal for newly deployed variants) |
| `error` | K2 priority not in known set — indicates an unlabelled code path; should not occur in normal operation |

The `P1-obs`–`P4-k1` values reflect the representative replica for the variant
— specifically the replica whose effective capacity equals the lower median
across all ready replicas (the same replica that determined `prc`). A `P1-obs`
reason means live inference data is available and the capacity estimate is
high-confidence; a `P4-k1` reason means no compute-bound signal was available
for any replica and the estimate is conservative.

The throughput analyzer sets one of these values:

| Reason | Meaning |
|---|---|
| `T1-ols` | capacity from Tier-1 OLS fit (observation window ready) |
| `T2-pinned` | capacity from Tier-2 constrained OLS with a prior fitted B |
| `T2-default` | capacity from Tier-2 constrained OLS with the default baseline B (cold start) |
| `T2-failed` | both tiers failed — all replicas idle or no usable ITL signal; variant skipped this cycle |

Other analyzers may set their own reason values or leave `reason` empty.

---

## Grep patterns

```bash
# All analyzer results for a specific model
kubectl logs <pod> | grep '"msg":"analyzer-result"' | grep '"modelID":"my-model"'

# Saturation analyzer only
kubectl logs <pod> | grep '"msg":"analyzer-result"' | grep '"analyzer":"saturation"'

# Scaling decisions only (scale-up events)
kubectl logs <pod> | grep '"msg":"scaling-decision"' | grep '"action":"ScaleUp"'

# Full cycle for one model (both lines)
kubectl logs <pod> | grep -E '"analyzer-result"|"scaling-decision"' | grep '"modelID":"my-model"'
```

---

## Ordering and timing

Within a single reconcile cycle for one model:

1. One `analyzer-result` line per enabled analyzer (saturation first, then
   any registered non-saturation analyzers in registration order).
2. One `scaling-decision` line after the optimizer has processed all models
   in the cycle.

The two line types are not atomically adjacent in the log — other models'
`analyzer-result` lines may appear between a model's last `analyzer-result`
and its `scaling-decision`. Filter by `modelID` and `namespace` when
correlating them.

---

## Enabling the log

These lines are emitted at INFO level and appear in the default controller
log output. No feature flag or configuration change is required.
