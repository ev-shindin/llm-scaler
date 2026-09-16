# Operating a pool: scraping, checks, troubleshooting

[← Warm pool guide](README.md)

## Scraping the pool, and why the demand number depends on it

The installer does this for you: it passes the namespace it put the monitoring
stack in (`MONITORING_NAMESPACE`) to every pool it creates. Creating a pool by
hand, name it yourself:

```
deploy/warmpool.sh create -n <namespace> --name <pool> ... --monitoring-namespace <where Prometheus runs>
```

Either way it creates a PodMonitor for the pool and admits that namespace to the
serving port. Both are needed, and leaving them out is quietly wrong rather than
obviously broken.

The shipped `config/warmpool` manifests name no monitoring namespace, because
they cannot know one — a default there would admit somebody else's namespace and
read as monitoring that is configured. So `kubectl apply -k config/warmpool`
gives a pool with no scraping, and the demand caveat below applies to it.

A **lent** pool Pod is serving one model's traffic. WVA sizes a fleet from the
load it can measure, so if nothing scrapes that Pod, the load moves onto the
bridge and the model's measured demand *falls* — at the exact moment the
shortfall that caused the borrow is worst. It then reappears when the bridge is
handed back, which reads as a spike arriving rather than as capacity leaving.
Nothing looks wrong from outside: the pool works, the model serves, the number is
just too small.

Two details worth knowing:

- **What is scraped is the proxy's `:8000`, not the engines' own ports.** The
  proxy forwards `/metrics` to whichever engine is awake, so one address covers
  every model the Pod holds and the engine ports stay reserved for the
  controller.
- **Only awake Pods become targets.** A pool Pod is Ready only while a model is
  awake in it, and the PodMonitor keeps Ready targets only. A sleeping Pod has
  nothing behind its proxy, so scraping it would produce a permanently-DOWN
  target for every idle Pod — a pool of ten mostly-idle Pods would read as broken
  monitoring.
- **Scrape the engines every 10 s, the pool's and the models' alike.** The
  scale-up signal — and so the lend — is a queue at the engine, and it can
  move only when Prometheus holds a sample that shows it, so the scrape
  interval is jitter on every decision. Measured on a two-model run, from
  each rate step to the scale-up decision: 25 / 56 / 56 / 87 s at 30 s,
  15 / 60 / 60 / 76 s at 10 s. The jitter goes; the ~60 s the queue itself
  takes to form does not — that is a property of the signal, not the scrape.
  The controller adds at most ~7 s after the sample. The pool's PodMonitor
  uses 10 s; set the same on the model servers'
  (`monitoring.podmonitor.interval` in a modelservice values file, as the
  shipped benchmark scenarios do).

> **Check your Prometheus actually selects it.** The operator only reads
> PodMonitors matching its `podMonitorSelector`. A stack installed with a
> selector like `release: kube-prometheus-stack` will ignore this one **without
> saying anything** — the PodMonitor exists, no scrape job is ever generated, and
> the symptom is the under-reading demand described above. To confirm, look for
> `podMonitor/<namespace>/wva-warm-pool-<pool>/0` in the generated config:
>
> ```
> kubectl get secret prometheus-<prometheus-name> -n <monitoring-ns> -o jsonpath='{.data.prometheus\.yaml\.gz}' | base64 -d | gunzip | grep warm-pool
> ```
>
> If it is absent, add whatever label your Prometheus selects on to the
> PodMonitor.

What WVA then does with the measurement is deliberately asymmetric:

| | counted? | why |
| --- | --- | --- |
| the bridge's **demand** | **yes**, into the model's total | it is the model's traffic, wherever it is being served |
| the bridge's **capacity** | **no**, never into supply | the Pod is borrowed. Counted as supply it would tell the optimizer the fleet is already big enough and suppress the scale-up the bridge exists to cover — after which the pool holds the Pod indefinitely, because the replicas that would release it are the ones it prevented |

The capacity is still measured, and published per variant for the retained-pool
switching decision, where there are no ordinary replicas coming and the pool *is*
the capacity. Look for `warm-pool-bridge-supply` in the controller log.

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
`sleep-min-size` — see [Sizing](sizing.md#sizing). WVA logs
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
metadata, and `deploy/warmpool.sh plan -n <namespace>` lists every model in
either state without waiting for the log:

```
- model-b    selects: h100  <- NO SUCH POOL
```

**The first spike is never bridged.** Expected. A model has to miss twice before
the pool warms it, so it does not spend a load on a one-off. Set `preload-top`
to warm your busiest variants without waiting, or `warmPoolCopies: "1"` on the
one model you cannot afford to miss.

**The pool never resizes, or is never used at all.** Check its ScaledObject
exists, that `scalerAddress` names the namespace WVA runs in, and that
`warmPoolName` matches the Deployment's `llm-d.ai/warm-pool` label. Without a
trigger there is no pool — WVA logs:

```
warm pool Deployments are holding accelerators but no ScaledObject declares them
```

`plan` shows the other side of this: the pools a namespace has actually
declared. A Deployment that does not appear there is holding accelerators for
nothing. Either give it a trigger, or `delete` it — which removes both objects,
so the state cannot recur.

**Two scale-ups of one model, only one bridged.** Automatic mode holds one warm
copy per model. Set `warmPoolCopies` to the number of concurrent scale-ups you
want covered.
