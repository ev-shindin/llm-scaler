# Installing and creating a pool

[← Warm pool guide](README.md)

## Prerequisites

A model already serving under WVA — follow
[Install WVA in a namespace](../install-in-namespace/) first.

The pool needs, in its namespace:

- Accelerators free for the pool Pods themselves, on the same GPU model as the
  workloads it will warm.
- A shared model cache the pool Pods can read, so a warm copy loads from local
  storage rather than a download. **This one is not optional, and it has to be
  the SAME claim the model servers read**: every pool Pod mounts a claim named
  `model-pvc` unless `--cache-claim NAME` says otherwise.

  `create` warns when the claim you name is not one the namespace's GPU
  workloads mount — which is the mistake worth catching, because a plausible
  wrong claim exists on most clusters. Where the models mount `model-pvc` *at
  path* `/model-cache`, a `model-cache` claim holding the Hugging Face cache
  usually exists too; pick that one and the Pod mounts something real, at the
  right path, containing no models. The engine is then started on a `--model`
  path that is not in the Pod, never answers, and the controller waits out its
  full admission window before reporting only that the port did not respond.

  It cannot check that the claim EXISTS, and it says nothing when no GPU
  workload is deployed yet. Without one `create` still reports SUCCESS, and the
  Pods sit `Pending` on

  ```
  0/3 nodes are available: persistentvolumeclaim "model-pvc" not found
  ```

  while the pool reports itself empty. Create one first, in the pool's
  namespace, with an RWX class:

  ```bash
  make model-cache NAMESPACE=<namespace> WVA_MODEL_PVC_SIZE=<size> WVA_MODEL_PVC_CLASS=<rwx-class>
  ```

  Run it with neither size nor class and it lists the RWX classes this cluster
  already serves from.
- RBAC allowing WVA to `patch` Pods. The shipped ClusterRole has this. If yours
  was scoped by hand, WVA refuses to start the pool and says so — it will not
  hold accelerators to warm models it could never lend.
- **Two container images, from two owners.** See below.

### The images a pool Pod runs

Each pool Pod runs two containers, and only one of them is built here:

| container | image | owner |
| --- | --- | --- |
| `inference-server` (pool of Pods) | `ghcr.io/llm-d-incubation/llm-d-fast-model-actuation/launcher` | Fast Model Actuation |
| `inference-server` (pool of **groups**) | `ghcr.io/ev-shindin/fma-launcher:v0.6.4-headless` — `--launcher-image` is required, and `create` refuses without it | **our fork**, [ev-shindin/llm-d-fast-model-actuation](https://github.com/ev-shindin/llm-d-fast-model-actuation) — see [pools of groups](groups.md#a-group-needs-a-patched-supervisor-image) |
| `proxy` | the image `config/warmpool` pins, or `--proxy-image` | this repo (`make docker-build-warmpool-proxy`) |

Everything below is about the **stock** launcher, which a pool of Pods runs. A
pool of groups needs the fork instead: the stock one cannot start a multi-node
follower at all.

The stock launcher image is **not** ours and is not optional. It is a full vLLM
runtime — vLLM, torch and CUDA — with FMA's launcher on top, and the pool needs
both halves of that:

- **vLLM has to be in the Pod.** A warm copy is an engine already loaded on this
  Pod's GPU. There is nothing to warm unless the engine runs here.
- **The launcher is the API WVA drives.** It serves `/v2/vllm/instances` on
  :8001, which is how the controller creates a model in a Pod, lists what is
  resident, and removes one. Without it a pool Pod holds accelerators and
  answers nothing — the controller reports the pool EMPTY while its GPUs are
  gone, which is exactly what happened when a hand-written manifest lost this
  container.

So a pool node must be able to pull from `ghcr.io/llm-d-incubation` — or, for a
pool of groups, from `ghcr.io/ev-shindin`. On an air-gapped or mirrored registry,
mirror whichever applies: it is easy to miss, because neither is built by this
repo, and the stock one is not even named in a registry we control.

Substituting your own is possible in principle: the launcher's source is
vendored at `warmpool/supervisor/` and any image offering the same instance API
over vLLM would do. FMA's is used because it is the one proven on this cluster.
Note this repo does not build that image, so the vendored source is there to be
read, not to be shipped — if FMA publishes a newer launcher, the copy here can
be a different version from the one actually running.

This is the live FMA dependency the [FMA post-mortem](../../proposals/fma-post-mortem.md)
warns about: FMA was dropped as an *actuation strategy*, and its launcher is
still what supervises every warm engine.

## Turning it on during installation

The installer can create pools for you, as part of the same pass that creates
ScaledObjects. It never does so by default: a pool holds its accelerators from
the moment it exists, which is a cost decision, so it acts only on an explicit
yes.

```bash
WVA_DEFAULT_SO=edit deploy/install.sh
```

`WVA_DEFAULT_SO=edit` discovers what is running, writes a plan, and opens it in
`$EDITOR`. Near the bottom is a `warmPools:` section — one suggestion per group
of models that could share a pool, every one of them `apply: no`:

```yaml
warmPools:
  - namespace: my-models
    name: h100
    accelerator: NVIDIA-H100-80GB-HBM3
    gpus: 1
    models: 4
    modelSize: 8B
    replicas: 2
    max: 6
    reserve: 1
    apply: no        # <- change to yes for the pools you want
```

Change the ones you want to `yes` and save. Pools are created **after** the
ScaledObjects, because a pool nothing can borrow from is accelerators held for
nothing.

Three values the plan cannot know, and where they come from:

| value | where it comes from | if it is missing |
| --- | --- | --- |
| `WARMPOOL_PROXY_IMG` | optional — defaults to the published image `config/warmpool` pins. Build your own with `make docker-build-warmpool-proxy docker-push-warmpool-proxy` | nothing: the default is used, and the installer says which |
| `MONITORING_NAMESPACE` | the install already has it — where it put the monitoring stack | the pool is created **unscraped**: it works, and every model it lends to reads as having less demand than it has |
| `WARMPOOL_RUNTIME_CLASS` | usually nothing — `create` adopts whatever the namespace's own GPU workloads use | nothing: most clusters need no RuntimeClass at all |

`WVA_DEFAULT_SO=plan` writes the plan and stops, if you would rather edit it and
apply later with `WVA_DEFAULT_SO_PLAN=<file>`.

## Creating a pool by hand

The two objects a pool is made of — the Deployment and its ScaledObject — are
only meaningful together, so there is one command that makes both:

```bash
deploy/warmpool.sh create -n <namespace> --name <pool>   --accelerator NVIDIA-H100-80GB-HBM3 --gpus 1   --models 4 --model-size 8B   --wva-namespace <where WVA runs>
```

`--models` and `--model-size` are how you size the Pod: they set the memory
limit, which **is** the warm-set budget. Pass `--dry-run` to see the manifests
without applying, and use `delete` to remove both objects together — removing
only one leaves either accelerators nobody can borrow, or a trigger pointing at
nothing.

`create` also applies the ingress boundary, because the only cluster-specific
value it needs is the WVA namespace and that is already a flag. It admits
`:8000` from this namespace and `:8001`, `:8002` and the engine range from WVA
alone. Pass `--no-network-policy` if your cluster manages policy centrally --
but not for convenience: without one, `:8001` accepts caller-supplied argv and
environment from anything that can reach the Pod IP, in a container that mounts
the shared model cache read-write.

If the pool later reports itself **empty** while holding accelerators, suspect
this first: a policy naming the wrong WVA namespace denies the supervisor read,
and the result is indistinguishable from a pool that is merely too small. Look
for `warm pool Pod could not be read` in the controller log.

It deliberately does not guess your accelerator: omit `--accelerator` and the
Pods may schedule on any GPU node, at which point WVA declines every model whose
accelerator it can prove differs.

The rest of this section is the manual path, for when you want to edit the
manifests directly.

### 1. Nothing to point at the pool

There is no step here, and that is the design. A namespace-scoped controller
warms the namespace it watches; a cluster-scoped one acts on pools wherever a
ScaledObject trigger declares them. Neither needs telling, and neither needs a
restart when a pool appears.

### 2. Edit the four things the manifests cannot know

`config/warmpool` is a template, not a working install. Each of these is
cluster-specific, and three fail in a way that looks like something else:

| in | what to set | if you skip it |
| --- | --- | --- |
| `warmpool-networkpolicy.yaml` | the `>>> EDIT THIS <<<` `namespaceSelector` — the namespace WVA runs in | every read fails; the pool reports itself **empty** while holding accelerators |
| `warmpool-scaledobject.yaml` | the `>>> EDIT THIS <<<` in `scalerAddress` | applies cleanly, KEDA creates the HPA, the address never resolves, and WVA never learns the pool exists |
| `warmpool-deployment.yaml` | the proxy `image` — build your own with `make docker-build-warmpool-proxy` | `ImagePullBackOff`; the shipped digest is a personal registry namespace |
| `warmpool-deployment.yaml` | `runtimeClassName`, and the `claimName` of your model cache | admission fails outright; or the second replica sits Pending forever if the claim is not **ReadWriteMany** |

The `scalerAddress` one is the quiet one: the placeholder is a legal YAML
string, so nothing rejects it and nothing warns.

### 3. Deploy the pool

```bash
kubectl apply -k config/warmpool -n <namespace>
```

`deploy/warmpool.sh create` renders and applies the same two objects, which is
why the edits above do not arise on that path: it takes them as flags.

## Adding and removing a pool later

Pools are ordinary day-2 objects. Nothing about adding or removing one needs the
controller restarted: it discovers pools from the ScaledObjects that declare
them, on its next pass.

**What exists now**

```bash
deploy/warmpool.sh plan -n <namespace>       # what this namespace wants, from its triggers,
                                            # and any pool workload nothing declares
kubectl get deploy,scaledobject,networkpolicy,podmonitor -n <namespace> -l app.kubernetes.io/component=warm-pool
```

**Add one**

```bash
deploy/warmpool.sh create -n <namespace> --name <pool>   --accelerator NVIDIA-H100-80GB-HBM3 --gpus 1   --models 4 --model-size 8B   --wva-namespace <where WVA runs>   --monitoring-namespace <where Prometheus runs>
```

Then point models at it, in each one's ScaledObject trigger metadata:

```yaml
warmPool: <pool>       # which pool this variant may borrow from
warmPoolCopies: "1"    # optional: how many copies of THIS model to keep warm
```

A model with no `warmPool` uses the namespace's only pool, if there is exactly
one. Adding or removing that line is the whole of joining or leaving a pool —
there is nothing to restart, and the next reconcile acts on it.

**Add scraping to a pool that already exists**

```bash
deploy/warmpool.sh monitor -n <namespace> --name <pool> --monitoring-namespace <where Prometheus runs>
```

`create` does this for you. `monitor` is for pools that predate it, or that were
applied from `config/warmpool`, which ships no PodMonitor because a static
manifest cannot know where Prometheus runs. It adds the PodMonitor and admits the
scraper to the serving port, and it is safe to re-run: an already-admitted
namespace is left alone.

**Remove one**

```bash
deploy/warmpool.sh delete -n <namespace> --name <pool>
```

That removes all four objects together — ScaledObject first so WVA stops lending
Pods that are about to disappear, then the workload, then the NetworkPolicy and
PodMonitor. Pass `--dry-run` to see what it would remove.

Remove the `warmPool:` line from any model still naming it, or WVA will report a
variant pointed at a pool that does not exist. The models keep serving either
way; what they lose is the bridge, so their next scale-up pays a full cold start.

> **Do not delete only the ScaledObject.** It is what *declares* the pool. What
> is left is a Deployment holding accelerators that WVA reports as undeclared and
> will never use again. To pin a pool's size instead, set `minReplicaCount` equal
> to `maxReplicaCount` and leave the ScaledObject in place.

**Resize one**

Re-run `create` with different `--replicas`/`--max`/`--reserve`/`--type`/
`--max-hold`; it is an `apply`, so the objects are updated in place. `--type`
and `--max-hold` are picked up live — both engines rebuild from the ConfigMap
without a restart — so a bridge can become retained, and back, on a running
pool. Changing `--models`/`--model-size`
changes the Pod's memory limit, which **rolls the pool** and reloads every
resident model — cheap to say and expensive to do, so decide the warm-set budget
before you fill it.

Changing the pool's KIND — a single-Pod pool to a group pool or back — is
refused, because `kubectl apply` would leave the old workload running and holding
its GPUs where nothing would look for it again. Delete the pool first.

## The pool is its ScaledObject

A warm pool is declared by a KEDA trigger, the same way every other thing WVA
knows about is. `warmPoolName` is what makes it a pool; the Deployment beside it
only supplies the Pods.

```yaml
spec:
  minReplicaCount: 2
  maxReplicaCount: 6              # must EXCEED the reserve
  triggers:
    - type: external-push
      metadata:
        scalerAddress: wva-external-scaler.<wva-namespace>.svc.cluster.local:9090
        warmPoolName: default     # must match the Deployment's llm-d.ai/warm-pool label
        warmPoolSleepMinSize: "1" # the reserve
```

**Deleting this ScaledObject deletes the pool**, not just its elasticity. The
Deployment goes on holding accelerators and WVA reports it as undeclared rather
than using it. For a fixed-size pool set `minReplicaCount` and `maxReplicaCount`
to the same number — do not delete the trigger.

To remove a pool, remove both together:

```bash
deploy/warmpool.sh delete -n <namespace> --name <pool>
```

It deletes the ScaledObject first, so WVA stops lending Pods that are about to
disappear, then the workload, then the NetworkPolicy `create` made -- in that
order, so live Pods are never left unprotected. It is safe to repeat. Models still naming that pool in their
trigger metadata are then warmed by nothing — `plan` lists them.
