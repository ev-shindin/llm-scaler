# Cap what each tenant may take

Declare how many GPUs of each accelerator type WVA may hold — for the whole
cluster, or per namespace — so one team's autoscaling cannot spend the fleet.
This is the other half of [bounding a fleet by real GPUs](../bound-by-gpus/):
that path asks whether the hardware exists, this one asks whether the tenant is
allowed it.

**Use it when** several teams scale on shared accelerators and somebody has
promised each of them a number, or when you want a ceiling per GPU type that
sits *below* what the cluster physically has.

**Do not use it to stop a workload from taking GPUs.** A quota bounds **WVA**,
and only WVA — it is an allowance granted to the autoscaler, not an admission
rule. The thing that stops any Pod is a Kubernetes `ResourceQuota`, and on a
cluster where non-WVA workloads compete for the same accelerators you want both.

## What it needs

- Cluster-admin rights to publish the policy where a tenant cannot edit it, and
  accelerators that resolve — the same prerequisites as
  [bound-by-gpus](../bound-by-gpus/), which checks them first.
- A decision about scope. One entry is tied to exactly one scope; a deployment
  that needs both caps declares two entries, and the chain consults both.

## Declaring it

Both shapes live in the `limiters:` list on the scaling policy's `default`
entry. **Cluster scope** caps a type across every namespace:

```yaml
limiters:
  - name: cluster-quota
    type: quota
    scope: cluster
    quotas:
      H100: 16
      A100: 8
      L40S: -1          # unlimited
```

**Namespace scope** caps each tenant, with an optional exclusion list and a
fallback for namespaces you did not list:

```yaml
limiters:
  - name: namespace-quota
    type: quota
    scope: namespace
    exclude: [kube-system, llm-d-system]
    namespaceQuotas:
      team-a:
        H100: 8
      team-priority:
        H100: -1        # unlimited for this tenant
      default:
        H100: 2         # fallback for any namespace not listed
```

`default` there is a **reserved key**, not the Kubernetes namespace of that
name: it selects the fallback, which means the real `default` namespace cannot
be given a quota by listing it. Exclude it, or omit the reserved key entirely so
that anything unlisted is denied.

## Three things that surprise people

**A quota does not bound the cluster.** It counts only what WVA's own variants
hold, so it can hand out capacity that non-WVA workloads have already taken.
Charging it the physical figure would be worse: a namespace with a 4-GPU
allowance beside an unrelated 4-GPU training job would read as fully spent while
WVA had placed nothing, and every scale-up would be refused against an untouched
allowance. "The hardware is full" is a different statement, made by the physical
limiter.

**Ceilings, not reservations.** A finite cap guarantees a tenant never *exceeds*
it — never that it can *reach* it. The cluster aggregate is the sum of the finite
caps, so a namespace that is unlimited (`-1`) or excluded draws from that shared
aggregate without contributing to it, and can consume budget a capped peer would
otherwise have used. Nobody exceeds their own authorization; it is still a
fairness gap, and worth knowing before you promise a team its number.

**A missing entry denies.** `0` or no entry for an accelerator type means no
allocation at all — `-1` is what means unbounded. A typo in a type name is
therefore a denial: the safe direction, and a confusing one.

Fairness is per **model**, not per namespace. Two models in one namespace each
get their own fair-share slot; the cap bounds each of them and their running sum
rather than pooling one model's allowance for the other. The quota clamps *after*
the fair-share split is computed — a ceiling on the outcome, not an input to it.

## Verifying it

```bash
# the policy every controller reads
kubectl get configmap wva-scaling-policy-config -n <system-namespace> -o yaml

# a model that wanted more and was held at its allowance
wva_model_scaling_blocked{exported_namespace="<ns>"}
```

A refusal here is a normal, visible outcome with a `reason` label — not an
error, and not silence.

## What it costs

No accelerators and no latency. What it costs is the fairness gap above, and the
ownership question of who may edit the policy — which is why it lives outside
tenant namespaces.

## How it is tested

- Unit: `internal/config/quota_limiter_test.go` (parsing, scopes, validation,
  the reserved-key rules) and
  `internal/engines/allocation/quota_inventory_test.go` (what the inventory
  reports and charges).
- End-to-end: `test/e2e/warm_pool_quota_test.go` covers a refusal — a pool
  asking for accelerators the quota will not give.
- **No e2e covers the quota limiter's own allocation path.**
  `test/e2e/limiter_test.go` exercises the GPU-inventory limiter only. That is
  the leg to add before treating a tenant cap as something CI protects.

## Tuning it

Every field, the namespace lookup rules, validation and the reload lifecycle:
[the quota limiter reference](../../reference/quota-limiter.md). What the two
usage bases mean and why they are counted differently:
[GPU capacity accounting](../../concepts/gpu-capacity-accounting.md).
