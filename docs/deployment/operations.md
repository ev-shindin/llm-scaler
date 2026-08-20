# After the install

Verifying WVA works, watching what it decides, and the first things to check when it does not.

> Part of the [WVA deployment guide](../../deploy/).

## Verifying the install

Every command on this page uses `$NS` for the namespace the **controller** runs
in — not a fixed name, since a per-team or cluster-scoped install can be
anywhere.

Start by running:

```bash
make verify-deployment
```

It reports the namespace it found the controller in ("WVA controller is
running and ready in `<ns>`"), or — if there is more than one WVA on this
cluster, or none — lists every candidate and stops rather than guess; choose
yours from that list.

For a namespace-scoped install (the common case — the controller runs in the
same namespace it manages):

```bash
NS="${NAMESPACE:-}"
```

For a cluster-scoped install, or one installed with `WVA_NS` set explicitly:

```bash
NS="${WVA_NS:-}"
```

Confirm registration matches what is actually serving:

```bash
make verify-scaledobjects
```

For more detail, straight from the controller and KEDA:

```bash
kubectl get pods -n "$NS"                     # the controller is Running
kubectl get scaledobject -n "$NAMESPACE"      # your managed workloads
kubectl get hpa -n "$NAMESPACE"               # KEDA created one per ScaledObject
```

A ScaledObject with a KEDA HPA whose `CurrentMetrics` is populated means the whole
chain works: WVA was called, decided, and KEDA received the answer. An empty
`CurrentMetrics` means KEDA never got one — check the trigger's `scalerAddress`
and that `modelID` is set.

If metrics are the problem, follow them forward:

```bash
# 1. the model server exposes them
kubectl port-forward -n <llm-namespace> <vllm-pod> 8000:8000
curl -s http://localhost:8000/metrics | grep vllm:

# 2. Prometheus scrapes them  (query vllm:num_requests_running)
kubectl port-forward -n <monitoring-namespace> svc/kube-prometheus-stack-prometheus 9090:9090

# 3. WVA reads them and decides
kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler   | grep -E "Collected replica metrics|scaling-decision"
```

## Watching what WVA decides

WVA writes no custom resource. Its decisions are visible in three places, and you
want them in this order: the **dashboard** for whether things are healthy, the
**metrics** for a specific question, the **logs** only for why a single decision
came out the way it did.

### The operational dashboard

The install publishes a Grafana dashboard, *WVA Operational Dashboard*, covering
the whole pipeline: GPU discovery, metric collection health and freshness,
saturation, capacity, scaling decisions and limiter impact.

```bash
kubectl port-forward -n <monitoring-namespace> svc/kube-prometheus-stack-grafana 3000:80
# then http://localhost:3000  (default admin password: prom-operator)
```

It ships as a labelled ConfigMap, so **an existing Grafana picks it up** — you do
not need to have let this install deploy Prometheus — *provided you publish into a
namespace that Grafana's sidecar actually watches*:

```bash
# publish into whichever namespace your Grafana's sidecar watches
DASHBOARD_NS=my-monitoring ./deploy/install.sh   # DEPLOY_PROMETHEUS=false is fine
```

A sidecar watches only the namespaces it is configured for — `ALL` for a Grafana
this installer deployed, often just its own for one you already had — so a wrong
`DASHBOARD_NS` can produce a ConfigMap nobody reads. The install checks and warns;
see [If you cannot write to the monitoring namespace](#if-you-cannot-write-to-the-monitoring-namespace).

Skip it entirely with `DEPLOY_OPERATIONAL_DASHBOARD=false`.

Read the panels top-down. The upper row answers "is WVA seeing the cluster at all";
until those are healthy, the scaling panels below them are meaningless.

#### Who owns the dashboard

The dashboard is published as a ConfigMap into a **monitoring** namespace, which
is normally not the namespace WVA runs in — so publishing the shared one is a
**cluster-admin** action even though it happens during the tenant install step.

| | can do |
| --- | --- |
| Cluster admin | publish and update the shared dashboard in the monitoring namespace; decide the datasource, and therefore who sees what |
| Namespace admin | use the shared dashboard through their `?var-namespace=` link; or import the JSON into Grafana by hand; or publish into a namespace **their Grafana already watches** — see the caveat below |

A namespace admin running `make deploy-wva` without rights to the monitoring
namespace gets a message saying so — the install continues, and only the
dashboard step is skipped. Nothing about WVA's scaling depends on it.

#### If you cannot write to the monitoring namespace

`DASHBOARD_NS=<your-namespace>` publishes a private copy into your own namespace,
which needs no cluster rights at all. **Whether it renders depends on the Grafana**,
so the installer checks and tells you:

- **A Grafana this installer deployed watches every namespace.** The bundled chart
  (grafana 12.10.4 via kube-prometheus-stack) sets the dashboard sidecar's
  `searchNamespace` to `ALL`, verified on a live install. So `DASHBOARD_NS` simply
  works, and the install says so.
- **A Grafana you already had may watch only its own namespace.** That is the
  sidecar's other common configuration, and there is no error when it applies: the
  ConfigMap is created, is correctly labelled, and is silently never read. The
  install detects this and warns rather than reporting success.

Ways out, cheapest first:

1. **Import the JSON by hand.** `deploy/grafana/operational-dashboard.json` is an
   ordinary Grafana dashboard. Anyone with Grafana editor rights can import it
   through the UI — *Dashboards → New → Import* — with **no Kubernetes permission
   at all**. This is the fastest self-service route and the right answer for most
   namespace admins.
2. **Use the shared dashboard, scoped to you.** If an admin has already published
   it, you need nothing: open the link the install prints,
   `…/d/wva-operational/wva-operational-dashboard?var-namespace=<your-namespace>`.
3. **Ask for the sidecar to watch all namespaces** — only needed for a
   pre-existing Grafana that does not already. One change,
   `grafana.sidecar.dashboards.searchNamespace=ALL` or an explicit list, after which
   `DASHBOARD_NS=<your-namespace>` works for every tenant, self-service.
4. **Run your own Grafana in your namespace**, then publish to it with
   `DASHBOARD_NS=<your-namespace>`. Fully self-service and needs no admin, at the
   cost of operating another Grafana.

On OpenShift, note that user-workload monitoring ships **no Grafana** — so options
1 and 4 are the practical ones there unless your cluster runs its own.

#### One dashboard, many installs

The ConfigMap has a **fixed name in whatever namespace you publish it to**, so
every install on a cluster writes the same object. That is deliberate — the
dashboard is generic and driven by variables, and one copy per tenant would fill
the picker with identical dashboards — but it has consequences worth knowing.

**The first row is the admin's view.** *Installs present* counts WVA Deployments
from kube-state-metrics; *Controllers reporting metrics* counts the ones whose
metrics actually arrive. A gap between them is an install nobody is scraping,
and it is the difference that matters: a controller that is not scraped looks
exactly like a controller that is not scaling. *Scrape targets DOWN* names how
many, and the **Controller** variable says which.

**Your namespace's view is a link, not a default.** A Grafana variable's default
lives inside the dashboard JSON, and this object is shared by every install on
the cluster — so there is no per-tenant default to set: pinning it would mean
everyone sees whichever tenant installed last. Each namespace gets its own entry
point into the one dashboard instead, which the install prints:

```text
<your-grafana>/d/wva-operational/wva-operational-dashboard?var-namespace=<your-namespace>
```

Drop the query string for the cluster-wide view. If you would rather have your
own dashboard object — pinned, private, nobody else writing it — publish a copy
into your own namespace:

```bash
DASHBOARD_NS=<your-namespace> make deploy-wva   # pinned to that namespace
```

This renders whenever the Grafana's sidecar watches that namespace, which is the
case for one this installer deployed (`searchNamespace: ALL`). The install verifies
it and warns if not, in which case import the JSON by hand instead.

Note the trade-off: the shared object is the default on purpose. One generic,
variable-driven dashboard serves every install, whereas a copy per tenant fills the
picker with near-identical entries.

**Namespace label:** `wva_*` metrics carry both `namespace` and
`exported_namespace`. `exported_namespace` is the *workload's* namespace and
`namespace` is the *controller's* — the same for a namespace-scoped install,
different for a cluster-scoped one, where grouping by `namespace` would collapse
every workload onto the controller's namespace. The dashboard defaults to
`exported_namespace` for that reason; the benchmark dashboard defaults to
`namespace`, because vLLM metrics carry only that one.

**Versions.** The ConfigMap records the WVA version that published it, and an
older install will not overwrite a newer dashboard — it says so and leaves it.
Panels for metrics a given version does not emit stay empty for that install:
on a cluster running several versions, an empty panel may mean "older
controller", not "nothing happening". Force a republish with:

```bash
kubectl delete configmap wva-operation-dashboard -n <dashboard-namespace>
```

**Who can see what is the datasource's decision, not WVA's.** On OpenShift,
`thanos-querier:9091` is cluster-monitoring-view — anything querying through it
reads every namespace, whatever scope WVA was installed with. For a tenant
Grafana that must only see its own namespace, point the datasource at
`thanos-querier:9092`, which enforces per-namespace RBAC.

That is also what makes the shared dashboard safe rather than merely tidy: with
a per-namespace datasource the **Namespace** variable only lists namespaces the
viewer may read, so "All" already means "all of mine". Pinning a default never
provided isolation — it only hid names from the dropdown while every query still
ran with the datasource's own permissions.

### The metrics that answer specific questions

All are exposed by the controller and scraped by the ServiceMonitor the install
creates. Full list: [Prometheus metrics](../developer-guide/prometheus.md).

**Is WVA working at all?**

| metric | healthy | what it means when it is not |
| --- | --- | --- |
| `wva_models_processed` | > 0 | no workload has registered — no ScaledObject names this scaler |
| `wva_metrics_pods_discovered` | > 0 per model | WVA cannot find the pods behind a model |
| `wva_metrics_freshness_status{status="fresh"}` | equals the pod count | pods sitting in `status="stale"` or `"missing"` are being decided on with old data, or none at all |
| `wva_errors_total` | flat | rising means the optimization cycle is failing |

`wva_metrics_freshness_status` is a per-`(variant_name, status)` gauge holding *how
many pods* are in that state — not a 0/1 flag. Compare the series:

```promql
wva_metrics_freshness_status{status!="fresh"} > 0
```

**Why is nothing scaling up?** — the two silent-stall causes, both of which look
like "WVA is fine" everywhere else:

| metric | meaning |
| --- | --- |
| `wva_node_access_denied` | `1` = a GPU limiter is configured but nodes are unreadable. Every variant gets no budget and **will not scale up**. |
| `wva_decisions_limited_total` | rising = a limiter is capping decisions. Pair with `wva_available_gpus` and `wva_spare_capacity`. |
| `wva_unattributed_gpus` | GPUs in use that could not be charged to a pool — usually a workload whose accelerator did not resolve |

**Is the decision itself sane?** — `wva_desired_replicas` vs `wva_current_replicas`,
with `wva_saturation_utilization` and `wva_analyzer_demand` / `wva_analyzer_target`
showing what drove it. A desired that never becomes current is an actuation
problem (KEDA, the HPA, or the workload), not a decision problem.

```bash
# read them straight off the controller
kubectl port-forward -n $NS svc/wva-controller-manager-metrics-service 8443:8443
curl -sk https://localhost:8443/metrics | grep -E '^wva_'
```

### The logs

Useful when a metric tells you *which* model is wrong and you want to know *why*.

| grep for | tells you | level |
| --- | --- | --- |
| `scaling-decision` | what WVA decided for a model, and the replica counts | Info |
| `Effective scaling policy` | which policy tier a model resolved to | Info |
| `GPU limiter (re)built from config` | a `limiters:` edit took effect, live | Info |
| `Collected replica metrics` | metrics are arriving | **`-v=4`** |

The controller runs at `-v=2` by default, so `Collected replica metrics` prints
nothing and grepping for it proves nothing either way. Use
`wva_metrics_pods_discovered` and `wva_metrics_freshness_status` for that question
instead — they are always on. If you do want the line, raise verbosity on the
container and put it back afterwards:

```bash
kubectl patch deployment -n $NS wva-controller-manager --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--v=4"}]'
```

```bash
kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler -f
```

WVA writes no custom resource, so its decisions are visible only in these logs, in
the metrics it publishes ([Prometheus metrics](../developer-guide/prometheus.md)),
and in the HPA state KEDA derives from them.

## Testing autoscaling

One command, against what you already installed:

```bash
make benchmark-smoke NAMESPACE=<namespace>
```

Decode-heavy load at 10 req/s for five minutes, then a snapshot of the dashboard
over exactly that window. It checks the whole chain first — KEDA, a WVA
controller that manages *this* namespace, model servers, an EPP, a ScaledObject
— and reports every gap at once rather than one per five-minute run. It stands
nothing up, so it is safe against a live namespace, and it needs no benchmark
CLI. The snapshot is written only when there is a dashboard to snapshot.

For runs whose numbers you intend to compare, use
[Benchmark WVA](../guides/benchmarking/). Full procedures, including the
simulator and the e2e suites, are in [Testing](../developer-guide/testing.md).

## First-line troubleshooting

| symptom | most likely cause | check |
| --- | --- | --- |
| WVA pod not `Running` | image pull, resources, or Prometheus unreachable | `make verify-deployment`, then `kubectl describe pod -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler` |
| "Metrics unavailable" in the logs | the ServiceMonitor does not select your model pods, so the series never reach Prometheus | `make verify-deployment`, then `kubectl get servicemonitor -A` and Prometheus `/targets` |
| HPA exists but `CurrentMetrics` is empty | KEDA never got an answer — usually the trigger's `scalerAddress` or a missing `modelID` | `kubectl describe hpa -n <ns> keda-hpa-<so-name>` |
| nothing scales, no errors | a limiter is declared and the workload's accelerator does not resolve, so it gets no GPU budget | `kubectl logs -n $NS -l app.kubernetes.io/name=workload-variant-autoscaler \| grep -i accelerator` |
| a model never wakes from zero | the EPP flow-control queue is not reaching WVA | see [Troubleshooting](../developer-guide/troubleshooting.md) |
| `READY False` on the ScaledObject, and the HPA's `TARGETS` reads `cpu: <unknown>/80%` | KEDA could not fetch the metric spec from WVA, so it fell back to a CPU metric. The trigger names a scaler it cannot reach — most often a `scalerAddress` naming a **different install's namespace** than the controller actually runs in. `make scaledobjects-repoint` fixes exactly this: it rewrites `scalerAddress` on objects that ask for WVA but name a namespace where no scaler runs, and leaves one pointing at a second live install alone | `kubectl get scaledobject -A -o custom-columns=NAME:.metadata.name,ADDR:.spec.triggers[0].metadata.scalerAddress` then `kubectl get svc -A \| grep external-scaler` |
| demand looks far too low for the load you are driving, and `has N ready pod(s) but none attributed` appears each cycle | FMA is in the namespace and nothing is scraping its launcher pods, so the traffic they serve is invisible | `make verify-fma`, see also [FMA launcher pods](#fma-launcher-pods) |
| WVA applies no decisions for a workload, silently, though the HPA reads a healthy ratio the whole time | the ScaledObject's `modelID` no longer matches what the container actually serves — a hand-changed model that nothing re-syncs | `make verify-scaledobjects` |
| `check-prereqs`/`deploy-wva` refuses: "No llm-d found in `<ns>`" | the namespace holds no llm-d model servers yet | finish the llm-d install in `<ns>`, or `SKIP_CHECKS=true` |
| refuses: "Monitoring not enabled for your llm-d servers; please enable Model-server metrics" | nothing scrapes the model servers — WVA would hold every workload at `minReplicas` and say so only in its log | `kubectl apply -n <ns> -k config/modelserver-metrics` (or llm-d's own `guides/recipes/modelserver/components/monitoring`) |
| refuses: "Router monitoring not enabled; please enable EPP metrics" | nothing scrapes the EPP — the throughput analyzer has no arrival-rate signal (`inference_extension_scheduler_attempts_total` reads 0) | `kubectl apply -n <ns> -k $REPO_ROOT/guides/recipes/observability` |
| refuses: "Router queue not enabled; please turn on flow control for EPP" | the `flowControl` feature gate is off — scale-from-zero and `wva_unmeasured_queue` have no queue-depth signal to read | add `featureGates: [flowControl]` to the EPP's `EndpointPickerConfig`; llm-d ships a ready values file at `guides/workload-autoscaling/keda-epp-queue/<guide>/router.values.yaml` |

### `no children to pick from` after a reinstall

KEDA's gRPC client backs off on a name that did not resolve, and keeps backing
off for far longer than an uninstall/reinstall takes. So a ScaledObject that
outlived a WVA uninstall can stay `READY False` against a scaler that is now
running perfectly — the name was NXDOMAIN while WVA was gone, and KEDA has not
re-resolved it yet.

```bash
kubectl rollout restart deploy/keda-operator -n keda
```

Verified on kind: the ScaledObjects went `READY True` within a poll interval of
the restart, with nothing else changed. Deleting the ScaledObjects before
uninstalling avoids it — which `make undeploy-wva` now does for the ones it
created.

### FMA launcher pods

A namespace running Fast Model Actuation needs two things WVA does not do by
default, and both fail silently when missing:

- **the launcher pods must be scraped.** They declare no container ports, so a
  PodMonitor selecting by port name generates no target for them at all — not a
  failing target, no target. Fix with
  `kubectl apply -k config/fma-launcher-metrics -n <ns>`.
- **the plan must target the requester**, not a decode Deployment, when the
  requester is the only serving workload there.

Symptoms: demand far lower than the load you are driving, `has N ready pod(s)
but none attributed` once per cycle, or a variant flat at `minReplicaCount`
while the queue grows.

The whole story — how attribution works, how to size `maxReplicas` from the
launcher pool, why GPU accounting is a lower bound, and what to check — is in
[WVA with Fast Model Actuation](../guides/fma/).

First stop for any of these:

```bash
kubectl logs -n "$NS"   -l app.kubernetes.io/name=workload-variant-autoscaler --tail=200
```

Deeper diagnosis — EPP metrics, scale-from-zero, slow scale-up — is in
[Troubleshooting](../developer-guide/troubleshooting.md).

## Command cheatsheet

```bash
# === WVA Controller ===
kubectl get pods -n "$NS"
kubectl logs -n "$NS" -l app.kubernetes.io/name=workload-variant-autoscaler -f
kubectl describe deployment wva-controller-manager -n "$NS"

# === Managed workloads (a ScaledObject IS the registration) ===
kubectl get scaledobject -A
kubectl describe scaledobject <name> -n <namespace>

# === Metrics and Monitoring ===
kubectl get servicemonitor -A
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1" | jq
kubectl port-forward -n <monitoring-namespace> svc/kube-prometheus-stack-prometheus 9090:9090

# === ScaledObjects / HPA ===
kubectl get scaledobject -A
kubectl describe scaledobject <name> -n <namespace>
kubectl get hpa -A
kubectl describe hpa <name> -n <namespace>

# === KEDA ===
kubectl get pods -n keda-system
kubectl logs -n keda-system -l app=keda-operator

# === vLLM / Application ===
kubectl get pods -n <app-namespace>
kubectl logs -n <app-namespace> <vllm-pod>
kubectl port-forward -n <app-namespace> <vllm-pod> 8000:8000

# === Configuration ===
kubectl get configmap -n "$NS"
kubectl get configmap wva-manager-config -n "$NS" -o yaml          # Prometheus URL, intervals
kubectl get configmap wva-scaling-policy-config -n "$NS" -o yaml   # thresholds, tiers, limiters
```

