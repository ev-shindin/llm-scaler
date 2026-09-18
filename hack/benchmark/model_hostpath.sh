#!/usr/bin/env bash
# Turn on node-local weights in a benchmark scenario copy.
#
#   model_hostpath.sh <scenario.yaml> <node dir> <namespace> [KEY=VALUE] [TAINTKEY,...]
#
# The harness (llm-d-benchmark) has the mechanism -- storage.hostPath in a
# scenario makes it create a static hostPath PersistentVolume, bind model-pvc
# to it, and run a DaemonSet that downloads the model onto every selected
# node before the engines deploy (the download Job is skipped). It is off in
# every scenario here because it needs leave to create PersistentVolumes,
# and because the node directory is the cluster's, not the harness's. This
# turns it on in the COPY the standup made, with the directory the caller
# named and the placement the rest of the repo uses for accelerator nodes:
# a KEY=VALUE nodeSelector when given, otherwise the affinity over the
# known GPU product labels (deploy/lib/accelerator_nodes.sh), which the
# harness's template renders once hack/benchmark/patch_harness.sh fix 11 is
# applied -- the same fix that makes the harness's "DaemonSet ready" wait
# mean "download complete" rather than "container started".
#
# Refused when the namespace already has a model-pvc on another storage
# class: the harness keeps an existing claim, so the volume it would create
# would never bind, the DaemonSet would download onto the node, and the
# engines would go on reading the shared volume -- a standup that looks
# like it worked. Delete the claim (the weights on the shared volume go
# with it) or use a fresh namespace.
#
# Edits with yq, like the standup's other scenario edits, so the file keeps
# its comments; nothing else reads the storage block after this.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../../deploy/lib/accelerator_nodes.sh
source "$HERE/../../deploy/lib/accelerator_nodes.sh"
# shellcheck source=../../deploy/lib/nodedir.sh
source "$HERE/../../deploy/lib/nodedir.sh"
# the lib's checks call log_error; here that is a plain failure
log_error() { printf 'model_hostpath: %s\n' "$1" >&2; exit 1; }

SCENARIO="${1:?usage: model_hostpath.sh <scenario.yaml> <node dir> <namespace> [KEY=VALUE] [TAINTKEY,...]}"
NODE_DIR="${2:?usage: model_hostpath.sh <scenario.yaml> <node dir> <namespace> [KEY=VALUE] [TAINTKEY,...]}"
NAMESPACE="${3:?usage: model_hostpath.sh <scenario.yaml> <node dir> <namespace> [KEY=VALUE] [TAINTKEY,...]}"
SELECTOR="${4:-}"
TOLERATIONS="${5:-}"   # comma list of taint keys beyond nvidia.com/gpu, as PREPULL_TOLERATIONS
STORAGE_CLASS="node-local-weights"

[ -f "$SCENARIO" ] || log_error "no scenario at ${SCENARIO}"
command -v yq >/dev/null 2>&1 || log_error "yq is required (the standup edits the scenario with it)"
why="$(nodedir_ok "$NODE_DIR")" || log_error "the node directory: ${why}"
accelerator_check_selector "$SELECTOR"
tolerations_json='[{"key":"nvidia.com/gpu","operator":"Exists"}]'
if [ -n "$TOLERATIONS" ]; then
    IFS=',' read -r -a taint_keys <<< "$TOLERATIONS"
    for t in "${taint_keys[@]}"; do
        accelerator_check_toleration "$t"
        tolerations_json="$(printf '%s' "$tolerations_json" | jq -c --arg k "$t" '. + [{"key": $k, "operator": "Exists"}]')"
    done
fi

# An existing claim on another class is the silent failure described above.
existing="$(kubectl get pvc -n "$NAMESPACE" model-pvc -o jsonpath='{.spec.storageClassName}' 2>/dev/null || true)"
if [ -n "$existing" ] && [ "$existing" != "$STORAGE_CLASS" ]; then
    log_error "namespace ${NAMESPACE} already has model-pvc on storage class '${existing}'; the harness keeps an existing claim, so node-local weights would not take. Delete the claim (kubectl delete pvc -n ${NAMESPACE} model-pvc; the shared copy of the weights goes with it) or use a fresh namespace"
fi

# The volume's capacity is the claim's request: a claim larger than its
# volume never binds.
size="$(yq -r '[.. | select(type == "!!map" and has("modelPvc")) | .modelPvc.size] | .[0] // ""' "$SCENARIO")"
[ -n "$size" ] || log_error "no storage.modelPvc.size in ${SCENARIO}; the scenario declares no model claim to bind"

placement_json='{}'
if [ -n "$SELECTOR" ]; then
    placement_json="{\"nodeSelector\":{\"${SELECTOR%%=*}\":\"${SELECTOR#*=}\"}}"
else
    # the one renderer of the default placement, read as JSON
    placement_json="$(accelerator_placement_yaml "" | sed 's/^      //' | yq -o json '.')"
fi
hp="$(mktemp)"
trap 'rm -f "$hp"' EXIT
printf '{"enabled":true,"path":"%s","storageClassName":"%s","capacity":"%s","daemonSetTimeout":3600}' \
    "$NODE_DIR" "$STORAGE_CLASS" "$size" \
    | jq --argjson p "$placement_json" --argjson t "$tolerations_json" '. + $p + {tolerations: $t}' > "$hp"

# every storage block that declares a model claim, wherever the scenario keeps it
HP_FILE="$hp" yq -i '(.. | select(type == "!!map" and has("modelPvc"))).hostPath = load(strenv(HP_FILE))' "$SCENARIO"

n="$(yq -r '[.. | select(type == "!!map" and has("hostPath")) | .hostPath.enabled] | map(select(. == true)) | length' "$SCENARIO")"
[ "$n" -gt 0 ] || log_error "the hostPath block did not take in ${SCENARIO}"
if [ -n "$SELECTOR" ]; then where="nodes with ${SELECTOR}"; else where="every node carrying a known GPU product label"; fi
echo "node-local weights: model-pvc binds to ${NODE_DIR} on ${where}; the harness downloads the model there on each node before the engines start (${n} storage block(s))"
