#!/usr/bin/env bash
#
# Undeploy and cleanup helpers for deploy/install.sh.
# Requires vars: KEDA_NAMESPACE, MONITORING_NAMESPACE, NAMESPACE, WVA_NS, WVA_PROJECT.
# Requires funcs: containsElement(),
# undeploy_prometheus_stack(), delete_namespaces(), undeploy_epp(), log_*().
#

# wva_scaledobjects_calling_us echoes `namespace/name` for every ScaledObject with
# a trigger pointing at this install's external scaler. Extra args are passed to
# kubectl, so a caller can narrow it with a label selector.
#
# Cluster-wide first, then this namespace: a namespace admin may not list across
# the cluster, and a denied list is not an empty cluster. Folding the two together
# would report "nothing points at the scaler you removed" to precisely the person
# whose own namespace is full of objects that do.
wva_scaledobjects_calling_us() {
    local tmpl='{{range .items}}{{$ns := .metadata.namespace}}{{$n := .metadata.name}}{{range .spec.triggers}}{{if .metadata.scalerAddress}}{{$ns}}/{{$n}} {{.metadata.scalerAddress}}{{"\n"}}{{end}}{{end}}{{end}}'
    local out
    if ! out=$(kubectl get scaledobject -A "$@" -o go-template="$tmpl" 2>/dev/null); then
        out=$(kubectl get scaledobject -n "$WVA_NS" "$@" -o go-template="$tmpl" 2>/dev/null) || return 0
    fi
    printf '%s\n' "$out" | grep -F "wva-external-scaler.${WVA_NS}." | awk '{print $1}' | sort -u || true
}

undeploy_keda() {
    if [ "$ENVIRONMENT" = "openshift" ]; then
        log_info "OpenShift: skipping KEDA uninstall (platform-managed)"
        return
    fi
    if [ "$ENVIRONMENT" = "kubernetes" ] && [ "${KEDA_HELM_INSTALL:-false}" != "true" ]; then
        log_info "Kubernetes: skipping KEDA uninstall (cluster-managed; set KEDA_HELM_INSTALL=true if this script installed KEDA)"
        return
    fi
    log_info "Uninstalling KEDA..."
    helm uninstall "$KEDA_RELEASE_NAME" -n "$KEDA_NAMESPACE" 2>/dev/null || \
        log_warning "KEDA not found or already uninstalled"
    kubectl delete namespace "$KEDA_NAMESPACE" --ignore-not-found --timeout=120s 2>/dev/null || true
    log_success "KEDA uninstalled"
}

# wva_undeploy_watch_namespace_rbac removes the Role and RoleBinding an
# admin-owned controller was given in the namespace it managed.
#
# The namespace is read from the live Deployment rather than from WVA_WATCH_NS,
# because an uninstall is typically run without the variables the install had —
# and RBAC granted in someone else's namespace is the last thing that should
# depend on the operator remembering to repeat a flag. Called before the
# Deployment is deleted, while it can still be asked.
wva_undeploy_watch_namespace_rbac() {
    local watched role binding
    watched="$(kubectl get deploy -n "$WVA_NS" -l app.kubernetes.io/name=workload-variant-autoscaler \
        -o jsonpath='{.items[0].spec.template.spec.containers[0].env[?(@.name=="WVA_WATCH_NAMESPACE")].value}' 2>/dev/null || true)"
    # The base leaves this as the literal $(POD_NAMESPACE) — Kubernetes expands it
    # inside the container, so the value ON THE OBJECT is the placeholder, and it
    # means "the controller's own namespace". Only an install with WVA_WATCH_NS
    # patches a real name in.
    #
    # Without this, the placeholder is non-empty and unequal to WVA_NS, so both
    # guards below pass: an ordinary self-managing install ran two deletes against
    # a namespace literally called $(POD_NAMESPACE) and then reported "Removed the
    # controller's permissions in $(POD_NAMESPACE)". Harmless — nothing of that
    # name exists — but it read like a bug in the uninstall, which is a poor thing
    # to be wondering about while uninstalling. Same normalization as
    # pl_policy_ns_for in physical_limiter.sh.
    case "$watched" in
        ''|'$(POD_NAMESPACE)') return 0 ;;
    esac
    [ "$watched" != "$WVA_NS" ] || return 0

    read -r role binding <<< "$(wva_watch_rbac_names)"
    kubectl delete rolebinding "$binding" -n "$watched" --ignore-not-found > /dev/null 2>&1 || true
    kubectl delete role "$role" -n "$watched" --ignore-not-found > /dev/null 2>&1 || true
    log_info "Removed the controller's permissions in $watched (Role/$role, RoleBinding/$binding)"
}

undeploy_wva_controller() {
    log_info "Uninstalling Workload-Variant-Autoscaler..."
    wva_undeploy_watch_namespace_rbac

    local kustomize_overlay
    kustomize_overlay="$(wva_overlay_dir)"

    local tmp_overlay
    tmp_overlay=$(mktemp -d)
    # The SAME base the install built, not a bare symlink to the overlay. A bare
    # symlink skips the WVA_ADMIN_GRANTS re-render, so the undeploy did not know
    # about the node-reader ClusterRole and Binding the install had created and
    # left both behind — found on a real cluster, by counting what survived.
    # Third time this file has paid for deploy and undeploy building the overlay
    # differently; there is now one function and both call it.
    wva_prepare_overlay_base "$tmp_overlay"
    cat > "$tmp_overlay/kustomization.yaml" <<EOF
namespace: $WVA_NS
resources:
- ./base
EOF

    # The SAME rename the install applied. Without it, `delete -k` resolves the
    # FIXED binding names and removes whichever install currently owns them — so
    # uninstalling one WVA stripped a different, healthy one of its permissions —
    # while leaking this install's own suffixed bindings. Found by installing and
    # uninstalling side by side on kind.
    wva_append_crb_name_patches "$tmp_overlay/kustomization.yaml" "$WVA_NS"

    # Bindings the overlay no longer renders cannot be reached by the filtered
    # delete below -- it removes what is rendered, and these are not. Without this
    # they survive every uninstall, forever.
    wva_delete_legacy_crbs "$WVA_NS"

    # Render and FILTER, rather than `kubectl delete -k`. One kind in the rendered
    # set does not belong to this install and must survive it:
    #
    #   Namespace    the overlay carries one, renamed to $WVA_NS. Deleting it
    #                cascades to everything inside — and a namespace-scoped install
    #                is deliberately placed IN the namespace holding the model
    #                servers, so `delete -k` took the tenant's workloads with it
    #                while the summary said "Namespaces preserved".
    #
    # ClusterRoles used to be filtered out too, because they were shared under
    # fixed names and deleting them left every other WVA's binding pointing at
    # nothing. They are now suffixed per install (WVA_OWNED_CLUSTER_ROLES), so each
    # install owns its own and can take them with it. An install made before that
    # change has unsuffixed roles: the render names only suffixed ones, so this
    # leaves those alone rather than deleting something another install may bind —
    # a leak, which is the safe direction.
    #
    # The suffixed ClusterRoles and ClusterRoleBindings, ServiceAccounts,
    # ConfigMaps, Services, Deployment and ServiceMonitor are this install's own.
    # Forbidden is not "already gone". A namespace owner can delete the controller
    # and everything beside it, and cannot delete the cluster-scoped RBAC an admin
    # created for them — so this delete partially fails for exactly the person the
    # tenant install is written for. Reporting that as "not found or already
    # removed" told them the uninstall was complete while cluster-scoped objects
    # carrying their namespace's name stayed on the cluster, with nothing said and
    # no admin command offered.
    local delete_err
    delete_err="$(kubectl kustomize "$tmp_overlay" 2>/dev/null \
        | yq 'select(.kind != "Namespace")' \
        | kubectl delete -f - --ignore-not-found 2>&1 >/dev/null)" && delete_err=""
    if [ -n "$delete_err" ]; then
        if printf '%s' "$delete_err" | grep -qi 'forbidden'; then
            log_warning "Some objects could not be removed because you are not allowed to delete them:"
            printf '%s\n' "$delete_err" | grep -i forbidden | sed 's/^/    /'
            log_warning "  Everything you CAN delete was deleted. The rest are the admin-owned ones — ask a cluster admin to run this uninstall for $WVA_NS."
        else
            log_warning "Some Workload-Variant-Autoscaler resources were not removed:"
            printf '%s\n' "$delete_err" | sed 's/^/    /'
        fi
    fi
    rm -rf "$tmp_overlay"

    log_info "Left in place: namespace $WVA_NS. Delete it yourself if you want it gone."

    # ScaledObjects outlive the controller, and that is a trap rather than a
    # convenience: their trigger points at wva-external-scaler.$WVA_NS, which no
    # longer exists. KEDA keeps the HPA and keeps calling a scaler that is gone.
    #
    # For a workload parked at zero — and scale-to-zero is ON by default — nothing
    # can wake it: activation only ever arrives from the scaler. That is a silent
    # outage that SURVIVES the uninstall, so it is worth a loud, specific list
    # rather than a general warning.
    # What we created, we remove. What we only pointed at WVA, we report: an
    # `apply: adopt` object belongs to whoever wrote it — it may be GitOps-managed
    # or hand-tuned, and deleting it would take a workload's autoscaling away
    # entirely rather than just taking WVA out of it. So the split is by who made
    # the object, not by who it currently calls.
    local orphans ours theirs
    orphans=$(wva_scaledobjects_calling_us)
    ours=$(wva_scaledobjects_calling_us \
        -l 'app.kubernetes.io/managed-by=workload-variant-autoscaler,app.kubernetes.io/component=default-scaledobject')
    theirs=$(comm -13 <(printf '%s\n' $ours | sort -u) <(printf '%s\n' $orphans | sort -u) | grep -v '^$' || true)

    if [ -n "$ours" ] && [ "${UNDEPLOY_SCALEDOBJECTS:-true}" = "true" ]; then
        log_info "Removing the ScaledObjects this installer created:"
        local so ns name
        for so in $ours; do
            ns="${so%%/*}"; name="${so#*/}"
            if kubectl delete scaledobject "$name" -n "$ns" --ignore-not-found > /dev/null 2>&1; then
                log_success "  $so removed"
            else
                log_warning "  $so could not be removed — delete it by hand, or its HPA will keep calling a scaler that is gone"
            fi
        done
        # Worth saying out loud: these carry restoreToOriginalReplicaCount, so KEDA
        # puts each workload back to the count it had before it was autoscaled. An
        # uninstall that quietly resizes running workloads is a surprise; an
        # uninstall that says it is doing so is just an uninstall.
        log_info "  KEDA restores each workload to its pre-autoscaling replica count as these go."
    elif [ -n "$ours" ]; then
        log_warning "Kept the ScaledObjects this installer created (UNDEPLOY_SCALEDOBJECTS=false). Their HPAs now call a scaler that is gone."
        printf '    %s\n' $ours
    fi

    # Whatever is left calls our scaler but was not ours to make.
    if [ -n "$theirs" ]; then
        log_warning "These ScaledObjects were not created by this installer, and still point at the external scaler you just removed:"
        printf '    %s\n' $theirs
        log_warning "  KEDA will keep their HPAs and keep calling a scaler that is gone. Any of them parked at zero CANNOT BE WOKEN — activation only comes from the scaler."
        log_warning "  Repoint their trigger at another WVA, or at whatever scaled them before, or delete them. They are not this installer's to remove."
    fi

    rm -f "$PROM_CA_CERT_PATH"

    log_success "WVA uninstalled"
}

cleanup() {
    log_info "Starting undeployment process..."
    log_info "======================================"
    echo ""

    # Shared infrastructure — Prometheus, KEDA, llm-d's EPP — is left alone unless
    # you ask for it, and this default is not symmetry with the install.
    #
    # It used to key off DEPLOY_PROMETHEUS / SCALER_BACKEND, whose defaults say what
    # a FRESH install would deploy, not what THIS install actually did. So
    # `make undeploy-wva-on-k8s WVA_NS=mine` on a cluster that already had them tore
    # down the cluster's Prometheus, KEDA and EPP — infrastructure the install never
    # touched and other things depend on. Reproduced here: an undeploy of a scratch
    # namespace emptied the monitoring namespace, and every WVA on the cluster lost
    # its metrics backend.
    #
    # Removing WVA is the job. Removing what WVA was pointed AT is a separate
    # decision, and now an explicit one.
    if [ "${UNDEPLOY_SHARED:-false}" = "true" ]; then
        log_warning "UNDEPLOY_SHARED=true: also removing Prometheus, the scaler backend and EPP. Anything else on this cluster using them will lose them."
        if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
            undeploy_prometheus_stack
        fi
        if [ "$SCALER_BACKEND" = "keda" ]; then
            undeploy_keda
        elif [ "$SCALER_BACKEND" = "none" ]; then
            log_info "Skipping scaler backend undeployment (SCALER_BACKEND=none)"
        else
            log_warning "Unsupported SCALER_BACKEND: $SCALER_BACKEND (supported: keda, none); skipping scaler backend undeployment — any installed backend may be left behind"
        fi
        undeploy_epp
    else
        log_info "Leaving Prometheus, the scaler backend and EPP in place — they are shared and this install may not have created them. Pass UNDEPLOY_SHARED=true to remove them too."
    fi

    if [ "$DEPLOY_WVA" = "true" ]; then
        undeploy_wva_controller
    fi

    # Delete namespaces if requested
    if [ "$DELETE_NAMESPACES" = "true" ] || [ "$DELETE_CLUSTER" = "true" ]; then
        delete_namespaces
    else
        log_info "Keeping namespaces (use --delete-namespaces or set DELETE_NAMESPACES=true to remove)"
    fi

    echo ""
    log_success "Undeployment complete!"
    echo ""
    echo "=========================================="
    echo " Undeployment Summary for $ENVIRONMENT"
    echo "=========================================="
    echo ""
    # Report what was actually removed. These lines used to key off
    # SCALER_BACKEND / DEPLOY_PROMETHEUS, which describe what an INSTALL would
    # deploy — so on the default path, twenty lines after saying "Leaving
    # Prometheus, the scaler backend and EPP in place", the summary claimed it had
    # removed them.
    echo "Removed components:"
    [ "$DEPLOY_WVA" = "true" ] && echo "✓ WVA Controller (its namespaced objects, and the ClusterRoles and ClusterRoleBindings suffixed for $WVA_NS)"
    if [ "${UNDEPLOY_SHARED:-false}" = "true" ]; then
        [ "$SCALER_BACKEND" = "keda" ] && echo "✓ KEDA"
        [ "$DEPLOY_PROMETHEUS" = "true" ] && echo "✓ Prometheus Stack"
        echo "✓ EPP"
    else
        echo ""
        echo "Left in place (shared; pass UNDEPLOY_SHARED=true to remove):"
        echo "  - Prometheus, the scaler backend (KEDA), EPP"
    fi

    if [ "$DELETE_NAMESPACES" = "true" ]; then
        echo "✓ Namespaces"
    else
        # Only the ones that are actually there. This used to print a fixed list,
        # so an uninstall claimed to have "preserved" namespaces that had never
        # existed on that cluster — which reads as "I found these and left them".
        local ns preserved=()
        for ns in "$NAMESPACE" "$WVA_NS" "$MONITORING_NAMESPACE" \
            $([ "$SCALER_BACKEND" = "keda" ] && echo "$KEDA_NAMESPACE"); do
            [ -n "$ns" ] || continue
            # Deduped: in a namespace-scoped install NAMESPACE and WVA_NS are the
            # same namespace, so the list printed it twice and read as though two
            # things had been left behind.
            local seen=""
            for already in ${preserved[@]+"${preserved[@]}"}; do
                [ "$already" = "$ns" ] && { seen=yes; break; }
            done
            [ -n "$seen" ] && continue
            kubectl get namespace "$ns" >/dev/null 2>&1 && preserved+=("$ns")
        done
        if [ ${#preserved[@]} -ne 0 ]; then
            echo ""
            echo "Namespaces preserved:"
            printf '  - %s\n' "${preserved[@]}"
        fi
    fi
    echo ""
    echo "=========================================="
}
