# Bounding scaling: the GPU limiter

WVA scales without a GPU budget unless you give it one. This is how, and what has
to be true first.

> Part of the [WVA deployment guide](../../deploy/).

## Turning it on

```bash
make deploy-wva-on-k8s WVA_LIMITER=gpu-inventory   # bound by GPUs actually free
make deploy-wva-on-k8s WVA_LIMITER=quota           # bound by declared caps
```

or later, by adding a `limiters:` entry to the `default` entry of the
scaling-policy ConfigMap — applied live, no restart. **Read the next section
first.**

> **Declare one kind, not both.** `limiters:` is a list, and it reads like a set
> of bounds that all apply. It is not. One limiter is built, and a quota entry
> wins: declaring `quota` alongside `gpu-inventory` gives you the declared caps
> and **no physical limiter at all**, so nothing checks whether the GPUs exist.
> That is the dangerous direction — the config says "bounded by real GPUs too"
> and a scale-from-zero wake can still be placed onto a full accelerator.
>
> WVA says so rather than dropping it silently: the ConfigMap parse and the
> startup log both name what is not enforced (`notEnforced`). Bounding by
> `min(physical, quota)` is [issue #1003](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1003).

## Who is allowed to change the bound

By default the limiter is read from the scaling-policy ConfigMap in the
controller's own namespace. For a **cluster-scoped** install that namespace belongs
to a cluster admin, so the bound and its subject already have different owners, and
there is nothing more to arrange.

It is the **namespace-scoped** install that needs care. There the controller runs
in the namespace it manages, so whoever administers that namespace administers the
controller — its Deployment, its args, its env, its ServiceAccount. Nothing carried
on the controller can bound the person who can edit the controller. A quota its
subject can edit is not a quota.

So WVA does not accept its limits from anything that person can write.

### What an admin sets, and where

`make enable-physical-limiter` does everything in this section — policy,
namespace, RBAC and the node read — in one command. What follows is the same
outcome by hand, for a cluster where those objects are managed by something else.

Everything authoritative is a **cluster-scoped object**: a namespace admin holds
RBAC *inside* their namespace and can neither create a `Namespace` nor edit one's
annotations.

```bash
# Once per cluster. Every WVA on the cluster reads this, and none can opt out.
kubectl create namespace wva-policy
kubectl -n wva-policy create configmap wva-scaling-policy-config \
  --from-file=default=cluster-limiters.yaml

# Let a tenant's controller read it (read only — never write)
kubectl -n wva-policy create role wva-policy-reader \
  --verb=get,list,watch --resource=configmaps
kubectl -n wva-policy create rolebinding team-a-wva \
  --role=wva-policy-reader \
  --serviceaccount=team-a:wva-controller-manager
```

A namespace that draws its limits from somewhere other than the default is
**labelled**:

```bash
kubectl label namespace team-a wva.llmd.ai/policy-namespace=platform-policy
```

A label rather than an annotation because it is selectable — this answers "which
namespaces does this policy govern?", which is the question an admin asks when
auditing:

```bash
kubectl get ns -l wva.llmd.ai/policy-namespace=platform-policy
```

### What the controller does with that

Policy is resolved once at startup, in order:

1. the `wva.llmd.ai/policy-namespace` **label** on the namespace being managed
2. the **default policy namespace**, `wva-policy` — a name hardcoded in WVA, which
   is why the install scripts never mention it: one definition, in one language,
   so the two cannot drift
3. the controller's own namespace

Step 2 applies only to a controller that manages its own namespace. A
**cluster-scoped** controller already runs where only a cluster admin can write,
so its own ConfigMap *is* cluster-defined policy — a second namespace would add a
place to look and nothing else. It is also a deliberate guard: letting `wva-policy`
override an admin-owned controller meant that creating it for one tenant silently
switched a working, correctly-bounded cluster install to an empty policy, taking
it from bounded to **unbounded** with no edit to the install that changed.

Limiters written in the controller's own ConfigMap are then ignored *and logged* —
a limiter that reads as enforcing and enforces nothing is worse than either
enforcing or erroring. Thresholds, tiers and per-model settings stay with the team
running the workload: those are tuning, not entitlement.

If the policy ConfigMap **exists but cannot be read** — usually a missing
RoleBinding — the controller refuses to start rather than run unbounded while an
admin believes a quota applies.

### When policy demands a limiter the controller cannot serve

If cluster policy declares `gpu-inventory` and the controller may not list nodes,
it reports **not ready** and stays that way.

That is deliberate, and it is a *readiness* check rather than a startup one
because the situation has two arrival times:

- **at install** — the rollout never completes, so the install fails loudly
  instead of reporting success;
- **long after** — an admin adds the limiter to cluster policy, which the
  controller reloads live. The pod goes NotReady, which is visible and alertable.

The alternative is the failure this guards against: every variant charged to no
accelerator pool, given no GPU budget, and never scaling up again — indistinguishable
from an idle cluster.

Grant the node read — `make setup-prereqs` creates the
node-reader ClusterRole and its binding — or remove the limiter from cluster
policy.

### What this does and does not bound

**A guardrail, not an enforcement boundary.** Whoever owns the controller's
Deployment owns its args, env and image, so a tenant running WVA inside their own
namespace can change what bounds them. What this does guarantee is that the GPU
budget is authoritative for every controller actually running WVA — which covers
misconfiguration and drift.

WVA's limiter also bounds only what *WVA* asks for. The ScaledObject belongs to
the workload's owner: raising `maxReplicaCount` or adding a second KEDA trigger
bypasses it entirely, because the HPA takes the maximum across triggers.

**For a tenant you do not trust, bound the namespace at admission instead:**

```bash
kubectl -n team-a create quota gpus --hard=requests.nvidia.com/gpu=8
```

### The arrangement where the bound does hold

Run the controller **outside** the namespace it manages, so the tenant never owns
it:

```bash
# WVA_NS is admin-owned — where the controller runs.
# WVA_WATCH_NS is the tenant's — what it manages.
make deploy-wva-on-k8s WVA_SCOPE=namespace WVA_NS=wva-team-a WVA_WATCH_NS=team-a
```

This is the recommended multi-tenant shape. Everything above then applies with the
Deployment out of the tenant's reach.

## Why it ships off

The shipped configuration declares **no limiter**: a fresh install scales
unconstrained, and a scale-from-zero wake is published without a capacity check.
That default is deliberate. A GPU-aware optimizer allocates out of per-accelerator
pools, so a variant whose accelerator it cannot resolve is charged to no pool, gets
no budget, and **never scales up** — silently, because nothing errors. Enabling it
by default would freeze exactly the workloads that are least carefully configured.

## What has to be true first: every accelerator must resolve

WVA resolves a variant's accelerator from, in order:

1. a **GPU product key in the workload's `nodeSelector` or `nodeAffinity`** — the
   only source that works before any pod exists, and therefore the only one that
   works for a workload parked at zero;
2. the **node its pods are running on**, once it has ready pods.

On a **single-accelerator** cluster neither is needed: there is one pool, so an
unconstrained workload is deduced onto it. On a **heterogeneous** cluster it
matters, and not as bookkeeping — a pod with no GPU nodeSelector can be scheduled
onto *any* GPU node, so a workload that does not state its accelerator genuinely
has not chosen one. Pin it:

```yaml
# in the model server's pod template
spec:
  nodeSelector:
    nvidia.com/gpu.product: NVIDIA-A100-PCIE-80GB
```

```bash
kubectl get nodes -L nvidia.com/gpu.product          # what your nodes advertise
kubectl logs -n workload-variant-autoscaler-system   -l app.kubernetes.io/name=workload-variant-autoscaler | grep -i "Accelerator not resolved"
```

## Permission: nodes

The limiter reads **nodes** to learn what GPUs exist, and nodes are cluster-scoped.

WVA reads nodes on every cycle regardless of the limiter — a variant's accelerator
is resolved from the nodes its pods run on, and that identity is what the capacity
model keys learned per-replica capacity by. What the limiter changes is whether
that identity is also used to charge the variant to a GPU **budget**.

The consequence of a missing node permission therefore depends on the limiter:

| limiter | controller can list nodes | outcome |
| --- | --- | --- |
| `none` | no | degraded: accelerators stay unresolved, so metrics lose the accelerator label and the capacity model cannot reuse learned capacity across variants |
| `gpu-inventory` | no | **install refused.** Every variant would be charged to no pool, receive no budget and stop scaling up, silently |
| `gpu-inventory` | yes | as intended |

`make check-prereqs WVA_LIMITER=gpu-inventory` checks this before you install.

### If the limiter is turned on later

An admin can add a `limiters:` entry to the scaling-policy ConfigMap at any time,
and it is applied live — including to a controller that was installed without node
permission. That combination is the one failure with no natural symptom, so it is
reported three ways:

- an **error log**, naming what has stopped working;
- `wva_node_access_denied` set to **1**;
- the **`WVANodeAccessDenied`** alert (critical, fires after 2m) if you installed
  with `DEPLOY_ALERTING_RULES=true`.

## FMA namespaces: the GPU picture is a lower bound

If a namespace runs Fast Model Actuation, **treat every GPU number here as a
minimum, not a measurement.**

FMA's launcher pods request no `nvidia.com/gpu` at all — deliberately, since the
requester reserves the accelerator and the launcher binds onto it, and requesting
on both halves would double-book N launchers plus N requesters on an N-GPU node.
While a pair is bound that is exactly right, and the GPU is charged to the
requester, which is the scale target this limiter already accounts against.

The gap opens when a pair unbinds. The launcher keeps its vLLM instance resident —
that is what makes the next bind take seconds instead of minutes — and it goes on
occupying a physical GPU that is now charged to nobody. Measured on a real
cluster, with every requester at `replicas: 0`:

| | |
| --- | --- |
| GPU requests charged in the namespace | 1 |
| launcher pods running a vLLM instance | 9, on 9 distinct GPU UUIDs |

Three consequences:

- A `ResourceQuota` on `requests.nvidia.com/gpu` counts requesters only. It cannot
  express a warm pool, so GPU quota does not bound FMA's real consumption.
- This limiter's view of what is free is too optimistic by the size of the warm
  pool, which is the unsafe direction — it is the input the wake-capacity check
  and reclamation trust.
- The scheduler sees those GPUs as allocatable and may place another workload on
  one, which then contends with the sleeping instance when FMA wakes it.

**There is no fix available from outside FMA.** The obvious one — read it off the
pods — does not work: `dual-pods.llm-d.ai/vllm-config`, `/server-port` and
`/accelerators` are present only *while bound*. An orphaned launcher still running
an instance carries neither, so the API server holds no record of what it is
using. Only the launcher's own API knows, and WVA does not call workload APIs.

So: when planning capacity in an FMA namespace, subtract the warm pool by hand.
Its ceiling is `launcherCount × maxInstances` per matching node, from the
`LauncherPopulationPolicy` and `LauncherConfig`. `deploy/lib` warns at plan time
when it detects launchers. The upstream request that would close this is item 1
in [the FMA post-mortem](../proposals/fma-post-mortem.md). Note that none of
those requests were ever filed upstream.

## Checking

The install warns too: enabling `WVA_LIMITER=gpu-inventory` counts the distinct GPU
products the cluster advertises and tells you whether pinning is needed. An
unresolved variant is logged once per change and counted in
`wva_unattributed_gpus`. See
[GPU Capacity Accounting](../concepts/gpu-capacity-accounting.md).

