#!/usr/bin/env bash
#
# Checks that a workload profile in test/benchmark/scenarios matches the harness
# it is about to be run with.
#
#   check-benchmark-profiles.sh                       classify every profile
#   check-benchmark-profiles.sh <harness> <workload>  gate one, before a run
#
# The two harnesses take DIFFERENT, mutually invalid profile schemas:
#
#   guidellm        metadata: / spec: {backend, profile, data, constraints}
#   inference-perf  load: / api: / server: / tokenizer: / data: / report: / storage:
#
# and benchmark-run copies whatever it is given into
# workload/profiles/$(BENCHMARK_HARNESS)/ without looking. BENCHMARK_HARNESS
# defaults to guidellm and most of the profiles here are inference-perf ones, so
# the default combination is a mismatch, and the way it fails is the problem:
#
#     Error: Invalid value for '--tokenizer' / '--data' / '--api' / ... :
#       - Field required (at 'backend.openai_http.target')
#       - Extra inputs are not permitted (at 'load')
#
# guidellm exits 2 having sent NOTHING -- and the run still produces a report:
# every latency cell "?", "Avg replicas 1.00", "Max replicas 1". That is not a
# missing result, it is a wrong one. It reads exactly like a real run of a fleet
# that correctly held at one replica, which is what it was being read as. The
# reason appears only in the harness pod's own log, five minutes and one pod
# after the mistake was made.
#
# So the pairing is checked before anything is stood up.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIR="$ROOT/test/benchmark/scenarios"

# profile_harness <file> -- guidellm, inference-perf, or unknown.
#
# Both keys are required for guidellm rather than just `spec:`, so a
# half-converted file comes back unknown instead of passing on one line.
profile_harness() {
    local f="$1"
    if grep -qE '^spec:' "$f" && grep -qE '^  backend:' "$f"; then
        echo guidellm
    elif grep -qE '^load:' "$f" && grep -qE '^server:' "$f"; then
        echo inference-perf
    else
        echo unknown
    fi
}

if [ $# -eq 2 ]; then
    # Gate mode: silent on success. It runs on every benchmark-run and has
    # nothing to say when the pairing is right.
    harness="$1" workload="$2"
    f="$DIR/${workload}.yaml.in"
    [ -f "$f" ] || f="$DIR/${workload}"
    # Not one of ours. The harness ships its own profiles and benchmark-run
    # fetches others from the inference-perf catalog; nothing to say about those.
    [ -f "$f" ] || exit 0
    got="$(profile_harness "$f")"
    [ "$got" = "$harness" ] && exit 0
    echo "ERROR: workload '$workload' is a ${got} profile, but BENCHMARK_HARNESS=${harness}."
    if [ "$got" = "unknown" ]; then
        echo "  It matches neither schema, so no harness will accept it."
    else
        echo "  ${harness} will reject it outright, exit 2 without sending a single request,"
        echo "  and the run will STILL produce a report -- every latency '?', replicas flat at"
        echo "  whatever the fleet already was, which reads like a real result."
        echo ""
        echo "  Either run it with its own harness:"
        echo "      make benchmark-run BENCHMARK_HARNESS=${got} BENCHMARK_WORKLOAD=${workload} ..."
    fi
    echo ""
    echo "  Profiles for BENCHMARK_HARNESS=${harness}:"
    for p in "$DIR"/*.yaml.in; do
        [ "$(profile_harness "$p")" = "$harness" ] && echo "    $(basename "$p" .yaml.in)"
    done
    exit 1
fi

if [ $# -ne 0 ]; then
    echo "usage: $(basename "$0") [<harness> <workload>]" >&2
    exit 2
fi

# Lint mode: every profile must be classifiable, and both harnesses must still
# have profiles. A count that collapses to zero means the classifier broke, not
# that the profiles went away -- and a classifier that calls everything unknown
# would otherwise turn the gate above into a permanent, uninformative failure.
FAIL=0
g=0 i=0
unknown=()
for p in "$DIR"/*.yaml.in; do
    case "$(profile_harness "$p")" in
        guidellm)       g=$((g + 1)) ;;
        inference-perf) i=$((i + 1)) ;;
        *)              unknown+=("$(basename "$p" .yaml.in)") ;;
    esac
done

if [ ${#unknown[@]} -ne 0 ]; then
    echo "FAIL these profiles match neither harness schema, so no run can use them:"
    printf '       %s\n' "${unknown[@]}"
    echo "     guidellm wants metadata:/spec:{backend,profile,data,constraints};"
    echo "     inference-perf wants load:/api:/server:/tokenizer:/data:."
    FAIL=1
fi
if [ "$g" -lt 3 ] || [ "$i" -lt 3 ]; then
    echo "FAIL classified $g guidellm and $i inference-perf profiles; both were well above 3."
    echo "     A collapse here means the classifier stopped recognising a schema, which would"
    echo "     make the pre-run gate reject correct pairings."
    FAIL=1
fi

[ "$FAIL" -ne 0 ] && { echo "benchmark profile check FAILED"; exit 1; }
echo "benchmark profiles OK ($g guidellm, $i inference-perf)"
