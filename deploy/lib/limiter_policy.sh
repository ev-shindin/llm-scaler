#!/usr/bin/env bash
#
# The scaling-policy documents shared datapath: building a `limiters:` list, and
# merging it into a policy without losing the rest of it.
#
# Its own file because TWO commands write that list and they load differently:
# the installer (deploy/install.sh, via infra_wva.sh) for one deployments own
# policy, and `make enable-physical-limiter` (physical_limiter.sh, sourced by the
# Makefile with only common.sh beside it) for the cluster policy every WVA reads.
# Both used to build the list inline, and both built the SAME invalid quota entry
# -- see limiter_entry_yaml for what that costs.
#
# Requires funcs: log_error.
# Requires vars: none unconditionally; the quota path reads WVA_QUOTAS,
# WVA_QUOTA_SCOPE, WVA_SCOPE, WVA_WATCH_NS and WVA_NS.

# limiter_entry_yaml emits the `limiters:` list that WVA_LIMITER declares, as
# YAML on stdout. Pure: it reads WVA_LIMITER-related variables and writes no
# cluster state, so hack/check-limiter-declaration.sh can execute it offline.
#
# It exists because the installer used to emit `[{"type": "quota"}]` and call
# that "bounded by the quota limiter". The controller rejects it —
#
#     Invalid saturation scaling config entry ... "limiters: entry[0]: name must
#     not be empty"
#
# — and rejecting the entry throws away the WHOLE `default` policy, thresholds
# included, then falls back to no limiter at all: "scaling is UNCONSTRAINED". The
# install printed success either way, so WVA_LIMITER=quota had never once bounded
# anything, and the one line that said so was an ERROR in the controller log.
#
# A quota entry needs a budget from the operator; there is no safe default. An
# empty one is not "unlimited" — QuotaForNamespace returns an empty map for a
# namespace it does not list, callers read that as zero, and the fleet freezes.
# So WVA_QUOTAS is required, and a missing or malformed one stops the install
# before the ConfigMap is touched.
limiter_entry_yaml() {
    local ltype="$1"

    if [ "$ltype" = "gpu-inventory" ] || [ "$ltype" = "inventory" ]; then
        # Physical capacity comes from the GPU operator, so there is nothing to
        # declare. A name is allowed but unused — NewLimiterFromConfig names the
        # inventory limiter itself — and quota fields are REJECTED on this type.
        printf -- '- type: %s\n' "$ltype"
        return 0
    fi
    if [ "$ltype" != "quota" ]; then
        log_error "limiter_entry_yaml: unknown limiter type '$ltype'"
    fi

    local scope="${WVA_QUOTA_SCOPE:-namespace}"
    case "$scope" in
        namespace|cluster) ;;
        *) log_error "WVA_QUOTA_SCOPE must be 'namespace' or 'cluster', got '$scope'" ;;
    esac

    if [ -z "${WVA_QUOTAS:-}" ]; then
        log_error "WVA_LIMITER=quota needs WVA_QUOTAS, a per-accelerator budget: WVA_QUOTAS='H200=8 A100=4'.
    There is no default to fall back on. A quota entry with no budget is not unlimited — an accelerator
    the entry does not name gets zero, so every managed workload stops scaling up. Use -1 for no cap on
    a type ('H100=-1'). The accelerator name is the one WVA resolves, which the controller logs per
    variant:
        kubectl logs -n $WVA_NS deploy/wva-controller-manager | grep accelerator"
    fi

    # Accept commas or whitespace between entries, so both of the shapes an
    # operator reaches for work: 'H200=8,A100=4' and 'H200=8 A100=4'.
    local pairs pair name value
    pairs="$(printf '%s' "$WVA_QUOTAS" | tr ',' ' ')"
    local types=""
    for pair in $pairs; do
        name="${pair%%=*}"
        value="${pair#*=}"
        if [ "$name" = "$pair" ] || [ -z "$name" ] || [ -z "$value" ]; then
            log_error "WVA_QUOTAS entry '$pair' is not TYPE=N (for example 'H200=8'). Whole value: '$WVA_QUOTAS'"
        fi
        case "$value" in
            -1|[0-9]|[0-9][0-9]*) ;;
            *) log_error "WVA_QUOTAS entry '$pair': the budget must be a whole number of GPUs, or -1 for no cap on that type" ;;
        esac
        # MaxQuotaValue in internal/config/quota_limiter.go. Above it the
        # controller rejects the entry, which costs the whole policy again.
        if [ "$value" -gt 1048576 ]; then
            log_error "WVA_QUOTAS entry '$pair' exceeds the maximum quota of 1048576 GPUs"
        fi
        types="${types}    ${name}: ${value}
"
    done

    # One line per argument rather than one format with embedded newlines: a `\n`
    # followed by a space is what hack/check-make-recipes' sibling guard in
    # lint-deploy-scripts looks for, because that is the shape a collapsed line
    # continuation leaves behind.
    printf '%s\n' '- name: install-quota' '  type: quota' "  scope: ${scope}"
    if [ "$scope" = "cluster" ]; then
        # Cluster scope caps the SUM across every namespace, so it is keyed by
        # accelerator type alone. Indented one level less than the namespace form.
        printf '  quotas:\n'
        printf '%s' "$types"
        return 0
    fi
    # Namespace scope is keyed by namespace. `default` is the reserved
    # fall-through key and means "this much PER unlisted namespace", not a shared
    # pool — so a cluster-scoped controller managing ten tenants hands out ten
    # budgets, not one. Name the managed namespace instead whenever there is
    # exactly one, which is every namespace-scoped install.
    local key
    if [ "${WVA_SCOPE:-cluster}" = "namespace" ]; then
        key="${WVA_WATCH_NS:-$WVA_NS}"
    else
        key="default"
    fi
    printf '%s\n' '  namespaceQuotas:' "    ${key}:"
    printf '%s' "$types" | sed 's/^/  /'
}

# policy_with_limiters <policy yaml> <limiters yaml> -- the policy with its
# limiters: list REPLACED by the given one, on stdout.
#
# Replaced, not merged: re-running the install with a different WVA_LIMITER must
# not leave both declared, because EffectiveLimiterMode collapses the list to one
# mode (quota wins) and the other entry is then silently unenforced.
#
# strenv, not env. `env()` parses the value as YAML and a multi-line document
# makes it fail -- `Error: EOF`, with no mention of the variable -- which aborted
# the install after it had already re-applied the shipped ConfigMap, leaving the
# policy with no limiters at all. Paired with limiter_entry_yaml here so the
# offline check exercises the same transform the install runs.
policy_with_limiters() {
    printf '%s\n' "$1" | LIMITERS_YAML="$2" yq '.limiters = (strenv(LIMITERS_YAML) | from_yaml)'
}
