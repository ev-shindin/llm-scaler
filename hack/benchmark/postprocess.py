#!/usr/bin/env python3
"""
Post-process llm-d-benchmark results into a markdown table.

Produces the exact table format used in docs/benchmark.md.

Usage:
    # Single run:
    python hack/benchmark/postprocess.py results/guidellm-*_1

    # Three runs (Run 1 | Run 2 | Run 3 | Avg):
    python hack/benchmark/postprocess.py results/guidellm-*_1 \\
                                          results/guidellm-*_2 \\
                                          results/guidellm-*_3

    # With scenario header:
    python hack/benchmark/postprocess.py --scenario "Prefill Heavy — Qwen/Qwen3-32B (600s)" \\
        results/guidellm-*_1 results/guidellm-*_2 results/guidellm-*_3

    # Two-variant run (primary TP=2, secondary TP=1 with suffix "v2"):
    python hack/benchmark/postprocess.py --secondary-suffix v2 \\
        --gpus-per-primary 2 --gpus-per-secondary 1 \\
        results/guidellm-*_1
"""

import argparse
import json
import os
import re
import sys
from datetime import datetime
from statistics import mean, median

METRICS = [
    "P95 TTFT (ms)",
    "P99 TTFT (ms)",
    "P95 ITL (ms/token)",
    "P99 ITL (ms/token)",
    "Avg replicas",
    "Max replicas",
    "Avg KV cache utilization",
    "Avg queue depth (EPP)",
    "Error count",
    "Avg pod startup (s)",
    "Warm binds (woken)",
    "Cold builds (rebuilt)",
    "Median replica start (s)",
]

# Metrics list used for two-variant runs; replica rows are split per variant
# and a weighted-cost row replaces the plain replica rows.
def _variant_metrics(primary_label, secondary_label):
    return [
        "P95 TTFT (ms)",
        "P99 TTFT (ms)",
        "P95 ITL (ms/token)",
        "P99 ITL (ms/token)",
        f"Avg {primary_label} replicas",
        f"Max {primary_label} replicas",
        f"Avg {secondary_label} replicas",
        f"Max {secondary_label} replicas",
        "Avg KV cache utilization",
        "Avg queue depth (EPP)",
        "Error count",
        "Avg pod startup (s)",
        "Cost (weighted avg replicas × GPU/hr)",
        "Warm binds (woken)",
        "Cold builds (rebuilt)",
        "Median replica start (s)",
    ]


def _read_tensor_from_yaml(yaml_path):
    """Return the first `tensor: N` value found in a YAML file, or 1 if absent."""
    if not yaml_path or not os.path.isfile(yaml_path):
        return 1
    with open(yaml_path) as f:
        for line in f:
            m = re.match(r'\s*tensor:\s*(\d+)', line)
            if m:
                return int(m.group(1))
    return 1


def _parse_prometheus_value(line, metric_name):
    """Extract a float from a Prometheus exposition-format line."""
    if not line.startswith(metric_name):
        return None
    rest = line[len(metric_name):]
    if rest and rest[0] not in ("{", " "):
        return None
    if rest.startswith("{"):
        close = rest.find("}")
        if close < 0:
            return None
        rest = rest[close + 1:]
    try:
        return float(rest.strip())
    except (ValueError, IndexError):
        return None


def _extract_latency_guidellm(results_dir):
    """P95 and P99 for TTFT and ITL, from guidellm's results.json.

    P95 as well as P99 because a tail read alone cannot tell a slow run from a
    run with a few slow requests: on the first pokprod run P95 TTFT was 63.5s
    against a P99 of 73.2s, so the bulk of the distribution sat close to the
    tail and the latency was the workload, not an outlier.

    guidellm writes every percentile it computes -- p001 through p999 -- so both
    are read from the same map. `or` is not used to fall back between them: a
    genuine 0.0 is falsy and would silently become max.
    """
    path = os.path.join(results_dir, "results.json")
    if not os.path.isfile(path):
        return None

    with open(path) as f:
        data = json.load(f)

    metrics = data["benchmarks"][0]["metrics"]

    def _pct(section_key, want):
        section = metrics.get(section_key, {}).get("successful", {})
        pcts = section.get("percentiles", {})
        if isinstance(pcts, list):
            pcts = {p["percentile"]: p["value"] for p in pcts}
        for key in (f"p{want}", want):
            if key in pcts:
                return pcts[key]
        return section.get("max")

    ttft, itl = "time_to_first_token_ms", "inter_token_latency_ms"
    return (_pct(ttft, 95), _pct(ttft, 99),
            _pct(itl, 95), _pct(itl, 99))


def _extract_latency_inference_perf(results_dir):
    """P95 and P99 for TTFT and ITL, from inference-perf's own
    summary_lifecycle_metrics.json -- present whether or not per-request
    logging is enabled (report.request_lifecycle.summary is a separate,
    much cheaper flag). Values there are in seconds; the report table's
    columns are in ms, so scale by 1000 here rather than at every caller.
    """
    path = os.path.join(results_dir, "summary_lifecycle_metrics.json")
    if not os.path.isfile(path):
        return None

    with open(path) as f:
        data = json.load(f)

    latency = data.get("successes", {}).get("latency", {})

    def _pct(section_key, want):
        section = latency.get(section_key, {})
        val = section.get(f"p{want}")
        return val * 1000.0 if val is not None else None

    ttft, itl = "time_to_first_token", "inter_token_latency"
    return (_pct(ttft, 95), _pct(ttft, 99),
            _pct(itl, 95), _pct(itl, 99))


def _extract_latency(results_dir):
    """P95/P99 TTFT and ITL, from whichever harness produced this run."""
    return (_extract_latency_guidellm(results_dir)
            or _extract_latency_inference_perf(results_dir)
            or (None, None, None, None))


def _extract_error_count(results_dir):
    """Error count, from whichever harness produced this run."""
    path = os.path.join(results_dir, "results.json")
    if os.path.isfile(path):
        with open(path) as f:
            data = json.load(f)
        return data["benchmarks"][0]["metrics"]["request_totals"].get("errored", 0)

    path = os.path.join(results_dir, "summary_lifecycle_metrics.json")
    if os.path.isfile(path):
        with open(path) as f:
            data = json.load(f)
        return data.get("failures", {}).get("count", 0)

    return 0


# Controller names that carry the replicas actually serving the model.
#
# "decode" is the modelservice layout. "fma-requester" is Fast Model Actuation,
# where the requester Deployment is the scale target and each of its replicas
# binds a launcher pod that runs the engine — so the requester's replica count is
# the served capacity even though the requester itself runs no engine.
#
# Matching only "decode" reported "Avg primary replicas 0.00" for every FMA run,
# which read as "nothing was serving" when four launchers were at 155, 136, 56 and
# 44 concurrent requests.
SERVING_CONTROLLER_MARKERS = ("decode", "fma-requester")


def _is_serving_controller(name):
    return any(marker in name for marker in SERVING_CONTROLLER_MARKERS)


def _extract_replica_stats(results_dir):
    """Avg and max ready replicas from replica_status_timeseries.json."""
    path = os.path.join(results_dir, "metrics", "processed",
                        "replica_status_timeseries.json")
    if not os.path.isfile(path):
        return None, None

    with open(path) as f:
        data = json.load(f)

    totals = []
    for snap in data["snapshots"]:
        ready = sum(
            (c.get("ready_replicas", 0) or 0) for c in snap["controllers"]
            if _is_serving_controller(c.get("name", ""))
        )
        totals.append(ready)

    if not totals:
        return None, None
    return mean(totals), max(totals)


# A replica that WOKE a sleeping vLLM is ready in about 3s; one that had to
# build an instance takes ~50-80s (model load). Nothing in between was ever
# observed, so the threshold is not delicate -- it only has to separate two
# clusters an order of magnitude apart.
WARM_BIND_SECONDS = 15


def _load_pod_timings(results_dir):
    """Per-replica start timings recorded by sample_replicas.sh, or None."""
    path = os.path.join(
        results_dir, "metrics", "processed", "wva_pod_timings.json"
    )
    if not os.path.isfile(path):
        return None
    try:
        with open(path) as f:
            pods = json.load(f).get("pods") or []
    except (ValueError, OSError):
        return None
    return pods or None


def _warm_cold_stats(results_dir):
    """How many replicas woke a sleeping instance, and how many rebuilt one.

    This is the measurement that distinguishes FMA working from FMA present.
    Replica counts cannot show it -- both paths end in "a replica arrived" --
    yet one costs 3s and the other the better part of two minutes. Every
    comparison we ran before measuring it was silently on the slow path,
    because FMA only reuses a sleeping instance when the requester reserved the
    very same GPU, and sleepers created by the launcher-populator never match.

    Returns (warm, cold, median_start_seconds); (None, None, None) if the run
    predates the sampler or recorded nothing -- reported as "not measured"
    rather than zero, which would read as "no warm binds".
    """
    pods = _load_pod_timings(results_dir)
    if not pods:
        return None, None, None

    durations = []
    for p in pods:
        created, ready = p.get("created"), p.get("ready_at")
        if not created or not ready:
            continue
        try:
            c = datetime.strptime(created, "%Y-%m-%dT%H:%M:%SZ")
            r = datetime.strptime(ready, "%Y-%m-%dT%H:%M:%SZ")
        except ValueError:
            continue
        d = (r - c).total_seconds()
        if d >= 0:
            durations.append(d)

    if not durations:
        return None, None, None
    warm = sum(1 for d in durations if d <= WARM_BIND_SECONDS)
    return warm, len(durations) - warm, median(durations)


def _load_replica_timeseries(results_dir):
    """The replica snapshots for a run, from whichever source actually has them.

    The harness writes replica_status_timeseries.json, and on FMA runs it comes
    back with snapshots but no controllers: collect_metrics.sh filters them by
    comparing LLMDBENCH_HARNESS_STACK_NAME (a stack name, e.g.
    "inference-scheduling-wva") against the llm-d.ai/model label (e.g.
    "qwen-qwe-..."), which never match. That cannot be corrected from our side --
    run_only.sh writes the variable into the harness pod from endpoint_stack_name,
    which is also used as --stack.

    So benchmark-run samples the same thing itself into
    wva_replica_samples.json. The harness file is still preferred when it has
    content, so nothing changes on runs where upstream works; ours is the
    fallback, and its absence is reported as "not measured" rather than zero.
    """
    processed = os.path.join(results_dir, "metrics", "processed")
    for name in ("replica_status_timeseries.json", "wva_replica_samples.json"):
        path = os.path.join(processed, name)
        if not os.path.isfile(path):
            continue
        try:
            with open(path) as f:
                data = json.load(f)
        except (OSError, ValueError):
            continue
        if any(s.get("controllers") for s in data.get("snapshots", [])):
            return data
    return None


def _extract_variant_replica_stats(results_dir, secondary_suffix):
    """Per-variant avg/max ready replicas from replica_status_timeseries.json.

    Returns (primary_avg, primary_max, secondary_avg, secondary_max).
    Controllers whose name ends with '-<secondary_suffix>' are secondary;
    all other SERVING controllers are primary — see SERVING_CONTROLLER_MARKERS,
    which covers both the modelservice (decode) and FMA (requester) layouts.
    """
    data = _load_replica_timeseries(results_dir)
    if data is None:
        return None, None, None, None

    primary_totals, secondary_totals = [], []
    saw_controller = False
    for snap in data["snapshots"]:
        p, s = 0, 0
        for c in snap.get("controllers", []):
            name = c.get("name", "")
            ready = c.get("ready_replicas", 0) or 0
            if not _is_serving_controller(name):
                continue
            saw_controller = True
            # Substring, not "-<suffix>". The FMA requester is named
            # fma-requester-<model>, with the marker at the START, so requiring a
            # leading hyphen matched nothing and every FMA replica was counted as
            # primary -- or, when the primary list was empty too, reported as
            # 0.00 replicas for a variant that had demonstrably scaled to 4.
            if secondary_suffix in name:
                s += ready
            else:
                p += ready
        primary_totals.append(p)
        secondary_totals.append(s)

    # Snapshots exist but none carried a serving controller: the collection ran
    # against a namespace that had already been torn down, which is a missing
    # measurement rather than a measurement of zero. Returning 0.00 here reported
    # "no replicas" for a run that used several, and a cost of 0.
    if not primary_totals or not saw_controller:
        return None, None, None, None
    return (mean(primary_totals), max(primary_totals),
            mean(secondary_totals), max(secondary_totals))


def _extract_kv_cache_avg(results_dir):
    """Average KV cache utilization (%) from raw vLLM metrics."""
    raw_dir = os.path.join(results_dir, "metrics", "raw")
    if not os.path.isdir(raw_dir):
        return None

    values = []
    for fname in os.listdir(raw_dir):
        if not fname.endswith(".log") or "router-epp" in fname:
            continue
        if fname == "collection_debug.log":
            continue
        fpath = os.path.join(raw_dir, fname)
        with open(fpath) as f:
            for line in f:
                val = _parse_prometheus_value(line.strip(),
                                              "vllm:kv_cache_usage_perc")
                if val is not None:
                    values.append(val * 100)
                    break

    return mean(values) if values else None


def _extract_queue_depth_avg(results_dir):
    """Average EPP queue depth from raw EPP metrics."""
    raw_dir = os.path.join(results_dir, "metrics", "raw")
    if not os.path.isdir(raw_dir):
        return None

    metric_names = [
        "llm_d_epp_average_queue_size",
    ]

    values = []
    for fname in sorted(os.listdir(raw_dir)):
        if not fname.endswith(".log") or "router-epp" not in fname:
            continue
        fpath = os.path.join(raw_dir, fname)
        found = False
        with open(fpath) as f:
            for line in f:
                stripped = line.strip()
                for metric_name in metric_names:
                    val = _parse_prometheus_value(stripped, metric_name)
                    if val is not None:
                        values.append(val)
                        found = True
                        break
                if found:
                    break

    return mean(values) if values else None


def _extract_pod_startup_avg(results_dir):
    """Average pod startup time (s) from pod_startup_times.json."""
    path = os.path.join(results_dir, "metrics", "processed",
                        "pod_startup_times.json")
    if not os.path.isfile(path):
        return None

    with open(path) as f:
        data = json.load(f)

    times = [p["startup_seconds"] for p in data.get("pods", [])
             if p.get("startup_seconds") is not None]
    return mean(times) if times else None


def _collection_failure(results_dir):
    """Why the metrics columns are empty, if they are.

    A "?" means a value was not collected, and the reason is two levels down in
    files nobody reads: metrics_summary.json says "no raw files found" and then
    GUESSES ("label selector may not match"), while metrics/raw/
    collection_debug.log holds what actually happened. On a real run that was

        open /home/shjohn/.kube/pokprod.token: no such file or directory

    -- the collector inheriting a kubeconfig whose tokenFile does not exist where
    collection runs, so every kubectl call failed. Each of those calls ends in
    `2>/dev/null || true`, which makes an auth failure and an empty cluster the
    same answer. Surfacing the log turns a table of "?" into a diagnosis.
    """
    dbg = os.path.join(results_dir, "metrics", "raw", "collection_debug.log")
    if not os.path.isfile(dbg):
        return None
    seen = []
    try:
        with open(dbg, errors="replace") as f:
            for line in f:
                line = line.strip()
                if not line or line in seen:
                    continue
                low = line.lower()
                if any(t in low for t in ("error", "no such file", "forbidden",
                                          "unauthorized", "refused", "not found")):
                    seen.append(line)
                if len(seen) >= 3:
                    break
    except OSError:
        return None
    return seen or None


def _fmt(metric, value):
    """Format a value to match the benchmark.md number style."""
    if value is None:
        return "?"

    if metric in ("P95 TTFT (ms)", "P99 TTFT (ms)"):
        return f"{value:,.0f}"
    if metric in ("P95 ITL (ms/token)", "P99 ITL (ms/token)"):
        return f"{value:.2f}" if (value * 100) % 10 != 0 else f"{value:.1f}"
    if metric in ("Avg replicas",) or metric.startswith("Avg ") and "replicas" in metric:
        return f"{value:.2f}"
    if metric in ("Max replicas",) or metric.startswith("Max ") and "replicas" in metric:
        return str(int(value))
    if metric == "Avg KV cache utilization":
        return f"{value:.1f}%"
    if metric == "Avg queue depth (EPP)":
        return f"{value:.1f}"
    if metric == "Error count":
        return f"{int(value):,}"
    if metric == "Avg pod startup (s)":
        return str(round(value))
    if metric == "Cost (weighted avg replicas × GPU/hr)":
        return f"{value:.2f}"
    if metric in ("Warm binds (woken)", "Cold builds (rebuilt)"):
        return f"{int(value)}"
    if metric == "Median replica start (s)":
        return f"{value:.0f}"
    return str(value)


def process_one(results_dir, secondary_suffix=None, gpus_per_primary=1,
                gpus_per_secondary=1, primary_label="primary",
                secondary_label="secondary"):
    """Extract all benchmark.md metrics from one results directory.

    When secondary_suffix is given, replica stats are split per variant and a
    weighted cost row is included.
    """
    p95_ttft, p99_ttft, p95_itl, p99_itl = _extract_latency(results_dir)
    kv_avg = _extract_kv_cache_avg(results_dir)
    queue_avg = _extract_queue_depth_avg(results_dir)
    startup_avg = _extract_pod_startup_avg(results_dir)
    error_count = _extract_error_count(results_dir)
    warm_n, cold_n, start_median = _warm_cold_stats(results_dir)

    if secondary_suffix:
        p_avg, p_max, s_avg, s_max = _extract_variant_replica_stats(
            results_dir, secondary_suffix)
        cost = None
        if p_avg is not None and s_avg is not None:
            cost = p_avg * gpus_per_primary + s_avg * gpus_per_secondary
        return {
            "P95 TTFT (ms)": p95_ttft,
            "P99 TTFT (ms)": p99_ttft,
            "P95 ITL (ms/token)": p95_itl,
            "P99 ITL (ms/token)": p99_itl,
            f"Avg {primary_label} replicas": p_avg,
            f"Max {primary_label} replicas": p_max,
            f"Avg {secondary_label} replicas": s_avg,
            f"Max {secondary_label} replicas": s_max,
            "Avg KV cache utilization": kv_avg,
            "Avg queue depth (EPP)": queue_avg,
            "Error count": error_count,
            "Avg pod startup (s)": startup_avg,
            "Cost (weighted avg replicas × GPU/hr)": cost,
            "Warm binds (woken)": warm_n,
            "Cold builds (rebuilt)": cold_n,
            "Median replica start (s)": start_median,
        }

    avg_rep, max_rep = _extract_replica_stats(results_dir)
    return {
        "P95 TTFT (ms)": p95_ttft,
        "P99 TTFT (ms)": p99_ttft,
        "P95 ITL (ms/token)": p95_itl,
        "P99 ITL (ms/token)": p99_itl,
        "Avg replicas": avg_rep,
        "Max replicas": max_rep,
        "Avg KV cache utilization": kv_avg,
        "Avg queue depth (EPP)": queue_avg,
        "Error count": error_count,
        "Avg pod startup (s)": startup_avg,
        "Warm binds (woken)": warm_n,
        "Cold builds (rebuilt)": cold_n,
        "Median replica start (s)": start_median,
    }


def _compute_avg(runs, metrics):
    """Compute average column across multiple runs (raw numeric values)."""
    avg = {}
    for m in metrics:
        vals = [r[m] for r in runs if r.get(m) is not None]
        avg[m] = mean(vals) if vals else None
    return avg


def format_table(runs, labels, metrics=None):
    """Render a markdown table matching the benchmark.md style."""
    if metrics is None:
        metrics = METRICS
    show_avg = len(runs) > 1
    cols = list(labels)
    data_cols = list(runs)

    if show_avg:
        cols.append("Avg")
        data_cols.append(_compute_avg(runs, metrics))

    header = "| Metric | " + " | ".join(cols) + " |"
    sep = "|--------|" + "|".join(["------"] * len(cols)) + "|"

    rows = []
    for m in metrics:
        cells = [_fmt(m, run.get(m)) for run in data_cols]
        rows.append(f"| {m} | " + " | ".join(cells) + " |")

    return "\n".join([header, sep] + rows)


def main():
    """CLI entry point."""
    ap = argparse.ArgumentParser(
        description="Post-process llm-d-benchmark results into a markdown table")
    ap.add_argument("results_dirs", nargs="+",
                    help="One or more benchmark results directories")
    ap.add_argument("--scenario", type=str, default=None,
                    help="Scenario heading (e.g. 'Prefill Heavy — Qwen/Qwen3-32B (600s)')")
    ap.add_argument("--json", action="store_true",
                    help="Output raw JSON instead of markdown")
    ap.add_argument("--secondary-suffix", type=str, default=None,
                    help="Controller-name suffix that identifies the secondary variant "
                         "(e.g. 'v2'); enables per-variant replica rows and weighted cost")
    ap.add_argument("--primary-label", type=str, default="primary",
                    help="Label for the primary variant in table rows (default: primary)")
    ap.add_argument("--secondary-label", type=str, default="secondary",
                    help="Label for the secondary variant in table rows (default: secondary)")
    ap.add_argument("--scenario-yaml", type=str, default=None,
                    help="Path to the primary scenario YAML; tensor: value sets gpus-per-primary")
    ap.add_argument("--variant-config", type=str, default=None,
                    help="Path to the secondary variant config YAML; tensor: value sets gpus-per-secondary")
    args = ap.parse_args()

    gpus_per_primary = _read_tensor_from_yaml(args.scenario_yaml)
    gpus_per_secondary = _read_tensor_from_yaml(args.variant_config)

    metrics = (
        _variant_metrics(args.primary_label, args.secondary_label)
        if args.secondary_suffix
        else METRICS
    )

    runs = []
    labels = []
    for d in args.results_dirs:
        if not os.path.isdir(d):
            print(f"WARNING: {d} is not a directory, skipping", file=sys.stderr)
            continue
        print(f"Processing: {d}", file=sys.stderr)
        runs.append(process_one(
            d,
            secondary_suffix=args.secondary_suffix,
            gpus_per_primary=gpus_per_primary,
            gpus_per_secondary=gpus_per_secondary,
            primary_label=args.primary_label,
            secondary_label=args.secondary_label,
        ))
        labels.append(f"Run {len(runs)}")

    if not runs:
        print("ERROR: No valid results directories found", file=sys.stderr)
        sys.exit(1)

    if args.json:
        print(json.dumps(runs, indent=2))
        return

    print()
    if args.scenario:
        print(f"### {args.scenario}\n")
    print(format_table(runs, labels, metrics))
    print()

    # A "?" is not a measurement of zero, and the reason is knowable.
    # Printed once, after the table, so a reader who sees empty columns is
    # told why instead of concluding the run had no KV cache or no queue.
    for _d in args.results_dirs:
        _why = _collection_failure(_d)
        if _why:
            print("Some columns are '?' because metrics collection failed:")
            for _line in _why:
                print("    " + _line)
            print("  KV cache, queue depth and pod startup come from in-cluster")
            print("  scrapes, so none were taken.")
            print()
            break


if __name__ == "__main__":
    main()
