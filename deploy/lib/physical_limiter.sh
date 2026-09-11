#!/usr/bin/env bash
#
# Cluster-admin action: turn the physical (gpu-inventory) GPU limiter on, or off,
# for every WVA on the cluster.
#
# Requires vars: WVA_POLICY_NS, WVA_LIMITER_TYPE, WVA_LIMITER_TARGETS.
# Requires funcs: log_info/log_success/log_warning/log_error, wva_ns_suffix.
#
# Why this is one command rather than a documented sequence of four:
#
# Declaring the limiter is the easy part — a ConfigMap. The three things around it
# are what make it work, and each one, left out, fails in a way that looks like
# something else:
#
#   1. the policy must live where a TENANT CANNOT EDIT IT, or it is not a bound;
#   2. every controller must be able to READ it, or that controller refuses to
#      start rather than run unbounded while you believe a limit applies;
#   3. every controller must be able to LIST NODES, or it goes NotReady — a
#      GPU-aware optimizer that cannot resolve an accelerator would otherwise
#      charge every variant to no pool and silently stop scaling anything up.
#
# (3) is the one that turns this into an operation with blast radius: declaring
# the limiter takes out every existing WVA that lacks node access, all at once.
# So this grants it to each of them FIRST, and says which controllers it touched.

# pl_policy_ns echoes the namespace cluster policy is published in.
#
# The default is the well-known name the controller looks for, and "well-known
# wins" is deliberate in the controller: a tenant cannot point their WVA somewhere
# more permissive. Overriding it here is for a cluster that already keeps policy
# elsewhere and labels its namespaces accordingly.
pl_policy_ns() {
    echo "${WVA_POLICY_NS:-wva-policy}"
}

# pl_controller_namespaces echoes every namespace running a WVA controller.
#
# Discovered rather than configured, because the hazard is precisely the install
# you forgot: this command's job is to leave NO controller unable to honour the
# policy it is about to publish. WVA_LIMITER_TARGETS overrides it when you mean
# to grant a subset — and then the ones you left out are named in the warning.
pl_controller_namespaces() {
    if [ -n "${WVA_LIMITER_TARGETS:-}" ]; then
        printf '%s\n' $WVA_LIMITER_TARGETS
        return
    fi
    kubectl get deploy -A -l app.kubernetes.io/name=workload-variant-autoscaler \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null | sort -u
}

# pl_policy_cm_ns echoes the namespace whose ConfigMap the controller in $1
# actually reads policy from — which is NOT always the well-known one.
#
# The controller's rule (internal/controller/policy_namespace.go), and it is
# deliberate: the well-known namespace wins only for a SELF-MANAGED controller —
# one whose watch namespace is its own, i.e. the tenant-owned shape. An
# admin-owned controller (cluster-scoped, or namespace-scoped pointed at someone
# else's namespace) keeps reading its OWN ConfigMap, because letting wva-policy
# win there would silently flip a correctly bounded cluster to unbounded the
# moment anyone created that namespace for an unrelated tenant.
#
# So writing policy to wva-policy and walking away would be a no-op for exactly
# the installs an admin is most likely to have. This asks each controller.
pl_policy_cm_ns() {
    local ns="$1" args watch
    args="$(kubectl get deploy -n "$ns" -l app.kubernetes.io/name=workload-variant-autoscaler \
        -o jsonpath='{.items[0].spec.template.spec.containers[0].args}' 2>/dev/null || true)"
    case "$args" in
        *--watch-namespace*) : ;;
        # No --watch-namespace: cluster-scoped. Its own ConfigMap is authoritative.
        *) echo "$ns"; return ;;
    esac
    watch="$(kubectl get deploy -n "$ns" -l app.kubernetes.io/name=workload-variant-autoscaler \
        -o jsonpath='{.items[0].spec.template.spec.containers[0].env[?(@.name=="WVA_WATCH_NAMESPACE")].value}' 2>/dev/null || true)"
    # The base leaves it as the literal $(POD_NAMESPACE), which resolves to the
    # controller's own namespace; an install with WVA_WATCH_NS patches in a name.
    case "$watch" in
        ''|'$(POD_NAMESPACE)') watch="$ns" ;;
    esac
    if [ "$watch" = "$ns" ]; then
        pl_policy_ns          # self-managed: the well-known namespace governs it
    else
        echo "$ns"            # watches someone else's namespace: admin-owned
    fi
}

# pl_needs_restart reports whether the controller in $1 resolved its policy
# namespace BEFORE the well-known one existed.
#
# The resolution runs once, before the manager starts. A controller that came up
# when wva-policy did not exist is reading its own ConfigMap and will go on doing
# so — so publishing policy without restarting it is a no-op that looks like a
# success. Compared against the namespace's creation time rather than assumed,
# because restarting controllers that do not need it is its own small outage.
pl_needs_restart() {
    local ns="$1" policy_ns started created
    [ "$(pl_policy_cm_ns "$ns")" = "$policy_ns_global" ] || return 1
    created="$(kubectl get namespace "$policy_ns_global" -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || true)"
    started="$(kubectl get pods -n "$ns" -l app.kubernetes.io/name=workload-variant-autoscaler \
        -o jsonpath='{.items[0].status.startTime}' 2>/dev/null || true)"
    [ -n "$created" ] && [ -n "$started" ] || return 1
    [[ "$started" < "$created" ]]   # RFC3339 sorts lexically
}

# pl_controller_sa echoes the ServiceAccount a controller in $1 runs as.
pl_controller_sa() {
    local ns="$1" sa
    sa="$(kubectl get deploy -n "$ns" -l app.kubernetes.io/name=workload-variant-autoscaler \
        -o jsonpath='{.items[0].spec.template.spec.serviceAccountName}' 2>/dev/null || true)"
    # An install that predates the phase split, or one being prepared before the
    # controller exists, still has a predictable SA name.
    echo "${sa:-wva-controller-manager}"
}

# pl_grant_node_read grants one controller the cluster-scoped node read.
#
# Same object names the namespace-scoped overlay uses, so a later install or
# upgrade is an idempotent apply over these rather than a second, competing pair.
pl_grant_node_read() {
    local ns="$1" sa="$2" suffix
    suffix="$(wva_ns_suffix "$ns")"
    kubectl apply -f - >/dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: wva-node-reader-role-${suffix}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
rules:
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: wva-node-reader-rolebinding-${suffix}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: wva-node-reader-role-${suffix}
subjects:
- kind: ServiceAccount
  name: ${sa}
  namespace: ${ns}
EOF
}

# pl_grant_policy_read lets one controller READ the policy ConfigMap. Read only,
# never write: the point of putting policy in an admin-owned namespace is that its
# subject cannot change it.
pl_grant_policy_read() {
    local ns="$1" sa="$2" policy_ns="$3" suffix
    suffix="$(wva_ns_suffix "$ns")"
    kubectl apply -f - >/dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: wva-policy-reader
  namespace: ${policy_ns}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: wva-policy-reader-${suffix}
  namespace: ${policy_ns}
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: wva-policy-reader
subjects:
- kind: ServiceAccount
  name: ${sa}
  namespace: ${ns}
EOF
}

# pl_set_limiter writes the limiters list into the policy ConfigMap's default
# entry, preserving every other setting in it.
#
# It REPLACES rather than appends: two limiter kinds in one list is not a chain,
# a quota entry wins over a gpu-inventory one, and the loser is built as nothing.
pl_set_limiter() {
    local policy_ns="$1" limiter="$2" current updated
    current="$(kubectl get configmap wva-scaling-policy-config -n "$policy_ns" \
        -o jsonpath='{.data.default}' 2>/dev/null || true)"
    if [ -z "$current" ]; then
        # A policy ConfigMap that carries ONLY limiters is complete and correct:
        # every other threshold has a default in the controller, and a partial
        # entry does not blank them.
        current="limiters: []"
    fi
    if [ "$limiter" = "none" ]; then
        updated="$(printf '%s\n' "$current" | yq 'del(.limiters)')"
    else
        # Through limiter_entry_yaml, the same builder the installer uses.
        #
        # This path wrote `[{"type": "quota"}]` of its own, which the controller
        # REJECTS ("name must not be empty") -- and a rejected entry costs the
        # whole `default` policy, so publishing it would have stripped the policy
        # from every WVA on the cluster and left them all unbounded, under the
        # banner "The quota limiter is now in force for every WVA on this
        # cluster." Exactly the claim a safety bound must never make falsely.
        # One call, and no status to forget. This path runs from a bare
        # `bash -c` in the Makefile with no `set -e`, so an unchecked failure
        # used to return 0 up the chain and enable_physical_limiter went on to
        # announce a limiter it had not written -- after blanking the policy in
        # every target namespace. policy_declared_limiters composes in THIS
        # shell, so a refusal ends the command rather than handing back an empty
        # string.
        #
        # WVA_QUOTA_NS_KEY=default, and it is not a detail. This ONE policy is
        # read by every controller on the cluster, across every namespace, so a
        # namespace-scoped budget here has to use the reserved per-unlisted-
        # namespace key. Keyed on a single namespace instead -- which is what the
        # installer's own rule would pick -- that namespace gets the budget and
        # every other one gets ZERO, so this command would stop scaling
        # everywhere it was meant to bound it.
        WVA_QUOTA_NS_KEY=default policy_declared_limiters "$limiter" "$current"
        updated="$POLICY_DECLARED"
    fi
    # An entry that is now empty is removed, not written as "{}". A ConfigMap
    # whose default entry is an empty object is a policy that says nothing while
    # looking like one that says something, and the next reader has to open it to
    # find out which.
    if [ "$limiter" = "none" ] && [ "$(printf '%s' "$updated" | tr -d '[:space:]')" = "{}" ]; then
        kubectl delete configmap wva-scaling-policy-config -n "$policy_ns" --ignore-not-found >/dev/null 2>&1
        return 0
    fi
    kubectl create configmap wva-scaling-policy-config -n "$policy_ns" \
        --from-literal=default="$updated" --dry-run=client -o yaml \
        | kubectl label --local -f - app.kubernetes.io/name=workload-variant-autoscaler -o yaml \
        | kubectl apply -f - >/dev/null
}

# Set by enable_physical_limiter before pl_needs_restart is called; bash has no
# clean way to pass it down through the loop's command substitutions.
policy_ns_global=""

enable_physical_limiter() {
    local limiter="${WVA_LIMITER_TYPE:-gpu-inventory}"
    local policy_ns; policy_ns="$(pl_policy_ns)"
    local namespaces ns sa granted=0

    case "$limiter" in
        gpu-inventory|quota) ;;
        *) log_error "WVA_LIMITER_TYPE must be gpu-inventory or quota (got '$limiter')" ;;
    esac
    # Validated HERE, before the namespace is created and the grants are made.
    # pl_set_limiter is the last step of a sequence that has already changed the
    # cluster, so a budget this rejects must stop the command while it still has
    # changed nothing. Discards the output: this call is for its refusals.
    WVA_QUOTA_NS_KEY=default limiter_entry_yaml "$limiter" >/dev/null

    if ! kubectl auth can-i create clusterrolebindings >/dev/null 2>&1; then
        log_error "This is a cluster-admin action: it publishes policy a tenant cannot edit, and grants the node read that policy then requires. You cannot create ClusterRoleBindings on this cluster."
    fi

    namespaces="$(pl_controller_namespaces)"
    if [ -z "$namespaces" ]; then
        log_warning "No WVA controller found on this cluster. Publishing the policy anyway — any WVA installed later reads it at startup."
    fi

    log_info "Publishing cluster policy in $policy_ns (limiter: $limiter)"
    kubectl create namespace "$policy_ns" --dry-run=client -o yaml \
        | kubectl apply -f - >/dev/null
    policy_ns_global="$policy_ns"

    # Grants BEFORE the policy. The other order publishes a limiter that every
    # controller without node access must refuse — a self-inflicted outage across
    # the cluster, arriving as NotReady pods rather than as an error from this
    # command.
    local targets="" restart_list="" cm_ns
    for ns in $namespaces; do
        sa="$(pl_controller_sa "$ns")"
        if [ "$limiter" = "gpu-inventory" ]; then
            pl_grant_node_read "$ns" "$sa"
        fi
        cm_ns="$(pl_policy_cm_ns "$ns")"
        if [ "$cm_ns" = "$policy_ns" ]; then
            pl_grant_policy_read "$ns" "$sa" "$policy_ns"
            pl_needs_restart "$ns" && restart_list="$restart_list $ns"
        fi
        case " $targets " in *" $cm_ns "*) : ;; *) targets="$targets $cm_ns" ;; esac
        log_success "  $ns/$sa — reads policy from ${cm_ns}$([ "$limiter" = "gpu-inventory" ] && echo ', and may list nodes')"
        granted=$((granted + 1))
    done

    # The well-known namespace too, matching what disable already clears — and it
    # is the ONLY target when the cluster has no controller yet. Without it, this
    # command created the policy namespace, wrote nothing into it, and reported
    # that "the limiter is now in force for every WVA on this cluster": the exact
    # claim a safety bound must never make falsely. A WVA installed the next day
    # would have read an empty namespace and scaled with no GPU budget at all.
    case " $targets " in *" $policy_ns "*) : ;; *) targets="$targets $policy_ns" ;; esac

    # Every ConfigMap some controller actually reads, not just the well-known one.
    for cm_ns in $targets; do
        pl_set_limiter "$cm_ns" "$limiter"
    done

    # A controller that started before the well-known namespace existed resolved
    # its policy source to its OWN namespace and will never look again. Without
    # this the command reports success and changes nothing for it.
    for ns in $restart_list; do
        log_info "  Restarting $ns: it resolved its policy source before $policy_ns existed, and that resolution runs once at startup"
        kubectl rollout restart deploy -n "$ns" \
            -l app.kubernetes.io/name=workload-variant-autoscaler >/dev/null 2>&1 || \
            log_warning "    could not restart the controller in $ns — restart it yourself, or it keeps reading its own ConfigMap"
    done

    echo ""
    log_success "The ${limiter} limiter is now in force for every WVA on this cluster."
    log_info "  Policy in:   $(echo $targets | tr ' ' ',') (ConfigMap wva-scaling-policy-config, data.default.limiters)"
    log_info "  Controllers: ${granted} granted"
    if [ -n "${WVA_LIMITER_TARGETS:-}" ]; then
        log_warning "  WVA_LIMITER_TARGETS was set, so only those namespaces were granted. Any OTHER controller on this cluster now reads a policy it cannot honour, and will report NotReady until it is granted too."
    fi
    log_info "  It applies live — controllers reload policy without a restart, and none can opt out."
    log_info "  Check:       kubectl get pods -A -l app.kubernetes.io/name=workload-variant-autoscaler"
    log_info "               a controller that stays NotReady is one this did not reach."
}

disable_physical_limiter() {
    local policy_ns; policy_ns="$(pl_policy_ns)"
    local ns cm_ns targets="" cleared=0

    # The SAME per-controller resolution enable uses. Looking only at the
    # well-known namespace left the limiter in force everywhere it had actually
    # been written — an "off" that reported success and disabled nothing, which
    # is the worse direction for a command whose subject is a safety bound.
    policy_ns_global="$policy_ns"
    for ns in $(pl_controller_namespaces); do
        cm_ns="$(pl_policy_cm_ns "$ns")"
        case " $targets " in *" $cm_ns "*) : ;; *) targets="$targets $cm_ns" ;; esac
    done
    # The well-known namespace too, even with no controller reading it today: a
    # WVA installed tomorrow would read whatever was left there.
    case " $targets " in *" $policy_ns "*) : ;; *) targets="$targets $policy_ns" ;; esac

    for cm_ns in $targets; do
        kubectl get configmap wva-scaling-policy-config -n "$cm_ns" >/dev/null 2>&1 || continue
        pl_set_limiter "$cm_ns" none
        log_success "Removed the limiter from ${cm_ns}/wva-scaling-policy-config"
        cleared=$((cleared + 1))
    done

    if [ "$cleared" -eq 0 ]; then
        log_info "No scaling-policy ConfigMap declared a limiter — nothing to disable."
        return 0
    fi
    log_warning "Scaling is now bounded by nothing WVA knows about: the optimizer allocates without a GPU budget, and a scale-from-zero wake is published without a capacity check."
    log_info "The RBAC is left in place — it grants reads only, and removing it would break the next enable."
}
