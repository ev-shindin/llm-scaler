"""Answer "should this model be warmed on this cluster?" from the cluster's own nodes.

Every recommendation about warming a model is really four questions, and all
four are decided by cluster facts rather than by preference:

  1. Does the model fit one node?          -> if not, the engine spans Pods (LWS)
  2. Can host RAM hold a level-1 sleeper?  -> if not, there is no warm copy
  3. Can it hold MORE THAN ONE?            -> if not, a pool IS an idle replica,
                                              and an idle replica at least serves
  4. Is the cold start weight-bound or fixed-cost bound?
                                           -> decides whether faster storage,
                                              a warm process, or neither helps

The thresholds move with the hardware, which is why this reads the nodes instead
of quoting numbers from one fleet. The COEFFICIENTS below are measured, and are
the part to distrust first when a prediction disagrees with reality.
"""

import argparse
import json
import subprocess
import sys

# Measured on pokprod 2026-08-26, FITTED TO TWO POINTS -- see
# docs/proposals/warm-pool-sleep-levels.md.
#
#   Qwen3-0.6B: 1.11 GiB of weights cost 2.91 GiB of host RAM asleep
#   Qwen3-8B:   15.27 GiB           cost 22.05 GiB
#
# Solving both gives 1.41 GiB + 1.352 x weights. The SLOPE is what matters at any
# model worth pooling, and it was extrapolated from the 0.6B point alone as 1.4 --
# confirmed within 3.4% once a real model was measured.
SLEEPER_FIXED_GIB = 1.41
SLEEPER_FACTOR = 1.352

# Cold start to serving, cached image, 0.6B: ~100 s, of which the weight read was
# 0.56 s. So this is the part that does NOT scale with the model.
FIXED_STARTUP_S = 100.0

# vLLM's startup weight loader, ONE rank, page cache warm: "Loading weights took
# 1.11 seconds" for 1.4 GiB. This is the rate that matters for a cold start --
# reload_weights runs a little faster (1.5-2.7 GB/s) but is a different path.
#
# It is FIVE TIMES slower than a warm cached read (6.3 GB/s), so a single rank is
# bound by parse and host-to-device, not by storage. Ranks parse concurrently, so
# the fleet's aggregate is this x TP, and storage only binds above that.
PARSE_RATE_GBPS = 1.26
# Host-to-device: what a level-1 wake actually costs. MEASURED, not assumed --
# Qwen3-8B woke in 0.42 s from 15.27 GiB, i.e. ~39 GB/s. An earlier guess of
# 20 GB/s made every published wake time about twice as pessimistic as reality.
PCIE_GBPS = 39.0

# A pool Pod can hold at most this many engines -- a port-range constant in the
# supervisor, not a tuning knob. Node RAM can imply far more than this, and the
# implication is wrong.
MAX_INSTANCES_PER_POD = 16

BYTES_PER_PARAM = {"bf16": 2.0, "fp16": 2.0, "fp8": 1.0, "int8": 1.0, "fp4": 0.5, "int4": 0.5}


def nodes_from_cluster(selector):
    cmd = ["kubectl", "get", "nodes", "-o", "json"]
    if selector:
        cmd += ["-l", selector]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    if out.returncode != 0:
        raise SystemExit("could not read nodes: %s" % out.stderr.strip())
    nodes = []
    for n in json.loads(out.stdout).get("items", []):
        cap, alloc = n["status"]["capacity"], n["status"]["allocatable"]
        # CAPACITY, not allocatable: a node's SHAPE is how many devices it has,
        # and what is free right now is a scheduling question. Grouping by free
        # GPUs invents node types -- a box with 4 of 8 idle reads as a 4-GPU
        # machine and gets sized as one.
        gpus = cap.get("nvidia.com/gpu") or cap.get("amd.com/gpu")
        if not gpus or int(gpus) == 0:
            continue
        free = alloc.get("nvidia.com/gpu") or alloc.get("amd.com/gpu") or "0"
        labels = n["metadata"].get("labels") or {}
        per_gpu_mib = labels.get("nvidia.com/gpu.memory")
        nodes.append({
            "name": n["metadata"]["name"],
            "gpus": int(gpus),
            "free": int(free),
            "per_gpu_gib": (float(per_gpu_mib) / 1024.0) if per_gpu_mib else None,
            "product": labels.get("nvidia.com/gpu.product", "unknown"),
            "ram_gib": _quantity_gib(cap.get("memory", "0")),
            "disk_gib": _quantity_gib(cap.get("ephemeral-storage", "0")),
        })
    return nodes


def _quantity_gib(q):
    q = str(q)
    for suffix, mult in (("Ki", 1 / 1048576.0), ("Mi", 1 / 1024.0), ("Gi", 1.0), ("Ti", 1024.0)):
        if q.endswith(suffix):
            return float(q[: -len(suffix)]) * mult
    return float(q) / (1024.0 ** 3)


def recommend(node, params_b, dtype, kv_headroom, shared_bw, local_bw, ram_frac):
    weights_gb = params_b * BYTES_PER_PARAM[dtype]
    node_gpu_gib = (node["per_gpu_gib"] or 0) * node["gpus"]
    if node_gpu_gib <= 0:
        raise SystemExit(
            "node %s does not publish nvidia.com/gpu.memory, so its capacity cannot be "
            "read. Pass --gpu-mem-gib to state it." % node["name"])

    need_gib = weights_gb * (1.0 + kv_headroom) / 1.073741824
    nodes_needed = max(1, -(-int(need_gib * 1000) // int(node_gpu_gib * 1000)))
    per_node_weights_gb = weights_gb / nodes_needed

    # 1. shape
    #
    # TP is what the MODEL needs, not what the node has. Assuming a model spreads
    # over every GPU on its node overstates the aggregate parse rate for small
    # models by the node's GPU count, and parse aggregate is what decides whether
    # storage is worth spending on.
    lines = []
    per_gpu = node["per_gpu_gib"]
    if nodes_needed == 1:
        tp = max(1, -(-int(need_gib * 1000) // int(per_gpu * 1000)))
        tp = min(tp, node["gpus"])
    else:
        tp = nodes_needed * node["gpus"]
    if nodes_needed == 1:
        lines.append(("SHAPE", "fits one node: Deployment, TP=%d of %d GPUs"
                      % (tp, node["gpus"])))
    else:
        lines.append(("SHAPE", "does NOT fit one node (%.0f GiB needed vs %.0f GiB): "
                               "%d nodes, TP=%d, engine spans Pods -> LWS required"
                      % (need_gib, node_gpu_gib, nodes_needed, tp)))

    # 2 + 3. can host RAM hold sleepers, and how many
    usable_ram = node["ram_gib"] * ram_frac
    per_sleeper = SLEEPER_FIXED_GIB + SLEEPER_FACTOR * (per_node_weights_gb / 1.073741824)
    by_ram = int(usable_ram // per_sleeper) if per_sleeper > 0 else 0
    fits = min(by_ram, MAX_INSTANCES_PER_POD)
    capped = by_ram > MAX_INSTANCES_PER_POD
    if fits < 1:
        lines.append(("SLEEP", "level 1 IMPOSSIBLE: one sleeper needs %.0f GiB per node, "
                               "%.0f GiB usable. No warm copy on this hardware."
                      % (per_sleeper, usable_ram)))
    elif capped:
        lines.append(("SLEEP", "level 1: RAM would allow %d, capped at %d per Pod "
                               "(port range, not a knob). %.0f GiB each."
                      % (by_ram, MAX_INSTANCES_PER_POD, per_sleeper)))
    else:
        lines.append(("SLEEP", "level 1 fits %d model(s) per node: %.0f GiB each, %.0f GiB usable"
                      % (fits, per_sleeper, usable_ram)))

    # 4. what dominates the cold start
    aggregate_parse = PARSE_RATE_GBPS * tp
    shared_s = weights_gb / min(shared_bw, aggregate_parse)
    local_s = weights_gb / min(local_bw * nodes_needed, aggregate_parse)
    wake_s = per_node_weights_gb / PCIE_GBPS
    lines.append(("COLD", "~%.0f s fixed + %.0f s weights (shared storage) = ~%.0f s"
                  % (FIXED_STARTUP_S, shared_s, FIXED_STARTUP_S + shared_s)))
    lines.append(("COLD", "~%.0f s fixed + %.0f s weights (node-local storage) = ~%.0f s"
                  % (FIXED_STARTUP_S, local_s, FIXED_STARTUP_S + local_s)))
    if fits >= 1:
        lines.append(("WAKE", "level-1 wake ~%.0f s (host-to-device, per node in parallel)" % wake_s))

    # The verdict, which is the only part anyone should act on.
    verdict = []
    if fits < 1:
        # Level 2 holds no host RAM at all, so it is the only warm option left --
        # and its wake is a reload, which means node-local storage is what decides
        # whether it is a bridge or a slow cold start.
        verdict.append(
            "Level 1 is IMPOSSIBLE here: host RAM cannot hold one sleeper. Level 2 "
            "holds no RAM, and its wake is a reload: ~%.0f s from node-local storage, "
            "~%.0f s from shared. That is the only warm option on this hardware."
            % (local_s, shared_s))
        verdict.append(
            "But level 2 SERVES GARBAGE unless reload_weights is called after every "
            "wake, and a failed reload poisons the engine until it restarts. Nothing "
            "implements that today. Pin minReplicaCount unless you build it "
            "(see warm-pool-sleep-levels.md).")
    elif fits == 1:
        verdict.append(
            "DO NOT pool this model: one sleeper per node means the pool holds exactly "
            "the accelerators a replica would, while serving nothing. Pin "
            "minReplicaCount >= 1 -- same GPU cost, and it answers requests.")
    else:
        # What a pool Pod HOLDS is the model's device count, not the node's.
        held = tp
        verdict.append(
            "POOL IT: %d models share %d GPU(s) instead of %d, switching in ~%.0f s "
            "against a ~%.0f s cold start. That multiplexing is the whole gain."
            % (fits, held, held * fits, wake_s, FIXED_STARTUP_S + local_s))

    if shared_s > FIXED_STARTUP_S and local_bw > shared_bw:
        verdict.append(
            "STAGE WEIGHTS ON NODE-LOCAL STORAGE: the weight read (%.0f s) exceeds the "
            "fixed startup (%.0f s), and local storage cuts it to %.0f s. Needs %.0f GiB "
            "per node, of %.0f GiB free. No code, no GPU -- price this before anything else."
            % (shared_s, FIXED_STARTUP_S, local_s, per_node_weights_gb / 1.073741824,
               node["disk_gib"]))
    elif shared_s < FIXED_STARTUP_S:
        verdict.append(
            "Faster storage will NOT help: the weight read (%.0f s) is already below the "
            "fixed startup cost (%.0f s). Only keeping a process alive removes that."
            % (shared_s, FIXED_STARTUP_S))

    if nodes_needed > 1 and node.get("whole_free") is not None:
        if node["whole_free"] < nodes_needed:
            verdict.append(
                "SCHEDULING: a group needs %d nodes with every GPU free, and only %d are. "
                "This will not schedule today without draining something."
                % (nodes_needed, node["whole_free"]))
    if nodes_needed > 1:
        verdict.append(
            "Multi-node sleep is DEMONSTRATED (both ranks release, no Ray -- vLLM rejects "
            "the ray backend for nnodes>1). But a group is all-or-nothing: one evicted "
            "worker destroys every warm model the group held.")
    return lines, verdict


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--params", required=True,
                    help="total parameters, e.g. 744B or 8B. For MoE use TOTAL, not active: "
                         "every expert must be resident.")
    ap.add_argument("--dtype", default="fp8", choices=sorted(BYTES_PER_PARAM))
    ap.add_argument("--kv-headroom", type=float, default=0.25,
                    help="fraction of weight size to leave for KV cache and activations "
                         "(default 0.25). Long-context serving needs far more.")
    ap.add_argument("--shared-bw", type=float, default=1.8,
                    help="shared storage read GB/s (default 1.8, measured on Spectrum Scale)")
    ap.add_argument("--local-bw", type=float, default=5.2,
                    help="node-local storage read GB/s per node (default 5.2, measured NVMe)")
    ap.add_argument("--ram-fraction", type=float, default=0.75,
                    help="share of host RAM usable for sleepers (default 0.75)")
    ap.add_argument("-l", "--selector", default="", help="node label selector")
    # A cluster you cannot reach is still a cluster you have to decide about, and
    # it is the common case when choosing hardware rather than using it.
    ap.add_argument("--gpus-per-node", type=int,
                    help="describe a node instead of reading a cluster")
    ap.add_argument("--gpu-mem-gib", type=float, help="memory per GPU, e.g. 141 for H200")
    ap.add_argument("--ram-gib", type=float, help="host RAM per node")
    ap.add_argument("--disk-gib", type=float, default=0.0, help="node-local disk per node")
    ap.add_argument("--product", default="described node", help="name for the described node")
    args = ap.parse_args()

    text = args.params.strip().upper().rstrip("B")
    try:
        params_b = float(text)
    except ValueError:
        raise SystemExit("--params should look like 744B or 8B")

    if args.gpus_per_node:
        if not (args.gpu_mem_gib and args.ram_gib):
            raise SystemExit("--gpus-per-node needs --gpu-mem-gib and --ram-gib too")
        nodes = [{"name": args.product, "gpus": args.gpus_per_node,
                  "free": args.gpus_per_node, "per_gpu_gib": args.gpu_mem_gib,
                  "product": args.product, "ram_gib": args.ram_gib,
                  "disk_gib": args.disk_gib, "described": True}]
    else:
        nodes = nodes_from_cluster(args.selector)
        if not nodes:
            raise SystemExit("no GPU nodes found. Pass -l to select them, or describe "
                             "one with --gpus-per-node/--gpu-mem-gib/--ram-gib.")

    # Group by shape: a fleet is only as capable as the node type the model lands on,
    # and mixed fleets give different answers per type.
    shapes = {}
    for n in nodes:
        key = (n["product"], n["gpus"], round(n["per_gpu_gib"] or 0), round(n["ram_gib"]))
        shapes.setdefault(key, []).append(n)

    weights_gb = params_b * BYTES_PER_PARAM[args.dtype]
    print("Model: %s params at %s = %.0f GB of weights\n" % (args.params, args.dtype, weights_gb))

    for (product, gpus, per_gpu, ram), members in sorted(shapes.items()):
        described = members[0].get("described")
        whole = sum(1 for m in members if m["free"] == m["gpus"])
        if described:
            print("Node: %s  %dx%.0f GiB GPU, %.0f GiB RAM, %.0f GiB local disk"
                  % (product, gpus, per_gpu, ram, members[0]["disk_gib"]))
        else:
            print("%d node(s) of: %s  %dx%.0f GiB GPU, %.0f GiB RAM, %.0f GiB local disk"
                  % (len(members), product, gpus, per_gpu, ram, members[0]["disk_gib"]))
            print("   %d of them currently have every GPU free" % whole)
        try:
            shape = dict(members[0], whole_free=(None if described else whole))
            lines, verdict = recommend(shape, params_b, args.dtype, args.kv_headroom,
                                       args.shared_bw, args.local_bw, args.ram_fraction)
        except SystemExit as e:
            print("   %s\n" % e)
            continue
        for tag, line in lines:
            print("   %-6s %s" % (tag, line))
        print("")
        for v in verdict:
            print("   -> %s" % v)
        print("")

    print("The warm-set budget is the POD's memory LIMIT, not the node's RAM: a Pod is\n"
          "admitted against its own limit, so these counts are what the hardware permits,\n"
          "not what a Pod will hold until you set that limit to match.\n")
    print("Coefficients are MEASURED at 0.6B and extrapolated: a sleeper costs\n"
          "%.1f GiB + %.1fx weights of host RAM, the fixed startup is ~%.0f s, and the\n"
          "load path runs ~%.1f GB/s per rank. Distrust these first if a prediction\n"
          "disagrees with the cluster." % (SLEEPER_FIXED_GIB, SLEEPER_FACTOR,
                                           FIXED_STARTUP_S, PARSE_RATE_GBPS))
    return 0


if __name__ == "__main__":
    sys.exit(main())
