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

## The init-only knobs the swap does NOT cover

`max_num_seqs` is not only a count: it bounds CUDA-graph capture.

```python
max_graph_size = min(max_num_seqs * 2, 512)      # config/vllm.py
```

So graphs exist only up to twice the LAUNCH value. Lowering is safe (the sizes
you need are a subset); raising past launch would silently drop batches into
eager execution, which the bound already refuses.

Three knobs carry behaviour and are init-only, so a budget swap does not reach
them:

| knob | why it matters |
| --- | --- |
| `enforce_eager` | all-or-nothing at init. An engine launched eager can NEVER become an efficient decode engine -- graphs cannot be captured later |
| `max_num_partial_prefills` | how many long prefills run concurrently |
| `enable_chunked_prefill` / `long_prefill_token_threshold` | shape how prefill is admitted |

**So launch at the UNION of both roles**: `max_num_batched_tokens` from prefill
(larger), `max_num_seqs` from decode (larger), graphs on if either role wants
them. The engine is then a superset of both, and the swap moves the budget
inside that envelope.

## Does the swap actually change behaviour?

Decode-shaped load (16 concurrent, 128 output tokens) does NOT discriminate --
a purpose-built decode engine, the union engine, and the union engine swapped
down all sit at 2200-2300 tok/s. The budget never binds at that shape.

Prefill-shaped load does, measured A/B/A/B with a unique prefix per run:

| budget | prefill tok/s | p50 latency |
| --- | --- | --- |
| decode 2048 | 38222 / 38011 | **0.68 s** |
| prefill 8192 | 38987 / 39114 | **0.76 s** |

Throughput is flat; p50 moves consistently and reversibly. A smaller budget
chunks more finely, so concurrent prompts interleave and finish more evenly.
The setting is honoured.

### A trap that nearly produced a fake result

The first attempt reused one prompt across runs and showed 205k -> 333k tok/s,
which read as a large budget effect. It was **prefix caching**: runs after the
first hit cached KV and did almost no prefill. The giveaway was the control --
returning to the original budget stayed fast instead of slowing down. Every run
now leads with a unique nonce.

## What the superset costs, and what it cannot buy

The design above launches at the UNION of both roles. Three objections, checked.

### 1. Memory: measured, and it is negligible

Predicted to hurt -- prefill's larger token budget inflates the model runner's
input buffers and the encoder cache, decode's larger sequence count captures more
CUDA graphs, and both come out of KV. Qwen3-8B at `gpu-memory-utilization 0.55`:

| launch | GPU KV cache | CUDA-graph mem | peak activation |
| --- | --- | --- | --- |
| decode-tuned 2048/16 | 195,616 tokens | 0.15 GiB | 1.19 GiB |
| union 8192/64 | 194,928 tokens | 0.23 GiB | 1.29 GiB |

**688 tokens, 0.35%.** The objection was wrong at this size.

### 2. Tensor parallelism: cannot be superset, and it is the point

llm-d's own statement of what distinguishes the roles is not the batching budget:

> using a larger TP for the memory-bound decoding phase while a smaller TP for
> the computation-bound prefill phase

TP fixes weight sharding at process start. One engine has one TP. If prefill
wants 4 and decode 8, **no runtime swap expresses that** -- it is two processes
on different GPU counts. This is the primary axis of specialisation and the swap
does not touch it.

### 3. Routing, not tuning, is where the benefit comes from

vLLM on why disaggregation exists:

> Without disaggregated prefilling, vLLM may insert some prefill jobs during the
> decoding of one request. This results in higher tail latency. ... With
> disaggregation, your decode engine runs exclusively decode batches. No
> compute-intensive prefill jobs interrupt the step cadence.

That is about what an engine RECEIVES, not how it is tuned. A budget swap cannot
stop prefill interrupting decode if the router keeps sending both. So the swap is
useful only alongside the routing change -- llm-d's `llm-d.ai/role` label, whose
values are `prefill`, `decode` and `prefill-decode`.

**Role is what you are sent; the budget is how you are tuned for it.** The swap
handles the second cheaply. The first is a label change and matters more.

### Where that leaves the fork

Worth doing if roles are already routed and differ only in batching: it removes a
process restart from a ratio change, for ~100-200 lines. Not worth doing if the
roles are meant to differ in tensor parallelism, because nothing in this design
reaches that.

## What this does NOT show

One model, one GPU, TP=1, and no actual P/D traffic -- no NixlConnector, no
prefill/decode routing. It shows the ENGINE tolerates the swap; it does not show
that a real disaggregated deployment benefits from it, nor that NixlConnector
handles GLM-5.2's MLA layout at all. Both are separate questions.
