#!/usr/bin/env bash
# Keep the engines' compile caches on every accelerator node's local disk, so
# a replica's start does not depend on a shared volume being writable.
#
# vLLM keeps three caches between runs -- the torch.compile artefacts, the
# FlashInfer autotune table and Triton's JIT cache -- and a replica that finds
# them warm skips the compile; one that does not pays it at every start.
# Pointing them at a shared RWX claim works until the claim does not: measured
# on one cluster, nine starts of the same pod spec split by node into 51-56 s
# and 75-81 s, and the 25 s was the shared claim being published read-only on
# some nodes by the CSI driver, the engine falling back to an empty cache
# under /tmp, and every start there compiling from nothing (14 s of
# torch.compile against 3 s from the cache, and 8 s more before the engine).
# This puts the caches on the node: one static hostPath PersistentVolume, one
# claim bound to it, and one DaemonSet that prepares the directory on every
# selected node and then reports Ready. The model servers mount the claim
# exactly as they mount any other; a hostPath is a bind mount with no
# storage driver in its path, and each node's cache is warm from the second
# start on that node. (The filesystem under it can still go read-only --
# that is what the preparer's readiness and the engines' guard are for.)
#
#   enginecache.sh apply  -n NS --path DIR --image IMG [--dry-run]   prepare DIR/engine-cache on every accelerator node
#   enginecache.sh status -n NS [--node-selector KEY=VALUE]           which nodes are prepared, which are not, and why
#   enginecache.sh delete -n NS [--dry-run]                           drop the claim, volume and preparer (the caches stay)
#
#   -n, --namespace NS          the namespace the claim and preparer live in
#
# Options:
#   --path DIR                  directory on the node, e.g. /mnt/local/weights/<ns>
#                               -- the same DIR weights.sh takes; the caches
#                               land under DIR/engine-cache (/var/mnt/weights/<ns>
#                               on RHCOS, under /var)
#   --image IMG                 image to prepare with: any image carrying /bin/sh
#                               -- the engine image itself is the natural
#                               choice, and it is then held on the node as a
#                               side effect
#   --capacity SIZE             the volume's declared capacity (default 200Gi;
#                               it is a declaration, hostPath has no quota)
#   --node-selector KEY=VALUE   which nodes count as accelerator nodes. The
#                               default is any node carrying a known GPU
#                               product label (deploy/lib/accelerator_nodes.sh);
#                               set this to what your model servers select
#                               on when they select on something
#   --toleration KEY            tolerate a taint with KEY (any value, any
#                               effect); nvidia.com/gpu is always tolerated
#   --dry-run                   apply: print the manifests instead of
#                               applying them; delete: print what would go
#
# The claim is named engine-cache, one per namespace (the caches inside are
# keyed by a hash of the engine config, so every model and flag set in the
# namespace has its own entry and none hits another's). A model server
# mounts it read-write at, say, /engine-cache and points VLLM_CACHE_ROOT,
# FLASHINFER_WORKSPACE_DIR and TRITON_CACHE_DIR under it -- and keeps a guard
# that unsets a variable whose directory is not writable, so a node that is
# not prepared costs a compile, not the replica. The volume is cluster-scoped:
# apply needs leave to create PersistentVolumes, which a namespace tenant does
# not have -- ask the cluster admin to run apply, or to create the volume.
# Nothing here uses a hostPath volume in a Pod (the preparer mounts the
# claim), so Pod Security "baseline" admits it and "restricted" does not. The
# node directory is cluster-shared, WORLD-writable state (1777 on the three
# cache directories, so any UID the engines run as can write; sticky, so one
# UID's files are not another's to remove), what is written there is charged
# to no quota, and what is written there is CODE: torch.compile artefacts,
# FlashInfer and Triton kernels that every engine on the node loads and
# runs. Whoever can write the directory runs code in every engine on that
# node -- on Kubernetes that is anyone with pods/create in a namespace that
# can mount the claim, which is no more than pods/create already grants,
# and across namespaces it is everyone pointed at the same DIR. So the
# rules are weights.sh's, harder: one DIR per trust domain, on a disk that
# is not the node's own, and wipe DIR/engine-cache on the nodes when a
# namespace or a directory changes hands -- delete keeps the caches, and a
# new tenant of the same name and DIR would load the old one's. 1777 is a
# trade-off, taken so the engines need no fsGroup; a namespace that runs
# its engines under one UID and wants the separation can chown the three
# directories to it and chmod them 1770 on each node. The preparer runs as
# its own ServiceAccount, engine-cache-preparer.
#
# On OpenShift -- NOT YET RUN THERE -- restricted-v2 runs the preparer and
# the engines as the project's range UID with GID 0, and the kubelet does not
# relabel a hostPath: prepare DIR/engine-cache on each node as weights.sh
# --help says (chgrp 0, chmod 2775, chcon -t container_file_t, or a
# MachineConfig mount with context=...:container_file_t:s0). The preparer
# then creates the three directories inside it as that UID, owns them, and
# its chmod 1777 takes (the setgid bit is not inherited by a mode set
# outright; the files carry the project's SELinux level either way). `status`
# says when the preparer died on a permission error.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BLUE='\033[0;34m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'
# shellcheck source=lib/common.sh
source "$HERE/lib/common.sh"
# shellcheck source=lib/accelerator_nodes.sh
source "$HERE/lib/accelerator_nodes.sh"
# shellcheck source=lib/nodedir.sh
source "$HERE/lib/nodedir.sh"
# NOTE: log_error EXITS. Nothing may follow it that needs to run.

NAMESPACE=""
NODE_PATH=""
IMAGE=""
CAPACITY="200Gi"
NODE_SELECTOR=""          # empty: every node carrying a known GPU product label
NODE_SELECTOR_GIVEN=false
TOLERATIONS=("nvidia.com/gpu")
DRY_RUN=false
LABEL_COMPONENT="app.kubernetes.io/component=node-local-engine-cache,app.kubernetes.io/managed-by=wva-enginecache"
STORAGE_CLASS="node-local-engine-cache"
CLAIM="engine-cache"
SUBDIR="engine-cache"
MARKER=".prepared"
SA_NAME="engine-cache-preparer"

usage() {
    sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; $d'
    exit 0
}

# pv_for is the volume's name. PersistentVolumes are cluster-scoped, so the
# namespace and the directory are in the hash: two namespaces preparing the
# same directory each get their own volume, bound to their own claim.
pv_for() {
    printf '%s-%s' "$CLAIM" "$(wva_ns_suffix "${NAMESPACE}/${NODE_PATH}")"
}

check_path() {
    local why
    why="$(nodedir_ok "$1")" || log_error "--path: ${why}"
}

check_image() {
    case "$1" in
        "") log_error "--image must not be empty" ;;
        *[!A-Za-z0-9._:/@-]*) log_error "not an image reference: '$1' (registry/path[:tag|@sha256:digest], no spaces or quotes)" ;;
    esac
}

# preparer_script is the preparer's command. A quoted heredoc: nothing in it
# is bash's to evaluate at render time -- it runs on the node, as written. A
# `$(...)` here would otherwise run on the operator's machine at apply and
# bake its output into every node's manifest. The marker's name reaches it
# as an environment variable. No `$(` and no `$$` in it: Kubernetes rewrites
# both in a container's command.
preparer_script() {
    cat <<'SCRIPT'
for d in vllm flashinfer triton; do
  mkdir -p "/engine-cache/$d" || exit 1
  chmod 1777 "/engine-cache/$d" 2>/dev/null || true
done
touch "/engine-cache/$MARKER" || exit 1
echo "engine cache: prepared on $HOSTNAME: /engine-cache/{vllm,flashinfer,triton}"
trap 'exit 0' TERM; while :; do sleep 3600 & wait; done
SCRIPT
}

# render prints the ServiceAccount, the volume, the claim and the preparer.
render() {
    local pv
    pv="$(pv_for)"
    cat <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${SA_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-engine-cache
    app.kubernetes.io/component: node-local-engine-cache
    app.kubernetes.io/managed-by: wva-enginecache
# The preparer's own identity, so what an admin grants for it (an SCC on
# OpenShift) does not land on the namespace's default ServiceAccount. It
# makes no API call, so no token.
automountServiceAccountToken: false
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${pv}
  labels:
    app.kubernetes.io/name: node-local-engine-cache
    app.kubernetes.io/component: node-local-engine-cache
    app.kubernetes.io/managed-by: wva-enginecache
  annotations:
    wva.llmd.ai/engine-cache-path: "${NODE_PATH}/${SUBDIR}"
spec:
  capacity:
    storage: ${CAPACITY}
  accessModes:
    - ReadWriteMany
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ${STORAGE_CLASS}
  # Bound to this claim and no other: a static volume without a claimRef is
  # taken by the first claim that asks for its class.
  claimRef:
    namespace: ${NAMESPACE}
    name: ${CLAIM}
  hostPath:
    path: ${NODE_PATH}/${SUBDIR}
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${CLAIM}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-engine-cache
    app.kubernetes.io/component: node-local-engine-cache
    app.kubernetes.io/managed-by: wva-enginecache
  annotations:
    wva.llmd.ai/engine-cache-path: "${NODE_PATH}/${SUBDIR}"
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${STORAGE_CLASS}
  volumeName: ${pv}
  resources:
    requests:
      storage: ${CAPACITY}
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ${CLAIM}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-engine-cache
    app.kubernetes.io/component: node-local-engine-cache
    app.kubernetes.io/managed-by: wva-enginecache
  annotations:
    wva.llmd.ai/engine-cache-path: "${NODE_PATH}/${SUBDIR}"
    wva.llmd.ai/engine-cache-node-selector: "${NODE_SELECTOR}"
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: node-local-engine-cache
      wva.llmd.ai/engine-cache: ${CLAIM}
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 100%
  template:
    metadata:
      labels:
        app.kubernetes.io/name: node-local-engine-cache
        app.kubernetes.io/component: node-local-engine-cache
        app.kubernetes.io/managed-by: wva-enginecache
        wva.llmd.ai/engine-cache: ${CLAIM}
    spec:
      serviceAccountName: ${SA_NAME}
      automountServiceAccountToken: false
      securityContext:
        seccompProfile:
          type: RuntimeDefault
$(accelerator_placement_yaml "$NODE_SELECTOR")
      tolerations:
$(for t in "${TOLERATIONS[@]}"; do printf '        - key: "%s"\n          operator: Exists\n' "$t"; done)
      terminationGracePeriodSeconds: 1
      volumes:
        - name: engine-cache
          persistentVolumeClaim:
            claimName: ${CLAIM}
      containers:
        - name: prepare
          image: "${IMAGE}"
          imagePullPolicy: IfNotPresent
          # One directory per cache, writable by any UID the engines run as
          # (1777: sticky, so one UID's files are not another's to remove --
          # and world-writable, so whoever can write here writes what every
          # engine on the node will load; see the header), then a marker,
          # then the pod stays, Ready, so the DaemonSet's numberReady is the
          # count of prepared nodes. The preparer creates the three
          # directories itself, so it owns them and the chmod takes, on
          # Kubernetes as root and on OpenShift as the project UID inside
          # the directory the admin prepared.
          command: ["/bin/sh", "-c"]
          args:
            - |
$(preparer_script | sed 's/^/              /')
          env:
            - name: MARKER
              value: "${MARKER}"
            # Engine images bake in NVIDIA_VISIBLE_DEVICES=all, and the NVIDIA
            # runtime honours it from a container that requested no GPU.
            - name: NVIDIA_VISIBLE_DEVICES
              value: "void"
          volumeMounts:
            - name: engine-cache
              mountPath: /engine-cache
          # Ready is "prepared, and writable by this UID": what the engines
          # will find, as the UID they run as.
          readinessProbe:
            exec:
              command: ["/bin/sh", "-c", "test -f /engine-cache/${MARKER} && test -w /engine-cache/vllm"]
            periodSeconds: 10
          resources:
            requests:
              cpu: 5m
              memory: 16Mi
            limits:
              memory: 64Mi
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
EOF
}

cmd_apply() {
    [ -n "$NAMESPACE" ] || log_error "apply needs -n NAMESPACE"
    [ -n "$NODE_PATH" ] || log_error "apply needs --path DIR (a directory on the node)"
    [ -n "$IMAGE" ] || log_error "apply needs --image IMG (an image with /bin/sh; the engine image)"
    check_path "$NODE_PATH"
    check_image "$IMAGE"
    if [ "$DRY_RUN" = true ]; then
        render
        return
    fi
    # Four documents in one apply: without leave for the cluster-scoped
    # volume, kubectl would still create the rest -- a Pending claim and
    # pods that never schedule -- and report only the volume's Forbidden.
    if ! kubectl auth can-i create persistentvolumes -A -q 2>/dev/null; then
        log_error "apply needs leave to create PersistentVolumes (cluster-scoped), which a namespace tenant does not have -- ask the cluster admin to run apply (storage-admin on OpenShift)"
    fi
    # A claim of this name on another class (or on none) is somebody else's:
    # the volume here would never bind to it, and the engines would go on
    # using it. And a claim's volume is immutable: a second apply with a new
    # --path would create a second volume, land the DaemonSet with the new
    # directory in its annotation, and leave the claim bound to the old one
    # -- status would then report a directory the engines do not mount.
    local existing
    existing="$(kubectl get pvc -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.metadata.name}/{.spec.storageClassName}' 2>/dev/null || true)"
    if [ -n "$existing" ] && [ "${existing#*/}" != "$STORAGE_CLASS" ]; then
        log_error "namespace ${NAMESPACE} already has a claim named ${CLAIM} on storage class '${existing#*/}', which this script did not make; delete it or use a namespace without one"
    fi
    # The directory the claim is really on is its volume's, not the
    # DaemonSet's annotation (the DaemonSet may be gone; the claim stays bound).
    local volume applied
    volume="$(kubectl get pvc -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)"
    applied=""
    [ -z "$volume" ] || applied="$(kubectl get pv "$volume" -o jsonpath='{.spec.hostPath.path}' 2>/dev/null || true)"
    if [ -n "$applied" ] && [ "$applied" != "${NODE_PATH}/${SUBDIR}" ]; then
        log_error "namespace ${NAMESPACE} already has its engine cache at ${applied} (volume ${volume}); the claim's volume cannot change. enginecache.sh delete -n ${NAMESPACE} first (the caches on the nodes stay), then apply with the new --path"
    fi
    render | kubectl apply -f - >/dev/null
    log_info "preparing ${NODE_PATH}/${SUBDIR} on $(accelerator_selector_text "$NODE_SELECTOR"); claim ${CLAIM} -- mount it read-write and point VLLM_CACHE_ROOT, FLASHINFER_WORKSPACE_DIR and TRITON_CACHE_DIR under it"
    if kubectl api-resources --api-group=security.openshift.io 2>/dev/null | grep -q securitycontextconstraints; then
        log_warning "OpenShift: the preparer and the engines run as the project UID (GID 0) under restricted-v2 and the kubelet does not relabel a hostPath; on each node, before this takes: mkdir -p ${NODE_PATH}/${SUBDIR} && chgrp 0 ${NODE_PATH}/${SUBDIR} && chmod 2775 ${NODE_PATH}/${SUBDIR} && chcon -t container_file_t ${NODE_PATH}/${SUBDIR} (oc debug node/<n> -- chroot /host ...), or a MachineConfig mount with context=system_u:object_r:container_file_t:s0. Not yet run on OpenShift"
    fi
    if ! command -v jq >/dev/null 2>&1; then
        log_warning "jq is not installed; skipping the per-node report (enginecache.sh status needs it)"
        return 0
    fi
    cmd_status report || true
}

# cmd_status prints, per node, whether the node is prepared. Ready is the
# marker present and the directory writable by the preparer's UID; a pod
# that keeps failing shows its reason, and no pod at all is a node the
# DaemonSet did not reach. Exits non-zero while any selected node is not
# prepared or the claim is not Bound.
cmd_status() {
    local mode="${1:-verdict}"
    command -v jq >/dev/null 2>&1 || log_error "jq is required for status (the node and pod lists are read with it)"
    [ -n "$NAMESPACE" ] || log_error "status needs -n NAMESPACE"
    local rc=0 ready=0 holders=0 selector="$NODE_SELECTOR" path
    path="$(kubectl get daemonset -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.metadata.annotations.wva\.llmd\.ai/engine-cache-path}' 2>/dev/null || true)"
    if [ -z "$path" ]; then
        if [ "$mode" = report ]; then
            log_warning "no engine-cache DaemonSet in ${NAMESPACE}"
            return 1
        fi
        log_error "no engine-cache DaemonSet in ${NAMESPACE} (enginecache.sh apply makes one)"
    fi
    if [ "$NODE_SELECTOR_GIVEN" = false ]; then
        # Off the live DaemonSet's pod template, held to the same rule as
        # the flag before it reaches kubectl (a tenant can edit the object).
        selector="$(kubectl get daemonset -n "$NAMESPACE" "$CLAIM" \
            -o jsonpath='{.spec.template.spec.nodeSelector}' 2>/dev/null \
            | jq -r 'to_entries | map(.key + "=" + .value) | first // ""' 2>/dev/null || true)"
        local why
        if ! why="$(accelerator_selector_ok "$selector")"; then
            [ "$mode" = report ] || log_error "DaemonSet ${CLAIM} carries a nodeSelector this script did not write (${why}); pass --node-selector to report on it"
            log_warning "DaemonSet ${CLAIM} carries a nodeSelector this script did not write (${why}); pass --node-selector to report on it"
            return 1
        fi
    fi
    local nodes_json err_file
    err_file="$(mktemp)"
    if ! nodes_json="$(accelerator_nodes_json "$selector" 2>"$err_file")"; then
        local why
        if grep -q Forbidden "$err_file"; then
            why="a namespace tenant may not list nodes; this needs cluster-scoped node read (cluster-reader) -- ask for it or check with the cluster admin"
        else
            why="$(tr '\n' ' ' < "$err_file")"
        fi
        rm -f "$err_file"
        [ "$mode" = report ] || log_error "cannot list nodes: ${why}"
        log_warning "cannot list nodes: ${why}"
        return 1
    fi
    rm -f "$err_file"
    local node_count
    node_count="$(printf '%s' "$nodes_json" | jq '.items | length')"
    if [ "$node_count" -eq 0 ]; then
        [ "$mode" = report ] || log_error "no node matches $(accelerator_selector_text "$selector") (--node-selector picks the nodes)"
        log_warning "no node matches $(accelerator_selector_text "$selector"); the cache is prepared nowhere (--node-selector picks the nodes)"
        return 1
    fi
    local pods_file bound
    pods_file="$(mktemp)"
    kubectl get pods -n "$NAMESPACE" -l "$LABEL_COMPONENT" -o json > "$pods_file"
    bound="$(kubectl get pvc -n "$NAMESPACE" "$CLAIM" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    echo "${path}  (claim ${CLAIM}: ${bound:-missing}; nodes: $(accelerator_selector_text "$selector"))"
    if [ "$bound" != Bound ]; then
        rc=1
        [ -z "$bound" ] || echo "  the claim is not bound: its volume is cluster-scoped and was not created -- apply needs leave to create PersistentVolumes (ask the cluster admin to run apply)"
    fi
    printf '  %-28s %-12s %s\n' NODE CACHE PREPARER
    local eacces=0
    # The unit separator keeps empty fields; a tab in IFS would swallow them.
    while IFS=$'\x1f' read -r node phase reason podname message; do
        local has=absent
        [ "$phase" = "no pod" ] || holders=$((holders + 1))
        if [ "$phase" = Ready ]; then
            has=prepared
            ready=$((ready + 1))
        else
            rc=1
        fi
        printf '  %-28s %-12s %s %s\n' "$node" "$has" "$phase" "$reason"
        # the reason carries the last exit after it ("CrashLoopBackOff (last exit 1 Error)")
        case "$reason" in
            CrashLoopBackOff*|Error*)
                if kubectl logs -n "$NAMESPACE" "$podname" --tail=20 2>/dev/null | grep -q 'Permission denied\|Read-only file system'; then
                    echo "    the preparer cannot write ${path}: $(kubectl logs -n "$NAMESPACE" "$podname" --tail=20 2>/dev/null | grep -o 'Permission denied\|Read-only file system' | head -1) in its log"
                    eacces=$((eacces + 1))
                fi ;;
            CreateContainerConfigError) [ -z "$message" ] || echo "    ${message}" ;;
        esac
    done < <(printf '%s' "$nodes_json" | jq -r --arg ds "$CLAIM" --slurpfile pods "$pods_file" \
        '.items[] as $node
         | ($pods[0].items | map(select(.metadata.labels["wva.llmd.ai/engine-cache"] == $ds
              and (.spec.nodeName == $node.metadata.name
                   or ((.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms // [])[0].matchFields // [])[0].values[0]? == $node.metadata.name))) | first) as $pod
         | [ $node.metadata.name,
             (if $pod == null then "no pod"
              elif $pod.status.phase == "Running" and (($pod.status.containerStatuses // []) | map(.ready) | all) then "Ready"
              elif $pod.status.phase == "Running" then "Running (not ready)"
              else $pod.status.phase + " (not ready)" end),
             (if $pod == null then ""
              else ($pod.status.reason // ((($pod.status.containerStatuses // [])[0].state // {}) | to_entries | (.[0].value.reason // "")))
                   + ((($pod.status.containerStatuses // [])[0].lastState.terminated) | if . then " (last exit " + (.exitCode | tostring) + (if .reason then " " + .reason else "" end) + ")" else "" end) end),
             ($pod.metadata.name // ""),
             (if $pod == null then "" else (((($pod.status.containerStatuses // [])[0].state // {}) | to_entries | (.[0].value.message // "")) | gsub("[\\t\\n]"; " ")) end)
           ] | join("")')
    rm -f "$pods_file"
    if [ "$eacces" -gt 0 ]; then
        echo "  ${eacces} node(s): the node directory is not writable by the preparer. On Kubernetes the preparer is root and the kubelet creates the directory root-owned, so this is a mount that is read-only or not there, or a directory that already existed with another owner (ls -ld on the node); on OpenShift it runs as the project UID (GID 0) under restricted-v2 and the directory is not relabelled -- on each node: mkdir -p DIR && chgrp 0 DIR && chmod 2775 DIR && chcon -t container_file_t DIR, or a MachineConfig mount with context=...:container_file_t:s0 (enginecache.sh --help)"
    fi
    echo "  ${ready}/${node_count} nodes prepared; $((node_count - ready)) not"
    [ -n "$selector" ] || printf '%s' "$nodes_json" | accelerator_vendor_warning
    if [ "$holders" -eq 0 ]; then
        local why
        why="$(kubectl get events -n "$NAMESPACE" \
            --field-selector "involvedObject.kind=DaemonSet,involvedObject.name=${CLAIM},reason=FailedCreate" \
            -o jsonpath='{.items[-1:].message}' 2>/dev/null || true)"
        [ -n "$why" ] && echo "  the DaemonSet cannot create its pods: ${why}"
    fi
    return $rc
}

run_delete() {
    if [ "$DRY_RUN" = true ]; then
        echo "would run: kubectl $*"
    else
        kubectl "$@"
    fi
}

# cmd_delete removes the preparer, the claim, the volume and the
# ServiceAccount. The caches on the nodes stay: the next apply finds them
# warm. The volume is found by this script's labels and its claimRef into
# this namespace, never by a name read off the claim (a tenant's object).
cmd_delete() {
    [ -n "$NAMESPACE" ] || log_error "delete needs -n NAMESPACE"
    command -v jq >/dev/null 2>&1 || log_error "jq is required for delete (the volume is matched on its claimRef with it)"
    local volumes=() line
    while IFS= read -r line; do
        [ -n "$line" ] && volumes+=("$line")
    done < <(kubectl get pv -l "$LABEL_COMPONENT" -o json 2>/dev/null \
        | jq -r --arg ns "$NAMESPACE" --arg claim "$CLAIM" \
            '.items[] | select(.spec.claimRef.namespace == $ns and .spec.claimRef.name == $claim) | .metadata.name' \
        2>/dev/null || true)
    run_delete delete daemonset -n "$NAMESPACE" "$CLAIM" --ignore-not-found
    run_delete delete pvc -n "$NAMESPACE" "$CLAIM" --ignore-not-found
    [ "${#volumes[@]}" -eq 0 ] || run_delete delete pv "${volumes[@]}" --ignore-not-found
    run_delete delete serviceaccount -n "$NAMESPACE" "$SA_NAME" --ignore-not-found
    return 0
}

CMD="${1:-}"
[ -n "$CMD" ] || usage
shift
case "$CMD" in
    -h|--help|help) usage ;;
    apply|status|delete) ;;
    *) log_error "unknown command: ${CMD} (apply | status | delete)" ;;
esac

while [ $# -gt 0 ]; do
    case "$1" in
        -n|--namespace|--path|--image|--capacity|--node-selector|--toleration)
            [ $# -ge 2 ] || log_error "$1 needs a value" ;;
    esac
    case "$1" in
        -n|--namespace) NAMESPACE="$2"; shift 2 ;;
        --path) NODE_PATH="$2"; shift 2 ;;
        --image) IMAGE="$2"; shift 2 ;;
        --capacity) CAPACITY="$2"; shift 2 ;;
        --node-selector) NODE_SELECTOR="$2"; NODE_SELECTOR_GIVEN=true; shift 2 ;;
        --toleration) accelerator_check_toleration "$2"; TOLERATIONS+=("$2"); shift 2 ;;
        --dry-run) [ "$CMD" != status ] || log_error "--dry-run applies to apply and delete, not status"; DRY_RUN=true; shift ;;
        -h|--help) usage ;;
        *) log_error "unknown option: $1" ;;
    esac
done
accelerator_check_selector "$NODE_SELECTOR"
case "$CAPACITY" in
    ""|*[!0-9A-Za-z.]*) log_error "--capacity must be a Kubernetes quantity such as 200Gi, got '${CAPACITY}'" ;;
esac

"cmd_${CMD}"
