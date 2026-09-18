#!/usr/bin/env bash
# Keep a model's weights on every accelerator node's local disk, so a replica
# reads them at the node's own speed instead of a shared volume's.
#
# The shared RWX volume that llm-d deployments mount the model from is a
# download cache, not a start-time measure: every replica reads the whole
# model through the network filesystem, and two replicas starting together
# share that pipe. Measured on one cluster, two nodes reading one 700 GB
# model from the shared volume took 1403 s against ~104 s from the node's
# NVMe -- past the engine's own 600 s startup timeout. This puts the weights
# on the node: one static hostPath PersistentVolume, one claim bound to it,
# and one DaemonSet that downloads the model onto every selected node and
# then reports Ready. The model servers mount the claim exactly as they
# mount any other; each node reads its own copy.
#
#   weights.sh apply  -n NS --model HFID --path DIR --image IMG [--dry-run]   download HFID under DIR on every accelerator node
#   weights.sh status -n NS [--model HFID] [--node-selector KEY=VALUE]        which nodes have it, which are still downloading
#   weights.sh delete -n NS (--model HFID | --all) [--dry-run]                drop the claim, volume and downloader (files stay)
#
# Options:
#   --model HFID                Hugging Face id, e.g. Qwen/Qwen3-32B
#   --path DIR                  directory on the node, e.g. /mnt/local/models
#                               (/var/mnt/models on RHCOS, where /mnt is the
#                               root disk); the model lands under DIR/models/HFID
#   --image IMG                 image to download with: any image carrying
#                               huggingface_hub -- the engine image itself
#                               is the natural choice, and it is then held
#                               on the node as a side effect
#   --hf-token-secret NAME[/KEY]  Secret with a Hugging Face token for gated
#                               models (key defaults to HF_TOKEN)
#   --capacity SIZE             the volume's declared capacity (default 1Ti;
#                               it is a declaration, hostPath has no quota)
#   --node-selector KEY=VALUE   which nodes count as accelerator nodes. The
#                               default is any node carrying a known GPU
#                               product label (deploy/lib/accelerator_nodes.sh);
#                               set this to what your model servers select
#                               on when they select on something. `status`
#                               lists the nodes each DaemonSet was applied
#                               for; this overrides that
#   --toleration KEY            tolerate a taint with KEY (any value, any
#                               effect); nvidia.com/gpu is always tolerated
#   --dry-run                   apply: print the manifests instead of
#                               applying them; delete: print what would go
#
# The claim is named weights-<model>-<hash> and printed by apply; a model
# server mounts the weights at pvc://<claim>/models/HFID. The volume is
# cluster-scoped: apply needs leave to create PersistentVolumes, which a
# namespace tenant does not have -- ask the cluster admin to run apply, or
# to create the volume. Nothing here uses a hostPath volume in a Pod (the
# downloader mounts the claim), so Pod Security "baseline" admits it and
# "restricted" does not (no runAsNonRoot: on Kubernetes the image runs as
# root, which is what writing a root-owned node directory takes). The node
# directory is cluster-shared, root-writable state: every namespace pointed
# at the same DIR shares it and trusts its marker, and nothing charges what
# is written there to a quota (the claim's --capacity IS charged to a
# storage quota) -- one DIR per trust domain, on a disk that is not the
# node's own. The downloader runs as its own ServiceAccount,
# weights-downloader, so anything granted for it is granted to it alone.
#
# On OpenShift -- NOT YET RUN THERE -- restricted-v2 admits the downloader
# and runs it as the project's range UID with GID 0, which cannot write a
# root-owned directory, and the kubelet does not relabel a hostPath, so
# container_t cannot write it either way. Before apply, on each node (oc
# debug node/<n> -- chroot /host): mkdir -p DIR && chgrp 0 DIR && chmod
# 2775 DIR && chcon -t container_file_t DIR; or mount the disk at DIR with
# a MachineConfig mount unit carrying context=system_u:object_r:
# container_file_t:s0. No SCC grant is then needed. `status` says when a
# downloader died on a permission error. Granting anyuid to
# weights-downloader is the other way, and it needs an SCC that also
# allows the runtime/default seccomp profile (stock anyuid does not).
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
MODEL=""
NODE_PATH=""
IMAGE=""
HF_SECRET=""
CAPACITY="1Ti"
NODE_SELECTOR=""          # empty: every node carrying a known GPU product label
NODE_SELECTOR_GIVEN=false
TOLERATIONS=("nvidia.com/gpu")
DRY_RUN=false
ALL=false
# Both labels: component alone is a generic value another tool could carry;
# managed-by is what says this script made it.
LABEL_COMPONENT="app.kubernetes.io/component=node-local-weights,app.kubernetes.io/managed-by=wva-weights"
STORAGE_CLASS="node-local-weights"
MARKER=".download-complete"
SA_NAME="weights-downloader"

usage() {
    sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; $d'
    exit 0
}

# name_for turns a model id into the claim's (and DaemonSet's) name: the id
# lower-cased with everything that is not [a-z0-9-] replaced, truncated to
# leave room for a hash of the FULL id -- two ids that differ only in a
# character the scrub folds must not collide.
name_for() {
    local model="$1"
    local base hash
    base="$(printf '%s' "$model" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9-]/-/g; s/^-*//; s/-*$//')"
    hash="$(wva_ns_suffix "$model")"
    printf 'weights-%s-%s' "${base:0:40}" "$hash" | sed 's/-\{2,\}/-/g'
}

# pv_for is the volume's name. PersistentVolumes are cluster-scoped, so the
# namespace is part of the hash: two namespaces holding the same model under
# the same directory each get their own volume, bound to their own claim.
pv_for() {
    printf '%s-%s' "$(name_for "$MODEL")" "$(wva_ns_suffix "${NAMESPACE}/${NODE_PATH}")"
}

# check_model refuses an id that is not one: [org/]name of the characters
# Hugging Face allows, no '..' or '--', no segment starting or ending with
# '.' or '-' -- the Hub's own rule, held here too because the id becomes a
# directory (/weights/models/<id>) and the probe's path before the library
# ever sees it.
check_model() {
    case "$1" in
        "") log_error "--model must not be empty" ;;
        */*/*|/*|*/) log_error "not a Hugging Face model id: '$1' (expected org/name)" ;;
        *[!A-Za-z0-9._/-]*) log_error "not a Hugging Face model id: '$1' (letters, digits, . _ - and one /)" ;;
        *..*|*--*) log_error "not a Hugging Face model id: '$1' (no '..' or '--')" ;;
        .*|-*|*/.*|*/-*|*.|*-|*./*|*-/*) log_error "not a Hugging Face model id: '$1' (a segment must not start or end with '.' or '-')" ;;
    esac
}

# check_path refuses a node directory the volume must not point at
# (deploy/lib/nodedir.sh says which and why).
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

# render prints the volume, the claim and the downloader for MODEL.
render() {
    local name pv
    name="$(name_for "$MODEL")"
    pv="$(pv_for)"
    local secret_name="${HF_SECRET%%/*}"
    local secret_key="${HF_SECRET#*/}"
    [ "$secret_key" != "$HF_SECRET" ] || secret_key="HF_TOKEN"
    cat <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${SA_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-weights
    app.kubernetes.io/component: node-local-weights
    app.kubernetes.io/managed-by: wva-weights
# The downloader's own identity: what an admin grants for it (an SCC on
# OpenShift) is granted to it alone, not to every pod that names no
# ServiceAccount. It makes no API call, so no token.
automountServiceAccountToken: false
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${pv}
  labels:
    app.kubernetes.io/name: node-local-weights
    app.kubernetes.io/component: node-local-weights
    app.kubernetes.io/managed-by: wva-weights
  annotations:
    wva.llmd.ai/weights-model: "${MODEL}"
    wva.llmd.ai/weights-path: "${NODE_PATH}"
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
    name: ${name}
  hostPath:
    path: ${NODE_PATH}
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-weights
    app.kubernetes.io/component: node-local-weights
    app.kubernetes.io/managed-by: wva-weights
  annotations:
    wva.llmd.ai/weights-model: "${MODEL}"
    wva.llmd.ai/weights-path: "${NODE_PATH}"
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
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: node-local-weights
    app.kubernetes.io/component: node-local-weights
    app.kubernetes.io/managed-by: wva-weights
  annotations:
    wva.llmd.ai/weights-model: "${MODEL}"
    wva.llmd.ai/weights-path: "${NODE_PATH}"
    wva.llmd.ai/weights-node-selector: "${NODE_SELECTOR}"
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: node-local-weights
      wva.llmd.ai/weights: ${name}
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 100%
  template:
    metadata:
      labels:
        app.kubernetes.io/name: node-local-weights
        app.kubernetes.io/component: node-local-weights
        app.kubernetes.io/managed-by: wva-weights
        wva.llmd.ai/weights: ${name}
      annotations:
        wva.llmd.ai/weights-model: "${MODEL}"
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
        - name: weights
          persistentVolumeClaim:
            claimName: ${name}
      containers:
        - name: download
          image: "${IMAGE}"
          imagePullPolicy: IfNotPresent
          # The marker is written after the last file; a restart resumes what
          # huggingface_hub left (it skips complete files). Then the pod
          # stays, Ready, so the DaemonSet's numberReady is the count of
          # nodes that hold the model. No command substitution and no
          # double dollar in the command: Kubernetes rewrites both.
          command: ["/bin/sh", "-c"]
          args:
            - |
              python3 - <<'PY' || exit 1
              import os, sys
              from huggingface_hub import snapshot_download
              d = os.environ["TARGET_DIR"]
              m = os.path.join(d, os.environ["MARKER"])
              if os.path.exists(m):
                  print("weights: present at", d, flush=True)
              else:
                  print("weights: downloading", os.environ["MODEL_ID"], "to", d, flush=True)
                  snapshot_download(repo_id=os.environ["MODEL_ID"], local_dir=d,
                                    token=os.environ.get("HF_TOKEN") or None,
                                    ignore_patterns=["original/*", "*.msgpack", "*.h5"],
                                    max_workers=8)
                  open(m, "w").close()
                  print("weights: complete", flush=True)
              PY
              trap 'exit 0' TERM; while :; do sleep 3600 & wait; done
          env:
            - name: MODEL_ID
              value: "${MODEL}"
            - name: TARGET_DIR
              value: "/weights/models/${MODEL}"
            - name: MARKER
              value: "${MARKER}"
            - name: HOME
              value: /tmp
            # Engine images bake in NVIDIA_VISIBLE_DEVICES=all, and the NVIDIA
            # runtime honours it from a container that requested no GPU.
            - name: NVIDIA_VISIBLE_DEVICES
              value: "void"
$(if [ -n "$HF_SECRET" ]; then printf '            - name: HF_TOKEN\n              valueFrom:\n                secretKeyRef:\n                  name: "%s"\n                  key: "%s"\n' "$secret_name" "$secret_key"; fi)
          volumeMounts:
            - name: weights
              mountPath: /weights
          # Ready is "the marker exists": the download finished on this node.
          readinessProbe:
            exec:
              command: ["test", "-f", "/weights/models/${MODEL}/${MARKER}"]
            periodSeconds: 10
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
            limits:
              memory: 2Gi
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
EOF
}

cmd_apply() {
    [ -n "$NAMESPACE" ] || log_error "apply needs -n NAMESPACE"
    [ -n "$MODEL" ] || log_error "apply needs --model HFID"
    [ -n "$NODE_PATH" ] || log_error "apply needs --path DIR (a directory on the node)"
    [ -n "$IMAGE" ] || log_error "apply needs --image IMG (an image with huggingface_hub; the engine image)"
    check_model "$MODEL"
    check_path "$NODE_PATH"
    check_image "$IMAGE"
    if [ -n "$HF_SECRET" ]; then
        case "$HF_SECRET" in *[!A-Za-z0-9._/-]*|*/*/*) log_error "not a Secret reference: '$HF_SECRET' (NAME or NAME/KEY)" ;; esac
    fi
    if [ "$DRY_RUN" = true ]; then
        render
        return
    fi
    render | kubectl apply -f - >/dev/null
    log_info "downloading ${MODEL} under ${NODE_PATH} on $(accelerator_selector_text "$NODE_SELECTOR"); claim $(name_for "$MODEL") -- mount pvc://$(name_for "$MODEL")/models/${MODEL}"
    # On OpenShift the downloader runs as the project's range UID and the
    # directory is not relabelled for containers: both are the admin's to
    # prepare on each node before this apply, and neither shows until the
    # first pod dies on it. Said once, here, when the cluster is one.
    if kubectl api-resources --api-group=security.openshift.io 2>/dev/null | grep -q securitycontextconstraints; then
        log_warning "OpenShift: the downloader runs as the project UID (GID 0) under restricted-v2 and the kubelet does not relabel a hostPath; on each node, before this takes: mkdir -p ${NODE_PATH} && chgrp 0 ${NODE_PATH} && chmod 2775 ${NODE_PATH} && chcon -t container_file_t ${NODE_PATH} (oc debug node/<n> -- chroot /host ...), or a MachineConfig mount at ${NODE_PATH} with context=system_u:object_r:container_file_t:s0. weights.sh --help has the rest; not yet run on OpenShift"
    fi
    # The report after an apply is not a verdict: the download just started.
    # Its exits (no node matches, a node list the caller may not read) are
    # warnings in report mode, and jq is checked here because log_error
    # inside cmd_status ends the process past any `|| true`.
    if ! command -v jq >/dev/null 2>&1; then
        log_warning "jq is not installed; skipping the per-node report (weights.sh status needs it)"
        return 0
    fi
    cmd_status report || true
}

# cmd_status prints, per model and per node, whether the node holds the
# model. Ready is the download complete (the readinessProbe is the marker),
# a running pod that is not Ready is still downloading, a pod that keeps
# failing shows its reason, and no pod at all is a node the DaemonSet did
# not reach. The nodes are the ones the DaemonSet was applied for (its
# recorded selector) unless --node-selector overrides. Exits non-zero while
# any selected node lacks the model or the claim is not Bound.
cmd_status() {
    local mode="${1:-verdict}"
    command -v jq >/dev/null 2>&1 || log_error "jq is required for status (the node and pod lists are read with it)"
    [ -n "$NAMESPACE" ] || log_error "status needs -n NAMESPACE"
    local models=()
    if [ -n "$MODEL" ]; then
        check_model "$MODEL"
        models=("$MODEL")
    else
        local line
        while IFS= read -r line; do
            [ -n "$line" ] && models+=("$line")
        done < <(kubectl get daemonset -n "$NAMESPACE" -l "$LABEL_COMPONENT" \
            -o jsonpath='{range .items[*]}{.metadata.annotations.wva\.llmd\.ai/weights-model}{"\n"}{end}')
        [ "${#models[@]}" -gt 0 ] || log_error "no weights DaemonSets in ${NAMESPACE} and no --model given"
    fi
    local pods_file
    pods_file="$(mktemp)"
    kubectl get pods -n "$NAMESPACE" -l "$LABEL_COMPONENT" -o json > "$pods_file"
    local rc=0
    local model
    for model in "${models[@]}"; do
        local name ready holders selector
        name="$(name_for "$model")"
        ready=0
        holders=0
        selector="$NODE_SELECTOR"
        if [ "$NODE_SELECTOR_GIVEN" = false ]; then
            # Off the live DaemonSet's pod template: a nodeSelector is the
            # KEY=VALUE it was applied with, none is the affinity default.
            # An object a namespace tenant can edit, so held to the same rule
            # as the flag before it reaches kubectl.
            selector="$(kubectl get daemonset -n "$NAMESPACE" "$name" \
                -o jsonpath='{.spec.template.spec.nodeSelector}' 2>/dev/null \
                | jq -r 'to_entries | map(.key + "=" + .value) | first // ""' 2>/dev/null || true)"
            local why
            if ! why="$(accelerator_selector_ok "$selector")"; then
                if [ "$mode" = report ]; then
                    log_warning "DaemonSet ${name} carries a nodeSelector this script did not write (${why}); pass --node-selector to report on it"
                    rc=1
                    continue
                fi
                log_error "DaemonSet ${name} carries a nodeSelector this script did not write (${why}); pass --node-selector to report on it"
            fi
        fi
        local nodes_json err_file
        err_file="$(mktemp)"
        # Caught on the line (report mode runs under `|| true`); the reason
        # is kubectl's, and only a Forbidden is the permission story.
        if ! nodes_json="$(accelerator_nodes_json "$selector" 2>"$err_file")"; then
            local why
            if grep -q Forbidden "$err_file"; then
                why="a namespace tenant may not list nodes; this needs cluster-scoped node read (cluster-reader) -- ask for it or check with the cluster admin"
            else
                why="$(tr '\n' ' ' < "$err_file")"
            fi
            rm -f "$err_file"
            if [ "$mode" = report ]; then
                log_warning "cannot list nodes for ${model}: ${why}"
                rc=1
                continue
            fi
            log_error "cannot list nodes: ${why}"
        fi
        rm -f "$err_file"
        local node_count
        node_count="$(printf '%s' "$nodes_json" | jq '.items | length')"
        if [ "$node_count" -eq 0 ]; then
            if [ "$mode" = report ]; then
                log_warning "no node matches $(accelerator_selector_text "$selector"); ${model} will be downloaded nowhere (--node-selector picks the nodes)"
                rc=1
                continue
            fi
            log_error "no node matches $(accelerator_selector_text "$selector") (--node-selector picks the nodes)"
        fi
        local bound
        bound="$(kubectl get pvc -n "$NAMESPACE" "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
        echo "${model}  (claim ${name}: ${bound:-missing}; nodes: $(accelerator_selector_text "$selector"))"
        [ "$bound" = Bound ] || rc=1
        printf '  %-28s %-12s %s\n' NODE WEIGHTS DOWNLOADER
        local eacces=0
        while IFS=$'\t' read -r node phase reason podname message; do
            local has=absent
            [ "$phase" = "no pod" ] || holders=$((holders + 1))
            if [ "$phase" = Ready ]; then
                has=present
                ready=$((ready + 1))
            elif [ "$phase" = Downloading ]; then
                has=downloading
                rc=1
            else
                rc=1
            fi
            printf '  %-28s %-12s %s %s\n' "$node" "$has" "$phase" "$reason"
            # A downloader that keeps dying says why in its log, not in its
            # state: a permission error is the node directory (root-owned,
            # or not labelled for containers), which is the platform's, not
            # the namespace's.
            case "$reason" in
                CrashLoopBackOff|Error)
                    if kubectl logs -n "$NAMESPACE" "$podname" --tail=20 2>/dev/null | grep -q 'Permission denied\|PermissionError\|Errno 13'; then
                        echo "    the downloader cannot write ${NODE_PATH:-the node directory}: Permission denied in its log"
                        eacces=$((eacces + 1))
                    fi ;;
                CreateContainerConfigError) [ -z "$message" ] || echo "    ${message}" ;;
            esac
        done < <(printf '%s' "$nodes_json" | jq -r --arg ds "$name" --slurpfile pods "$pods_file" \
            '.items[] as $node
             | ($pods[0].items | map(select(.spec.nodeName == $node.metadata.name and .metadata.labels["wva.llmd.ai/weights"] == $ds)) | first) as $pod
             | [ $node.metadata.name,
                 (if $pod == null then "no pod"
                  elif $pod.status.phase == "Running" and (($pod.status.containerStatuses // []) | map(.ready) | all) then "Ready"
                  elif $pod.status.phase == "Running" and ((($pod.status.containerStatuses // [])[0].state // {}) | has("running")) then "Downloading"
                  else $pod.status.phase + " (not ready)" end),
                 (if $pod == null then ""
                  else ($pod.status.reason // ((($pod.status.containerStatuses // [])[0].state // {}) | to_entries | (.[0].value.reason // ""))) end),
                 ($pod.metadata.name // ""),
                 (if $pod == null then "" else (((($pod.status.containerStatuses // [])[0].state // {}) | to_entries | (.[0].value.message // "")) | gsub("[\\t\\n]"; " ")) end)
               ] | @tsv')
        if [ "$eacces" -gt 0 ]; then
            echo "  ${eacces} node(s): the node directory is not writable by the downloader. On Kubernetes the downloader is root and the kubelet creates the directory root-owned, so this is a mount that is read-only or not there; on OpenShift it runs as the project UID (GID 0) under restricted-v2 and the directory is not relabelled -- on each node: mkdir -p DIR && chgrp 0 DIR && chmod 2775 DIR && chcon -t container_file_t DIR, or a MachineConfig mount with context=...:container_file_t:s0 (weights.sh --help)"
        fi
        echo "  ${ready}/${node_count} nodes hold it; $((node_count - ready)) do not"
        [ -n "$selector" ] || printf '%s' "$nodes_json" | accelerator_vendor_warning
        if [ "$holders" -eq 0 ]; then
            local why
            why="$(kubectl get events -n "$NAMESPACE" \
                --field-selector "involvedObject.kind=DaemonSet,involvedObject.name=${name},reason=FailedCreate" \
                -o jsonpath='{.items[-1:].message}' 2>/dev/null || true)"
            [ -n "$why" ] && echo "  the DaemonSet cannot create its pods: ${why}"
        fi
    done
    rm -f "$pods_file"
    return $rc
}

# run_delete runs a kubectl delete, or under --dry-run prints it.
run_delete() {
    if [ "$DRY_RUN" = true ]; then
        echo "would run: kubectl $*"
    else
        kubectl "$@"
    fi
}

# cmd_delete removes the downloader, the claim and the volume. The files on
# the nodes stay: they are the point, and the next apply finds the marker
# and skips the download.
#
# The volumes to delete are found by what only this script (with volume
# write rights) can set -- the labels on the PersistentVolume and its
# claimRef into this namespace -- never by a name read off a claim: a claim
# is a tenant's object, and a tenant who writes a foreign volume's name
# into one must not have the admin's delete remove that volume.
cmd_delete() {
    [ -n "$NAMESPACE" ] || log_error "delete needs -n NAMESPACE"
    command -v jq >/dev/null 2>&1 || log_error "jq is required for delete (the volumes are matched on their claimRef with it)"
    local selector=() claim=""
    if [ "$ALL" = true ]; then
        selector=(-l "$LABEL_COMPONENT")
    else
        [ -n "$MODEL" ] || log_error "delete needs --model HFID or --all"
        check_model "$MODEL"
        claim="$(name_for "$MODEL")"
        selector=("$claim")
    fi
    local volumes=()
    local line
    while IFS= read -r line; do
        [ -n "$line" ] && volumes+=("$line")
    done < <(kubectl get pv -l "$LABEL_COMPONENT" -o json 2>/dev/null \
        | jq -r --arg ns "$NAMESPACE" --arg claim "$claim" \
            '.items[] | select(.spec.claimRef.namespace == $ns and ($claim == "" or .spec.claimRef.name == $claim)) | .metadata.name' \
        2>/dev/null || true)
    run_delete delete daemonset -n "$NAMESPACE" "${selector[@]}" --ignore-not-found
    run_delete delete pvc -n "$NAMESPACE" "${selector[@]}" --ignore-not-found
    [ "${#volumes[@]}" -eq 0 ] || run_delete delete pv "${volumes[@]}" --ignore-not-found
    # the ServiceAccount is shared by every model's downloader; it goes with --all
    [ "$ALL" = true ] && run_delete delete serviceaccount -n "$NAMESPACE" "$SA_NAME" --ignore-not-found
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
        -n|--namespace|--model|--path|--image|--hf-token-secret|--capacity|--node-selector|--toleration)
            [ $# -ge 2 ] || log_error "$1 needs a value" ;;
    esac
    case "$1" in
        -n|--namespace) NAMESPACE="$2"; shift 2 ;;
        --model) MODEL="$2"; shift 2 ;;
        --path) NODE_PATH="$2"; shift 2 ;;
        --image) IMAGE="$2"; shift 2 ;;
        --hf-token-secret) HF_SECRET="$2"; shift 2 ;;
        --capacity) CAPACITY="$2"; shift 2 ;;
        --node-selector) NODE_SELECTOR="$2"; NODE_SELECTOR_GIVEN=true; shift 2 ;;
        --toleration) accelerator_check_toleration "$2"; TOLERATIONS+=("$2"); shift 2 ;;
        --dry-run) [ "$CMD" != status ] || log_error "--dry-run applies to apply and delete, not status"; DRY_RUN=true; shift ;;
        --all) [ "$CMD" = delete ] || log_error "--all is a delete option"; ALL=true; shift ;;
        -h|--help) usage ;;
        *) log_error "unknown option: $1" ;;
    esac
done
accelerator_check_selector "$NODE_SELECTOR"
case "$CAPACITY" in
    ""|*[!0-9A-Za-z.]*) log_error "--capacity must be a Kubernetes quantity such as 1Ti, got '${CAPACITY}'" ;;
esac

"cmd_${CMD}"
