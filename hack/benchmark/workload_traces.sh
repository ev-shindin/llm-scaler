#!/usr/bin/env bash
# Put the replay traces a workload profile references beside it in the
# harness, and nothing else.
#
#   workload_traces.sh <profile> <scenarios dir> <harness profiles dir> <harness>
#
# A replay profile (GuideLLM `profile: kind: replay`) schedules every request
# from a pre-generated trace it names by an in-pod path:
#
#     data:
#       - kind: trace_synthetic
#         path: /workspace/profiles/guidellm/shape_6k1000_1k4000_rps6_20m.jsonl
#
# The harness bundles EVERY file in workload/profiles/<harness>/ that is not
# a *.yaml.in into the <harness>-profiles ConfigMap and mounts it there. The
# traces live in test/benchmark/scenarios beside their profiles and their
# params files (hack/benchmark/gen_shape_trace.py makes one from the other),
# but benchmark-run copied only the profile, so a run from a fresh clone
# failed inside the harness pod, five minutes in, on "Trace file not found"
# -- and the run still produced a report. This copies what the profile
# names, refuses when it is not there, removes traces of ours the profile
# does not name (a ConfigMap holds 1 MiB in all, and one trace is 900 KB),
# and refuses when what would be bundled exceeds that.
set -euo pipefail

PROFILE="${1:?usage: workload_traces.sh <profile> <scenarios dir> <harness profiles dir> <harness>}"
SCENARIOS="${2:?usage: workload_traces.sh <profile> <scenarios dir> <harness profiles dir> <harness>}"
DEST="${3:?usage: workload_traces.sh <profile> <scenarios dir> <harness profiles dir> <harness>}"
HARNESS="${4:?usage: workload_traces.sh <profile> <scenarios dir> <harness profiles dir> <harness>}"
CONFIGMAP_CAP=1048576

[ -f "$PROFILE" ] || { echo "workload_traces: no profile at ${PROFILE}" >&2; exit 1; }
[ -d "$DEST" ] || { echo "workload_traces: no harness profiles directory at ${DEST}" >&2; exit 1; }

# the trace files the profile names under the mount the harness serves
referenced=()
while IFS= read -r name; do
    [ -n "$name" ] && referenced+=("$name")
done < <(sed -n "s#^[[:space:]]*path:[[:space:]]*/workspace/profiles/${HARNESS}/\([A-Za-z0-9._-]*\)[[:space:]]*\$#\1#p" "$PROFILE")

for name in ${referenced[@]+"${referenced[@]}"}; do
    if [ ! -f "$SCENARIOS/$name" ]; then
        params="$SCENARIOS/${name%.jsonl}.params.yaml"
        echo "workload_traces: $(basename "$PROFILE") replays ${name}, which is not in ${SCENARIOS}." >&2
        if [ -f "$params" ]; then
            echo "  Generate it from its params file and commit both:" >&2
            echo "      python3 hack/benchmark/gen_shape_trace.py ${params} --out ${SCENARIOS}/${name}" >&2
        else
            echo "  There is no ${name%.jsonl}.params.yaml beside it either; see hack/benchmark/gen_shape_trace.py." >&2
        fi
        exit 1
    fi
    cp "$SCENARIOS/$name" "$DEST/$name"
    echo "Copying replay trace ${name} ($(wc -c < "$SCENARIOS/$name") bytes) to the ${HARNESS} harness..."
done

# traces of ours that this profile does not name: an earlier run's, and each
# one counts against the ConfigMap
for f in "$DEST"/*.jsonl; do
    [ -f "$f" ] || continue
    name="$(basename "$f")"
    keep=false
    for r in ${referenced[@]+"${referenced[@]}"}; do [ "$r" = "$name" ] && keep=true; done
    if [ "$keep" = false ] && [ -f "$SCENARIOS/$name" ]; then
        rm -f "$f"
        echo "Removed replay trace ${name} from the ${HARNESS} harness (another workload's; the profiles ConfigMap holds 1 MiB in all)."
    fi
done

# what the harness will bundle: every non-template file in the directory
total=0
for f in "$DEST"/*; do
    [ -f "$f" ] || continue
    case "$f" in *.yaml.in) continue ;; esac
    total=$((total + $(wc -c < "$f")))
done
if [ "$total" -gt "$CONFIGMAP_CAP" ]; then
    echo "workload_traces: the ${HARNESS} profiles directory holds ${total} bytes of files the harness bundles into one ConfigMap, over its ${CONFIGMAP_CAP}-byte cap; the create would fail. Files:" >&2
    for f in "$DEST"/*; do [ -f "$f" ] || continue; case "$f" in *.yaml.in) continue ;; esac; printf '  %10d %s\n' "$(wc -c < "$f")" "$(basename "$f")" >&2; done
    exit 1
fi
