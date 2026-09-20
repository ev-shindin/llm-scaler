#!/usr/bin/env bash
# Point a benchmark scenario copy's engine caches at the node-local claim.
#
#   engine_cache_claim.sh <scenario.yaml> <namespace> [claim]
#
# The scenarios under hack/benchmark/scenarios/guides/ mount the harness's own
# workload PVC (RWX, shared) at /engine-cache as the home of VLLM_CACHE_ROOT,
# FLASHINFER_WORKSPACE_DIR and TRITON_CACHE_DIR. deploy/enginecache.sh puts
# those caches on the node's disk instead, behind a claim named engine-cache;
# this rewrites the scenario COPY the standup made so every `engine-cache`
# volume names that claim, and drops the subPath (the volume's directory is
# the cache directory itself). The env vars and the writability guard in the
# scenario stay as they are: a node the preparer has not reached costs a
# compile, not the replica.
#
# Refused when the namespace has no Bound claim of that name: the harness
# would deploy engines whose cache volume never mounts, and every replica
# would sit Pending -- a standup that fails late and says "ContainerCreating".
#
# Edits with yq, like the standup's other scenario edits, so the file keeps
# its comments.
set -euo pipefail
log_error() { printf 'engine_cache_claim: %s\n' "$1" >&2; exit 1; }
SCENARIO="${1:?usage: engine_cache_claim.sh <scenario.yaml> <namespace> [claim]}"
NAMESPACE="${2:?usage: engine_cache_claim.sh <scenario.yaml> <namespace> [claim]}"
CLAIM="${3:-engine-cache}"
STORAGE_CLASS="node-local-engine-cache"
export CLAIM_NAME="$CLAIM"

[ -f "$SCENARIO" ] || log_error "no scenario at ${SCENARIO}"
command -v yq >/dev/null 2>&1 || log_error "yq is required (the standup edits the scenario with it)"
case "$CLAIM" in
    *[!a-z0-9-]*|"") log_error "not a claim name: '${CLAIM}' (a DNS-1123 label)" ;;
esac

# A static volume with a claimRef binds within moments of the apply, but not
# in the same instant: wait a little for Bound before refusing.
phase=""
for _ in $(seq 1 15); do
    phase="$(kubectl get pvc -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$phase" = Bound ] && break
    sleep 2
done
[ "$phase" = Bound ] || log_error "namespace ${NAMESPACE} has no Bound claim named ${CLAIM} after 30 s (found: '${phase:-none}'); run deploy/enginecache.sh apply first (make engine-cache), and check make engine-cache-status"
class="$(kubectl get pvc -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.spec.storageClassName}' 2>/dev/null || true)"
[ "$class" = "$STORAGE_CLASS" ] || log_error "claim ${CLAIM} in ${NAMESPACE} is on storage class '${class}', not ${STORAGE_CLASS}: it is not the node-local cache deploy/enginecache.sh makes"

# every engine-cache volume, wherever the scenario keeps it
n="$(yq -r '[.. | select(type == "!!map" and has("additionalVolumes")) | .additionalVolumes[] | select(.name == "engine-cache")] | length' "$SCENARIO")"
[ "$n" -gt 0 ] || log_error "no additionalVolumes entry named engine-cache in ${SCENARIO}; the scenario declares no engine cache to move"
yq -i '(.. | select(type == "!!map" and has("additionalVolumes")) | .additionalVolumes[] | select(.name == "engine-cache")) |= (.type = "persistentVolumeClaim" | .persistentVolumeClaim = {"claimName": strenv(CLAIM_NAME)})' "$SCENARIO"
yq -i 'del(.. | select(type == "!!map" and has("additionalVolumeMounts")) | .additionalVolumeMounts[] | select(.name == "engine-cache") | .subPath)' "$SCENARIO"

left="$(yq -r '[.. | select(type == "!!map" and has("additionalVolumes")) | .additionalVolumes[] | select(.name == "engine-cache") | .persistentVolumeClaim.claimName] | map(select(. != strenv(CLAIM_NAME))) | length' "$SCENARIO")"
[ "$left" = 0 ] || log_error "an engine-cache volume still names another claim in ${SCENARIO}"
sub="$(yq -r '[.. | select(type == "!!map" and has("additionalVolumeMounts")) | .additionalVolumeMounts[] | select(.name == "engine-cache") | has("subPath")] | map(select(.)) | length' "$SCENARIO")"
[ "$sub" = 0 ] || log_error "an engine-cache mount still carries a subPath in ${SCENARIO}"
echo "node-local engine caches: ${n} engine-cache volume(s) now mount claim ${CLAIM}; the engines compile once per node"
