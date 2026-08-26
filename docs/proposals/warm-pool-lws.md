# Warming an engine that spans Pods (LeaderWorkerSet)

Status: proposal, and **gated on an experiment nobody has run**. Read §3 before
costing any of the work below.

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
- **Only the leader serves.** In both deployment modes vLLM documents — Ray and
  native multiprocessing — the HTTP API runs on the leader; workers join via
  `LWS_LEADER_ADDRESS` as Ray nodes or by `--master-addr` / `--node-rank`. There
  is exactly one endpoint per group.
- **A group is fragile as a unit.** With `RestartPolicy:
  RecreateGroupOnPodRestart`, any Pod failing recreates the whole group. Even
  under the default policy a vLLM group whose worker restarts is not coherent.

The second fact is the encouraging one. Sleep and wake are HTTP calls to the API
server, so they are calls to **the leader** — structurally the same shape as the
single-Pod case, one address to talk to, with vLLM responsible for reaching the
ranks.

## 3. The gate: multi-node sleep is claimed, not demonstrated

vLLM's sleep-mode documentation says it "supports distributed workloads: works
with tensor parallelism, pipeline parallelism, etc.", and that TP groups
synchronise sleep and wake across workers. What it does **not** say anywhere is
whether that holds when the ranks are on different MACHINES. There are no
rank-coordination specifics, and no statement that all ranks participate in a
wake.

That is the whole design resting on an undemonstrated claim, and this repository
has been bitten by exactly that before: sleepers that could not wake at all, and
woken engines with open output-correctness bugs upstream. Level-1 sleep also
moves weights to HOST memory — per node — so a multi-node sleeper's cost is per
Pod, and nobody here has measured it.

**Run this before anything else.** Two Pods, one LWS group, vLLM with
`--distributed-executor-backend ray` and `--enable-sleep-mode`, then:

1. `POST /sleep?level=1` on the leader. Check GPU memory falls on **both** nodes,
   not just the leader's — `nvidia-smi` per node, and `memory.current` for host
   cost, not `anon` (a sleeper's anonymous memory barely moves).
2. `GET /is_sleeping` — and confirm the workers are actually asleep, not merely
   the leader reporting for itself.
3. `POST /wake_up`, then **check the output is correct**, not merely that it
   returns 200. A woken engine that produces plausible nonsense is the failure
   mode to look for.
4. Repeat across a worker restart to see what a group does when one Pod is
   recreated under it.

If (1) or (3) fails, everything below is unbuildable and the honest answer stays
"LWS is out of scope". If they pass, the design is tractable and mostly reuses
what exists.

### First attempt, 2026-08-26: blocked before it could measure anything

A two-Pod group was stood up on pokprod — anti-affinity across nodes, TP=2,
`--enable-sleep-mode`, `VLLM_SERVER_DEV_MODE=1`, the same
`vllm/vllm-openai:v0.26.0` image the cluster's own decode Pods run.

**The image has no Ray.** `ray: command not found` in the leader. So the
Ray-based multi-node path vLLM documents for LWS is not available in the image
this cluster actually runs, and `ray start --head` failed silently — the leader
sat in its wait loop, the group hit `RecreateGroupOnPodRestart`, and it churned
until it was torn down.

That is a prerequisite nobody had written down, and it changes the shape of the
experiment rather than its conclusion. Before retrying, decide which of these is
being tested:

- **an image with Ray in it** — closest to the documented path, but it is not the
  image llm-d ships, so a pass would prove sleep works somewhere this cluster
  does not run;
- **vLLM's non-Ray multi-node**, if v0.26.0 supports it — proves the thing that
  matters for THIS deployment, and is the more useful answer.

Either way the finding stands on its own: **an LWS warm pool cannot use the
image the fleet is on today.** Whatever else is true of multi-node sleep, that
has to be solved first, and it is a packaging problem rather than a design one.

## 4. Design

### 4a. A warm pool of GROUPS

A pool for LWS is a set of standing LWS groups, each running the launcher on its
leader:

- the **leader** Pod runs the supervisor and the Ray head; the engine it spawns
  spans the group
- **worker** Pods are generic Ray nodes — they hold GPUs and join, and hold no
  pool-specific logic
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

1. **Run the §3 experiment.** It is a day's work and it decides everything else.
   Publish the numbers whether they are good or bad.
2. If multi-node sleep holds, build §4a — the controller changes are modest
   because the leader is a single endpoint and named pools already carry shape.
3. If it does not, say so in the guide and price §6 instead.

Do not build §4a speculatively. The single-Pod pool spent this month discovering
that its silent states were the expensive part; an LWS pool holds an order of
magnitude more GPU per unit, and a silent failure there is proportionally worse.

## Sources

- vLLM, [Sleep Mode](https://docs.vllm.ai/en/latest/features/sleep_mode/)
- vLLM, [LWS deployment](https://docs.vllm.ai/en/stable/deployment/frameworks/lws/)
- LWS, [Failure handling and restart policies](https://lws.sigs.k8s.io/docs/concepts/failure-handling/)
- vLLM, [Expert Parallel deployment](https://docs.vllm.ai/en/latest/serving/expert_parallel_deployment/)
- vLLM issue [#17103](https://github.com/vllm-project/vllm/issues/17103), sleep/wake output correctness
