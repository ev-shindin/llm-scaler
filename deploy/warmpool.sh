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
# Where Prometheus runs. Empty means no scrape config is created and no
# monitoring peer is opened: a bridge's engine metrics are then reachable by
# nothing, and a variant's demand under-reads for as long as the pool is lending
# to it. Guessing a namespace would be worse -- a rule admitting the wrong one
# reads as monitoring that is configured.
MONITORING_NAMESPACE=""
# The RuntimeClass the GPU operator installed, if it installed one. CLUSTER
# SPECIFIC, and wrong in either direction is a failure: naming one the cluster
# does not have fails ADMISSION, so the Pods are never created; omitting one it
# requires gives containers with no GPU access, which fails later and far more
# quietly. So the default is the value the manifests carry, and create REFUSES
# if the cluster has no such RuntimeClass rather than producing Pods that cannot
# schedule. Pass --runtime-class none to omit it.
RUNTIME_CLASS="nvidia-legacy"
CACHE_CLAIM="model-pvc"
APPLY=1
# On by DEFAULT. The only cluster-specific value the shipped policy needs is the
# WVA namespace, which is already a required flag, so there is nothing left for
# an operator to fill in -- and the unprotected state is not a mild one.
NETWORK_POLICY=1
# The supervisor image. Empty keeps the one the manifests pin.
#
# A GROUP pool needs one carrying the follower fix -- see the --group-size check
# in cmd_create for why the stock image cannot serve one.
LAUNCHER_IMAGE=""

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
  --runtime-class NAME the RuntimeClass for the pool Pods (default nvidia-legacy),
                       or `none` to set none. Cluster-specific: a name the
                       cluster does not have fails admission outright, and
                       omitting one it needs gives containers with no GPU
  --monitoring-namespace NS
                       where Prometheus runs. Creates a PodMonitor for the pool
                       and admits that namespace to the serving port, so a LENT
                       Pod's engine metrics are scraped. Without it the traffic
                       a bridge carries is invisible and the variant it is
                       covering for reads as having LESS demand than it has
  --cache-claim NAME   RWX model cache PVC. Default: model-pvc
  --launcher-image REF the supervisor image. REQUIRED with --group-size > 1:
                       the stock launcher sends every rank to vLLM's API server,
                       which has no follower path, so an engine spanning Pods
                       cannot start. Build one from the fork carrying that fix.
  --no-network-policy  do not create the ingress boundary. For clusters where
                       policy is managed centrally -- NOT a convenience: without
                       one, :8001 accepts caller-supplied argv from anything that
                       can reach the Pod IP, in a container that mounts the
                       shared model cache read-write.
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
  # Both operands checked as TEXT before awk sees them, because awk compares a
  # non-numeric operand as a string: "large" <= 0 is false, so the b <= 0 guard
  # below never fired and a typo fell through to the 8Gi floor with a confident
  # "Sizing for ..." line printed over it. That limit IS the warm-set budget, so
  # the pool either refused every admission or OOM-killed the launcher and took
  # every resident model with it.
  #
  # A unit suffix is rejected rather than interpreted: --model-size 8Gi means a
  # size in gibibytes to whoever typed it, and reading it as 8 BILLION
  # parameters is a factor of six the operator never asked for.
  case "$count" in
    ''|*[!0-9]*) printf ''; return ;;
  esac
  case "$size" in
    *[Bb]) : ;;
    *[!0-9.]*) printf ''; return ;;
  esac
  billions="$(printf '%s' "$size" | sed 's/[Bb]$//')"
  case "$billions" in
    ''|*[!0-9.]*|*.*.*) printf ''; return ;;
  esac
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

  # A pool is a Deployment or a LeaderWorkerSet, never both -- delete assumes it,
  # and two workloads under one name would both supply Pods carrying the same
  # pool label. `kubectl apply` cannot notice: it creates the new kind and leaves
  # the old one running, holding its accelerators, invisible to this script from
  # then on.
  #
  # Refused rather than cleaned up. Removing the other kind would delete a live
  # workload whose Pods may be lent out and serving, which is not something a
  # `create` should do without being asked.
  local other=deployment
  if [ "$GROUP_SIZE" -le 1 ]; then
    other=leaderworkerset
  fi
  if [ "$APPLY" -eq 1 ] && kubectl get "$other" "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" >/dev/null 2>&1; then
    log_error "pool '${POOL_NAME}' already exists in ${NAMESPACE} as a ${other}, and --group-size ${GROUP_SIZE} would create the other kind. Both would supply Pods under the same pool label and the old one would go on holding its accelerators unseen. Remove it first: ${0##*/} delete -n ${NAMESPACE} --name ${POOL_NAME}"
  fi

  local memory
  if [ -n "$MODELS" ] && [ -n "$MODEL_SIZE" ]; then
    # GPUS_PER_POD is ranks per Pod: every device the engine spans is a worker
    # process with its own CUDA context, and they all share this one limit.
    memory="$(memory_for "$MODELS" "$MODEL_SIZE" "$GPUS_PER_POD")"
    if [ -z "$memory" ]; then
      log_error "--models must be a whole number and --model-size must look like 8B or 0.6B (a unit like 8Gi is not a parameter count); got --models '$MODELS' --model-size '$MODEL_SIZE'"
    fi
    log_info "Sizing for ${MODELS} x ${MODEL_SIZE} sleepers at ${GPUS_PER_POD} rank(s) each -> memory ${memory} (${GPUS_PER_POD} x 2.6GiB + 1.4x weights per model, measured)"
  else
    memory="128Gi"
    log_warning "Sizing needs BOTH --models and --model-size (got --models '${MODELS:-}' --model-size '${MODEL_SIZE:-}'); defaulting memory to ${memory}. That limit IS the warm-set budget: it decides how many models a Pod can hold, and changing it later rolls the pool and reloads every resident model."
  fi

  if [ -z "$ACCELERATOR" ]; then
    log_warning "No --accelerator given, so these Pods may schedule on ANY GPU node. A warm copy is only reusable on the GPU it was loaded on: WVA will decline every model whose accelerator it can prove differs, and this pool will hold devices while warming nothing."
  fi

  if [ "$GROUP_SIZE" -gt 1 ] && [ -z "$LAUNCHER_IMAGE" ]; then
    # Refused rather than warned. A group pool on the stock launcher schedules,
    # goes Ready, holds every one of its accelerators, and can never form an
    # engine: the launcher runs each rank through vLLM's OpenAI API server,
    # which knows nothing about multi-node rank, so the follower builds a full
    # engine core and dies asserting "collective_rpc should not be called on
    # follower node". Nothing about that is visible from outside -- Pods
    # running, GPUs held, admissions timing out -- which is exactly the failure
    # this script exists to keep operators out of.
    log_error "--group-size ${GROUP_SIZE} needs --launcher-image naming a build with the follower fix (llm-d-fast-model-actuation, 'Route a follower rank to the headless executor'). The stock launcher holds the GPUs and never forms the engine"
  fi

  if [ "$GROUP_SIZE" -gt 1 ]; then
    log_info "Group pool: each warm unit is ${GROUP_SIZE} Pods x ${GPUS_PER_POD} GPU = $((GROUP_SIZE * GPUS_PER_POD)) devices"
    log_warning "A group serves ONLY models declaring --nnodes ${GROUP_SIZE}. A group's size is fixed when it is created -- an engine laid out across a different number of Pods is a different engine, and WVA declines it permanently."
    log_info "Each Pod of a group runs the SAME image and the same supervisor. WVA"
    log_info "  gives each rank its position when it admits a model -- --node-rank from"
    log_info "  the Pod's LeaderWorkerSet worker index, --master-addr from the leader,"
    log_info "  and --headless on everything but rank 0. Nothing here has to say it,"
    log_info "  and a worker template that hard-coded a rank would fight the fan-out."
  fi

  local manifest
  if [ "$GROUP_SIZE" -gt 1 ]; then
    manifest=$(warmpool_group_manifest "$memory")
  else
    manifest=$(warmpool_manifest "$memory")
  fi

  if [ -n "$LAUNCHER_IMAGE" ]; then
    # Substituted rather than templated into the generated Pod spec: that block
    # is rendered from config/warmpool and checked for drift, so the image stays
    # one fact in one place and this only overrides it.
    manifest="$(printf '%s\n' "$manifest" | sed "s|image: ghcr.io/llm-d-incubation/llm-d-fast-model-actuation/launcher:[^[:space:]]*|image: ${LAUNCHER_IMAGE}|g")"
    log_info "Supervisor image: ${LAUNCHER_IMAGE}"
  fi

  if [ "$NETWORK_POLICY" -eq 1 ]; then
    manifest="${manifest}
---
$(warmpool_networkpolicy)"
  fi

  if [ -n "$MONITORING_NAMESPACE" ]; then
    manifest="${manifest}
---
$(warmpool_podmonitor)"
  fi

  if [ "$APPLY" -eq 0 ]; then
    printf '%s\n' "$manifest"
    return 0
  fi

  # The RuntimeClass has to EXIST. Naming one the cluster does not have fails
  # admission, so the Deployment is created, reports nothing useful, and no Pod
  # ever appears -- which reads as a scheduling problem rather than as a name
  # that was wrong from the start. The default is the name ONE cluster's GPU
  # operator installed, so this is the flag most likely to be wrong on a cluster
  # nobody has run this on yet.
  #
  # Checked rather than guessed away: --runtime-class none is how you say the
  # cluster needs none, and that is a different statement from not having thought
  # about it.
  if [ -n "$RUNTIME_CLASS" ] && ! kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1; then
    log_error "no RuntimeClass '${RUNTIME_CLASS}' on this cluster, so every pool Pod would fail admission and none would ever be created. Name the one your GPU operator installed with --runtime-class, or pass --runtime-class none if it installed none. Available: $(kubectl get runtimeclass -o name 2>/dev/null | tr '\n' ' ')"
  fi

  printf '%s\n' "$manifest" | kubectl apply -f - >/dev/null
  log_success "Pool '${POOL_NAME}' created in ${NAMESPACE}: ${POOL_REPLICAS} Pods (max ${POOL_MAX}), reserve ${RESERVE}, ${GPUS_PER_POD} GPU each, ${memory} per Pod"
  log_info "Models join it with:  warmPool: ${POOL_NAME}   in their ScaledObject trigger metadata"
  if [ "$NETWORK_POLICY" -eq 1 ]; then
    log_info "NetworkPolicy wva-warm-pool-${POOL_NAME}: :8001/:8002/:9001-9016 admit only WVA in ${WVA_NAMESPACE}; :8000 admits this namespace"
    log_info "If the pool later reports itself EMPTY while holding accelerators, check this first: a wrong WVA namespace denies the supervisor read, which looks exactly like a pool that is too small."
  else
    log_warning "--no-network-policy: nothing restricts :8001, which spawns processes with caller-supplied argv in a container that mounts the model cache read-write. Apply your own boundary."
  fi
  if [ -n "$MONITORING_NAMESPACE" ]; then
    log_info "PodMonitor wva-warm-pool-${POOL_NAME}: scrapes :8000/metrics on AWAKE Pods only, and ${MONITORING_NAMESPACE} is admitted to that port"
  else
    log_warning "No --monitoring-namespace, so nothing scrapes this pool. A Pod lent to a model serves that model's traffic; unscraped, that load is invisible and the model reads as having LESS demand while the pool is covering for it."
  fi
}

# BEGIN GENERATED POD SPEC -- regenerate with: make warmpool-render
#
# Generated from config/warmpool/warmpool-deployment.yaml. DO NOT EDIT by
# hand: run `make warmpool-render` after changing that manifest. CI fails
# on drift, because a pool Pod that disagrees with the shipped one holds
# GPUs and answers nothing -- which is what this file exists to prevent.
pool_pod_spec() {
  local memory="$1" indent="$2" role="${3:-leader}"

  # Conditional: an accelerator nobody named adds no key, rather than an
  # empty one, which would pin the Pod to a GPU product called "".
  local WP_NODE_SELECTOR=""
  if [ -n "$ACCELERATOR" ]; then
    WP_NODE_SELECTOR="nodeSelector:
  nvidia.com/gpu.product: ${ACCELERATOR}"
  fi

  # Conditional for the same reason and with more at stake: naming a
  # RuntimeClass the cluster does not have fails ADMISSION, so the Pod is
  # never created -- and omitting one the cluster DOES require gives a
  # container with no GPU access, which fails later and much more quietly.
  local WP_RUNTIME_CLASS=""
  if [ -n "$RUNTIME_CLASS" ]; then
    WP_RUNTIME_CLASS="runtimeClassName: ${RUNTIME_CLASS}"
  fi
  local WP_MEMORY="$memory"

  local out
  if [ "$role" = "worker" ]; then
    out=$(cat <<YAML
${WP_NODE_SELECTOR}
${WP_RUNTIME_CLASS}
automountServiceAccountToken: false
securityContext:
  runAsNonRoot: true
  seccompProfile:
    type: RuntimeDefault
affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        topologyKey: kubernetes.io/hostname
        labelSelector:
          matchLabels:
            app.kubernetes.io/name: workload-variant-autoscaler
            app.kubernetes.io/component: warm-pool
tolerations:
- key: nvidia.com/gpu
  operator: Exists
  effect: NoSchedule
terminationGracePeriodSeconds: 120
containers:
- name: inference-server
  image: ghcr.io/llm-d-incubation/llm-d-fast-model-actuation/launcher:v0.6.4@sha256:9df3f3bee8c321c14d5c0265d94ab8ae533cff1899c65eeb2623a5dcd753647e
  imagePullPolicy: IfNotPresent
  command:
  - /bin/bash
  - -c
  args:
  - 'exec python3 /app/launcher.py \\

    --host 0.0.0.0 \\

    --log-level info \\

    --port=8001

    '
  ports:
  - name: supervisor
    containerPort: 8001
    protocol: TCP
  env:
  - name: HOME
    value: /pod-home
  - name: PYTHONNOUSERSITE
    value: '1'
  - name: HF_HOME
    value: /model-cache
  - name: VLLM_CACHE_ROOT
    value: /model-cache/vllm
  - name: FLASHINFER_WORKSPACE_DIR
    value: /model-cache/flashinfer
  - name: TRITON_CACHE_DIR
    value: /model-cache/triton
  - name: XDG_CACHE_HOME
    value: /model-cache
  - name: XDG_CONFIG_HOME
    value: /model-cache/config
  - name: USER
    value: vllm
  - name: NODE_NAME
    valueFrom:
      fieldRef:
        fieldPath: spec.nodeName
  - name: NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
  securityContext:
    allowPrivilegeEscalation: false
    capabilities:
      drop:
      - ALL
  resources:
    limits:
      nvidia.com/gpu: ${GPUS_PER_POD}
      memory: ${WP_MEMORY}
    requests:
      nvidia.com/gpu: ${GPUS_PER_POD}
      memory: ${WP_MEMORY}
      cpu: '2'
  volumeMounts:
  - name: pod-home
    mountPath: /pod-home
  - name: model-cache
    mountPath: /model-cache
  lifecycle:
    preStop:
      exec:
        command:
        - python3
        - -c
        - "import json, time, urllib.request\\n# One deadline for the whole hook, not\\
          \\ a timeout per call.\\n# Per-call timeouts SUM: listing plus one /is_sleeping\\
          \\ per\\n# resident model plus the sleep itself came to 120s or more\\n# for\\
          \\ a Pod holding several, which is the grace period, so\\n# SIGKILL landed\\
          \\ mid-drain exactly when the engines were\\n# unhealthy and the drain mattered\\
          \\ most. 100s leaves the\\n# interpreter room to start and the kubelet room\\
          \\ to act.\\ndeadline = time.monotonic() + 100\\ndef left(cap):\\n    return\\
          \\ max(1, min(cap, deadline - time.monotonic()))\\ndef post(url):\\n    try:\\n\\
          \\        urllib.request.urlopen(urllib.request.Request(url, method=\\"POST\\"\\
          ), timeout=left(110)).read()\\n    except Exception as err:\\n        print(\\"\\
          drain:\\", url, err)\\ntry:\\n    raw = urllib.request.urlopen(\\"http://127.0.0.1:8001/v2/vllm/instances\\"\\
          , timeout=left(5)).read()\\n    for inst in json.loads(raw).get(\\"instances\\"\\
          , []):\\n        if time.monotonic() >= deadline:\\n            print(\\"drain:\\
          \\ out of time before every instance was checked\\")\\n            break\\n\\
          \\        opts = (inst.get(\\"options\\") or \\"\\").split()\\n        port =\\
          \\ next((opts[i + 1] for i, f in enumerate(opts) if f == \\"--port\\"), None)\\n\\
          \\        if not port:\\n            continue\\n        try:\\n            st\\
          \\ = json.loads(urllib.request.urlopen(f\\"http://127.0.0.1:{port}/is_sleeping\\"\\
          , timeout=left(5)).read())\\n        except Exception:\\n            continue\\n\\
          \\        if not st.get(\\"is_sleeping\\", True):\\n            post(f\\"http://127.0.0.1:{port}/sleep?level=1&mode=wait\\"\\
          )\\nexcept Exception as err:\\n    print(\\"drain: could not list instances:\\"\\
          , err)\\n"
  readinessProbe:
    exec:
      command:
      - python3
      - -c
      - "import sys, urllib.request\\ntry:\\n    urllib.request.urlopen(\\"http://127.0.0.1:8001/health\\"\\
        , timeout=3)\\nexcept Exception as err:\\n    print(err); sys.exit(1)\\n"
    initialDelaySeconds: 5
    periodSeconds: 10
  livenessProbe:
    exec:
      command:
      - python3
      - -c
      - "import sys, urllib.request\\ntry:\\n    urllib.request.urlopen(\\"http://127.0.0.1:8001/health\\"\\
        , timeout=5)\\nexcept Exception as err:\\n    print(err); sys.exit(1)\\n"
    initialDelaySeconds: 30
    periodSeconds: 30
    failureThreshold: 3
volumes:
- name: pod-home
  emptyDir: {}
- name: model-cache
  persistentVolumeClaim:
    claimName: ${CACHE_CLAIM}
YAML
    )
  else
    out=$(cat <<YAML
${WP_NODE_SELECTOR}
${WP_RUNTIME_CLASS}
automountServiceAccountToken: false
securityContext:
  runAsNonRoot: true
  seccompProfile:
    type: RuntimeDefault
affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        topologyKey: kubernetes.io/hostname
        labelSelector:
          matchLabels:
            app.kubernetes.io/name: workload-variant-autoscaler
            app.kubernetes.io/component: warm-pool
tolerations:
- key: nvidia.com/gpu
  operator: Exists
  effect: NoSchedule
terminationGracePeriodSeconds: 120
containers:
- name: inference-server
  image: ghcr.io/llm-d-incubation/llm-d-fast-model-actuation/launcher:v0.6.4@sha256:9df3f3bee8c321c14d5c0265d94ab8ae533cff1899c65eeb2623a5dcd753647e
  imagePullPolicy: IfNotPresent
  command:
  - /bin/bash
  - -c
  args:
  - 'exec python3 /app/launcher.py \\

    --host 0.0.0.0 \\

    --log-level info \\

    --port=8001

    '
  ports:
  - name: supervisor
    containerPort: 8001
    protocol: TCP
  env:
  - name: HOME
    value: /pod-home
  - name: PYTHONNOUSERSITE
    value: '1'
  - name: HF_HOME
    value: /model-cache
  - name: VLLM_CACHE_ROOT
    value: /model-cache/vllm
  - name: FLASHINFER_WORKSPACE_DIR
    value: /model-cache/flashinfer
  - name: TRITON_CACHE_DIR
    value: /model-cache/triton
  - name: XDG_CACHE_HOME
    value: /model-cache
  - name: XDG_CONFIG_HOME
    value: /model-cache/config
  - name: USER
    value: vllm
  - name: NODE_NAME
    valueFrom:
      fieldRef:
        fieldPath: spec.nodeName
  - name: NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
  securityContext:
    allowPrivilegeEscalation: false
    capabilities:
      drop:
      - ALL
  resources:
    limits:
      nvidia.com/gpu: ${GPUS_PER_POD}
      memory: ${WP_MEMORY}
    requests:
      nvidia.com/gpu: ${GPUS_PER_POD}
      memory: ${WP_MEMORY}
      cpu: '2'
  volumeMounts:
  - name: pod-home
    mountPath: /pod-home
  - name: model-cache
    mountPath: /model-cache
  lifecycle:
    preStop:
      exec:
        command:
        - python3
        - -c
        - "import json, time, urllib.request\\n# One deadline for the whole hook, not\\
          \\ a timeout per call.\\n# Per-call timeouts SUM: listing plus one /is_sleeping\\
          \\ per\\n# resident model plus the sleep itself came to 120s or more\\n# for\\
          \\ a Pod holding several, which is the grace period, so\\n# SIGKILL landed\\
          \\ mid-drain exactly when the engines were\\n# unhealthy and the drain mattered\\
          \\ most. 100s leaves the\\n# interpreter room to start and the kubelet room\\
          \\ to act.\\ndeadline = time.monotonic() + 100\\ndef left(cap):\\n    return\\
          \\ max(1, min(cap, deadline - time.monotonic()))\\ndef post(url):\\n    try:\\n\\
          \\        urllib.request.urlopen(urllib.request.Request(url, method=\\"POST\\"\\
          ), timeout=left(110)).read()\\n    except Exception as err:\\n        print(\\"\\
          drain:\\", url, err)\\ntry:\\n    raw = urllib.request.urlopen(\\"http://127.0.0.1:8001/v2/vllm/instances\\"\\
          , timeout=left(5)).read()\\n    for inst in json.loads(raw).get(\\"instances\\"\\
          , []):\\n        if time.monotonic() >= deadline:\\n            print(\\"drain:\\
          \\ out of time before every instance was checked\\")\\n            break\\n\\
          \\        opts = (inst.get(\\"options\\") or \\"\\").split()\\n        port =\\
          \\ next((opts[i + 1] for i, f in enumerate(opts) if f == \\"--port\\"), None)\\n\\
          \\        if not port:\\n            continue\\n        try:\\n            st\\
          \\ = json.loads(urllib.request.urlopen(f\\"http://127.0.0.1:{port}/is_sleeping\\"\\
          , timeout=left(5)).read())\\n        except Exception:\\n            continue\\n\\
          \\        if not st.get(\\"is_sleeping\\", True):\\n            post(f\\"http://127.0.0.1:{port}/sleep?level=1&mode=wait\\"\\
          )\\nexcept Exception as err:\\n    print(\\"drain: could not list instances:\\"\\
          , err)\\n"
  readinessProbe:
    exec:
      command:
      - python3
      - -c
      - "import sys, urllib.request\\ntry:\\n    urllib.request.urlopen(\\"http://127.0.0.1:8001/health\\"\\
        , timeout=3)\\nexcept Exception as err:\\n    print(err); sys.exit(1)\\n"
    initialDelaySeconds: 5
    periodSeconds: 10
  livenessProbe:
    exec:
      command:
      - python3
      - -c
      - "import sys, urllib.request\\ntry:\\n    urllib.request.urlopen(\\"http://127.0.0.1:8001/health\\"\\
        , timeout=5)\\nexcept Exception as err:\\n    print(err); sys.exit(1)\\n"
    initialDelaySeconds: 30
    periodSeconds: 30
    failureThreshold: 3
- name: proxy
  image: ${PROXY_IMAGE}
  imagePullPolicy: IfNotPresent
  args:
  - --port=8000
  - --control-port=8002
  - --v=2
  ports:
  - name: serving
    containerPort: 8000
    protocol: TCP
  - name: proxy-control
    containerPort: 8002
    protocol: TCP
  livenessProbe:
    exec:
      command:
      - /warmpool-proxy
      - --check=live
      - --control-port=8002
    initialDelaySeconds: 3
    periodSeconds: 10
  readinessProbe:
    exec:
      command:
      - /warmpool-proxy
      - --check=ready
      - --control-port=8002
    initialDelaySeconds: 3
    periodSeconds: 1
    failureThreshold: 1
    timeoutSeconds: 2
  securityContext:
    allowPrivilegeEscalation: false
    capabilities:
      drop:
      - ALL
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
    limits:
      cpu: 500m
      memory: 128Mi
volumes:
- name: pod-home
  emptyDir: {}
- name: model-cache
  persistentVolumeClaim:
    claimName: ${CACHE_CLAIM}
YAML
    )
  fi

  # Blank lines go: an unset nodeSelector leaves one, and a stray blank
  # line inside a Pod spec is harmless but reads as a mistake.
  printf '%s\n' "$out" | grep -v '^[[:space:]]*$' | sed "s/^/${indent}/"
}
# END GENERATED POD SPEC


# warmpool_networkpolicy renders the ingress boundary for these Pods.
#
# Emitted by DEFAULT rather than left to the operator, because the shipped
# manifest needs exactly one substitution -- the namespace WVA runs in -- and
# that is already a required flag here. Leaving it out was not neutral: without
# a policy, :8001 is reachable by anything that can route to the Pod IP, and
# :8001 spawns processes with caller-supplied argv and environment in a
# container that mounts the shared model cache read-write. That is arbitrary
# execution plus a write primitive over other tenants' weights, so "apply this
# separately" is not a safe default to ship.
#
# The port range is pool.BasePort..MaxInstancesPerPod, which is all an engine
# can bind -- the controller calls /is_sleeping and /wake_up on the instances
# directly, from outside the Pod, so omitting it leaves the pool inert.
# warmpool_podmonitor prints the scrape config for this pool.
#
# The PROXY's serving port, not the engines' own: the proxy forwards /metrics to
# whichever engine is awake, so one stable address covers every model the Pod
# holds, and the engine ports stay reserved for the controller.
#
# Only READY Pods become targets, and readiness here means a model is awake --
# that is exactly what the Pod's readiness probe reports. Dropping the rest
# before a target is built is what keeps an idle pool from showing as a wall of
# permanently-DOWN targets, which reads as broken monitoring rather than as a
# pool with nothing awake.
warmpool_podmonitor() {
  cat <<YAML
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: wva-warm-pool-${POOL_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
    app.kubernetes.io/component: warm-pool
    llm-d.ai/warm-pool: ${POOL_NAME}
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: warm-pool
      llm-d.ai/warm-pool: ${POOL_NAME}
  podMetricsEndpoints:
    - port: serving
      path: /metrics
      interval: 30s
      relabelings:
        - sourceLabels: [__meta_kubernetes_pod_ready]
          action: keep
          regex: "true"
        - sourceLabels: [__meta_kubernetes_pod_name]
          targetLabel: pod_name
          action: replace
YAML
}

warmpool_networkpolicy() {
  # RANK-TO-RANK, and only for a pool whose unit spans Pods.
  #
  # An engine laid across several Pods rendezvouses over the network: torch
  # distributed on its master port, then NCCL on ports it chooses at runtime.
  # None of that is expressible as a port list, and none of it is admitted by
  # the rules above -- so with a policy and no peer rule, the ranks start, wait
  # for each other, and never form. Measured on a real two-node group: both
  # ranks alive for five minutes, the leader's API never bound, and a connect
  # from rank 1 to rank 0 TIMED OUT rather than being refused, which is what a
  # dropped packet looks like.
  #
  # Omitted for single-Pod pools, where the Pods have nothing to say to each
  # other and admitting them would only widen the policy.
  #
  # The peer is selected by THIS pool's own label, not by the component label
  # alone: the ranks of one engine already share its memory and are one trust
  # domain, but two different pools are not.
  # SCRAPING, on the serving port. A lent Pod's engine metrics are reachable
  # there and nowhere else a scraper may go: the proxy forwards /metrics to
  # whichever engine is awake, and the engine ports are the controller's. Denied,
  # the bridge is scraped by nothing and the load it carries is invisible to the
  # analyzer -- so a variant's demand READS LOWER while a bridge is covering its
  # shortfall, which is the moment it is highest.
  #
  # Only when a namespace was named. Guessing one would write a rule admitting
  # some other namespace, which is worse than no rule: it reads as monitoring
  # that is configured.
  local MONITORING_RULE=""
  if [ -n "$MONITORING_NAMESPACE" ]; then
    MONITORING_RULE="
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ${MONITORING_NAMESPACE}"
  fi

  local PEER_RULE=""
  if [ "$GROUP_SIZE" -gt 1 ]; then
    PEER_RULE="
    - from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/component: warm-pool
              llm-d.ai/warm-pool: ${POOL_NAME}"
  fi
  cat <<YAML
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: wva-warm-pool-${POOL_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
    app.kubernetes.io/component: warm-pool
    llm-d.ai/warm-pool: ${POOL_NAME}
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/component: warm-pool
      llm-d.ai/warm-pool: ${POOL_NAME}
  policyTypes:
    - Ingress
  ingress:
    # Serving traffic, from this namespace: the EPP dispatches to the Pod's
    # InferencePool target port. Prometheus is admitted here too, when a
    # monitoring namespace was named -- see MONITORING_RULE above.
    - ports:
        - protocol: TCP
          port: 8000
      from:
        - podSelector: {}${MONITORING_RULE}
    # Control, for the controller ONLY. The namespaceSelector is required, not
    # belt and braces: a bare podSelector matches this policy's own namespace,
    # which is the tenant's, and would put :8001 one \`kubectl run\` away from
    # anyone who can create a Pod here.
    - ports:
        - protocol: TCP
          port: 8001
        - protocol: TCP
          port: 8002
        - protocol: TCP
          port: 9001
          endPort: 9016
      from:
        # kubernetes.io/metadata.name is applied by the API server, so naming a
        # namespace needs no labelling step.
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ${WVA_NAMESPACE}
          podSelector:
            matchLabels:
              app.kubernetes.io/name: workload-variant-autoscaler
              control-plane: controller-manager${PEER_RULE}
YAML
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

# A pool of GROUPS. The leader runs the supervisor and serves the API; workers
# hold devices and join its process group. WVA counts only leaders as members,
# reads the group's size from the annotation LWS stamps on every Pod, and treats
# a group missing a Ready Pod as absent -- ranks that cannot form are no engine.
warmpool_group_manifest() {
  local memory="$1"
  local spec
  spec="$(pool_pod_spec "$memory" "        " leader)" || log_error "the pool Pod could not be described, so nothing was applied"
  local worker_spec
  worker_spec="$(pool_pod_spec "$memory" "        " worker)" || log_error "the worker Pod could not be described, so nothing was applied"
  # Leader and worker run the same supervisor and hold the same devices; what
  # makes a Pod rank 2 of an engine is the OPTIONS it is given when a model is
  # admitted, not anything baked into its template. WVA reads the rank from the
  # Pod's LeaderWorkerSet worker index and passes --node-rank, --master-addr and
  # --headless accordingly (see warmGroup), so a rank written here would be a
  # second, fixed answer to a question the controller already answers per model.
  #
  # The ONE difference is the proxy, which only the leader runs: it is the Pod's
  # traffic gate, and a worker takes no traffic.
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
${worker_spec}
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
    apiVersion: leaderworkerset.x-k8s.io/v1
    kind: LeaderWorkerSet
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

cmd_delete() {
  require NAMESPACE namespace
  # --dry-run means the same thing here as everywhere else: say what would
  # happen and change nothing. It was accepted and ignored, so the one
  # subcommand where the flag matters most was the one that deleted anyway.
  if [ "$APPLY" -eq 0 ]; then
    log_info "Would delete from ${NAMESPACE}: scaledobject/wva-warm-pool-${POOL_NAME}, deployment or leaderworkerset/wva-warm-pool-${POOL_NAME}, networkpolicy/wva-warm-pool-${POOL_NAME}"
    return 0
  fi
  # BOTH, and the ScaledObject FIRST: it is what declares the pool, so removing
  # it stops WVA lending Pods that are about to disappear.
  kubectl delete scaledobject "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" --ignore-not-found >/dev/null
  # Whichever kind it is. A pool is one or the other and never both, so asking
  # for both costs one NotFound and removes the chance of leaving a group behind.
  kubectl delete deployment "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete leaderworkerset "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
  # LAST, and only the one create names. A policy left behind selects Pods that
  # no longer exist, which is harmless but accumulates; deleting it first would
  # briefly leave live pool Pods unprotected instead.
  kubectl delete networkpolicy "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
  # Whether or not one was created, and tolerating a cluster with no Prometheus
  # operator: a scrape config left behind selects Pods that no longer exist.
  kubectl delete podmonitor "wva-warm-pool-${POOL_NAME}" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
  log_success "Pool '${POOL_NAME}' removed from ${NAMESPACE}: ScaledObject, workload, NetworkPolicy and PodMonitor"
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
    --monitoring-namespace) MONITORING_NAMESPACE="$2"; shift 2 ;;
    --runtime-class)
      # `none` omits the key. An empty string would too, but spelling it makes
      # the intent unmistakable in a script somebody reads later.
      if [ "$2" = "none" ]; then RUNTIME_CLASS=""; else RUNTIME_CLASS="$2"; fi
      shift 2 ;;
    --cache-claim)   CACHE_CLAIM="$2"; shift 2 ;;
    --dry-run)       APPLY=0; shift ;;
    --launcher-image) LAUNCHER_IMAGE="$2"; shift 2 ;;
    --no-network-policy) NETWORK_POLICY=0; shift ;;
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
