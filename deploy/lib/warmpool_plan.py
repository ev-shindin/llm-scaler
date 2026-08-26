"""Group a namespace's ScaledObjects by the pool shape each one would need.

Two models can share a warm pool only if they agree on two things: the
accelerator they demand, and how many devices one replica takes. Neither is
negotiable at run time -- a warm copy is only reusable on the GPU it was loaded
on, and a model needing more devices than a pool Pod holds cannot start in one
however much memory is free. Everything else about a model (its size, its
traffic, its policy) is a tuning question the pool absorbs.

So the plan is: read what each model's scale target actually demands, bucket by
that pair, and say which buckets are already served and which want a pool.

Reads only. Prints a plan; creating anything is `warmpool.sh create`.
"""

import collections
import json
import subprocess
import sys

GPU_RESOURCE = "nvidia.com/gpu"
GPU_PRODUCT = "nvidia.com/gpu.product"


def shape_of(namespace, target):
    """Return (accelerator, gpus-per-replica) for a scale target, or None.

    None means "not a Deployment this tool can read" -- an LWS engine spans
    Pods, and a pool Pod holds engines, so those cannot be warmed at all. That
    is reported rather than guessed at.
    """
    proc = subprocess.run(
        ["kubectl", "get", "deployment", target, "-n", namespace, "-o", "json"],
        capture_output=True,
        text=True,
        timeout=30,
    )
    if proc.returncode != 0:
        return None
    spec = json.loads(proc.stdout)["spec"]["template"]["spec"]

    # llm-d declares its accelerator through nodeAffinity, not nodeSelector, so
    # reading only the latter finds nothing on a real deployment. Read both, and
    # take a single-valued match expression only: a term listing three products
    # is a model that runs on any of them, which is not a pool requirement.
    accelerator = (spec.get("nodeSelector") or {}).get(GPU_PRODUCT, "")
    if not accelerator:
        affinity = ((spec.get("affinity") or {}).get("nodeAffinity") or {}).get(
            "requiredDuringSchedulingIgnoredDuringExecution"
        ) or {}
        for term in affinity.get("nodeSelectorTerms") or []:
            for expr in term.get("matchExpressions") or []:
                values = expr.get("values") or []
                if expr.get("key") == GPU_PRODUCT and len(values) == 1:
                    accelerator = values[0]

    gpus = 0
    for container in spec.get("containers", []):
        resources = container.get("resources") or {}
        count = (resources.get("limits") or {}).get(GPU_RESOURCE) or (
            resources.get("requests") or {}
        ).get(GPU_RESOURCE)
        if count:
            gpus = max(gpus, int(count))
    return (accelerator or "unknown", gpus or 1)


def suggested_name(accelerator, gpus):
    if accelerator == "unknown":
        return "pool-%dgpu" % gpus
    short = accelerator.lower().replace("nvidia-", "").split("-")[0]
    return "%s-%dgpu" % (short, gpus)


def main():
    namespace = sys.argv[1]
    document = json.load(sys.stdin)

    pools = []
    models = collections.defaultdict(list)
    for scaled_object in document.get("items", []):
        name = scaled_object["metadata"]["name"]
        triggers = scaled_object.get("spec", {}).get("triggers") or [{}]
        metadata = triggers[0].get("metadata") or {}
        if metadata.get("warmPoolName"):
            pools.append((metadata["warmPoolName"], name))
            continue
        if not metadata.get("modelID"):
            continue
        target = (scaled_object.get("spec", {}).get("scaleTargetRef") or {}).get("name", "")
        models[target].append((name, metadata.get("warmPool", "")))

    if pools:
        print("Pools already declared in %s:" % namespace)
        for pool_name, scaled_object in sorted(pools):
            print("  - %-20s (ScaledObject %s)" % (pool_name, scaled_object))
        print("")

    if not models:
        print("No model ScaledObjects found in %s." % namespace)
        return 0

    buckets = collections.defaultdict(list)
    for target, members in sorted(models.items()):
        shape = shape_of(namespace, target)
        if shape is None:
            print(
                "  ? %s: scale target is not a readable Deployment. An engine that "
                "spans Pods (LWS) cannot be warmed by a pool Pod." % target
            )
            continue
        buckets[shape].append((target, [selection for _, selection in members]))

    if not buckets:
        print("No model ScaledObject in %s has a warmable scale target." % namespace)
        return 0

    declared = {pool_name for pool_name, _ in pools}
    dangling = set()
    print("Models grouped by what a pool would have to provide:\n")
    for (accelerator, gpus), members in sorted(buckets.items()):
        print("  accelerator=%s  gpusPerReplica=%d   (%d model(s))" % (accelerator, gpus, len(members)))
        for target, selections in members:
            named = [selection for selection in selections if selection]
            if not named:
                chosen = "(no warmPool set -- nothing warms this model)"
            else:
                # A model naming a pool nobody declared is worse off than one
                # naming none: it reads as configured. WVA reports it at run
                # time; the value of saying it HERE is that it can be fixed
                # before it has cost anything.
                missing = [name for name in named if name not in declared]
                dangling.update(missing)
                chosen = ", ".join(
                    name + "  <- NO SUCH POOL" if name in missing else name
                    for name in named
                )
            print("    - %-40s selects: %s" % (target, chosen))
        if len(members) > 1:
            print(
                "    These %d can SHARE one pool: same accelerator, same device count."
                % len(members)
            )
        name = suggested_name(accelerator, gpus)
        if name in declared:
            print("    Pool '%s' already exists -- point these at it with  warmPool: %s" % (name, name))
        elif accelerator == "unknown":
            print(
                "    Accelerator undeclared, so no pool is suggested: a pool that cannot "
                "name its GPU cannot prove a model fits it."
            )
        else:
            print("    Suggested:")
            print(
                "      deploy/warmpool.sh create -n %s --name %s --gpus %d \\\n"
                "        --accelerator %s \\\n"
                "        --models 4 --model-size 8B --proxy-image REF --wva-namespace NS"
                % (namespace, name, gpus, accelerator)
            )
        print("")

    if dangling:
        print(
            "%d pool name(s) are selected by a model but declared by nobody: %s.\n"
            "Those models are warmed by nothing, while their ScaledObject reads as\n"
            "though they were. Create the pool, or drop the warmPool key.\n"
            % (len(dangling), ", ".join(sorted(dangling)))
        )

    print(
        "Each model names its pool with the warmPool trigger key. Models in different\n"
        "groups above need different pools: a pool serves exactly one (accelerator,\n"
        "device-count) shape.\n"
        "The --models/--model-size in each suggestion are PLACEHOLDERS. They set the\n"
        "Pod memory limit, which IS the warm-set budget, and only you know how many\n"
        "models of what size a pool must hold at once."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
