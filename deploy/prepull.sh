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
#   prepull.sh apply  -n NS --image IMG [--image IMG ...] [--dry-run]   pre-pull IMG on every accelerator node
#   prepull.sh status -n NS [--image IMG] [--node-selector KEY=VALUE]   which nodes have it, which do not
#   prepull.sh delete -n NS (--image IMG | --all) [--dry-run]           stop holding it
#
# Options:
#   --node-selector KEY=VALUE   which nodes count as accelerator nodes. The
#                               default is any node carrying a known GPU
#                               product label (GPU Feature Discovery,
#                               CoreWeave, GKE, EKS, Karpenter, AMD -- the
#                               list in deploy/lib/accelerator_nodes.sh);
#                               set this to what your model servers select
#                               on when they select on something
#   --toleration KEY            tolerate a taint with KEY (any value, any
#                               effect); nvidia.com/gpu is always tolerated
#   --dry-run                   apply: print the manifests instead of
#                               applying them; delete: print what would go
#
# The image string must be EXACTLY what the model server's pod spec names --
# same registry prefix, same tag or digest -- or the kubelet has pulled
# something else. `status` compares against the node's own image list, so a
# mismatch shows as "absent" on every node, which is the right answer.
# `status` lists the nodes each DaemonSet was applied for (the selector is
# recorded on it); --node-selector on status overrides that.
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
IMAGES=()
NODE_SELECTOR=""          # empty: every node carrying a known GPU product label
NODE_SELECTOR_GIVEN=false
TOLERATIONS=("nvidia.com/gpu")
DRY_RUN=false
ALL=false
# Both labels: the component name is generic enough that another tool's
# puller could carry it; managed-by is what says this script made it.
LABEL_COMPONENT="app.kubernetes.io/component=image-prepull,app.kubernetes.io/managed-by=wva-prepull"

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
    wva.llmd.ai/prepull-node-selector: "${NODE_SELECTOR}"
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
        app.kubernetes.io/managed-by: wva-prepull
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
$(accelerator_placement_yaml "$NODE_SELECTOR")
      tolerations:
$(for t in "${TOLERATIONS[@]}"; do printf '        - key: "%s"\n          operator: Exists\n' "$t"; done)
      terminationGracePeriodSeconds: 1
      containers:
        - name: hold
          image: "${image}"
          imagePullPolicy: IfNotPresent
          command: ["/bin/sh", "-c", "trap 'exit 0' TERM; while :; do sleep 3600 & wait \$!; done"]
          env:
            # Engine images bake in NVIDIA_VISIBLE_DEVICES=all, and the NVIDIA
            # runtime honours it from a container that requested no GPU: the
            # holder would get every device on the node injected. "void" is
            # the runtime's "behave like runc": no devices, no driver libs.
            - name: NVIDIA_VISIBLE_DEVICES
              value: "void"
          resources:
            requests:
              cpu: 5m
              memory: 16Mi
            limits:
              memory: 64Mi
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
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
        log_info "holding ${image} on $(accelerator_selector_text "$NODE_SELECTOR") (DaemonSet $(name_for "$image"))"
    done
    [ "$DRY_RUN" = true ] && return 0
    # The status after an apply is a report, not a verdict: holders created a
    # second ago are still pulling, and that is not a failure of the apply.
    # `status` on its own returns non-zero for a node that lacks the image.
    # "report" mode turns the exits inside cmd_status (no node matches, a node
    # list the caller may not read) into warnings: log_error exits the
    # process, and no `|| true` on this line could catch that. The jq check
    # is here for the same reason -- without jq the apply still happened.
    if ! command -v jq >/dev/null 2>&1; then
        log_warning "jq is not installed; skipping the per-node report (prepull.sh status needs it)"
        return 0
    fi
    cmd_status report || true
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
#
# The nodes are the ones the DaemonSet was applied for -- its recorded
# selector -- unless --node-selector overrides; a status run with a different
# default than the apply would otherwise report the wrong nodes.
#
# mode: verdict (status; every problem exits) or report (after apply; a
# selector matching nothing or a node list the caller may not read is a
# warning, since the apply already happened).
cmd_status() {
    local mode="${1:-verdict}"
    command -v jq >/dev/null 2>&1 || log_error "jq is required for status (the node and pod lists are read with it)"
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
    # The pod list once, to a file: --argjson with a namespace's worth of pods
    # exceeded the argument length limit; --slurpfile reads a file, no
    # limit on a cluster of any size.
    local pods_file
    pods_file="$(mktemp)"
    kubectl get pods -n "$NAMESPACE" -l "$LABEL_COMPONENT" -o json > "$pods_file"
    local rc=0
    local image
    for image in "${images[@]}"; do
        local name present pulled holders selector
        name="$(name_for "$image")"
        present=0
        pulled=0
        holders=0
        selector="$NODE_SELECTOR"
        if [ "$NODE_SELECTOR_GIVEN" = false ]; then
            # Off the live DaemonSet's pod template, not an annotation: a
            # nodeSelector is the KEY=VALUE it was applied with (a DaemonSet
            # from before this script recorded anything included), none is
            # the affinity default.
            selector="$(kubectl get daemonset -n "$NAMESPACE" "$name" \
                -o jsonpath='{.spec.template.spec.nodeSelector}' 2>/dev/null \
                | jq -r 'to_entries | map(.key + "=" + .value) | first // ""' 2>/dev/null || true)"
            # Read off an object a namespace tenant can edit; held to the
            # same rule as the flag before it reaches kubectl.
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
        # Caught on the line: in report mode this runs under `|| true`, which
        # turns set -e off inside the function, and a Forbidden (a namespace
        # tenant listing nodes) must be a failure with a reason, not a report
        # of "0/ nodes". The reason is kubectl's, and only a Forbidden is the
        # permission story.
        if ! nodes_json="$(accelerator_nodes_json "$selector" 2>"$err_file")"; then
            local why
            if grep -q Forbidden "$err_file"; then
                why="a namespace tenant may not list nodes; this needs cluster-scoped node read (cluster-reader) -- ask for it or check with the cluster admin"
            else
                why="$(tr '\n' ' ' < "$err_file")"
            fi
            rm -f "$err_file"
            if [ "$mode" = report ]; then
                log_warning "cannot list nodes for ${image}: ${why}"
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
                log_warning "no node matches $(accelerator_selector_text "$selector"); ${image} will be held nowhere (--node-selector picks the nodes)"
                rc=1
                continue
            fi
            log_error "no node matches $(accelerator_selector_text "$selector") (--node-selector picks the nodes)"
        fi
        echo "${image}  (nodes: $(accelerator_selector_text "$selector"))"
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
                pulled=$((pulled + 1))
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
        echo "  $((present + pulled))/${node_count} nodes have it (${present} held, ${pulled} pulled but not held); $((node_count - present - pulled)) do not"
        [ -n "$selector" ] || printf '%s' "$nodes_json" | accelerator_vendor_warning
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

# run_delete runs a kubectl delete, or under --dry-run prints it.
run_delete() {
    if [ "$DRY_RUN" = true ]; then
        echo "would run: kubectl $*"
    else
        kubectl "$@"
    fi
}

cmd_delete() {
    [ -n "$NAMESPACE" ] || log_error "delete needs -n NAMESPACE"
    if [ "$ALL" = true ]; then
        run_delete delete daemonset -n "$NAMESPACE" -l "$LABEL_COMPONENT" --ignore-not-found
        return
    fi
    [ "${#IMAGES[@]}" -gt 0 ] || log_error "delete needs --image IMG or --all"
    local image
    for image in "${IMAGES[@]}"; do check_image "$image"; done
    for image in "${IMAGES[@]}"; do
        run_delete delete daemonset -n "$NAMESPACE" "$(name_for "$image")" --ignore-not-found
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
        --node-selector) NODE_SELECTOR="$2"; NODE_SELECTOR_GIVEN=true; shift 2 ;;
        --toleration) accelerator_check_toleration "$2"; TOLERATIONS+=("$2"); shift 2 ;;
        --dry-run) [ "$CMD" != status ] || log_error "--dry-run applies to apply and delete, not status"; DRY_RUN=true; shift ;;
        --all) [ "$CMD" = delete ] || log_error "--all is a delete option"; ALL=true; shift ;;
        -h|--help) usage ;;
        *) log_error "unknown option: $1" ;;
    esac
done
accelerator_check_selector "$NODE_SELECTOR"

"cmd_${CMD}"
