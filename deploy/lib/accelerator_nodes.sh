#!/usr/bin/env bash
# Which nodes are the accelerator nodes -- for the per-node DaemonSets
# (deploy/prepull.sh, deploy/weights.sh) that have to land on every node a
# model server could be scheduled to.
#
# "Has a GPU" is not one label. NVIDIA GPU Feature Discovery writes
# nvidia.com/gpu.product (and nvidia.com/gpu.present); CoreWeave, GKE, EKS
# Auto Mode, Karpenter and the AMD operator each write their own key, and on
# those clusters a DaemonSet selecting on the GFD key runs on NO node while
# the model servers, which select on nothing, schedule to every GPU node.
# So the default is not a nodeSelector but a node affinity with one term per
# known product-label key -- terms are OR-ed, which a nodeSelector cannot
# express -- and the node listing filters on the same keys. Each term is
# "the key exists AND is not empty": CoreWeave writes gpu.nvidia.com/model=""
# on its CPU nodes, and Exists alone put a holder on four of them. An
# explicit KEY=VALUE selector replaces both.
#
# The key list is the one deploy/lib/accelerator_labels.py and the
# controller resolve through; hack/check-accelerator-labels.sh fails when
# any copy drifts from the others.

# Keys whose presence on a node says an accelerator is there. Order is the
# canonical GFD key first, as in the other copies.
ACCELERATOR_PRODUCT_KEYS=(
    "nvidia.com/gpu.product"
    "gpu.nvidia.com/model"
    "gpu.nvidia.com/class"
    "cloud.google.com/gke-accelerator"
    "eks.amazonaws.com/instance-gpu-name"
    "karpenter.k8s.aws/instance-gpu-name"
    "karpenter.azure.com/sku-gpu-name"
    "amd.com/gpu.product-name"
    "beta.amd.com/gpu.product-name"
    "habana.ai/product.name"
    "gpu.intel.com/product"
)

# accelerator_check_selector refuses a KEY=VALUE that is not one: a label
# key ([A-Za-z0-9._/-]) and a label value ([A-Za-z0-9._-]), one '='. The
# string is substituted into a YAML document, where a quote or a newline
# would open a different field, and passed to `kubectl get nodes -l`, where
# a comma would mean a second term the DaemonSet's nodeSelector cannot
# carry. Empty means the default (every accelerator node).
accelerator_check_selector() {
    [ -n "$1" ] || return 0
    local key="${1%%=*}" value="${1#*=}"
    case "$1" in
        *=*) ;;
        *) log_error "--node-selector must be KEY=VALUE, got '$1'" ;;
    esac
    case "$key" in
        ""|*[!A-Za-z0-9._/-]*) log_error "--node-selector: not a label key: '${key}'" ;;
    esac
    case "$value" in
        *[!A-Za-z0-9._-]*) log_error "--node-selector: not a label value: '${value}' (letters, digits, . _ -; no comma, no quotes)" ;;
    esac
}

# accelerator_check_toleration refuses a taint key that is not one. An empty
# key with operator Exists tolerates EVERY taint, which is not what a caller
# who typed --toleration "" meant.
accelerator_check_toleration() {
    case "$1" in
        ""|*[!A-Za-z0-9._/-]*) log_error "--toleration: not a taint key: '$1'" ;;
    esac
}

# accelerator_placement_yaml prints the pod-spec placement for $1: with a
# KEY=VALUE, a nodeSelector; empty, the OR-ed affinity over the product
# keys. Indented for a pod template's spec (6 spaces), the way the
# DaemonSets here lay it out.
accelerator_placement_yaml() {
    local selector="$1"
    if [ -n "$selector" ]; then
        cat <<EOF
      nodeSelector:
        ${selector%%=*}: "${selector#*=}"
EOF
        return
    fi
    cat <<'EOF'
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
EOF
    local key
    for key in "${ACCELERATOR_PRODUCT_KEYS[@]}"; do
        cat <<EOF
              - matchExpressions:
                  - key: "${key}"
                    operator: Exists
                  - key: "${key}"
                    operator: NotIn
                    values: [""]
EOF
    done
}

# accelerator_nodes_json prints the node list (a v1 NodeList JSON document)
# the placement for $1 would match: `kubectl get nodes -l KEY=VALUE`, or
# every node carrying any product key. Fails as kubectl fails (a Forbidden
# for a namespace tenant) -- the caller decides what that means.
accelerator_nodes_json() {
    local selector="$1"
    if [ -n "$selector" ]; then
        kubectl get nodes -l "$selector" -o json
        return
    fi
    local keys_json
    keys_json="$(printf '%s\n' "${ACCELERATOR_PRODUCT_KEYS[@]}" | jq -R . | jq -s .)"
    kubectl get nodes -o json | jq --argjson keys "$keys_json" \
        '.items |= map(select((.metadata.labels // {}) as $l | any($keys[]; $l[.] != null and $l[.] != "")))'
}

# accelerator_selector_text says, for a log line, what $1 selects.
accelerator_selector_text() {
    if [ -n "$1" ]; then printf '%s' "$1"; else printf 'any node carrying a known GPU product label'; fi
}
