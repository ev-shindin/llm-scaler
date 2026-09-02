# Watching what WVA decides

The dashboard, the metrics that answer specific questions, and how to read the
logs. If WVA is installed but you cannot tell what it is doing, this is the page.

> Part of the [WVA deployment guide](../../deploy/). For whether the install
> worked at all, see [After the install](operations.md).

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

### How the dashboard is organised

The panels sit in three rows, and the row says what a panel is scoped to:

| row | scope | what it answers |
| --- | --- | --- |
| **Fleet health — all namespaces** | every namespace WVA manages | Is WVA itself working? Installs present, controllers reporting, scrape targets down, collection and optimization timings. |
| **Selected namespace: `$namespace`** | the **Namespace** variable | What did WVA do here? Replicas, capacity, saturation, scaling decisions, limiters, wake-from-zero. |
| **Serving** (collapsed) | the **Namespace** variable | What the workload experienced: TTFT, inter-token latency, router and per-replica queues, KV utilization. Mostly from the EPP, so it reads the same for vLLM and SGLang. |

The split exists because the two audiences differ. A panel in the fleet row does
not change when you pick a namespace, and one in either lower row does — before
the rows, that was invisible, and a "Models blocked from scaling" stat counting
the whole cluster sat beside a chart of the same thing counting one namespace.
`make dashboards-check` fails if a panel's query disagrees with the row it sits
under.

### When the dashboard shows nothing

- **No Grafana at all** — `services "kube-prometheus-stack-grafana" not found`
  means the install ran with `DEPLOY_OPERATIONAL_DASHBOARD=false`, or against a
  cluster whose Grafana it did not deploy. Point the port-forward at your own.
- **Panels render but say No Data** — hover the panel's `i` icon first; several
  say which metric is missing and why. Then test the Prometheus datasource under
  *Connections / Data sources*.
- **The dashboard is not in the picker** — it lives in the `wva-operation-dashboard`
  ConfigMap. If that object exists in a namespace your Grafana's sidecar does not
  watch, nothing reads it: see [If you cannot write to the monitoring
  namespace](#if-you-cannot-write-to-the-monitoring-namespace).
- **Every variant shows the controller's namespace** — toggle the
  `$namespace_label` variable at the top of the dashboard between
  `exported_namespace` (the default, correct when Prometheus scrapes with
  `honorLabels: false`) and `namespace`.
- **You want an editable copy** — the shipped dashboard is read-only. Import
  `deploy/grafana/operational-dashboard.json` under a name of your own, or
  publish a private copy with `DASHBOARD_NS=<your-namespace>`.

### The metrics that answer specific questions

All are exposed by the controller and scraped by the ServiceMonitor the install
creates. Full list: [Prometheus metrics](prometheus.md).

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
the metrics it publishes ([Prometheus metrics](prometheus.md)),
and in the HPA state KEDA derives from them.
