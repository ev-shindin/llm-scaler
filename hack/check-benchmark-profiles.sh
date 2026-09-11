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
# benchmark-run reads BENCHMARK_SCENARIOS_DIR, which is overridable. Gating a
# file from a different directory than the one that gets copied would be worse
# than not gating at all.
DIR="${BENCHMARK_SCENARIOS_DIR:-$ROOT/test/benchmark/scenarios}"

# profile_harness <file> -- guidellm, inference-perf, or unknown.
#
# Both keys are required for guidellm rather than just `spec:`, so a
# half-converted file comes back unknown instead of passing on one line.
profile_harness() {
    local f="$1"
    # BOTH keys per schema, deliberately. A half-converted file -- `spec:` with no
    # `backend:` -- must come back unknown rather than be classified on one line
    # and then rejected by the harness five minutes later. Asserted below against
    # a fixture, because dropping either key from this function is invisible to a
    # count of how many profiles classified.
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
    # Every form benchmark-run will accept, in the directory it will look in.
    # BENCHMARK_SCENARIOS_DIR is overridable there, so honour it here or the gate
    # judges a different file from the one that gets copied.
    f=""
    for cand in "${workload}.yaml.in" "${workload}.in" "${workload}"; do
        if [ -f "$DIR/$cand" ]; then f="$DIR/$cand"; break; fi
    done
    # Not one of ours. The harness ships its own profiles and benchmark-run
    # fetches others from the inference-perf catalog; nothing to say about those.
    # It also swallows a typo, so say which it was -- silently exiting 0 on
    # `burstyy` looks identical to a correct pairing.
    if [ -z "$f" ]; then
        echo "note: '$workload' is not in $DIR; leaving it to the harness (a local profile of that name would be gated here)." >&2
        exit 0
    fi
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
# Only against THIS repo's own profile set. An overridden directory legitimately
# holds one profile, or none of one kind, and the floor is a statement about what
# this repo ships -- not about the classifier.
if [ -z "${BENCHMARK_SCENARIOS_DIR:-}" ] && { [ "$g" -lt 3 ] || [ "$i" -lt 3 ]; }; then
    echo "FAIL classified $g guidellm and $i inference-perf profiles; both were well above 3."
    echo "     A collapse here means the classifier stopped recognising a schema, which would"
    echo "     make the pre-run gate reject correct pairings."
    FAIL=1
fi

# The counts above say the classifier still answers. They do NOT say it answers
# CORRECTLY -- swapping the two labels inside profile_harness leaves both counts
# healthy (17 guidellm, 5 inference-perf reads as fine) while inverting the gate,
# so every correct pairing is refused and every mismatch waved through. Two
# profiles are therefore pinned by name, one of each schema.
if [ -n "${BENCHMARK_SCENARIOS_DIR:-}" ]; then
    # The pinned names below are the ones THIS repo ships. Against an overridden
    # directory they are simply absent, and asserting on them produced
    # "flat_8k1000_10rps_12m classifies as 'unknown', the classifier is wrong" --
    # a confident accusation about a file that was never there. The gate above
    # still honours the override; only the pinned-name and gate-direction blocks
    # are skipped.
    echo "note: BENCHMARK_SCENARIOS_DIR is set, so the checks pinned to this repo's own profiles are skipped."
    echo "benchmark profiles OK ($g guidellm, $i inference-perf, in $DIR)"
    exit "$FAIL"
fi

for pair in "prefill_heavy guidellm" "symmetrical guidellm" "bursty inference-perf" "flat_8k1000_10rps_12m inference-perf"; do
    set -- $pair
    got="$(profile_harness "$DIR/$1.yaml.in")"
    if [ "$got" != "$2" ]; then
        echo "FAIL $1 classifies as '$got', but it is a $2 profile."
        echo "     The classifier is wrong, not merely quiet: the pre-run gate would refuse"
        echo "     correct pairings and accept the mismatch it exists to catch."
        FAIL=1
    fi
done

# And the gate itself, in both directions, because lint mode never enters that
# branch -- the Makefile comment claiming this check proves the gate works was
# only true once these ran it.
# A HALF-CONVERTED profile. Dropping either of the two required keys from
# profile_harness leaves every count and every pinned name unchanged, so nothing
# above notices -- but the gate then accepts a file the harness will reject,
# which is the whole failure. Built here rather than shipped, so the repo does
# not carry a broken profile.
HALF="$(mktemp -d)"
trap 'rm -rf "$HALF"' EXIT
printf '%s
' 'metadata:' '  labels:' '    name: half' 'spec:' '  profile:' '    kind: constant'     > "$HALF/half-converted.yaml.in"
if BENCHMARK_SCENARIOS_DIR="$HALF" "$0" guidellm half-converted >/dev/null 2>&1; then
    echo "FAIL the gate accepts a half-converted profile (spec: with no backend:)."
    echo "     guidellm rejects it on sight, so the run would send nothing and still report a table."
    FAIL=1
fi

if "$0" guidellm prefill_heavy >/dev/null 2>&1; then
    : # correct pairing accepted
else
    echo "FAIL the gate refuses guidellm + prefill_heavy, which is a correct pairing"
    FAIL=1
fi
if "$0" guidellm bursty >/dev/null 2>&1; then
    echo "FAIL the gate accepts guidellm + bursty, an inference-perf profile: the five-minute"
    echo "     all-'?' run this check exists to prevent would go ahead."
    FAIL=1
fi
if "$0" inference-perf bursty >/dev/null 2>&1; then
    : # correct pairing accepted
else
    echo "FAIL the gate refuses inference-perf + bursty, which is a correct pairing"
    FAIL=1
fi

[ "$FAIL" -ne 0 ] && { echo "benchmark profile check FAILED"; exit 1; }
echo "benchmark profiles OK ($g guidellm, $i inference-perf)"
