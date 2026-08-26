# Bridge a scale-up with a warm pool

## Overview

A scale-up is slow because a new replica has to load a model before it can serve.
A warm pool holds a small number of Pods with models already loaded and asleep,
and lends one to a variant that is scaling up. The borrowed Pod joins the
model's InferencePool, serves while the ordinary replica loads, and is handed
back the moment that replica is ready.

The pool is **insurance, not capacity**. It is sized by how often you spike and
how long a replica takes to start, never by peak load.

**It is not free.** Every pool Pod holds its accelerators continuously, whether
lending or idle, and those accelerators count against your namespace's quota
like any other workload. A pool of N Pods lowers your maximum fleet by N. That
is a cost decision, which is why WVA never creates a pool for you.

Two properties are worth knowing before you size one:

- A warm copy is only reusable on the **accelerator it was loaded on**. One pool
  cannot serve two GPU types.
- A pool whose replica count does not **exceed** its reserve can never warm
  anything at all. See [Sizing](#sizing).

## Prerequisites

A model already serving under WVA — follow
[Install WVA in a namespace](../install-in-namespace/) first.

The pool needs, in its namespace:

- Accelerators free for the pool Pods themselves, on the same GPU model as the
  workloads it will warm.
- A shared model cache the pool Pods can read, so a warm copy loads from local
  storage rather than a download.
- RBAC allowing WVA to `patch` Pods. The shipped ClusterRole has this. If yours
  was scoped by hand, WVA refuses to start the pool and says so — it will not
  hold accelerators to warm models it could never lend.

## Turning it on

### 1. Point the controller at the pool

For a namespace-scoped install this is already known and you can skip it: the
pool lives where the workloads live. Only a cluster-scoped controller needs to
be told:

```bash
--warm-pool-namespace=<namespace>
```

### 2. Edit the four things the manifests cannot know

`config/warmpool` is a template, not a working install. Each of these is
cluster-specific, and three fail in a way that looks like something else:

| in | what to set | if you skip it |
| --- | --- | --- |
| `warmpool-networkpolicy.yaml` | the `>>> EDIT THIS <<<` `namespaceSelector` — the namespace WVA runs in | every read fails; the pool reports itself **empty** while holding accelerators |
| `warmpool-scaledobject.yaml` | the `>>> EDIT THIS <<<` in `scalerAddress` | applies cleanly, KEDA creates the HPA, the address never resolves, the pool never resizes |
| `warmpool-deployment.yaml` | the proxy `image` — build your own with `make docker-build-warmpool-proxy` | `ImagePullBackOff`; the shipped digest is a personal registry namespace |
| `warmpool-deployment.yaml` | `runtimeClassName`, and the `claimName` of your model cache | admission fails outright; or the second replica sits Pending forever if the claim is not **ReadWriteMany** |

The `scalerAddress` one is the quiet one: the placeholder is a legal YAML
string, so nothing rejects it and nothing warns.

### 3. Deploy the pool

```bash
kubectl apply -k config/warmpool -n <namespace>
```

## Sizing

Two numbers on the pool Deployment, and they have to agree:

```yaml
metadata:
  annotations:
    llm-d.ai/warm-pool-sleep-min-size: "1"   # the reserve
spec:
  replicas: 2                                 # must EXCEED the reserve
```

`sleep-min-size` is the **reserve**: Pods kept free for the next spike. The pool
may only warm models into what is left over, so:

> **replicas must be greater than sleep-min-size.**

At equality there is nothing left over, so the pool warms nothing for its entire
life while holding every accelerator it has. Nothing about this looks like an
error, because nothing is wrong — the reserve is doing exactly what it was told.
WVA reports it at startup rather than letting you find out from the bill.

Start with `replicas: 2`, `sleep-min-size: 1`, and raise replicas if you see
borrows blocked.

### How many models fit in one Pod

A third number, and it lives in the pod template rather than in an annotation:

```yaml
resources:
  limits:   { memory: 128Gi }
  requests: { memory: 128Gi }
```

**This is the warm-set budget.** A level-1 sleeper moves its weights into shared
memory and the container's cgroup is charged for all of it, so what a Pod can
hold is decided here and nowhere else. Measured on an H100, a resident model
costs roughly **2.6 GiB + 1.4× its weights**:

| model | weights | charged while asleep | fit in 128Gi |
| --- | --- | --- | --- |
| 0.6B | 1.1 GiB | 4.1 GiB | ~30 (capped at 16) |
| 8B | 14.9 GiB | 23.4 GiB | ~5 |

There is a hard ceiling of 16 instances per Pod, but for anything above about a
3B model **memory binds first** — you will run out of budget long before ports.

Two things follow:

- Getting it wrong is the expensive mistake. One model too many does not fail
  its own admission; it OOM-kills the launcher and takes **every model already
  resident in that Pod** with it. WVA will not admit against a budget larger
  than the limit for exactly this reason.
- Unlike the annotations, changing it **rolls the pool** — it is a pod-template
  field, so every Pod restarts and every resident model is loaded again. Size it
  at deploy time; the annotations are the things that are free to retune.

Watch `memory.current`, not `anon`: a sleeper's anonymous memory barely moves,
so anything watching `anon` reports it as costing nothing.

## Tuning

Everything else is an annotation on the pool Deployment. They take effect on the
next reconcile and do **not** restart Pods, so retuning a live pool costs
nothing — which is why they are not in the pod template.

| annotation | does |
| --- | --- |
| `llm-d.ai/warm-pool-sleep-min-size` | Pods held free for the next spike |
| `llm-d.ai/warm-pool-max-hold` | how long a borrowed Pod may serve before it is returned regardless |
| `llm-d.ai/warm-pool-preload-top` | warm this many of the busiest variants without waiting for a miss |
| `llm-d.ai/warm-pool-gpu-memory-utilization` | how much of the card a warm copy claims |

`max-hold` bounds the case where the ordinary replica never arrives, so a
failing scale-up cannot turn your reserve into permanent capacity for one
variant.

`gpu-memory-utilization` trades KV cache for warm-set size. A workload's own
value is usually sized for a Pod running one engine and claims nearly the whole
card, which leaves no room for sleepers beside an awake one; the pool's default
is lower for that reason.

What you do **not** configure here: how many accelerators a Pod holds and how
much memory it may use. Both are read from the pool Pod's own spec, so they
cannot disagree with it — but the memory limit is still a decision, and a
load-bearing one. See [How many models fit in one Pod](#how-many-models-fit-in-one-pod).

## More than one pool

A warm copy is only reusable on the accelerator it was loaded on. A cluster with
two GPU models therefore needs **two pools** — and a variant that needs several
devices needs a pool whose Pods hold that many.

Copy the Deployment and change the pool name in **all three** places, plus the
object's own name:

```yaml
metadata:
  name: wva-warm-pool-h100        # must differ from the first pool's
  labels:
    llm-d.ai/warm-pool: h100      # the pool's name
spec:
  selector:
    matchLabels:
      llm-d.ai/warm-pool: h100    # or both Deployments select the same Pods
  template:
    metadata:
      labels:
        llm-d.ai/warm-pool: h100  # must match
```

Missing the **selector** is the one that bites quietly: two Deployments with
identical selectors fight over one set of Pods, and neither's replica count
means anything afterwards.

A second pool that should resize itself needs its own ScaledObject too, with a
matching `warmPoolName` and `scaleTargetRef`.

Then each model says which pool it may borrow from, in its ScaledObject trigger
metadata:

```yaml
triggers:
  - type: external-push
    metadata:
      modelID: <model>
      warmPool: h100
```

**With one pool you write none of this.** There is nothing to disambiguate, so a
ScaledObject that says nothing still gets a warm copy. The key is only needed
once a namespace holds more than one pool — and then it is required: WVA will
not guess, because guessing wrong spends a full model load on a copy that can
never serve. It names the variant and the pools that exist instead.

WVA also declines a model whose accelerator does not match the pool's, or that
needs more devices than a Pod holds, and says so:

```
warm pool will not warm this model {"variant":"...","reason":"needs 4 GPUs, this Pod holds 1"}
```

Said once per reason, not once per cycle.

## Choosing what stays warm

By default the pool decides: parked models first, then the busiest, then
anything that has missed twice. That is right for almost everything, and needs
no configuration.

Two things it cannot work out for itself, both set per model in the ScaledObject
trigger metadata:

```yaml
triggers:
  - type: external-push
    metadata:
      modelID: <model>
      warmPoolCopies: "2"
```

| value | means |
| --- | --- |
| *absent* | automatic — the pool ranks it, and holds at most one copy |
| `"0"` | never warm this model — and release it if it already is |
| `"1"` | always keep one warm, whatever the ranking thinks |
| `"N"` | keep N warm, so N scale-ups of this model can bridge **at once** |

`"1"` is not the same as absent. Absent lets a quiet model lose its slot to a
busier one; `"1"` pins it — which is what a low-traffic but latency-critical
model needs, and what popularity ranking can never give it.

`"N"` is the only way to cover **simultaneous** scale-ups of one model. A single
warm copy bridges a single scale-up; the second goes cold with free Pods sitting
beside it. It is also the only way to weight a shared pool toward one model,
since automatic mode holds one copy of each and no more.

Copies always land in different Pods — a second copy in the same Pod would share
the first's accelerators and could never serve a second bridge.

Lowering the number releases the excess, oldest copy first, so the setting means
what it says rather than "at least this many". A copy that is currently BRIDGING
is never released: it is serving live traffic, and `max-hold` already returns it.
Automatic mode never releases anything — a model it warmed is one it judged
worth warming.

## Letting the pool resize itself

The pool is scaled the same way every other workload here is: **WVA computes a
size, KEDA writes it.** `config/warmpool` ships a ScaledObject for the pool
alongside its Deployment — delete it and the pool simply stays the size the
Deployment says, which is what most installs want to begin with.

```yaml
spec:
  minReplicaCount: 2      # must EXCEED the pool's reserve
  maxReplicaCount: 6
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:   { stabilizationWindowSeconds: 60 }
        scaleDown: { stabilizationWindowSeconds: 900 }
  triggers:
    - type: external-push
      metadata:
        scalerAddress: wva-external-scaler.<wva-namespace>.svc.cluster.local:9090
        warmPoolName: default   # must match the Deployment's llm-d.ai/warm-pool
```

WVA publishes `lent + reserve + 1`: enough Pods to keep the reserve free
alongside whatever is currently bridging, plus the one spare that makes
admission possible at all.

Three things follow from scaling it this way, and they are the reason for it:

- **WVA needs no permission to resize anything.** Its ClusterRole stays
  read-only. A cluster-wide licence to change replica counts is the permission a
  cluster admin is most right to refuse, and the pool no longer asks for one.
- **The asymmetry lives where you can see it.** Grow promptly, shrink slowly —
  because a pool that is too small costs latency on every spike it cannot cover,
  one that is too large costs money, and paying a full model load to grow back
  is the worst of both. That is the `behavior` block, not something buried in
  the controller.
- **`minReplicaCount` must exceed the reserve.** Otherwise the pool spends every
  quiet period in the one state where it can never warm anything.

### Keeping the pool a fixed size

Set them equal:

```yaml
minReplicaCount: 3
maxReplicaCount: 3
```

KEDA accepts it, the HPA is created with min and max both 3, and the pool stays
there whatever WVA publishes. This is the better way to pin a pool when KEDA is
present, because the size still lives in the one object you already read to
understand it. Deleting the ScaledObject also works and is the right answer on a
cluster with no KEDA at all — the pool then stays at the Deployment's `replicas`.

A pinned pool never relieves its own blocked borrows: if `outcome="blocked"`
appears, it will keep appearing until you raise the numbers. That is the trade
you are choosing, and it is a legitimate one — a fixed GPU budget is easier to
reason about than an elastic one.

Never scale the pool to zero. A pool at zero holds nothing warm, so the first
spike after a quiet period pays a full cold start — and then the pool grows,
loads a model, and is ready exactly in time for the spike that is already over.

## Checking it works

The pool reports its state whenever that state changes:

```bash
kubectl logs deploy/wva-controller-manager -n <wva-namespace> | grep "warm pool"
```

```
warm pool state {"pool":"default","state":"pods=2 free=2 resident=1 variants=1 lent=0 accelerator=NVIDIA-H100-80GB-HBM3"}
```

- `pods` / `free` — how many exist, and how many are available to lend
- `resident` — models currently held warm (`0` on a pool that has warmed nothing)
- `lent` — bridges open right now
- `accelerator` — what the pool's Pods sit on. `unknown` means WVA cannot read
  the nodes, so it cannot match a model's accelerator against the pool's and
  will not try.

A steady pool logs this **once**, not every cycle, so no news is good news.

Then the metrics:

| metric | means |
| --- | --- |
| `wva_warmpool_borrow_total{outcome="hit"}` | a scale-up was bridged |
| `…{outcome="miss"}` | the model was not warm — raise `preload-top`, or the pool is too small to hold it |
| `…{outcome="blocked"}` | the model *was* warm but no Pod was free — raise `replicas` |
| `wva_warmpool_bridge_seconds` | how long bridges last |
| `wva_warmpool_free_pods` | the reserve, live |

`bridge_seconds` is the one to watch. A bridge should last about as long as an
ordinary replica takes to start. Bridges sitting at `max-hold` mean the
scale-ups they cover are failing, and the pool is hiding it.

## Troubleshooting

**Nothing is ever warmed, and no errors.** Check `replicas` against
`sleep-min-size` — see [Sizing](#sizing). WVA logs
`warm pool cannot admit any model: every Pod is reserve`.

**The pool reports itself empty while Pods are running.** WVA logs
`no warm pool Pod could be read … usually the pool NetworkPolicy`. Its ingress
`namespaceSelector` has to name the namespace the controller runs in — step 3
above.

**The pool does not start at all.** WVA logs
`the warm pool is disabled: this controller may not patch Pods`. Grant `patch`
on pods in the pool namespace and restart.

**A model never gets a warm copy in a multi-pool namespace.** WVA logs
`variant will get no warm copy` with the reason — either it named no pool, or it
named one that does not exist. Both are fixed in the ScaledObject trigger
metadata.

**The first spike is never bridged.** Expected. A model has to miss twice before
the pool warms it, so it does not spend a load on a one-off. Set `preload-top`
to warm your busiest variants without waiting, or `warmPoolCopies: "1"` on the
one model you cannot afford to miss.

**The pool never resizes.** Check the pool ScaledObject exists, that its
`scalerAddress` names the namespace WVA runs in, and that its `warmPoolName`
matches the Deployment's `llm-d.ai/warm-pool` label. A pool with no ScaledObject
stays exactly the size its Deployment says, which is not a fault.

**Two scale-ups of one model, only one bridged.** Automatic mode holds one warm
copy per model. Set `warmPoolCopies` to the number of concurrent scale-ups you
want covered.
