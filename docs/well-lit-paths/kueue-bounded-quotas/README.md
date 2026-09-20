# Bound tenants by the quotas Kueue already holds

> **Experimental.** The merge rules, the reader and the installer knob are unit
> tested, and an end-to-end suite proves the read-and-merge chain against a real
> API server, up to the effective entry the limiter logs — but that suite
> installs only Kueue's **CRDs**, not Kueue. **No run has yet been made against
> a live Kueue controller**, so the two things only a live one shows — a queue
> going inactive under it, and finalizers on its objects — are covered by tests
> that imitate them. That is the short leg. Two defaults may still move: the
> 30s `refreshInterval`, and how an untyped grant is applied (to each named type
> separately, and the largest across vendors rather than the sum).

If Kueue is already the place where GPU quotas are decided, a second copy of
every number in WVA's ConfigMap is a second thing to keep right. This path has
WVA read Kueue's ClusterQueues and enforce, per namespace and accelerator type,
**the smaller of Kueue's grant and the static cap** — so the autoscaler does not
ask KEDA for a replica that Kueue would then hold pending.

It is an extension of [capping what each tenant may take](../tenant-gpu-quotas/):
everything there about scope, the reserved `default` key and what a quota does
*not* bound still applies. Read that page first; this one adds one block to its
entry.

**Use it when** the namespaces WVA scales have LocalQueues, their GPU grants are
ClusterQueue nominal quotas, and you want WVA to follow those numbers as they
change rather than mirror them by hand.

**Do not use it where several namespaces share one ClusterQueue and the shared
total is what must hold**: each such namespace is capped at the queue's full
grant (the second surprise below). And read the third surprise before choosing
`-1` as the static cap: it decides what happens when Kueue cannot be read.

## What it needs

- Everything [tenant-gpu-quotas](../tenant-gpu-quotas/) needs.
- Kueue's `ClusterQueue`, `LocalQueue` and `ResourceFlavor` kinds served by the
  cluster. WVA lists them and never talks to Kueue's controller, so the CRDs
  alone are enough for the reader to work — but a real installation is what
  makes the grants mean anything.
- RBAC to read them. A **cluster-scoped** install has it in the manager
  ClusterRole. A **namespace-scoped** install reads LocalQueues in the namespace
  it manages through its Role, and needs the two cluster-scoped kinds from the
  `kueue-reader` prerequisite, which the admin phase creates
  (`make setup-prereqs SCOPE=namespace ENVIRONMENT=kubernetes`, the same run
  that creates `node-reader`). An install that predates the prerequisite still
  upgrades; the reader logs `external quota source unreadable` each cycle if
  the block is turned on without it.

## Declaring it

The installer writes it. Add `WVA_QUOTA_KUEUE=true` to the same `WVA_LIMITER=quota`
install the tenant-quotas path uses, and put the accelerator types in
`WVA_QUOTAS`:

```bash
# one install's own policy
WVA_LIMITER=quota WVA_QUOTAS='H100=-1' WVA_QUOTA_KUEUE=true make deploy-wva ...

# the cluster policy every controller reads. With WVA_QUOTA_KUEUE=true this also
# grants each target controller the cluster-scoped half of the Kueue read
# (ClusterQueues, ResourceFlavors), as it grants the node read for gpu-inventory.
WVA_LIMITER_TYPE=quota WVA_QUOTAS='H100=-1' WVA_QUOTA_KUEUE=true make enable-physical-limiter
```

The LocalQueue read is in the tenant Role, so a namespace-scoped controller
installed before this feature existed also needs its owner to run
`make deploy-wva` once; until then it logs `external quota source unreadable`
and runs on the static entry.

That renders as one more block on the quota entry (comments added here; the
installer writes the two lines under `kueue:`):

```yaml
limiters:
  - name: install-quota
    type: quota
    scope: namespace
    kueue:
      enabled: true
    namespaceQuotas:
      <managed namespace>:
        H100: -1                    # the TYPE is yours; the NUMBER is Kueue's
```

Hand-write the entry when you want several namespaces named, or a finite static
cap for some of them: wherever both sides name a type, the smaller figure wins.

A namespace is **governed** by Kueue when one of its LocalQueues reaches a
ClusterQueue that declares a GPU resource; it then gets that queue's grant. A
namespace with no such LocalQueue is **not governed**, and the static entry
alone decides for it. What each Kueue object becomes, and what is not read:
[the reference](../../reference/quota-limiter.md#what-is-read).

## Three things that surprise people

**Kueue's usual flavors name no accelerator type, so Kueue alone cannot open one.**
The quickstart's `default-flavor` has no node labels. Its grant is *untyped*: it
bounds every type the static entry names and, on its own, names none. Turn the
block on with **no static map** and untyped flavors, and the entry — now an
empty map — **denies every type**; the limiter says so once in its log
(`grants GPUs of no named accelerator type`, with the fix in the message). Hence
the recipe above: name the types, let Kueue give the numbers (`H100: -1`). Give
the flavors a product label (`nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3`)
and the grant becomes typed — and then a per-flavor split (PCIe 4, SXM 0) is
honoured exactly. The cost of the untyped shape: a namespace that names two
types may reach the untyped grant on *each*; the limiter has no per-namespace
total.

**A shared ClusterQueue is a ceiling per namespace, not a partition.** Each
namespace with a LocalQueue on it is capped at the queue's full grant, so three
of them may in sum still ask for three times what the queue admits — Kueue holds
the excess pending, as before. Give each tenant its own ClusterQueue, with a
cohort for borrowing, where the shared total matters.

**A Kueue outage keeps the last grant; a Kueue that was never read leaves only
the static entry.** A failed read keeps the last good snapshot in force, and the
log says how old it is. Before the *first* successful read — RBAC missing, CRDs
absent, a fresh controller — the static entry alone applies, and with `-1` that
is **no cap at all**. If a controller must stay bounded when Kueue cannot be
read, put a finite number in the static entry; Kueue then only tightens it. A
missing CRD or a Forbidden is reported as such every cycle, never folded into
"Kueue grants nothing"; both engines treat a limiter error as "no constraint",
so `Refresh` never surfaces one and the reader's errors go to the log.

## Verifying it

```bash
# the effective entry, logged once each time Kueue's grant changes
kubectl logs -n <wva-namespace> deploy/wva-controller-manager \
  | grep -E 'quota entry bounded by external caps|external quota source'
```

The `bounded by external caps` line carries four maps: Kueue's grant per
namespace and for the cluster, and the **effective** namespace and cluster caps
after the merge — the numbers WVA enforces. `external quota source unreadable`
names an RBAC or CRD problem and says whether a stale snapshot or the static
entry is in force. `grants GPUs of no named accelerator type` is the
untyped-flavor case above.

**The reader's state is visible only here.** No metric or condition reports a
Forbidden or a missing CRD; the tenant path's `wva_model_scaling_blocked` still
shows a model held at its cap, but not why the cap is what it is.

A change in Kueue appears after at most `refreshInterval` plus one
[optimize cycle](../../reference/cycle-log.md). The e2e runs with
`refreshInterval: 5s` and allows three minutes; it asserts the transition, not
its latency.

## What it costs

Three list calls against the API server per `refreshInterval` (30s by default),
shared by both engines — no informer, no watch. And the lag above. No benchmark
scenario backs this page: a bound changes what the optimizer may order, not how
it scales.

## How it is tested

- Unit: `internal/config/quota_limiter_kueue_test.go` (every merge rule,
  untyped grants, the per-flavor split, the reserved key and exclusions),
  `internal/kueue/reader_test.go` (attribution, inactive queues, both served
  API versions, the refresh cache, a caller's cancelled context),
  `internal/engines/allocation/quota_inventory_source_test.go` (the effective
  entry reaching the optimizer's constraints, stale snapshots, one shared
  reader), `internal/accelerator/identity_test.go` (how a full product label
  meets a short static key). `hack/check-limiter-declaration.sh` pins what
  `WVA_QUOTA_KUEUE` emits.
- End-to-end: `test/e2e/kueue_quota_test.go` (label `full`, `kueue-quota`) on
  kind — Kueue grants one GPU against a static cap of three; the limiter's
  effective entry reads one, then three again after the LocalQueue is deleted.
  It runs with Kueue's CRDs only (`deploy/ci-pr-checks/install-kueue-crds.sh`,
  wired into `test-e2e-full-with-setup` on kind); it fails, not skips, when
  they are absent there, and skips with a reason on a cluster that does not
  serve them.
- **Not covered:** a live Kueue controller (inactive queues, finalizers,
  `stopPolicy` transitions are imitated by fixtures); enforcement past the
  effective entry, which is the same optimizer path
  [tenant-gpu-quotas](../tenant-gpu-quotas/) relies on, with the same gap; and
  the reader's state, which has no metric.

## Tuning it

Every field, the full merge table and what is not read:
[the quota limiter reference, "Kueue as a quota source"](../../reference/quota-limiter.md#kueue-as-a-quota-source)
— `resources` narrows which extended resources count as GPUs, `refreshInterval`
how often Kueue is re-read. The installer variables:
[configuration](../../reference/configuration.md). Why a quota counts only
WVA's own consumption:
[GPU capacity accounting](../../concepts/gpu-capacity-accounting.md).
