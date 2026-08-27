# After the install

Verifying WVA works, and the first things to check when it does not.

> Part of the [WVA deployment guide](../../deploy/).

This page is the operational entry point. Two longer subjects have their own:
>
> - **[Watching what WVA decides](monitoring.md)** -- the dashboard, the metrics
>   that answer specific questions, and reading the logs.
> - **[Preparing a workload to be scaled](workload-preparation.md)** -- the model
>   cache, draining before scale-down, and `make workload-patch`.

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
[the FMA post-mortem](../proposals/fma-post-mortem.md).

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
