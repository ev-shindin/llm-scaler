# Transferring weights instead of reading them

Status: investigated against `vllm/vllm-openai:v0.26.0` on pokprod, 2026-08-26.
Nothing built. This is what the shipped machinery can and cannot do, and which
of it is worth building.

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
