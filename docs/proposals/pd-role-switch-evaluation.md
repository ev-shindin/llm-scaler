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

### Where each arm's engine comes from

All three run the same base image, so the image is never a variable:

```
vllm/vllm-openai:nightly-385dce36bcee42309924a5ece951a96db3dce7f2
```

**Arms A and B** use it unmodified.

**Arm C** needs the role-switch patch, which lives in a **different repository**:

| | |
| --- | --- |
| repo | `github.com/ev-shindin/vllm-conf` (the vLLM fork) |
| branch | `feat/pd-role-switch` |
| how to apply | `tools/pd_role_switch/README.md` §"1b. Put this branch into a stock image" |
| verify | the injection must report **13 hunks, 0 rejects**; then `import vllm.v1.engine.pd_role` must succeed |

That base image is chosen, not arbitrary: it is two commits from the branch's own
merge base, and those two touch only the mooncake connector and its tests — none
of the thirteen files the injection patches. A base that drifts further fails at
the `.rej` check rather than at runtime, which is the intended behaviour.

Arm C also needs `deep_ep` rebuilt against the NCCL in the image;
`tools/pd_role_switch/build_deep_ep.sh` in the same repo does it and takes about
80 s. Note **`deepep_v2` requires an RDMA device even on one node**, so the pod
must request `rdma/ib` or every rank fails with `DeepEPv2 requires NCCL GIN`.

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

Read from the cached `config.json` of `zai-org/GLM-5.3` (703.8 GB, 282
safetensors, fp8, already on the node-local NVMe):

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

> **Where each half of this plan comes from.** The load shapes in §3 are from the
> **GLM-5.2** platform overview — the measured production window, 6.28M requests
> of real traffic. The configuration here is from the **GLM-5.3** manifests,
> which are labelled a canary: `llm-d.ai/model: GLM-5.3`, namespace
> `glm53-serving`, instance `wide-ep-lws-v2runner-canary`. Two adjacent
> generations, not a contradiction — 5.3 is what is being rolled out and 5.2 is
> what has a measured window. Two consequences: the workload shapes are assumed
> to carry across the generation (they are agent-workload properties, so they
> should, but that is an assumption), and the configuration is a **canary**, so
> it may not be the settled production shape.

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
3. **InfiniBand, not RoCE GDR** — different transport under NVSHMEM/DeepEP.
4. **CPU/P2P tier at ~1/200th of production capacity.** The mechanism is
   configured as the canary configures it (50 GiB per pod, LRU,
   `offload_prompt_only`, P2P secondary), but production's 28.1 TiB aggregate
   comes from ~17 groups. Expect a far lower hit rate than 85%; report the
   achieved rate rather than targeting one.

**The model delta is closed.** GLM-5.3 is already on the node-local NVMe
(703.8 GB, 282 safetensors, fp8) and is what production serves, so the
evaluation uses it. GLM-5.2-FP8 is identical in every field that affects sizing
— 78 layers, `first_k_dense_replace` 3, 256 experts top-8, `kv_lora_rank` 512,
`qk_rope_head_dim` 64, hidden size 6144, 1M context, fp8, same size on disk —
so every figure in §2 and §2b holds for either.

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

**Launch.** The three arms' flags and environment are
[`test/evaluation/pd-role-switch/arms.env`](../../test/evaluation/pd-role-switch/arms.env),
not repeated here:

```bash
source test/evaluation/pd-role-switch/arms.env
arm_flags C prefill     # flags for arm C in the prefill role
arm_env   C             # its environment
```

Rank 0 serves the API; rank 1 is `--headless --data-parallel-start-rank 8`.

**Environment**, all runs: `VLLM_DEEPEP_V2_ALLOW_HYBRID_MODE=0` (two nodes),
`NVSHMEM_HCA_PREFIX=ibp`, `VLLM_ENGINE_READY_TIMEOUT_S=3600`,
`HF_HOME=/mnt/local/hf-cache`. Pod must request `rdma/ib`.

**`VLLM_PD_PAUSE_MODE` is left at its default (`wait`)** and stated in results.
It is worth 40× on switch-under-load and nothing idle.

### The load generator

**Use the llm-d-benchmark harness already in this repo**, not a new script.
`test/benchmark/scenarios/` holds 17 scenarios in its format and the
`make benchmark-*` targets drive them, so a second generator would be another
thing to trust for no gain.

The four patterns are committed as harness scenarios, so nothing here needs
transcribing into a new file:

| pattern | scenario |
| --- | --- |
| P1 | `test/benchmark/scenarios/pd_eval_p1_agent.yaml.in` |
| P2 | `test/benchmark/scenarios/pd_eval_p2_automation.yaml.in` |
| P3 | `test/benchmark/scenarios/pd_eval_p3_longctx.yaml.in` |
| D1 | `test/benchmark/scenarios/pd_eval_d1_decode.yaml.in` |

Each carries its shape and its provenance; `__REQUEST_RATE__` and
`__MAX_DURATION__` are substituted by the Makefile, so **one file serves every
rung of a ladder**.

**The harness drives by rate, and so does the production document** — 8,444
req/min at the AutomationBench boundary, 463 for AgentX, 968 for CyberGym. Rate
is therefore the axis, and the ceilings in §2b become a **bound on in-flight
work** rather than the dial: a rate whose in-flight set exceeds them preempts,
and preemption silently distorts latency.

**Production rates cannot be used directly** — they are fleet-wide across 9P+8D,
and one group at EP=16 is a fraction of that. Drive a **ladder** and report where
it breaks, as production established 2,500 agents comfortable and 3,000 the
boundary:

| pattern | ladder (req/s) | stop when |
| --- | --- | --- |
| P1 | 0.25, 0.5, 1, 2, 4 | TTFT p90 leaves its unloaded band, or KV > 90% |
| P2 | 1, 2, 4, 8, 16 | as above |
| P3 | 0.1, 0.25, 0.5, 1 | as above |
| D1 | 2, 4, 8, 16, 32 | output tok/s stops rising |

Each rung is a separate run. **Arms are compared at equal offered rate**, and the
rung where an arm saturates is itself a result — an arm that saturates earlier
has less headroom, which is what A-vs-B and B-vs-C are really asking.

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

**Output:** a concurrency ceiling per pattern, used as `--max-num-seqs` and as
the in-flight bound the rate ladders must not exceed.
**Gate:** if measured capacity is within 20% of the §2b derived figures,
proceed. If not, the arithmetic is wrong and every sizing here needs redoing.

---

### Set 1 — isolated prefill and decode, EP=16

One arm at a time, no llm-d, no EPP, nothing between generator and engine.

#### Prefill runs — metric is **latency**

| run | arm | backend | pattern | cache mode | load |
| --- | --- | --- | --- | --- | --- |
| 1.1 | A | `deepep_high_throughput` | P1, P2, P3 | unique | the full ladder |
| 1.2 | B | `deepep_v2` | P1, P2, P3 | unique | the same rungs as 1.1 |
| 1.3 | C | `deepep_v2` | P1, P2, P3 | unique | the same rungs as 1.1 |
| 1.4 | A | `deepep_high_throughput` | P1, P2 | shared-prefix | saturating rung |
| 1.5 | B | `deepep_v2` | P1, P2 | shared-prefix | saturating rung |
| 1.6 | C | `deepep_v2` | P1, P2 | shared-prefix | saturating rung |

Every arm runs the **same rungs**, so a comparison is always at equal offered
rate. Where an arm saturates is itself a result.

The lowest rung and the saturating rung are both required: the existing result
is that our engine is **1.46x faster unloaded and 1.35x slower at 16
concurrent**, so a single load level would support either conclusion.

`--max-num-batched-tokens 1024`, which is what production's prefill group runs.
Note this differs from `run_switch_test.sh`, whose default is 2048 -- that is a
harness default, not a definition of the role, and the budget-keyed KV direction
works from any change in it.

#### Decode runs — metric is **throughput**

| run | arm | backend | pattern | load | duration |
| --- | --- | --- | --- | --- | --- |
| 1.7 | A | `deepep_low_latency` | D1 | the D1 ladder | ≥300 s per rung |
| 1.8 | B | `deepep_v2` | D1 | the same rungs | ≥300 s per rung |
| 1.9 | C | `deepep_v2` | D1 | the same rungs | ≥300 s per rung |

`--max-num-batched-tokens 128` and
`--compilation-config '{"cudagraph_mode":"FULL_DECODE_ONLY"}'`, matching
production's decode group. Sustained, not single-shot: report tok/s over the
steady window after warm-up, and stop climbing the ladder when output tok/s
stops rising.

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

### Set 2 — prefill and decode groups under llm-d with EPP

Prefill and decode groups behind the endpoint picker, so routing, NIXL, the CPU
offload tier and P2P are all in the path. **2P + 1D, EP=16 each = 6 nodes per
arm.**

**Why 2P and not 1P.** Run 2.4 switches a prefill replica into the decode role.
With a single prefill group that leaves the fleet with *no prefill capacity* and
it stops serving — the experiment cannot run. Production's actual move was
10P/7D → 9P/8D: one of many groups changed role. 2P+1D → 1P+2D is the smallest
topology that reproduces that shape, and it is also what makes the P2P tier
non-degenerate, since P2P pulls between prefill ranks and has nowhere to pull
from when there is only one.

**The CPU offload tier and P2P are configured, matching the canary.** Both are
vLLM connector configuration rather than separate infrastructure, so there is no
reason to omit them. Prefill runs a MultiConnector — Nixl for P/D transfer plus:

`OffloadingConnector` with `TieringOffloadingSpec`, 50 GiB of CPU KV per pod,
LRU, `offload_prompt_only`, and a P2P secondary tier — copied from the canary's
prefill MultiConnector in `glm_data/prefill LWS`.

Production's 28.1 TiB aggregate is that same per-pod figure
across a fleet of ~17 groups; at 2P+1D we get the **mechanism at
about 1/200th the capacity**, which is enough to test whether switchability
interferes with offload and P2P, and not enough to reproduce the 85% hit rate.

| run | arm | prefill groups (x2) | decode group | patterns |
| --- | --- | --- | --- | --- |
| 2.1 | A | `deepep_high_throughput` | `deepep_low_latency` | P1, P2, P3 |
| 2.2 | B | `deepep_v2` | `deepep_v2` | P1, P2, P3 |
| 2.3 | C | `deepep_v2`, switchable | `deepep_v2`, switchable | P1, P2, P3 |

**Additional metrics:** end-to-end TTFT *and* output throughput together, NIXL
transfer count and p90 (production reference: **471 ms p90, ~1 transfer per
request, zero failures**), queue wait at the picker, per-group KV utilisation.

#### Run 2.4 — re-roling a fleet whose ratio is wrong (arm C only)

The run that justifies the work. **2P+1D → 1P+2D**, mirroring production's
10P/7D → 9P/8D.

1. Drive P2 at a rate where **decode is the limiter** — production found
   exactly this, moving 10P/7D → 9P/8D because decode KV saturated first.
   Confirm it *is* the limiter from per-group KV utilisation before switching,
   rather than assuming the load shape produced it.
2. Record steady-state throughput and TTFT with the ratio wrong.
3. Switch **one of the two** prefill replicas into the decode role **and move its
   `llm-d.ai/role` pod label**, per `docs/guides/pd-role-switch/README.md`. The
   fleet keeps serving throughout on the remaining prefill group — which is the
   point, and is why 1P+1D could not have tested this.
4. Re-measure, and record what the CPU/P2P tier did across the switch: a
   re-roled replica's offloaded prompt KV becomes a P2P source for a role it no
   longer holds, and whether that is harmless is not known.

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
| set 1 prefill (1.1–1.6) | 2, sequential | ~5 h |
| set 1 decode (1.7–1.9) | 2, sequential | ~2 h |
| set 1 switch-under-load (1.10) | 2 | ~30 min |
| set 2 (2.1–2.4) | **6 per arm**, sequential | ~7–8 h |

Set 1 prefill is the long pole and the ladder is why: three arms x three
patterns x up to five rungs is about 45 measured runs. They share an engine
within an arm, so the cost is three engine starts plus the runs themselves —
but it is a day, not an afternoon, and worth knowing before booking nodes.

Weight load is ~106 s from node-local NVMe against 1403 s from the shared PVC —
`HF_HOME=/mnt/local/hf-cache` is not optional at this run count. Kermit has had
7 free 8-GPU H200 nodes; one set-2 arm fits, three concurrently do not.

---

## 7. What this does not establish

- **Fleet-level benefit.** 2P+1D is not 9P/8D. This establishes the mechanism
  and its cost, and that one group can change role while the fleet keeps
  serving — not what a 17-group fleet gains from doing it.
- **Cache-tier behaviour at production scale.** The CPU offload tier and P2P are
  configured as the canary configures them, so the mechanism is exercised. What
  is not reproducible is the capacity: 50 GiB per pod across three groups
  against production'''s 28.1 TiB across ~17. Expect hit rates nothing like the
  85% that defines this workload, and report what was achieved rather than
  comparing it to that figure.
- **EP=32.** Everything here is EP=16.
- **Whether switching is the right control action.** That needs a controller
  that does not exist yet.
