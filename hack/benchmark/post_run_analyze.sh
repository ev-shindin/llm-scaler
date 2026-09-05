#!/usr/bin/env bash
# Post-run analyzer for two-variant WVA benchmarks.
# Wraps the steps that should always run after `make benchmark-run`:
#   1. dump WVA controller decisions + V2 saturation analysis numbers from logs
#      (must run while the controller pod's log buffer still covers the run
#       window — kubectl rotates, so do this promptly after the benchmark)
#   2. compute capacity & 3-component demand estimate from raw vLLM/EPP scrapes
#   3. request-rate series from EPP counters
#   4. WVA Prometheus metrics from raw scrapes
#   5. render the pipeline plot
#   6. per-cycle k1/k2 priority decisions from live controller logs (same
#      log-rotation window constraint as step 1 -- run promptly)
#   7. WVA-vs-arm comparison table (TTFT/TPOT/GPU-time)
#
# Usage:
#   ./hack/benchmark/post_run_analyze.sh <results_dir> [namespace] [suffix]
#
# Where:
#   <results_dir> is e.g. <user>-20260531-130812-164/results/guidellm-1780222131-3ew5uw_1
#   [namespace]   defaults to $BENCHMARK_NAMESPACE; required if that is unset
#   [suffix]      optional title suffix for the plot
set -euo pipefail
# --help prints this file's header comment -- the documentation the script
# already carries, so it cannot drift from what the script does. Placed before
# any argument handling because several of these take a namespace as $1, and
# without it `--help` was consumed as one.
case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//; $d'
        exit 0
        ;;
esac


RESULTS_DIR="${1:?usage: $0 <results_dir> [namespace] [suffix]}"
# No fallback namespace. This used to default to a person's namespace, so a run
# with BENCHMARK_NAMESPACE unset scraped logs from a namespace that does not
# exist, the optional steps swallowed the failure, and the result was an empty
# analysis reported as a success. Every benchmark make target requires this
# variable and says so; this now matches them.
NS="${2:-${BENCHMARK_NAMESPACE:-}}"
if [ -z "$NS" ]; then
    echo "ERROR: no namespace. Pass it as the second argument, or set BENCHMARK_NAMESPACE=<namespace>." >&2
    exit 1
fi
SUFFIX="${3:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Steps 1, 3 and 4 are best-effort by design -- a missing controller log or an
# older collect_metrics.sh should not cost you the rest of the analysis. What
# they must not do is disappear: every failure is recorded and named at the end,
# because the previous version swallowed them and then printed "Done" over a list
# of files that were never written.
FAILED=""

echo "[1/5] dump_wva_target_timeseries.py (decisions + V2 analyzer numbers)"
python3 "$SCRIPT_DIR/dump_wva_target_timeseries.py" "$RESULTS_DIR" -n "$NS" || {
    FAILED="${FAILED}  [1] dump_wva_target_timeseries.py — no WVA decisions or analyzer numbers. The controller's log buffer may no longer cover the run window (kubectl rotates it), or namespace '$NS' is wrong."$'\n'
}

echo "[2/5] dump_capacity_demand_estimate.py (raw scrape estimate)"
python3 "$SCRIPT_DIR/dump_capacity_demand_estimate.py" "$RESULTS_DIR"

echo "[3/5] dump_epp_throughput.py (request rate from EPP counters)"
python3 "$SCRIPT_DIR/dump_epp_throughput.py" "$RESULTS_DIR" || {
    FAILED="${FAILED}  [3] dump_epp_throughput.py — no request-rate series."$'\n'
}

echo "[4/5] dump_wva_full_timeseries.py (WVA Prometheus metrics — empty if collect_metrics.sh predates the WVA scrape patch)"
python3 "$SCRIPT_DIR/dump_wva_full_timeseries.py" "$RESULTS_DIR" || {
    FAILED="${FAILED}  [4] dump_wva_full_timeseries.py — no WVA Prometheus series."$'\n'
}

echo "[5/7] plot_two_variant_pipeline.py"
if [ -n "$SUFFIX" ]; then
    python3 "$SCRIPT_DIR/plot_two_variant_pipeline.py" "$RESULTS_DIR" --suffix "$SUFFIX"
else
    python3 "$SCRIPT_DIR/plot_two_variant_pipeline.py" "$RESULTS_DIR"
fi

echo "[6/7] dump_k2_decisions.py (per-cycle k1/k2 priority decisions)"
python3 "$SCRIPT_DIR/dump_k2_decisions.py" "$RESULTS_DIR" -n "$NS" || {
    FAILED="${FAILED}  [6] dump_k2_decisions.py — no k2-decision report. Needs run_metadata.yaml in the results dir and the controller's log buffer to still cover the run window."$'\n'
}

echo "[7/7] comparison_table.py (WVA-vs-arm TTFT/TPOT/GPU-time table)"
mkdir -p "$RESULTS_DIR/metrics/reports"
python3 "$SCRIPT_DIR/comparison_table.py" "$RESULTS_DIR" > "$RESULTS_DIR/metrics/reports/comparison_table.md" || {
    # A file the failed command still created (redirection truncates/creates it
    # before the command runs) is not a report -- remove it rather than let the
    # Outputs list below claim an empty file is one.
    rm -f "$RESULTS_DIR/metrics/reports/comparison_table.md"
    FAILED="${FAILED}  [7] comparison_table.py — no comparison table."$'\n'
}

echo ""
if [ -n "$FAILED" ]; then
    echo "Finished, but some steps produced nothing:"
    printf '%s' "$FAILED"
    echo ""
fi

# Only files that exist are listed. A path printed under "Outputs" is a claim
# that it is there.
echo "Outputs:"
found_any=false
for f in "$RESULTS_DIR/metrics/processed/wva_target_timeseries.json" \
         "$RESULTS_DIR/metrics/processed/capacity_demand_estimate.json" \
         "$RESULTS_DIR/metrics/processed/epp_throughput.json" \
         "$RESULTS_DIR/metrics/processed/wva_metrics_timeseries.json" \
         "$RESULTS_DIR/metrics/graphs/two_variant_v2_full_pipeline.png" \
         "$RESULTS_DIR/metrics/processed/k2_decisions.json" \
         "$RESULTS_DIR/metrics/reports/k2_decision_report.md" \
         "$RESULTS_DIR/metrics/reports/comparison_table.md"; do
    if [ -e "$f" ]; then
        echo "  $f"
        found_any=true
    else
        echo "  (not written) $f"
    fi
done
[ "$found_any" = true ] || exit 1
