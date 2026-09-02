# GPU Capacity Accounting

How WVA decides whether GPUs are available, what that number actually means
today, and the ways it can still over-state free capacity.

Two consumers read these budgets:

- the **GPU-aware optimizer** (`GreedyByScoreOptimizer`), active when the
  saturation config declares any `limiters:` entry — off in the shipped config,
  see the caveat below; and
- the **scale-from-zero placement check**, which refuses to wake a variant onto
  an accelerator with no room (see
  [`scaleFromZero`](../reference/scaling-policy.md#scalefromzero)).

Both over-allocate when the budget over-states availability: pods land in
`Pending`, and for scale-from-zero the queued request that triggered the wake
times out anyway — a wake that looks like progress and delivers none.

> **Declaring a limiter requires resolvable accelerators.** The GPU-aware
> optimizer allocates out of per-accelerator pools, so a variant whose accelerator
> it cannot resolve (no GPU `nodeSelector`/`nodeAffinity` on the workload, and no
> running pods to observe it from) is charged to no pool, receives
> no budget, and **never scales up** — silently, since nothing errors. That is why
> the shipped config declares no limiter: enabling it by default would freeze
> exactly the workloads that are least carefully configured. Check for
> `AcceleratorNotResolved` warning events across the fleet before declaring one.

## How a budget is computed

`DefaultLimiter.ComputeConstraints` builds a `ResourcePool` per accelerator type:

```text
Limit = sum of node Allocatable for that GPU type, across nodes  (node discovery)
Used  = GPUs in use, on the basis this provider asked for      (caller-supplied)
Available() = max(0, Limit - Used)
```

## `Used` depends on the question the provider is answering

There are two measures of "GPUs in use", they are not interchangeable, and each
provider declares which one it needs via `allocation.UsageBasis`. The caller
(`gpuUsageViews` in the saturation engine, `Engine.gpuUsageViews` in
scale-from-zero) hands each provider the matching view.

| basis | counts | produced by | consumed by |
|---|---|---|---|
| `PhysicalUsage` | every GPU held on the cluster's GPU nodes, whoever holds it | `internal/gpuusage`, on its own 15s ticker | physical inventory (`TypeInventory`) |
| `ManagedUsage` | only what WVA's own variants hold | the saturation engine's population sum (`gpuUsageByType`) | quota inventory (`QuotaInventory`) |

The split exists because the two answer different questions:

- a **physical inventory** asks *"will the scheduler find a free device?"*. Every
  GPU-requesting pod counts, whoever owns it — a training job in another
  namespace is as real an obstacle as one of WVA's own replicas. This view is
  attributed by the **node** a pod runs on, so nothing is unattributable and it
  does not depend on WVA having discovered the workload;
- a **quota** asks *"how much of the operator's declared allowance has WVA
  consumed?"*. A quota governs WVA-managed variants and nothing else. Charged the
  physical figure, a namespace with a 4-GPU WVA quota and an unrelated 4-GPU
  training job reads as fully spent while WVA has placed nothing, and every
  scale-up is refused. The managed view sums `CurrentReplicas × GPUsPerReplica`
  over the variants WVA manages, keyed by each variant's resolved accelerator.

Physical is the default: anything that does not declare a basis gets it. That is
the safe direction — a physical figure over-states consumption for a quota, which
errs toward refusing, rather than under-stating what the hardware holds, which
would place a variant onto a device that is already taken.

**The physical observation is only taken when something reads it.**
`internal/gpuusage.Refresher` runs its timer only while a physical limiter is
declared — the saturation engine is the only consumer that reads the published
snapshot as-is. The scale-from-zero engine
instead calls `EnsureFresh` at the moment it decides a wake, so its capacity check
is unaffected by the timer being off, and it only asks when a physical provider
exists. A quota-only deployment therefore never walks the cluster for a number
nothing reads. See [quota limiter](../reference/quota-limiter.md#resource-access-in-quota-mode).

**A missing view is not zero.** Absent means "unknown", which both engines treat
as permissive; an empty map is a confident claim that nothing is in use, and a
provider handed one reports its entire capacity free. Callers check
`GPUUsageViews.MissingBasis` before computing any constraint, and fall back to
the unlimited optimizer (saturation) or an unchecked wake (scale-from-zero) when
a needed view has not been observed. Only the bases some provider actually asks
for are gathered, so a quota-only deployment is never held up waiting for a
physical observation it does not consult, and vice versa.

## Key reconciliation: `Used` vs `Limit` (fixed)

The two sides of a pool are written in different vocabularies, and getting this
wrong made the budgets inert rather than merely inaccurate.

- **Limits** are keyed by *normalized short names*: `TypeInventory.Refresh` runs
  each discovered node product label through `NormalizeAcceleratorName`, so
  `NVIDIA-A100-PCIE-80GB` becomes `A100`.
- **Usage** arrives keyed by whatever the workload declares. `gpuUsageByType`
  keys by `VariantMetadata.AcceleratorName`, i.e. the nodeSelector/label value
  verbatim — for the common deployment shape, the raw product label.

Unreconciled, a product-label usage key never matched a limit key,
`GetResourcePools` reported `Used = 0` for it, and every pool claimed its full
complement was free however much was running. Both consumers therefore saw an
empty cluster.

`TypeInventory.SetUsed` now reconciles incoming keys onto the discovered limit
keys, trying the declared name first and only then its normalization. Order
matters: `NormalizeAcceleratorName` falls back to "the segment after the first
hyphen" for names with no vendor prefix it recognises, so an already-short
`Gaudi-2` would become `2` and match nothing if normalized unconditionally. The
demand side does the same at lookup time (`pipeline.resolvePoolKey`), so both
spellings work on both sides.

Pinned by `TestUsageIsReconciledOntoPoolKeys` and `TestPoolKeysAreShortNames`.

> **This changed allocation behaviour.** Before it, `Used` was effectively always
> 0 in nodeSelector deployments, so the GPU-aware optimizer believed the cluster
> was empty and allocated against the full installed capacity. It now sees real
> usage and will allocate less. That is the correction, but it is a behavioural
> change for any existing deployment that declares a limiter.

## Gap 1: usage on an unresolved accelerator is still unattributable

A variant that constrains no accelerator and has no running pods to observe
resolves to
`constants.DefaultAcceleratorName` — the internal `"unknown"` placeholder. That
name matches no discovered type, so reconciliation drops it: its GPUs are charged
to no pool.

The per-type budgets therefore still over-state free capacity by however many
GPUs unresolved variants hold. With 8 H100s, 2 held by a resolved variant and 4
by unresolved ones, the pool reports `Available() == 6` while only 2 are
genuinely free.

What changed is that the inconsistency is gone: dropped usage is excluded from
`TotalUsed` as well, so the aggregate and the sum of the pools now agree.
Previously `SetUsed` summed every key while the pools iterated only discovered
types, leaving the two views contradicting each other in opposite directions.

Pinned by `internal/engines/allocation/unresolved_accelerator_usage_test.go`.

**Why it is not fixed further.** Usage that cannot be attributed to a type cannot
be charged to a pool, and charging it to every candidate type would deny
legitimate scale-up on a guess. The durable fix is making the accelerator
resolvable — set a `nodeSelector`/`nodeAffinity` GPU key on the workload. Once it has
running pods WVA also resolves it by observation, from the nodes the scheduler
actually placed them on. WVA emits an
`AcceleratorNotResolved` warning event per affected variant.

## Gap 2 (mostly closed): `Limit` is installed GPUs, not available GPUs

`Limit` sums each node's **Allocatable** for the GPU resource. Allocatable is the
node's total; it does not subtract what running pods have requested, and it does
not go to zero when a node is cordoned or `NotReady`.

The `Used` side of that gap is **closed for physical providers**.
`internal/gpuusage` observes the cluster directly — walking the pods that occupy
GPU nodes and attributing each to the node it is scheduled to — so a physical
pool now nets out:

- workloads in other namespaces, or not managed by WVA at all;
- system/DaemonSet pods holding GPUs;
- pods scheduled but not yet running (the scheduler has already committed those
  devices; treating them as free would let two wakes land on the same one).

Gap 1 does not apply to that view either: attribution is by node, so there is no
unresolved bucket to leak through. Budgets are correspondingly **tighter** than
they were before this landed, which is the correction — they were over-stated.

Two pieces remain open:

1. **Unschedulable nodes still contribute their GPUs to `Limit`.** Allocatable
   alone does not exclude cordoned or `NotReady` nodes, so drained capacity reads
   as available.
2. **Quota providers still use the population sum**, and deliberately so — see
   the usage-basis section above. Gap 1 therefore still applies to them: a variant
   with an unresolved accelerator is charged to no pool, so a quota reports more
   of its allowance free than it has. `wva_unattributed_gpus` reports the amount.

## Gap 3: an FMA warm pool holds GPUs that no pod requests

Both gaps above are about GPUs this accounting charges to the wrong place. This
one is about GPUs it cannot see at all, and unlike the others it cannot be closed
from inside WVA.

Fast Model Actuation splits a server across a requester pod, which reserves the
accelerator, and a launcher pod, which runs the engine on it. Launchers request
**no** `nvidia.com/gpu` — deliberately, since requesting on both halves would
double-book N launchers plus N requesters on an N-GPU node. While a pair is
bound the accounting is exactly right: the GPU is charged to the requester, which
is the scale target this file is about.

The gap opens on unbind. The launcher keeps its vLLM instance resident — that is
what makes the next bind take seconds — and goes on occupying a physical GPU
charged to nobody. Measured on pokprod001 with every requester at `replicas: 0`:

```text
GPU requests charged in the namespace : 1
launcher pods running a vLLM instance : 9, on 9 distinct GPU UUIDs
```

So `Used` under-states by the size of the warm pool, and `Limit − Used` over-states
free capacity by the same amount — the same direction as Gap 1, and for a
different reason.

**Why it cannot be closed here.** The obvious fix is to read the GPUs off the
pods: FMA does record them, in `dual-pods.llm-d.ai/vllm-config` and
`/accelerators`. But those annotations, along with `/server-port` and
`/instance-id`, exist **only while the pair is bound**. An orphaned launcher still
running an instance carries neither, so the API server holds no record of what it
is using. Only the launcher's own HTTP API knows, and the collector does not call
workload APIs.

Two things follow for anyone reading these numbers:

- In an FMA namespace, treat the budget as a **lower bound on usage** and an
  **upper bound on what is free**. `deploy/lib` warns at plan time when it finds
  launcher pods.
- A `ResourceQuota` on `requests.nvidia.com/gpu` has the same blind spot, so it is
  not a backstop here — which matters, because the limiter is advisory precisely
  on the grounds that ResourceQuota is the real boundary.

Closing it needs FMA to make the occupancy visible; that is item 1 in
[the FMA post-mortem](../proposals/fma-post-mortem.md).
Operator-facing guidance is in
[the GPU limiter guide](../reference/gpu-limiter.md), "FMA namespaces".

## Testing the placement check end-to-end

The scale-from-zero placement check has unit coverage on every seam, and its
**deny** branch is exercised end-to-end by
`test/e2e/scale_from_zero_capacity_test.go`. Three conditions must hold
simultaneously for a denial to be reachable at all:

1. the variant's accelerator resolves — a `nodeSelector` on a GPU product label,
   (observation covers a running variant, but a parked one has no pods). Without it the
   candidate contributes no demand and `FitsGPUBudget` returns true having
   evaluated nothing;
2. a GPU-usage snapshot exists for the basis the configured provider needs. For
   the default physical provider this no longer requires an active WVA variant —
   `internal/gpuusage` observes on its own ticker from process start, which is
   what makes the check meaningful for a fleet parked at zero. A quota provider
   does still need a completed saturation cycle, which publishes the managed view
   (including as an explicit zero when nothing is active); and
3. that usage is attributed to the same pool key the limits use (see the key
   reconciliation above).

Condition 2 interacts awkwardly with the trigger. Scale-from-zero fires on EPP
flow-control queueing, and requests only queue when the pool has **no ready
endpoints** — so the running variant needed for condition 2 must not sit in the
pool under test, or it serves the requests itself. The one-model-one-pool
contract makes that natural: the occupier serves its own model, so it belongs to
its own pool (`fixtures.WithPoolGuide`).

The suite went green once `internal/gpuusage` became an independent producer.
Before that it could not pass: the parked fleet meant the saturation engine
returned without publishing, no snapshot existed when the wake was decided, and
"unknown" is permissive by design — so the wake the suite requires to be refused
was allowed, with nothing in the logs saying the check had been skipped.

## Assumption: one model, one InferencePool

The scale-from-zero wake path assumes a model's variants all sit behind a single
EPP. `buildCandidates` resolves one pool for the model group, and the group's
pending-request verdict is read from that pool's flow-control queue.

That matches the project's contract, and it is what keeps the wake cheap: one
scrape per model per tick, rather than one per variant.

**Per-role pools are a possible future shape and are not implemented.** If a
model's decode sat behind one EPP and its prefill behind another, the current
code would read both variants' demand from whichever pool resolved first — so a
decode variant could be judged by the prefill queue, and its activation logged
against an EPP that was never consulted. That is silently wrong rather than
imprecise, which is why the assumption is stated here and checked in code.

Supporting per-role pools would mean:

1. resolving the pool **per candidate** rather than per model group;
2. reading demand from each distinct pool in turn, attributing the verdict to the
   pool it came from; and
3. keeping selection across the whole model, so a P/D pair is still chosen as one
   set against one joint GPU budget even when its halves are behind different
   pools.

Until then, a model that resolves to more than one pool is logged
("Model resolves to more than one InferencePool"), and only the first is used.
