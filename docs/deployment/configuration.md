# Deployment configuration reference

Every option `deploy/install.sh` reads. Verified against the script: each entry here is read by something, and every `VAR=${VAR:-default}` in `install.sh` and `deploy/lib/*.sh` appears below.

> Part of the [WVA deployment guide](../../deploy/).

## Required

| Variable | Description | Required For |
|----------|-------------|--------------|
| `HF_TOKEN` | HuggingFace token | llm-d deployment |

## Core configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `ENVIRONMENT` | Deployment environment (`kubernetes` or `openshift`) | `kubernetes` |
| `WVA_SCOPE` | `cluster` or `namespace` — see [Scope](../guides/install-cluster-wide/). `namespace` everywhere, including `deploy/install.sh` run directly. It used to be inferred from the platform (`namespace` on OpenShift, `cluster` elsewhere); that inference is gone. Cluster scope still works and warns that it is WIP | `namespace` (`SCOPE=cluster` to change it) |
| `WVA_LIMITER` | `none`, `gpu-inventory` or `quota` — declares the limiter in the scaling-policy ConfigMap | `none` |
| `WVA_WATCH_NS` | Namespace a namespace-scoped controller **manages**, when that differs from the one it runs in. Setting it puts the controller outside the namespace it manages, so the workloads' owner does not administer the controller — the arrangement where a GPU bound actually holds. See [the GPU limiter](gpu-limiter.md#the-arrangement-where-the-bound-does-hold) | the controller's own namespace |
| `INSTALL_PHASE` | `prereqs` (cluster admin: namespace, RBAC, ServiceMonitor, Prometheus/KEDA) \| `wva` (the controller) \| `all`. **Usually leave it unset**: an install that is not told picks the half left to do — `wva` when the caller cannot create cluster-scoped objects, or when the admin half is already in place. `make setup-prereqs` is the `prereqs` phase — see [deploy/](../../deploy/) | auto |
| `WVA_PROJECT` | Repository root the script installs from | `$PWD` |

## Image

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_IMAGE_REPO` | WVA image repository | `ghcr.io/llm-d/llm-d-workload-variant-autoscaler` |
| `WVA_IMAGE_TAG` | WVA image tag | `latest` |
| `WVA_IMAGE_PULL_POLICY` | Image pull policy | `Always` |

## Namespaces

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_NS` | WVA controller namespace | `workload-variant-autoscaler-system` |
| `MONITORING_NAMESPACE` | Prometheus namespace | `workload-variant-autoscaler-monitoring` |
| `NAMESPACE` | Where llm-d runs — **llm-d's own variable**, exported by its guides. `deploy/install-epp.sh` installs EPP there, `DEPLOY_LLMD_NS=true` creates it, and setting it **defaults `WVA_NS` for the `*-namespace-on-*` targets** (an explicit `WVA_NS` wins; cluster-scoped targets are unaffected). It is never passed to the controller — WVA has no watch and no listing, and ScaledObject discovery does not read it — see [Which namespace is which](#which-namespace-is-which). **Required in namespace scope**, which is the default: the install refuses rather than guess which namespace it manages, so the fallback below applies only to cluster scope and `kind-emulator` | `llm-d-optimized-baseline` (cluster scope only) |
| `LLMD_NS` | **Deprecated** alias for `NAMESPACE`, this repo's own former name for it. Still honoured, with a warning | — |

## Deployment flags

| Variable | Description | Default |
|----------|-------------|---------|
| `DEPLOY_PROMETHEUS` | Deploy the Prometheus stack **when the cluster has none of its own**. A Prometheus outside `MONITORING_NAMESPACE` is used as it is, and no monitoring namespace is created. Set `PROMETHEUS_FORCE_INSTALL=true` to deploy alongside one (two operators then contend over the same CRs) | `true` |
| `DEPLOY_OPERATIONAL_DASHBOARD` | Publish the WVA Grafana dashboard. On Kubernetes also enables Grafana in kube-prometheus-stack. Publishing into a **shared** monitoring namespace is a cluster-admin action; a namespace admin is told so and the install continues ([details](monitoring.md#if-you-cannot-write-to-the-monitoring-namespace)) | `true` |
| `DEPLOY_WVA` | Deploy WVA controller | `true` |
| `DEPLOY_LWS` | Deploy LeaderWorkerSet (needed only for full e2e suite; skip for smoke, benchmarks, or pre-installed clusters) | `false` |
| `DEPLOY_ALERTING_RULES` | Install the PrometheusRule alerts | `false` |
| `WVA_FMA_LAUNCHER_METRICS` | Apply a PodMonitor that scrapes FMA launcher pods, which nothing else does — they declare no container ports, so a port-name endpoint generates no target for them. Applied to the **workload** namespace (`WVA_WATCH_NS`, else `WVA_NS`), and skipped if another PodMonitor already scrapes launchers there. See [FMA launcher pods](operations.md#fma-launcher-pods) | `false` |
| `DEPLOY_LLMD_NS` | Create an empty llm-d namespace up front. Useful only for a demo that wants it to exist before anything is deployed into it; `deploy/install-epp.sh` creates its own when it deploys EPP. WVA does not watch namespaces, so this creates a namespace nothing is looking at | `false` |
| `ENABLE_SCALE_TO_ZERO` | Allow a model to be parked at zero replicas, and enable the EPP `flowControl` gate that makes waking it possible | `true` |
| `SKIP_CHECKS` | Skip prerequisite checks | `false` |
| `SCALER_BACKEND` | `keda` or `none` (use a pre-installed backend) | `keda` |
| `KEDA_NAMESPACE` | Namespace KEDA is installed in | `keda-system` |
| `KEDA_HELM_INSTALL` | Install KEDA with Helm rather than assuming it is present | `false` |
| `KEDA_CHART_VERSION` | KEDA Helm chart version | `2.19.0` |
| `UNDEPLOY` | Remove instead of install (`install.sh` doubles as the uninstaller) | `false` |
| `DELETE_NAMESPACES` | With `UNDEPLOY=true`, also delete the WVA and monitoring namespaces | `false` |
| `DELETE_LLMD_NS` | With `DELETE_NAMESPACES=true`, also delete `LLMD_NS`. Separate because that namespace holds the model servers: deleting it takes the workloads with it | `false` |
| `CHECK_ONLY` | Run the prerequisite and permission checks, then exit without deploying. Set by `--check` / `make check-prereqs` | `false` |
| `WVA_REPLICAS` | Controller replicas. The manifest already elects a leader, so extra replicas are **warm standbys, not extra throughput** — only the leader runs the optimization loops. Two turns a node drain from "no decisions until rescheduled" into a lease timeout | `1` |
| `UNDEPLOY_SCALEDOBJECTS` | On uninstall, remove the ScaledObjects **this installer created** (KEDA then restores each workload to its pre-autoscaling replica count). Objects you adopted are never removed — they were not this installer's to make — but are listed, because their trigger now calls a scaler that is gone | `true` |
| `UNDEPLOY_SHARED` | With `UNDEPLOY=true`, also remove Prometheus, the scaler backend and EPP. **Off by default**: they are shared, this install may not have created them, and removing them takes out everything else on the cluster that uses them | `false` |

> `make deploy-e2e-infra` passes `ENABLE_SCALE_TO_ZERO=$(SCALE_TO_ZERO_ENABLED)`,
> whose Makefile default is **`false`** — the opposite of `install.sh`'s. So an
> e2e deploy has scale-to-zero OFF unless you pass
> `SCALE_TO_ZERO_ENABLED=true`, while a plain `make deploy-wva-on-k8s` has it ON.
> Three e2e suites skip silently when it is off.

ScaledObjects, HPA stabilization (`spec.advanced.horizontalPodAutoscalerConfig.behavior`) and vLLM ModelService tuning are not controlled by `install.sh`; manage them via `kubectl apply` directly (see the [llm-d guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline) for reference manifests).

## Default ScaledObjects

**Read this if WVA is installed and nothing is scaling.** A ScaledObject is how a
workload *registers* with WVA: the controller has no watch and no listing, so it
only ever learns about workloads KEDA calls it about. An install with no
ScaledObject anywhere is a controller that is never asked anything — idle, and
reporting itself healthy.

These options create one per llm-d model server, so you do not have to hand-write
them. A model server is any Deployment or LeaderWorkerSet labelled
`llm-d.ai/inferenceServing=true`; its model is read from `--served-model-name`, or
`--model` where it sets no other.

It works as **plan then apply**, because creating autoscaling objects across a
cluster is not something to discover the shape of afterwards.

```bash
# 1. See what would be created. Nothing is applied.
make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml

# 2. Edit it: apply: yes|no|adopt, the modelID, the replica bounds, the cost.
$EDITOR wva-plan.yaml

# 3. Apply exactly that file.
make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml
```

The plan is YAML, and carries its own documentation — every field it accepts is
explained in the comments it is written with, so editing it needs nothing open
next to it. Printed as a table on the way past, and written like this:

```yaml
# WVA ScaledObject plan. Nothing here has been applied yet.
#
# apply          Required. One of:
#                  yes    create a ScaledObject for this workload
#                  no     leave the workload alone
#                  adopt  it already has a ScaledObject — repoint that one at WVA
#                         instead of adding a second.
# ...
plan:

  - apply: "yes"
    namespace: llm-d-sim
    kind: Deployment
    name: sotest-a
    modelID: "e2ewva/dummy-model"
    minReplicas: 1
    maxReplicas: 10
    variantCost: "10.0"
    # scalingPolicy: "standard"
    # inferencePool: optimized-baseline

  # note: no --served-model-name or --model on the container, so the model could
  # not be read. Fill in modelID and set apply: yes to include it.
  - apply: "no"  # yes | no
    namespace: llm-d-sim
    kind: Deployment
    name: sotest-b
    modelID: ""
    minReplicas: 1
    maxReplicas: 10
    variantCost: "10.0"
    # scalingPolicy: "standard"
    # inferencePool: (none)
```

Two things the entries carry that are not fields. The `apply:` line lists the
values **that entry** accepts — `adopt` appears only where there is something to
adopt. And anything informational is a comment, so that editing it cannot change
what happens: `# scaledObject:` names the object `adopt` would repoint, and
`# inferencePool:` the EPP queue the workload sits behind.

| field | |
| --- | --- |
| `apply` | `yes` creates, `no` skips, `adopt` repoints a ScaledObject the workload already has. Never both: two ScaledObjects on one target is two HPAs writing the same replica count, so `yes` on a workload that already has one is refused rather than quietly adopted |
| `modelID` | **Required.** What the container serves, and the grouping key — entries sharing a `modelID` are sized against each other. An entry with none is never applied: an object created without it registers a variant of a model nobody runs |
| `minReplicas` / `maxReplicas` | What KEDA holds the workload between; WVA decides within them. Default 1 and 10, and for `adopt` they are read from the object being adopted, so applying it unedited changes only who decides the count |
| `variantCost` | The relative price of one replica of this variant. Only the ratio between variants of one model matters, so with one variant it changes nothing |
| `scalingPolicy` | Optional, and commented out by default. Names a reusable policy tier — `interactive`, `standard`, `batch` — from the scaling-policy ConfigMap. Leaving it out means "whatever the cluster default says", which an admin can then change for every workload at once; naming one opts this workload out of that. A name no tier matches falls back to the default silently |
| `inferencePool` | Informational, a comment, never applied: the EPP queue that workload sits behind, resolved by matching pod labels against each pool's selector — the same way WVA resolves it. `(none)` means no pool has adopted it |

Entries that cannot be applied are marked `no` **and kept**, with the reason,
rather than dropped: the file is then the whole truth about what was found, and
turning a `no` into a `yes` is a deliberate act.

If you would rather review interactively, `make scaledobjects-edit` opens the same
plan in `$EDITOR` and asks before applying. It needs a terminal; everything it can
do is also reachable through plan-then-apply, which does not.

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_DEFAULT_SO` | `false` (do nothing), `plan` (list and stop), `edit` (list, `$EDITOR`, confirm), `true` (apply everything found) | `false` |
| `WVA_DEFAULT_SO_NS` | Namespace to scan. `wva` for WVA's own, `all` for every namespace holding model servers. The default follows what this install can reach — `all` when cluster-scoped, its own namespace when namespace-scoped — so you rarely need to set it | scope-derived |
| `WVA_DEFAULT_SO_PLAN` | An existing file is applied as-is, edits included. Otherwise, where the generated plan is written | a temp file |
| `WVA_DEFAULT_SO_MIN` | `minReplicaCount` on generated objects. Not `0` even with scale-to-zero on: parking a model costs its next request a cold start, which is a decision about that workload's users | `1` |
| `WVA_DEFAULT_SO_MAX` | `maxReplicaCount` on generated objects | `10` |
| `WVA_DEFAULT_SO_TEMPLATE` | Your own ScaledObject template, substituted per workload. Placeholders: `{{NAMESPACE}}` `{{NAME}}` `{{KIND}}` `{{APIVERSION}}` `{{MODEL_ID}}` `{{SCALER_ADDRESS}}` `{{MIN}}` `{{MAX}}` `{{VARIANT_COST}}` `{{SCALING_POLICY}}`. Start from `config/samples/keda/external-scaler/scaledobject-template.yaml` | the shipped shape |

| `WVA_SO_SCALE_UP_PERIOD` | `periodSeconds` on the generated object's scale-up policy. A rate-limit window, not a delay — and nothing acts faster than the HPA control loop's sync period (15s by default) | `5` |
| `WVA_SO_SCALE_UP_STABILIZATION` | `stabilizationWindowSeconds` for scale-up. This is the knob that delays a scale-up: HPA takes its most conservative recommendation across the window. `0` means act on the current one | `0` |
| `WVA_SO_SCALE_DOWN_PERIOD` | `periodSeconds` on the scale-down policy | `120` |
| `WVA_SO_SCALE_DOWN_STABILIZATION` | `stabilizationWindowSeconds` for scale-down — how long demand must stay low before a replica is removed | `300` |

`WVA_DEFAULT_SO_MIN` and `_MAX` are only the values the plan is *written* with.
What gets applied is what the file says when you apply it, per entry.

### Workload readiness (`make workload-patch`)

Two pod-spec settings decide whether autoscaling a model server is safe to turn
on: a preStop hook, so a removed replica finishes what it is writing, and weights
that land on a mounted volume, so a new replica does not re-download them.
Neither belongs to WVA — they belong to the chart that owns the pod spec — so
`make workload-patch` writes a patch and, on request, applies the half that is
safe to apply. Full description in
[After the install](workload-preparation.md#writing-the-patch-make-workload-patch).

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_WORKLOAD_PATCH_FILE` | Where the patch is written | `wva-workload-patch.yaml` |
| `WVA_WORKLOAD_PATCH_APPLY` | `true` also applies the **drain half** to the live workloads, which replaces their pods | `false` |
| `WVA_WORKLOAD_PATCH_APPLY_WEIGHTS` | `true` also applies the **weights volume**, but only where it cannot break the workload: no volume of that name, nothing already mounted at that path, and the claim exists. Otherwise it is emitted with the reason, and the drain half still applies | `false` |
| `WVA_MODEL_PVC_SIZE` | Size for `make model-cache`. No default — it depends on how many models share the claim | *(required)* |
| `WVA_MODEL_PVC_CLASS` | StorageClass for `make model-cache`. No default, and it must support ReadWriteMany. Run `make model-cache` with neither to list the classes this cluster already serves RWX from | *(required)* |
| `WVA_MODEL_PVC_TIMEOUT` | How long to wait for the claim to bind. A `WaitForFirstConsumer` class is detected and not waited on | `120` |
| `WVA_DRAIN_GRACE_SECONDS` | `terminationGracePeriodSeconds` in the emitted patch. An existing longer value is kept, never lowered | `120` |
| `WVA_DRAIN_SLEEP_SECONDS` | The preStop sleep — the drain window itself | `45` |
| `WVA_MODEL_PVC_NAME` | The claim the emitted weights volume names. Nothing creates it | `model-pvc` |
| `WVA_MODEL_VOLUME_NAME` | The volume name in the emitted patch | `model-storage` |
| `WVA_MODEL_CACHE_PATH` | Where that volume is mounted, and the parent of the emitted `HF_HOME` | `/model-cache` |

Scope follows the same rules as the ScaledObject plan above: `NAMESPACE=<ns>`
pins the scan to one namespace, and without it a cluster-scoped install walks
every namespace holding model servers.

The scaling behaviour is written into every generated ScaledObject rather than
inherited. Kubernetes' own defaults happen to match the windows above, so on a
stock cluster only the policy periods differ — but those defaults belong to the
API server, and a cluster that retunes them would quietly change how every
workload WVA creates scales, with nothing in the object to show it. The
asymmetry is deliberate: scale-up acts immediately because the wait is already
paid in cold start, while scale-down is patient because removing a replica too
early costs that cold start on the next request and keeping one too long only
costs money.

**Adopting an existing ScaledObject leaves its behaviour alone.** `apply: adopt`
repoints the triggers and bounds; polling interval, cooldown, fallback and
`behavior` stay as whoever tuned them left them.

Set them on a deploy to do this during install:

```bash
make deploy-wva-on-k8s WVA_DEFAULT_SO=plan   # install, then show what it would create
make deploy-wva-on-k8s WVA_DEFAULT_SO=true WVA_DEFAULT_SO_NS=all
```

Two things it will never do. It **does not touch a workload that already has a
ScaledObject** — that one may be hand-tuned or GitOps-managed, and two
ScaledObjects on one target is two HPAs fighting over a replica count. And it
**skips any workload whose model it cannot determine** rather than guessing,
because a wrong `modelID` groups a workload with a model it does not serve and
mis-scales both.

Generated objects use an `external-push` trigger, so KEDA holds a stream open and
WVA pushes activation the moment it decides — the difference between waking a
parked workload in about the detection interval and waiting out a poll.

## Pointing at an existing Prometheus

Required whenever this install did not deploy Prometheus itself. `PROMETHEUS_URL`
is written into the controller's config; without it the controller keeps the
shipped default and **exits at startup** if nothing answers there
(`CRITICAL: Failed to connect to Prometheus`).

| Variable | Description | Default |
|----------|-------------|---------|
| `PROMETHEUS_URL` | Full base URL, e.g. `https://prom.monitoring.svc.cluster.local:9090` | the kube-prometheus-stack this install deploys |
| `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | Connect without verifying the server certificate | `true` in the shipped config |
| `DEPLOY_PROMETHEUS` | Deploy a Prometheus stack when the cluster has none of its own — see [Deployment flags](#deployment-flags) | `true` |

## Which namespace is which

Three namespaces appear in these options and they do different jobs:

| variable | what lives there | who reads it |
| --- | --- | --- |
| `WVA_NS` | the controller, its ConfigMaps and its external-scaler Service | the installer, and the controller (as `POD_NAMESPACE`) |
| `NAMESPACE` | your model servers | **the installer only** — never passed to the controller |
| `MONITORING_NAMESPACE` | Prometheus and Grafana, if this install deploys them | the installer |

**Which namespaces get ScaledObjects follows the scope, not `NAMESPACE`.** A
cluster-scoped install scans every namespace holding model servers, because it can
manage them all; a namespace-scoped install scans its own, because that is the only
namespace it can read. `WVA_DEFAULT_SO_NS` narrows it if you want less.

`NAMESPACE` not reaching the controller is not an oversight. WVA has no watch and no
listing: it learns about a workload when KEDA calls its external scaler about it,
from any namespace. It never goes looking in a namespace, so it has no use for the
name of one.

### What the scope actually changes

`WVA_SCOPE` controls the controller's **cache**, via `--watch-namespace`:

| scope | `--watch-namespace` | consequence |
| --- | --- | --- |
| `cluster` | unset | reads Deployments, pods, InferencePools and nodes in **any** namespace |
| `namespace` | its own (`POD_NAMESPACE`) | reads only **its own namespace** |

The scopes differ in who can install them as well as in what is read. `cluster`
creates 4 ClusterRoles and 4 ClusterRoleBindings and needs a cluster admin;
`namespace` **on Kubernetes** creates none — only Roles and RoleBindings in its
own namespace — so a namespace admin can install it themselves.

**On OpenShift, namespace scope still needs a cluster admin.** The overlay there
creates 3 ClusterRoles and 5 ClusterRoleBindings: the platform's monitoring
wiring is cluster-scoped (`cluster-monitoring-view`, so the controller can query
Thanos and user-workload Prometheus can scrape it), and without it the controller
cannot reach Prometheus at all. A namespace-scoped install on Kubernetes carries
cluster-scoped RBAC too, for the metrics authn filter, EPP metrics and the node
read `gpu-inventory` needs.

None of that stops a namespace admin owning the controller: an admin creates
those once with `INSTALL_PHASE=prereqs`, and the controller phase needs none of
them.

Whichever combination you have, `./deploy/install.sh --check` answers it for your
install specifically: it renders the overlay this install would apply and asks
whether you may create each kind in it, rather than assuming from the scope name.

Either way the grant is read-only: WVA never writes to the cluster, because KEDA
performs the actuation. The one genuinely cluster-scoped read is **nodes**, used to
resolve each variant's accelerator, which is why the `gpu-inventory` limiter needs
the node-reader ClusterRole the prereqs phase creates — see
[GPU limiter](gpu-limiter.md#permission-nodes).

> **The constraint that follows, and it is easy to get wrong:** a namespace-scoped
> WVA can only manage model servers **in its own namespace**. Installing one into
> `wva-system` while your models run in `llm-d-prod` gives you a controller that
> KEDA will call and that cannot read the workload it is being asked about. For a
> namespace-scoped install, either put the controller in the namespace with the
> models, or point it at that namespace with `WVA_WATCH_NS`.
>
> Cluster-scoped has no such constraint: models can be anywhere.

## Living beside what the cluster already has

**KEDA is never overwritten.** On `kubernetes` and `openshift` the install does not
Helm-install KEDA at all — it checks that `scaledobjects.keda.sh` exists and fails
with instructions if it does not. Your KEDA, its version and its configuration are
untouched. `KEDA_HELM_INSTALL=true` is the opt-in that would install one; even then
it skips when a working KEDA (CRD + running operator + metrics APIService) is
already there. `SCALER_BACKEND=none` skips the check entirely.

**Uninstalling WVA does not uninstall KEDA, Prometheus or EPP.** Removing WVA is the
job; removing what WVA was pointed at is a separate decision, and an explicit one —
`UNDEPLOY_SHARED=true`.

**A second WVA is refused.** Their workloads would be separate — a workload
registers with the scaler address its trigger names — but their GPU budgets would
not. See
[One WVA per cluster](../guides/install-cluster-wide/).

## Adding a model later

Deploy the model server, then re-run:

```bash
make scaledobjects-apply NAMESPACE=<your namespace>
```

It creates a ScaledObject for the **new** workload and leaves every existing one
alone — a workload that already has one is reported as such and skipped, so this is
safe to run as often as you like:

```text
[SUCCESS]   llm-d-sim/model-new (Deployment) -> ScaledObject model-new-wva (modelID: org/new-model)
[SUCCESS] Default ScaledObjects: 1 created, 1 not applied
```

Use `make scaledobjects-plan` first if you want to see the list before anything is
created. Nothing about the controller needs restarting or reconfiguring: it learns
about the workload from the first KEDA call.

## High availability

The controller elects a leader (`--leader-elect=true`, with tunable lease, renew and
retry durations), so more than one replica is safe — but the extra replicas are
**standbys**. Only the leader runs the collection and optimization loops; the others
wait on the lease.

```bash
make deploy-wva-on-k8s WVA_REPLICAS=2
```

What that buys is failover: a node drain or a crash costs you a lease timeout rather
than the time to reschedule a pod. What it does not buy is throughput — WVA's cycle
is one process reasoning about the whole fleet at once, deliberately, because a GPU
budget cannot be split across controllers that cannot see each other's decisions —
which is also why there is no supported way to run two.

## Advanced

| Variable | Description | Default |
|----------|-------------|---------|
| `SKIP_TLS_VERIFY` | Skip Prometheus TLS verification | `false`, forced to `true` on OpenShift and for in-cluster self-signed Prometheus |
| `WVA_LOG_LEVEL` | WVA logging level | `info` |
| `PROMETHEUS_SECRET_NAME` | Secret holding the Prometheus serving cert | `prometheus-web-tls` |
| `PROMETHEUS_SECRET_NS` | Namespace of that secret | `$MONITORING_NAMESPACE` |
| `PROM_CA_CERT_PATH` | Where the extracted Prometheus CA is written | `/tmp/prometheus-ca.crt` |
| `GATEWAY_API_VERSION` | Gateway API version installed for llm-d | `v1.2.0` |
| `LWS_NAMESPACE` | Namespace for LeaderWorkerSet installation | `lws-system` |
| `LWS_CHART_VERSION` | LeaderWorkerSet Helm chart version | `0.8.0` |

## Optional: scaling band after `make deploy-e2e-infra`

If `SCALE_UP_THRESHOLD` and/or `SCALE_DOWN_BOUNDARY` are set in the environment, the Makefile patches the `wva-scaling-policy-config` ConfigMap after install. Note the patch replaces the whole `default` entry, so it writes `analyzerName: saturation` alongside the band.

