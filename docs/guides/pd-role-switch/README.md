# Serve a model with replicas that change P/D role in place

A prefill replica and a decode replica of the same model differ by configuration,
not by weights. An engine can therefore be told to change role and does so in
**well under a second**, without reloading its weights or starting a new pod.
Measured on 2 x 8 H200 at EP=16: 797-799 ms on vLLM v0.28.0 and 52 ms on a
current vLLM, for a switch onto a buffer the engine already held.

This guide deploys such a model on llm-d in a shape where that is actually
usable. Nothing here is model-specific -- the property that matters is
structural -- so set the model on the one line the manifest calls out.
The shape matters: a stock llm-d guide **cannot** host role switching, for a
reason measured below.

The engine side lives in the vLLM fork, not here:

| what | where |
| --- | --- |
| the switch itself, `POST /switch_pd_role` | [`vllm/v1/engine/pd_role.py`](https://github.com/ev-shindin/vllm-conf/blob/feat/pd-role-switch/vllm/v1/engine/pd_role.py) |
| measurements, reproduction, troubleshooting | [`tools/pd_role_switch/README.md`](https://github.com/ev-shindin/vllm-conf/blob/feat/pd-role-switch/tools/pd_role_switch/README.md) |
| running the fork on a stock image | same README, step 1b |
| what it costs, for a non-engine audience | [`tools/pd_role_switch/ARCHITECTURE.md`](https://github.com/ev-shindin/vllm-conf/blob/feat/pd-role-switch/tools/pd_role_switch/ARCHITECTURE.md) |

## Why the stock llm-d shape cannot do this

llm-d decides which endpoints are prefill and which are decode from a **pod
label**, `llm-d.ai/role`. Its guides put that label in each role Deployment's
`spec.selector`:

```json
"selector": {"matchLabels": {"llm-d.ai/guide": "...", "llm-d.ai/role": "prefill", ...}}
```

A Deployment's selector is immutable, and a Deployment owns exactly the pods
matching it. So relabelling a running pod does not move it between roles — it
removes the pod from its owner.

`check-label-semantics.sh` demonstrates it both ways in about a minute, with no
GPUs:

<!-- guide:prerequisites.label_semantics start -->
```bash
# Prove the claim this guide rests on before deploying anything: that
# llm-d.ai/role in a Deployment's selector makes relabelling orphan the pod.
# Takes about a minute and needs no GPUs -- it deploys two pause Deployments,
# one with the role in the selector and one without, relabels a pod in each,
# and reports who owns it afterwards.
./check-label-semantics.sh "$NAMESPACE"
# expect BOTH:
#   PASS  role in the selector: relabelling orphaned the pod ...
#   PASS  role out of the selector: pod kept its owner, no replacement
```
<!-- guide:prerequisites.label_semantics end -->
```
role IN  selector: pods=3 owner_of_relabelled=<none, ORPHANED>
role OUT selector: pods=2 owner_of_relabelled=labeltest-role-out-of-selector-54c5d4f558

PASS  role in the selector: relabelling orphaned the pod and forced a replacement
PASS  role out of the selector: pod kept its owner, no replacement
```

Relabelling under the stock shape gets you an orphan **and** a cold replacement —
the opposite of the point. So the role must be kept out of every selector.

## The deployment

[`switchable.yaml`](switchable.yaml) puts every switchable replica in
**one** Deployment whose selector omits the role:

```yaml
selector:
  matchLabels:
    llm-d.ai/guide: pd-switchable      # llm-d.ai/role deliberately absent
    llm-d.ai/model: pd-switchable
    llm-d.ai/engine-type: vllm
```

The InferencePool still selects on `llm-d.ai/guide`, so a role change never
ejects a replica from the pool, and the endpoint picker needs no changes — it
already filters on the label and does not care who owns the pod.

**The ratio becomes a labelling decision.** Scaling the Deployment changes total
capacity; relabelling changes the prefill:decode split.

<!-- guide:deploy start -->
```bash
# One Deployment holds every switchable replica and its selector omits the
# role, so a pod can be relabelled without leaving its owner. The model is one
# line in the manifest; nothing else in it is model-specific.
kubectl apply -n "$NAMESPACE" -f switchable.yaml
kubectl rollout status -n "$NAMESPACE" deploy/pd-switchable --timeout=60m
```
<!-- guide:deploy end -->
Settings in there that are not guessable, and each of which has cost a run:

| setting | why |
| --- | --- |
| `VLLM_NIXL_SIDE_CHANNEL_HOST` from `status.podIP` | the peer dials this for the KV handshake; the default `localhost` cannot work across pods |
| `containerPort: 5600` named `nixl` | the KV side channel |
| `VLLM_SERVER_DEV_MODE=1` | `/switch_pd_role` is a dev route. Without it every switch returns 404 — which reads as a 6 ms switch on an engine that still answers correctly |
| `VLLM_USE_DEEP_GEMM=0`, no `--moe-backend` | `deepep_v2` pads its layout and deep_gemm asserts on the padded tensor |
| `VLLM_PD_KEEP_PREVIOUS=1` | parks the outgoing buffer, which keeps CUDA graphs valid and makes the return switch a reuse |
| `--max-num-batched-tokens=2048` | boot at the **prefill** budget. It sizes buffers at init, so a switch may lower it but never raise it past launch |
| `kv_role: kv_both` | covers both directions; llm-d sets it on both roles and the switch leaves it alone |

## Changing a replica's role

Four steps. Only step 3 is the engine's.

<!-- guide:switch start -->
```bash
# Four steps, and only step 3 belongs to the engine. Between steps 1 and 4 the
# replica serves nothing -- that is the cost of doing it safely, a few hundred
# milliseconds against the minutes a replacement replica takes.
# 
# The engine must not relabel itself. That would put Kubernetes write
# credentials in every inference pod so each could mutate cluster state about
# itself. The engine owns the mechanism; the controller owns the decision, the
# label and the ordering.
POD=$(kubectl get pod -n "$NAMESPACE" -l llm-d.ai/guide=pd-switchable \
        -o name | head -1)

# 1. out of BOTH roles, so the picker stops selecting it. Pool membership is
#    untouched -- that keys on llm-d.ai/guide.
kubectl label -n "$NAMESPACE" "$POD" llm-d.ai/role-

# 2. let in-flight requests finish

# 3. switch the engine -- and CHECK IT. A switch that did nothing also
#    returns fast and still answers correctly, so timing and output prove
#    nothing.
kubectl exec -n "$NAMESPACE" "$POD" -- curl -s -X POST localhost:8000/switch_pd_role \
  -H 'Content-Type: application/json' \
  -d '{"backend":"deepep_v2","max_num_tokens":128,"max_num_batched_tokens":128}'
#    expect ranks_switched == the rank count and layers_switched > 0

# 4. back in, as the new role
kubectl label -n "$NAMESPACE" "$POD" llm-d.ai/role=decode
```
<!-- guide:switch end -->
Between steps 1 and 4 the replica serves nothing. That is the cost of doing it
safely — a few hundred milliseconds, against the minutes a replacement replica
takes.

**The engine must not relabel itself.** That would put Kubernetes write
credentials in every inference pod so each could mutate cluster state about
itself. The engine owns the mechanism; the controller owns the decision, the
label and the ordering.

## Where WVA comes in

Steps 1, 2 and 4 are policy and orchestration, and belong to the autoscaler —
it already watches load and already holds RBAC on pods. **That controller does
not exist yet**; today the sequence above is run by hand.

What it would need to decide: when the ratio is wrong, and which replica to
convert. A concrete starting signal comes from an operator running GLM-5.2 P/D
in production, who rebalanced 10P/7D to 9P/8D **because decode KV utilisation
hit 99.93%** on his hottest rank.

## What is proven, and what is not

Measured on hardware (details in the fork's README):

- the switch, 16/16 ranks at EP=16 across two nodes, output identical
- KV transfers between two GLM-5.2 engines and **still transfers after both
  change role**, in the reversed direction
- both role buffers resident for 286 MiB; the return switch allocates 0 MiB
- the label semantics above, both ways

Not yet run: **this deployment end to end under a live endpoint picker.** The
pieces are each verified; the assembly is not. Nor is the throughput cost free —
a switchable engine gives up about 13.7% of decode throughput against a
dedicated decode replica, because it cannot use the same expert kernel. The
fork's README has that comparison in full.
