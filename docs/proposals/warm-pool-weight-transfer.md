# Transferring weights instead of reading them

Status: investigated against `vllm/vllm-openai:v0.26.0` on pokprod, 2026-08-26.
Nothing built. This is what the shipped machinery can and cannot do, and which
of it is worth building.

## 0. What is settled, and what is not

This document grew in the order things were discovered, and several later
sections overturn earlier ones. **Read this table before reading linearly**, or
§3 and §4b will tell you things §4h and §4i take back.

| claim | status | where |
| --- | --- | --- |
| A level-2 sleeper can be refilled from a peer instead of storage | **MEASURED** -- byte-identical, 9x faster than `reload_weights` | §4c |
| A fresh `--load-format dummy` replica can be filled the same way, with no pool | **MEASURED** -- byte-identical | §4e |
| It works across nodes | **MEASURED** -- byte-identical | §4g |
| A SERVING replica can donate its own weights (no weight server) | **MEASURED at TP=1** | §4h |
| ...at TP>1 | **DESIGNED, NOT RUN** -- needs the sender to un-shard first | §4i |
| A *dedicated* weight server is worth building | **NO** -- costs what a warm pool costs and is slower | §4b |
| NIXL can move weights | **NO** -- the engine ships NCCL and IPC backends only | see sleep-levels §5c |
| The bandwidth figures predict your fleet | **NO** -- pokprod is not the deployment target, and one run used TCP by mistake | §4f, §4g |

**The failure mode that governs all of it** (§4): a receiver that is dummy-loaded
or level-2 woken, and whose transfer does not complete, serves confident nonsense
with a 200 -- and a later level-1 sleep preserves the damage. Four independent
routes to that state are now known. Readiness must gate on a verified transfer.

**If you only build one thing:** §4c and §4e are the measured wins, and neither
needs a weight server.

## 1. Why this is a different question from sleep levels

[warm-pool-sleep-levels.md](warm-pool-sleep-levels.md) asked how to restore ONE
engine's weights cheaply, and the honest answer was "from host RAM, or from
storage at ~2 GB/s, and nothing else".

Scale-up is a different shape, and a much more favourable one: **the new replica
wants the same model an existing replica already holds in GPU memory.** Nothing
has to be read at all, if one process can hand the bytes to another.

That is not the circular problem the pool has. A pool Pod warming N *different*
models has no peer holding any of them. A scale-up of one model has, by
definition, at least one peer holding exactly it.

## 2. What vLLM ships

A weight-transfer engine, built for RLHF, with three backends:

```
distributed/weight_transfer/{nccl_engine,ipc_engine,sparse_nccl_engine}.py
```

`WeightTransferConfig.backend` is `"nccl" | "ipc" | "sparse_nccl"`. **There is no
NIXL backend** — NIXL 1.3.1 is in the image for the KV connector only.

The split that decides the design:

| side | how it is driven |
| --- | --- |
| **receiver** | HTTP: `/init_weight_transfer_engine`, `/start_weight_update`, `/update_weights`, `/finish_weight_update` — with `HTTPVLLMWeightSyncClient` as a ready-made client |
| **sender** | Python only: `trainer_init()`, `trainer_send_weights()` |

No *built-in* HTTP route makes a running vLLM a sender: every weight route under
`entrypoints/openai/` is receiver-side. **But one can be added without patching
vLLM.** `ParallelConfig.worker_extension_cls` is "dynamically inherited by the
worker class ... to inject new attributes and methods to the worker class for use
in `collective_rpc` calls". So a small extension class exposing `send_weights()`,
mounted into the image and named by a flag, makes a SERVING replica a sender,
driven through the `/collective_rpc` route that already exists.

That matters more than it looks, and §4b was originally written without it: it
means the sender need not be a dedicated weight server holding its own copy. It
can be a replica the fleet is already running and already paying for.

The NCCL engine is a **broadcast from rank 0 to N receivers** of dense
checkpoint-format weights — one sender, many receivers, which is the right shape
for a scale-up from 1 to N.

## 3. The buildable design, and what it costs

A **weight server**: a process that loads one model's weights once and broadcasts
them on demand.

1. new replica starts with `--load-format dummy` — allocates the tensors, reads
   nothing. Compile and CUDA-graph capture depend on shapes, not values, so they
   proceed normally.
2. the weight server calls `trainer_init` to form an NCCL group with it.
3. `trainer_send_weights` broadcasts; the replica is driven through the four
   HTTP routes.

**What it saves.** A cold start is `fixed startup + weight load`. This removes
the weight-load term and nothing else — the ~100 s fixed floor measured in the
sleep-levels document is untouched.

| model | weight load from storage (~2 GB/s) | NCCL broadcast | total cold start |
| --- | --- | --- | --- |
| 8B | ~7 s | ~0.6 s (RDMA) | 107 s → 101 s (**6%**) |
| 70B | ~65 s | ~5 s | 165 s → 105 s (**1.6x**) |
| 405B fp8 | ~190 s | ~15 s | 310 s → 115 s (**2.7x**) |

Broadcast estimated at ~25 GB/s inter-node RDMA; intra-node NVLink is an order
of magnitude higher again. **Estimates** — nothing here is measured.

**So it pays only for big models**, and for the same reason everything else in
this area does: only there does the weight load dominate the fixed cost.

**What it costs.** The weight server holds a full copy of the weights on a GPU,
so it costs at least one accelerator per model held. That is the same "hold a
copy" bargain as the warm pool, but amortised across every replica of that model
rather than one — which is what makes it interesting where the pool is not.

## 4. It has the level-2 failure mode, exactly

A replica started with `--load-format dummy` holds **random weights**. If the
transfer fails, is partial, or is never driven, that replica serves confident
nonsense with a 200 — indistinguishable from a healthy one from outside.

This is the same failure as a level-2 wake without its reload, and the
sleep-levels document showed it is worse than a one-off: once garbage is in GPU
memory, a later level-1 sleep faithfully preserves it, so the damage survives
until the process restarts.

So the same rule is not optional here either: **a replica must not pass readiness
until its transfer is verified.** Dummy-loaded and unverified must be
indistinguishable, to the router, from not running.

That is also why this is not simply "turn on a flag". The RLHF path assumes a
trainer that knows it is pushing weights; a scale-up path has to prove it.

## 4b. Worked example: GLM-5.2 on 8xH100-80GB, and why the answer is no

744B parameters at fp8 is ~744 GB of weights. On 8xH100-80GB (637 GiB/node) that
does not fit one node, so it is 2 nodes at TP=16. The fleet advertises
`rdma/roce_gdr`, so GPU-to-GPU RDMA is genuinely available.

Scale-up from one replica to two, every option priced the same way:

| approach | time to serving | GPUs the MECHANISM costs |
| --- | --- | --- |
| cold, shared PVC | ~513 s | 0 |
| cold, weights staged on node-local NVMe | ~172 s | 0 |
| weight transfer, GPU-resident weight server | ~130 s | **16** |
| weight transfer, host-RAM server staging through a GPU | ~167 s | ~2 + 744 GB RAM |
| **warm pool, level 1** | **~19 s** | 16 |

**A DEDICATED weight server for this model needs 744 GB of GPU memory — two
nodes, sixteen H100s. That is exactly what warm-pooling the model costs, and it
delivers 130 s instead of 19 s.** On that comparison it is strictly dominated.

**But the sender does not have to be dedicated.** With `worker_extension_cls`
(§2) the existing serving replica broadcasts its own weights, and the mechanism
costs **no additional accelerator at all** — only bandwidth and some GPU time
stolen from a replica that is serving. That row is the one to read:

| approach | time to serving | extra GPUs |
| --- | --- | --- |
| transfer from a SERVING replica | ~130 s | **0** |
| cold, staged on node-local NVMe | ~172 s | 0 |
| warm pool, level 1 | ~19 s | 16 |

So it is not dominated after all: it beats NVMe staging on time and matches it on
accelerator cost. What it costs instead is engineering — an extension class, an
orchestrator to drive four calls in order, and the readiness rule in §4, which is
not optional because the receiver starts with random weights.

The host-RAM variant avoids most of the GPU cost and lands at ~167 s — which is
no better than simply staging the weights on node-local NVMe, for a new
component, a new failure mode, and four HTTP calls.

### The economics are inverted at both ends

This generalises, and it is the reason not to keep reaching for the idea:

- **Weight transfer helps most where weights dominate the cold start** — big
  models. That is exactly where the weight server is most expensive, because it
  must hold the whole model.
- **The server is cheapest for small models** — an 8B needs 16 GB on one GPU.
  That is exactly where it helps least: the weight read is ~9 s of a ~109 s cold
  start, so it saves 8%.

There is no size at which both halves are favourable.

### The one case that survives

Many replicas of ONE big model, scaling up several at once. A pool holds one warm
copy and bridges one scale-up at a time; an NCCL broadcast reaches every receiver
in the same transfer. If the fleet routinely goes from one replica to five, a
weight server amortises across all five while a pool does not.

That is a real case, and it is not the case this pool was built for. Price it
only if that fan-out is what actually happens.

## 4c. The combination worth building: level-2 sleepers filled from a peer

Everything above treats "restore from storage" and "receive from a peer" as
alternatives to each other. They are alternatives to the SAME step, and the
interesting design uses the second inside the first's structure.

A level-2 sleeper is a process that is alive, compiled, its CUDA graphs captured,
holding ~1.3 GiB of GPU residue and **no host RAM at all** — with garbage where
its weights were. That is precisely a receiver waiting for a broadcast.

```
/wake_up                 ~88 ms   remaps memory, restores buffers
/start_weight_update
/update_weights  x N              NCCL from a SERVING replica of that model
/finish_weight_update
```

It skips both terms: the ~100 s fixed startup, because the process never died,
and the storage read, because the bytes come over the fabric.

### What it changes is capacity, not latency

| GLM-5.2, 744 GB, TP=16 | bridge | host RAM/node | models per Pod |
| --- | --- | --- | --- |
| level 1 pool | ~19 s | **488 GiB each** | **3** |
| level 2 + storage reload (node-local) | ~72 s | 0 | 16 |
| **level 2 + peer transfer** | **~30 s** | **0** | **16** |

Level 1 is still the fastest wake. What level 2 buys is that a sleeper stops
costing host RAM, so the warm set is bounded by the 16-per-Pod port range and
~1.3 GiB of GPU residue each rather than by RAM. **One two-node group could hold
sixteen 744 GB models warm instead of three.** For a fleet with many large models
that are individually idle, that is a different order of usefulness.

### It only covers scale-UP, which is what the pool is for

The transfer needs a peer holding that model's weights, so it works when the
model is already running and is being scaled — and not when it is at zero
replicas. That is not a gap: bridging a scale-up is the pool's stated job, and
scale-from-zero falls back to a storage reload at ~72 s from node-local NVMe,
which is still far better than a ~513 s cold start.

### RUN AND CONFIRMED, 2026-08-26

It works. Qwen3-0.6B on one Pod with two H100s: vLLM on device 0, a sender
process on device 1 holding the same checkpoint, NCCL between them.

```
baseline                          ' Paris. The capital of France is also the capital of the'
POST /sleep?level=2               GPU 22.7 GiB -> 0.81 GiB
POST /wake_up                     200
  same prompt                     '!!!!!!!!!!!!'          <- poisoned, as expected
  broadcast 1.40 GiB from peer    0.11 s  (13.1 GB/s)
  same prompt                     ' Paris. The capital of France is also the capital of the'
  a second prompt                 ' 4, so the sum is '    <- coherent, not just memorised
```

**Byte-identical to the pre-sleep answer.** The engine's own log confirms the
mechanism: level-2 sleep "freed 19.97 GiB ... of which 0.00 GiB is backed up in
CPU and the rest 19.97 GiB is discarded directly".

Timed on the same engine and model, the two ways to refill it:

| fill path | time | rate |
| --- | --- | --- |
| storage, `reload_weights` | **999 ms** | ~1.5 GB/s |
| **peer, NCCL broadcast** | **110 ms** | **13.1 GB/s** |

So the whole bridge is ~80 ms wake + ~110 ms fill ~= **0.2 s**, against level 1's
0.12 s -- within about 2x of level 1 **while holding no host RAM at all.** That
is the result that matters: it is the host RAM, not the latency, that caps how
many models a Pod can hold.

Extrapolating to GLM-5.2 (744 GB) at the same 13.1 GB/s: ~57 s same-node.
Inter-node RoCE will be slower -- call it ~30-60 s -- against ~275-500 s for the
storage reload and ~19 s for a level-1 wake that needs 488 GiB of RAM per node.

### Three prerequisites nobody had written down

1. **The receiver must be started with `--weight-transfer-config
   '{"backend":"nccl"}'`.** Without it every route returns "Weight transfer not
   configured". It is a startup flag, so a pool Pod's engines must carry it from
   the beginning -- it cannot be turned on when a transfer is first needed.
2. **`WeightTransferTrainerFactory` has no registered engines in v0.26.0.**
   `WeightTransferEngineFactory` registers nccl/ipc/sparse_nccl at import;
   the trainer-side factory registers nothing, so `trainer_init` raises
   "Available trainer engines: []". The sender must drive
   `nccl_common.trainer_init` and `NCCLWeightTransferEngine.trainer_send_weights`
   directly.
3. **The two halves must overlap.** The receiver blocks inside `/update_weights`
   waiting on the broadcast, so the HTTP calls and the broadcast must run
   concurrently. Drive them in sequence and it deadlocks.

The reproduction is `test/experiments/weight-transfer/`.

### What is still untested

- **Inter-node.** This was two GPUs in one Pod. The 13.1 GB/s is a same-host
  number and says nothing about RoCE across nodes.
- **A serving replica as the sender.** The sender here was a standalone process
  holding the checkpoint. Using a live replica needs the `worker_extension_cls`
  route in §2, which is unbuilt.
- **At size.** 1.4 GiB transferred; GLM-5.2 is 500x that.
- **Under TP>1**, where every rank must receive its own shard.
- The composition is *permitted* -- there is no `is_sleeping` guard anywhere in
  `gpu_worker.py` or the transfer base -- but that also means
- **Nothing guards the ordering either**, and that cuts the other way: calling
  `update_weights` before `/wake_up` has remapped the allocator is undefined, and
  nothing will stop it.
- **The silent-garbage failure is multiplied by sixteen.** Each sleeper in a Pod
  can independently end up with random weights and a 200 on every request. §4's
  readiness rule is the price of entry, not a refinement.
- It needs the `worker_extension_cls` sender (§2) on every serving replica.

**The experiment that would decide it** is small and does not need a big model:
take a 0.6B, sleep it to level 2, wake it, and drive `update_weights` from a
second engine holding the same weights. If the output comes back byte-identical,
the design is real and the only remaining question is bandwidth.

## 4e. It works on a plain scale-up too, with no pool at all

§4c refilled a level-2 SLEEPER -- an existing process whose weights had been
discarded. The same path works on a brand-new replica that never read weights,
which is the case that needs no warm pool and therefore no held accelerators.

Measured the same day, same rig:

```
vllm serve ... --load-format dummy --weight-transfer-config '{"backend":"nccl"}'
  init engine (profile, kv cache, warmup)   18.5 s     <- no weight read at all
  prompt                                    ' Intl is Intl is Intl is Intl is'
  broadcast 1.40 GiB from peer              0.11 s  (13.1 GB/s)
  prompt                                    ' Paris. The capital of France is also the capital of the'
  a second prompt                           ' 4, so the sum is '
```

`--load-format dummy` allocates the parameter tensors and fills them with random
values, reading nothing. Compile and CUDA-graph capture depend on shapes rather
than values, so they proceed normally, and `/update_weights` then writes the real
parameters into the tensors that are already there.

**This removes the weight-read term from a cold start entirely**, without a pool
and without holding a single extra accelerator, because the sender is a replica
the fleet is already running.

At 0.6B the saving is invisible -- the read is about a second. It scales with the
model: for GLM-5.2 the shared-PVC read is ~413 s of a ~513 s cold start, so a
scale-up becomes roughly the ~100 s fixed startup plus the transfer.

### How it compares with the pool

| | bridge | extra GPUs held | needs |
| --- | --- | --- | --- |
| level-1 warm pool | ~0.12 s | a pool Pod's worth | host RAM for every sleeper |
| level-2 sleeper + peer fill | ~0.2 s | a pool Pod's worth | a peer with the weights |
| **dummy-load + peer fill** | **~100 s (fixed startup)** | **none** | a peer with the weights |

They are not competitors so much as different points on a curve. The pool buys
away the fixed startup and pays accelerators for it; dummy-load plus transfer
buys away only the weight read but costs nothing to hold. On a fleet where GPUs
are scarce and models are large, the third row is the one that needs no argument
about whether holding capacity is worth it.

## 4d. Prefill/decode is the case this fits best

A P/D deployment runs the same model twice with different plumbing: the roles
differ by `--kv-transfer-config` (`kv_producer` vs `kv_consumer`) and by
scheduling flags, and **their parameters are byte-identical**.

Two consequences, and the second is the interesting one.

**The pool already models them correctly.** A resident instance is keyed by its
full engine options, and `adapter.go` refuses to reuse one whose options differ:
"an instance carrying different options is a different model, a different shape,
or a different compile cache key". So a prefill engine and a decode engine of one
model are already two warm entries rather than one that could be misused. That
refusal is not a limitation to work around -- serving decode traffic from a
prefill-configured engine would answer one variant's requests with another's
engine.

**P/D guarantees the peer that route 2 needs.** The awkward precondition for a
weight transfer is that something must already hold the model's weights in GPU
memory. In a P/D deployment that is free: the decode replicas hold exactly the
weights a prefill sleeper needs, and vice versa. The thing that makes transfer
conditional everywhere else is structural here.

So switching the P/D ratio without a cold start looks like:

- the pool holds both role variants of the model, level-2 asleep. Each costs
  ~0.8 GiB of GPU residue and **no host RAM**, so holding both is two slots of
  the sixteen, not two copies of the weights.
- when the ratio must change, wake the role that is short and fill it from a
  serving replica of the other role.
- measured cost of that fill: ~0.2 s for a 0.6B; extrapolated ~30-60 s for a
  GLM-5.2-sized model.

A running engine's role cannot be mutated -- the KV connector is constructed at
engine init -- so this is "have the other role already warm", not "convert this
one". With level 1 that would cost a second full copy in host RAM and would
usually be refused on those grounds. With level 2 it is nearly free, which is
what makes the idea practical rather than merely possible.

**Unverified:** whether the supervisor's instance options can carry a
`--kv-transfer-config` unchanged (it is a JSON blob inside a flag), and whether
two role variants of one model are counted as independent demand by the policy.
Both are repo questions rather than vLLM questions, and both should be checked
before this is designed.

## 4g. Inter-node: the mechanism holds, the bandwidth was not measured

Run on 2026-08-26 with Qwen3-8B, sender and receiver on **different nodes**
(required anti-affinity), 15.26 GiB of weights:

```
receiver: sleep level 2, wake        -> '!!!!!!!!!!!!'
rendezvous across nodes              0.4 s
BROADCAST 15.26 GiB                  14.77 s   (1.11 GB/s)
same seeded prompt                   ' Paris. The capital of Italy is Rome. The capital of'
```

**The mechanism works across machines**: the rendezvous forms over the pod
network, the broadcast completes, and the output is byte-identical to the
pre-sleep answer. That is the result worth having, and it transfers to any
cluster.

**The 1.11 GB/s is not a fabric measurement, it is a misconfiguration of mine.**
The pods requested `{cpu, memory, nvidia.com/gpu}` and nothing else, so NCCL had
no RDMA device and fell back to TCP over the default CNI. For scale, this cluster
advertises `rdma/roce_gdr: 1k` per node and defines two multi-NIC attachments
(`multi-nic-compute`, `multi-nic-inference`, ipvlan at MTU 9000) -- none of which
this run touched.

Taken at face value the number would say something false and discouraging:
1.11 GB/s is about what the same model loads from the shared PVC (1.08 GB/s), so
a transfer would look pointless. **That conclusion does not follow from this
run.**

### To measure the fast path

1. Annotate both pods `k8s.v1.cni.cncf.io/networks: multi-nic-compute`, or
   request `rdma/roce_gdr: 1` for the verbs path.
2. Point NCCL at the secondary interface (`NCCL_SOCKET_IFNAME`) and use its
   address for `MASTER`, not the default pod IP.
3. Confirm from NCCL's own logs which transport was selected before believing
   any number -- that is the check this run failed to make.

## 4h. A SERVING replica can be the sender -- measured

§2 said no HTTP route makes a running vLLM a sender, and §3 proposed a dedicated
weight server as the way round it. `worker_extension_cls` is the better way, and
it now has a measurement rather than an argument behind it.

A ~40-line class with `weight_metadata()` and `send_weights_to()`, mounted from a
ConfigMap and named by `--worker-extension-cls wext.WeightDonor`. vLLM reports:

```
Injected <class 'wext.WeightDonor'> into <class 'vllm.v1.worker.gpu_worker.Worker'>
for extended collective_rpc calls ['send_weights_to', 'weight_metadata']
```

Qwen3-8B, sender and receiver on **different nodes**, receiver level-2 poisoned
first:

```
sender holds 291 parameters (fused: 72)
  init_weight_transfer_engine / start / update_weights / finish   all 200
  same seeded prompt   ' Paris. The capital of Italy is Rome. The capital of'
  a second prompt      ' 4, 4 + 2'
```

**Byte-identical**, and the second prompt matches the checkpoint-sourced run
exactly. No dedicated weight server, no checkpoint access on the sender, no
extra accelerator: the donor is a replica the fleet is already paying for.

### The fused-parameter worry was real and does not bite

A running engine holds **fused** parameters -- `qkv_proj`, `gate_up_proj` -- where
the checkpoint has separate `q_proj`/`k_proj`/`v_proj` and `gate_proj`/`up_proj`:
291 parameters against the checkpoint's 399. The NCCL engine's docstring says it
carries "dense checkpoint-format weights", so there was a real chance the
receiver would reject them or, worse, mis-load them silently.

It accepts them, because the receiving model's own parameters carry exactly those
names and `load_weights` matches by name. One benign warning appears --
`RotaryEmbedding: Failed to load weights`, a module with no learnable weights --
and the output is unaffected.

**This only holds while sender and receiver run the same vLLM version and
parallelism.** Fusion layout is an internal detail, not a wire format: two
engines that fuse differently would exchange names that do not match, and
nothing in the protocol would notice. Any implementation must pin both.

### A trap for whoever writes the orchestration

The sender's `/collective_rpc` returned in **0.77 s** while the receiver's
`/update_weights` took **14.78 s**. **The sender returning does not mean the
receiver has the weights.** Anything that gates readiness on the sender's
response will mark a replica ready with most of a model still in flight -- which
is the silent-garbage failure of §4, reached by a new route.

## 4i. TP>1: the sender must UN-SHARD, and it can

An earlier revision of this section concluded that a live replica at TP>1 is
"structurally blocked" from donating. **That was wrong**, and the reasoning that
produced it was half right, so it is worth separating the two halves.

### What is true

A TP=2 engine holds **sharded** parameters. Measured, both ranks:

| parameter | TP=1 | TP=2, per rank |
| --- | --- | --- |
| `layers.0.self_attn.qkv_proj.weight` | `[6144, 4096]` | **`[3072, 4096]`** |
| `embed_tokens.weight` | `[151936, 4096]` | **`[75968, 4096]`** |

And the protocol wants a single unsharded sender: `nccl_common.trainer_init`
hard-codes *"the trainer is always rank 0"*, while workers take
`rank_offset + worker_rank`. One sender, N receivers, each sharding on load.

So sending a TP>1 replica's parameters as-is would put half-tensors on the wire
under full-tensor names. That much stands.

### What was wrong

"Cannot be that sender" does not follow from "is not that sender". Every
parameter a parallel layer builds is **tagged with the dimension it was split
along** -- `output_dim` for column-parallel, `input_dim` for row-parallel,
neither when replicated (`model_executor/layers/linear.py`,
`vocab_parallel_embedding.py`) -- and `get_tp_group().all_gather(t, dim=)` is a
shipped helper.

So the sender can rebuild the checkpoint-shaped tensor, one parameter at a time,
and broadcast that. No protocol change, no receiver change, nothing new upstream.

`test/experiments/weight-transfer/worker_extension.py` does this: gather on every
TP rank (it is a collective -- a rank that skips it hangs the others), then only
`rank_in_group == 0` opens the transfer group and sends.

**This is better than the rank-pairing it replaces**, because it leaves the
sender's TP and the receiver's TP independent. Rank-pairing would require them
equal and would tie both to a shard layout that is an internal detail.

### Status: VERIFIED on a TP=2 engine -- and the first attempt was wrong

Run at last on 2026-08-27. All 291 parameters gather, and the checked ones match
the checkpoint exactly:

| parameter | kind | gathered | checkpoint |
| --- | --- | --- | --- |
| `embed_tokens.weight` | column | `[151936, 4096]` | same |
| `self_attn.qkv_proj.weight` | column | `[6144, 4096]` | same |
| `self_attn.o_proj.weight` | **row** | `[4096, 4096]` | same |
| `mlp.gate_up_proj.weight` | column | `[24576, 4096]` | same |
| `mlp.down_proj.weight` | **row** | `[4096, 12288]` | same |

**The first implementation was wrong, and only this check found it.** It chose
the gather axis by reading `output_dim` and falling back to `input_dim`. But vLLM
sets BOTH on many weights -- `set_weight_attrs(weight, {"input_dim": 1,
"output_dim": 0})` in `vocab_parallel_embedding.py` -- so their PRESENCE
disambiguates nothing. Row-parallel weights were gathered along axis 0 and came
back `[8192, 2048]` where the checkpoint has `[4096, 4096]`.

The sharded axis follows the **layer class**, not the attributes: row-parallel
splits the input, every column-ish layer splits the output, and a row-parallel
bias is replicated. `_shard_dims()` walks `named_modules()` and classifies.

Had this gone unchecked, a TP>1 donor would have put correctly-named,
wrongly-shaped tensors on the wire -- §4's failure again, and this time the
receiver's `load_weights` might well have accepted the column-parallel majority
and mangled the rest.

**Still not run end to end from a TP>1 sender.** Shapes are right; a full
transfer from a TP=2 donor into a live receiver has not been done.

## 4f. A note on which cluster these numbers came from

Every bandwidth figure here was measured on pokprod, and pokprod is not the
hardware any of this would be deployed on. Its fabric is what it is; a fleet
bought for multi-node inference would have a faster one.

So read the transfer numbers as **existence proofs and lower bounds**, not as the
design's expected performance:

- that the mechanism works at all, that the output is byte-identical, and that
  the three prerequisites in §4c are real -- those transfer to any cluster;
- the GB/s -- same-host or cross-node -- is this cluster's fabric, and the same
  design on a properly provisioned interconnect should do better.

Where a decision turns on bandwidth rather than on mechanism, it must be re-taken
against the target fleet's numbers. That applies to the whole of §4b, which
compares a transfer against staging weights on local disk: both sides of that
comparison move with the hardware.

## 5. Other transports, ranked by what they would actually buy

1. **CUDA IPC (`ipc` backend), same node.** Shares GPU memory by handle rather
   than copying. In principle a same-node replica could map an existing
   replica's weights at near-zero cost. Attractive and delicate: the two then
   share memory, so eviction and lifetime become coupled. Worth measuring before
   it is designed.
2. **GPUDirect Storage, NVMe straight to GPU.** Bypasses the host bounce and the
   parse. NIXL supports GDS, but vLLM's weight loader does not use it, so this
   is upstream work, not configuration.
3. **Page-cache warming.** Keep the weights resident in the node's page cache so
   the read is memory-speed. Measured: cached reads run 6.7 GB/s against 1.8 GB/s
   from the shared PVC. It holds no GPU at all, which makes it the cheapest thing
   on this list — but it only removes the read, and the load path still costs
   ~2 GB/s, so the ceiling is modest.
4. **`sparse_nccl`.** For sparse and MoE update patterns. Not relevant here.

Note that (3) needs no new component and no GPU, and (1) needs no network. Both
should be priced before (2) or a weight server.

## 6. Recommendation

1. **Do not build a DEDICATED weight server.** At 8B it saves 6% of a cold
   start; at GLM-5.2 scale it costs the same sixteen GPUs as warm-pooling the
   model and is seven times slower (§4b).
2. **Transfer from a serving replica is a different proposition** and is not
   ruled out: `worker_extension_cls` makes it possible without patching vLLM, and
   it costs no extra accelerator. For a big model it reaches ~130 s against
   ~172 s for NVMe staging and ~513 s cold. It is worth building only after the
   two free options below, because it carries a silent-garbage failure mode that
   neither of them has.
3. **Stage weights on node-local storage first.** It reaches ~172 s against
   the transfer's ~130 s, costs no accelerator, needs no component, and cannot
   serve a wrong answer. On a fleet with node-local NVMe this is most of the
   benefit for almost none of the risk.
4. **The shape that most favours a transfer** is a high fan-out scale-up of one big
   model, where a broadcast reaches every receiver at once and a pool bridges one
   at a time (§4b).
5. **Measure page-cache warming.** It is free, it needs no component, and
   nobody has measured what it does to a real cold start here.
6. Whatever is built, the readiness rule in §4 comes with it. Three ways to reach
   a silently-wrong engine are already known from the sleep-level work; a
   dummy-loaded replica is a fourth.

## Sources

All read from `vllm/vllm-openai:v0.26.0` in-cluster rather than from
documentation, after the LWS episode showed the docs lag the shipped code:

- `vllm/config/weight_transfer.py` — the three backends
- `vllm/distributed/weight_transfer/{base,clients,nccl_engine,nccl_common,ipc_engine}.py`
- `vllm/config/load.py` — `dummy`, `runai_streamer`, `runai_streamer_sharded`
- grep over `vllm/entrypoints/openai/` — no sender-side route exists
