# Preparing a workload to be scaled

Three things decide whether a scale-up actually helps: whether a new replica can
get to Ready quickly -- its weights, and everything else on the way -- whether
an old one can leave without dropping requests, and whether the Deployment says
enough about itself for WVA to act on. None are WVA settings -- they are
properties of the model server. `make workload-patch` writes the first two;
the rest of the start path is a checklist, below.

> Part of the [WVA deployment guide](../../deploy/). For what to watch once it is
> running, see [Watching what WVA decides](monitoring.md).

## Weights and the model cache

A replica that has to fetch its weights before it can serve turns every scale-up
into a download. It is slow, it can be rate-limited, and it makes scaling depend
on Hugging Face being reachable from a cluster that may deliberately not have
egress.

llm-d already solves this: the modelservice chart defaults to `uriProtocol: pvc`
and mounts a shared cache at `/model-cache`, so the weights are fetched once and
every later pod reads them locally. On a stock llm-d install there is nothing to
turn on.

To check, ask `make workload-patch` rather than looking for a volume. Listing the
PVCs on the pod is the obvious test and it is the wrong one: a claim mounted at
one path while the engine downloads to another passes it, and re-downloads on
every scale-up anyway. What matters is where the weights *land*.

`make scaledobjects-plan` reports a workload whose engine downloads outside any
volume it has mounted, because registering a ScaledObject is the point at which
new replicas start appearing without anyone asking. The test is where the weights
*land* — `HF_HOME` (or `--download-dir`) under a mounted path — not whether a
volume exists somewhere in the pod: a cache mounted with nothing pointed at it is
the case that looks solved and is not.

### What it does not buy

**It does not make scale-up fast.** An 8B model still spends tens of seconds
getting weights it already has locally into GPU memory, and a 70B model minutes.
That is the same order as a cold start.

The reason is not the disk. Measured here with direct I/O, the shared RWX PVC
reads at ~1.8 GB/s and local NVMe at ~5.2 GB/s — fast enough that an 8B model's
weights are a few seconds of pure I/O. What costs is the *load*: parsing
safetensors, sharding, and copying to the device runs at roughly 2 GB/s even
with the file already in page cache.

So the cache removes the *download*, not the *load*, and faster storage does not
remove the load either. Anything aimed at it is a different mechanism — keeping
a process warm, or snapshotting a loaded engine.

The first of those ships here as the **warm pool**: Pods that hold an engine
already loaded on a GPU, which a scaling model borrows instead of paying the
load for. Whether a pool is worth it, and how many models one Pod can hold, are
cluster questions rather than defaults — see the [warm pool
guide](../guides/warm-pool/), and run `deploy/warmpool.sh plan -n <ns>`
to see which of a namespace's models could share one.

(An earlier revision of this section gave ~430 MB/s as the PVC's bandwidth. That
was an end-to-end weight-load rate, not storage throughput; the conclusion above
is unchanged, but the reason it gave was wrong.)

## The rest of the start path

Why start time is an autoscaling concern and not only an operations one: the
running replicas serve alone until the new one is Ready, the queue they build
in that time is charged to the fleet as demand, and the ramp is sized by it. A
replica that takes twice as long to start does not cost one more replica; it
costs however many the doubled queue implies, and they arrive after the queue
is gone. So before tuning the scaler, take the seconds out of the start. These
are properties of the pod template -- a Deployment's or a LeaderWorkerSet's --
and hold with or without a benchmark harness in front of the workload:

| on the way to Ready | what to check |
|---|---|
| **Nothing installed at container start.** | The container's command runs the engine and nothing else: no `apt`, `pip`, `curl` or clone before it. Anything the engine needs is in the image. A start that depends on a package mirror is as slow as the mirror that day, which is the one term that makes start time *vary* between otherwise identical replicas. |
| **`startupProbe` period.** | `periodSeconds` of a few seconds; a probe that fires every 30 s reports a started engine up to 30 s late, every time. Keep the time budget by raising `failureThreshold` (period 5 with threshold 360 is the same 30 minutes as period 30 with threshold 60). The first probes fail on connection refused while the server binds; that is what the threshold is for. |
| **Engine caches that outlive the pod.** | vLLM writes its torch.compile artefacts, the FlashInfer autotune table and Triton's JIT cache under `/tmp` unless told otherwise, so every replica recompiles from nothing. Point `VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_DIR` and `TRITON_CACHE_DIR` at a read-write path shared across replicas -- a subPath of an RWX claim -- and the second replica finds the first one's. If the model claim is mounted read-only, use another claim rather than mounting it a second time: a CSI driver publishes a claim once per pod, and the second mount inherits read-only. And make the engine tolerate a cache path that turns out not to be writable -- a storage hiccup must cost one compile, not the replica; vLLM fails hard on a read-only cache directory unless the variable is unset first. The cache is keyed by a hash of the engine config, so a changed model, flag or version misses rather than hits stale. |
| **The image is already on the node.** | An engine image is 10-20 GB; a node that has to pull it adds a minute or more before the container even starts, and the kubelet evicts unused images under disk pressure, so "it was pulled once" does not stay true. `make prepull IMAGES=<image> NAMESPACE=<ns>` holds the image open on every accelerator node (one DaemonSet per image, the image itself asleep, no accelerator requested), and `make prepull-status` says per node whether the kubelet has it -- see [Holding the image on the nodes](#holding-the-image-on-the-nodes). With the image held, a pinned tag should pull `IfNotPresent`: `Always` contacts the registry at every start for a digest that cannot have changed. |
| **The weights** | are the section above. |

What is left after those is the cold process itself -- imports, the API
server, the KV-transfer connector, the profile run -- and the weight load on a
large model. Below that floor the only lever is not starting cold: the
[warm pool](../guides/warm-pool/), or a minimum replica count of two.

### Holding the image on the nodes

```bash
# exactly the reference the model server's pod spec names, tag or digest included
make prepull IMAGES=docker.io/vllm/vllm-openai:v0.26.0 NAMESPACE=<ns>
make prepull-status NAMESPACE=<ns>          # per accelerator node: present / absent, and the holder's state
make prepull-delete NAMESPACE=<ns>          # stop holding every image (or IMAGES=<image> for one)
```

`PREPULL_NODE_SELECTOR=<key=value>` picks the nodes (default
`nvidia.com/gpu.present=true`, the GPU operator's label; set it to whatever
the model servers select on). The holder runs the image itself, asleep, with a
memory limit and no accelerator: a container that exited would not protect
its image from the kubelet's garbage collection, a running one does. The
image has to carry `/bin/sh` for that; one that does not is still pulled
(the kubelet fetched it to create the container) and `status` says so --
`pulled`, not `present` -- but nothing holds it. Several images are a
comma-separated list. Nothing in WVA depends on the holder; it
is a start-time measure, and `prepull-status` is how you know it worked --
the node's own image list is compared against the reference, so an image
named differently from what the pods pull shows as absent on every node.
A node whose holder reads `Failed Evicted` is under `DiskPressure`: the
kubelet is evicting pods and garbage-collecting images there, a replica
scheduled to it would pull from scratch, and nothing in the namespace can
fix that -- it is the node's disk. (Seen on the first run of this on a
17-node cluster: 16 held the image within two minutes, one was that node.)

The benchmark harness this repository uses puts its own steps on the engine's
start path; what they are and how the benchmark scenarios handle them is in
[Benchmark WVA](../guides/benchmarking/README.md#replica-start-time-in-the-harness).

## Draining before scale-down

A replica removed while it is streaming takes its open responses with it. The
client sees a truncated body — `ClientPayloadError` from aiohttp, an incomplete
stream elsewhere — and the request is lost after it was already paid for in GPU
time.

This is not something WVA can fix from its side. It decides the count; the pod
spec decides what happens to a pod that is going away, and on llm-d that spec
belongs to the modelservice chart (`managed-by: Helm`, release `<model>-ms`).
Anything the installer wrote there would be reverted by the next `helm upgrade`.

It is worth knowing how much this costs before you turn autoscaling on. Each
scale-down produces a burst of client-side stream failures: they begin when the
replica goes and stop about a generation later, once the responses it was
writing have all been cut. Long generations make it worse, because the window in
which a pod is mid-response is then most of its life.

Two fields on the **model server's** pod template fix it:

```yaml
spec:
  template:
    spec:
      # Long enough for the longest generation you are willing to wait for.
      terminationGracePeriodSeconds: 120
      containers:
      - name: vllm
        lifecycle:
          preStop:
            exec:
              # Stop taking new work, then let what is in flight finish.
              # The endpoint is removed from the Service as soon as the pod goes
              # Terminating, so this only has to outlast the requests already on it.
              command: ["/bin/sh", "-c", "sleep 30"]
```

`sleep` is the crude version and it is usually enough, though not for the reason
it looks like. Endpoint removal is **not** instantaneous from the pod's point of
view: the pod is marked Terminating at once, but the withdrawal propagates
asynchronously to every kube-proxy and to the EPP, which keep routing to it in
the meantime. The window therefore covers two things — arrivals still being sent
here, and the generations already running — which is why the emitted patch uses a
longer sleep (45s) than the 30s in the example above. An engine that exposes a
drain endpoint should be asked to drain instead.

Set `terminationGracePeriodSeconds` above the preStop duration plus the time the
longest generation needs — the grace period is the total budget, and the kubelet
sends SIGKILL when it expires regardless of what the hook is doing.

`make scaledobjects-plan` reports this per workload, because registering a
ScaledObject is the moment scale-down becomes possible:

```
note: Scale-down will cut in-flight requests: vllm declares no preStop hook,
      and terminationGracePeriodSeconds is 30. ...
```

## Writing the patch: `make workload-patch`

The two sections above describe the same shape of problem — a pod spec that is
fine while the replica count is fixed and costly once something starts changing
it — and `make workload-patch` writes the fix for both:

```bash
make workload-patch                       # everything in scope
make workload-patch NAMESPACE=my-models   # one namespace
```

### The whole flow, when it reports both problems

```bash
# 1. What is missing, and why it costs something. Changes nothing.
make workload-patch NAMESPACE=my-models

# 2. Only if it reported re-downloaded weights: create the shared cache.
#    Run it with no size or class first — it lists the StorageClasses this
#    cluster is already serving ReadWriteMany from, which is the question
#    "which class do I use?" answered from what already works here.
make model-cache NAMESPACE=my-models
make model-cache NAMESPACE=my-models \
    WVA_MODEL_PVC_SIZE=500Gi WVA_MODEL_PVC_CLASS=<an-rwx-class>

# 3. Apply. The drain half needs nothing; the weights half needs the claim from
#    step 2 and its own opt-in, because mounting storage is a bigger change than
#    adding a hook.
make workload-patch NAMESPACE=my-models \
    WVA_WORKLOAD_PATCH_APPLY=true \
    WVA_WORKLOAD_PATCH_APPLY_WEIGHTS=true
```

Step 3 patches the live objects, which **replaces their pods**. The durable
alternative is to copy the emitted file's contents into your modelservice values
and let the chart roll them out — the next `helm upgrade` reverts anything
applied directly. Either way step 2 is the same: the claim is yours, not the
chart's.

`make model-cache` is safe to re-run. If the claim already exists it reports what
it is and changes nothing, so "did I already create this?" is a question you can
answer by typing the command again.

**The weights half is refused, with the reason, when it would break something:**
a volume of that name already exists on the workload (a strategic merge cannot
replace one, so the API server rejects the whole object — including the drain
hook), something is already mounted at that path, or the claim does not exist
(every pod the rollout creates would stay `Pending`). In each case the drain half
is still applied and the weights half stays in the emitted file.

It writes `wva-workload-patch.yaml`: one document per model server that needs
something, naming the engine container, with a comment saying which of the two
problems that workload has.

**It writes a file rather than applying it, and that is deliberate.** The pod
spec belongs to the model server's chart. A server-side apply is not merely
impolite here, it is refused — `conflict with helm ...
.spec.template.spec.terminationGracePeriodSeconds` — and a patch that does land
is reverted by the next `helm upgrade`, silently. Put the contents into your
modelservice values, where they will survive.

**A missing cache is not a hard failure, and it is reported loudly anyway.**
Nothing here blocks on it: a model server with no persistent weights autoscales
correctly, it is just expensive to scale. But the cost only appears at the moment
a replica is added, and it looks exactly like the autoscaler being slow, so it is
named per workload on the console rather than left in the file:

```text
[WARNING]   ns/qwen-decode RE-DOWNLOADS ITS WEIGHTS on every scale-up: vllm
            fetches Qwen/Qwen3-0.6B from Hugging Face, outside every volume it mounts.
[WARNING] 1 of 1 model server(s) will RE-DOWNLOAD their weights every time a replica is added.
```

On a cluster with no egress it is not a cost but a failure, and the first time
anyone sees it is the first scale-up.

`WVA_WORKLOAD_PATCH_APPLY=true` additionally applies the **drain half** to the
live objects, for a cluster where that is the trade you want. Two things it says
at the time, and both are worth reading before you type it:

- **It replaces pods.** A patch is a new pod template, so every model server it
  touches rolls. It rolls *before* the hook exists, so requests in flight during
  that first rollout are still cut — the fix costs one truncation to install.
  With the default strategy at `replicas: 1` the new pod must schedule before the
  old one goes, so on a cluster with no spare GPU the rollout stalls with the old
  pod still serving.
- **The weights half has its own opt-in**, `WVA_WORKLOAD_PATCH_APPLY_WEIGHTS=true`,
  because mounting storage is a bigger change than adding a hook: it can be
  refused by the API server outright, and it changes where an engine reads its
  weights from. With the opt-in it is applied only where it cannot break the
  workload — no volume of that name, nothing already mounted at that path, and
  the claim exists. Any of those three refuses it, says which, and still applies
  the drain half.

  The first check is the one that matters: `volumes` merges with `retainKeys`,
  and only `kubectl apply` generates that directive, so
  `kubectl patch --type=strategic` merges *into* a volume of the same name and
  the API server rejects the whole object — `may not specify more than 1 volume
  type` — taking the drain hook down with it.

| variable | default | what it sets |
| --- | --- | --- |
| `WVA_WORKLOAD_PATCH_FILE` | `wva-workload-patch.yaml` | where the patch is written |
| `WVA_WORKLOAD_PATCH_APPLY` | `false` | `true` also applies the drain half to the live workloads |
| `WVA_DRAIN_GRACE_SECONDS` | `120` | `terminationGracePeriodSeconds` in the emitted patch. An existing longer value is kept, never lowered |
| `WVA_DRAIN_SLEEP_SECONDS` | `45` | the preStop sleep — the drain window itself |
| `WVA_MODEL_PVC_NAME` | discovered, else `model-pvc` | the claim the emitted weights volume names. Setting it wins over discovery |
| `WVA_MODEL_VOLUME_NAME` | `model-storage` | the volume name in the emitted patch |
| `WVA_MODEL_CACHE_PATH` | `/model-cache` | where that volume is mounted, and the parent of the emitted `HF_HOME` |

The last three affect the emitted file only — nothing applies a volume.

What it will and will not do:

- **It reuses a claim you already have, where there is an unambiguous one.**
  A namespace whose cache is called `llm-d-model-cache` should not be told to
  use `model-pvc`, because the obvious response is to provision a second
  terabyte for weights that are already on the cluster. Only a **Bound**
  `ReadWriteMany`/`ReadOnlyMany` claim qualifies; with several candidates it
  takes the one whose name says what it holds, and with two equally plausible
  ones it names none and falls back to the default rather than guessing at
  someone else's data.
- **`workload-patch` does not create the PersistentVolumeClaim** — `make
  model-cache` does, as a separate, deliberate step. Two fields decide whether
  the cache helps or breaks scale-up, and neither is guessed:

  - **`accessModes` must be `ReadWriteMany`.** Many replicas reading one copy is
    the entire point. A cluster's *default* StorageClass is often RWO block
    storage, and a ReadWriteOnce claim binds to one node — the second replica
    then cannot schedule at all. An auto-created claim on the wrong class turns a
    slow scale-up into a failed one, which is worse than no cache.
  - **`storage` is a function of how many models share it and how large they
    are.** At roughly two bytes per parameter, a 0.6B model is ~1.2 GiB and a 70B
    is ~140 GB; size the claim for every model that will share it, plus room.

  ```bash
  make model-cache NAMESPACE=<ns>     # lists classes this cluster serves RWX from
  make model-cache NAMESPACE=<ns> WVA_MODEL_PVC_SIZE=<size> WVA_MODEL_PVC_CLASS=<class>
  ```

  Run bare, it answers "which class do I use?" from evidence — the classes this
  cluster already has Bound RWX claims on — rather than from a table of
  provisioner names that would go stale. It is safe to re-run: an existing claim
  is reported, not touched. A `WaitForFirstConsumer` class stays `Pending` until a
  pod mounts it, which is correct and is reported as such rather than waited on.

  It is not part of the install, and the claim is not deleted by `undeploy-wva`:
  a claim holds data, so an uninstall that removed it would either destroy
  someone's weights or silently leave a terabyte behind. Deciding that is the
  operator's, which is why it is a separate command. The emitted patch file also
  carries the claim as a commented manifest, for anyone who would rather apply
  their own.
- **It skips LeaderWorkerSets, loudly.** Draining a multi-node group means the
  worker template too — killing a worker aborts the leader's in-flight
  generations whatever hook the leader carries — and the API server will not
  accept a strategic merge for a custom resource. Emitting a confident, wrong
  document is worse than emitting nothing.
- **It never reports a workload it could not read as healthy.** A listing or a
  `get` that fails is said out loud and makes the run exit non-zero, and so does
  a missing `jq` (or `yq`, which is needed only when applying). "Nothing to patch" is a claim about pods, and it is not
  made about pods nobody could read.

The emitted file opens with a header saying most of this, because it otherwise
looks exactly like something to `kubectl apply -f` — which is the one thing not
to do with it. The header also carries a marker line, and only a file carrying
that marker is ever deleted once the workloads stop needing a patch: a file you
wrote yourself at your own `WVA_WORKLOAD_PATCH_FILE` path is left alone.

`make benchmark-standup` and `make benchmark-add-variant` run this with
`APPLY=true`, pinned to the benchmark namespace, and wait for the rollout: those
model servers are ours, and a run whose scale-downs truncate requests is
measuring the harness rather than the autoscaler.
