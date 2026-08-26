# Warming an engine that spans Pods (LeaderWorkerSet)

Status: proposal. **The gate in §3 has now been run and it passed** -- multi-node
sleep and wake work, on the fleet's own image, without Ray. Read §3 for what was
measured before costing the work below.

## 1. The question

A warm pool Pod is one Pod holding several engines. A LeaderWorkerSet engine is
several Pods holding one engine. Today `Demand` skips any variant whose scale
target is not a Deployment, so an LWS model never reaches the pool at all — it
now says so explicitly rather than failing with "cannot read its scale target",
but it is still a refusal.

The models that most need warming are exactly the ones that use LWS: a 405B
engine's cold start is minutes, not the ~35 s a single-Pod model costs. So the
value is highest precisely where the mechanism does not reach.

## 2. What an LWS engine actually looks like

Three facts decide the design, all confirmed against upstream:

- **The group is the unit.** `replicas` counts GROUPS; `size` counts Pods per
  group. Scaling adds and removes whole groups.
- **Only the leader serves.** The HTTP API runs on the leader; workers join it
  headless. Measured on v0.26.0: the worker runs `vllm serve ... --headless
  --nnodes N --node-rank i --master-addr $LWS_LEADER_ADDRESS`, which starts a
  `MultiprocExecutor` and joins the leader's `torch.distributed` group over NCCL.
  There is exactly one endpoint per group.
- **A group is fragile as a unit.** With `RestartPolicy:
  RecreateGroupOnPodRestart`, any Pod failing recreates the whole group. Even
  under the default policy a vLLM group whose worker restarts is not coherent.

The second fact is the encouraging one. Sleep and wake are HTTP calls to the API
server, so they are calls to **the leader** — structurally the same shape as the
single-Pod case, one address to talk to, with vLLM responsible for reaching the
ranks.

## 3. The gate: multi-node sleep, measured

vLLM's sleep-mode documentation says it "supports distributed workloads: works
with tensor parallelism, pipeline parallelism, etc." What it does not say is
whether that holds when the ranks are on different MACHINES. That was the whole
design resting on an undemonstrated claim, and this repository has been bitten by
exactly that before: sleepers that could not wake at all, and woken engines with
open output-correctness bugs upstream.

It has now been run, twice over, and the second run answers it.

### First attempt, 2026-08-26: blocked by following the wrong instructions

A two-Pod group with `--distributed-executor-backend ray` failed with `ray:
command not found`, and the group churned under
`RecreateGroupOnPodRestart` until it was torn down. Ray really is absent from
`vllm/vllm-openai:v0.26.0` -- confirmed since, as a missing Python module and not
merely a missing binary on `PATH`.

That was recorded here as "an LWS warm pool cannot use the image the fleet is on
today." **That conclusion was wrong**, and the way it was wrong is worth keeping:
it came from following vLLM's LWS documentation, which describes the Ray path,
without checking whether the installed vLLM still needed it. It does not.

### Ray is not required, and for this image is not even allowed

From `vllm/config/parallel.py` in the image itself:

- with `nnodes > 1` on CUDA, the executor backend **defaults to `mp`**;
- `nnodes > 1` is rejected outright unless the backend is `mp`, `uni` or
  `external_launcher` -- *"nnodes > 1 can only be set when distributed executor
  backend is mp, uni or external_launcher."*

So Ray is the legacy multi-node path, not the required one. The supported shape
for this image is:

```
leader:  vllm serve MODEL --tensor-parallel-size 2            --nnodes 2 --node-rank 0            --master-addr $LWS_LEADER_ADDRESS --master-port 29500            --enable-sleep-mode                      # VLLM_SERVER_DEV_MODE=1
worker:  vllm serve MODEL --headless            --tensor-parallel-size 2            --nnodes 2 --node-rank $LWS_WORKER_INDEX            --master-addr $LWS_LEADER_ADDRESS --master-port 29500            --enable-sleep-mode
```

### Second attempt, 2026-08-26: it works

One LWS group, `size: 2`, TP=2, one H100 per Pod, Pods forced onto **different
nodes** by required anti-affinity, Qwen3-0.6B from the shared RWX cache, on
`vllm/vllm-openai:v0.26.0` -- the image the fleet's own decode Pods run.

The group formed across nodes: `world_size=2 rank=0` on the leader,
`rank=1` on the worker, NCCL over `tcp://<leader-fqdn>:29500`.

| | leader node | worker node |
| --- | --- | --- |
| GPU in use, awake | 28093 MiB | 28093 MiB |
| GPU in use, `POST /sleep?level=1` | **1169 MiB** | **1169 MiB** |
| GPU in use, after `POST /wake_up` | 25321 MiB | 25321 MiB |

- **Both ranks release.** The remote rank's GPU frees on a call the leader
  received. This is the fact the whole design needed and could not assume.
- `GET /is_sleeping` reported `true` asleep and `false` awake.
- **Wake took 2.5 s**, against ~100 s for this group to cold-start from a local
  PVC. That ratio, not the absolute number, is the case for an LWS pool.
- **The output is correct, not merely a 200.** Same prompt, greedy, seeded,
  before and after: `' Paris. The capital of France is also the capital of the'`
  byte-identical. That was the failure mode most worth looking for.

### What the fourth check found: a group is all-or-nothing, including asleep

Deleting the WORKER Pod of a **sleeping** group took the LEADER down with it --
the leader's container went to `Completed`, and LWS rebuilt both Pods. Under
`RecreateGroupOnPodRestart` that is documented behaviour, and it confirms the
rule in §4c rather than contradicting it: a group missing a Pod is not a degraded
engine, it is no engine, and every warm model it held is gone with it.

For a pool this is sharper than for a serving deployment. A single worker
eviction destroys the entire warm set of that group, not one model's copy.

### What is still unmeasured, and needs a bigger model

Level-1 sleep moves weights to host memory, per node. At 0.6B the cgroup
`memory.current` did not visibly move (5.83 GiB leader / 5.34 GiB worker, awake
and asleep alike) -- the weights are simply too small to see against the
runtime's own footprint. **The mechanism was the question here and size does not
change it; the economics are the question next, and size is all that changes
them.** Before §4a is costed, repeat the sleep at a model whose weights dominate:
the per-node host cost is what sets the memory limit of a pooled LWS Pod, and
that limit is the warm-set budget.

## 4. Design

### 4a. A warm pool of GROUPS

A pool for LWS is a set of standing LWS groups, each running the launcher on its
leader:

- the **leader** Pod runs the supervisor; the engine it spawns spans the group
- **worker** Pods run vLLM headless at their own `node-rank` — they hold GPUs and
  join the leader's process group, and hold no pool-specific logic
- sleep, wake and `is_sleeping` are issued to the leader, exactly as they are to
  a single pool Pod today
- a borrow labels **the leader only** into the model's InferencePool; a worker
  must never be labelled, because it serves nothing

The controller's model barely changes. `Membership` becomes group-scoped, the
"Pod" in every decision becomes "leader Pod", and the supervisor protocol is
untouched. The reserve, the admission budget, `max-hold`, eviction and the
borrow/bridge/return cycle all carry over unaltered.

### 4b. Shape is not fungible, and named pools already solve it

A group's `size` is fixed when it is created. A warm group of size 4 can only
warm a model that wants exactly 4 Pods, so the general-purpose property of the
single-Pod pool — any Pod holds any model — is gone.

This needs no new mechanism. It is exactly what **named pools** are for:

```yaml
warmPoolName: h100-4pod      # groups of 4
warmPoolName: h100-8pod      # groups of 8
```

and a model selects with `warmPool: h100-4pod`. Accelerator matching already
declines a model pointed at the wrong pool; a `size` mismatch declines the same
way, by the same rule, with the same message shape.

### 4c. What the pool must NOT do

- **Never label a worker.** Only the leader serves; a labelled worker takes
  traffic nothing will answer.
- **Never treat a group as partly usable.** A group missing a Pod is not a
  degraded engine, it is no engine. Membership must be all-or-nothing, and a
  group whose Pods are not all Running is absent from the observation — the same
  rule that already governs an unreadable Pod.
- **Never resize a group.** `size` is the engine's shape; changing it is a
  different model, not a scaled one.

## 5. What it costs, and when it pays

The economics invert relative to the single-Pod pool.

| | single-Pod pool | LWS pool |
| --- | --- | --- |
| held per warm unit | 1 Pod × 1 GPU | `size` Pods × GPUs each |
| cold start avoided | ~35 s (8B, local cache) | minutes (405B class) |
| reuse across models | any model that fits | only models of this exact shape |
| host memory per sleeper | one Pod's budget | per-Pod, on every node |

So an LWS warm group is far more expensive to hold and saves far more when it is
used — and it is useful to far fewer models. That pushes it toward **one pinned
group per critical model** rather than a general reserve: `minReplicaCount ==
maxReplicaCount`, `warmPoolCopies: "1"` on the model that must not cold-start.

The break-even is a rate question and the rate is unmeasured. A 405B model that
spikes twice a day does not justify holding 8 GPUs; one that spikes hourly might.

## 6. Cheaper alternative worth pricing first

If the experiment in §3 fails, or the arithmetic does not clear, there is a much
cheaper intervention that needs no pool at all: keep the **weights hot in page
cache** on the nodes a group will schedule onto, so a cold start reads from RAM
rather than storage. It holds no GPU, so it costs almost nothing — and it saves
only the read, not the compile or the load. Measured here previously: a shared
weights volume moved ~430 MB/s, so an 8B read was ~37 s; at 405B the read is a
large enough share of the cold start to be worth removing on its own.

That is a strictly smaller win and a strictly smaller cost, and it is the right
thing to do first if GPUs are scarce.

## 7. Recommendation

1. ~~Run the §3 experiment.~~ **Done, and it passed.** Multi-node sleep releases
   both ranks' GPUs, wakes in 2.5 s against a ~100 s cold start, and returns
   byte-identical output. Ray is not needed and, for `nnodes > 1`, not permitted.
2. **Measure the host-memory cost at a model whose weights dominate**, before
   costing anything. That number sets a pooled LWS Pod's memory limit, which is
   its warm-set budget, and 0.6B could not show it. This is now the gate.
3. Then build §4a — the controller changes are modest because the leader is a
   single endpoint and named pools already carry shape.

Still do not build §4a speculatively. The single-Pod pool spent this month
discovering that its silent states were the expensive part; an LWS pool holds an
order of magnitude more GPU per unit, and §3 showed the blast radius is the whole
group: one evicted worker destroys every warm model that group held. A silent
failure there is proportionally worse.

One correction worth carrying beyond this document: the first attempt's
conclusion — "an LWS warm pool cannot use the image the fleet is on today" — was
wrong, and it was wrong because it trusted vLLM's LWS documentation over the
installed vLLM. The documentation describes the Ray path; the code rejects it.
Read the config the image actually ships before recording a prerequisite.

## Sources

- `vllm/config/parallel.py` in `vllm/vllm-openai:v0.26.0` — the backend rules for
  `nnodes > 1`, read from the image rather than the docs
- `vllm/entrypoints/cli/serve.py:209` — "Run headless workers (for multi-node
  PP/TP)", the join path a worker Pod takes
- vLLM, [Sleep Mode](https://docs.vllm.ai/en/latest/features/sleep_mode/)
- vLLM, [LWS deployment](https://docs.vllm.ai/en/stable/deployment/frameworks/lws/)
- LWS, [Failure handling and restart policies](https://lws.sigs.k8s.io/docs/concepts/failure-handling/)
- vLLM, [Expert Parallel deployment](https://docs.vllm.ai/en/latest/serving/expert_parallel_deployment/)
- vLLM issue [#17103](https://github.com/vllm-project/vllm/issues/17103), sleep/wake output correctness
