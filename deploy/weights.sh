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
#   weights.sh status -n NS [--model HFID]                                    which nodes have it, which are still downloading
#   weights.sh delete -n NS (--model HFID | --all) [--dry-run]                drop the claim, volume and downloader (files stay)
#
# Options:
#   --model HFID                Hugging Face id, e.g. Qwen/Qwen3-32B
#   --path DIR                  directory on the node, e.g. /mnt/local/models;
#                               the model lands under DIR/models/HFID
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
#                               on when they select on something
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
# downloader mounts the claim), so Pod Security "baseline" admits it;
# "restricted" does not (the image runs as root), and on OpenShift the
# downloader needs the anyuid SCC -- the node directory is root-owned, and
# restricted-v2 would run it as another UID that cannot write there.
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

# check_model refuses an id that is not one: [org/]name, the characters
# Hugging Face allows. It is substituted into YAML, a path and a Python
# string.
check_model() {
    case "$1" in
        "") log_error "--model must not be empty" ;;
        */*/*|/*|*/) log_error "not a Hugging Face model id: '$1' (expected org/name)" ;;
        *[!A-Za-z0-9._/-]*) log_error "not a Hugging Face model id: '$1' (letters, digits, . _ - and one /)" ;;
    esac
}

# check_path refuses anything but an absolute path made of the characters a
# hostPath and a YAML scalar take unquoted.
check_path() {
    case "$1" in
        "") log_error "--path must not be empty" ;;
        /) log_error "--path must not be the root directory" ;;
        *[!A-Za-z0-9._/-]*) log_error "not a node path: '$1' (absolute, letters, digits, . _ - /)" ;;
        /*) ;;
        *) log_error "--path must be absolute: '$1'" ;;
    esac
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
            selector="$(kubectl get daemonset -n "$NAMESPACE" "$name" \
                -o jsonpath='{.metadata.annotations.wva\.llmd\.ai/weights-node-selector}' 2>/dev/null || true)"
        fi
        local nodes_json
        if ! nodes_json="$(accelerator_nodes_json "$selector")"; then
            if [ "$mode" = report ]; then
                log_warning "cannot list nodes (a namespace tenant may not); the download is running, but this report needs cluster-scoped node read -- ask for cluster-reader or check with the cluster admin"
                rm -f "$pods_file"
                return 1
            fi
            log_error "cannot list nodes: status needs cluster-scoped node read (cluster-reader), which a namespace tenant does not have"
        fi
        local node_count
        node_count="$(printf '%s' "$nodes_json" | jq '.items | length')"
        if [ "$node_count" -eq 0 ]; then
            if [ "$mode" = report ]; then
                log_warning "no node matches $(accelerator_selector_text "$selector"); the downloader will run nowhere (--node-selector picks the nodes)"
                rm -f "$pods_file"
                return 1
            fi
            log_error "no node matches $(accelerator_selector_text "$selector") (--node-selector picks the nodes)"
        fi
        local bound
        bound="$(kubectl get pvc -n "$NAMESPACE" "$name" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
        echo "${model}  (claim ${name}: ${bound:-missing}; nodes: $(accelerator_selector_text "$selector"))"
        [ "$bound" = Bound ] || rc=1
        printf '  %-28s %-12s %s\n' NODE WEIGHTS DOWNLOADER
        while IFS=$'\t' read -r node phase reason; do
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
        done < <(printf '%s' "$nodes_json" | jq -r --arg ds "$name" --slurpfile pods "$pods_file" \
            '.items[] as $node
             | ($pods[0].items | map(select(.spec.nodeName == $node.metadata.name and .metadata.labels["wva.llmd.ai/weights"] == $ds)) | first) as $pod
             | [ $node.metadata.name,
                 (if $pod == null then "no pod"
                  elif $pod.status.phase == "Running" and (($pod.status.containerStatuses // []) | map(.ready) | all) then "Ready"
                  elif $pod.status.phase == "Running" and ((($pod.status.containerStatuses // [])[0].state // {}) | has("running")) then "Downloading"
                  else $pod.status.phase + " (not ready)" end),
                 (if $pod == null then ""
                  else ($pod.status.reason // ((($pod.status.containerStatuses // [])[0].state // {}) | to_entries | (.[0].value.reason // ""))) end)
               ] | @tsv')
        echo "  ${ready}/${node_count} nodes hold it; $((node_count - ready)) do not"
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
# and skips the download. The volume's name is read off the claim before
# the claim goes (it hashes the path, which delete is not told).
cmd_delete() {
    [ -n "$NAMESPACE" ] || log_error "delete needs -n NAMESPACE"
    local selector=()
    if [ "$ALL" = true ]; then
        selector=(-l "$LABEL_COMPONENT")
    else
        [ -n "$MODEL" ] || log_error "delete needs --model HFID or --all"
        check_model "$MODEL"
        selector=("$(name_for "$MODEL")")
    fi
    local volumes=()
    local line
    while IFS= read -r line; do
        [ -n "$line" ] && volumes+=("$line")
    done < <(kubectl get pvc -n "$NAMESPACE" "${selector[@]}" --ignore-not-found \
        -o jsonpath='{range .items[*]}{.spec.volumeName}{"\n"}{end}' 2>/dev/null || true)
    run_delete delete daemonset -n "$NAMESPACE" "${selector[@]}" --ignore-not-found
    run_delete delete pvc -n "$NAMESPACE" "${selector[@]}" --ignore-not-found
    [ "${#volumes[@]}" -eq 0 ] || run_delete delete pv "${volumes[@]}" --ignore-not-found
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
