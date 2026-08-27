#!/usr/bin/env bash
# Warm pool lifecycle: plan, create, delete.
#
# A pool is TWO objects that must exist together -- a Deployment supplying the
# Pods, and a ScaledObject whose trigger DECLARES the pool to WVA. So there is
# one place that makes both and one that removes both. Deleting only the
# ScaledObject leaves a Deployment holding accelerators nothing will ever use;
# deleting only the Deployment leaves a trigger pointing at nothing. WVA reports
# the first, but reporting it is worse than not causing it.
#
#   warmpool.sh plan   -n NS                     which pools this namespace wants
#   warmpool.sh create -n NS --name h100-1gpu    make one
#   warmpool.sh delete -n NS --name h100-1gpu    remove both objects
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# common.sh logs through these and does not define them -- install.sh does, and
# it is not our caller. Under `set -u` an undefined BLUE aborts the first log.
BLUE='[0;34m'
GREEN='[0;32m'
YELLOW='[1;33m'
RED='[0;31m'
NC='[0m'
# shellcheck source=lib/common.sh
source "$HERE/lib/common.sh"
# NOTE: log_error EXITS. Nothing may follow it that needs to run.

NAMESPACE=""
POOL_NAME="default"
POOL_REPLICAS=2
POOL_MAX=6
RESERVE=1
MODELS=""
MODEL_SIZE=""
GPUS_PER_POD=1
# The repo this script lives in: config/warmpool is read from it, so the pool
# Pod is described in exactly one place.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ACCELERATOR=""
GROUP_SIZE=1
PROXY_IMAGE=""
WVA_NAMESPACE=""
CACHE_CLAIM="model-pvc"
APPLY=1

usage() {
  cat <<'USAGE'
Warm pool lifecycle.

  warmpool.sh plan   -n NS                     which pools this namespace wants
  warmpool.sh create -n NS --name NAME ...     make one (Deployment + ScaledObject)
  warmpool.sh delete -n NS --name NAME         remove both objects
  warmpool.sh sizing --params 744B [--dtype fp8]
                                              should this model be warmed on THIS
                                              cluster? Reads the nodes and answers.

create options:
  --name NAME          pool name (llm-d.ai/warm-pool). Default: default
  --models N           how many models a Pod should hold
  --model-size SIZE    largest model it must hold, e.g. 8B or 0.6B
                       With --models, the memory limit is COMPUTED from these.
  --gpus N             GPUs per Pod. Default: 1
  --group-size N       Pods per warm unit. >1 builds a LeaderWorkerSet pool for
                       engines that span machines; the models it serves must
                       declare the same --nnodes. Default: 1
  --accelerator PROD   the nvidia.com/gpu.product this pool must run on.
                       A pool named for one GPU that schedules on another is
                       the silent mismatch this whole design exists to avoid.
  --replicas N         starting Pod count, and minReplicaCount. Default: 2
  --max N              maxReplicaCount. MUST exceed the reserve. Default: 6
  --reserve N          warmPoolSleepMinSize. Default: 1
  --proxy-image REF    the warm-pool image you built
  --wva-namespace NS   where WVA runs, for scalerAddress
  --cache-claim NAME   RWX model cache PVC. Default: model-pvc
  --dry-run            print the manifests instead of applying
USAGE
}

# memory_for computes a Pod's memory limit from what it must hold.
#
# A level-1 sleeper is charged roughly 2.6 GiB PER RANK + 1.4x its weights --
# measured on an H100, where a 0.6B costs 4.1 GiB and an 8B costs 23.4 GiB at
# TP=1, and on an H200 where a second rank cost as much as the first (3.16 ->
# 6.75 GiB awake). Operators do not know that number and should not have to:
# getting it wrong is the expensive direction, because one model too many does
# not fail its own admission, it OOM-kills the launcher and takes every model
# already resident with it.
#
# The rank term MUST match internal/warmpool/pool/shape.go. That is the estimate
# the controller admits against, so a Pod sized by a different formula either
# refuses models it had room for or accepts one it cannot hold. They disagreed by
# (ranks-1) x 2.6 GiB per model once, which at --gpus 8 is 18 GiB of silent drift.
memory_for() {
  local count="$1" size="$2" ranks="${3:-1}" billions
  billions="$(printf '%s' "$size" | sed 's/[Bb]$//')"
  awk -v n="$count" -v b="$billions" -v r="$ranks" 'BEGIN {
    if (b <= 0) { print ""; exit }
    if (r < 1) r = 1
    weights = b * 2
    per = 2.6 * r + 1.4 * weights
    total = per * n
    if (total < 8) total = 8
    printf "%dGi", int(total + 0.999)
  }'
}

require() {
  local var="$1" flag="$2"
  if [ -z "${!var:-}" ]; then
    log_error "--${flag} is required"
  fi
}

cmd_plan() {
  require NAMESPACE namespace
  log_info "Grouping ScaledObjects in $NAMESPACE by what a pool would have to provide"
  kubectl get scaledobject -n "$NAMESPACE" -o json 2>/dev/null |
    python3 "$HERE/lib/warmpool_plan.py" "$NAMESPACE"
}

cmd_create() {
  require NAMESPACE namespace
  require PROXY_IMAGE proxy-image
  require WVA_NAMESPACE wva-namespace

  if [ "$POOL_MAX" -le "$RESERVE" ]; then
    log_error "--max ($POOL_MAX) must EXCEED --reserve ($RESERVE): admission draws on free-minus-reserve, so at or below the reserve the budget is zero forever and the pool holds accelerators while warming nothing"
  fi

  local memory
  if [ -n "$MODELS" ] && [ -n "$MODEL_SIZE" ]; then
    # GPUS_PER_POD is ranks per Pod: every device the engine spans is a worker
    # process with its own CUDA context, and they all share this one limit.
    memory="$(memory_for "$MODELS" "$MODEL_SIZE" "$GPUS_PER_POD")"
    if [ -z "$memory" ]; then
      log_error "--model-size must look like 8B or 0.6B"
    fi
    log_info "Sizing for ${MODELS} x ${MODEL_SIZE} sleepers at ${GPUS_PER_POD} rank(s) each -> memory ${memory} (${GPUS_PER_POD} x 2.6GiB + 1.4x weights per model, measured)"
  else
    memory="128Gi"
    log_warning "No --models/--model-size given; defaulting memory to ${memory}. That limit IS the warm-set budget: it decides how many models a Pod can hold, and changing it later rolls the pool and reloads every resident model."
  fi

  if [ -z "$ACCELERATOR" ]; then
    log_warning "No --accelerator given, so these Pods may schedule on ANY GPU node. A warm copy is only reusable on the GPU it was loaded on: WVA will decline every model whose accelerator it can prove differs, and this pool will hold devices while warming nothing."
  fi

  if [ "$GROUP_SIZE" -gt 1 ]; then
    log_info "Group pool: each warm unit is ${GROUP_SIZE} Pods x ${GPUS_PER_POD} GPU = $((GROUP_SIZE * GPUS_PER_POD)) devices"
    log_warning "A group serves ONLY models declaring --nnodes ${GROUP_SIZE}. A group's size is fixed when it is created -- an engine laid out across a different number of Pods is a different engine, and WVA declines it permanently."
    log_warning "WARMING A GROUP IS NOT IMPLEMENTED YET. WVA counts a group as one"
    log_warning "  lendable unit, but every actuation path addresses a single Pod: nothing"
    log_warning "  starts the ranks on the workers. A model matching this group is DECLINED,"
    log_warning "  with that reason, rather than half-started. This pool will hold its GPUs"
    log_warning "  and warm nothing until group actuation exists -- create it only to test"
    log_warning "  the counting, not to serve traffic."
  fi

  local manifest
  if [ "$GROUP_SIZE" -gt 1 ]; then
    manifest=$(warmpool_group_manifest "$memory")
  else
    manifest=$(warmpool_manifest "$memory")
  fi

  if [ "$APPLY" -eq 0 ]; then
    printf '%s\n' "$manifest"
    return 0
  fi

  printf '%s\n' "$manifest" | kubectl apply -f - >/dev/null
  log_success "Pool '${POOL_NAME}' created in ${NAMESPACE}: ${POOL_REPLICAS} Pods (max ${POOL_MAX}), reserve ${RESERVE}, ${GPUS_PER_POD} GPU each, ${memory} per Pod"
  log_info "Models join it with:  warmPool: ${POOL_NAME}   in their ScaledObject trigger metadata"
  log_warning "No NetworkPolicy is created here. The shipped one (config/warmpool) restricts the supervisor ports to WVA, and its namespaceSelector must name ${WVA_NAMESPACE}."
}

# pool_pod_spec prints the pool Pod's spec, DERIVED from config/warmpool rather
# than written out again here.
#
# It was written out again here, and the two copies diverged completely: this
# script emitted ONE container -- the proxy image under the name
# inference-server -- with no command, no ports, no env and no supervisor. The
# shipped manifest has two, the launcher serving the supervisor API on :8001 and
# the proxy on :8000/:8002. Every pool this script created therefore held its
# GPUs and answered nothing, and the controller reported it EMPTY: measured on
# pokprod, where a pool formed correctly across two nodes and then reported
# pods=0 while holding both.
#
# So there is one source now. The knobs below are the only things this script
# decides; everything else -- the launcher image, the env that keeps caches off
# a writable HOME, the probes, the volumes -- comes from the manifest that is
# reviewed and deployed.
pool_pod_spec() {
  local memory="$1" indent="$2"
  local built
  # NOTE: this function runs inside $( ). log_error EXITS THE SUBSHELL ONLY, so
  # every failure here returns non-zero and the CALLER reports it. Getting that
  # wrong applied a Deployment with no containers at all, after printing the
  # error that was supposed to have stopped it.
  if ! built="$(kubectl kustomize "${REPO_ROOT}/config/warmpool" 2>&1)"; then
    printf "could not build %s/config/warmpool: %s
" "$REPO_ROOT" "$built" >&2
    return 1
  fi

  # Every value this script decides, and nothing else. yq's env() reads the
  # PROCESS environment, so each one is exported on the call rather than left as
  # a shell variable -- unexported, they read as empty and the whole spec comes
  # back without them.
  local expr='select(.kind == "Deployment") | .spec.template.spec
      | (.containers[] | select(.name == "inference-server") | .resources.limits["nvidia.com/gpu"]) = env(WP_GPUS)
      | (.containers[] | select(.name == "inference-server") | .resources.requests["nvidia.com/gpu"]) = env(WP_GPUS)
      | (.containers[] | select(.name == "inference-server") | .resources.limits.memory) = env(WP_MEMORY)
      | (.containers[] | select(.name == "inference-server") | .resources.requests.memory) = env(WP_MEMORY)
      | (.containers[] | select(.name == "proxy") | .image) = env(WP_PROXY_IMAGE)
      | (.volumes[] | select(.name == "model-cache") | .persistentVolumeClaim.claimName) = env(WP_CACHE_CLAIM)'
  # Only when one is named. An empty nodeSelector value would pin the Pod to a
  # product called "" and it would never schedule.
  if [ -n "$ACCELERATOR" ]; then
    expr="${expr}
      | .nodeSelector[\"nvidia.com/gpu.product\"] = env(WP_ACCELERATOR)"
  fi

  local out
  if ! out="$(printf '%s' "$built" | WP_GPUS="$GPUS_PER_POD" WP_MEMORY="$memory" \
      WP_PROXY_IMAGE="$PROXY_IMAGE" WP_CACHE_CLAIM="$CACHE_CLAIM" \
      WP_ACCELERATOR="$ACCELERATOR" yq eval "$expr" - 2>&1)"; then
    printf "could not shape the pool Pod spec: %s
" "$out" >&2
    return 1
  fi
  if [ -z "$out" ]; then
    printf "the pool Pod spec came out empty; config/warmpool built nothing usable
" >&2
    return 1
  fi
  printf '%s\n' "$out" | sed "s/^/${indent}/"
}


warmpool_manifest() {
  local memory="$1"
  local spec
  spec="$(pool_pod_spec "$memory" "      ")" || log_error "the pool Pod could not be described, so nothing was applied"
  cat <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: wva-warm-pool-${POOL_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
    app.kubernetes.io/component: warm-pool
    llm-d.ai/warm-pool: ${POOL_NAME}
spec:
  replicas: ${POOL_REPLICAS}
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/component: warm-pool
      llm-d.ai/warm-pool: ${POOL_NAME}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: workload-variant-autoscaler
        app.kubernetes.io/component: warm-pool
        llm-d.ai/warm-pool: ${POOL_NAME}
    spec:
${spec}
YAML
}

# sizing answers the question that comes before "which pools" -- whether a given
# model can be warmed on this hardware at all. Every threshold moves with the
# node shape, so it reads the nodes rather than quoting one fleet's numbers.
cmd_sizing() {
  python3 "$HERE/lib/warmpool_sizing.py" "$@"
}

# A pool of GROUPS. The leader runs the supervisor and serves the API; workers
# hold devices and join its process group. WVA counts only leaders as members,
# reads the group's size from the annotation LWS stamps on every Pod, and treats
# a group missing a Ready Pod as absent -- ranks that cannot form are no engine.
warmpool_group_manifest() {
  local memory="$1"
  local spec
  spec="$(pool_pod_spec "$memory" "        ")" || log_error "the pool Pod could not be described, so nothing was applied"
  # The worker template is the SAME Pod as the leader, deliberately and for now.
  # A worker ought to run its own rank of the engine, and nothing does that yet
  # -- see the decline in policy.doesNotFit. Giving it the leader's shape keeps
  # the group countable (which is what is tested) without inventing a rank
  # protocol here that the controller does not implement.
  cat <<YAML
apiVersion: leaderworkerset.x-k8s.io/v1
kind: LeaderWorkerSet
metadata:
  name: wva-warm-pool-${POOL_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
    app.kubernetes.io/component: warm-pool
    llm-d.ai/warm-pool: ${POOL_NAME}
spec:
  replicas: ${POOL_REPLICAS}
  leaderWorkerTemplate:
    size: ${GROUP_SIZE}
    # A partial group is not a degraded engine, it is no engine: its ranks
    # cannot form, so a Pod lost takes the whole group with it.
    restartPolicy: RecreateGroupOnPodRestart
    leaderTemplate:
      metadata:
        labels:
          app.kubernetes.io/name: workload-variant-autoscaler
          app.kubernetes.io/component: warm-pool
          llm-d.ai/warm-pool: ${POOL_NAME}
      spec:
${spec}
    workerTemplate:
      metadata:
        labels:
          app.kubernetes.io/name: workload-variant-autoscaler
          app.kubernetes.io/component: warm-pool
          llm-d.ai/warm-pool: ${POOL_NAME}
      spec:
${spec}
YAML
}

cmd_delete() {
  require NAMESPACE namespace
  # BOTH, and the ScaledObject FIRST: it is what declares the pool, so removing
  # it stops WVA lending Pods that are about to disappear.
  kubectl delete scaledobject "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" --ignore-not-found >/dev/null
  # Whichever kind it is. A pool is one or the other and never both, so asking
  # for both costs one NotFound and removes the chance of leaving a group behind.
  kubectl delete deployment "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete leaderworkerset "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
  log_success "Pool '${POOL_NAME}' removed from ${NAMESPACE}: ScaledObject and Deployment"
}

if [ $# -lt 1 ]; then
  usage
  exit 2
fi
ACTION="$1"
shift
SIZING_ARGS=()
if [ "$ACTION" = "sizing" ]; then
  SIZING_ARGS=("$@")
  set --
fi
while [ $# -gt 0 ]; do
  case "$1" in
    -n|--namespace)  NAMESPACE="$2"; shift 2 ;;
    --name)          POOL_NAME="$2"; shift 2 ;;
    --models)        MODELS="$2"; shift 2 ;;
    --model-size)    MODEL_SIZE="$2"; shift 2 ;;
    --gpus)          GPUS_PER_POD="$2"; shift 2 ;;
    --accelerator)   ACCELERATOR="$2"; shift 2 ;;
    --group-size)    GROUP_SIZE="$2"; shift 2 ;;
    --replicas)      POOL_REPLICAS="$2"; shift 2 ;;
    --max)           POOL_MAX="$2"; shift 2 ;;
    --reserve)       RESERVE="$2"; shift 2 ;;
    --proxy-image)   PROXY_IMAGE="$2"; shift 2 ;;
    --wva-namespace) WVA_NAMESPACE="$2"; shift 2 ;;
    --cache-claim)   CACHE_CLAIM="$2"; shift 2 ;;
    --dry-run)       APPLY=0; shift ;;
    -h|--help)       usage; exit 0 ;;
    *) usage; log_error "unknown option: $1" ;;
  esac
done

case "$ACTION" in
  plan)   cmd_plan ;;
  sizing) cmd_sizing "${SIZING_ARGS[@]}" ;;
  create) cmd_create ;;
  delete) cmd_delete ;;
  *) usage; log_error "unknown action: $ACTION" ;;
esac
