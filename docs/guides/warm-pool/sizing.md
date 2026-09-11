# Sizing and tuning a pool

[← Warm pool guide](README.md)

## Sizing

Two numbers, now in the same object, and they have to agree:

`warmPoolSleepMinSize` is the **reserve**: Pods kept free for the next spike. The
pool may only warm models into what is left over, so:

> **maxReplicaCount must be greater than warmPoolSleepMinSize.**

At equality there is nothing left over, so the pool warms nothing for its entire
life while holding every accelerator it has. Nothing about it looks like an
error, because nothing is wrong — the reserve is doing exactly what it was told.
WVA reports it rather than letting you find out from the bill.

It is the **ceiling** that matters, not the pool's size right now: a pool
momentarily at its reserve simply grows on the next pass, but a pool whose
ceiling *is* its reserve can never grow past it. This is easiest to get wrong
with a pinned pool, where `min == max` makes the ceiling the only number there is.

Start with `maxReplicaCount: 6`, `warmPoolSleepMinSize: "1"`, and raise the
ceiling if you see borrows blocked.

### How many models fit in one Pod

A third number, and it lives in the pod template rather than in the trigger:

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
- Unlike the trigger metadata, changing it **rolls the pool** — it is a
  pod-template field, so every Pod restarts and every resident model is loaded
  again. Size it at deploy time; the trigger keys are what is free to retune.

Watch `memory.current`, not `anon`: a sleeper's anonymous memory barely moves,
so anything watching `anon` reports it as costing nothing.

## Tuning

Everything else is trigger metadata on the pool's ScaledObject. Edits take
effect on the next reconcile and do **not** restart Pods, so retuning a live
pool costs nothing.

| key | does |
| --- | --- |
| `warmPoolSleepMinSize` | Pods held free for the next spike (`--reserve` at create) |
| `warmPoolRetained` | whether the pool keeps a lent Pod rather than reclaiming it (`--type bridge`/`--type retained` at create) |
| `warmPoolMaxHold` | how long a borrowed Pod may serve before it is returned regardless (`--max-hold` at create; meaningless when retained, and refused together with it) |
| `warmPoolPreloadTop` | warm this many of the busiest variants without waiting for a miss |
| `warmPoolGPUMemoryUtilization` | how much of the card a warm copy claims |

A value that cannot be read refuses the **whole** pool rather than being
skipped: these decide how many accelerators it holds, so applying some of them
would leave you reading a number that is in force nowhere. WVA names the pool
and the key.

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

**Why the tuning is not on the Deployment.** A warm pool is a WVA concept that
happens to have Pods, not a workload WVA happens to manage: nothing outside WVA
reads one or creates one. Declaring it through a trigger keeps one rule with no
exceptions — WVA manages what it is called about — and puts the reserve beside
the ceiling it has to fit inside.

## Letting the pool resize itself

The pool is scaled the same way every other workload here is: **WVA computes a
size, KEDA writes it.** `config/warmpool` ships a ScaledObject for the pool
alongside its Deployment, and it is not optional: the trigger is what
**declares** the pool to WVA. Delete it and you do not get a fixed-size pool,
you get no pool at all — a Deployment holding accelerators that WVA reports as
undeclared and will never warm anything into. To pin the size, set
`minReplicaCount` equal to `maxReplicaCount` and leave the ScaledObject in
place.

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
there whatever WVA publishes.

This is the **only** way to pin a pool. Deleting the ScaledObject does not give
you a fixed pool — it gives you no pool at all, because the trigger is what
declares it. The Deployment keeps its accelerators and WVA reports it as
undeclared.

Watch the ceiling when you pin: with `min == max`, `maxReplicaCount` is the only
number there is, so it must still exceed `warmPoolSleepMinSize` or the pool can
never warm anything.

A pinned pool never relieves its own blocked borrows: if `outcome="blocked"`
appears, it will keep appearing until you raise the numbers. That is the trade
you are choosing, and it is a legitimate one — a fixed GPU budget is easier to
reason about than an elastic one.

Never scale the pool to zero. A pool at zero holds nothing warm, so the first
spike after a quiet period pays a full cold start — and then the pool grows,
loads a model, and is ready exactly in time for the spike that is already over.
