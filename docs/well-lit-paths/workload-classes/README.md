# Give classes of workloads different scaling behaviour

An interactive assistant and an overnight batch job should not be scaled the
same way, and neither should be configured one model at a time. A **tier** is a
named, reusable policy entry — `interactive`, `standard`, `batch` — that a
variant selects by name from its own ScaledObject. A tier carries no model
identity, which is the entire point: one tier serves many models, and retuning
the tier retunes all of them at once.

**Use it when** your models fall into a small number of classes with different
urgency, and you would rather change a class than edit every model in it.

**Do not use it when** one model genuinely needs its own numbers. That is a
per-model override, and it wins over any tier the variant names — reach for it
deliberately, not as the way you configure everything.

## What it needs

- Tier entries in the scaling-policy ConfigMap. An entry **is** a tier when its
  key is not `default` and its body sets no `model_id`:

```yaml
data:
  default: |
    scaleUpThreshold: 0.85
    scaleDownBoundary: 0.70
    priority: 1.0
    defaultPolicy: standard      # what a variant naming no tier gets
  interactive: |
    priority: 5.0
    scaleUpThreshold: 0.75
  batch: |
    priority: 0.5
```

- A `scalingPolicy` key in each variant's KEDA trigger metadata. The trigger is
  already the registration, so selecting a tier needs nothing watched, listed or
  matched:

```yaml
triggers:
  - type: external-push
    metadata:
      modelID: ibm/granite-13b
      scalingPolicy: interactive
```

## How a variant's policy is resolved

Most specific wins: **per-model override → named tier → `default.defaultPolicy`
→ `default`**. `defaultPolicy` is read from the `default` entry only, because a
fallback chosen by a tier would be that tier choosing for everyone but itself.

Two things that could be silent are not:

- **A tier that does not exist** falls back to `default` — the right outcome and
  the wrong silence, so it is logged against the variants that asked for it,
  listing the tiers that do exist.
- **Variants of one model naming different tiers** resolve to the
  lexicographically smallest, so the allocation comes out the same on every
  cycle rather than flipping with map order.

## Setting it up

The tier surface, the full field list and the per-model override that outranks
it are in [the scaling policy reference](../../reference/scaling-policy.md).
Tiers are edited in place: the policy is dynamic configuration, so the engine
picks up a changed ConfigMap without a restart.

## Verifying it

```bash
# which policy each variant resolved to, and the thresholds it got
kubectl logs -n <wva-namespace> deploy/<wva> | grep analyzer-result
```

The thresholds in the `analyzer-result` line are the ones the variant actually
resolved — the way to tell a tier that applied from a tier that was named and
silently fell back.

## What it costs

Nothing to run. The cost is a naming decision: a tier is a promise that every
model wearing it wants the same behaviour, and the moment that stops being true
somebody adds a per-model override and the class stops meaning anything.

## How it is tested

End-to-end: `test/e2e/scaling_policy_tier_test.go` — a named tier taking a
workload where the default entry would not, fallback when the tier does not
exist, a per-model override beating the tier, and the tier's thresholds
resuming once that override is removed.

**The suite needs `USE_SIMULATOR=true`** and skips without it: it drives
`llm-d-inference-sim --fake-metrics`, which real vLLM rejects. A run without
the simulator is green while covering none of this.

## Tuning it

Priorities interact with the limiter. When there are not enough accelerators
for every model's demand, priority is the weight in the fair-share split
(`fairShareValue` in `internal/engines/allocation/`) — so a tier's `priority`
is what a class is worth under contention, not just how eagerly it scales when
there is room. See [bounding a fleet by real GPUs](../bound-by-gpus/).
