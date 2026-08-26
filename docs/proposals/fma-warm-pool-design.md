# Design: a shared warm pool for fast multi-model actuation

**Status: PARKED, and superseded in practice.** This design was never built.
A warm pool WAS built, on a different plan -- see
[the review](fast-model-loading.md) for the argument and
[the implementation design](fast-model-loading-implementation.md) for what
shipped as `internal/warmpool`. Read this document for its reasoning about
launcher-owned GPUs, not as a description of the system.

FMA itself is NOT superseded: the pool that shipped runs FMA's launcher as the
per-Pod engine supervisor (`llm-d-fast-model-actuation/launcher:v0.6.0-alpha.13`
in `config/warmpool/warmpool-deployment.yaml`).
**Supersedes the exploration in** [`fma-launcher-owned-warm-pool.md`](fma-launcher-owned-warm-pool.md),
which records why every alternative was rejected and should be read only for that
reasoning.
**Prerequisites are unmet** — see [Prerequisites](#9-prerequisites-before-any-of-this-is-built).
Two of them can invalidate the design, so they come before implementation.

---

## 1. What this is

A small, shared population of GPU-holding Pods that keep several models resident
but asleep, so that capacity for any of them arrives in **seconds instead of
~41 s**. WVA wakes a model when it scales up, and puts it back to sleep once
ordinary replicas take over.

It serves two needs with one mechanism:

- **burst** — cover a spike while ordinary replicas start;
- **parking** — keep a scaled-to-zero model wakeable without holding a GPU for it.

**Why a shared pool rather than warm capacity per model:** N models would need N
warm GPUs, which is the same as simply running them. The proposition is that
**K shared GPUs give fast-start coverage for N models**.

### When this pays, and when it does not

The pool is **insurance**. The premium is K GPUs held continuously; the payout is
`T_ordinary - T_wake` seconds of faster capacity, per spike.

```text
payout per spike  =  T_ordinary - T_wake        # today: ~41 s - ~3 s  =  ~38 s
premium           =  K GPUs, held whether or not a spike arrives
```

**Anything that reduces `T_ordinary` reduces the payout proportionally, without
touching the premium.** That is the design's central sensitivity and it must be
checked before building, because other work in llm-d attacks exactly that number
— see [§8](#8-relationship-to-the-snapshot-orchestrator). If ordinary replicas
start in 8 s rather than 41 s, the payout falls from ~38 s to ~5 s while the cost
stays identical.

A pool slot used once an hour to save 38 s has a duty cycle under 2 %. That is not
an argument against it — insurance is meant to sit idle — but it is the honest
frame for the cost conversation, and it means the pool is justified by **tail
latency and availability**, not by utilisation.

### Sizing K

K is the number of slots held concurrently, which is Little's law:

```text
K  >=  lambda x W

  lambda = spike arrival rate (spikes per second, across all models on the pool)
  W      = mean slot hold time = min(T_ordinary_start, maxHoldSeconds)
```

Both terms are measurable before anything is built: `lambda` from historical
scaling events, `W` from replica start times. Neither is currently measured, and
sizing K without them is guesswork.

### Scope and commitment

**Phase 1 builds both pool tiers** — level-1 (RAM-backed) and level-2
(storage-backed) — because which one wins depends on a bandwidth we have not
measured, and because the two differ only in a label, a memory request and the
argument to `/sleep`. Building one and not the other would save almost nothing and
would force the measurement to be re-litigated later.

**We commit to adopting the Snapshot Orchestrator's mechanism once it is ready.**
This design exists because that work is a declared prototype today (§8), not
because a RAM tier and a disk tier are rivals. The WVA side is therefore built so
the mechanism is swappable — one port, two adapters, tier intent rather than
mechanism calls — and the intended end state is WVA allocating across *both* tiers
with their agent owning the disk one.

### Scope: phase 1 is single-Pod

**Phase 1** covers models that fit within one Pod — any tensor-parallel size, so
long as the GPUs are on one node. That is the whole of this document apart from
§10.

**Phase 2** is `LeaderWorkerSet`: models spanning several nodes. It is deliberately
out of scope here. Its prerequisite is not yet checked and does not gate phase 1 —
see [§10](#10-phase-2-leaderworkerset).

### What it is not


Not a serving fleet. The pool covers the gap until ordinary replicas arrive, or
until a hold timeout expires. Ordinary Deployments continue to do the serving, and
if the pool is empty or has no warm copy of the model, behaviour degrades exactly
to what happens today: a cold start. Nothing goes `Pending`, so the pool can be
introduced incrementally and switched off without risk.

---

## 2. Architecture

**One pool = one ordinary Kubernetes Deployment.** No new controller, no new CRD.

| property | value | why |
| --- | --- | --- |
| GPUs per Pod | **exactly the TP size** of the models it serves | one awake instance then occupies the whole Pod, so one Pod hosts one *live* model — Pod readiness stays meaningful and one `llm-d.ai/model` label is unambiguous |
| instances per Pod | 1 awake + N asleep | `LauncherConfig.maxInstances` caps N |
| pool identity | (accelerator type, TP size, **sleep level**) | each is immutable per Pod, so each needs its own Deployment |
| pool size | `spec.replicas` | ordinary scaling; `kubectl scale` works |
| traffic switch | **Pod readiness gate** | asleep → gate false → EPP drops the Pod from its active set entirely. **Verified on kind: a NotReady Pod receives no dispatch and no polling** (§9.1). Order matters: gate false *before* `/sleep` |
| which model is live | `llm-d.ai/model` label | selects the InferencePool the Pod joins |

**Why one GPU (or one TP group) per Pod:** a 4-GPU Pod running four independent
TP=1 instances would need to express "model A serving, model B asleep" through a
single Pod readiness flag and a single model label. It cannot. One live model per
Pod keeps Services, EndpointSlices and InferencePool semantics untouched.

### Sleep level is a property of the pool

Pod resource requests are **immutable after admission**. A Pod that might hold a
level-1 model must reserve RAM for it *at creation*, before anything knows which
models will land there. So the level cannot be chosen per model at placement time.

| | **level-1 pool** | **level-2 pool** |
| --- | --- | --- |
| sleeping weights | offloaded to **host RAM** | **discarded**; re-read on wake |
| memory request | sized for the warm set | small, constant |
| warm set bounded by | **host RAM** | GPU residue and `--sleeper-limit` |
| wake cost | `weight / B_h2d` — storage-independent | `weight / B_storage` |
| suits | large models, or slow storage | small models, or fast storage |

**Level 2 is the default; level 1 is a workaround for slow storage.** As
`B_storage` approaches `B_h2d`, level 2 strictly dominates — the same wake time
without any of the RAM.

**`B_storage` measured, 2026-08-19.** Read a 1.5 GB safetensors file off
`/model-cache` from inside a launcher Pod, using `dd iflag=direct` to bypass the
page cache:

| | measured |
| --- | --- |
| backing store | **IBM Spectrum Scale (GPFS)**, `ReadWriteMany`, 1 TiB |
| **cold read, O_DIRECT** | **1.7 GB/s** steady (1.1 GB/s on the first run) |
| cached read | **5.6 GB/s** |

That is **4x faster than the ~430 MB/s previously assumed here.** The earlier
figure came from a ~2.8 s weight-load observation which evidently included open,
metadata and Python overhead rather than raw bandwidth. A shared RWX filesystem is
not automatically slow — GPFS on a fast fabric is within a few times of local NVMe.

With `B_storage = 1.7 GB/s` and a 5 s wake target, the crossover lands near
**~8.5 GB of weights**, so level 2 covers much more of the range than the earlier
estimate implied:

| model | weights | level 1 (~7 GB/s) | level 2 (1.7 GB/s) | level-1 RAM |
| --- | --- | --- | --- | --- |
| 0.6 B | ~1.5 GB | ~0.2 s | **~0.9 s** | 1.5 GB |
| 4 B | ~8 GB | ~1.1 s | **~4.7 s** | 8 GB |
| 8 B | ~16 GB | ~2.3 s | ~9.4 s | 16 GB |
| 70 B | ~140 GB | ~20 s | ~82 s | 140 GB |

**So on this cluster level 2 is the right default up to roughly 4 B parameters**,
and level 1 earns its RAM only above that.

**But `B_storage` is shared, and that measurement is single-reader.** 1.7 GB/s is
what *one* Pod gets. K Pods waking together divide it, so a level-2 wake degrades
precisely during a correlated burst — the case the pool exists for. Level 1 has no
such coupling, since each Pod restores from its own host RAM. That is an argument
for level 1 on pools sized for concurrent wakes, **independent of model size**, and
it is the part of `B_storage` still unmeasured.

---

## 3. Configuration

### A pool

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: warm-pool-h100-tp1-l2
  labels:
    llm-d.ai/warm-pool: "true"                      # WVA discovers pools by this
    llm-d.ai/warm-pool-level: "2"                   # 1 or 2; immutable per pool
    llm-d.ai/accelerator: NVIDIA-H100-80GB-HBM3
spec:
  replicas: 4                                        # K — the pool size knob
  template:
    spec:
      readinessGates:                                # WVA owns this condition
        - conditionType: llm-d.ai/WarmServing        # false while asleep -> out of the EndpointSlice
      containers:
        - name: launcher                             # the FMA launcher image, unmodified
          env:
            - name: VLLM_SERVER_DEV_MODE
              value: "1"                             # REQUIRED: /sleep and /wake_up exist only with this
          resources:
            limits:
              nvidia.com/gpu: 1                      # == TP size of this pool's models
              memory: 8Gi                            # level 2: buffers + process overhead only
          # No readinessProbe here: readiness is owned externally, through the
          # readinessGate declared below. Nothing needs a webhook, because this
          # Deployment is ours to author. A probe
          # inside the Pod cannot express "the controller has not admitted me
          # yet", and today's launcher probe answers 200 whenever the launcher
          # lives, regardless of whether its instance sleeps.
          volumeMounts:
            - name: model-cache
              mountPath: /model-cache
      volumes:
        - name: model-cache
          persistentVolumeClaim:
            claimName: model-pvc
```

A level-1 pool differs in exactly two fields:

```yaml
    llm-d.ai/warm-pool-level: "1"
    memory: 64Gi        # <- the warm-set budget; WVA admits models against this
```

### Sizing

```text
# level 1 — wake bounded by PCIe; RAM-hungry
pod memory  >=  SUM(weight size of every model in the warm set) + overhead
pod GPU     >=  awake instance + ~1.4 GiB per sleeper

# level 2 — RAM-cheap; wake bounded by the read path
pod memory  >=  small, ~MB per sleeper
pod GPU     >=  awake instance + ~1.4 GiB per sleeper

# the level itself is derived from a MEASURED bandwidth, not a size cutoff:
level 2  iff  weight_size / B_storage <= T_wake
```

**The Pod's memory request is the warm-set budget.** No `warmSlots` field: a byte
budget handles heterogeneous models naturally, where a slot count would either
waste capacity or overcommit it.

### Policy, per ScalingPolicy tier

```yaml
warmPool:
  eligible: true          # interactive tier yes, batch tier no
  maxSlots: 2             # cap per model, so one spike cannot consume the pool
  onExhausted: fallback   # fallback | queue | preempt-lower-tier
  maxHoldSeconds: 900     # release the slot even if no ordinary replica arrived
```

`onExhausted` and `maxHoldSeconds` exist because a slot may be held for a long
time — see [§4.5](#45-handover-from-pool-to-ordinary-replicas).

### Admission rule

WVA admits model M to pool P when:

```text
P.accelerator matches M
P.gpuCount    == M.tensorParallelSize
GPU residue and --sleeper-limit permit one more sleeper on the chosen Pod

and either
    P.level == 2  and  weight(M) / B_storage <= T_wake
or  P.level == 1  and  warmBytes(Pod) + weight(M) + overhead <= P.memoryRequest
```

If no pool admits M, the outcome is a cold start, and it must be **visible**: a
`reason` on `wva_model_scaling_blocked` (e.g. `no_warm_pool_admits_model`),
following this repo's convention that new diagnostics become reasons rather than
new gauges, since WVA owns no API object to carry a status condition.

---

## 4. Sequencing, and what each step costs

Provenance matters here, so each figure is marked: **[M]** measured on pokprod,
**[P]** published by vLLM, **[?]** not measured.

### 4.1 Pool creation

| step | cost |
| --- | --- |
| Deployment created, Pods scheduled | seconds; **[?]** on a busy cluster, unbounded if no GPUs are free |
| image pull (launcher image, first time per node) | **[?]** typically tens of seconds |
| launcher process start, GPU discovery via `pynvml` | seconds **[?]** |
| **Pod Ready with an empty warm set** | **[?]** — but off any critical path |

A new pool Pod arrives **empty**. It becomes useful only as models are admitted to
it. This is a provisioning operation, not a serving one.

### 4.2 New model admission (also: parking)

Adding a model to a Pod is **one call to a running launcher** — nothing is scaled
or restarted.

| step | cost |
| --- | --- |
| `POST /v2/vllm/instances` — launcher forks a vLLM process | sub-second **[?]** |
| process start, vLLM import, API startup | ~29 s **[M]** (the residue of a 41 s cold start after compile and weight load) |
| weight load from `/model-cache` | ~2.8 s observed **[M]**; raw bandwidth since measured at **1.7 GB/s cold** (§2), so most of that 2.8 s is open/metadata/Python overhead rather than transfer |
| `torch.compile` | 3.08 s **[M]** with the shared cache hitting; 12.35 s on a miss |
| engine init: profile, KV cache, warmup | 9.29 s **[M]** |
| `POST /sleep?level=N` | **[?]** |
| **total to warm-and-asleep** | **~41 s [M]**, entirely off the critical path |

**Parking a scaled-to-zero model is exactly this flow.** A terminating replica
cannot donate its warmth — the process dies with the weights — so parking is an
asynchronous re-creation, paid once, at the moment demand has already gone.

**Constraint:** the load needs transient GPU memory of roughly the full weight
size, while a sleeper rests at ~1.4 GiB **[M]**. On a card already serving at
`--gpu-memory-utilization 0.95` only ~4 GiB is free, so **models are added to
*idle* Pods**; a serving Pod has its warm set effectively frozen. Running pool Pods
at `0.85` leaves ~12 GiB free and lifts that restriction, at the cost of KV cache
for whatever is serving.

### 4.3 Wake — the step this whole design exists for

| | level 1 | level 2 |
| --- | --- | --- |
| Qwen3-0.6B | 0.26 s **[P]** | 0.85 s **[P]** |
| Phi-3-vision | 0.82 s **[P]** | 2.58 s **[P]** |
| observed on pokprod (level 1) | **2–3 s [M]** | — |
| scaling | `weight / B_h2d` | `weight / B_storage` |

Then the Pod must become Ready and reach the InferencePool: **[?]** the
label → EndpointSlice → InferencePool propagation is unmeasured and **may dominate
the wake itself**.

Against a cold start of ~41 s **[M]**, this is the ~15× saving that motivates
everything above.

### The failure this design exists to prevent, now measured

Attempting to wake the **unbound** sleeping launchers on pokprod — the ones whose
GPU was released when their requester went away — produced this:

| launcher | `POST /wake_up` |
| --- | --- |
| one | `CUDA Error: invalid argument at cumem_allocator.cpp:145` |
| another | **`CUDA Error: out of memory at cumem_allocator.cpp:139`** |
| a third | succeeded, and served correct output |

**Two of three sleeping instances could not be woken at all.** Not slowly, not
incorrectly — the call returns HTTP 500 and the instance stays asleep.

The mechanism follows from the dual-pods design. The launcher requests **no GPU**;
the *requester* holds it. When the requester goes away the GPU allocation is
released, but the sleeping instance's `cumem` handles still refer to memory on that
device. Another workload can then take it. On wake there is either nothing valid to
restore into (`invalid argument`) or no room to restore into (`out of memory`). The
third launcher woke because its card happened to still have space.

**This is the strongest argument for the design in this document.** A warm pool of
GPU-*holding* Pods cannot hit either error: the device stays allocated to the Pod
for its lifetime, so the memory a sleeper needs is reserved by construction and
nobody else can take it. Holding the GPU is not merely what makes the wake *fast* —
it is what makes the wake *possible*.

It also re-explains this project's measurement history better than the GPU-UUID
account did. Runs recorded 0 woke / 3 rebuilt, twice, then 1 woke / 2 rebuilt. If
roughly one sleeper in three is wakeable because its card still has room, those
numbers are exactly what one would expect — and no GPU-identity mismatch is needed
to explain them. Both effects are real; this one is larger and had been invisible.

### 4.4 Sleep

| step | cost |
| --- | --- |
| drain: mark not-ready, let in-flight requests finish | **[?]**, bounded by request duration |
| `POST /sleep?level=N` | **[?]** |
| Pod leaves the EndpointSlice | seconds **[?]** |

### 4.5 Handover from pool to ordinary replicas

1. WVA wakes the pool instance and labels the Pod in — serving in **~3 s [M]**.
2. In parallel, it scales the ordinary Deployment.
3. Ordinary replicas start — **~41 s [M]** each, **and unbounded if no GPUs are free**.
4. When they are **serving** (not merely `Ready`), WVA sleeps the pool instance.

**Hand over on *serving*, not `Ready`.** `readyReplicas` is not the same condition
as being in the EndpointSlice and reachable through the router;
`hack/benchmark/wait_serving.sh` exists because a 503 in that window destroyed
whole benchmark runs.

**The bridge needs GPUs on both sides at once.** On a full cluster the ordinary
replicas never schedule, the bridge never completes, and the pool silently becomes
permanent capacity. `maxHoldSeconds` bounds it; `onExhausted` decides what happens
next.

### 4.6 Pool scaling

| | cost and consequence |
| --- | --- |
| scale **up** | a new empty Pod, per §4.1; it fills by admission or by use |
| scale **down** | **destroys warm sets.** Prefer Pods whose models are warm elsewhere; never a Pod currently serving |

### 4.7 How a Pod comes to hold several models

- **Assigned** — WVA places a model when parking it or when it predicts a spike.
- **Accumulated** — a Pod that served model Y keeps Y asleep afterwards instead of
  deleting it, so the warm set drifts toward models actually used: an LRU cache of
  weights, filled by real traffic.

Eviction is the counterpart, and is policy: least recently used, lowest tier,
least likely to spike.

### Summary of the time budget

| operation | cost | on the critical path? |
| --- | --- | --- |
| wake a warm model | **0.26–3 s** [P]/[M] | **yes — the point of the design** |
| label → InferencePool propagation | **[?]** | **yes, and unmeasured** |
| sleep a model | **[?]** sub-second + drain | no |
| admit / park a model | **~41 s** [M] | no |
| create a pool Pod | **[?]** tens of seconds | no |
| ordinary replica start | **~41 s** [M], unbounded if GPU-starved | it is what the pool hides |

---

## 5. What is reused from FMA, and what is built

### Reused, unmodified

| component | role |
| --- | --- |
| `inference_server/launcher/launcher.py` (967 lines) | the multi-process vLLM host: create/delete instances, pin each to GPUs by translating `gpu_uuids` into `CUDA_VISIBLE_DEVICES` (`:176-191`), list them, and stream changes over a Kubernetes-watch-style NDJSON endpoint. **No Kubernetes client, no Pod concept, no requester vocabulary.** |
| `launcher/gputranslator.py` (247) | UUID↔CUDA-index mapping via `pynvml`; how a GPU-owning Pod discovers its own cards with no requester to report them |
| `dockerfiles/Dockerfile.launcher.*` | the launcher image |
| `LauncherConfig.maxInstances` | already means "instances per launcher Pod", i.e. one awake plus N asleep |
| vLLM `/sleep`, `/wake_up`, `/is_sleeping` | the actual mechanism, on each instance's own port — requires `VLLM_SERVER_DEV_MODE=1` |

**The launcher API has no sleep/wake endpoints and needs none** — sleep and wake
are calls to the vLLM instance itself, which is what FMA's controller already
does (`pkg/controller/dual-pods/inference-server.go:1565`, `:1780`, `:2053`).

### Not needed — the entire Kubernetes half of FMA

Because a pool is an ordinary Deployment:

- `dual-pods-controller` (~3.5k lines, the bulk of the Go) — nothing binds
  requesters to providers;
- `launcher-populator` and `LauncherPopulationPolicy` — the Deployment controller
  places Pods;
- requester binary, `pkg/spi`, `pkg/server/requester/{probes,proxy}` — no
  requesters exist;
- `pod-helper.go` and its `removeGPUResourceLimits` — **never called**; we author
  the pool Deployment and simply request the GPU.

**The FMA fork is therefore not on the critical path.** Commit `aa072ef` fixes a
reclaim path this architecture does not use. It stays correct for the *current*
deployment model, but nothing here waits on it.

### To be built

| piece | size | where |
| --- | --- | --- |
| pool Deployment manifests | config | GitOps, beside the models |
| **readiness gate** | **config only — no webhook** | A `readinessGate` declared directly in the pool Pod template, whose condition only WVA sets. **We author the pool Deployment, so nothing needs mutating.** The Snapshot Orchestrator needs a webhook only because it must inject the gate into Pods created by somebody else's Deployment; that requirement does not apply here. Strictly better than an in-Pod probe, which cannot express "not admitted yet". |
| pool discovery and warm-set state | small | WVA — `GET /v2/vllm/instances` on each pool Pod already returns exactly this, so no sidecar or annotation is needed |
| actuation: wake/sleep, patch the model label, gate readiness | small | WVA, in the loop and RBAC it already has |
| **allocation policy** — which models are warm where, spike anti-affinity, eviction, exhaustion, hold timeouts | the real work | WVA |

---

## 6. Constraints that shape the policy

**Awake-slot contention, and what it does to the headline claim.** Models
co-resident on a card are cheap in memory (~1.4 GiB asleep) but **contend for the
single awake slot**: while a Pod serves X, its other warm models cannot be woken —
for the whole hold, which may be long.

So "K GPUs give fast-start coverage for N models" holds **only while bursts do not
overlap**. Two models warm on the same card that spike together get one wake and
one cold start. That is a real qualification of the pitch, not a footnote: a burst
buffer is wanted precisely when demand is bursty, which is when overlap is most
likely.

Two consequences. The warm set wants **anti-affinity** — spread models that may
spike together across Pods, and co-locate only models unlikely to spike
simultaneously; dense packing is the wrong instinct. And **K must be sized against
concurrent holds** (Little's law, §1), not against the number of models kept warm,
because it is concurrency and not breadth that exhausts the pool.

**Large models may not pay.** At 70 B, level 1 costs ~20 s and ~140 GB of host RAM
per Pod, while level 2 costs minutes. A cold start or a permanently running replica
is likely better. This is a real boundary on applicability, independent of hardware.

**Attribution must not double-count.** During a handover, capacity is genuinely
doubled. If WVA counts pool instances as durable supply it will suppress the very
scale-up they exist to cover — the same failure mode as pending replicas counting
toward anticipated supply.

---

## 7. Failure modes

Each of these is silent unless it is deliberately surfaced, and this repo's
convention is that new diagnostics become a `reason` on
`wva_model_scaling_blocked` rather than a new gauge, since WVA owns no API object
to carry a status condition.

| failure | effect | handling |
| --- | --- | --- |
| **pool Pod evicted, OOM-killed or drained** | its **entire warm set is lost**; coverage silently drops and the next spike for those models cold-starts | detect by diffing intended against observed warm sets; re-admit elsewhere; report |
| **wake fails or times out** | the spike is uncovered while WVA believes it is covered | bound the wait, fall back to the ordinary cold path, report `warm_wake_failed` |
| **woken engine serves wrong output** | worst case: fast, wrong answers | see [§9](#9-prerequisites-before-any-of-this-is-built) — this is a prerequisite, not a runtime concern |
| **ordinary replicas never schedule** | the bridge never ends; the pool becomes permanent capacity | `maxHoldSeconds` then `onExhausted` |
| **pool exhausted** | later spikes get no slot | `onExhausted`: fallback, queue, or preempt a lower tier |
| **model admission fails** (no idle Pod with headroom) | the model is never parked, and nobody notices until it spikes | report `no_warm_pool_admits_model`; retry when a Pod goes idle |
| **two controllers manage one Pod** | the warm pool and a snapshot agent both drive `/sleep` and readiness | must be prevented by construction — see [§8](#8-relationship-to-the-snapshot-orchestrator) |

**Losing a pool Pod is the most under-appreciated of these.** Warm capacity is
in-memory state with no persistence, so an eviction destroys minutes of accumulated
loading with no error anywhere. It is the counterpart of the coverage decay this
repo already measured on the current architecture.

## 8. Relationship to the Snapshot Orchestrator

`Snapshot Orchestrator: Kubernetes-Native Product Design` attacks the same problem
— vLLM cold start — from the opposite direction, and the two designs are more
complementary than competing. This section exists because it materially affects
whether the warm pool is worth building.

**Its mechanism:** after a vLLM server has fully warmed up, freeze it to **disk**
as a golden snapshot — vLLM level-2 sleep, then NVIDIA `cuda-checkpoint` to pull
GPU driver state into host memory, then CRIU to dump the process tree. Later Pods
for the same model **restore** instead of cold-booting, keyed by a compatibility
tuple (model, TP, image digest, vLLM version, GPU architecture, driver, kernel).

### What actually exists today, as opposed to what is designed

Checked directly rather than taken from the architecture document.

**`github.com/wseaton/snapshot-orchestrator`** — the prototype:

- **Rust**, in `crates/`: an orchestrator daemon, a snapshot API, a node agent
  (`vllm-snapshot-agent`, a privileged DaemonSet doing CRIU dump/restore with
  native container-runtime integration) and a supervisor/shim.
- **CRDs are `SnapshotReplica` and `SnapshotFleet`** — *not* the
  `SnapshotCheckpoint` the architecture document describes. The design is still
  moving, so its API surface should not be treated as settled.
- Describes itself as a proof-of-concept for "non-destructive CRIU snapshot fan-out
  for vLLM", and states plainly: **"This project is experimental and is not
  intended for production use."** 29 commits on main.
- Requires **custom patches to vLLM, Model Express and the llm-d router**
  (`integrations/`), so it is not yet consumable as an add-on.
- Checkpoints to **local NVMe** — consistent with the storage-bandwidth analysis in
  §3: the approach depends on a fast local read path, and would degrade on shared
  network storage exactly as level-2 sleep does.
- Ships a demo console with an unauthenticated REST/WebSocket API — prototype
  hygiene, not a deployable surface.

**Upstream vLLM: nothing merged.** [RFC #34303](https://github.com/vllm-project/vllm/issues/34303)
(CUDA Checkpoint/Restore for near-zero cold starts, opened February 2026) is a
**draft** with a five-phase roadmap and no implementation merged. Its constraints
matter for both designs:

- Linux x86_64, **NVIDIA driver 570+** (580+ for GPU remapping);
- **cannot support UVM or IPC memory allocations**;
- **tensor parallelism**: NCCL communicators are destroyed by the checkpoint and
  need ~1-3 s of re-initialisation, "potentially negating benefits at large TP
  scales (TP=8)";
- **multi-node explicitly deferred** — the same conclusion this design reached
  independently for LWS (§10).

Also [PR #51316](https://github.com/vllm-project/vllm/pull/51316), the gRPC control
plane that would replace dev-mode HTTP for sleep and weight reload, is **in
flight** and stacked on another PR — so both designs depend on
`VLLM_SERVER_DEV_MODE=1` for now.

**What this means for sequencing.** Their mechanism is a promising prototype whose
API is unsettled, which needs forked vLLM, llm-d and Model Express, whose
foundational vLLM support is an unmerged draft, and which requires a privileged
node agent. That is a reasonable thing to adopt later and an unreasonable thing to
block on. It is the argument for building the RAM and storage tiers now behind a
swappable port, and it is not an argument against their design.

### A calibration that narrows our own claim

RFC #34303 gives a cold-start breakdown that includes **GPU transfer at 2-10 s**,
and estimates suspend/resume at **4-10 s against 6-32 s for existing sleep/wake**.
Those figures are far above the 0.26-0.85 s the sleep-mode blog reports — because
the blog measured **small** models (Qwen3-0.6B, Phi-3-vision) and the RFC is
describing large ones.

This is the `size / B_h2d` scaling of §3 seen from another angle, and it is
independent corroboration of the formula. But it also narrows the pool's advantage
at the top end:

| model scale | level-1 wake | CUDA-checkpoint resume | gap |
| --- | --- | --- | --- |
| small (≤1 B) | **~0.3 s** | ~4 s | **large — the pool wins clearly** |
| medium | ~1-3 s | ~4-8 s | meaningful |
| large (TP=8) | ~6-32 s, plus NCCL re-init | ~4-10 s, plus NCCL re-init | **converges; the pool may not win at all** |

So the pool's advantage is **largest for small and medium models** and may vanish
for the largest. Combined with §6's note that a 70 B model costs ~20 s and ~140 GB
of RAM per Pod at level 1, the conclusion is consistent from two directions:
**warm-pooling is a small-to-medium-model technique.**

### Where the two agree, independently

| | both designs |
| --- | --- |
| primitive | vLLM sleep, and both run into `VLLM_SERVER_DEV_MODE=1` |
| sleep level | **level 2** — theirs to keep the checkpoint small, ours to avoid host RAM |
| traffic gating | keep non-serving Pods out of the Inference Gateway |
| lifecycle state | Pod labels |
| posture | an add-on layer; neither owns the serving data plane, both operate on Pods created by ordinary Deployments |
| **LWS / multi-node** | **explicitly deferred to a later phase**, for the same TP>1 / NCCL reasons |

That convergence is worth noting: two independent designs reached the same
conclusions about level 2, about label-based lifecycle, about not owning the data
plane, and about deferring multi-node.

### Where they differ, and why it matters

| | warm pool (this doc) | snapshot orchestrator |
| --- | --- | --- |
| GPU held while idle | **yes — this is the cost** | **no** |
| latency | **0.26–3 s** | slower: skips CUDA init, warmup and compile, but **still reloads weights** |
| capacity | bounded by K | **unbounded** — any new Pod can restore |
| extra state | none | a checkpoint per (model, config) on disk |
| privileged component | **none** | a DaemonSet using CRIU and `nsenter` |
| survives upgrades | yes, a live process | **no** — the compatibility key includes driver, kernel and image digest |

Their document is explicit that *"the win is skipping CUDA init, warmup, and
compile rather than weight load"*. Against our measured breakdown of a ~41 s cold
start — ~29 s process start and import, 9.29 s engine init, 3.08 s compile, ~2.8 s
weight load — a restore plausibly lands near **5-10 s**.

### The consequence for this design

**Snapshot lowers the floor for every replica; the warm pool removes it for K.**
So snapshot does not compete with the pool, it **shrinks the pool's job** twice
over:

- the payout per spike falls from ~38 s to perhaps ~5 s (§1), while the premium of
  K held GPUs is unchanged;
- `W`, the mean hold time, falls with `T_ordinary_start`, so **K itself can be
  smaller** (Little's law, §1).

Both effects point the same way: **if snapshot lands and delivers ~8 s restores,
build a much smaller pool, or possibly none.** That makes snapshot's actual restore
latency a first-class input to this design, and it is not yet measured — their
prototype has validated the sleep → `cuda-checkpoint` → CRIU chain end to end, but
no restore timings are quoted.

### Composition: they layer in time

The two systems cover **different parts of the same latency curve**, so the useful
configuration is both, not either.

```text
t=0        spike arrives
t~3s       warm pool serving           <- pool covers the head
t~8s       restored ordinary replica serving   <- snapshot covers the tail
t~8s+      pool slot released, back to standby
```

Today that curve is: nothing until ~41 s. With snapshot alone: nothing until ~8 s.
With both: **~3 s to first token, ~8 s to durable capacity.**

And the composition is self-reinforcing rather than merely additive. Because
`W`, the mean slot hold time, becomes the snapshot restore time instead of a cold
boot, Little's law (§1) gives:

```text
K  >=  lambda x W        W: ~41 s  ->  ~8 s      so K falls by ~5x
```

**Snapshot makes the pool cheaper at the same coverage.** A pool that needed 5
slots needs 1. That is the strongest argument for building the pool *after*
snapshot rather than instead of it — and for sizing it against snapshot's restore
time rather than against today's cold boot.

### Composition: snapshot could make warm sets durable

The worst failure mode in §7 is that an evicted pool Pod destroys its whole warm
set — minutes of accumulated loading, in memory, with no persistence. Snapshot is
exactly a persistence mechanism for warmed vLLM state, so in principle a pool Pod
could be checkpointed and restore its warm set instead of re-loading each model at
~41 s apiece.

**Not available today, for a concrete reason:** their `SnapshotCheckpoint` is keyed
by a compatibility tuple *(model, TP, image digest, vLLM version, GPU arch, driver,
kernel)* — one model per checkpoint. A pool Pod hosts several models at once, so it
does not fit that key. Recorded as a real future opportunity that would need their
CRD to describe a *set* of models, not as something to assume.

### Compatibility: what has to hold for both to run in one cluster

| concern | status |
| --- | --- |
| **Readiness gates** | **Compose correctly.** Kubernetes ANDs all readiness gates, so a Pod carrying both `snapshot.llm-d.ai/Serving` and a warm-pool gate is Ready only when both agree. No conflict by construction. |
| **Label namespaces** | No collision: `snapshot.llm-d.ai/*` against `llm-d.ai/warm-pool*`. |
| **Level-2 sleep semantics** | Shared dependency, and both rely on the CUDA VMM (`cumem`) allocator that sleep mode uses. Their prototype has validated that chain end to end, which is evidence for our design too. |
| **Control plane** | Both currently depend on dev-mode HTTP (`VLLM_SERVER_DEV_MODE=1`). vLLM PR [#51316](https://github.com/vllm-project/vllm/pull/51316) replaces it with gRPC. A shared dependency worth aligning on rather than each working around. |
| **Container entrypoint** | **Conflict.** Snapshot requires its shim as PID 1 and never execs, because CRIU restores processes with their dump-time PIDs. A pool Pod runs the FMA launcher as its entrypoint. Making one Pod both would mean the shim spawning the launcher, which changes the process tree CRIU captures. Untested. |
| **The `llm-d.ai/model` label** | **Conflict.** The pool *swaps* this label as the live model changes; snapshot treats model identity as part of an immutable compatibility key. A Pod cannot do both. |
| **Who calls `/sleep`** | **Conflict.** Two controllers driving the same endpoint on the same process, with different levels and different intent. |

### The invariant, and where it binds

The conflicts above are all **per-Pod**, not per-cluster or per-model:

> **A given Pod is owned by the warm pool or by the snapshot orchestrator, never
> both. The two systems compose at the level of a MODEL's lifecycle, not within a
> single Pod.**

Enforced, not merely documented: a pool Pod must not carry
`snapshot.llm-d.ai/managed`, and WVA must refuse to adopt a Pod that does. The
reverse guard belongs on their side.

With that boundary held, the intended steady state for one model is:

- its **ordinary Deployment** is snapshot-managed, so replicas restore in ~8 s;
- its **warm copy in the pool** is WVA-managed, so the first seconds are covered;
- neither system sees the other's Pods, and they meet only in the InferencePool,
  where both are just endpoints.

### Choosing between them

| situation | use |
| --- | --- |
| many models, infrequent bursts, cost-sensitive | **snapshot only** — no GPU held idle |
| few models, strict tail-latency requirement | **pool**, sized by Little's law |
| strict latency across many models | **both**, with K sized against snapshot's restore time |
| scale-to-zero with fast return | **either**; the pool returns faster, snapshot costs nothing while parked |

Note the last row: scale-from-zero is the case where the two are closest
substitutes, and where snapshot's zero idle cost is most attractive.

### Decision: build this, and make the WVA side migration-ready

We implement this design now rather than waiting on the Snapshot Orchestrator, and
we shape the WVA side so that adopting their mechanism later is an adapter swap
rather than a rewrite. The reasoning: our niche is real (§8, the RAM tier is the
only thing that avoids the weight load), their timeline is not ours to control, and
the seams that make migration cheap are cheap to install now and expensive to
retrofit.

**The one decision that determines migration cost: what WVA emits.**

| | migration cost |
| --- | --- |
| brain emits *mechanism calls* — "POST `/sleep` to this Pod" | rewrite the brain |
| brain emits **tier intent** — "model X warm at tier RAM on node N" | swap one adapter |

So WVA's output is **desired tier**, never a mechanism call:

```text
llm-d.ai/desired-tier: ram | disk | none      # written by WVA, per (Pod, model)
llm-d.ai/observed-tier: ram | disk | none     # written by whatever actuates
```

That is deliberately the same shape as their design's split between desired
configuration and agent-reported state, and it maps onto a memory hierarchy in
which their snapshot is simply the `disk` tier:

```text
GPU        serving
host RAM   level-1 sleep     <- tier "ram": this design
disk        CRIU snapshot     <- tier "disk": theirs, when it lands
cold        weight load from storage
```

### The mechanism interface to isolate

The brain must **not** call the launcher API or vLLM endpoints directly. One
narrow port, with the FMA-based implementation behind it:

```text
ListWarm(pod)                  -> []{model, tier, lastUsed}
Warm(pod, model, tier)          # create the instance and sleep it at that tier
Activate(pod, model)            # wake; make it the live model
Deactivate(pod, model)          # sleep; drop out of the InferencePool
Evict(pod, model)               # remove entirely, freeing budget
```

Today: launcher HTTP (`/v2/vllm/instances`) plus vLLM `/sleep` and `/wake_up`.
Later: their node agent, unchanged above the port. Everything in §4's sequencing
is expressed in these five calls, and nothing above them knows about CRIU, the
launcher, or dev-mode HTTP.

### Vocabulary aligned to theirs, so state means the same thing

Reuse their lifecycle phase names wherever the meaning is identical, rather than
inventing parallel ones:

| their label | ours | note |
| --- | --- | --- |
| `snapshot.llm-d.ai/state`: `serving`, `draining`, `failed` | `llm-d.ai/warm-state`, same values | identical meanings; a later merge is a key rename |
| `snapshot.llm-d.ai/startup-method`: `cold-booted`, `restored` | same key, plus `woken` | our RAM tier adds one value rather than a new key |
| `snapshot.llm-d.ai/checkpoint-ref` | `llm-d.ai/warm-ref` | points at the warm entry rather than a CR |
| readiness gate `snapshot.llm-d.ai/Serving` | a gate of the same shape | gates AND, so both can coexist during migration |

**Record their compatibility key on every warm entry** — *(model, TP, image digest,
vLLM version, GPU architecture, driver version, kernel version)* — even though a
RAM-tier entry does not strictly need it. It costs nothing, and it means promoting
a RAM-tier entry to a disk snapshot later is a tier change rather than a
re-derivation. Without it, migration has to reconstruct identity for every warm
model.

### The entrypoint, decided now to avoid a later conflict

Their shim must hold **PID 1 and never exec**, because CRIU restores processes with
their dump-time PIDs. Our pool Pods would naturally run the launcher as PID 1,
which is the one incompatibility that cannot be papered over later.

**So adopt the shim-shaped entrypoint from the start:** a thin PID-1 process that
spawns the launcher as a child and stays resident, reaping and forwarding signals.
It costs almost nothing now, is useful anyway for signal handling, and removes the
blocking conflict at migration time.

### What migration actually looks like, then

| layer | changes when their agent lands? |
| --- | --- |
| allocation policy, tiering decisions, K sizing | **no** |
| `desired-tier` contract and labels | **no** |
| the five-call mechanism port | **no** — same signatures |
| the adapter behind it | **yes** — launcher HTTP becomes their agent |
| pool Pod spec | mostly no; add their `managed` annotation, drop ours |
| the `disk` tier | **new capability, not a replacement** |

The result is additive: their arrival gives WVA a second tier to allocate across,
rather than invalidating what was built. And the brain — which is the part that
does not exist anywhere else today (§8) — is untouched by the swap.

### What to adopt from it regardless### What to adopt from it regardless

1. **The readiness gate** as the mechanism for externally-owned readiness — but
   **not** their webhook. They need a mutating webhook because they inject the gate
   into Pods created by somebody else's Deployment. We author the pool Deployment,
   so the gate is declared in the template and no admission plumbing, TLS or
   certificate rotation is required. Adopted in §2 and §5.
2. **vLLM PR [#51316](https://github.com/vllm-project/vllm/pull/51316)** — a gRPC
   control plane for sleep and weight reload, targeting the Rust frontend. Today
   both designs depend on dev-mode HTTP endpoints (`VLLM_SERVER_DEV_MODE=1`), which
   is not a durable interface. This is the replacement to track.

## 9. Prerequisites, before any of this is built

**Four of five answered; none invalidated the design.** All five
concern phase 1; the phase-2 prerequisite is in [§10](#10-phase-2-leaderworkerset).

1. ~~**Does a NotReady Pod actually stop receiving EPP traffic?**~~
   **ANSWERED 2026-08-19 — yes. Measured on kind against a live InferencePool and
   EPP** (`inference.networking.k8s.io`, EPP with `FailOpen`).

   Method: a backend Pod matching the pool selector, held NotReady by an
   unsatisfied `readinessGate`, then the gate satisfied as a control arm. The
   measurement instrument was validated first — a request sent straight to the Pod
   was confirmed to leave a trace — so a "no traffic" reading means something.

   | arm | EPP interaction with the Pod |
   | --- | --- |
   | gate false (NotReady) | **zero** — no dispatch, and no polling either |
   | gate true (Ready) | continuous polling, ~20 `GET /` per second |

   EPP does not merely decline to dispatch to a NotReady Pod; **it does not touch
   it at all.** So the traffic switch this design relies on is sound, and no EPP
   filter is required.

   **Caveat on what was proven.** No user request completed successfully in either
   arm, because the probe backend was `agnhost` rather than a vLLM and returns 404
   for `/v1/completions`. What is established is *admission* — whether EPP
   considers the Pod part of its active set — not the response path. For this
   design's safety question, "will a sleeping Pod receive user traffic", admission
   is the property that matters and it fails closed.

   **And a design requirement falls out of it.** Readiness gates *admission*; it
   does nothing about a Pod that is Ready but cannot serve. That is precisely the
   historical failure here — launchers retaining `inferenceServing=true` while
   unable to answer, giving ~20 % 503s. So the ordering is load-bearing:

   > **Set the gate false and wait for EPP to drop the Pod BEFORE calling
   > `/sleep`.** Sleeping first leaves a window in which the Pod is Ready and
   > asleep, which is exactly the 503 condition.

   The reverse ordering applies on wake: `/wake_up`, confirm the engine answers,
   *then* set the gate true.

2. ~~**Does a woken engine return CORRECT output?**~~ **ANSWERED 2026-08-19 — yes,
   byte-identical.** Tested on pokprod with greedy decoding (`temperature: 0`),
   comparing a woken instance against a never-slept instance of the same model as
   reference:

   ```text
   reference (never slept):  "Paris. The capital of France is also the capital of
                              the French Republic. The capital of France is ..."
   after /wake_up:          " Paris. The capital of France is also the capital of
                              the French Republic. The capital of France is ..."
   ```

   Identical. Issues [#16234](https://github.com/vllm-project/vllm/issues/16234)
   and [#17103](https://github.com/vllm-project/vllm/issues/17103) do **not**
   reproduce on the engine in use here. The risk that a woken pool serves fast
   garbage is retired.

3. ~~**Measure `B_storage` properly.**~~ **ANSWERED 2026-08-19.**
   `dd iflag=direct` against `/model-cache` from inside a launcher Pod:
   **1.7 GB/s cold, 5.6 GB/s cached**, on IBM Spectrum Scale (GPFS) RWX — 4x
   faster than assumed, which moves the level-1/level-2 crossover from ~2 GB to
   ~8.5 GB of weights (§2). **Still open:** how that bandwidth divides under
   *concurrent* wakes, which is the case that decides level 1 versus level 2 for a
   burst.

4. ~~**Measure the label → EndpointSlice → InferencePool propagation delay.**~~
   **ANSWERED 2026-08-19 — sub-second in both directions**, measured on kind
   against the live EPP by timing log arrivals at a probe backend:

   | direction | measured |
   | --- | --- |
   | **admit** — gate false→true until EPP's first request | **462 ms** |
   | **drain** — gate true→false until EPP stops | **631 ms** |

   So propagation does **not** dominate a wake, and the ~3 s figure this document
   worried about was an artefact of the first attempt: patching Pod conditions with
   a JSON *merge* patch replaces the whole array and wipes `Ready`, which produced a
   spurious 3.7 s. Pod conditions carry `patchMergeKey: type`, so a **strategic**
   merge is required to add one condition without clobbering the others — worth
   knowing for the implementation, since WVA will be doing exactly this patch.

   **Operational consequence:** after gating false, wait ~1 s (2 s for margin)
   before calling `/sleep`. That is the ordering requirement from §9.1, now with a
   number attached.

5. ~~**Confirm several instances co-reside on one card.**~~ **ANSWERED
   2026-08-19 — yes, and the memory arithmetic matches.** A second instance was
   created on the *same* GPU as an existing sleeper on pokprod, pinned by
   `gpu_uuids`, on its own port:

   | state of `GPU-1bb953f9` | used |
   | --- | --- |
   | one sleeper only | **1399 MiB** — the ~1.4 GiB figure, confirmed directly |
   | plus a second instance at `--gpu-memory-utilization 0.15` | **14339 MiB** ≈ 1.4 GB + 0.15 x 81.5 GB |
   | after deleting the second | **1399 MiB** — no leak |

   `total=2 running=2`, both reporting the same GPU UUID, and **the first
   instance's `is_sleeping=true` was undisturbed** throughout. So one card hosts
   several instances with independent state, which is what the warm set requires.

   **Deliberately not tested: the 0.95 case.** Creating an instance at the ISC's
   real `--gpu-memory-utilization 0.95` would have claimed a whole 80 GB card
   outside Kubernetes' accounting on a shared cluster, which could break an
   incoming tenant Pod. What remains unverified is therefore the memory *ceiling*
   — one awake instance at 0.95 plus N sleepers — which is arithmetic (§2) rather
   than a mechanism question. The mechanism is confirmed.

   Also unverified: sleeping *both* instances. The second was still initialising
   when the window closed — `status=running` from the launcher means the process
   started, not that the engine is serving, which is worth remembering when
   building the actuator.

---

## 10. Phase 2: LeaderWorkerSet

Out of scope for phase 1, recorded so the shape is known and the open question is
not rediscovered.

Models too large for one node are served by `LeaderWorkerSet` — a leader plus
workers forming one group. WVA already treats LWS as a scale target
(`internal/collector/locator/walk.go` walks ownerReferences to `Deployment` or
`LeaderWorkerSet`), so the policy half generalises for free.

The mechanism generalises too, with the unit becoming a **group** instead of a Pod:

| phase 1 | phase 2 |
| --- | --- |
| pool Pod owns N GPUs | pool **group** owns N Pods across nodes |
| one awake instance per Pod | one awake instance per **group** |
| Pod readiness gates the endpoint | **leader** readiness gates it; LWS already has group-level readiness |
| `llm-d.ai/model` on the Pod | the same label on the leader |
| `spec.replicas` sizes the pool | `replicas` x `size`; `replicas` still sizes the pool |

**The prerequisite, to be answered when phase 2 starts:** does sleep/wake work
across NODES? vLLM claims tensor- and pipeline-parallel support, but its docs never
mention Ray or cross-node, every published benchmark is single-node (the largest
being Qwen3-235B at TP=4 on one host), and
[#21231](https://github.com/vllm-project/vllm/issues/21231) reports
`--enable-sleep-mode` having no effect on Ray workers in a multi-node deployment.

If it does not work, phase 2 ends there: a warm group would hold *full* GPU memory
and could keep exactly one model warm, which is plain over-provisioning with none
of the multi-model sharing that justifies a pool.

**A second obstacle waits behind it.** FMA's launcher is a per-Pod process manager,
so a multi-node instance needs create/sleep/wake coordinated across every Pod of
the group. That is the one place where "we only reuse the launcher" stops holding.

**And the economics invert in both directions.** A warm group holds an entire
multi-node allocation, far more than a one-GPU Pod — but multi-node models have the
longest cold starts, paying Pod scheduling across nodes, image pulls, distributed
rendezvous *and* a large weight load. The cheap first step, needing none of this
machinery, is to measure that cold start.

## 11. References

**vLLM**

- [Sleep Mode](https://docs.vllm.ai/en/latest/features/sleep_mode/) — levels,
  `VLLM_SERVER_DEV_MODE=1`, `wake_up(tags=[...])`, and the level-1 CPU-memory
  requirement
- [Zero-Reload Model Switching with vLLM Sleep Mode](https://vllm-project.github.io/2025/10/26/sleep-mode.html)
  — the level-1/level-2 benchmark table
- Issues: [#16234](https://github.com/vllm-project/vllm/issues/16234),
  [#17103](https://github.com/vllm-project/vllm/issues/17103),
  [#21231](https://github.com/vllm-project/vllm/issues/21231)

**This repo**

- [`fma-launcher-owned-warm-pool.md`](fma-launcher-owned-warm-pool.md) — the
  exploration, and why each alternative was rejected on evidence
- [`fma-shared-warm-pool.md`](fma-shared-warm-pool.md) — measured sleeper
  economics, `--sleeper-limit`, and why allocation belongs in WVA
- [`fma-fork-problem-statement.md`](fma-fork-problem-statement.md) — the defects
  in the current dual-pods path, and Fix 1 as landed
- [`../guides/fma/`](../guides/fma/) — operating the current
  design, and `warm_pool.sh coverage` for the number that predicts a wake
