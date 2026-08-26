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
ACCELERATOR=""
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
# A level-1 sleeper is charged roughly 2.6 GiB + 1.4x its weights -- measured on
# an H100, where a 0.6B costs 4.1 GiB and an 8B costs 23.4 GiB. Operators do not
# know that number and should not have to: getting it wrong is the expensive
# direction, because one model too many does not fail its own admission, it
# OOM-kills the launcher and takes every model already resident with it.
memory_for() {
  local count="$1" size="$2" billions
  billions="$(printf '%s' "$size" | sed 's/[Bb]$//')"
  awk -v n="$count" -v b="$billions" 'BEGIN {
    if (b <= 0) { print ""; exit }
    weights = b * 2
    per = 2.6 + 1.4 * weights
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
    memory="$(memory_for "$MODELS" "$MODEL_SIZE")"
    if [ -z "$memory" ]; then
      log_error "--model-size must look like 8B or 0.6B"
    fi
    log_info "Sizing for ${MODELS} x ${MODEL_SIZE} sleepers -> memory ${memory} (2.6GiB + 1.4x weights each, measured)"
  else
    memory="128Gi"
    log_warning "No --models/--model-size given; defaulting memory to ${memory}. That limit IS the warm-set budget: it decides how many models a Pod can hold, and changing it later rolls the pool and reloads every resident model."
  fi

  if [ -z "$ACCELERATOR" ]; then
    log_warning "No --accelerator given, so these Pods may schedule on ANY GPU node. A warm copy is only reusable on the GPU it was loaded on: WVA will decline every model whose accelerator it can prove differs, and this pool will hold devices while warming nothing."
  fi

  local manifest
  manifest=$(warmpool_manifest "$memory")

  if [ "$APPLY" -eq 0 ]; then
    printf '%s\n' "$manifest"
    return 0
  fi

  printf '%s\n' "$manifest" | kubectl apply -f - >/dev/null
  log_success "Pool '${POOL_NAME}' created in ${NAMESPACE}: ${POOL_REPLICAS} Pods (max ${POOL_MAX}), reserve ${RESERVE}, ${GPUS_PER_POD} GPU each, ${memory} per Pod"
  log_info "Models join it with:  warmPool: ${POOL_NAME}   in their ScaledObject trigger metadata"
  log_warning "No NetworkPolicy is created here. The shipped one (config/warmpool) restricts the supervisor ports to WVA, and its namespaceSelector must name ${WVA_NAMESPACE}."
}

warmpool_manifest() {
  local memory="$1"
  local node_selector=""
  if [ -n "$ACCELERATOR" ]; then
    node_selector="      nodeSelector:
        nvidia.com/gpu.product: ${ACCELERATOR}"
  fi
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
      automountServiceAccountToken: false
${node_selector}
      containers:
        - name: inference-server
          image: ${PROXY_IMAGE}
          resources:
            limits:
              nvidia.com/gpu: "${GPUS_PER_POD}"
              memory: ${memory}
            requests:
              nvidia.com/gpu: "${GPUS_PER_POD}"
              memory: ${memory}
          volumeMounts:
            - name: model-storage
              mountPath: /model-cache
      volumes:
        - name: model-storage
          persistentVolumeClaim:
            claimName: ${CACHE_CLAIM}
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: wva-warm-pool-${POOL_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
    app.kubernetes.io/component: warm-pool
spec:
  scaleTargetRef:
    name: wva-warm-pool-${POOL_NAME}
  minReplicaCount: ${POOL_REPLICAS}
  maxReplicaCount: ${POOL_MAX}
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          stabilizationWindowSeconds: 60
        scaleDown:
          stabilizationWindowSeconds: 900
  triggers:
    - type: external-push
      metadata:
        scalerAddress: wva-external-scaler.${WVA_NAMESPACE}.svc.cluster.local:9090
        warmPoolName: ${POOL_NAME}
        warmPoolSleepMinSize: "${RESERVE}"
YAML
}

# sizing answers the question that comes before "which pools" -- whether a given
# model can be warmed on this hardware at all. Every threshold moves with the
# node shape, so it reads the nodes rather than quoting one fleet's numbers.
cmd_sizing() {
  python3 "$HERE/lib/warmpool_sizing.py" "$@"
}

cmd_delete() {
  require NAMESPACE namespace
  # BOTH, and the ScaledObject FIRST: it is what declares the pool, so removing
  # it stops WVA lending Pods that are about to disappear.
  kubectl delete scaledobject "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" --ignore-not-found >/dev/null
  kubectl delete deployment "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" --ignore-not-found >/dev/null
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
