#!/usr/bin/env bash
#
# Refuse to install a second WVA on top of an existing one.
#
# Requires vars: WVA_NS.
# Requires funcs: log_info/log_success/log_warning/log_error, wva_install_scope.
#
# The contract:
#
#   ONE cluster-scoped WVA, and nothing else.
#   OR any number of namespace-scoped WVAs, at most one per namespace.
#
# Never a mix. A cluster-scoped controller already covers every namespace, so a
# namespace-scoped one beside it is a second controller for the same workloads;
# and a cluster-scoped one installed beside existing namespace-scoped ones
# swallows them the moment it starts.
#
# Note what does NOT partition, and what does. Discovery is call-driven: a workload
# registers with the WVA whose scalerAddress its trigger names, and every install
# has its own wva-external-scaler.<ns> service — so no two controllers ever see the
# same workload, whatever their scope. Each install also gets its own
# ClusterRoleBindings, suffixed with a hash of its namespace, so they cannot take
# permissions from one another. The namespace IS the instance identity; there is
# nothing else to name.
#
# What is NOT partitioned is the physical GPU pool. Namespace-scoped controllers
# each compute free capacity from the same nodes and allocate against it without
# seeing the other's claim, so several of them can oversubscribe a shared pool.
# That is a real caveat of this topology rather than a reason to forbid it: bound
# each install with a quota limiter, or give each tenant its own GPU nodes.
#

# wva_installations echoes "namespace name scope" for every WVA controller
# Deployment in the cluster. Scope is read from the container args rather than any
# label: --watch-namespace is what actually restricts the controller's cache, so it
# is the truth about what an install can reach.
# The args are read in the SAME list call, not one call per install. This function
# used to fetch each install's Deployment again to read its args, which is one round
# trip per WVA on the cluster: 12 installs on pokprod001 cost ~13 SECONDS of the
# preflight, for information the first response already contained. The scope decision
# is unchanged -- it is still --watch-namespace in the container args, because that is
# what actually restricts the controller's cache -- it is just no longer paid for
# twice.
#
# Emitted as "namespace name scope" so callers are unaffected. The args are collapsed
# to one line inside the template; a newline in an arg would otherwise split one
# install across two records.
WVA_INSTALLS_TEMPLATE='{{range .items}}{{.metadata.namespace}} {{.metadata.name}} {{range (index .spec.template.spec.containers 0).args}}{{.}}~{{end}}{{"\n"}}{{end}}'

wva_installations() {
    local ns name args scope
    while read -r ns name args; do
        [ -n "$ns" ] || continue
        case "$args" in
            *--watch-namespace*) scope=namespace ;;
            *)                   scope=cluster ;;
        esac
        echo "$ns $name $scope"
    done < <(wva_installations_raw)
}

# wva_installations_raw lists WVA Deployments cluster-wide, and reports the
# difference between "none found" and "not allowed to look".
#
# Listing across namespaces needs cluster-wide read, which a namespace admin does
# not have. Swallowing that Forbidden made the guard silently PASS for exactly the
# installer it exists to protect: a tenant could add a second controller to a
# cluster that already had one and be told nothing.
wva_installations_raw() {
    local out rc
    # control-plane=controller-manager is what makes it a CONTROLLER, and the
    # name label alone is not. Every WVA component carries the name label --
    # notably the warm-pool Deployment, which is a data-plane workload with no
    # reconcile loop at all. Matching on the name alone found one and, because
    # its args carry no --watch-namespace, classified it cluster-scoped by the
    # rule below. The install then refused with "A cluster-scoped WVA is already
    # installed: evgensh-wva-test/wva-warm-pool", naming a Deployment that
    # manages nothing, and there was no way to proceed except to disable a check
    # that was right in principle.
    out=$(kubectl get deployments -A -l app.kubernetes.io/name=workload-variant-autoscaler,control-plane=controller-manager -o go-template="$WVA_INSTALLS_TEMPLATE" 2>&1); rc=$?
    if [ $rc -ne 0 ]; then
        if printf '%s' "$out" | grep -qi 'forbidden'; then
            log_warning "Cannot check whether another WVA is already installed (listing Deployments cluster-wide is not permitted for you)."
            log_warning "  Two controllers managing the same workloads each allocate from the same free GPUs without seeing the other's claims. Confirm with whoever administers the cluster."
        fi
        return 0
    fi
    printf '%s
' "$out"
}

check_single_installation() {
    local peers="" cluster_wide="" ns name scope
    while read -r ns name scope; do
        [ -n "$ns" ] || continue
        # Our own namespace is an upgrade, not a collision.
        if [ "$ns" = "$WVA_NS" ]; then
            log_info "Existing WVA in $WVA_NS — this install upgrades it in place."
            continue
        fi
        peers="${peers:+$peers, }$ns/$name ($scope-scoped)"
        [ "$scope" = "cluster" ] && cluster_wide="${cluster_wide:+$cluster_wide, }$ns/$name"
    done < <(wva_installations)

    [ -n "$peers" ] || return 0

    local want
    want="$(wva_install_scope)"

    # A cluster-wide controller already manages every namespace. Nothing joins it.
    if [ -n "$cluster_wide" ]; then
        log_error "A cluster-scoped WVA is already installed: $cluster_wide.

It manages every namespace, including $WVA_NS, so a second controller here would be
a second controller for the same workloads.

Pick one:
  - use it as it is — it already covers $WVA_NS
  - upgrade it:                     WVA_NS=${cluster_wide%%/*} ...
  - replace it with per-namespace controllers:
        make undeploy-wva SCOPE=cluster WVA_NS=${cluster_wide%%/*}
    then install one WVA_SCOPE=namespace per namespace that needs one
  - or remove it and install this one in its place"
    fi

    # Installing cluster-scoped on top of existing namespace-scoped installs would
    # swallow their namespaces the moment it starts.
    if [ "$want" = "cluster" ]; then
        log_error "Installing a cluster-scoped WVA, but namespace-scoped ones already exist: $peers.

A cluster-scoped controller manages every namespace, including theirs — both would
then decide for the same workloads.

Pick one:
  - install this one namespace-scoped too:   WVA_SCOPE=namespace
  - or remove the existing ones first, then install cluster-scoped"
    fi

    # Namespace-scoped beside namespace-scoped: the supported multi-controller shape.
    log_info "WVA already installed in: $peers. Continuing: this install is namespace-scoped, so it manages only $WVA_NS and they manage only theirs."
    log_info "  Each install has its own external-scaler Service and its own ClusterRoleBindings (suffixed per namespace), so they do not overlap. They DO draw on the same physical GPUs — bound them with quota limiters, or give each its own GPU nodes."
}
