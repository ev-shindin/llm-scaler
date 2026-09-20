# Quota Limiter

The **quota limiter** enforces operator-declared GPU caps that are independent
of the physical inventory observed in the cluster. It addresses the case where
an administrator wants to cap accelerator consumption regardless of how many
nodes actually exist — for example, to reserve burst capacity, divide a cluster
across tenants, or simulate a smaller budget than the hardware allows.

This document covers the design, configuration, and pipeline integration of
the quota limiter. The implementation closes
[#1002](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1002).

## Enabling

**A `limiters:` list containing a `quota` entry** on the saturation-scaling
ConfigMap's `default` entry, and that is the whole prerequisite. The installer
writes it for you — `make deploy-wva-on-k8s WVA_LIMITER=quota WVA_QUOTAS='H200=8'`
for one install's own policy, or `make enable-physical-limiter
WVA_LIMITER_TYPE=quota WVA_QUOTAS='H200=8'` for the cluster policy every
controller reads. `WVA_QUOTAS` is **required** by both: an entry that names no
accelerator is a budget of zero for every type, not unlimited, and would stop
every managed workload from scaling up. `WVA_QUOTA_SCOPE` selects `namespace`
(the default: each managed namespace gets the budget) or `cluster` (the sum
across all of them). It is the sole source that selects the quota
limiter (see [Selection & lifecycle](#selection--lifecycle)) and is applied
**live** — no restart. With no `limiters:` list, nothing limits: neither the
optimizer budget nor the scale-from-zero capacity check.

> **Changed:** this used to require a second setting, `enableLimiter: true`, and a
> `quota` entry declared without it was constructed and then never applied —
> replicas scaled unconstrained while the config read as capped. Declaring the
> limiter is now the request to be limited, so that failure mode is gone.

The quota entries use the same schema described below; place them inline under the
saturation `default` entry's `limiters:` list (see
[scaling-policy-config.md](scaling-policy.md#limiters-cluster-default-only-live)).

## Scope

The quota limiter is one of several limiters in the resource-limiting pipeline.
It is intentionally narrow:

- It does **not** discover physical inventory (that is `TypeInventory`'s job).
- It does **not** make scaling decisions (that is the optimizer's job).
- It does **not** compose itself with other limiters (that is the limiter
  chain's job — see sub-issue
  [#3](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1003)).

A `QuotaInventory` instance is tied to exactly one scope. Deployments that need
both cluster-wide and per-namespace caps configure two limiter entries; the
limiter chain consults both before committing a decision.

## Configuration

The limiter is declared via a ConfigMap parsed into `QuotaLimiterEntries`. The
top-level `limiters` key holds a list of entries; each entry has a `scope` of
either `cluster` or `namespace`.

> **Entry `type:`.** Within a quota entry the `type:` field must be `"quota"`.
> (The saturation `limiters:` list additionally accepts a `{type: gpu-inventory}`
> entry to select the physical limiter instead — see
> [Selection & lifecycle](#selection--lifecycle).) The field exists so future
> limiter types (e.g. `reservation`) can share this schema.

### Cluster scope

A cluster-scoped entry caps the total GPUs of a given accelerator type across
all namespaces. The `quotas` map keys are accelerator type names (e.g.,
`H100`); values are caps in GPUs.

```yaml
limiters:
  - name: cluster-quota
    type: quota
    scope: cluster
    quotas:
      H100: 16
      A100: 8
      L40S: -1  # unlimited
```

### Namespace scope

A namespace-scoped entry caps per-namespace consumption. The top-level keys of
`namespaceQuotas` are namespace names; inner keys are accelerator types.

```yaml
limiters:
  - name: namespace-quota
    type: quota
    scope: namespace
    exclude:
      - kube-system
      - llm-d-system
    namespaceQuotas:
      team-a:
        H100: 8
      team-priority:
        H100: -1   # unlimited for this tenant
      default:
        H100: 2    # per-namespace fallback for unlisted namespaces
```

> ⚠️ **Reserved key collision.** The string `default` is a reserved key in
> `namespaceQuotas` and selects the per-namespace fallback for unlisted
> namespaces. As a consequence, the *actual* Kubernetes `default` namespace
> cannot be assigned a quota by listing it directly. To enforce a cap on
> workloads in the K8s `default` namespace, either (a) list it in `exclude`
> if you want it to bypass this limiter, or (b) use a strict allowlist
> (omit the reserved key entirely so any unlisted namespace including
> `default` is denied).

### Special values

| Value | Meaning |
|-------|---------|
| Positive integer (up to `MaxQuotaValue` = 1,048,576) | Cap in GPUs |
| `-1` | Unlimited (Kubernetes convention; pass-through in the allocator) |
| `0` or missing entry | Denied (no allocation) |

A finite cap must not exceed `MaxQuotaValue` (`1 << 20` = 1,048,576) — far above any
realistic accelerator count; a larger value is rejected at startup (see Validation).

### Namespace lookup rules

When the allocator evaluates a request for namespace `N` and type `T` in
`namespace` scope:

1. If `N` appears in `exclude`, the limiter **passes through** — the request is
   returned unchanged and no usage is recorded. Other limiters in the chain
   still apply.
2. If `N` is listed in `namespaceQuotas`, that namespace's **whole map is a
   closed allowlist**: `namespaceQuotas[N][T]` is the cap if present, and a type
   `T` that `N` does **not** list is **denied** (granted = 0). A listed namespace
   never falls through to `default` for a missing type — listing a namespace opts
   it into "only the types I name".
3. Otherwise (`N` is not listed), if `namespaceQuotas["default"]` is present, the
   **`default` map** becomes `N`'s per-namespace budget: each unlisted namespace
   gets its own copy of those caps (a type absent from `default` is still
   denied). This matches the Kubernetes `LimitRange` default semantic; it is
   *not* a shared pool across unlisted namespaces.
4. Otherwise, the request is denied (granted = 0).

The fall-through to `default` is therefore at the **namespace** granularity, not
per `(namespace, type)`: it applies only when `N` is wholly unlisted.

### Kueue as a quota source

A quota entry can be **bounded by Kueue**. Kueue is the cluster's admission-time
quota authority; reading its figures makes WVA respect the same caps before it
asks KEDA for a replica that Kueue would then hold pending. Set `kueue.enabled`
on the entry:

```yaml
limiters:
  - name: namespace-quota
    type: quota
    scope: namespace
    kueue:
      enabled: true
      # resources: [nvidia.com/gpu]   # which extended resources count as GPUs;
      #                               # default: every vendor resource WVA knows
      # refreshInterval: 30s          # how often Kueue is re-read (default 30s)
    namespaceQuotas:                  # optional: the static caps below still apply
      team-a:
        H100: 8
```

**Both sources present → the smaller cap wins**, per namespace and accelerator
type. Neither source can raise what the other granted. With no static map at all
(`namespaceQuotas` / `quotas` omitted), Kueue's figures are the whole answer.

#### What is read

| Kueue object | Scope | Used for |
|---|---|---|
| `ResourceFlavor` | cluster | `spec.nodeLabels` names the accelerator behind a flavor — the vendor product label (`nvidia.com/gpu.product`, `amd.com/gpu.product-name`, …, or the GKE alias), kept **as written**: a flavor exists to tell one product from another (a 40GB PCIe A100 from an 80GB SXM one), so the grant is keyed by the full product and meets a static key at bound time (see below). A ClusterQueue that names a flavor **not present** on the cluster, or whose `stopPolicy` is `Hold`/`HoldAndDrain`, is inactive in Kueue and admits nothing — here it stays a *governing* queue that grants **nothing** (an untyped grant of 0), so its namespaces are denied rather than left to the static entry. `namespaceSelector` is not evaluated (it would need namespace labels a tenant install cannot read). A flavor **without** a product label — Kueue's quickstart `default-flavor`, or any single-GPU-type cluster — is an **untyped grant**: it is *not* named after the flavor (a flavor called `default-flavor` or `h100-sxm` is not an accelerator type, and treating it as one would compete with every static key and zero them all). |
| `ClusterQueue` | cluster | `spec.resourceGroups[].flavors[].resources[].nominalQuota` for each GPU resource, per flavor, summed per product (typed flavors) or into the untyped grant (unlabelled flavors). Two grants of the **same** product add up; two products of one family (SXM and PCIe H100) stay apart until the static key says they are one. **`borrowingLimit`, `lendingLimit` and cohorts are not counted** — the bound wants the guaranteed figure. |
| `LocalQueue` | namespaced | `spec.clusterQueue` links a namespace to a ClusterQueue. A namespace is granted the caps of every ClusterQueue one of its LocalQueues points at; two LocalQueues on one ClusterQueue count it once. |

The cluster-scope figure is the sum over **all** ClusterQueues. Two things this
attribution does *not* do, deliberately:

- **A shared ClusterQueue is a per-namespace ceiling, not a partition.** Every
  namespace with a LocalQueue on it is bounded at the queue's full nominal
  quota, so several such namespaces may in sum still ask for more than the
  queue admits and Kueue holds the excess pending. The bound removes the
  single-namespace over-ask; a shared budget stays Kueue's to arbitrate. Give
  each tenant its own ClusterQueue (cohorts for borrowing) where that matters.
- **Untyped grants carry no vendor.** Within one ClusterQueue the untyped
  figures of different GPU resources (`nvidia.com/gpu`, `amd.com/gpu`) are not
  summed; the largest is taken, the loosest bound no single resource
  contradicts. Pin `kueue.resources` to one vendor when a mixed queue needs the
  distinction.
- **An untyped grant bounds each static type separately, not their sum.** A
  namespace that names two types with `-1` and holds an untyped grant of 8 may
  reach 8 of *each*. The limiter has no per-namespace total; if the total is
  what must hold, give the flavors product labels so the grant is typed.

The reads are
unstructured and go through the API server directly (no informer, no watch),
at most once per `refreshInterval`; the API version is whatever the cluster
serves (the fields used are identical in `v1beta1` and `v1beta2`), and the
project carries no Kueue module dependency.

#### Merge rules

Absence means different things at the two levels, and the rules follow from
that:

| Situation | Effective cap |
|---|---|
| Namespace listed in `namespaceQuotas` **and** governed by Kueue | per type, `min(static, kueue)` over the **union** of types; a static type Kueue does not name takes Kueue's **untyped** grant if there is one, else **0**; a Kueue type the static map does not name is **0** (both maps are closed allowlists) |
| Namespace governed by Kueue, unlisted, static `default` present | `min(default, kueue)`; the namespace becomes explicitly listed at that budget |
| Namespace governed by Kueue, unlisted, **no** static map at all | Kueue's **typed** caps, as is. An untyped grant alone names no type and so cannot open anything; the limiter logs it as unapplied — name the types in the entry (`default: {H100: -1}`) and Kueue supplies the figure |
| Namespace governed by Kueue, unlisted, strict static allowlist without `default` | **denied**, as before — Kueue cannot open a namespace the operator's list denies |
| Namespace **not** governed by Kueue | static treatment, unchanged |
| Namespace in `exclude` | bypasses both sources |
| Static `-1` (unlimited) | defers to Kueue's figure |

A namespace is *governed by Kueue* when one of its LocalQueues reaches a
ClusterQueue that **declares a GPU resource**. A LocalQueue pointing at a
CPU-only queue, or at a queue that does not exist, says nothing about GPUs and
leaves the namespace to the static entry — otherwise enabling the reader on a
cluster that uses Kueue for CPU batch jobs would zero every inference namespace.
A ClusterQueue that declares a GPU resource with `nominalQuota: 0` is a real
grant of nothing and denies.

Kueue never produces the reserved `default` key. Quotas Kueue holds for the
Kubernetes namespace literally named `default` are skipped, for the same reason
that namespace cannot be configured statically (see the reserved-key note above).

Type keys are matched across the two maps by accelerator identity
(`accelerator.SameName`: identical, equal ignoring case, or one being the
other's short name — two different full product names are never equated) and
the **static spelling is kept**. A short static key (`H100`) is bounded by the
**sum** of every grant in its family (PCIe + SXM); a full-product static key
(`NVIDIA-H100-PCIE-80GB`) only by its own grant, which is how a per-flavor
Kueue split (PCIe 4, SXM 0) is honoured. Every accelerator-keyed lookup in the
allocator resolves through the same identity (`accelerator.FindKey`), so usage
keyed by a raw product label, or by a lower-case GKE value, lands on the right
pool whichever spelling the effective map carries.

**The common Kueue install has no product labels on its flavors.** Then the
whole of Kueue's grant is untyped and the pattern is: name the types in the
static entry, let Kueue give the numbers:

```yaml
kueue: { enabled: true }
namespaceQuotas:
  default: { H100: -1 }      # every Kueue-governed namespace: H100 ≤ its Kueue nominal quota
```

#### Failure posture

`QuotaInventory.Refresh` reads the source and installs `static.BoundBy(snapshot)`
as the effective entry. It **never returns an error**: both engines treat a
failed `ComputeConstraints` as "no constraint" and fall back to their unlimited
path, so surfacing a Kueue outage as an error would *lift* the cap the operator
declared. Instead:

- with a previous snapshot, it stays in force and the error is logged with the
  snapshot's age (the reader hands back its last good read);
- with none — Kueue not installed, RBAC missing, first read failing — the
  **static entry alone** applies and the error is logged every cycle until a
  read succeeds. A Forbidden or a missing CRD is reported as such, never folded
  into "Kueue grants nothing".

Changes to the effective caps are logged once, at `Info`, with both the Kueue
figures and the merged result, so "what is bounding this namespace?" is
answerable from the controller log.

#### RBAC

The manager ClusterRole carries `get`/`list` on `clusterqueues`, `localqueues`
and `resourceflavors` in `kueue.x-k8s.io`. A namespace-scoped install lists
LocalQueues only in its own namespace (the tenant Role grants that) and gets the
two cluster-scoped kinds from `components/kueue-reader`, applied by the prereqs
phase like `node-reader`. A rule naming an API group the cluster does not serve
is inert, so the grant is harmless without Kueue.

### Validation

`QuotaLimiterEntries.Validate()` returns a fatal error for any of:

- Empty or duplicate `name`.
- `type` other than `"quota"`.
- `scope` outside `{cluster, namespace}`.
- `namespaceQuotas` or `exclude` set on a cluster-scoped entry.
- `quotas` set on a namespace-scoped entry.
- Negative quota values other than `-1`.
- A quota value exceeding `MaxQuotaValue` (1,048,576).
- Empty accelerator type or namespace key.
- In a `kueue:` block, an empty resource name or a `refreshInterval` that is not
  a positive duration; a `kueue:` block on a `gpu-inventory` entry.

It returns a non-fatal **warning** when a namespace appears in both `exclude`
and `namespaceQuotas`. `exclude` wins; the warning surfaces a likely
configuration mistake without rejecting the ConfigMap.

### Reload lifecycle

The quota configuration lives in the saturation-scaling ConfigMap and is applied
**live**. The controller watches that ConfigMap, and the saturation engine rebuilds
the GPU limiter at the top of the next optimization cycle whenever the effective
limiter mode or quota entries change (`Engine.refreshLimiter`, keyed by
`limiterSignature`). Editing the `limiters:` list therefore takes effect without a
controller restart — no `kubectl rollout restart` required.

## Pipeline integration

`QuotaInventory` implements the `Inventory` interface from
`internal/engines/allocation/limiter_interfaces.go` plus the
`NamespaceAwareInventory` extension. The lifecycle mirrors `TypeInventory`:

1. The controller constructs `QuotaInventory` from a validated config entry.
2. Before each cycle, `DefaultLimiter.ComputeConstraints` unconditionally calls
   `inventory.SetUsed(usedByType)` and, when the inventory satisfies
   `NamespaceAwareInventory`, additionally calls
   `SetUsedByNamespace(usedByNS)` via feature detection. Each
   `QuotaInventory` instance ignores the method irrelevant to its scope
   (`SetUsed` is a no-op on namespace-scoped instances; `SetUsedByNamespace`
   is a no-op on cluster-scoped instances), so callers do not branch on
   scope.
3. `ComputeConstraints` reads `GetResourcePools` (and, for namespace scope,
   `NamespaceResourcePools`) and returns them as the `ResourceConstraints` the
   optimizer allocates within.

The limiter supplies **constraints only** — it never modifies scaling decisions.
Enforcement happens in the optimizer, which is the single decision-maker (see
*V2 enforcement* below).

`Refresh` is a no-op for a purely static entry. For an entry with
`kueue.enabled` it re-reads Kueue (through `internal/kueue.Reader`, at most once
per `refreshInterval`) and installs the static entry bounded by the snapshot as
the effective config — see [Kueue as a quota source](#kueue-as-a-quota-source).

### A quota is charged for WVA's variants only

`QuotaInventory` declares `UsageBasis() == allocation.ManagedUsage`, so the usage it
is fed counts only what WVA's own variants hold — summed from the saturation
engine's population, not from the cluster-wide pod walk that feeds a physical
inventory. Both figures are assembled per cycle and routed per provider; see
[GPU capacity accounting](../concepts/gpu-capacity-accounting.md) for the two bases.

This is not a detail. A quota is an allowance granted to WVA and may only be spent
by WVA. Charged the physical figure, a namespace with a 4-GPU WVA quota sharing
space with an unrelated 4-GPU training job reads as fully spent while WVA has
placed nothing, and every scale-up is refused with the allowance nominally
untouched. The hardware being full is a separate statement, made by the physical
limiter.

The corollary is that a quota does **not** bound the cluster: it can hand out
capacity that non-WVA workloads have already taken. Deployments that need both
guarantees compose a physical limiter and a quota limiter, and each is then fed
the measure it is asking about.

### `GetResourcePools` representation

`GetResourcePools` returns `map[string]ResourcePool` keyed by accelerator type.
For the cluster scope it is one pool per configured type. For the namespace
scope it is the per-type **sum** across explicitly-listed (non-excluded,
non-`default`) namespaces — the aggregate cluster budget that the per-namespace
caps then partition. The per-`(namespace, type)` breakdown is exposed separately
via `NamespaceResourcePools` (see *V2 enforcement* below).

Unlimited (`-1`) entries are handled by scope. At **cluster** scope they are
emitted as a sentinel `ResourcePool{Limit == -1}` (like the namespace branch), so
the V2 optimizer can tell "unlimited" (allocate up to demand) apart from a type
with no configured quota (absent → deny). At **namespace** scope an unlimited
`(namespace, type)` is likewise emitted as a sentinel in `NamespaceResourcePools`.
Either way `Limit = 0` stays unambiguous: a pool with `Limit = 0` always means a
real **deny** cap (a configured quota of `0`), never "unlimited". (`TotalLimit`,
`TotalAvailable`, and `Remaining` still exclude unlimited entries from their
totals.)

## V2 enforcement (optimizer path)

The V2 (token-based saturation) analyzer — the default — drives scaling through
the optimizer's `ResourceConstraints`, built from
`DefaultLimiter.ComputeConstraints`. Namespace quota is enforced here with
closed-allowlist semantics:

- **Per-type / cluster** caps come from `GetResourcePools` (`ResourceConstraints.Pools`).
- **Per-namespace** caps come from `NamespaceResourcePools`
  (`ResourceConstraints.NamespacePools`, keyed namespace → type). Each non-excluded
  active namespace is a **closed allowlist**: the optimizer may scale a model
  onto a type only if that namespace lists it. For a listed type, the bound is
  `min(per-type budget, that namespace's cap)`, decremented as it allocates.
- A type the namespace does **not** list is **denied** — it does *not* fall
  through to the cluster aggregate, so one namespace can never draw on another's
  quota (no cross-namespace leak). An **unlimited** (`-1`) per-namespace entry is
  carried as a sentinel and bounds the model only by the cluster budget for that
  type (or is unbounded if the cluster does not cap it). An **excluded**
  namespace is omitted entirely, so it is "open" and bound only by the cluster
  constraint.
- **Multi-entry (composite)** quota configs are fully consulted: the engine
  computes constraints from *each* constituent of a `CompositeLimiter`.

Active namespaces are taken from the optimizer's request set, so a namespace
carrying a quota but currently running zero replicas is still constrained (its
`default` fall-through and `exclude` rules are applied via `QuotaForNamespace`).
A namespace with neither an explicit quota nor a `default` fall-through is a
real **deny-all**: it is present in `NamespacePools` with no listed types, and
the optimizer allocates it nothing.

Remaining boundaries:

- Composing a **physical-inventory** limiter with a quota limiter as
  `min(physical, quota)` within a single constraint set is tracked by the
  limiter chain (sub-issue #1003).
- A **purely unlimited** namespace-quota config with no finite cluster cap for
  the relevant type does not scale under the V2 `GreedyByScore` optimizer (its
  fair-share loop stops when the finite cluster aggregate is zero). This is a
  benign under-provision, not an isolation breach; configure a finite cluster or
  per-namespace cap for any type you expect to scale under V2.
- An **unlimited (`-1`) cluster-scope** quota scales up: `GetResourcePools`
  emits it as a sentinel pool that `mergeConstraints` carries through as an
  unbounded budget, so the type allocates up to demand. (This differs from the
  namespace-scope purely-unlimited case above, whose cluster aggregate is
  derived from the finite namespace caps only.)

### Fair-share interaction

Quotas are **hard ceilings applied on top of** the optimizer's fair-share loop,
not inputs to it. Two properties follow from that:

- The fair-share mean each round is the **average of the active models' remaining
  fair-share metric** (priority × score × unmet demand — see the worked-example
  caveat below) — it is **not** `cluster GPUs / number of active models`, and
  **not** quota-weighted. The cluster GPU total is only the loop's stop condition
  (fair-sharing halts once the aggregate budget is exhausted). Quotas only clamp
  each model after the mean is computed.
- Fairness is **per-model**, not per-namespace. Two models in one namespace each
  get their own fair-share slot; the namespace quota bounds each of them (and
  their running sum) but does not pool one model's allowance for the other.

> **Ceilings, not reservations.** A finite per-namespace cap guarantees a tenant
> will never *exceed* it — it does **not** guarantee the tenant can *reach* it.
> Under V2 the cluster aggregate is the **sum of the finite namespace caps**, and
> an **unlimited (`-1`)** or **excluded** namespace competing for the same
> accelerator type draws from that shared aggregate without contributing to it,
> so it can consume budget a finite-capped peer would otherwise have used (no
> isolation breach — nobody exceeds their own authorization — but a real fairness
> footgun). If you need a tenant's cap to behave like a floor, avoid mixing
> unlimited/excluded namespaces on the same type, and give every type a finite
> cluster-scope cap.

When a model is capped below its fair-share slot it takes only up to its quota;
the unused remainder is **released to subsequent rounds**, where the
still-hungry models split it — it is not handed to other models within the same
round.

Worked example — cluster of 8 GPUs, three models:

| Model | Wants | Quota |
|-------|-------|-------|
| M1    | 3     | 2     |
| M2    | 4     | 4     |
| M3    | 4     | 4     |

- **Round 1:** the mean is the average of the models' remaining demand,
  ≈ (3 + 4 + 4) / 3 ≈ 3.67. M1 is capped at its quota of 2 (below its demand of 3),
  so part of its fair-share slot goes unused; M2 and M3 pull toward the mean under
  their own quotas.
- **Round 2:** with M1 satisfied at 2, the remaining cluster budget (8 − 2 = 6) is
  split between the still-hungry M2 and M3.

Final allocation: **M1 = 2, M2 ≈ 3, M3 ≈ 3** (total 8). The outcome respects every
quota and exhausts the cluster budget; the multi-round path is why a
quota-constrained model's slack flows to later rounds rather than to its
round-mates. (The exact per-round means come from the fair-share metric —
priority × score × demand — so treat the numbers here as an illustration of the
*path*, not an exact trace.)

## Interaction with `TypeInventory`

`QuotaInventory` is composed with `TypeInventory` by the limiter chain. The
intended model:

- `TypeInventory` knows what is **physically available** (nodes × GPUs per
  node).
- `QuotaInventory` knows what is **administratively allowed**.
- The chain takes the minimum: a decision must fit under both.

In the initial implementation the two are **mutually exclusive**, not composed:
the `limiters:` list selects either physical inventory **or** quota (a `quota`
entry wins over a `gpu-inventory` entry). There is no `min(physical, quota)` chain
yet (tracked in sub-issue #1003), so quota mode does **not** enforce physical
bounds at all — a deployment in quota mode will allocate beyond the cluster's
actual capacity if the quota permits (the surplus simply yields `Pending` pods,
not an isolation breach). Set quota caps at or below real capacity until
composition with `TypeInventory` lands.

## Selection & lifecycle

The controller selects between physical-inventory and quota enforcement from the
`limiters:` list on the saturation-scaling ConfigMap's **`default`** entry — the
sole source, honored only at that cluster-scope entry (like `enableRescale`):

| `limiters:` on `default` | Behavior |
|--------------------------|----------|
| absent, or a `{type: gpu-inventory}` entry | `TypeInventory` discovers physical GPUs via the GPU operator and caps decisions at `min(physical, requested)`. This is the default. |
| a `{type: quota, ...}` entry | Operator-declared quotas from the inline entries. A `quota` entry wins over any `gpu-inventory` entry in the same list. The two are mutually exclusive in the initial implementation (composing with physical inventory as `min(physical, quota)` is tracked in [#1003](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1003)) — quota mode does **not** consult physical inventory. |

The selection is exposed on `*config.Config` as `EffectiveLimiterMode()` and
`EffectiveQuotaEntries()`, both reading the live ConfigMap. The saturation engine
rebuilds the limiter when they change (`Engine.refreshLimiter`), so edits apply
without a restart. The factory in
`internal/engines/allocation/limiter_factory.go` translates the selection into a
concrete `allocation.Limiter`:

- **Inventory** — `DefaultLimiter` wrapping `TypeInventoryWithUsage`.
- **Single quota entry** — `DefaultLimiter` wrapping one `QuotaInventory`.
- **Multiple quota entries** (e.g., cluster + namespace combined) —
  `CompositeLimiter` wrapping one `DefaultLimiter` per entry. Each constituent
  applies its cap in declaration order against the shared decisions slice,
  so the most-restrictive bound wins. This is intentionally simple — full
  chain composition with `min(physical, quota)` is sub-issue #1003.

### Per-namespace usage feeding

`DefaultLimiter.ComputeConstraints` feature-detects `NamespaceAwareInventory` and
calls `SetUsedByNamespace(usedByNS)` in addition to the always-safe `SetUsed`.
For a quota the caller supplies the managed view, so `usedByNS` is
`CurrentReplicas * GPUsPerReplica` summed by `(Namespace, AcceleratorName)` over
the cycle's population (`computeCurrentGPUUsageByNamespace`). No additional
discovery or API calls are needed.

Every namespace being decided about must be **present** in that map, with an
empty inner map when it holds nothing: `NamespaceResourcePools` materialises caps
only for the namespaces it is given, so an absent one silently loses its quota and
is judged against the cluster aggregate instead. Both callers guarantee this — the
saturation engine for every namespace it is optimizing, the scale-from-zero engine
for the namespace it is placing into.

### Example configuration

Inventory mode is the default — no `limiters:` list is required. To select quota
mode, add a `quota` entry to the saturation `default` entry's `limiters:` list:

```yaml
# saturation-scaling ConfigMap, data."default":
default: |
  analyzers:
    - type: saturation
  limiters:                    # declaring it is what enforces it
    - type: quota
      name: cluster-h100
      scope: cluster
      quotas: { H100: 32 }
```

Inline quota entries are validated at ConfigMap parse time
(`SaturationScalingConfig.validateLimiters`): each `quota` entry is checked against
the `QuotaLimiterEntries` schema (name uniqueness, scope, per-type ranges), and a
`gpu-inventory` entry must carry no quota fields. Invalid entries are skipped with
an error log/metric, exactly like any other invalid saturation entry.

## Resource access in quota mode

Quota mode keeps the **limiter path** independent of physical node discovery.
When the effective limiter mode is quota:

- The limiter, inventory, allocator, and factory paths do not call
  `discovery.K8sWithGpuOperator.Discover` / `DiscoverUsage` /
  `DiscoverNodes`.
- `QuotaInventory.Refresh` reads no nodes; its usage comes from the
  saturation engine's population sum (`ManagedUsage`), which needs no cluster
  discovery of its own. (With `kueue.enabled` it lists Kueue objects, which
  is Kueue API traffic, not Node API traffic.)
- `collector.CollectInventoryK8S` (called from the saturation engine's
  per-cycle `optimize` when `WVA_LIMITED_MODE=true`) is **also** gated on
  the effective limiter mode via `shouldCollectClusterInventory` — it only runs
  when `EffectiveLimiterMode() == inventory`. This keeps the "no Node API access
  in quota mode" contract intact even if an operator combines quota mode with
  `WVA_LIMITED_MODE=true`. When that combination is detected at startup,
  `main.go` emits an informational log so the operator sees that their inventory
  logging is intentionally suppressed.

Combination matrix — **limiter-path** Node API access (limiter mode is the
effective mode from the ConfigMap):

| Effective limiter mode | `WVA_LIMITED_MODE` | Node API access from the limiter? |
|------------------------|--------------------|------------------|
| `inventory` (default) | `false` (default) | Yes, via the limiter's `Refresh` cycle. |
| `inventory` | `true` | Yes, via both the limiter and `CollectInventoryK8S`. |
| `quota` | `false` | **No.** |
| `quota` | `true` | **No.** `CollectInventoryK8S` is skipped; a startup log notes the suppression. |

The cluster GPU-usage observer honours this too. `internal/gpuusage.Refresher`
publishes the physical view, which a quota never consults, so its timer runs only
when a physical limiter is declared (`Refresher.Periodic`, wired in
`cmd/main.go`) — quota mode and no-limiter mode both fail that test.

So a quota-only deployment takes no periodic observation and lists no nodes or
pods for one. The gate is re-evaluated per tick rather than latched at startup,
because limiter mode is live-reloadable.

> **One caveat on "no Node API access".** The gate covers the *timer*, not
> on-demand reads. The scale-from-zero engine calls `EnsureFresh` at the moment it
> decides a wake — that is deliberately ungated, and it is why its capacity check
> keeps working with the timer off. It only asks when a provider that reads the
> physical view is configured, so quota mode still never triggers it. The
> observation walks the pod informer's cache rather than calling the API, but the
> RBAC is held either way.

## Future work

- **Limiter chain composition** (sub-issue #1003) — replace the simple
  `CompositeLimiter` with a smarter chain that:
  - Composes physical (`TypeInventory`) and quota bounds as
    `min(physical, quota)` instead of the current mutually-exclusive choice.
  - Owns DecisionStep ordering / `LimitedBy` selection when multiple caps
    bind the same decision.
- **Reservation-style limiters** — the `type: "quota"` discriminator leaves
  room for additional limiter types (e.g., `reservation`, `priority`) under
  the same ConfigMap schema.
