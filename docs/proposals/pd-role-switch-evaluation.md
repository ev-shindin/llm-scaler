# Evaluating the P/D role switch — experiment specification

**The question.** What does a switchable engine cost when it is *not* switching,
and what does it buy when the prefill:decode ratio is wrong.

Load shapes are derived from the GLM-5.2 production platform overview; the
derivation is shown so it can be disputed. Engine and model facts are read from
the deployed config, not assumed.

---

## 1. The three arms

| arm | `--all2all-backend` | patch | extra env | isolates |
| --- | --- | --- | --- | --- |
| **A** | `deepep_high_throughput` (prefill), `deepep_low_latency` (decode) | none (stock) | `VLLM_USE_DEEP_GEMM=1` | the production pair |
| **B** | `deepep_v2` | none (stock) | `VLLM_USE_DEEP_GEMM=0` | what `deepep_v2` costs, before any switching |
| **C** | `deepep_v2` | role-switch branch | `VLLM_USE_DEEP_GEMM=0`, `VLLM_SERVER_DEV_MODE=1`, `VLLM_PD_KEEP_PREVIOUS=1` | what *being able to switch* costs |

**B vs C is the measurement that has never been made.** Everything published so
far compares a switchable engine against a dedicated replica, which confounds
two things: "`deepep_v2` instead of the specialist" and "switchable instead of
fixed". B separates them. C differs from B only by the retained spare buffer
(286 MiB/GPU at EP=16) and the patched rebuild path.

Arms B and C use `vllm/vllm-openai:nightly-385dce36bcee42309924a5ece951a96db3dce7f2`
(arm C with the injection from `tools/pd_role_switch/README.md` §1b). Arm A is
the same image unpatched — same base for all three, so the image is not a
variable.

> **The backend names read backwards, and the production manifests settle it.**
>
> Decode is throughput-oriented and prefill is latency-oriented, so "HT for
> prefill, LL for decode" looks inverted. It is not — the names describe the
> **all-to-all dispatch**, not the serving SLO. Prefill dispatches ~1024 tokens
> in one bulk transfer; decode dispatches ~1 token per sequence per step, so
> thousands of tiny transfers where per-dispatch latency dominates. Decode
> reaches its throughput goal *by making each small dispatch fast*.
>
> `glm_data/` holds the deployed LeaderWorkerSets and their startup logs:
>
> | | prefill | decode |
> | --- | --- | --- |
> | `--all2all-backend` | `deepep_high_throughput` | `deepep_low_latency` |
> | pod label | `llm-d.ai/role: prefill` | `llm-d.ai/role: decode` |
>
> Two further confirmations: measured buffer sizes (HT 1264 MiB, LL 3226 MiB —
> LL reserves a worst-case per-rank buffer, the decode pattern), and
> `deepep_v2`'s own mode split (decode = worst-case allocation, prefill =
> exact). `pd_role.py`'s `_BACKEND_KV_ROLE` already encodes this correctly.

---

## 2. Platform facts (measured, not assumed)

Read from the cached `config.json` of `zai-org/GLM-5.2-FP8`:

| property | value | consequence |
| --- | --- | --- |
| attention | **MLA** — `kv_lora_rank` 512, `qk_rope_head_dim` 64 | KV is ~43.9 KiB/token at fp8, not ~1.8 MiB |
| layers | 78, `first_k_dense_replace` 3 | **75 MoE layers** — matches `ranks_switched` logs |
| experts | 256 routed, top-8 | |
| max context | 1,048,576 | not the binding limit; KV budget is |
| weights | 87.7 GiB/GPU | leaves ~39 GiB for KV at `--gpu-memory-utilization 0.90` on 141 GiB H200 |

**Derived KV capacity per rank**: ~39 GiB ÷ 43.9 KiB ≈ **930K tokens** at the
prefill utilisation. With DP=16 and TP=1 each rank holds whole sequences, so
per-rank concurrency is roughly that divided by input length. §2b works this
through per role and pattern. It is arithmetic, not a measurement — **run 0
exists to check it**.

---

## 2b. Reference configuration, and how we size for our rig

`glm_data/` holds the deployed prefill and decode LeaderWorkerSets. Production
is **EP32 on H100-80GB**; this evaluation is **EP16 on H200-141GB**. Those are
not interchangeable, so parameters divide into two kinds.

### Copy: what defines the role

These say what prefill and decode *are*, and must match or the arms stop being
comparable to production at all.

| | prefill | decode |
| --- | --- | --- |
| `--all2all-backend` | `deepep_high_throughput` | `deepep_low_latency` |
| `--max-num-batched-tokens` | **1024** | **128** |
| `--moe-backend` | `deep_gemm` | `deep_gemm` |
| speculative decoding | MTP, 3 tokens | MTP, 3 tokens |
| `cudagraph_mode` | default | `FULL_DECODE_ONLY` |
| KV connector | Nixl, `kv_role: kv_both` | Nixl, `kv_role: kv_both` |
| `--block-size` / KV dtype | 64 / fp8 | 64 / fp8 |
| `--tensor-parallel-size` | 1 | 1 |

Two consequences worth stating rather than discovering:

- **MTP speculative decoding is on in production.** It changes decode
  throughput materially, so every decode arm runs it or none of the decode
  numbers mean anything against the production reference.
- **Arms B and C cannot use `deep_gemm`.** `deepep_v2` pads its contiguous
  layout and `deep_gemm` derives the expert count from the padded tensor, so the
  oracle falls back to `FLASHINFER_CUTLASS`. Production runs `deep_gemm` on both
  roles, so this is a real thing arms B and C give up — part of what A-vs-B
  measures, not a confound to remove.
- **`kv_role: kv_both` on both roles** is exactly the case `_switch_kv_role`
  refuses to move, confirming the guard was written for the production
  configuration rather than a corner case.

### Re-derive: what depends on the hardware

**Do not copy production's `--max-num-seqs` or concurrency.** Those were sized
for 8 experts per rank on an 80 GiB card. At EP=16 each rank holds **16 of 256
experts**, so expert weight per GPU roughly doubles — which is precisely why
production runs EP32 on H100-80GB and why our rig needs H200-141GB.

Sizing for EP16 / H200-141GB, using the measured 87.7 GiB of weights per GPU and
43.9 KiB/token of MLA KV:

| | prefill (`util` 0.90) | decode (`util` 0.95) |
| --- | --- | --- |
| budget | 127 GiB | 134 GiB |
| less weights | − 87.7 GiB | − 87.7 GiB |
| KV available per rank | **~39 GiB** | **~46 GiB** |
| KV capacity per rank | **~930K tokens** | **~1.10M tokens** |

Derived concurrency ceilings, per rank and across DP=16:

| pattern | tokens/req | per rank | across 16 ranks |
| --- | --- | --- | --- |
| P1 (60,810) | 60.8K | ~15 | **~240** |
| P2 (18,380) | 18.4K | ~50 | **~800** |
| P3 (115,600) | 115.6K | ~8 | **~128** |
| D1 (2,560) | 2.6K | ~430 | — (seq-limited, not KV-limited) |

`--max-num-seqs` is set per pattern from the right-hand column, not from
production's 128. **Run 0 measures these rather than trusting the arithmetic**,
and the gate is ±20%.

### Deltas this evaluation cannot close

All belong in any statement of results:

1. **EP=16, not 32** — half a production group; all2all cost scales with rank
   count, so absolute figures do not transfer.
2. **H200, not H100** — and at EP16 the extra memory is *required*, not spare.
3. **GLM-5.2, not 5.3** — 5.2 is what is cached on the nodes.
4. **InfiniBand, not RoCE GDR** — different transport under NVSHMEM/DeepEP.
5. **No CPU KV tier, no P2P** — the 85% reuse is only partly reproducible.

---

## 3. Load patterns

### Derivation

The overview publishes aggregate rates. Dividing by request rate gives the
per-request shape a generator needs:

| source workload | req/min | input tok/req | output tok/req |
| --- | --- | --- | --- |
| CyberGym | 968 | 114,566 | not stated |
| AgentX | 463 | 60,799 | 810 |
| AutomationBench @ 3,000 agents | 8,444 | 17,623 | 782 |
| External reference (~700 agents) | 510 | 75,294 | 776 |
| Retained 7.13-day window | — | 57,846 | 608 |

Two properties dominate, and both are unusual enough to invalidate a
conventional benchmark:

- **Inputs are huge, outputs are small** — 57,846 in / 608 out over the retained
  window, a ratio near 95:1. A 2K-in/128-out benchmark measures a different
  machine.
- **85.24% of input is cached.** Only ~**8,497** of those 57,846 tokens reach
  prefill compute. The Factory plateau is starker: 776.7M input tok/min, 98.7%
  cached, 10.5M/min of real work.

### The patterns

| id | shape | input | output | derived from | reference TTFT p90 |
| --- | --- | --- | --- | --- | --- |
| **P1** | interactive agent | 60,000 | 810 | AgentX | 5.77 s |
| **P2** | saturated automation | 17,600 | 780 | AutomationBench @3,000 | 14.43 s (queue p90 10.18 s) |
| **P3** | long-context episode | 115,000 | 600 | CyberGym | 17.6 s |
| **D1** | decode-bound | 512 | 2,048 | decode isolation, not a production shape | — |

`D1` is deliberately not production-shaped: its job is to saturate decode with
prefill out of the way, so the decode arms measure decode.

### Cache modelling — run each prefill pattern twice

A single replica has no CPU tier and no P2P, so 85% reuse is not reproducible by
itself. Both modes are required and must be labelled in results:

- **`unique`** — every prompt unique. Measures full prefill cost. This is the
  controlled comparison between arms.
- **`shared-prefix`** — requests share a long common prefix so vLLM's own prefix
  cache supplies hits; report the achieved hit rate rather than targeting one.
  Checks that switchability does not interfere with prefix caching, which is not
  obvious and has never been tested.

---

## 4. Common method

Identical across arms unless a table says otherwise.

**Launch** (2 nodes, EP=16; rank 0 serves the API, rank 1 is `--headless
--data-parallel-start-rank 8`):

```bash
vllm serve zai-org/GLM-5.2-FP8 \
  --trust-remote-code \
  --block-size 64 \
  --kv-cache-dtype fp8 \
  --data-parallel-size 16 --data-parallel-size-local 8 \
  --data-parallel-address $MASTER_ADDR --data-parallel-rpc-port 5555 \
  --enable-expert-parallel \
  --tensor-parallel-size 1 \
  --all2all-backend <per arm> \
  --moe-backend <deep_gemm for arm A; omit for B and C> \
  --speculative-config '{"method":"mtp","num_speculative_tokens":3}' \
  --max-num-batched-tokens <1024 prefill | 128 decode> \
  --max-num-seqs <from the §2b sizing table, per pattern> \
  --gpu-memory-utilization <0.90 prefill | 0.95 decode> \
  --max-model-len <input + output, rounded up>
```

Decode runs add `--compilation-config '{"cudagraph_mode":"FULL_DECODE_ONLY"}'`.

**Environment**, all runs: `VLLM_DEEPEP_V2_ALLOW_HYBRID_MODE=0` (two nodes),
`NVSHMEM_HCA_PREFIX=ibp`, `VLLM_ENGINE_READY_TIMEOUT_S=3600`,
`HF_HOME=/mnt/local/hf-cache`. Pod must request `rdma/ib`.

**`VLLM_PD_PAUSE_MODE` is left at its default (`wait`)** and stated in results.
It is worth 40× on switch-under-load and nothing idle.

**Generator rules** — each corresponds to a measurement previously discarded:

| rule | why |
| --- | --- |
| identical MTP config on every arm | speculative decoding changes decode throughput; unmatched, it swamps the comparison |
| 2 warm-up passes discarded, ≥4 measured | an unconverged warm-up turned a 13.7% penalty into 34% |
| prompts unique per request *and per repeat* (outside `shared-prefix`) | 16×1800 tokens "prefilled" in 298 ms were all cache hits |
| TTFT to first **text-bearing** chunk, never headers | a 4 ms TTFT was header timing |
| report min–max or p90, never a lone median | one number cannot show whether two arms differ |
| results written to a **mounted volume** | `kubectl cp` cannot reach a completed pod |

**Recorded every run**: TTFT p50/p90/p99, output tok/s, TPOT p90, requests
completed/failed, peak GPU memory per rank, KV utilisation at peak, engine
version string, and for arm C `ranks_switched` on any switch.

---

## 5. Experiments

### Run 0 — capacity probe (arm B, ~20 min, 2 nodes)

**Purpose:** the §2 KV arithmetic is unverified; every later run's `max-num-seqs`
depends on it, and a wrong value either wastes capacity or triggers preemption
that silently distorts latency.

**Method:** launch arm B at `--max-model-len 131072`. From the startup log
record the reported KV cache blocks and derive tokens per rank. Then issue 1, 4,
8, 16 concurrent P1-shaped requests and record where preemption first appears.

**Output:** a concurrency ceiling per pattern, used as `--max-num-seqs` below.
**Gate:** if measured capacity is within 20% of the §2b derived figures,
proceed. If not, the arithmetic is wrong and every sizing here needs redoing.

---

### Set 1 — isolated prefill and decode, EP=16

One arm at a time, no llm-d, no EPP, nothing between generator and engine.

#### Prefill runs — metric is **latency**

| run | arm | pattern | cache mode | concurrency |
| --- | --- | --- | --- | --- |
| 1.1 | A | P1, P2, P3 | unique | 1, then run-0 ceiling |
| 1.2 | B | P1, P2, P3 | unique | as 1.1 |
| 1.3 | C | P1, P2, P3 | unique | as 1.1 |
| 1.4 | A | P1, P2 | shared-prefix | run-0 ceiling |
| 1.5 | B | P1, P2 | shared-prefix | run-0 ceiling |
| 1.6 | C | P1, P2 | shared-prefix | run-0 ceiling |

Concurrency 1 and saturated are both required: the existing result is that our
engine is **1.46× faster unloaded and 1.35× slower at 16 concurrent**, so a
single concurrency level would support either conclusion.

`--max-num-batched-tokens 2048`, matching the prefill role the switch defines.

#### Decode runs — metric is **throughput**

| run | arm | pattern | concurrency | duration |
| --- | --- | --- | --- | --- |
| 1.7 | A | D1 | 16, 64, ceiling | ≥60 s sustained |
| 1.8 | B | D1 | 16, 64, ceiling | ≥60 s sustained |
| 1.9 | C | D1 | 16, 64, ceiling | ≥60 s sustained |

`--max-num-batched-tokens 128`, matching the decode role. Sustained, not
single-shot: report tok/s over the steady window after warm-up.

#### Run 1.10 — switch under production-shaped load (arm C only)

Drive P2 at the run-0 ceiling, perform `POST /switch_pd_role` mid-run, and
record: switch wall time, `ranks_switched` (must equal 16), in-flight requests
completed, and output identity across a round trip. **Idle switching is already
measured; under production-shaped load it is not.**

#### What set 1 answers

- **A vs B** — the cost of `deepep_v2` against the specialist. Expect the larger
  gap on decode: `deepep_v2` pads its layout, `deep_gemm` rejects the padding,
  and the oracle falls back to `FLASHINFER_CUTLASS`.
- **B vs C** — the price of switchability alone. Expect small. **If B and C
  differ by more than a few percent, that is the headline finding** and it
  changes the case for the approach.

---

### Set 2 — a P/D pair under llm-d with EPP

Prefill group and decode group behind the endpoint picker, so routing, NIXL and
the KV connector are in the path. **1P + 1D, EP=16 each = 4 nodes per arm.**

| run | arm | prefill group | decode group | patterns |
| --- | --- | --- | --- | --- |
| 2.1 | A | `deepep_high_throughput` | `deepep_low_latency` | P1, P2, P3 |
| 2.2 | B | `deepep_v2` | `deepep_v2` | P1, P2, P3 |
| 2.3 | C | `deepep_v2`, switchable | `deepep_v2`, switchable | P1, P2, P3 |

**Additional metrics:** end-to-end TTFT *and* output throughput together, NIXL
transfer count and p90 (production reference: **471 ms p90, ~1 transfer per
request, zero failures**), queue wait at the picker, per-group KV utilisation.

#### Run 2.4 — re-roling a fleet whose ratio is wrong (arm C only)

The run that justifies the work.

1. Drive P2 at a concurrency where **decode is the limiter** — production found
   exactly this, moving 10P/7D → 9P/8D because decode KV saturated first.
2. Record steady-state throughput and TTFT with the ratio wrong.
3. Switch the prefill replica into the decode role **and move its
   `llm-d.ai/role` pod label**, per `docs/guides/pd-role-switch/README.md`.
4. Re-measure.

**Compare against leaving it alone**, not against a dedicated fleet. The claim
under test is "a wrong ratio can be corrected in seconds without restarting",
not "this beats a correctly sized fleet".

The engine does not move the label and must not — that is the autoscaler's job.
For this experiment it is manual. **The standard llm-d guide cannot host this**:
`llm-d.ai/role` sits in the Deployment selector, which Kubernetes will not let
you change, so the switchable deployment shape in that guide is a prerequisite.

---

## 6. Capacity and sequencing

| phase | nodes | wall time |
| --- | --- | --- |
| run 0 | 2 | ~20 min |
| set 1 (runs 1.1–1.10) | 2, sequential | ~4–5 h incl. repeats |
| set 2 (runs 2.1–2.4) | 4 per arm, sequential | ~5–6 h |

Weight load is ~106 s from node-local NVMe against 1403 s from the shared PVC —
`HF_HOME=/mnt/local/hf-cache` is not optional at this run count. Kermit has had
7 free 8-GPU H200 nodes; one set-2 arm fits, three concurrently do not.

---

## 7. What this does not establish

- **Fleet-level benefit.** 1P/1D is not 9P/8D. This establishes mechanism and
  cost, not what a production fleet gains.
- **Cache-tier behaviour.** No CPU offload tier (production: 28.1 TiB), no P2P.
  The 85% reuse that defines this workload is only partly reproducible.
- **EP=32.** Everything here is EP=16.
- **Whether switching is the right control action.** That needs a controller
  that does not exist yet.
