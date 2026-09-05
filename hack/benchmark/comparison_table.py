#!/usr/bin/env python3
"""
Post-process llm-d-benchmark results into the WVA-vs-KEDA comparison table
format (Avg/P50/P95/P99 TTFT+TPOT, GPU time in GPU-min), as used in
workload-variant-autoscaler's comparison-* studies. postprocess.py produces
docs/benchmark.md's format (P95/P99 only, "Cost" in GPU/hr) -- this is the
wider row set for side-by-side arm comparisons; the two are not interchangeable.

The original script that produced those comparison_table.md files is not in
either repo's current tree, so this reconstructs the same rows/units from
summary_lifecycle_metrics.json and replica_status_timeseries.json using the
same field mappings postprocess.py already relies on (time_to_first_token,
inter_token_latency, both in seconds -> scaled to ms here).

Usage:
    # Single run, one column labeled "WVA":
    python hack/benchmark/comparison_table.py results/inference-perf-*_1

    # Multiple arms: all results_dirs first, then --label once per dir in the
    # SAME order (argparse won't reliably interleave a nargs='+' positional
    # with a repeated flag, so dirs and labels are two separate groups):
    python hack/benchmark/comparison_table.py \\
        results/run1 results/run2 \\
        --label "WVA V1" --label "KEDA t=50"
"""

import argparse
import json
import os
import sys
from statistics import mean

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from postprocess import (  # noqa: E402
    _extract_replica_stats,
    _extract_kv_cache_avg,
    _extract_queue_depth_avg,
    _extract_pod_startup_avg,
    _extract_error_count,
    _collection_failure,
)

METRICS = [
    "Avg TTFT (ms)",
    "P50 TTFT (ms)",
    "P95 TTFT (ms)",
    "P99 TTFT (ms)",
    "Avg TPOT (ms/token)",
    "P50 TPOT (ms/token)",
    "P95 TPOT (ms/token)",
    "P99 TPOT (ms/token)",
    "Avg replicas",
    "Max replicas",
    "GPU time (GPU-min)",
    "Avg KV cache utilization",
    "Avg queue depth (EPP)",
    "Error count",
    "Avg pod startup (s)",
]


def _extract_latency_stats(results_dir):
    """Avg/P50/P95/P99 TTFT and TPOT (=inter_token_latency), in ms.

    inference-perf's own summary; present whether or not per-request logging
    is enabled. TPOT here is inter_token_latency, matching postprocess.py's
    ITL mapping -- NOT time_per_output_token, which folds the first token in.
    """
    path = os.path.join(results_dir, "summary_lifecycle_metrics.json")
    if not os.path.isfile(path):
        return {}

    with open(path) as f:
        data = json.load(f)

    latency = data.get("successes", {}).get("latency", {})

    def _get(section_key, key):
        section = latency.get(section_key, {})
        val = section.get(key)
        return val * 1000.0 if val is not None else None

    out = {}
    for label, section_key in (("TTFT", "time_to_first_token"), ("TPOT", "inter_token_latency")):
        out[f"Avg {label} (ms)" if label == "TTFT" else f"Avg {label} (ms/token)"] = _get(section_key, "mean")
        out[f"P50 {label} (ms)" if label == "TTFT" else f"P50 {label} (ms/token)"] = _get(section_key, "median")
        out[f"P95 {label} (ms)" if label == "TTFT" else f"P95 {label} (ms/token)"] = _get(section_key, "p95")
        out[f"P99 {label} (ms)" if label == "TTFT" else f"P99 {label} (ms/token)"] = _get(section_key, "p99")
    return out


def _replica_window_minutes(results_dir):
    """Wall-clock span of replica_status_timeseries.json's own snapshots.

    This is the SAME window _extract_replica_stats averages over (full
    sampler start-to-stop, i.e. harness start to stop -- not just the active
    load-gen stages), so multiplying by it keeps "Avg replicas" and
    "GPU time" internally consistent. It will run wider than the nominal
    scenario duration whenever there's post-load-gen drain/report time --
    unlike the original fixed-duration KEDA-vs-WVA study runs, where the two
    coincided.
    """
    path = os.path.join(results_dir, "metrics", "processed",
                        "replica_status_timeseries.json")
    if not os.path.isfile(path):
        return None
    with open(path) as f:
        data = json.load(f)
    snaps = data.get("snapshots", [])
    if len(snaps) < 2:
        return None
    from datetime import datetime
    first = datetime.fromisoformat(snaps[0]["timestamp"].replace("Z", "+00:00"))
    last = datetime.fromisoformat(snaps[-1]["timestamp"].replace("Z", "+00:00"))
    return (last - first).total_seconds() / 60.0


def process_one(results_dir, gpus_per_replica=1):
    row = _extract_latency_stats(results_dir)
    avg_rep, max_rep = _extract_replica_stats(results_dir)
    window_min = _replica_window_minutes(results_dir)
    gpu_min = (avg_rep * gpus_per_replica * window_min
               if avg_rep is not None and window_min is not None else None)
    row.update({
        "Avg replicas": avg_rep,
        "Max replicas": max_rep,
        "GPU time (GPU-min)": gpu_min,
        "Avg KV cache utilization": _extract_kv_cache_avg(results_dir),
        "Avg queue depth (EPP)": _extract_queue_depth_avg(results_dir),
        "Error count": _extract_error_count(results_dir),
        "Avg pod startup (s)": _extract_pod_startup_avg(results_dir),
    })
    return row


def _fmt(metric, value):
    if value is None:
        return "?"
    if metric.endswith("TTFT (ms)"):
        return f"{value:,.0f}"
    if metric.endswith("TPOT (ms/token)"):
        return f"{value:.2f}"
    if metric == "Avg replicas":
        return f"{value:.2f}"
    if metric == "Max replicas":
        return str(int(value))
    if metric == "GPU time (GPU-min)":
        return f"{value:.1f}"
    if metric == "Avg KV cache utilization":
        return f"{value:.1f}%"
    if metric == "Avg queue depth (EPP)":
        return f"{value:.1f}"
    if metric == "Error count":
        return f"{int(value):,}"
    if metric == "Avg pod startup (s)":
        return str(round(value))
    return str(value)


def _compute_avg(runs, metrics):
    avg = {}
    for m in metrics:
        vals = [r[m] for r in runs if r.get(m) is not None]
        avg[m] = mean(vals) if vals else None
    return avg


def format_table(runs, labels, show_avg=None):
    """Render with every column padded to its own widest cell.

    A markdown renderer doesn't need this -- it ignores whitespace inside
    pipes -- but the raw .md text does not otherwise line up in a plain-text
    view (VSCode's editor pane, `cat`, a terminal), where every column is
    only as wide as its narrowest row.

    show_avg default (None) is len(runs) > 1 -- right for repeated trials of
    the SAME arm (postprocess.py's convention: "Run 1"/"Run 2"/... averaged
    into a single number). Averaging across DIFFERENT arms (WVA vs KEDA) is
    not a meaningful number, so callers comparing named arms should pass
    show_avg=False explicitly rather than relying on the default.
    """
    if show_avg is None:
        show_avg = len(runs) > 1
    cols = list(labels)
    data_cols = list(runs)
    if show_avg:
        cols.append("Avg")
        data_cols.append(_compute_avg(runs, METRICS))

    metric_w = max(len("Metric"), max(len(m) for m in METRICS))
    rows_cells = [[_fmt(m, run.get(m)) for run in data_cols] for m in METRICS]
    col_w = [
        max(len(col), *(len(rows_cells[i][c]) for i in range(len(METRICS))))
        for c, col in enumerate(cols)
    ]

    def _row(first, cells):
        padded = [f"{first:<{metric_w}}"] + [f"{c:<{w}}" for c, w in zip(cells, col_w)]
        return "| " + " | ".join(padded) + " |"

    header = _row("Metric", cols)
    sep = "|-" + "-" * metric_w + "-|-" + "-|-".join("-" * w for w in col_w) + "-|"
    rows = [_row(m, rows_cells[i]) for i, m in enumerate(METRICS)]
    return "\n".join([header, sep] + rows)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("results_dirs", nargs="+",
                    help="Results directories, one per column (default label: WVA, WVA-2, ...)")
    ap.add_argument("--label", action="append", default=None,
                    help="Column label, one per --label, matched positionally to results_dirs "
                         "in the order given. Defaults to 'WVA' for a single dir.")
    ap.add_argument("--gpus-per-replica", type=int, default=1,
                    help="GPUs per replica, e.g. tensor parallel degree (default: 1)")
    ap.add_argument("--no-avg", action="store_true",
                    help="Omit the trailing Avg column -- use when columns are different "
                         "arms (e.g. WVA vs KEDA), not repeated trials of the same one")
    args = ap.parse_args()

    if args.label and len(args.label) != len(args.results_dirs):
        print("ERROR: --label count must match the number of results_dirs", file=sys.stderr)
        sys.exit(1)
    labels = args.label or (["WVA"] if len(args.results_dirs) == 1
                            else [f"WVA-{i+1}" for i in range(len(args.results_dirs))])

    runs = []
    for d in args.results_dirs:
        if not os.path.isdir(d):
            print(f"WARNING: {d} is not a directory, skipping", file=sys.stderr)
            continue
        print(f"Processing: {d}", file=sys.stderr)
        runs.append(process_one(d, gpus_per_replica=args.gpus_per_replica))

    if not runs:
        print("ERROR: No valid results directories found", file=sys.stderr)
        sys.exit(1)

    print()
    print(format_table(runs, labels, show_avg=False if args.no_avg else None))
    print()

    for d in args.results_dirs:
        why = _collection_failure(d)
        if why:
            print("Some columns are '?' because metrics collection failed:")
            for line in why:
                print("    " + line)
            print("  KV cache, queue depth and pod startup come from in-cluster scrapes,")
            print("  so none were taken.")
            print()
            break


if __name__ == "__main__":
    main()
