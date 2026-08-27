# Install methods and platform notes

How the installer is invoked, for cases the [guides](../guides/) do not
cover: GitOps, a direct Kustomize apply, or reading exactly what the script does.

> The guides are the supported paths. This page is reference.

## Deployment methods

### Method 1: Automated Deployment Script (Recommended)

The deployment script provides a complete, automated setup including:

- WVA controller with RBAC configuration
- Prometheus stack (or connects to existing)
- llm-d infrastructure (Gateway, Scheduler, vLLM)
- KEDA for external metrics (ScaledObject-driven)
- ServiceMonitors for metric collection
- ScaledObjects are yours to create — they are how a workload reaches WVA
- Automatic GPU detection
- Environment-specific optimizations

#### Quick Start with Make

```bash
# Set required environment variable
export HF_TOKEN="hf_xxxxxxxxxxxxx"

# Deploy to Kubernetes
make deploy-wva

# Deploy to OpenShift
make deploy-wva-on-openshift

# Deploy to Kind (with emulated GPUs)
CREATE_CLUSTER=true make deploy-e2e-infra
```

#### Manual Script Execution

```bash
# Navigate to deploy directory
cd deploy

# Set environment variables
export HF_TOKEN="hf_xxxxxxxxxxxxx"
export ENVIRONMENT="kubernetes"  # or "openshift", "kind-emulator"

# Run deployment script
bash install.sh
```

#### Script Configuration Options

The script accepts both command-line flags and environment variables:

**Command-line flags** (`deploy/install.sh`):

```text
bash install.sh [OPTIONS]

Options:
  -i, --wva-image IMAGE    WVA container image (default: ghcr.io/llm-d/llm-d-workload-variant-autoscaler:latest)
  -c, --check              Run the prerequisite and permission checks, then exit
  -p, --phase PHASE        prereqs | wva | all (default: all) — see deploy/README.md
  -u, --undeploy           Undeploy WVA, monitoring, and scaler (not llm-d)
  -e, --environment ENV    kubernetes | openshift | kind-emulator
  -h, --help               Show help
```

`--check` is what `make check-prereqs` runs. Use it before a real install: it
verifies the tools, the cluster connection, and that you can create everything the
install produces — the last of which is the usual reason an install dies halfway.

**llm-d stack** (gateway, EPP, ModelService): deploy using the [llm-d guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline) directly. For EPP-only setup (llm-d-router-standalone chart + tokenreview RBAC), use `deploy/install-epp.sh` after `install.sh`.

**Environment variables**: every option the script reads is tabulated in
[Configuration Reference](configuration.md).

#### Script deployment examples

**ScaledObjects** are not created by `install.sh` — create them with `kubectl apply`, or let your tests/operators manage them. KEDA creates and owns the HPA behind each one; you never create an HPA yourself.

##### Example 1: Base WVA infra + EPP

```bash
./deploy/install.sh -e kubernetes
# EPP (llm-d-router-standalone chart + RBAC):
LLM_D_ROUTER_VERSION=v0.9.0 GAIE_VERSION=v1.5.0 NAMESPACE=llm-d-optimized-baseline \
  ./deploy/install-epp.sh
# Model server: follow llm-d/llm-d guides/optimized-baseline
```

##### Example 2: E2E-style stack (same as `make deploy-e2e-infra`)

```bash
make deploy-e2e-infra ENVIRONMENT=kind-emulator IMG=localhost/llm-d-workload-variant-autoscaler:dev
```

##### Example 3: WVA + monitoring only (no llm-d)

```bash
export DEPLOY_WVA=true
export DEPLOY_PROMETHEUS=true
export DEPLOY_OPERATIONAL_DASHBOARD=true
export SCALER_BACKEND=keda
./deploy/install.sh -e kubernetes
```

##### Example 4: Install with LeaderWorkerSet (for full e2e suite)

```bash
export DEPLOY_LWS=true
./deploy/install.sh -e kubernetes
```

### Method 2: Kustomize (GitOps and direct installs)

The overlays are plain Kustomize, so you can apply them yourself — for Argo CD or
Flux, or to review exactly what lands. Scope, namespace and limiter are the same
decisions as above; see [Choosing your install](../guides/).

**The overlays alone are not a complete install.** `deploy/install.sh` does four
things around them that the raw apply does not, and each one has bitten somebody:

| What the script does | If you skip it |
| --- | --- |
| Writes `PROMETHEUS_URL` into the controller ConfigMap | The controller starts, looks healthy, and reads no metrics — it uses the shipped in-cluster default, which is not your Prometheus |
| Renames the shared ClusterRoleBindings per install | A second install **takes the bindings from the first**, which silently loses all its permissions |
| Checks no incompatible WVA is already installed | Two controllers scaling the same workloads |
| Refuses to delete the namespace and shared ClusterRoles on undeploy | `kubectl delete -k` removes the **Namespace** — taking the model servers in it — and the shared **ClusterRoles**, breaking every other WVA on the cluster |

So use this method when you are installing **one** WVA and wiring Prometheus
yourself, or when a GitOps tool owns the manifests. Otherwise prefer Method 1.

#### Applying the overlays directly

```bash
# Set the controller image
cd config/base/manager
kustomize edit set image controller=ghcr.io/llm-d/llm-d-workload-variant-autoscaler:v0.7.0

# Apply the overlay for your scope and platform
kubectl apply -k ../../overlays/cluster-scoped/kubernetes
#             ../../overlays/namespace-scoped/kubernetes
#             ../../overlays/cluster-scoped/openshift
#             ../../overlays/namespace-scoped/openshift
```

Then point the controller at your Prometheus — without this it will not collect
anything:

```bash
# NOTE: the overlays hardcode `namespace: wva-system`. Only deploy/install.sh
# rewrites that to $WVA_NS, so a raw `apply -k` lands in wva-system.
kubectl -n wva-system edit configmap wva-manager-config
# under config.yaml, set:
#   prometheus:
#     baseURL: https://<your-prometheus>.<ns>.svc.cluster.local:9090
kubectl -n wva-system rollout restart deployment wva-controller-manager
```

If you will run **more than one** WVA on this cluster, rename the shared
ClusterRoleBindings in your overlay first — otherwise the second install takes them
from the first. `deploy/lib/common.sh:wva_append_crb_name_patches` is the patch set
the script generates; the names are listed in `WVA_SHARED_CLUSTER_ROLE_BINDINGS`.

#### Undeploy

`kubectl delete -k` is **not** the inverse of the apply: the overlay contains a
Namespace and the shared ClusterRoles, and deleting those takes the workloads in
that namespace and the permissions of every other WVA with it. Delete the
install's own objects and leave the shared ones:

```bash
kubectl kustomize config/overlays/cluster-scoped/kubernetes \
  | yq 'select(.kind != "Namespace" and .kind != "ClusterRole")' \
  | kubectl delete -f - --ignore-not-found
```

Or just use `./deploy/install.sh --undeploy`, which does exactly this.


## After the controller: warm pools

Installing WVA does not create a warm pool, and a namespace does not need one to
autoscale. A pool trades held accelerators for scale-up latency, so it is worth
it only where a model's load time actually costs something — see [Weights and the
model cache](operations.md#weights-and-the-model-cache) for why that load is not
a storage problem.

To see whether this namespace has models that could share one:

```bash
deploy/warmpool.sh plan -n <namespace>
```

`plan` reads only. It groups the namespace's models by what a pool would have to
provide — accelerator, and devices per replica, the two things a warm copy cannot
change — and prints a `create` command for each group it can serve, plus any pool
that is declared and unused or selected and missing.

The sizing arguments in what it prints are placeholders: they set the Pod memory
limit, which *is* the warm-set budget, and only you know how many models of what
size a pool must hold at once. `deploy/warmpool.sh sizing -n <namespace>` answers
that against this cluster's actual nodes.

Full reasoning, the configuration surface, and the removal path are in the [warm
pool guide](../guides/warm-pool/README.md).

## Platform-specific guides

For platform-specific instructions and considerations:

- **[Kubernetes Guide](../../deploy/kubernetes/)**: Detailed Kubernetes-specific instructions including kube-prometheus-stack setup, GPU operator installation, and ServiceMonitor configuration
- **[OpenShift Guide](../../deploy/openshift/)**: OpenShift-specific instructions including User Workload Monitoring (Thanos), Routes, Security Context Constraints (SCC), and GPU operator on OpenShift
- **[Kind Guide (Local Testing)](../../deploy/kind-emulator/)**: Local development and testing with Kind clusters and emulated GPUs

Each guide includes platform-specific examples, troubleshooting, and quick start commands. All guides use the same [Configuration Reference](configuration.md) documented below.

