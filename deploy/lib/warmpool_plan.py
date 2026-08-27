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
# Column the plan's inline comments start at, so the block reads as a table.
COMMENT_COLUMN = 26
GPU_PRODUCT = "nvidia.com/gpu.product"


# Where each kind keeps the pod template WVA would read. Mirrors
# SO_POD_PATH_DEPLOYMENT / SO_POD_PATH_LWS in scaledobject.sh; the leader is the
# right template for an LWS, since that is the Pod holding rank 0.
POD_PATH = {
    "deployment": ("deployment", ["spec", "template", "spec"]),
    "leaderworkerset": (
        "leaderworkerset",
        ["spec", "leaderWorkerTemplate", "leaderTemplate", "spec"],
    ),
}


def shape_of(namespace, target, kind="Deployment"):
    """Return (accelerator, gpus-per-replica) for a scale target, or None.

    None means the workload could not be read at all. A kind this tool does not
    know is reported rather than guessed at, because guessing a shape wrong
    sizes a pool that then cannot start the model it was built for.
    """
    resource, path = POD_PATH.get(kind.lower(), (None, None))
    if resource is None:
        return None
    proc = subprocess.run(
        ["kubectl", "get", resource, target, "-n", namespace, "-o", "json"],
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=30,
    )
    if proc.returncode != 0:
        return None
    spec = json.loads(proc.stdout)
    for key in path:
        spec = (spec or {}).get(key) or {}

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


def emit_plan_block(rows):
    """Write a `warmPools:` section for the ScaledObject plan.

    `rows` are "namespace|kind|name" lines -- the workloads the install plan just
    found. It groups by what a pool must provide and suggests one per group.

    This runs at INSTALL-plan time, when the ScaledObjects do not exist yet: the
    plan is what creates them. So it groups by the WORKLOADS, where the
    standalone `warmpool.sh plan` groups by ScaledObject triggers on a namespace
    that is already running. Same question, different evidence.

    Everything is written `apply: "no"`. A pool holds accelerators from the
    moment it exists, so it is never the safe default, and the two values that
    decide its memory budget are placeholders only the operator can set.
    """
    buckets = collections.defaultdict(list)
    unreadable = []
    for row in rows:
        parts = row.strip().split("|")
        if len(parts) != 3 or not parts[2]:
            continue
        namespace, kind, name = parts
        shape = shape_of(namespace, name, kind)
        if shape is None:
            unreadable.append("%s/%s" % (kind, name))
            continue
        buckets[(namespace,) + shape].append("%s/%s" % (kind, name))

    print("")
    print("# Warm pools. Each group below is a set of workloads that could SHARE one")
    print("# pool: they agree on the accelerator and on how many devices a replica")
    print("# takes, the two things a warm copy cannot change.")
    print("#")
    print("# A pool holds engines already loaded on a GPU, so a scale-up serves while")
    print("# its own replica is still loading. It holds those accelerators the whole")
    print("# time, so it is worth it only where the load time costs something.")
    print("#")
    print("# apply: \"yes\" creates the pool when this plan is applied. Everything is")
    print("# \"no\" until you say otherwise.")
    print("#")
    print("# models/modelSize are PLACEHOLDERS. Together they set the Pod memory")
    print("# limit, which IS the warm-set budget -- and that budget must cover the")
    print("# level-1 sleep charge, which appears the first time a model sleeps and is")
    print("# never released. `deploy/warmpool.sh sizing --params <size>` answers what")
    print("# this cluster can actually hold.")
    print("#")
    print("# Full guide, including how to remove a pool:")
    print("#   docs/guides/warm-pool/README.md")
    if unreadable:
        print("#")
        print("# Not shaped, so not offered: %s." % ", ".join(sorted(set(unreadable))))
    print("warmPools:")

    if not buckets:
        print("  # Nothing to suggest: no workload here declares both an accelerator")
        print("  # and a device count. A pool that cannot name its GPU cannot prove a")
        print("  # model fits it.")
        return 0

    for (namespace, accelerator, gpus), members in sorted(buckets.items()):
        print("")
        for member in sorted(members):
            print("  # serves: %s" % member)
        if accelerator == "unknown":
            print("  # No accelerator declared on these, so no pool is suggested for")
            print("  # them: a pool that cannot name its GPU cannot prove a model fits.")
            continue
        # Comments aligned to a fixed column so the file reads as a table. The
        # name and accelerator vary in length, so this cannot be done by hand.
        first = True

        def field(text, *comment):
            nonlocal first
            lead = "  - " if first else "    "
            first = False
            body = lead + text
            # At LEAST one space before the #, whatever the column. Without it a
            # value longer than the column runs straight into the comment and
            # YAML reads the whole line as the value: an accelerator called
            # NVIDIA-H200-141GB came back as "NVIDIA-H200-141GB# nvidia.com/..."
            # and would have built a pool that could never schedule.
            width = max(COMMENT_COLUMN, len(body) + 1)
            print("%-*s# %s" % (width, body, comment[0]))
            for extra in comment[1:]:
                print("%-*s# %s" % (width, "", extra))

        field('apply: "no"', 'yes | no -- "yes" creates this pool on apply')
        field("namespace: %s" % namespace,
              "where the pool Pods run: with the models they serve")
        field("name: %s" % suggested_name(accelerator, gpus),
              "the pool's name; a model selects it with warmPool: <name>")
        field("accelerator: %s" % accelerator,
              "nvidia.com/gpu.product these Pods must land on. A pool",
              "named for one GPU that schedules on another is the",
              "silent mismatch this whole design exists to avoid.")
        field("gpus: %d" % gpus,
              "GPUs per Pod. Must match what one replica takes, or",
              "the model cannot start in a pool Pod at all.")
        field("models: 4",
              "how many models one Pod must hold at once")
        field("modelSize: 8B",
              "the largest of them. With models:, these two COMPUTE",
              "the Pod memory limit -- the warm-set budget.")
        field("replicas: 2",
              "Pods to start, and the floor KEDA holds. Each one holds",
              "its GPUs from creation, warm model in it or not.")
        field("max: 6",
              "ceiling KEDA may grow the pool to. MUST exceed reserve,",
              "or the admission budget is zero forever.")
        field("reserve: 1",
              "Pods kept free for the next borrow. Admission draws on",
              "free-minus-reserve, so this is held back, not used.")
    return 0


def main():
    # Two callers, two kinds of evidence. --from-workloads is the install plan,
    # which has workload rows and no ScaledObjects yet; the default is the
    # standalone command, which reads a running namespace's ScaledObjects.
    if sys.argv[1] == "--from-workloads":
        return emit_plan_block(sys.stdin.read().splitlines())

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
        if pools:
            # The inverse of a dangling selection, and just as quiet: every Pod
            # these pools hold is an accelerator nothing can ever borrow.
            print(
                "\n%d pool(s) are declared here with no model to serve. They hold\n"
                "accelerators that nothing can borrow. Delete them, or point a model\n"
                "at them:  deploy/warmpool.sh delete -n %s --name <pool>"
                % (len(pools), namespace)
            )
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

    # With exactly one pool a model needs no warmPool key -- there is nothing to
    # disambiguate -- so a lone pool is in use by everything. Past one, a pool
    # nobody names serves nobody, however many models the namespace has.
    if len(declared) > 1:
        selected = {name for _, members in buckets.items() for _, sels in members for name in sels if name}
        unused = sorted(declared - selected)
        if unused:
            print(
                "%d declared pool(s) are named by no model: %s.\n"
                "They hold accelerators nothing can borrow. Once a namespace has more\n"
                "than one pool, every model must name the one it wants -- WVA will not\n"
                "guess, because guessing wrong spends a full load on a copy that can\n"
                "never serve.\n" % (len(unused), ", ".join(unused))
            )

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
