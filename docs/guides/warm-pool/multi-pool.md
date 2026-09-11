# Several pools, and what each holds

[← Warm pool guide](README.md)

## More than one pool

A warm copy is only reusable on the accelerator it was loaded on. A cluster with
two GPU models therefore needs **two pools** — and a variant that needs several
devices needs a pool whose Pods hold that many.

Each additional pool is another `create`, with its own name and the accelerator
it is for:

```bash
deploy/warmpool.sh create -n <namespace> --name h100   --accelerator NVIDIA-H100-80GB-HBM3 --gpus 1   --models 4 --model-size 8B   --wva-namespace <where WVA runs>
```

`plan` will have told you which pools the namespace wants, and which models can
share each one.

If you copy the manifests by hand instead, change the pool name in **all three**
places, plus the object's own name:

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

Then give it its own ScaledObject — **this is what makes it a pool**, not an
extra. Its `warmPoolName` must match the label above and its `scaleTargetRef`
must name this Deployment. A copied Deployment with no trigger of its own holds
accelerators that nothing will ever use, and WVA reports it as undeclared.

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

A pool created without `--accelerator` has no `nodeSelector`, so its Pods may
land on any GPU node and the pool's accelerator is whatever they happened to get.
That is the mismatch below, arriving by accident rather than by configuration.

WVA also declines a model whose accelerator does not match the pool's, or that
needs more devices than a Pod holds, and says so:

```
warm pool will never warm this model; it is pointed at a pool that cannot hold it {"variant":"...","reason":"needs 4 GPUs, this Pod holds 1"}
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

## Retained pools: which model holds the GPUs

A retained pool *is* the serving capacity — the models in it are too large to
start on demand, so no ordinary replicas are coming and exactly one model is
awake at a time. Something has to decide which, or the model that happened to
wake first keeps the accelerators however the load moves.

Retention itself is chosen at create, so the pool is never a bridge for the
window between making it and patching it:

```bash
deploy/warmpool.sh create -n <ns> --name <pool> --type retained ...
```

The two switch knobs are trigger metadata on the pool's ScaledObject, alongside
the `warmPoolRetained: "true"` that `--type retained` writes:

```yaml
warmPoolSwitchSpareThreshold: "20"    # percent
warmPoolMinSwitchInterval: "10m"      # default 10m
```

The rule, in the order it is applied:

1. **A candidate must be under more pressure than the model already awake.**
   Either it has fallen below the spare threshold and the awake one has not, or
   the optimizer wants to scale it up and does not want to scale up the awake
   one.
2. **Equal pressure is not enough.** If the awake model is *also* below the
   threshold, or *also* needs to scale up, nothing moves — switching would move
   the shortage from one model to the other and pay a drain and a wake to do it.
   The pool cannot make two models comfortable at once.
3. **Ties go to the tightest.** Between two candidates at the same urgency, the
   one with least spare capacity wins.
4. **No switch within `warmPoolMinSwitchInterval` of the last one — unless the
   candidate needs to scale up.** A model heading toward trouble can wait; one
   that is short of capacity *now* cannot, and on a retained pool no replicas are
   coming to relieve it. Preemption is safe because of rule 2: a candidate has to
   be under more pressure than the awake model, so if both are growing the
   candidate never gets this far and the pool cannot trade its GPUs between
   them.

Spare is measured as a **fraction of each model's own supply**, so one threshold
compares models of different sizes. Nothing is decided from a reading that does
not exist: a model the optimizer has not measured is neither a candidate nor a
reason to stay put.

Leave `warmPoolSwitchSpareThreshold` unset and only the scale-up rule applies —
the pool has not been asked to switch on spare capacity, so it will not.

Look for `retained pool is switching the model it holds awake` in the controller
log, which names the model, the reason and the interval. The decision *not* to
switch is logged too, at debug, with the reason.
