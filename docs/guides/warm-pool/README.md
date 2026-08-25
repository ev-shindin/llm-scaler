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

### 2. Deploy the pool

```bash
kubectl apply -k config/warmpool -n <namespace>
```

### 3. Let the controller reach it

`config/warmpool/warmpool-networkpolicy.yaml` denies everything except WVA, and
it cannot guess where WVA runs. Edit the marked `namespaceSelector` to name the
controller's namespace **before** applying, or every read fails and the pool
reports itself empty while holding accelerators.

WVA says so when this happens — see [Troubleshooting](#troubleshooting).

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

What you do **not** configure: how many accelerators a Pod holds and how much
memory it may use. Both are read from the pool Pod itself, so they cannot
disagree with it.

## More than one pool

A warm copy is only reusable on the accelerator it was loaded on. A cluster with
two GPU models therefore needs **two pools** — and a variant that needs several
devices needs a pool whose Pods hold that many.

Copy the Deployment and give it a different name in both places:

```yaml
metadata:
  labels:
    llm-d.ai/warm-pool: h100      # the pool's name
spec:
  template:
    metadata:
      labels:
        llm-d.ai/warm-pool: h100  # must match
```

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

WVA also declines a model whose accelerator does not match the pool's, so a
mismatched `warmPool` costs you a log line rather than a wasted Pod.

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
| `"0"` | never warm this model, freeing its slot for models that gain more |
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

## Letting the pool resize itself

Off by default, and it stays off unless you give it a range. Both bounds are
required: a ceiling alone could never grow, and a floor alone could shrink the
pool the first quiet period and never bring it back.

```yaml
metadata:
  annotations:
    llm-d.ai/warm-pool-min-replicas: "2"
    llm-d.ai/warm-pool-max-replicas: "6"
```

It grows when the pool has been **short of its reserve for several consecutive
passes** — one short pass is a borrow doing its job, and growing on that buys a
Pod for every spike.

It shrinks only after a much longer quiet period, and only when **no bridge is
open anywhere**. The asymmetry is deliberate: a pool that is too small costs
latency on every spike it cannot cover, a pool that is too large costs money,
and paying a full model load to grow back is the worst of both.

Two things worth knowing before you turn it on:

- **It needs a permission the controller does not have by default.**
  `config/warmpool` ships a `Role` granting WVA the `scale` subresource on the
  pool Deployment *by name*, in that namespace only. Edit its `namespace` to
  name where WVA runs. Everything else in WVA's own ClusterRole is read-only, on
  purpose — KEDA does the writing — so this is the one narrow exception, and it
  arrives only if you deploy a pool.
- **GitOps will see drift.** WVA owns `replicas` once you set a range. If Argo or
  Flux manages the pool Deployment, tell it to ignore that field, exactly as you
  would for a workload under an HPA.

## Checking it works

The pool reports its state whenever that state changes:

```bash
kubectl logs deploy/wva-controller-manager -n <namespace> | grep "warm pool"
```

```
warm pool state {"pool":"default","state":"pods=2 free=2 resident=1 variants=1 lent=0 accelerator=NVIDIA-H100-80GB-HBM3"}
```

- `pods` / `free` — how many exist, and how many are available to lend
- `resident` — models currently held warm
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

**The pool never resizes.** Check both bounds are set — half a range is refused
whole, and WVA logs why. If they are set, check the `Role` from
`config/warmpool`: its `namespace` has to name where WVA runs, and its
`resourceNames` has to match your pool Deployment's name.

**Two scale-ups of one model, only one bridged.** Automatic mode holds one warm
copy per model. Set `warmPoolCopies` to the number of concurrent scale-ups you
want covered.
