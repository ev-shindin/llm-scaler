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

**No HTTP route makes a running vLLM a sender.** Confirmed by grep over
`entrypoints/openai/`: every weight route is receiver-side. So a serving replica
cannot be asked to donate its weights without new code inside it.

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

1. **Do not build a weight server for the current fleet.** At 8B it saves 6% of a
   cold start. The models where it pays — 70B and above — are not what the pool
   serves today.
2. **If big-model support becomes real, this is the mechanism to reach for**, not
   sleep level 2: it removes the same term, costs one GPU per model instead of
   one per replica, and does not require the receiving engine to have been kept
   alive.
3. **Measure page-cache warming first.** It is free, it needs no component, and
   nobody has measured what it does to a real cold start here.
4. Whatever is built, the readiness rule in §4 comes with it. Three ways to reach
   a silently-wrong engine are already known from the sleep-level work; a
   dummy-loaded replica is a fourth.

## Sources

All read from `vllm/vllm-openai:v0.26.0` in-cluster rather than from
documentation, after the LWS episode showed the docs lag the shipped code:

- `vllm/config/weight_transfer.py` — the three backends
- `vllm/distributed/weight_transfer/{base,clients,nccl_engine,nccl_common,ipc_engine}.py`
- `vllm/config/load.py` — `dummy`, `runai_streamer`, `runai_streamer_sharded`
- grep over `vllm/entrypoints/openai/` — no sender-side route exists
