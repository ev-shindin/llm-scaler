#!/usr/bin/env python3
"""Generate the two-variant V2 full-pipeline 5-panel timeseries plot.

Mirrors `two_variant_v2_full_pipeline_v3.png` from biran-20260527-101013-246.
Panels: Replica Count | KV Cache Util (avg per variant) | Requests Running
(sum per variant) | vLLM Requests Waiting (sum per variant) | EPP Queue Metrics.
"""
import argparse
import json
import os
import re
import subprocess
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path

try:
    import yaml
except ImportError:
    yaml = None

# Plotting is optional. matplotlib is not a dependency of running a benchmark —
# it is only needed to draw the picture afterwards — so a machine without it must
# get one clear line, not a traceback and a failed make target. The benchmark has
# already succeeded by the time this runs; the numbers are in the results
# directory either way.
try:
    import matplotlib.dates as mdates
    import matplotlib.pyplot as plt
    from matplotlib.ticker import MaxNLocator
    from matplotlib.transforms import blended_transform_factory
except ImportError:
    print("Skipping the two-variant plot: matplotlib is not installed "
          "(pip install matplotlib). The benchmark results are unaffected.")
    raise SystemExit(0)

PRIMARY_COLOR = "#1f77b4"
V2_COLOR = "#d62728"

VLLM_METRICS = {
    "kv": re.compile(r"^vllm:kv_cache_usage_perc\{[^}]*\}\s+([0-9.eE+-]+)"),
    "running": re.compile(r"^vllm:num_requests_running\{[^}]*\}\s+([0-9.eE+-]+)"),
    "waiting": re.compile(r"^vllm:num_requests_waiting\{[^}]*\}\s+([0-9.eE+-]+)"),
}
EPP_METRICS = {
    "fc_queue": re.compile(
        r"^inference_extension_flow_control_queue_size\{[^}]*\}\s+([0-9.eE+-]+)"
    ),
    "pool_avg": re.compile(
        r"^inference_pool_average_queue_size\{[^}]*\}\s+([0-9.eE+-]+)"
    ),
    "per_pod": re.compile(
        r'^inference_pool_per_pod_queue_size\{model_server_pod="([^"]+)"[^}]*\}\s+([0-9.eE+-]+)'
    ),
}

FILE_RE = re.compile(r"^(?P<pod>.+?)_(?P<ts>\d{10})_metrics\.log$")


def parse_pod_log(path: Path):
    """Extract vllm metrics from a single decode pod log. Returns dict or None."""
    try:
        text = path.read_text()
    except Exception:
        return None
    if '"object":"error"' in text:
        return None
    out = {}
    for line in text.splitlines():
        for k, rx in VLLM_METRICS.items():
            if k in out:
                continue
            m = rx.match(line)
            if m:
                out[k] = float(m.group(1))
    return out or None


def parse_epp_log(path: Path):
    try:
        text = path.read_text()
    except Exception:
        return None
    fc = pa = None
    per_pod = defaultdict(float)
    for line in text.splitlines():
        if fc is None:
            m = EPP_METRICS["fc_queue"].match(line)
            if m:
                fc = float(m.group(1))
                continue
        if pa is None:
            m = EPP_METRICS["pool_avg"].match(line)
            if m:
                pa = float(m.group(1))
                continue
        m = EPP_METRICS["per_pod"].match(line)
        if m:
            per_pod[m.group(1)] = float(m.group(2))
    return {
        "fc_queue": fc or 0.0,
        "pool_avg": pa or 0.0,
        "per_pod": dict(per_pod),
    }


def collect(raw_dir: Path):
    decode_series = defaultdict(list)  # ts -> list of (pod, metrics dict)
    epp_series = []  # list of (ts, epp_dict)
    for f in sorted(raw_dir.glob("*_metrics.log")):
        m = FILE_RE.match(f.name)
        if not m:
            continue
        ts = int(m.group("ts"))
        pod = m.group("pod")
        if "gaie-epp" in pod or "router-epp" in pod:
            ed = parse_epp_log(f)
            if ed:
                epp_series.append((ts, ed))
        elif "decode" in pod or "prefill" in pod:
            md = parse_pod_log(f)
            if md is None:
                continue
            decode_series[ts].append((pod, md))
    return decode_series, epp_series


def is_v2(pod_name: str) -> bool:
    # "-decode-v2-": the cost-tier decode sibling from two-variant-wva.
    # "prefill": P/D disaggregation's second role. Bucketed into the same
    # "v2" slot deliberately, rather than adding a real third variant kind --
    # labels_for() below picks the right display text either way, so the
    # data is correctly split regardless of which convention produced it.
    return "-decode-v2-" in pod_name or "prefill" in pod_name


def labels_for(decode_series) -> tuple[str, str]:
    """(primary_label, secondary_label) for this run's legends/titles.
    ("decode", "prefill") when the "v2" bucket was populated by the P/D role
    marker rather than a real cost-tier "-v2-" sibling, else the literal
    ("primary", "v2") this file's data model was originally built around."""
    is_pd = any("prefill" in pod for pods in decode_series.values() for pod, _ in pods)
    return ("decode", "prefill") if is_pd else ("primary", "v2")


def aggregate_decode(decode_series):
    """Per timestamp: avg KV (per variant), sum running, sum waiting."""
    rows = []
    for ts in sorted(decode_series.keys()):
        kvs = {"primary": [], "v2": []}
        runs = {"primary": 0.0, "v2": 0.0}
        waits = {"primary": 0.0, "v2": 0.0}
        for pod, m in decode_series[ts]:
            tag = "v2" if is_v2(pod) else "primary"
            if "kv" in m:
                kvs[tag].append(m["kv"])
            if "running" in m:
                runs[tag] += m["running"]
            if "waiting" in m:
                waits[tag] += m["waiting"]
        rows.append({
            "ts": ts,
            "kv_primary": (sum(kvs["primary"]) / len(kvs["primary"]) * 100.0)
                if kvs["primary"] else None,
            "kv_v2": (sum(kvs["v2"]) / len(kvs["v2"]) * 100.0)
                if kvs["v2"] else None,
            "run_primary": runs["primary"],
            "run_v2": runs["v2"],
            "wait_primary": waits["primary"],
            "wait_v2": waits["v2"],
        })
    return rows


def epp_panels(epp_series):
    rows = []
    for ts, ed in sorted(epp_series, key=lambda x: x[0]):
        per_pod = ed["per_pod"]
        sum_p = sum(v for k, v in per_pod.items() if not is_v2(k))
        sum_v = sum(v for k, v in per_pod.items() if is_v2(k))
        rows.append({
            "ts": ts,
            "fc_queue": ed["fc_queue"],
            "pool_avg": ed["pool_avg"],
            "per_pod_primary": sum_p,
            "per_pod_v2": sum_v,
        })
    return rows


def replica_timeseries(results_dir: Path):
    p = results_dir / "metrics" / "processed" / "replica_status_timeseries.json"
    snaps = json.loads(p.read_text())["snapshots"]
    out = []
    for s in snaps:
        ts = int(datetime.fromisoformat(s["timestamp"].replace("Z", "+00:00")).timestamp())
        prim = v2 = 0
        for c in s["controllers"]:
            # Same "prefill buckets as v2" simplification as is_v2() above.
            if c["name"].endswith("-v2") or "prefill" in c["name"]:
                v2 = c["ready_replicas"]
            else:
                prim = c["ready_replicas"]
        out.append((ts, prim, v2))
    return out


def wva_target_timeseries(results_dir: Path):
    """Optional overlay: WVA's per-variant target decisions. Returns [] if not present."""
    p = results_dir / "metrics" / "processed" / "wva_target_timeseries.json"
    if not p.is_file():
        return []
    samples = json.loads(p.read_text()).get("samples", [])
    return [(int(s["timestamp"]), s.get("primary"), s.get("v2")) for s in samples]


def wva_supply_demand_timeseries(results_dir: Path):
    """WVA-side analyzer numbers (totalSupply/Demand etc.). Returns [] if absent.
    Output rows: (ts, supply, demand, util, required, spare).
    """
    p = results_dir / "metrics" / "processed" / "wva_target_timeseries.json"
    if not p.is_file():
        return []
    samples = json.loads(p.read_text()).get("samples", [])
    rows = []
    for s in samples:
        if s.get("totalSupply") is None and s.get("totalDemand") is None:
            continue
        rows.append((
            int(s["timestamp"]),
            s.get("totalSupply"),
            s.get("totalDemand"),
            s.get("utilization"),
            s.get("requiredCapacity"),
            s.get("spareCapacity"),
        ))
    return rows


def capacity_demand_estimate(results_dir: Path):
    """Estimated capacity & 3-component demand from raw vLLM/EPP scrapes.
    Returns [] if not present."""
    p = results_dir / "metrics" / "processed" / "capacity_demand_estimate.json"
    if not p.is_file():
        return []
    return json.loads(p.read_text()).get("samples", [])


def epp_throughput(results_dir: Path):
    """Per-window request and token rates derived from EPP counters."""
    p = results_dir / "metrics" / "processed" / "epp_throughput.json"
    if not p.is_file():
        return []
    return json.loads(p.read_text()).get("samples", [])


def wva_metrics_per_variant(results_dir: Path):
    """Per-variant WVA Prometheus metrics over time. Returns:
       [{ts, primary: {metric_name: value, ...}, v2: {...}, _model: {...}}, ...]"""
    p = results_dir / "metrics" / "processed" / "wva_metrics_timeseries.json"
    if not p.is_file():
        return []
    return json.loads(p.read_text()).get("samples", [])


def to_dt(ts):
    return datetime.fromtimestamp(ts, tz=timezone.utc)


def _load_experiment_metadata(results_dir: Path):
    """Best-effort description of the workload and its load schedule, for the
    plot title and the stage-boundary markers. Reads run_metadata.yaml (always
    written by the harness) plus the saved workload file it points at, and
    understands both scenario shapes used in this repo: inference-perf's
    `load.stages` / `data.*_distribution`, and guidellm's `spec.profile.rate`
    (a scalar or a sweep list) / `spec.constraints[].max_duration` /
    `spec.data[0].prompt_tokens`/`output_tokens`.

    Returns {} (not an error) if yaml is unavailable, or run_metadata.yaml or
    the workload file is missing/unrecognized — the plot still renders, just
    without these extras.
    """
    meta = {}
    if yaml is None:
        return meta
    run_meta_path = results_dir / "run_metadata.yaml"
    if not run_meta_path.is_file():
        return meta
    try:
        run_meta = yaml.safe_load(run_meta_path.read_text()) or {}
    except Exception:
        return meta

    meta["namespace"] = run_meta.get("namespace")
    meta["model"] = run_meta.get("model")
    start = run_meta.get("harness_start")
    if start:
        try:
            meta["harness_start_epoch"] = int(datetime.fromisoformat(start).timestamp())
        except ValueError:
            pass

    workload = {}
    workload_name = run_meta.get("harness_workload")
    if workload_name:
        # guidellm doesn't copy its rendered profile into
        # results/<treatment>_<i>/ (only inference-perf does) -- it stays at
        # <run_dir>/workload/profiles/<harness>/<name>, three levels above
        # results_dir. Try both rather than assuming one harness's layout.
        candidates = [results_dir / workload_name]
        harness_name = run_meta.get("harness_name")
        if harness_name:
            candidates.append(
                results_dir.parent.parent / "workload" / "profiles" / harness_name / workload_name
            )
        for wp in candidates:
            if wp.is_file():
                try:
                    workload = yaml.safe_load(wp.read_text()) or {}
                except Exception:
                    workload = {}
                break

    stages = []
    input_tokens = output_tokens = None
    if "load" in workload:  # inference-perf shape
        for s in workload.get("load", {}).get("stages", []) or []:
            stages.append((s.get("rate"), s.get("duration")))
        din = (workload.get("data") or {}).get("input_distribution") or {}
        dout = (workload.get("data") or {}).get("output_distribution") or {}
        input_tokens = din.get("mean", din.get("max"))
        output_tokens = dout.get("mean", dout.get("max"))
    elif "spec" in workload:  # guidellm shape
        spec = workload["spec"]
        profile = spec.get("profile") or {}
        if profile.get("kind") == "replay":
            # Trace-replay workloads (hack/benchmark/gen_shape_trace.py):
            # spec.profile carries no rate and spec.data[0] only names the
            # trace file, so neither token shape nor the per-phase schedule
            # is in this rendered profile at all -- the trace generator's own
            # *.params.yaml has both. Located by convention (same basename as
            # the trace file, in test/benchmark/scenarios/) rather than a
            # path recorded anywhere in the run, since the rendered profile
            # doesn't carry one.
            data0 = (spec.get("data") or [{}])[0]
            stem = Path(data0.get("path", "")).stem
            params_path = (
                Path(__file__).resolve().parents[2]
                / "test" / "benchmark" / "scenarios" / f"{stem}.params.yaml"
            )
            phases = []
            if stem and params_path.is_file():
                try:
                    params = yaml.safe_load(params_path.read_text()) or {}
                    for p in params.get("phases") or []:
                        phases.append({
                            "rate": p.get("rate_rps"),
                            "duration": p.get("duration_s"),
                            "input_tokens": (p.get("input_tokens") or {}).get("mean"),
                            "output_tokens": (p.get("output_tokens") or {}).get("mean"),
                        })
                except Exception:
                    phases = []
            meta["shape_phases"] = phases
            for p in phases:
                stages.append((p.get("rate"), p.get("duration")))
        else:
            rate = profile.get("rate")
            duration = None
            for c in spec.get("constraints") or []:
                if c.get("kind") == "max_duration":
                    duration = c.get("seconds")
            for r in (rate if isinstance(rate, list) else [rate]):
                stages.append((r, duration))
            data0 = (spec.get("data") or [{}])[0]
            input_tokens = data0.get("prompt_tokens")
            output_tokens = data0.get("output_tokens")

    meta["stages"] = stages
    meta["input_tokens"] = input_tokens
    meta["output_tokens"] = output_tokens
    return meta


def _load_keda_policy(namespace, model_id):
    """Best-effort KEDA scaleUp/scaleDown behavior for this model's
    ScaledObject, read live via kubectl. Matches on the `llm-d.ai/model-id`
    annotation (current annotation-based discovery) or, failing that, the
    trigger's `modelID` (older ScaledObjects predating that migration).
    Returns None if kubectl is unavailable, times out, or nothing matches —
    the title simply omits this line rather than failing the plot.
    """
    if not namespace:
        return None
    try:
        proc = subprocess.run(
            ["kubectl", "get", "scaledobject", "-n", namespace, "-o", "json"],
            capture_output=True, text=True, timeout=10,
        )
        if proc.returncode != 0:
            return None
        data = json.loads(proc.stdout)
    except Exception:
        return None
    for item in data.get("items", []):
        ann = (item.get("metadata") or {}).get("annotations") or {}
        triggers = (item.get("spec") or {}).get("triggers") or []
        trig_model = next((t.get("metadata", {}).get("modelID") for t in triggers), None)
        if model_id and ann.get("llm-d.ai/model-id") != model_id and trig_model != model_id:
            continue
        behavior = ((item.get("spec") or {}).get("advanced", {}) or {}) \
            .get("horizontalPodAutoscalerConfig", {}).get("behavior")
        if behavior:
            return behavior
    return None


def _format_workload_line(meta):
    parts = []
    phases = meta.get("shape_phases")
    if phases:
        # Trace-replay shape-swap run: the schedule's real content is the
        # token-shape change, not rate (constant across every phase in every
        # such run so far) -- lead with the shape, and only spell out RPS
        # per-phase if it actually varies.
        shape_str = " → ".join(
            f"{p['input_tokens']:g}/{p['output_tokens']:g}"
            for p in phases if p.get("input_tokens") is not None
        )
        if shape_str:
            parts.append(f"{shape_str} in/out tokens")
        durations = {p["duration"] for p in phases if p.get("duration")}
        if len(durations) == 1:
            d = durations.pop()
            parts.append(f"{d / 60:g}m/phase" if d >= 60 else f"{d:g}s/phase")
        rates = {p["rate"] for p in phases if p.get("rate") is not None}
        if len(rates) == 1:
            parts.append(f"RPS {rates.pop():g} throughout")
        elif rates:
            parts.append("RPS " + " → ".join(f"{p.get('rate'):g}" for p in phases))
        return "   |   ".join(parts) if parts else None
    if meta.get("input_tokens") is not None and meta.get("output_tokens") is not None:
        parts.append(f"{meta['input_tokens']:g}/{meta['output_tokens']:g} in/out tokens")
    stages = meta.get("stages")
    if stages:
        stage_str = " → ".join(
            f"{r:g} RPS" + (f" ({d:g}s)" if d else "")
            for r, d in stages if r is not None
        )
        parts.append(f"RPS stages: {stage_str}")
    return "   |   ".join(parts) if parts else None


def _format_keda_line(behavior):
    if not behavior:
        return None
    up = behavior.get("scaleUp") or {}
    down = behavior.get("scaleDown") or {}
    up_pol = (up.get("policies") or [{}])[0]
    down_pol = (down.get("policies") or [{}])[0]
    return (
        f"KEDA: scaleUp +{up_pol.get('value', '?')}%/{up_pol.get('periodSeconds', '?')}s"
        f" (stab {up.get('stabilizationWindowSeconds', '?')}s)"
        f"   scaleDown +{down_pol.get('value', '?')}%/{down_pol.get('periodSeconds', '?')}s"
        f" (stab {down.get('stabilizationWindowSeconds', '?')}s)"
    )


def _stage_boundaries(meta):
    """Absolute epoch timestamps (and a short label for the new stage) at
    each load-schedule transition, for vertical markers. Excludes the run's
    own start — stage 0 needs no marker, it's the left edge of the plot.

    Labels with the new token shape for trace-replay runs (shape_phases
    present): that is the schedule's real content there, and RPS constant
    across phases would repeat the same uninformative number at every
    marker. Otherwise labels with the new rate, as before.
    """
    start = meta.get("harness_start_epoch")
    stages = meta.get("stages") or []
    phases = meta.get("shape_phases") or []
    if start is None:
        return []
    boundaries = []
    t = start
    for i, (rate, duration) in enumerate(stages):
        if i > 0:
            if i < len(phases) and phases[i].get("input_tokens") is not None:
                label = f"{phases[i]['input_tokens']:g}/{phases[i]['output_tokens']:g} tok"
            else:
                label = f"{rate:g} RPS"
            boundaries.append((t, label))
        if not duration:
            break
        t += duration
    return boundaries


def plot(results_dir: Path, out_path: Path, title_suffix: str):
    decode_series, epp_series = collect(results_dir / "metrics" / "raw")
    # Ground truth for "is this actually a two-variant run": a decode pod
    # whose name matches the -v2- convention was scraped at least once.
    # Every v2 series in a single-variant run is otherwise just a flat zero
    # line -- worth omitting rather than cluttering every legend with an
    # entry that never carries information.
    has_v2 = any(is_v2(pod) for pods in decode_series.values() for pod, _ in pods)
    PRIM, SEC = labels_for(decode_series)
    drows = aggregate_decode(decode_series)
    erows = epp_panels(epp_series)
    repls = replica_timeseries(results_dir)
    wva_targets = wva_target_timeseries(results_dir)
    wva_sd = wva_supply_demand_timeseries(results_dir)
    cd_est = capacity_demand_estimate(results_dir)
    exp_meta = _load_experiment_metadata(results_dir)
    keda_behavior = _load_keda_policy(exp_meta.get("namespace"), exp_meta.get("model"))

    has_supply_demand = bool(wva_sd or cd_est)
    epp_rates = epp_throughput(results_dir)
    has_rates = bool(epp_rates)
    wva_full = wva_metrics_per_variant(results_dir)
    has_wva_full = bool(wva_full)
    # Skip the EPP-queue panel when it carries no real data (e.g. on v0.7.0
    # the renamed router-epp isn't scraped by the gaie-keyed collector), so the
    # plot doesn't show a null panel.
    has_epp = bool(erows) and any(
        (r.get("fc_queue") or r.get("pool_avg")
         or r.get("per_pod_primary") or r.get("per_pod_v2")) for r in erows)
    epp_shift = 0 if has_epp else 1
    n_extra = (1 if has_supply_demand else 0) + (1 if has_rates else 0) + (2 if has_wva_full else 0)
    n_panels = (5 if has_epp else 4) + n_extra
    fig, axes = plt.subplots(
        n_panels, 1,
        figsize=(8, 11 + 2 * n_extra),
        sharex=True,
    )
    # Panel offset for the original 5 panels: increment for each optional panel inserted before them.
    base = 1 if has_supply_demand else 0

    # 1. Replica Count (actual ready) + optional overlay of WVA target decisions
    ax = axes[0]
    title = "Replica Count"
    if wva_targets:
        title += " — solid: ready,  dashed: WVA desired"
    ax.set_title(title, pad=8)
    if repls:
        x = [to_dt(r[0]) for r in repls]
        ax.step(x, [r[1] for r in repls], where="post", color=PRIMARY_COLOR, label=f"{PRIM} (ready)", linewidth=2)
        if has_v2:
            ax.step(x, [r[2] for r in repls], where="post", color=V2_COLOR, label=f"{SEC} (ready)", linewidth=2)
    if wva_targets:
        xt = [to_dt(t[0]) for t in wva_targets]
        prim_t = [t[1] for t in wva_targets]
        ax.step(xt, prim_t, where="post", color=PRIMARY_COLOR, linestyle="--", linewidth=1.4,
                label=f"{PRIM} (WVA target)", alpha=0.8)
        if has_v2:
            v2_t = [t[2] for t in wva_targets]
            ax.step(xt, v2_t, where="post", color=V2_COLOR, linestyle="--", linewidth=1.4,
                    label=f"{SEC} (WVA target)", alpha=0.8)
    ax.set_ylabel("Replicas")
    ax.yaxis.set_major_locator(MaxNLocator(integer=True))
    ax.legend(loc="best", fontsize=7)
    ax.grid(alpha=0.3)

    # 1b. (Optional) Estimated Capacity & Demand — tokens
    # Stacked bars per scrape snapshot show the 3-component demand
    # decomposition (in-use / vLLM waiting / EPP queue). Capacity is a
    # step line on top — bars exceeding it indicate over-saturation.
    # WVA-analyzer numbers from the controller log overlay as markers
    # when present (typically a sparse subset of reconciles).
    if has_supply_demand:
        ax = axes[1]
        ax.set_title("Estimated Demand (stacked) vs Capacity  "
                     "[bars from raw vLLM+EPP scrapes; ●  = WVA analyzer]")
        if cd_est:
            xs = [to_dt(r["timestamp"]) for r in cd_est]
            in_use = [r["demandInUse"] for r in cd_est]
            waiting = [r["demandWaitingPods"] for r in cd_est]
            eppq = [r["demandEppQueue"] for r in cd_est]
            cap = [r["capacityRaw"] for r in cd_est]
            # Bar width based on sample cadence (matplotlib date units = days).
            if len(xs) >= 2:
                interval_sec = max(
                    (cd_est[i + 1]["timestamp"] - cd_est[i]["timestamp"])
                    for i in range(len(cd_est) - 1)
                ) or 30
                width_days = (interval_sec * 0.9) / 86400.0
            else:
                width_days = 15.0 / 86400.0
            base_lower = [0.0] * len(xs)
            base_mid = [a + b for a, b in zip(base_lower, in_use)]
            base_top = [a + b for a, b in zip(base_mid, waiting)]
            ax.bar(xs, in_use, width=width_days, bottom=base_lower,
                   color="#1f77b4", edgecolor="none",
                   label="in-use (KV occupancy)")
            ax.bar(xs, waiting, width=width_days, bottom=base_mid,
                   color="#ff7f0e", edgecolor="none",
                   label="+ vLLM waiting queue")
            ax.bar(xs, eppq, width=width_days, bottom=base_top,
                   color="#d62728", edgecolor="none",
                   label="+ EPP queue (gateway)")
            ax.step(xs, cap, where="post", color="black", linewidth=2,
                    label="capacity (Σ num_gpu_blocks·block_size)")
        if wva_sd:
            xs_sup = [to_dt(r[0]) for r in wva_sd if r[1] is not None]
            sup = [r[1] for r in wva_sd if r[1] is not None]
            xs_dem = [to_dt(r[0]) for r in wva_sd if r[2] is not None]
            dem = [r[2] for r in wva_sd if r[2] is not None]
            if sup:
                ax.scatter(xs_sup, sup, color="black", marker="o", s=24, zorder=5,
                           label="WVA totalSupply")
            if dem:
                ax.scatter(xs_dem, dem, edgecolor="black", facecolor="#d62728",
                           marker="o", s=24, linewidths=0.6, zorder=5,
                           label="WVA totalDemand")
        ax.set_ylabel("Tokens")
        ax.legend(loc="upper left", fontsize=7, ncol=2)
        ax.grid(alpha=0.3, axis="y")
        ax.ticklabel_format(axis="y", style="sci", scilimits=(0, 0))

    # 2. KV Cache Utilization
    ax = axes[1 + base]
    ax.set_title("KV Cache Utilization (avg per variant)")
    if drows:
        x = [to_dt(r["ts"]) for r in drows]
        ax.plot(x, [r["kv_primary"] for r in drows], color=PRIMARY_COLOR, label=PRIM)
        if has_v2:
            ax.plot(x, [r["kv_v2"] for r in drows], color=V2_COLOR, label=SEC)
    ax.set_ylabel("KV %")
    ax.set_ylim(0, 100)
    ax.legend(loc="upper right", fontsize=8)
    ax.grid(alpha=0.3)

    # 3. Requests Running
    ax = axes[2 + base]
    ax.set_title("Requests Running (sum per variant)")
    if drows:
        x = [to_dt(r["ts"]) for r in drows]
        ax.plot(x, [r["run_primary"] for r in drows], color=PRIMARY_COLOR, label=PRIM)
        if has_v2:
            ax.plot(x, [r["run_v2"] for r in drows], color=V2_COLOR, label=SEC)
    ax.set_ylabel("Running")
    ax.legend(loc="upper left", fontsize=8)
    ax.grid(alpha=0.3)

    # 4. Requests Waiting
    ax = axes[3 + base]
    ax.set_title("vLLM Requests Waiting (sum per variant)")
    if drows:
        x = [to_dt(r["ts"]) for r in drows]
        ax.plot(x, [r["wait_primary"] for r in drows], color=PRIMARY_COLOR, label=PRIM)
        if has_v2:
            ax.plot(x, [r["wait_v2"] for r in drows], color=V2_COLOR, label=SEC)
    ax.set_ylabel("Waiting")
    ax.legend(loc="upper left", fontsize=8)
    ax.grid(alpha=0.3)

    # 5. EPP Queue (skipped entirely when it has no real data)
    if has_epp:
        ax = axes[4 + base]
        ax.set_title("EPP Queue Metrics (single y-axis, all in same units)")
        x = [to_dt(r["ts"]) for r in erows]
        ax.plot(x, [r["fc_queue"] for r in erows], color="black", label="flow_control_queue (gateway)")
        ax.plot(x, [r["pool_avg"] for r in erows], color="orange", label="pool_average_queue", alpha=0.8)
        ax.plot(x, [r["per_pod_primary"] for r in erows], color=PRIMARY_COLOR, linestyle="--", label=f"per pod sum: {PRIM}")
        if has_v2:
            ax.plot(x, [r["per_pod_v2"] for r in erows], color=V2_COLOR, linestyle="--", label=f"per pod sum: {SEC}")
        ax.set_ylabel("Requests in queue")
        ax.legend(loc="best", fontsize=7)
        ax.grid(alpha=0.3)

    # 6. (Optional) Request rate from EPP counters
    if has_rates:
        ax = axes[5 + base - epp_shift]
        ax.set_title("Gateway throughput  (EPP counters → finite-difference rate)")
        x = [to_dt(s["timestamp"]) for s in epp_rates]
        rps = [s.get("rates", {}).get("request_total_per_s", 0.0) for s in epp_rates]
        err_ps = [s.get("rates", {}).get("request_error_total_per_s", 0.0) for s in epp_rates]
        ax.plot(x, rps, color="black", linewidth=2, label="requests/s (offered)")
        if any(v for v in err_ps if v):
            ax.plot(x, err_ps, color="#d62728", linewidth=1.4, label="errors/s")
        ax.set_ylabel("req / s")
        ax.legend(loc="upper left", fontsize=7)
        ax.grid(alpha=0.3)

    # 7. + 8. (Optional) WVA-analyzer per-variant metrics — only when the
    # patched harness scraped the WVA controller during the run.
    if has_wva_full:
        x_wva = [to_dt(s["timestamp"]) for s in wva_full]

        # Panel 7: per-variant wva_saturation_utilization (the analyzer's own
        # internal "how loaded is each variant" reading; differs from the
        # KV-only panel because it folds in queue contributions and uses the
        # capacity-weighted formula).
        ax = axes[5 + base + (1 if has_rates else 0) - epp_shift]
        ax.set_title(
            "WVA Saturation Utilization  (per variant, analyzer-internal)")
        sat_pri = [s.get("primary", {}).get("wva_saturation_utilization") for s in wva_full]
        ax.plot(x_wva, sat_pri, color=PRIMARY_COLOR, label=PRIM, linewidth=2)
        if has_v2:
            sat_v2 = [s.get("v2", {}).get("wva_saturation_utilization") for s in wva_full]
            ax.plot(x_wva, sat_v2, color=V2_COLOR, label=SEC, linewidth=2)
        # Reference lines from the saturation config: 0.85 scale-up, 0.70 scale-down
        ax.axhline(0.85, color="black", linestyle=":", linewidth=0.8, alpha=0.6)
        ax.axhline(0.70, color="black", linestyle=":", linewidth=0.8, alpha=0.6)
        ax.text(x_wva[-1] if x_wva else 0, 0.85, " 0.85 scaleUp",
                fontsize=7, va="center")
        ax.text(x_wva[-1] if x_wva else 0, 0.70, " 0.70 scaleDown",
                fontsize=7, va="center")
        ax.set_ylabel("utilization")
        ax.legend(loc="upper left", fontsize=7)
        ax.grid(alpha=0.3)
        ax.set_ylim(bottom=0)

        # Panel 8: per-variant tokens used vs capacity (analyzer view).
        # Solid = wva_kv_cache_tokens_used, dashed = wva_kv_cache_tokens_capacity.
        ax = axes[6 + base + (1 if has_rates else 0) - epp_shift]
        ax.set_title("WVA KV Tokens In Use vs Capacity  (per variant)")
        used_pri = [s.get("primary", {}).get("wva_kv_cache_tokens_used") for s in wva_full]
        cap_pri  = [s.get("primary", {}).get("wva_kv_cache_tokens_capacity") for s in wva_full]
        ax.plot(x_wva, used_pri, color=PRIMARY_COLOR, label=f"{PRIM} used",     linewidth=2)
        ax.plot(x_wva, cap_pri,  color=PRIMARY_COLOR, label=f"{PRIM} capacity",
                linewidth=1.2, linestyle="--", alpha=0.7)
        if has_v2:
            used_v2 = [s.get("v2", {}).get("wva_kv_cache_tokens_used") for s in wva_full]
            cap_v2  = [s.get("v2", {}).get("wva_kv_cache_tokens_capacity") for s in wva_full]
            ax.plot(x_wva, used_v2, color=V2_COLOR, label=f"{SEC} used", linewidth=2)
            ax.plot(x_wva, cap_v2,  color=V2_COLOR, label=f"{SEC} capacity",
                    linewidth=1.2, linestyle="--", alpha=0.7)
        ax.set_ylabel("tokens")
        ax.legend(loc="upper left", fontsize=7, ncol=2)
        ax.grid(alpha=0.3)
        ax.ticklabel_format(axis="y", style="sci", scilimits=(0, 0))

    # Bound the x-axis to the active window (load + scale-down), clipping the
    # dead/zero tail after collection so the load isn't squished into the left.
    act = [r["timestamp"] for r in cd_est] if cd_est else []
    if repls:
        act += [t[0] for t in repls if (t[1] or 0) > 1 or (t[2] or 0) > 1]
    # When the full experiment schedule is known, show it in its entirety --
    # otherwise the stage-boundary markers below can fall outside the
    # "active" window above (e.g. a stage that never pushed replicas past 1)
    # or bunch up against its edge.
    exp_start = exp_meta.get("harness_start_epoch")
    exp_stages = exp_meta.get("stages") or []
    if exp_start is not None and exp_stages and all(d for _, d in exp_stages):
        act += [exp_start, exp_start + sum(d for _, d in exp_stages)]
    if act:
        lo, hi = min(act), max(act)
        span = max(hi - lo, 60)
        axes[-1].set_xlim(to_dt(lo - span * 0.03), to_dt(hi + span * 0.05))

    axes[-1].set_xlabel("Time (UTC)")
    axes[-1].xaxis.set_major_formatter(mdates.DateFormatter("%H:%M", tz=timezone.utc))

    # "cost-aware" used to be printed unconditionally, which was simply wrong
    # for a run where nothing pointed a ScaledObject at WVA at all. wva_targets
    # is derived (dump_wva_target_timeseries.py) from actual per-variant WVA
    # decisions in the controller log, so its absence is a real signal that
    # WVA never drove this run -- not just that its dump step was skipped.
    scaling_mode = "cost-aware" if wva_targets else "KEDA well-lit path (no WVA)"
    title_lines = [
        f"Two-Variant V2 — FULL PIPELINE {title_suffix}",
        scaling_mode,
    ]
    wl_line = _format_workload_line(exp_meta)
    if wl_line:
        title_lines.append(wl_line)
    # _load_keda_policy queries the LIVE cluster, not a per-run snapshot, so
    # it is only trustworthy right after a run -- once the ScaledObject that
    # drove it is gone (e.g. a temporary KEDA-only arm, restored to WVA
    # afterward), it silently shows whatever is live NOW mislabeled as this
    # run's behavior. Gate it on the same wva_targets signal as the title's
    # scaling_mode: no WVA decisions in THIS run's log means we cannot vouch
    # for the live ScaledObject still being the one that drove it either.
    keda_line = _format_keda_line(keda_behavior) if wva_targets else None
    if keda_line:
        title_lines.append(keda_line)
    fig.suptitle("\n".join(title_lines), fontsize=9)
    fig.tight_layout(rect=[0, 0, 1, 0.94 if len(title_lines) > 2 else 0.97])

    # Free a thin strip above panel 0 for the stage-rate labels: shrink its
    # axes slightly (lifting its top frame down) rather than guessing at a
    # pad/position that has to clear both this panel's own (long, with the
    # solid/dashed legend suffix) title text AND the label -- tight_layout
    # already gave the title just enough room for itself, not for anything
    # above it too. Only panel 0 moves; every other panel's position from
    # tight_layout is untouched. The label sits near the TOP of the freed
    # strip in absolute figure coordinates (not axes-fraction), so its
    # position doesn't depend on exactly how tall the title renders --
    # the title (pinned to the new, lower axes edge by a small pad) sits
    # near the strip's bottom, with clear separation from the label above.
    stage_boundaries = _stage_boundaries(exp_meta)
    label_y = None
    if stage_boundaries:
        pos = axes[0].get_position()
        # Proportional, not a fixed figure-fraction: a fixed amount ate a much
        # bigger share of panel 0 whenever there were more panels overall (the
        # optional supply/demand/rates/wva-full panels shrink every panel's
        # average height, but a fixed absolute shrink didn't shrink with it),
        # making panel 0 visibly shorter than the rest and forcing its legend
        # to overlap the plotted lines.
        shrink = pos.height * 0.15
        label_y = pos.y0 + pos.height - 0.008
        axes[0].set_position([pos.x0, pos.y0, pos.width, pos.height - shrink])

    # Vertical markers at each load-schedule transition (e.g. RPS 4 -> 14 ->
    # 4), drawn on every panel since they all share the x-axis. The new rate
    # is labeled once, in the strip just freed above panel 0, to avoid
    # repeating it 8x.
    if stage_boundaries:
        trans = blended_transform_factory(axes[0].transData, fig.transFigure)
        for b_ts, b_label in stage_boundaries:
            b_dt = to_dt(b_ts)
            for ax in axes:
                ax.axvline(b_dt, color="gray", linestyle=":", linewidth=1.2, alpha=0.7, zorder=0)
            axes[0].text(b_dt, label_y, b_label, transform=trans,
                         fontsize=7, ha="center", va="top", color="gray")

    fig.savefig(out_path, dpi=120)
    print(f"Wrote {out_path}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir", help="Path to .../results/<treatment>_<i>")
    ap.add_argument("--name", default="two_variant_v2_full_pipeline.png")
    ap.add_argument("--suffix", default="")
    args = ap.parse_args()
    rd = Path(args.results_dir).resolve()
    out = rd / "metrics" / "graphs" / args.name
    out.parent.mkdir(parents=True, exist_ok=True)
    plot(rd, out, args.suffix)


if __name__ == "__main__":
    main()
