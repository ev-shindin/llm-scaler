#!/usr/bin/env bash
# Which nodes are the accelerator nodes -- for the per-node DaemonSets
# (deploy/prepull.sh, and whatever follows it) that have to land on every
# node a model server could be scheduled to.
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
# What places the model servers is their accelerator RESOURCE request, not
# a label; these keys are the labels that say a node has one. On a cluster
# mixing vendors the default lands a CUDA image on an AMD, Intel or Gaudi
# node too -- disk spent where the engine can never run -- which is why
# `status` names the key each node matched and warns on those, and why
# --node-selector is the answer there.
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

# accelerator_selector_ok says whether $1 is one KEY=VALUE of label
# characters: a key of [A-Za-z0-9._/-], a value of [A-Za-z0-9._-], one '='.
# Empty is fine (the default). Prints why not, returns 1, when it is not.
accelerator_selector_ok() {
    [ -n "$1" ] || return 0
    local key="${1%%=*}" value="${1#*=}"
    case "$1" in
        *=*) ;;
        *) echo "must be KEY=VALUE, got '$1'"; return 1 ;;
    esac
    case "$key" in
        ""|*[!A-Za-z0-9._/-]*) echo "not a label key: '${key}'"; return 1 ;;
    esac
    case "$value" in
        *[!A-Za-z0-9._-]*) echo "not a label value: '${value}' (letters, digits, . _ -; no comma, no quotes)"; return 1 ;;
    esac
}

# accelerator_check_selector refuses a --node-selector that is not one. The
# string is substituted into a YAML document, where a quote or a newline
# would open a different field, and passed to `kubectl get nodes -l`, where
# a comma would mean a second term the DaemonSet's nodeSelector cannot
# carry.
accelerator_check_selector() {
    local why
    why="$(accelerator_selector_ok "$1")" || log_error "--node-selector: ${why}"
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

# accelerator_keys_json prints the key list as a JSON array, for jq.
accelerator_keys_json() {
    printf '%s\n' "${ACCELERATOR_PRODUCT_KEYS[@]}" | jq -R . | jq -s .
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
    kubectl get nodes -o json | jq --argjson keys "$(accelerator_keys_json)" \
        '.items |= map(select((.metadata.labels // {}) as $l | any($keys[]; $l[.] != null and $l[.] != "")))'
}

# accelerator_vendor_warning prints a warning when any of the nodes (a v1
# NodeList JSON document on stdin) matched only through a key that is not
# NVIDIA: a CUDA image is held there for nothing.
accelerator_vendor_warning() {
    local n
    n="$(jq --argjson keys "$(accelerator_keys_json)" \
        '[.items[] | (.metadata.labels // {}) as $l
          | first($keys[] | select($l[.] != null and $l[.] != "")) // ""
          | select(startswith("amd.com/") or startswith("beta.amd.com/") or startswith("habana.ai/") or startswith("gpu.intel.com/"))] | length')"
    [ "${n:-0}" -gt 0 ] || return 0
    log_warning "${n} node(s) carry an AMD, Intel or Gaudi accelerator label and are held too; if the image is CUDA-only that is disk for nothing -- --node-selector KEY=VALUE picks the nodes the model servers actually run on"
}

# accelerator_selector_text says, for a log line, what $1 selects.
accelerator_selector_text() {
    if [ -n "$1" ]; then printf '%s' "$1"; else printf 'any node carrying a known GPU product label'; fi
}
