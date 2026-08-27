# Swapping a P/D role at run time, without moving weights

Scoping experiment: how much of vLLM must change to turn a prefill-shaped engine
into a decode-shaped one and back, on the same GPUs, without reallocating KV or
moving weights?

**Answer, measured 2026-08-27 on Qwen3-8B: two scheduler fields, bounded above by
the value the engine launched with.**

## Why this is the whole change

llm-d configures both roles from the same `vllmCommon` -- both get
`kv_role: kv_both` -- so what distinguishes them is the scheduler's budget:
`max_num_batched_tokens` and `max_num_seqs`. The scheduler caches each into one
field at init (`scheduler.py`: `max_num_scheduled_tokens`,
`max_num_running_reqs`).

KV geometry must NOT change: llm-d requires prefill and decode to agree on
"KV layout, page size, dtype, attention variant", which is what makes the
NixlConnector handoff possible. So a role swap must preserve it, not reconfigure
it -- and since geometry follows the model, it already does.

## The bound, which is the design constraint

`max_num_batched_tokens` also sizes, at init and from the LAUNCH value:

| | |
| --- | --- |
| model runner input buffers | `gpu_model_runner.py` `_make_buffer(self.max_num_tokens, ...)` |
| torch.compile specialisation range | `config/vllm.py` `compile_range_end` |
| encoder cache | `config/scheduler.py` `encoder_cache_size` |
| attention workspace | `flashinfer.py` |

All are then oversized for any smaller budget, so **lowering is safe and raising
past the launch value is not**. Hence: start every engine at the prefill
maximum, and swap within that ceiling.

## Measured

```
launch                         8192 tokens / 64 seqs
swap to decode-shaped          2048 / 16      -> applied, output byte-identical
raise to 16384                 -> REFUSED, values unchanged
2611-token prompt at 2048      -> chunked correctly, coherent completion
swap back                      8192 / 64      -> applied
```

The 2611-token prompt is the one that matters: it exceeds the lowered budget, so
it forces chunked prefill through the new value. The budget is honoured, not
merely recorded.

## Running it

The scheduler lives in the EngineCore PROCESS, which `/collective_rpc` cannot
reach -- that route dispatches to workers. This prototype therefore intercepts
`EngineCore.collective_rpc` and handles one reserved method name itself, which
needs no new HTTP surface and keeps it to one file. A real change would add a
method on EngineCore and a route; comparable size.

```bash
kubectl cp sitecustomize.py <ns>/<pod>:/tmp/patch/sitecustomize.py
PYTHONPATH=/tmp/patch VLLM_BUDGET_PATCH=1 vllm serve MODEL \
  --max-num-batched-tokens 8192 --max-num-seqs 64 ...

curl -sX POST localhost:8000/collective_rpc -H 'Content-Type: application/json' \
  -d '{"method":"__budget","kwargs":{"max_num_batched_tokens":2048,"max_num_seqs":16}}'
```

## What this does NOT show

One model, one GPU, TP=1, and no actual P/D traffic -- no NixlConnector, no
prefill/decode routing. It shows the ENGINE tolerates the swap; it does not show
that a real disaggregated deployment benefits from it, nor that NixlConnector
handles GLM-5.2's MLA layout at all. Both are separate questions.
