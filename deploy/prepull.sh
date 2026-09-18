#!/usr/bin/env bash
# Keep an engine image present on every accelerator node, so a scale-up never
# starts with a pull.
#
# A replica's start time is what the autoscaler's first ramp is sized by, and
# on a node that does not have the image a 10-20 GB engine image is the largest
# single term in it -- a minute or more before the container even starts. This
# runs one DaemonSet per image on the accelerator nodes, holding the image OPEN
# (the image itself, asleep) rather than pulling it once: a container that
# exited does not protect its image from the kubelet's image garbage collection
# under disk pressure, a running one does. It requests no accelerator and a few
# megabytes of memory.
#
#   prepull.sh apply  -n NS --image IMG [--image IMG ...]   pre-pull IMG on every accelerator node
#   prepull.sh status -n NS [--image IMG]                   which nodes have it, which do not
#   prepull.sh delete -n NS [--image IMG | --all]           stop holding it
#
# Options:
#   --node-selector KEY=VALUE   which nodes count as accelerator nodes
#                               (default nvidia.com/gpu.present=true, the GPU
#                               operator's / NFD's label; set it to whatever
#                               your model servers select on)
#   --toleration KEY            tolerate a taint with KEY (any value, any
#                               effect); nvidia.com/gpu is always tolerated
#   --dry-run                   print the manifests instead of applying them
#
# The image string must be EXACTLY what the model server's pod spec names --
# same registry prefix, same tag or digest -- or the kubelet has pulled
# something else. `status` compares against the node's own image list, so a
# mismatch shows as "absent" on every node, which is the right answer.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BLUE='\033[0;34m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'
# shellcheck source=lib/common.sh
source "$HERE/lib/common.sh"
# NOTE: log_error EXITS. Nothing may follow it that needs to run.

NAMESPACE=""
IMAGES=()
NODE_SELECTOR="${WVA_PREPULL_NODE_SELECTOR:-nvidia.com/gpu.present=true}"
TOLERATIONS=("nvidia.com/gpu")
DRY_RUN=false
ALL=false
LABEL_COMPONENT="app.kubernetes.io/component=image-prepull"

usage() {
    sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; $d'
    exit 0
}

# name_for turns an image reference into a DaemonSet name: the last path
# element and tag, lower-cased, with everything that is not [a-z0-9-] replaced,
# truncated to leave room for a hash of the FULL reference -- two images that
# differ only in registry or digest must not collide on the name.
name_for() {
    local image="$1"
    local base hash
    base="$(printf '%s' "$image" | sed 's#.*/##; s/@sha256:/-/; s/:/-/g' | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9-]/-/g; s/^-*//; s/-*$//')"
    hash="$(wva_ns_suffix "$image")"
    printf 'prepull-%s-%s' "${base:0:40}" "$hash" | sed 's/-\{2,\}/-/g'
}

# render prints the DaemonSet for one image. The pod runs the image itself and
# sleeps: no accelerator request, no ports, a few megabytes. The image must
# carry /bin/sh, which every engine image here does; one that does not shows
# up in `status` as a pod that never became Ready, with the reason.
render() {
    local image="$1"
    local name
    name="$(name_for "$image")"
    local key="${NODE_SELECTOR%%=*}"
    local value="${NODE_SELECTOR#*=}"
    cat <<EOF
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: image-prepull
    app.kubernetes.io/component: image-prepull
    app.kubernetes.io/managed-by: wva-prepull
  annotations:
    wva.llmd.ai/prepull-image: "${image}"
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: image-prepull
      wva.llmd.ai/prepull: ${name}
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 100%
  template:
    metadata:
      labels:
        app.kubernetes.io/name: image-prepull
        app.kubernetes.io/component: image-prepull
        wva.llmd.ai/prepull: ${name}
      annotations:
        wva.llmd.ai/prepull-image: "${image}"
    spec:
      # The holder makes no API call; a token in a long-lived root shell on
      # every accelerator node is surface with no use. Same stance as the
      # warm pool's Pods.
      automountServiceAccountToken: false
      securityContext:
        seccompProfile:
          type: RuntimeDefault
      nodeSelector:
        ${key}: "${value}"
      tolerations:
$(for t in "${TOLERATIONS[@]}"; do printf '        - key: "%s"\n          operator: Exists\n' "$t"; done)
      terminationGracePeriodSeconds: 1
      containers:
        - name: hold
          image: "${image}"
          imagePullPolicy: IfNotPresent
          command: ["/bin/sh", "-c", "trap 'exit 0' TERM; while :; do sleep 3600 & wait \$!; done"]
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

# check_image refuses a reference that is not one. The string is substituted
# into a YAML document and a DaemonSet name; a quote, a space or a newline in
# it would reach kubectl as a different document, and a typo would render a
# holder that pulls nothing on every node. Registry, path, tag and digest
# characters only.
check_image() {
    case "$1" in
        "") log_error "--image must not be empty" ;;
        *[!A-Za-z0-9._:/@-]*) log_error "not an image reference: '$1' (registry/path[:tag|@sha256:digest], no spaces or quotes)" ;;
    esac
}

cmd_apply() {
    [ -n "$NAMESPACE" ] || log_error "apply needs -n NAMESPACE"
    [ "${#IMAGES[@]}" -gt 0 ] || log_error "apply needs at least one --image"
    local image
    for image in "${IMAGES[@]}"; do check_image "$image"; done
    for image in "${IMAGES[@]}"; do
        if [ "$DRY_RUN" = true ]; then
            render "$image"
            echo "---"
            continue
        fi
        render "$image" | kubectl apply -n "$NAMESPACE" -f - >/dev/null
        log_info "holding ${image} on nodes with ${NODE_SELECTOR} (DaemonSet $(name_for "$image"))"
    done
    # The status after an apply is a report, not a verdict: holders created a
    # second ago are still pulling, and that is not a failure of the apply.
    # `status` on its own returns non-zero for a node that lacks the image.
    # "report" mode turns the one exit inside cmd_status (no node matches the
    # selector) into a warning: log_error exits the process, and no `|| true`
    # on this line could catch that.
    [ "$DRY_RUN" = true ] || cmd_status report || true
}

# cmd_status lists, per selected node, whether the image is present and what
# the holder pod on that node is doing. "present" is either of two facts: the
# holder is Running (its container IS the image, so the kubelet has it), or
# the node's own image list names the reference. The second alone is not
# enough: the kubelet reports at most 50 images per node (--node-status-max-
# images), so on a busy node an image can be present and unlisted. A node
# whose holder is Failed with a reason (Evicted under DiskPressure, an image
# the registry will not serve) shows that reason: that node is the one a
# replica would start slowly on, and the holder cannot fix it from inside.
cmd_status() {
    command -v jq >/dev/null 2>&1 || log_error "jq is required for status (the node and pod lists are read with it)"
    local mode="${1:-verdict}"
    [ -n "$NAMESPACE" ] || log_error "status needs -n NAMESPACE"
    # `${arr[@]+"${arr[@]}"}`: an empty array expanded under set -u is an
    # unbound variable on bash before 4.4, and no --image is the documented
    # way to call status.
    local images=(${IMAGES[@]+"${IMAGES[@]}"})
    if [ "${#images[@]}" -eq 0 ]; then
        # every image a holder in this namespace declares. A while-read, not
        # mapfile: the deploy scripts run on macOS's bash 3.2 too
        # (deploy/lib/prereqs.sh says why).
        local line
        while IFS= read -r line; do
            [ -n "$line" ] && images+=("$line")
        done < <(kubectl get daemonset -n "$NAMESPACE" -l "$LABEL_COMPONENT" \
            -o jsonpath='{range .items[*]}{.metadata.annotations.wva\.llmd\.ai/prepull-image}{"\n"}{end}')
    fi
    [ "${#images[@]}" -gt 0 ] || log_error "no pre-pull DaemonSets in ${NAMESPACE} and no --image given"
    local nodes_json
    # Caught on the line: after an apply this runs under `|| true`, which
    # turns set -e off inside the function, and a Forbidden (a namespace
    # tenant listing nodes) must be a failure with a reason, not a report of
    # "0/ nodes".
    if ! nodes_json="$(kubectl get nodes -l "$NODE_SELECTOR" -o json)"; then
        if [ "$mode" = report ]; then
            log_warning "cannot list nodes (a namespace tenant may not); the DaemonSets are applied, but this report needs cluster-scoped node read -- ask for cluster-reader or check with the cluster admin"
            return 1
        fi
        log_error "cannot list nodes: status needs cluster-scoped node read (cluster-reader), which a namespace tenant does not have"
    fi
    local node_count
    node_count="$(printf '%s' "$nodes_json" | jq '.items | length')"
    if [ "$node_count" -eq 0 ]; then
        if [ "$mode" = report ]; then
            log_warning "no nodes match ${NODE_SELECTOR}; the DaemonSets are applied but will run nowhere until --node-selector names the label your model servers select on"
            return 1
        fi
        log_error "no nodes match ${NODE_SELECTOR}; set --node-selector to the label your model servers select on"
    fi
    # The pod list goes through a file: as a jq argument it exceeds the argv
    # limit on a cluster of any size.
    local pods_file
    pods_file="$(mktemp)"
    kubectl get pods -n "$NAMESPACE" -l "$LABEL_COMPONENT" -o json > "$pods_file"
    local rc=0
    for image in "${images[@]}"; do
        local name present holders
        name="$(name_for "$image")"
        present=0
        holders=0
        echo "${image}"
        printf '  %-28s %-8s %s\n' NODE IMAGE HOLDER
        # Per node: listed-by-kubelet, holder phase, holder reason (the pod's
        # own status.reason first -- Evicted -- then the container state's).
        while IFS=$'\t' read -r node listed phase reason; do
            local has=absent
            [ "$phase" = "no pod" ] || holders=$((holders + 1))
            if [ "$phase" = Running ] || [ "$listed" = listed ]; then
                has=present
                present=$((present + 1))
            elif [ "$reason" = RunContainerError ] || [ "$reason" = CrashLoopBackOff ] || [ "$reason" = Error ] || [ "$reason" = Completed ]; then
                # The container was created from the image (and could not exec
                # /bin/sh, or ran and stopped), so the kubelet HAS the image;
                # nothing is holding it, though, and garbage collection can
                # take it back. Measured: an image without a shell reports
                # RunContainerError on every node.
                has=pulled
                present=$((present + 1))
                reason="${reason} (pulled, not held: the image runs no /bin/sh)"
            else
                rc=1
            fi
            printf '  %-28s %-8s %s %s\n' "$node" "$has" "$phase" "$reason"
        done < <(printf '%s' "$nodes_json" | jq -r --arg img "$image" --arg ds "$name" --slurpfile pods "$pods_file" \
            '.items[] as $node
             | ($pods[0].items | map(select(.spec.nodeName == $node.metadata.name and .metadata.labels["wva.llmd.ai/prepull"] == $ds)) | first) as $pod
             | [ $node.metadata.name,
                 (if ([$node.status.images[]?.names[]?] | index($img)) != null then "listed" else "unlisted" end),
                 (if $pod == null then "no pod"
                  elif $pod.status.phase == "Running" and (($pod.status.containerStatuses // []) | map(.ready) | all) then "Running"
                  else $pod.status.phase + " (not ready)" end),
                 (if $pod == null then ""
                  else ($pod.status.reason // ((($pod.status.containerStatuses // [])[0].state // {}) | to_entries | (.[0].value.reason // ""))) end)
               ] | @tsv')
        echo "  ${present}/${node_count} nodes hold it; $((node_count - present)) do not"
        # No holder anywhere is the DaemonSet controller failing to create
        # pods -- a Pod Security "restricted" namespace (runAsNonRoot), a
        # ResourceQuota, a LimitRange -- and the reason is on the DaemonSet's
        # events, not on any pod. Print the latest one so "no pod" on every
        # node is not the whole answer. Keyed on holders, not on present: a
        # node can still list the image from an earlier pull while every
        # pod is being refused.
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

cmd_delete() {
    [ -n "$NAMESPACE" ] || log_error "delete needs -n NAMESPACE"
    if [ "$ALL" = true ]; then
        kubectl delete daemonset -n "$NAMESPACE" -l "$LABEL_COMPONENT" --ignore-not-found
        return
    fi
    [ "${#IMAGES[@]}" -gt 0 ] || log_error "delete needs --image IMG or --all"
    for image in "${IMAGES[@]}"; do
        kubectl delete daemonset -n "$NAMESPACE" "$(name_for "$image")" --ignore-not-found
    done
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
        -n|--namespace|--image|--node-selector|--toleration)
            [ $# -ge 2 ] || log_error "$1 needs a value" ;;
    esac
    case "$1" in
        -n|--namespace) NAMESPACE="$2"; shift 2 ;;
        --image) IMAGES+=("$2"); shift 2 ;;
        --node-selector) NODE_SELECTOR="$2"; shift 2 ;;
        --toleration) TOLERATIONS+=("$2"); shift 2 ;;
        --dry-run) DRY_RUN=true; shift ;;
        --all) ALL=true; shift ;;
        -h|--help) usage ;;
        *) log_error "unknown option: $1" ;;
    esac
done
case "$NODE_SELECTOR" in
    *=*) ;;
    *) log_error "--node-selector must be KEY=VALUE, got '${NODE_SELECTOR}'" ;;
esac

"cmd_${CMD}"
