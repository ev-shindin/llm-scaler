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
| **Engine caches that outlive the pod.** | vLLM writes its torch.compile artefacts, the FlashInfer autotune table and Triton's JIT cache under `/tmp` unless told otherwise, so every replica recompiles from nothing. Point `VLLM_CACHE_ROOT`, `FLASHINFER_WORKSPACE_DIR` and `TRITON_CACHE_DIR` at a read-write path that outlives the pod and the second replica finds the first one's. The best home is the node's own disk -- [Engine caches on the node's disk](#engine-caches-on-the-nodes-disk), `make engine-cache` -- a shared RWX claim is the other. If the model claim is mounted read-only, use another claim rather than mounting it a second time: a CSI driver publishes a claim once per pod, and the second mount inherits read-only. And make the engine tolerate a cache path that turns out not to be writable -- a storage hiccup must cost one compile, not the replica; vLLM fails hard on a read-only cache directory unless the variable is unset first. The cache is keyed by a hash of the engine config, so a changed model, flag or version misses rather than hits stale. |
| **The image is already on the node.** | An engine image is 10-20 GB; a node that has to pull it adds a minute or more before the container even starts, and the kubelet evicts unused images under disk pressure, so "it was pulled once" does not stay true. `make prepull IMAGES=<image> NAMESPACE=<ns>` holds the image open on every accelerator node (one DaemonSet per image, the image itself asleep, no accelerator requested), and `make prepull-status` says per node whether the kubelet has it -- see [Holding the image on the nodes](#holding-the-image-on-the-nodes). With the image held, a pinned tag should pull `IfNotPresent`: `Always` contacts the registry at every start for a digest that cannot have changed. |
| **The weights** | are the [section above](#weights-and-the-model-cache) -- the download; for a large model, also [Weights on the node's disk](#weights-on-the-nodes-disk) -- the read. |

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

By default the holder lands on every node carrying any known GPU product
label -- GPU Feature Discovery's, CoreWeave's, GKE's, EKS's, Karpenter's,
the AMD operator's; the list in `deploy/lib/accelerator_nodes.sh`, the same
one the controller resolves nodes through. What places the model servers
is their accelerator *resource* request, not a label, so on a cluster of
one vendor these are the same nodes; on a cluster mixing vendors the
default also holds a CUDA image on AMD, Intel or Gaudi nodes, where the
engine can never run -- `prepull-status` names those nodes and warns, and
there `PREPULL_NODE_SELECTOR=<key=value>` is required, not optional: set it
to what the model servers select on.
`PREPULL_TOLERATIONS=<key>[,<key>]` adds taints beyond `nvidia.com/gpu`,
which is always tolerated: a holder `Pending` on every node with no reason
is a taint it does not tolerate. `prepull-status` lists the nodes each
DaemonSet was applied for (the selector is recorded on it), so it needs no
selector of its own. The holder runs the image itself, asleep, with a
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

Three things to know before relying on it:

- `prepull-status` (and the report `prepull` prints after applying) lists
  nodes, which is cluster-scoped. A namespace tenant has no `list nodes`
  by default -- the controller's own read of nodes is granted by the
  cluster-admin setup, not to the person running `make`. Without it the
  DaemonSets still apply; the status is what fails, with a Forbidden.
- The holder runs the engine image as that image runs -- as root on
  Kubernetes, since engine images are built that way and `runAsNonRoot`
  would fail the container; as a UID from the project's range on
  OpenShift, where `restricted-v2` assigns one, which `sleep` does not
  mind -- with no privilege, every capability dropped, a read-only root
  filesystem and the runtime's default seccomp profile. (No `runAsUser`
  either way: a fixed one is what a MustRunAsRange SCC rejects.) Engine
  images bake in `NVIDIA_VISIBLE_DEVICES=all`, which the NVIDIA runtime
  honours even from a container that requested no GPU -- on OpenShift's
  GPU operator too, whose toolkit ships that acceptance on; the holder
  sets it to `void`, so no device is injected. That is admitted under Pod
  Security `baseline` and under OpenShift's `restricted-v2` SCC (by
  analysis; not yet run there), and rejected where Pod Security
  `restricted` is *enforced* (it requires `runAsNonRoot`) -- OpenShift's
  label syncer only warns at that level, so there `kubectl apply` prints
  a `runAsNonRoot != true` warning and the apply goes through. When no
  holder appears on any node, `prepull-status` prints the DaemonSet's
  latest `FailedCreate` event, which is where a pod-security, quota, SCC
  or LimitRange rejection is reported.
- Held with `IfNotPresent` under a *tag*, a node keeps whatever that tag
  pointed at when it pulled; with `Always` the registry's current digest
  would win at every start. For a pinned release tag that is the point.
  If the tag can move under you and that matters, name the image by
  digest (`repo@sha256:...`) -- the holder, `status` and the model
  server's pod spec all take one.


### Weights on the node's disk

The shared cache above is where the weights are; it is not where they are
read fastest. Every replica reads the whole model through the network
filesystem behind the claim, and replicas starting together -- which is
what a scale-up is -- share that pipe. For a small model on a fast shared
class that is seconds and not worth a second copy. For a large one it is
the start path: on one cluster, two nodes reading one 700 GB model from
the shared volume took 1403 s against ~104 s from the node's own NVMe --
past the engine's 600 s startup timeout, so the replica never came up at
all. The load term the previous section describes stays either way; this
removes the read term when it is the one that dominates.

```bash
# the engine image is any image carrying huggingface_hub; the claim is printed
make weights WEIGHTS_MODEL=Qwen/Qwen3-32B WEIGHTS_PATH=/mnt/local/models \
     WEIGHTS_IMAGE=docker.io/vllm/vllm-openai:v0.26.0 NAMESPACE=<ns>
# on RHCOS (OpenShift) the directory is under /var, one per project: WEIGHTS_PATH=/var/mnt/weights/<ns>
make weights-status NAMESPACE=<ns>          # per accelerator node: present / downloading, and why not
make weights-delete NAMESPACE=<ns>          # drop the claim, volume and downloader; the files stay
```

One static `hostPath` PersistentVolume, one claim bound to it (and to no
other: the volume carries a `claimRef`), and one DaemonSet that downloads
the model onto every accelerator node -- the same placement as the image
holder, `WEIGHTS_NODE_SELECTOR` / `WEIGHTS_TOLERATIONS` defaulting to the
`PREPULL_*` values -- and then reports Ready, so `weights-status` and the
DaemonSet's own `numberReady` both mean "the download finished on this
node". The model lands under `<dir>/models/<id>`, the layout the benchmark
harness uses, and a model server mounts it as `pvc://<claim>/models/<id>`,
the way it mounts any claim: each node then reads its own copy, and nothing
in the pod spec says hostPath. A gated model takes
`WEIGHTS_HF_TOKEN_SECRET=<secret>[/<key>]`. Deleting keeps the files; the
next apply finds the marker and downloads nothing.

What it costs, and what it needs:

- Disk: the model's size on every accelerator node, and the download from
  Hugging Face once per node -- a 60 GB model on 16 nodes is a terabyte of
  egress, once. The directory is the cluster's, and it is cluster-shared,
  root-writable state that outlives every object here: every namespace
  pointed at the same directory shares the files and trusts the marker
  (the first to write wins, and the others download nothing), and nothing
  charges what is written there to a quota -- a fill from one namespace is
  `DiskPressure` for every pod on the node. One directory per trust domain
  (`/mnt/local/weights/<namespace>`), on a disk that is not the node's own
  (`/mnt/local/...` on CoreWeave), never a network mount, and never a
  system path: the script refuses `/etc`, `/var/lib`, `/var/spool`,
  `/tmp`, `/home`, `/opt/bin` and their kind (and where RHCOS keeps them:
  `/var/home`, `/var/roothome`, `/var/usrlocal`, `/sysroot`, `/ostree`),
  `/var/mnt` and `/var/srv` themselves, a top-level directory on its own,
  and `..`.
- Leave to create PersistentVolumes, which are cluster-scoped; a namespace
  tenant does not have it (on OpenShift the `storage-admin` role carries
  it). Ask the cluster admin to run `make weights` or to create the
  volume. The claim's `WEIGHTS_CAPACITY` (1Ti unless set) is a request a
  `requests.storage` quota charges -- a project with a storage quota
  refuses the claim; set the capacity near the model's size there. And a
  cluster that reaches Hugging Face through a proxy does not inject its
  proxy into arbitrary pods: the downloader would need `HTTPS_PROXY`.
- Pod Security `baseline` admits the downloader (it mounts the claim, not a
  hostPath); `restricted` does not. On Kubernetes it runs as root, which
  is what writing the root-owned directory the kubelet creates takes. It
  runs as its own ServiceAccount, `weights-downloader`, so a grant for it
  does not land on the namespace's `default` ServiceAccount -- but any pod
  in the namespace may name that ServiceAccount, so an SCC bound to it is
  root for everyone with `pods/create` there, not a private grant.

**On OpenShift** -- by analysis; not yet run there, and `make weights`
says so when the cluster has SecurityContextConstraints:

- `restricted-v2` admits the downloader and runs it as the project's range
  UID with GID 0, which cannot write a root-owned directory; and the
  kubelet does not relabel a hostPath, so even root as `container_t` could
  not write a directory labelled `var_t`/`mnt_t`. Both surface as
  `CrashLoopBackOff` with `Permission denied` in the log, which
  `weights-status` names. Neither exists to fix until the first pod has
  run, so prepare the directory on each node **before** `make weights`:
  ```bash
  oc debug node/<node> -- chroot /host sh -c \
    'mkdir -p /var/mnt/weights/<ns> && chgrp 0 /var/mnt/weights/<ns> && chmod 2775 /var/mnt/weights/<ns> && chcon -t container_file_t /var/mnt/weights/<ns>'
  ```
  or, for an extra disk, a MachineConfig mount unit at that path with
  `Options=context=system_u:object_r:container_file_t:s0`, which labels
  the whole disk for containers and survives everything (and puts every
  file at level `s0`, so the per-project isolation below no longer
  applies -- the directory split is then the only separation). No SCC
  grant is needed on this path. The alternative is an SCC that runs the
  downloader as root: `oc adm policy add-scc-to-user <scc> -z
  weights-downloader -n <ns>`, where the SCC has to allow the
  `runtime/default` seccomp profile the pod sets -- stock `anyuid` does
  not, and admission then falls through to `restricted-v2` as if nothing
  had been granted (`oc get pod <p> -o
  jsonpath='{.metadata.annotations.openshift\.io/scc}'` says which SCC
  took the pod). It removes the `chgrp`/`chmod`, not the `chcon` (the
  kubelet still does not relabel the hostPath), and it is root for every
  pod creator in the project.
- The directory is under `/var` on RHCOS (`/var/mnt/<x>`, `/var/srv/<x>`):
  the root is read-only, `/mnt`, `/home`, `/opt`, `/srv` are symlinks into
  `/var`, and `/var/mnt` on its own is the root disk -- the one the
  kubelet's image store and eviction thresholds live on. Mount the NVMe
  there first (a MachineConfig mount unit); the script cannot tell a
  mountpoint from a directory. A new top-level directory fails outright
  (`read-only file system`).
- Files the downloader writes carry the project's SELinux level, so the
  engines in the same project read them and another project's pods
  cannot even see the marker: on OpenShift one directory per project is
  not advice but the only thing that works, unless the disk is mounted
  with `context=` as above.
- A node that joins later has no copy until the DaemonSet reaches it, and a
  replica scheduled there meanwhile reads from a directory that is being
  written. `weights-status` says which nodes are there yet. A cordoned node
  or one under `DiskPressure` still counts for the DaemonSet (its controller
  tolerates both), keeps evicting the downloader, and shows as `absent
  Failed (not ready) Evicted` for as long as it is in that state; nothing in
  the namespace fixes that node.

On node-local NVMe the engine's default loader (memory-mapped safetensors)
is the right one; `--safetensors-load-strategy prefetch` exists for network
filesystems and buys nothing here. Loaders that read with direct I/O and
many threads exist and are a further step, unmeasured here.

The benchmark standup has the same measure under
`BENCHMARK_MODEL_HOSTPATH=<dir>`; what the harness does with it is in
[Benchmark WVA](../guides/benchmarking/README.md#replica-start-time-in-the-harness).

### Engine caches on the node's disk

The caches in the table above -- torch.compile, FlashInfer autotune, Triton
JIT -- are worth keeping only if the next replica finds them, and on a
shared RWX claim that holds until the claim does not. Measured on one
cluster: nine starts of one decode pod spec, three per node on three nodes,
were repeatable within 2.5 s on a node and split by node into 51-56 s and
75-81 s. The 25 s was the cache. On the slow nodes the CSI driver had
published the shared claim read-only (`ro` in `/proc/mounts`; `touch` fails
with "Read-only file system"), the guard in the engine's command fell back
to the engine default under `/tmp`, and every start on those nodes compiled
from nothing: 14.4 s of torch.compile against 2.9 s from the cache, and
8 s more in the API server before the engine came up. Which node a
replica landed on decided its start time, and nothing in the pod said so.

```bash
# any image with /bin/sh; the engine image is the natural choice. Same directory as the weights.
make engine-cache ENGINE_CACHE_PATH=/mnt/local/weights/<ns> \
     ENGINE_CACHE_IMAGE=docker.io/vllm/vllm-openai:v0.26.0 NAMESPACE=<ns>
# with make weights already run, the path and image default to WEIGHTS_PATH / WEIGHTS_IMAGE
make engine-cache-status NAMESPACE=<ns>     # per accelerator node: prepared / not, and why
make engine-cache-delete NAMESPACE=<ns>     # drop the claim, volume and preparer; the caches stay
```

One static `hostPath` PersistentVolume at `<dir>/engine-cache`, one claim
named `engine-cache` bound to it (and to no other: the volume carries a
`claimRef`), and one DaemonSet that creates the three cache directories on
every accelerator node -- world-writable and sticky, so any UID the engines
run as can write and none removes another's files -- writes a marker, and
stays Ready, so `engine-cache-status` and the DaemonSet's `numberReady`
both mean "prepared, and writable by this UID". A model server mounts the
claim read-write and points the three variables under it:

```yaml
volumes:
  - name: engine-cache
    persistentVolumeClaim:
      claimName: engine-cache
containers:
  - name: vllm
    volumeMounts:
      - name: engine-cache
        mountPath: /engine-cache
    env:
      - {name: VLLM_CACHE_ROOT,          value: /engine-cache/vllm}
      - {name: FLASHINFER_WORKSPACE_DIR, value: /engine-cache/flashinfer}
      - {name: TRITON_CACHE_DIR,         value: /engine-cache/triton}
```

A hostPath is a bind mount with no storage driver in its path, so no
driver publishes it read-only (the filesystem under it can still go
read-only -- an NVMe error, an admin's remount -- which is what the
preparer's readiness and the guard are for), and each node's cache is warm
from the second start on that node -- the first start on a node compiles once (measured: 76 s and
81 s on two nodes whose cache was empty, then 55 s and 55 s; 50 s on a
third whose cache a previous pod had already filled), as on the shared
claim the first start anywhere did. The caches are keyed by a hash of the
engine config, so one claim per namespace serves every model and flag set
in it, and a changed engine misses rather than hits stale. Keep the guard
from the table: a node the preparer has not reached (`engine-cache-status`
lists it) costs a compile, not the replica.

What it costs, and what it needs, is the weights section's list with two
things changed. The size: a few gigabytes per engine config per node rather
than a model. And what the directory holds: **code**. The caches are
torch.compile artefacts, FlashInfer and Triton kernels that every engine
on the node loads and runs, and the three directories are world-writable
(1777: any UID the engines run as can write; sticky, so one UID's files are
not another's to remove). Whoever can write the directory runs code in
every engine on that node. On Kubernetes that is anyone with `pods/create`
in a namespace that can mount the claim -- no more than `pods/create`
already grants, since such a pod runs as root and could overwrite the
weights too; in a namespace that separates its workloads by UID (Pod
Security `restricted`, a `runAsUser` per Deployment) it is more, and the
1770 variant below is for that -- and across namespaces it is everyone
pointed at the same directory. So the weights section's rules hold harder here: one directory
per trust domain, on a disk that is not the node's own, never a system
path (the script refuses the same paths); and wipe `<dir>/engine-cache` on
the nodes when a namespace or a directory changes hands -- `delete` keeps
the caches, and a new tenant of the same namespace name and directory
would load the old one's. 1777 is a trade-off, taken so the engines need no
`fsGroup`; a namespace that runs its engines under one UID and wants the
separation can `chown` the three directories to it and `chmod 1770` them
on each node. The rest is the same: leave to create PersistentVolumes (the
claim's `ENGINE_CACHE_CAPACITY`, 200Gi unless set, is what a storage quota
charges); Pod Security `baseline` (the preparer mounts the claim, not a
hostPath). On OpenShift -- by analysis, not yet run there --
`restricted-v2` runs the preparer and the engines as the project's range
UID, and the kubelet does not relabel a hostPath: prepare
`<dir>/engine-cache` on each node as the weights section says (`chgrp 0`,
`chmod 2775`, `chcon -t container_file_t`, or a `context=` mount). The
preparer then creates the three directories inside it as that UID, owns
them, and its `chmod 1777` takes; the files carry the project's SELinux
level, so another project's pods cannot read them unless the disk is
mounted with `context=`, in which case the directory split is the only
separation.

The benchmark standup has the same measure under
`BENCHMARK_ENGINE_CACHE_HOSTPATH=<dir>`, which defaults to
`BENCHMARK_MODEL_HOSTPATH`; what the harness does with it is in
[Benchmark WVA](../guides/benchmarking/README.md#replica-start-time-in-the-harness).

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
