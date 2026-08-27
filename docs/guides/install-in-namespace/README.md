# Install WVA in a namespace

## Overview

This guide installs the Workload-Variant-Autoscaler into one namespace, where it
sizes the llm-d model servers running there and nothing else. WVA decides how
many replicas each variant needs from saturation and cost, and hands the decision
to KEDA, which owns the HPA and does the scaling.

It is the common path, and it works the same whether llm-d is already serving or
you have just deployed it. A cluster admin runs one command first, once — see
[Cluster-admin setup](../admin-cluster-setup/) — after which the
namespace's owner installs and upgrades the controller themselves.

## Prerequisites

- **llm-d model servers in the namespace, deployed first.** WVA scales what is
  already serving; it does not deploy models, and the install refuses a namespace
  with none rather than leave a healthy controller with nothing to scale. No
  model yet? [Install a small llm-d model](../install-small-model/) puts one on a
  single GPU.
- KEDA. On OpenShift it is installed for you. On Kubernetes it is **not**:
  `setup-prereqs` assumes the cluster already has one, because on a shared
  cluster installing an operator is somebody else's decision. If the cluster
  has none, re-run with `KEDA_HELM_INSTALL=true make setup-prereqs`.
- a Prometheus scraping those model servers
- **a model cache on those model servers.** Autoscaling means new replicas,
  and a replica that has to fetch its weights first turns every scale-up into
  a download from Hugging Face. llm-d mounts one by default, so this is
  normally already true — `make scaledobjects-plan` says so per workload if it
  is not. See [Weights and the model
  cache](../../deployment/operations.md#weights-and-the-model-cache), which
  also covers what it does not buy: the cache removes the download, not the
  load.
- **a drain hook on those model servers.** Autoscaling also means replicas being
  *removed*, and a replica removed mid-stream takes its open responses with it:
  the client sees a truncated body, after the request was already paid for in GPU
  time. This one is normally **not** already true: llm-d does not set a preStop
  hook.

`make workload-patch` reports both and writes the fix for whichever is missing.
It writes a file rather than changing your workloads, because the pod spec
belongs to your modelservice chart; see [Writing the
patch](../../deployment/operations.md#writing-the-patch-make-workload-patch).

If you want a model to scale **from zero**, its EPP must run with the
`flowControl` feature gate: that gate is what publishes the queue depth, and at
zero replicas there are no model-server metrics, so it is the only evidence that
anyone is asking for the model. WVA enables it on an EPP it installs itself; an
EPP that came from an llm-d guide has it off unless you turned it on. WVA reads
the queue by scraping the EPP, and falls back to reading the same metric from
Prometheus when it cannot — so a Prometheus that already scrapes your EPP covers
it. Without the gate, neither path has anything to read, and scale-from-zero
never fires while ordinary scaling works.

Every install/preflight check that can refuse below is described, with why and
the fix, in [First-line troubleshooting](../../deployment/operations.md#first-line-troubleshooting).
`SKIP_CHECKS=true` bypasses all of them and installs anyway; the controller
then sizes from whatever signals do exist, and stays silent about the rest.

<!-- guide:env.static.namespace start -->
```bash
export NAMESPACE=<your-namespace>
```
<!-- guide:env.static.namespace end -->

Required, not merely conventional: the install refuses without it rather than
guess which namespace it manages. `WVA_NS` is a different setting — it names
where the *controller* is installed, and only differs from the managed namespace
in the cluster-wide shape.

**Read this before you run the check below**: it is read-only, but an empty
namespace is a warning worth taking seriously, not a note to skim past — a
controller pointed at the wrong one installs cleanly, reports healthy, and
scales nothing. If the check below names one, stop and fix the namespace before
going further.

<!-- guide:prerequisites.check start -->
```bash
make check-prereqs
```
<!-- guide:prerequisites.check end -->

Read-only. It reports the namespace it resolved, whether that namespace holds
model servers, and the Prometheus it found. `SKIP_CHECKS=true` bypasses the tool
and permission checks on an **install** — for CI, or for someone who has already
confirmed what they would have checked. It does not apply here: running the
checks is all this command does, so it runs them either way. Nor is it a blanket
bypass of the install — the guard against a second install repointing the shared
ClusterRoleBindings, and the post-install verification, both run regardless. A
skipped check is a failure mode it stops warning you about, not one that stops
happening.

## Installation Instructions

### 1. Cluster admin: prepare the namespace

**This step needs cluster-admin rights** — everything after it does not.

<!-- guide:deploy.prereqs start -->
```bash
make setup-prereqs
```
<!-- guide:deploy.prereqs end -->

Once per namespace, not once per upgrade. It creates the namespace, the
cluster-scoped RBAC and the ServiceMonitor: the objects a namespace admin is not
allowed to create. See [Cluster-admin setup](../admin-cluster-setup/)
for what each one is and why it needs an admin.

On Kubernetes this expects the cluster to already have KEDA. If it does not,
the step stops and tells you to re-run as:

```bash
KEDA_HELM_INSTALL=true make setup-prereqs
```

It is a separate opt-in because installing a cluster operator on a shared
cluster is a decision for whoever runs it. OpenShift installs KEDA for you.

Already done for your namespace? `make check-prereqs` above names anything still
missing; if it names nothing, go to step 2.

### 2. Install the controller

<!-- guide:deploy.controller start -->
```bash
make deploy-wva
```
<!-- guide:deploy.controller end -->

It installs the controller only, and says so: creating cluster-scoped objects is
not something a namespace owner can do, so that half is the one step 1 covers. If
step 1 has not happened, this stops and names what is missing.

### 3. Make the workloads safe to scale

The two prerequisites above are stated at the top of this guide and are easy to
skip, so this is the step that checks them. It reports and changes nothing.

<!-- guide:deploy.readiness start -->
```bash
# What the model servers need before anything scales them. Reports first;
# changes nothing until you ask.
make workload-patch NAMESPACE=${NAMESPACE}
```
<!-- guide:deploy.readiness end -->

A model server with no preStop hook loses in-flight responses on every
scale-down, and one that downloads its weights outside every volume it mounts
re-fetches them on every scale-up. Both are pod-spec settings owned by the chart
that deployed the model server, so this writes a patch rather than applying one.
See [Writing the
patch](../../deployment/operations.md#writing-the-patch-make-workload-patch) for
what to do with it, including `make model-cache` when the weights half is what is
missing.

### 4. Register the workloads

Nothing scales until a ScaledObject exists: WVA is only ever asked about
workloads KEDA calls it about.

<!-- guide:deploy.register start -->
```bash
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml
# edit wva-plan.yaml: apply: yes|no|adopt, the modelID, the replica bounds
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```
<!-- guide:deploy.register end -->

`scaledobjects-plan` finds the model servers and writes one entry each, applying
nothing:

```yaml
plan:

  - apply: "yes"                        # yes | no | adopt
    namespace: llm-d-sim
    kind: Deployment
    name: dev-model-decode
    modelID: "meta/llama"               # required — what the container serves
    minReplicas: 1
    maxReplicas: 10
    variantCost: "10.0"
    inferencePool: "optimized-baseline" # informational
```

Edit it; `scaledobjects-apply` does exactly what you left in it. Every field is
explained in the comments the file is written with, so there is nothing to look
up. `apply: adopt` is for a workload something else already scales: it repoints
that object at WVA instead of adding a second, because two ScaledObjects on one
target is two HPAs writing the same replica count. A workload whose model could
not be read is written as `no` with the reason, and never created without one.

> **Running Fast Model Actuation?** The plan can carry a second entry for the
> same model — the FMA half — switched off by default, and an entry can arrive as
> `apply: no` until the launcher pods are scraped. See
> [the FMA post-mortem](../../proposals/fma-post-mortem.md).

## Verification

### 1. Check KEDA received the decision

<!-- guide:verify.objects start -->
```bash
kubectl get scaledobject,hpa -n ${NAMESPACE}
```
<!-- guide:verify.objects end -->

A working registration looks like this:

```text
NAME                                        SCALETARGETKIND      SCALETARGETNAME    MIN  MAX  READY  ACTIVE  TRIGGERS
scaledobject.keda.sh/dev-model-decode-wva   apps/v1.Deployment   dev-model-decode   1    10   True   True    external-push

NAME                                                  REFERENCE                     TARGETS     MINPODS  MAXPODS  REPLICAS
horizontalpodautoscaler.autoscaling/keda-hpa-dev-...  Deployment/dev-model-decode   1/1 (avg)   1        10       1
```

Three things to read, in this order:

- **`READY True`** — KEDA reached WVA and got a metric spec back. The HPA is
  KEDA's; it creates one per ScaledObject.
- **`TARGETS` showing a number** — the decision is flowing. `1/1 (avg)` is WVA
  saying one replica is the right size.
- **`ACTIVE True`** — there is traffic. `Unknown` on a workload nobody is
  calling is normal, and not a fault.

`TARGETS` reading **`cpu: <unknown>/80%`** means the opposite of it looks: KEDA
could not fetch the metric spec from WVA and fell back to a CPU metric, so the
workload is not being scaled by WVA at all. `READY False` accompanies it. The
usual cause is a trigger naming a scaler it cannot reach, which is what a
ScaledObject written for a different install does — the shipped samples name the
default namespace, and a namespace-scoped install is not there. Repair it with:

```bash
make scaledobjects-repoint
```

It takes no arguments — it finds the install that is running and points the
object at it. It rewrites `scalerAddress` only, leaves your `modelID`,
`variantCost` and bounds untouched, and is idempotent. See
[First-line troubleshooting](../../deployment/operations.md#first-line-troubleshooting)
for the other causes.

### 2. Read the decisions

**Give it an optimize cycle first.** WVA decides on a timer, so a ScaledObject
created seconds ago has not been through one yet and this returns nothing —
which reads like a broken install and is not. Measured on a fresh namespace: 0
decision lines immediately after `scaledobjects-apply`, 53 a few minutes later,
with no change in between. If it is still empty after a few minutes, that is
worth investigating; straight away, it is not.

<!-- guide:verify.decisions start -->
```bash
kubectl logs -n ${NAMESPACE} deploy/wva-controller-manager | grep scaling-decision
```
<!-- guide:verify.decisions end -->

```text
scaling-decision {"modelID":"meta/llama","decisions":[{"name":"llama-decode-wva","curr":1,"tgt":3,"action":"scale-up"}]}
```

### 3. (Optional) See it in a dashboard

<!-- guide:visualize.dashboard start -->
```bash
make dashboard
```
<!-- guide:visualize.dashboard end -->

**OpenShift only.** It reads Thanos through the platform's tenancy port and
publishes itself through a Route, neither of which exists on vanilla Kubernetes;
it checks, and says so rather than building a Grafana that cannot work. On plain
Kubernetes, import `deploy/grafana/operational-dashboard.json` into your own
Grafana and point it at the Prometheus `make check-prereqs` reports.

Stands up a Grafana private to this namespace — not the shared cluster
instance, whose dashboards are read-only — with WVA's operational dashboard
already imported, reading Thanos through a namespaced role rather than any
cluster-scoped grant. Requires grafana-operator's CRDs; that is a cluster-admin
install, so this only creates namespaced objects and says plainly if they are
missing. It also refuses a namespace with no model servers in it: the namespace
is what the datasource enforces and what the dashboard pins its variable to, so
the wrong one yields a dashboard that is empty rather than broken. Prints the
dashboard URL and the admin password on every run, so it is safe to re-run for
either.

### 4. (Optional) Drive some load and look at it

<!-- guide:visualize.smoke start -->
```bash
make benchmark-smoke NAMESPACE=${NAMESPACE}
```
<!-- guide:visualize.smoke end -->

Symmetric 1024-in/1024-out at 15 req/s for five minutes, then a snapshot of the
dashboard over
exactly that window, and a line telling you where it is. It answers one question
— is WVA actually scaling this? — and it needs a GPU cluster, because
that is what serving a model needs.

It is not a measurement of the model: five minutes of Poisson arrivals is not a
latency study, and a small model's TTFT says nothing about a large one's. For
numbers worth comparing, see [Benchmark WVA](../benchmarking/).

## Cleanup

<!-- guide:cleanup.uninstall start -->
```bash
make undeploy-wva
```
<!-- guide:cleanup.uninstall end -->

This removes the ScaledObjects it created, and KEDA restores each workload to the
replica count it had before WVA sized it. Objects you adopted are left alone and
listed by name — they were not this installer's to delete — so repoint or remove
them yourself: their trigger now calls a scaler that is gone, KEDA keeps the HPA,
and a workload parked at zero can never be woken.

`UNDEPLOY_SCALEDOBJECTS=false` keeps everything, for reinstalling in place.

### The model servers are still running

`make undeploy-wva` removes the **autoscaler only**. Every model server, EPP and
InferencePool in the namespace keeps serving

-- which is usually what you want,
since WVA did not create them and something else may depend on them.

A drain hook applied with `WVA_WORKLOAD_PATCH_APPLY=true` stays with them for the
same reason: those fields belong to the model server, not to WVA, so they outlive
the uninstall. The next `helm upgrade` of that chart reverts them. A model cache
created by `make model-cache` is likewise left alone -- it holds data. — which is usually what you want,
since WVA did not create them and something else may depend on them.

If you stood the model up with
[Install a small llm-d model](../install-small-model/), that is what removes it:

<!-- guide:cleanup.model start -->
```bash
make benchmark-teardown BENCHMARK_NAMESPACE=${NAMESPACE}
```
<!-- guide:cleanup.model end -->

Run it **after** `make undeploy-wva`, not instead of it. It tears down the llm-d
releases and the namespace, and a controller left behind would keep calling a
scaler for workloads that no longer exist.

To check nothing was missed — a namespace-scoped install still creates
cluster-scoped RBAC, which deleting the namespace alone would strand:

```bash
kubectl get clusterrole,clusterrolebinding -o name | grep -i "${NAMESPACE}"
```

## Configuration

Optional. Everything below is detected; set one only to override what the
preflight reported.

| Parameter | Default | Example |
| --- | --- | --- |
| `NAMESPACE` | the namespace running llm-d, discovered | `llm-d-optimized-baseline` |
| `IMG` | the image CI builds from main | `ghcr.io/you/wva:dev` |
| `PROMETHEUS_URL` | detected; Thanos on OpenShift | `https://prom.monitoring.svc:9090` |
| `KEDA_HELM_INSTALL` | `false` on Kubernetes — assumes cluster KEDA | `true`, to install one with Helm |

Full list: [Configuration reference](../../deployment/configuration.md).

## Next

- [After the install](../../deployment/operations.md)
- [Bound every WVA by real GPUs](../admin-gpu-bounding/) — otherwise
  scaling is bounded only by each workload's `maxReplicaCount`
- [Scaling policy](../../developer-guide/scaling-policy-config.md)
