# Sleep level 1 or 2, and whether NVMe can stand in for RAM

Status: measured on pokprod, 2026-08-26. The pool implements **level 1 only**,
and this is the case for that, plus what level 2 would cost if host RAM ever
becomes the binding constraint.

## 0. The numbers, and which of them are trustworthy

Written in discovery order, and §5f corrects §5d while §5e replaces the
extrapolations §5b was built on. **Read this first.**

### Measured

| | value | where |
| --- | --- | --- |
| level-1 sleeper host cost | **`1.41 GiB + 1.352 x weights`** (fitted, 0.6B and 8B) | §5e |
| level-1 wake | **~39 GB/s** host-to-device (8B: 15.27 GiB in 0.42 s) | §5e |
| vLLM startup weight load, ONE rank | **1.08-1.26 GB/s** -- parse-bound, not storage-bound | §5d, §5e |
| aggregate load vs tensor parallelism | **`x TP**0.34`** -- badly sub-linear, 1.27x for 2x the ranks | §5f |
| raw storage | NVMe 5.0-5.3 GB/s, shared PVC 1.7-1.8 GB/s, cached read 6.3 GB/s | §5, §5d |
| fixed cold-start cost | ~100 s, independent of model size | §5b |

### Settled conclusions

- **Level 2 does not reload the model.** It discards weights and serves garbage
  unless `reload_weights` is called; a failed reload poisons the engine until
  restart. §2, §5c.
- **Level 1 cannot be backed by NVMe.** The offload buffer is pinned memory,
  which the kernel may neither swap nor file-back. §5c.
- **NIXL cannot move weights.** The transfer engine ships NCCL and IPC only. §5c.
- **The fast loaders do not help.** `reload_weights` refuses `runai_streamer`. §5c.
- **Faster storage is worth less than it looks.** One rank is parse-bound, and
  aggregation across ranks is sub-linear, so node-local NVMe is ~1.55x for a
  744 GB model rather than the 5.7x an earlier linear model predicted. §5f.

### Weakest number here

`TP**0.34`, fitted to two points on one model on one node, and applied at TP=16 --
eight times beyond anything measured. Direction is known; magnitude is not. §5f.

## 1. The question

A level-1 sleeper keeps its weights in host RAM, so a Pod's memory limit bounds
how many models it can hold. That limit is the warm-set budget, and on a
RAM-poor node it binds long before GPU memory does.

Level 2 appears to remove that bound: the weights are not kept. If the wake path
re-read them from fast local storage, a pool could hold far more models per Pod
and pay only a slower wake. These nodes have 1.8 TB of local NVMe each, so the
storage is there.

## 2. What level 2 actually does — and does not do

**Level 2 does not reload the model.** From `v1/worker/gpu_worker.py` in
`vllm/vllm-openai:v0.26.0`, level-2 sleep saves `model.named_buffers()` to CPU —
buffers, not parameters — and `wake_up` restores exactly those. The weights are
discarded and nothing brings them back.

Measured, on a Pod that had just answered correctly:

```
POST /sleep?level=2      GPU 26155 -> 1357 MiB
POST /wake_up            GPU 1357 -> 25837 MiB, HTTP 200, is_sleeping=false
same seeded prompt   ->  '!!!!!!!!!!!!'
```

The engine reports itself awake and healthy and serves garbage. This is the
failure mode the design has been most afraid of, and it is not hypothetical: it
is what level 2 does by default, on the current image, with a 200 on every call.

Level 2 is built for RLHF, where the weights are discarded precisely because a
trainer is about to push new ones. It is not a cheaper level 1.

## 3. It can be rescued, and the rescue is one call

`/collective_rpc` is exposed by the same dev-mode surface as `/sleep`, and the
worker has a `reload_weights` method. Calling it after an L2 wake restores the
model:

```
POST /collective_rpc {"method":"reload_weights"}   -> {"results":[null]}
same seeded prompt   ->  ' Paris. The capital of France is also the capital of the'
```

byte-identical to the pre-sleep answer. So an NVMe-backed warm model is
buildable: **sleep 2, wake, reload_weights** — three calls where level 1 needs
one, and the pool would have to make the third or serve nonsense.

## 4. What each level costs

Qwen3-0.6B, one H100, weights ~1.2 GiB, timed **inside the Pod** (see §6):

| | level 1 | level 2 |
| --- | --- | --- |
| sleep | 62 ms | 26 ms |
| wake | **119 ms** | 88 ms |
| reload weights | — | **560 ms** |
| **total to serving** | **119 ms** | **648 ms** |
| GPU held asleep | 1357 MiB | 1357 MiB |
| host RAM held asleep | **+2.9 GiB** | none |
| correct output without extra calls | yes | **no** |

Level 1 costs about **2.4× the weights** in host RAM — 2.9 GiB for a 1.2 GiB
model. Level 2 costs about **5×** the wall-clock, and a third call that the pool
must not forget.

Neither level frees much GPU beyond the other: the ~1.3 GiB residue is context
and allocator state, not weights.

### The host cost is paid at the FIRST sleep and never given back

Measured 2026-08-27 on an H200 with Qwen3-8B (~15.3 GiB of weights), on Pods
started fresh for the purpose, reading the container's own cgroup:

| | awake, never slept | after first level-1 sleep | after waking again |
| --- | --- | --- | --- |
| TP=1 | 3.16 GiB | **25.22 GiB** | 25.22 GiB |
| TP=2 | 6.75 GiB | **29.89 GiB** | 29.89 GiB |

Waking gives back the GPU (6.6 GiB -> 75.6 GiB held at TP=2) and returns **none**
of the host RAM. So a pool Pod's memory high-water mark is not reached at
startup, it is reached the first time anything sleeps — and sizing a pool by
watching an awake engine understates it by ~22 GiB, in the direction that
OOM-kills the Pod one sleep cycle later rather than at start.

Two traps in measuring this:

- **The charge is `shmem`, not `anon`.** Level 1 moves the weights into shared
  memory, so `ps`, RSS, and anything summing process memory show no change at
  all. Only the cgroup sees it. It is not `/dev/shm` either — that stayed at
  66 MiB throughout.
- **A Pod that has already slept shows nothing.** Re-measuring a used Pod gives
  identical numbers either side of a sleep, because the buffer is already
  allocated. The first pass of this measurement read exactly that and concluded,
  wrongly, that sleeping was free.

The sleep CHARGE itself is TP-invariant — 22.06 GiB at TP=1 against 23.14 GiB at
TP=2 — which confirms that tensor parallelism shards one copy of the weights
rather than duplicating it. What does scale with TP is the awake baseline, 3.16
to 6.75 GiB, because each rank is its own worker process with its own CUDA
context. `shape.go` charges its overhead term per rank for that reason.

## 5. Does NVMe help? Less than it looks

Measured with direct I/O, three runs each, on a pokprod GPU node:

| | bandwidth |
| --- | --- |
| local NVMe (`/dev/nvme5n1p4`, via emptyDir) | **5.0–5.3 GB/s** |
| shared RWX PVC (Spectrum Scale) | **1.7–1.8 GB/s** |

Two corrections fall out of this, both to numbers this project has been quoting:

- **The PVC is not 430 MB/s.** It reads at ~1.8 GB/s. The 430 MB/s figure on
  record was an end-to-end *weight-load* rate, not storage bandwidth, and it was
  used to argue that level 2 could never pay. That argument was built on the
  wrong number.
- **Measure after a sync.** A first attempt read 374 MB/s from NVMe because the
  `dd` raced the writeback of the file `cp` had just written. It looked like
  NVMe was slower than the network filesystem.

But the reload does **not** run at storage speed. 1.5 GB reloaded in 560 ms is
~2.7 GB/s with the page cache warm — between the two figures above, so the load
path (safetensors parse, host-to-device copy) is a real cost alongside the read.
Moving weights to NVMe therefore buys something, but it is bounded: **NVMe is
~3× the PVC's bandwidth and would not make the reload ~3× faster.**

Projecting the reload at a conservative ~2 GB/s effective, and level 1's wake at
PCIe host-to-device speed:

| model | weights | L1 wake (est.) | L2 reload (est.) | L1 host RAM |
| --- | --- | --- | --- | --- |
| 0.6B | 1.2 GiB | 0.12 s (measured) | 0.56 s (measured) | 2.9 GiB (measured) |
| 8B | 15 GiB | ~0.8 s | ~7 s | ~36 GiB |
| 70B | 131 GiB | ~7 s | ~65 s | ~310 GiB |

The last row is the point. At 70B, level 1 needs more host RAM than most nodes
have, and level 2 takes a minute — which is no longer a bridge, it is a cold
start with extra steps. **The interesting range for level 2 as a bridge is the
middle**: 8B to ~30B, on RAM-poor nodes, where 7 s is still far better than a
~100 s cold start and 36 GiB per model is more RAM than a Pod can spare.

That is the conclusion **as a bridge**, and it is not the whole answer. Read
§5b before acting on it: at TP=8 the NVMe arithmetic above changes, and above
30B level 2 stops competing on latency and starts competing on GPU count, which
is a different argument with a different winner.

## 5b. Big models: what level 2 actually buys, and the case it unlocks

A cold start is **fixed startup + weight load**. Level 1 skips both — its wake is
a PCIe copy from host RAM. Level 2 skips only the fixed part and still pays the
weight load, which is the term that grows with the model.

Measured here, 0.6B, cached image, TP=1: **~100 s** cold to serving, of which
`reload_weights` is **0.56 s**. So this cluster has a ~100 s fixed floor, and
**level 2 saves approximately that, near-constantly, whatever the model size.**
As models grow it stays 100 s and becomes a smaller fraction.

Projected from the measured ~2 GB/s single-rank load rate. **Estimates**; only
the first row is measured:

| | cold start | L2 + reload | L1 wake |
| --- | --- | --- | --- |
| 0.6B | 100 s | 0.6 s | 0.12 s |
| 70B, TP=4 | ~165 s | ~65 s (2.5x) | ~7 s (24x) |
| 405B fp8, TP=8, shared PVC | ~310 s | ~210 s (1.5x) | ~19 s (16x) |
| 405B fp8, TP=8, local NVMe | ~172 s | ~72 s (2.4x) | ~19 s |

### TP=8 is where NVMe stops being second-order

§5 concluded that NVMe converts poorly because the load path costs as much as the
read. That was measured on ONE rank, and it does not generalise: with TP=8 each
rank loads its own shard in its own process, so the parse and host-to-device work
parallelises across eight cores while the **storage does not**. A shared 1.8 GB/s
PVC then becomes the binding constraint, and NVMe's 3x converts.

So: NVMe is second-order at TP=1 and first-order at TP=8. LWS extends the same
logic — each node brings its own NVMe, so aggregate load bandwidth scales with
the group, provided the weights are node-local rather than on the shared PVC.

### The comparison that decides it is not against a cold start

A sleeping Pod frees GPU **memory**, not the **device**: it still holds
`nvidia.com/gpu`, so nothing else can schedule there. For a big model that needs
a whole node, the real comparison is therefore not "level 2 versus a cold start"
but **"level 2 versus simply running the replica"** — which costs the same eight
accelerators and answers requests. On that comparison a single warm big model
loses outright.

Level 2 wins only when it **multiplexes**: N big models sharing one set of GPUs,
one awake at a time, switching in ~1–3 minutes instead of ~5. Then the fleet
holds 8 GPUs instead of 8N. That is a real design — cold storage for big models
with minute-scale activation — and it is exactly what level 1 cannot do at this
size, because level 1's host-RAM cost (~2.4x weights) allows about one resident
model per Pod.

**This corrects an earlier claim of mine** that a pool "earns nothing" above about
30B. That was derived from level 1's RAM bound and silently assumed level 2 was
unavailable. Level 2 has no RAM bound, so the multiplexing gain survives at any
model size; what shrinks is the latency advantage, not the GPU saving.

### The measurement that would settle it

Run an 8B at TP=1 and TP=8 and time `reload_weights` against a full cold start.
Two runs give both coefficients — the fixed floor and the per-byte rate — and
every row above stops being an extrapolation. Do that before costing any big
model work.

## 5c. Can NVMe back level 1? Can NIXL move the weights? No, and no

Both were asked directly, and the image answers both.

### Level 1 cannot be backed by NVMe

The offload buffer is one allocation, in `device_allocator/cumem.py`:

```python
cpu_backup_tensor = torch.empty(size_in_bytes, dtype=torch.uint8,
                                device="cpu", pin_memory=PIN_MEMORY)
```

Pinned memory is page-locked by definition: the kernel may not swap it and may
not back it with a file. So **no configuration puts a level-1 sleeper's weights
on NVMe** — not swap, not mmap. It would take a patch at that allocation site,
and it would trade away the pinned-memory copy that makes an L1 wake 119 ms.
A small patch with a real cost, not a knob.

### The weight-transfer engine does not speak NIXL, and does not read storage

vLLM 0.26 ships a weight-transfer engine (`/init_weight_transfer_engine`,
`/start_weight_update`, `/update_weights`). Its backends are:

```
distributed/weight_transfer/{ipc_engine,nccl_engine,nccl_common}.py
```

NCCL and CUDA-IPC. **There is no NIXL engine for weights** — NIXL 1.3.1 is in the
image for the KV connector, not for this. And the engine is built for RLHF: it
RECEIVES weights pushed from a trainer over GPU-to-GPU transport, and never reads
storage. For N different models that is circular — something must already hold
each model's weights in GPU memory, which is the cost being avoided.

### Scale-up is a different question, and has a better answer

Everything above is about restoring ONE engine from storage. Handing weights from
a peer that already holds them is a separate mechanism with much better numbers,
and it is the right lever for big models. See
[warm-pool-weight-transfer.md](warm-pool-weight-transfer.md).

### The fast loaders exist, and `reload_weights` refuses them

The image ships `runai_model_streamer` 0.16.1 and `fastsafetensors` 0.3.3, and
vLLM exposes `runai_streamer` and `runai_streamer_sharded` as load formats. They
look like the obvious fix for a parse-bound reload. They are not:

```
POST /collective_rpc {"method":"reload_weights"}
  -> 500  Exception: Model reloading with `runai_streamer` format
```

The fast loaders serve the INITIAL load only. `reload_weights` supports the
default safetensors path, so the ~2 GB/s single-rank reload measured in §4 is
what level 2 costs, and no flag improves it.

### And a failed rescue is permanent

This is the part that settles it. After the refused reload, the engine served
`'!!!!!!!!!!!!'` — and **an L1 sleep/wake cycle afterwards did not recover it.**
Level 1 faithfully backs up whatever is in GPU memory, which by then is garbage.
So a level-2 wake whose reload does not happen does not serve one bad answer: it
poisons that engine until the process restarts, and every later sleep preserves
the damage.

Three separate ways to reach that state are now known — forgetting the call,
calling it against an unsupported loader, and the call failing at run time — and
none of them is visible from outside. §7's rule follows from this, not from
taste: if level 2 is ever built, the reload must be inseparable from the wake and
its failure must take the engine out of service.

## 5d. Page-cache warming, measured at last -- and it is parse that binds

Recommended three times in this document and its neighbours as "the cheapest
thing on the list, price it first". Now priced, on a pokprod GPU node:

| | rate |
| --- | --- |
| raw read, `iflag=direct` (cache bypassed) | 1.1 GB/s |
| raw read, buffered, cache warm | **6.3 GB/s** |
| **vLLM's own weight load, cache warm** | **1.26 GB/s** ("Loading weights took 1.11 seconds") |

**The load runs five times slower than the read it depends on.** So it is bound
by parsing safetensors and copying to the device, not by storage, and warming the
page cache cannot take it below that floor.

What warming is actually worth, at TP=1: the cached read saves ~1.1 s of a ~2.2 s
cold load, so roughly **2x on the weight-load term** -- not the 5x the raw
numbers suggest. Worth having, free, and much smaller than it looks.

### This resolves the NVMe question too

§5 said NVMe converts poorly and §5b said it converts at TP=8. Both are right,
and the parse rate is why:

- **TP=1**: one rank parses at ~1.26 GB/s. Storage at 1.8 (PVC) or 5.2 (NVMe)
  is already faster than that, so neither NVMe nor a warm cache moves the
  binding constraint much.
- **TP=8-16**: every rank parses its own shard concurrently, so aggregate parse
  capacity is ~10-20 GB/s. NOW storage binds, and the 1.8 -> 5.2 GB/s jump
  converts nearly fully.

So the single rule to carry: **compare storage bandwidth against the aggregate
load rate, and spend on whichever side is smaller.** Below the crossover, faster
storage buys nothing.

**But do not compute that aggregate as `rate x TP`.** It was measured after this
section was written and it is badly sub-linear -- see §5f, which corrects the
"it buys almost everything" this paragraph originally ended with.

### A cluster trap found while measuring this

Two pods pinned with `nodeName` vanished outright -- `Failed`, then NotFound --
while an unpinned pod on the same fleet ran fine. The nodes carry no taints, so
something in this cluster removes `nodeName`-pinned pods. Schedule with
`nodeSelector` or affinity here, not `nodeName`, or a measurement disappears
with no error to read.

## 5e. The gate is closed: level 1 measured at 8B

Every big-model figure in this document was extrapolated from a 0.6B model, and
§7 made that the gate before anything was costed. Qwen3-8B was downloaded to the
shared cache and measured, on one H100:

| | value |
| --- | --- |
| weights | 15.27 GiB |
| awake | GPU 48207 MiB, host 18.44 GiB |
| **asleep, level 1** | GPU **1911 MiB**, host **40.49 GiB** |
| **host RAM cost of the sleeper** | **+22.05 GiB** = 1.44x weights |
| sleep call | 7.8 s |
| **wake call** | **0.42 s** |
| output after wake | byte-identical |
| cold start | 14.87 s weights + 37.25 s engine init (17.75 s of it compilation) |

### The extrapolation held; two constants were wrong

Fitting both measured points -- 1.11 GiB of weights costing 2.91 GiB asleep, and
15.27 GiB costing 22.05 -- gives

```
sleeper host cost  =  1.41 GiB  +  1.352 x weights
```

against the `2.6 GiB + 1.4 x weights` used until now. **The slope, which is the
only term that matters at any model worth pooling, was right within 3.4%.** The
intercept was over by 1.2 GiB, which mattered only at 0.6B where it dominated.

**The wake rate was wrong in the useful direction.** 15.27 GiB restored in 0.42 s
is ~39 GB/s, not the 20 GB/s assumed from PCIe. Every wake figure published
before today was about twice as pessimistic as reality: GLM-5.2's per-node wake
falls from ~19 s to **~10 s**.

### And the parse rate holds across a 14x size change

"Loading weights took 14.87 seconds" for 16 GB is 1.08 GB/s, against 1.26 GB/s
measured at 0.6B. So §5d's model -- one rank bound by parse at ~1.1-1.3 GB/s,
storage binding only above `parse x TP` -- survives the only size change
available to test it with.

### What this does NOT settle

8B is 46x smaller than GLM-5.2 and ran at TP=1 on one GPU. The linear fit is two
points; nothing here proves it stays linear at 500 GiB, where allocator and NUMA
effects could appear. But the direction of risk is now known rather than
guessed, and the slope has survived one order of magnitude.

## 5f. Correction: aggregate load does NOT scale with TP

§5d closed with a rule -- "compare storage bandwidth against `1.26 GB/s x TP`,
and spend on whichever side is smaller" -- which assumed each rank parses its own
shard concurrently, so N ranks give N times the throughput. **Measured, it does
not.**

Qwen3-8B, same node, same checkpoint:

| | weight load | aggregate |
| --- | --- | --- |
| TP=1 | 14.87 s | 1.08 GB/s |
| TP=2 | 12.70 s | 1.26 GB/s |
| TP=2, CPU tripled (32 cores) | 11.71 s | **1.37 GB/s** |

**1.27x for twice the ranks**, and tripling CPU returned only 8% more -- so it is
not CPU contention. Fitting gives `1.08 GB/s x TP**0.34`, against the linear
model's `x TP`.

| TP | fitted aggregate | the linear model said |
| --- | --- | --- |
| 2 | 1.37 GB/s (measured) | 2.2 |
| 8 | 2.20 GB/s | 8.6 |
| 16 | 2.80 GB/s | 17.3 |

### What this changes

**Node-local storage is worth much less than §5b claimed.** For GLM-5.2 at TP=16
the linear model put a staged load at ~72 s against the shared PVC's 413 s -- a
5.7x win. With the fitted exponent the parse ceiling is ~2.8 GB/s, so NVMe's
5.2 GB/s cannot be used and the load is **~268 s: a 1.55x win, not 5.7x.**

Still worth doing -- 145 s off a cold start for no accelerator and no code -- but
it is an improvement, not a transformation, and it should not be sold as one.

**And it strengthens the case for warming.** If the load path cannot be made fast
by any amount of storage or parallelism, the only thing that removes it is not
performing it: a level-1 wake (~10 s per node for GLM-5.2) or a peer transfer.

### Confidence

Two points, one model, one node. The exponent is the weakest number in this
document and TP=16 is eight times beyond the largest TP measured. TP=4 could not
be scheduled -- the fleet reported "15 Insufficient nvidia.com/gpu" -- so the
gap between 2 and 16 is unmeasured. Treat `tp**0.34` as "sub-linear, direction
known, magnitude uncertain" rather than as a calibrated law.

## 6. Method trap: `kubectl exec` cost more than everything measured

Every wake figure this project recorded before today was taken by timing a
`kubectl exec ... curl` from outside the cluster. On this cluster that call has
a **~2.4 s floor** — measured against `/is_sleeping`, which does nothing.

So a "2.5 s wake" was a ~0.1 s wake and 2.4 s of overhead, and the earlier
multi-node figure in [warm-pool-lws.md](warm-pool-lws.md) has the same defect.
Time inside the Pod with `curl -w '%{time_total}'`, or measure nothing.

This did not change any decision — sub-second is better than the 2.5 s that was
already good enough — but it would have, had the number been used to argue that
a bridge is too slow to be worth building.

## 7. Recommendation

1. **Keep level 1 as the only implemented path for the pool as it exists**,
   which serves small and medium models. It is one call, it is ~5x faster, and
   it cannot serve garbage by omission. This is not an argument that level 2 is
   useless — see §5b for the big-model case, which is a different product.
2. **Do not add level 2 as a knob** on the strength of the RAM saving alone. It
   requires the pool to make a second call that has no failure signal of its own:
   skip `reload_weights` and the model answers, quickly, with nonsense. If it is
   ever added, the reload must be part of the wake path and a wake without it
   must be impossible to express — not a flag that can be left off.
3. **Revisit only if host RAM becomes the binding constraint** at 8B–30B. The
   measurement to take first is a level-1 sleeper's host cost at that size; the
   0.6B number here (2.4× weights) is too small a sample to extrapolate a Pod's
   memory limit from.
4. Storage choice is a second-order lever. NVMe is ~3× the PVC, the reload path
   does not convert that fully, and neither changes the conclusion above.

## Sources

- `vllm/v1/worker/gpu_worker.py` (`sleep`, `wake_up`, `reload_weights`) in
  `vllm/vllm-openai:v0.26.0` — read from the image, not the docs
- `GET /openapi.json` on a running server — the dev-mode route list, including
  `/collective_rpc`
- vLLM issues [#16234](https://github.com/vllm-project/vllm/issues/16234) and
  [#17103](https://github.com/vllm-project/vllm/issues/17103) — wake-then-wrong
  output, which §2 reproduces deterministically for level 2
