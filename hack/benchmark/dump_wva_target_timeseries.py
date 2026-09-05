#!/usr/bin/env python3
"""Extract WVA controller decisions and V2 saturation analysis numbers for a
given results dir's run window. Output:

  metrics/processed/wva_target_timeseries.json

Captured per reconcile timestamp:
  - per-variant `target` (from "Applied saturation decision via shared cache")
  - model-level totalSupply / totalDemand / utilization / requiredCapacity /
    spareCapacity (from "V2 saturation analysis completed")

Both lines fire at the same reconcile, so we group by integer timestamp.

Primary source is the controller's own logs. They're the richest (they're
where totalSupply comes from -- Thanos has no direct equivalent metric) but
ephemeral: a container's log buffer rotates away on any run longer than a
few minutes, especially once k1/k2 decision logging is also active on the
same controller. When the logs come up empty, this falls back to querying
Thanos directly for wva_desired_replicas / wva_analyzer_demand /
wva_required_capacity / wva_spare_capacity / wva_saturation_utilization,
which Prometheus retains far longer than any pod's log file survives.
Everything except totalSupply is recoverable that way.

Usage
-----
  python hack/benchmark/dump_wva_target_timeseries.py \
      <results>/<treatment>_<i> -n NAMESPACE
"""
import argparse
import json
import re
import socket
import ssl
import subprocess
import sys
import time
import urllib.parse
import urllib.request
from contextlib import closing
from datetime import datetime, timezone
from pathlib import Path

try:
    import yaml
except ImportError:
    print("ERROR: PyYAML required. pip install pyyaml", file=sys.stderr)
    sys.exit(1)


# The caller field is matched loosely on the FILE name only. Pinning the package
# path is what silently broke this: the decision log moved from saturation/ to
# steadystate/, the pattern stopped matching, and the dump went to zero samples
# with no error — and because post_run_analyze.sh then swallowed the failure, two
# plot panels just disappeared. That script now names a step that produced
# nothing, but a zero-sample dump that still exits 0 stays invisible to it, so
# this pattern remains the place the breakage has to be prevented.
DECISION_PAT = re.compile(
    r'^(?P<ts>\S+)	\S+	\S*engine\.go:\d+	'
    r'Applied saturation decision via shared cache	'
    r'(?P<json>\{.*\})$'
)
# The V2 analyzer emits "analyzer-result"; there has never been a
# "V2 saturation analysis completed" line in the tree, so this matched nothing.
ANALYSIS_PAT = re.compile(
    r'^(?P<ts>\S+)	\S+	\S*engine_v2\.go:\d+	'
    r'analyzer-result	'
    r'(?P<json>\{.*\})$'
)

# analyzer-result uses short keys; the plots use the long ones.
ANALYSIS_KEYS = {
    "supply": "totalSupply",
    "demand": "totalDemand",
    "util": "utilization",
    "rc": "requiredCapacity",
    "sc": "spareCapacity",
}


def _free_local_port():
    with closing(socket.socket(socket.AF_INET, socket.SOCK_STREAM)) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _thanos_query_range(promql, start_ts, end_ts, step=15):
    """Range-query Thanos for one PromQL expression, via a temporary kubectl
    port-forward to thanos-querier. Returns the raw `result` list from the
    API response (each item has `metric` labels and a `values` list of
    [ts, value] pairs), or [] on any failure -- no cluster access, no oc
    token, a query error. This is a best-effort fallback, not a hard
    dependency: the caller treats an empty result exactly like "no data
    available", not an error.
    """
    port = _free_local_port()
    pf = subprocess.Popen(
        ["kubectl", "port-forward", "-n", "openshift-monitoring",
         "svc/thanos-querier", f"{port}:9091"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    try:
        ready = False
        for _ in range(20):
            time.sleep(0.25)
            with closing(socket.socket(socket.AF_INET, socket.SOCK_STREAM)) as s:
                if s.connect_ex(("127.0.0.1", port)) == 0:
                    ready = True
                    break
        if not ready:
            return []

        token = subprocess.run(
            ["oc", "whoami", "-t"], capture_output=True, text=True,
        ).stdout.strip()
        if not token:
            return []

        params = urllib.parse.urlencode({
            "query": promql, "start": start_ts, "end": end_ts, "step": step,
        })
        req = urllib.request.Request(
            f"https://127.0.0.1:{port}/api/v1/query_range?{params}",
            headers={"Authorization": f"Bearer {token}"},
        )
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        with urllib.request.urlopen(req, timeout=15, context=ctx) as resp:
            data = json.loads(resp.read())
        if data.get("status") != "success":
            return []
        return data.get("data", {}).get("result", [])
    except Exception:
        return []
    finally:
        pf.terminate()
        try:
            pf.wait(timeout=5)
        except subprocess.TimeoutExpired:
            pf.kill()


def _thanos_fallback(namespace, model_id, start, stop):
    """Reconstruct the same per-timestamp samples the log parser produces,
    from Thanos-retained metrics instead of the (likely rotated-away)
    controller logs. totalSupply has no direct Thanos equivalent and stays
    None; everything else -- replica targets, demand, utilization, required
    and spare capacity -- does.
    """
    start_ts, end_ts = int(start.timestamp()), int(stop.timestamp())
    by_ts = {}

    def bucket(ts):
        return by_ts.setdefault(int(float(ts)), {})

    for series in _thanos_query_range(
        f'wva_desired_replicas{{exported_namespace="{namespace}"}}', start_ts, end_ts,
    ):
        variant = series.get("metric", {}).get("variant_name", "")
        # "prefill" buckets as "v2": a P/D run has no cost-tier sibling, so
        # its second role is deliberately mapped onto the same slot rather
        # than adding a real third variant kind. Legend/labels downstream
        # still read "primary"/"v2"; only the underlying data differs.
        tag = "v2" if variant.endswith("-v2") or "prefill" in variant else "primary"
        for ts, v in series.get("values", []):
            bucket(ts)[tag] = int(float(v))

    if model_id:
        for series in _thanos_query_range(
            f'sum(wva_analyzer_demand{{exported_namespace="{namespace}", '
            f'model_name="{model_id}"}}) by (model_name)', start_ts, end_ts,
        ):
            for ts, v in series.get("values", []):
                bucket(ts)["totalDemand"] = float(v)

    for metric, key in (
        ("wva_required_capacity", "requiredCapacity"),
        ("wva_spare_capacity", "spareCapacity"),
        ("wva_saturation_utilization", "utilization"),
    ):
        for series in _thanos_query_range(
            f'{metric}{{exported_namespace="{namespace}"}}', start_ts, end_ts,
        ):
            for ts, v in series.get("values", []):
                b = bucket(ts)
                b[key] = b.get(key, 0.0) + float(v)

    samples = []
    for ts, b in sorted(by_ts.items()):
        samples.append({
            "timestamp": ts,
            "primary":          b.get("primary"),
            "v2":               b.get("v2"),
            "totalSupply":      None,
            "totalDemand":      b.get("totalDemand"),
            "utilization":      b.get("utilization"),
            "requiredCapacity": b.get("requiredCapacity"),
            "spareCapacity":    b.get("spareCapacity"),
        })
    return samples


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir", help="Path to .../results/<treatment>_<i>")
    ap.add_argument("-n", "--namespace", required=True)
    args = ap.parse_args()

    rd = Path(args.results_dir).resolve()
    meta_path = rd / "run_metadata.yaml"
    if not meta_path.is_file():
        print(f"ERROR: run_metadata.yaml not found in {rd}", file=sys.stderr)
        sys.exit(1)
    meta = yaml.safe_load(meta_path.read_text())

    def parse_iso(s):
        return datetime.fromisoformat(s.replace("Z", "+00:00"))

    start = parse_iso(meta["harness_start"])
    stop = parse_iso(meta["harness_stop"])

    # Prefer a captured tail file (written by `make benchmark-run` via
    # tail_wva_logs.sh from the moment the run started) over a live
    # `kubectl logs` call: the controller's own log buffer is bounded by
    # kubelet's fixed per-container rotation size, not by run length, so
    # anything past the first few minutes is commonly gone by the time this
    # runs after the fact. The captured file has no such limit.
    captured = rd / "wva_controller.log"
    if captured.is_file() and captured.read_text().strip():
        print(f"Using captured log tail: {captured}", file=sys.stderr)
        logs = captured.read_text()
    else:
        # Pull WVA logs covering the run window. We query "since" relative to
        # now plus a small buffer to ensure we capture the harness-start tick.
        now = datetime.now(timezone.utc)
        since_seconds = int((now - start).total_seconds()) + 90
        logs = subprocess.run(
            ["kubectl", "logs", "-n", args.namespace,
             "-l", "app.kubernetes.io/name=workload-variant-autoscaler",
             f"--since={since_seconds}s", "--tail=200000"],
            capture_output=True, text=True,
        ).stdout

    samples_by_ts = {}

    # Bucket reconciles by integer timestamp. Some reconciles fire both
    # "V2 saturation analysis" and per-variant "Applied decision" lines at the
    # same wall-clock second; we want them merged into one sample.
    def bucket(ts_dt):
        return samples_by_ts.setdefault(int(ts_dt.timestamp()), {})

    for line in logs.splitlines():
        m = DECISION_PAT.match(line)
        if m:
            try:
                ts_dt = parse_iso(m.group("ts"))
                if ts_dt < start or ts_dt > stop:
                    continue
                d = json.loads(m.group("json"))
            except (ValueError, json.JSONDecodeError):
                continue
            variant = d.get("variant", "")
            target = d.get("target")
            if target is None:
                continue
            # "prefill" buckets as "v2": a P/D run has no cost-tier sibling, so
            # its second role is deliberately mapped onto the same slot rather
            # than adding a real third variant kind. Legend/labels downstream
            # still read "primary"/"v2"; only the underlying data differs.
            tag = "v2" if variant.endswith("-v2") or "prefill" in variant else "primary"
            bucket(ts_dt)[tag] = int(target)
            continue

        m = ANALYSIS_PAT.match(line)
        if m:
            try:
                ts_dt = parse_iso(m.group("ts"))
                if ts_dt < start or ts_dt > stop:
                    continue
                d = json.loads(m.group("json"))
            except (ValueError, json.JSONDecodeError):
                continue
            b = bucket(ts_dt)
            for short, long in ANALYSIS_KEYS.items():
                if short in d:
                    b[long] = d[short]
                elif long in d:
                    b[long] = d[long]

    samples = []
    for ts, b in sorted(samples_by_ts.items()):
        samples.append({
            "timestamp": ts,
            "primary":         b.get("primary"),
            "v2":              b.get("v2"),
            "totalSupply":     b.get("totalSupply"),
            "totalDemand":     b.get("totalDemand"),
            "utilization":     b.get("utilization"),
            "requiredCapacity": b.get("requiredCapacity"),
            "spareCapacity":   b.get("spareCapacity"),
        })

    source = "controller logs"
    if not samples:
        print("No samples from controller logs (likely rotated past the run "
              "window) — falling back to Thanos...", file=sys.stderr)
        samples = _thanos_fallback(args.namespace, meta.get("model"), start, stop)
        source = "Thanos" if samples else source

    out = rd / "metrics" / "processed" / "wva_target_timeseries.json"
    out.parent.mkdir(parents=True, exist_ok=True)

    # Don't clobber an existing non-empty file with zero new samples from
    # either source — preserve whatever was previously captured.
    if not samples and out.is_file():
        try:
            existing = json.loads(out.read_text()).get("samples", [])
        except (OSError, json.JSONDecodeError):
            existing = []
        if existing:
            print(f"Skipped overwriting {out}: 0 new snapshots from logs or "
                  f"Thanos, existing file has {len(existing)}.")
            return

    out.write_text(json.dumps({"samples": samples}, indent=2))
    print(f"Wrote {out} ({len(samples)} snapshots from {source}, "
          f"window {start.isoformat()} -> {stop.isoformat()})")


if __name__ == "__main__":
    main()
