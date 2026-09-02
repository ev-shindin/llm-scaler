# Install WVA for the whole cluster

## Overview

This guide installs one Workload-Variant-Autoscaler that sizes the llm-d model
servers in **every** namespace. WVA decides how many replicas each variant needs
and hands the decision to KEDA, which owns the HPA and does the scaling.

Use it when one team runs the cluster. Where namespaces belong to different
teams, prefer one controller per namespace — see
[Install WVA in a namespace](../install-in-namespace/) — which keeps
failure domains, policy and upgrades separate.

**This scope is WIP**, and the install says so as it runs. It is not disabled and
is not going away: what it does not get is the preflight's namespace-centric
checks, which are written for a single namespace where llm-d is already serving.
In this shape the namespaces WVA manages need not exist yet, need not hold model
servers yet, and are discovered rather than named — so those checks would be
asking the wrong questions. `SKIP_CHECKS=true` installs without them.

## Prerequisites

- cluster-admin rights
- llm-d model servers somewhere on the cluster
- KEDA, installed for you if the cluster has none
- a Prometheus scraping those model servers

Every EPP the controller will size from needs the `flowControl` feature gate.
`make check-prereqs` is **fatal** without it — not degraded, refused — and the
only way past, `SKIP_CHECKS=true`, disables every preflight check rather than
that one. Cluster-wide this matters more than in a single namespace, because one
EPP missing the gate blocks the install for all of them. Enable it in each EPP's
`EndpointPickerConfig`:

```yaml
featureGates:
- flowControl
```

The gate is what publishes the queue depth. At zero replicas there are no
model-server metrics at all, so that queue is the only evidence anyone is asking
for a model — without it scale-from-zero can never fire. It also changes what the
engine metrics mean while running: flow control admits only what an engine can
absorb, so `vllm:num_requests_waiting` can sit at 0 while requests are genuinely
queued at the router.

<!-- guide:prerequisites.check start -->
```bash
make check-prereqs SCOPE=cluster
```
<!-- guide:prerequisites.check end -->

## Installation Instructions

### 1. Install WVA

<!-- guide:deploy.all start -->
```bash
make deploy-wva SCOPE=cluster
```
<!-- guide:deploy.all end -->

One command, because at this scope the cluster-admin half and the install half are
the same person: it runs [`setup-prereqs`](../admin-cluster-setup/) —
cluster-scoped RBAC, the namespace, the ServiceMonitor — and then the controller.
Where those are separate people, the namespace guide splits them into two steps.

### 2. Register the workloads

Nothing scales until a ScaledObject exists. At this scope the plan covers every
namespace holding model servers, so read it before applying it.

The same is true of `make workload-patch`, which reports the pod-spec settings
autoscaling needs — a drain hook, and weights on a volume. Say which you mean,
because `SCOPE` defaults to `namespace` everywhere in this repo:

```bash
make workload-patch SCOPE=cluster        # every namespace holding model servers
make workload-patch NAMESPACE=<ns>       # one namespace
```

With neither, it scans the controller's own namespace and reports
`No model servers found in scope` — which is accurate and not what you meant.

`SCOPE=cluster` matters most with `WVA_WORKLOAD_PATCH_APPLY=true`, which replaces
the pods it patches: at this scope that is a rolling restart of every model
server on the cluster. See [Writing the
patch](../../reference/workload-preparation.md#writing-the-patch-make-workload-patch).

<!-- guide:deploy.register start -->
```bash
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml
# edit wva-plan.yaml: apply: yes|no|adopt, the modelID, the replica bounds
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```
<!-- guide:deploy.register end -->

One entry per model server, applying nothing until you say so. `apply` takes
`yes`, `no`, or `adopt` — the last repoints a ScaledObject something else already
owns rather than adding a second. Each entry also carries `minReplicas`,
`maxReplicas` and `variantCost`; the file explains all of them in its own
comments. There is an example in
[Install WVA in a namespace](../install-in-namespace/README.md#4-register-the-workloads).

## Verification

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -A
```
<!-- guide:verify.objects end -->

**`READY True` on the ScaledObject** is the signal, together with a number in
the HPA's `TARGETS` — `1/1 (avg)` is WVA saying one replica is the right size.

Do not read `CurrentMetrics` as the check. On a ScaledObject whose trigger WVA
never answers, it is still populated — with an empty entry, `[{"type":""}]` —
while `READY` is `False` and `TARGETS` reads `<unknown>/1 (avg)` and nothing is
being scaled. Measured on kind: it says "populated" in exactly the case you are
verifying against.

`TARGETS` showing `<unknown>` means KEDA could not reach the scaler. The usual
cause is a ScaledObject naming a different install's namespace — most easily hit
by installing cluster-wide over a namespace-scoped install, or the other way
round, since the object keeps the old address. Repoint it at the install that is
running:

<!-- guide:troubleshoot.repoint start -->
```bash
make scaledobjects-repoint
```
<!-- guide:troubleshoot.repoint end -->

## Cleanup

<!-- guide:cleanup.uninstall start -->
```bash
make undeploy-wva SCOPE=cluster
```
<!-- guide:cleanup.uninstall end -->

Prometheus, KEDA and the namespaces stay — they are shared, and this install may
not have created them. `UNDEPLOY_SHARED=true` removes them too.

That removes the **autoscaler only**. Model servers keep serving in every
namespace the controller was managing, which matters more here than in a
single-namespace install: one `undeploy` can leave many namespaces running.

If a namespace's model came from
[Install a small llm-d model](../install-small-model/), remove it per namespace:

<!-- guide:cleanup.model start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=<the model namespace>
```
<!-- guide:cleanup.model end -->

## Configuration

Optional.

| Parameter | Default | Example |
| --- | --- | --- |
| `SCOPE` | `namespace` — **set `cluster` for this guide** | `cluster` |
| `WVA_NS` | `workload-variant-autoscaler-system` | `wva-system` |
| `IMG` | the image CI builds from main | `ghcr.io/you/wva:dev` |

Full list: [Configuration reference](../../reference/configuration.md).

## Next

- `make benchmark-smoke NAMESPACE=<ns>` — drive load at one of the namespaces
  this controller manages and snapshot the dashboard
- [Bound every WVA by real GPUs](../admin-gpu-bounding/)
- `deploy/warmpool.sh plan -n <ns>` — read-only: whether a namespace this
  controller manages has models that could share a [warm pool](../warm-pool/),
  which holds engines loaded so a scale-up serves while its replica starts
- [After the install](../../reference/operations.md)
- [Install methods](../../reference/install-methods.md) — GitOps, direct
  Kustomize, and what the script does
