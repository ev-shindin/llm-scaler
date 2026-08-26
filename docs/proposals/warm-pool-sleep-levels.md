# Sleep level 1 or 2, and whether NVMe can stand in for RAM

Status: measured on pokprod, 2026-08-26. The pool implements **level 1 only**,
and this is the case for that, plus what level 2 would cost if host RAM ever
becomes the binding constraint.

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
start with extra steps. **The interesting range for level 2 is the middle**: 8B
to ~30B, on RAM-poor nodes, where 7 s is still far better than a ~100 s cold
start and 36 GiB per model is more RAM than a Pod can spare.

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

1. **Keep level 1 as the only implemented path.** It is one call, it is
   ~5× faster, and it cannot serve garbage by omission.
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
