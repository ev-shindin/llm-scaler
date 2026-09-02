#!/usr/bin/env bash
#
# Shared WVA-specific deployment helpers.
# Requires vars: WVA_NS, NAMESPACE, MONITORING_NAMESPACE, WVA_PROJECT,
# chart/image values, env mode lists.
# Requires funcs: log_info/log_warning/log_success/log_error, containsElement().
#

set_tls_verification() {
    log_info "Setting TLS verification..."

    # Auto-detect TLS verification setting if not specified
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
            SKIP_TLS_VERIFY="true"
            log_info "Emulated environment detected - enabling TLS skip verification for self-signed certificates"
    else
        case "$ENVIRONMENT" in
            "kubernetes")
                # TODO: change to false when Kubernetes support for TLS verification is enabled
                SKIP_TLS_VERIFY="true"
                log_info "Kubernetes cluster - enabling TLS skip verification for self-signed certificates"
                ;;
            "openshift")
                # For OpenShift, we can use proper TLS verification since we have the Service CA
                # However, defaulting to true for now to match current behavior
                # TODO: Set to false once Service CA certificate extraction is fully validated
                SKIP_TLS_VERIFY="true"
                log_info "OpenShift cluster - TLS verification setting: $SKIP_TLS_VERIFY"
                ;;
            *)
                SKIP_TLS_VERIFY="true"
                log_warning "Unknown environment - enabling TLS skip verification for self-signed certificates"
                ;;
        esac
    fi

    export SKIP_TLS_VERIFY

    log_success "Successfully set TLS verification to: $SKIP_TLS_VERIFY"
}

set_wva_logging_level() {
    log_info "Setting WVA logging level..."

    # Set logging level based on environment
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
        WVA_LOG_LEVEL="debug"
        log_info "Development environment - using debug logging"
    else
        WVA_LOG_LEVEL="info"
        log_info "Production environment - using info logging"
    fi

    export WVA_LOG_LEVEL
    log_success "WVA logging level set to: $WVA_LOG_LEVEL"
    echo ""
}

# wva_watch_rbac_names echoes `role binding` — the names this install owns in the
# namespace it manages. Suffixed by the controller's namespace, so two admin-owned
# controllers pointed at one tenant do not overwrite or delete each other's.
wva_watch_rbac_names() {
    local suffix
    suffix="$(wva_ns_suffix "$WVA_NS")"
    printf 'wva-manager-role-%s wva-manager-rolebinding-%s' "$suffix" "$suffix"
}

# wva_apply_watch_namespace_rbac grants the controller its permissions in the
# namespace it MANAGES, when that is not the namespace it RUNS IN.
#
# The overlay puts the Role and RoleBinding in the controller's own namespace,
# which is right when those are the same namespace and useless when they are not:
# a controller in an admin-owned namespace watching a tenant's could not read a
# ConfigMap there, and crash-looped on its first bootstrap read. So the one
# configuration that puts a controller beyond the reach of the tenant it bounds
# was the one configuration that could not run.
#
# It is applied in the PREREQS phase because it is an admin's act by definition:
# writing RBAC into someone else's namespace is precisely what the tenant cannot
# do, and what they must not be able to undo.
#
# The Role is a copy of the rendered one rather than a second definition, so it
# cannot drift from the permissions the controller actually needs.
wva_apply_watch_namespace_rbac() {
    local tmp_overlay="$1" role binding rendered
    [ -n "${WVA_WATCH_NS:-}" ] || return 0
    [ "$WVA_WATCH_NS" != "$WVA_NS" ] || return 0

    read -r role binding <<< "$(wva_watch_rbac_names)"
    rendered=$(kubectl kustomize "$tmp_overlay" 2>/dev/null         | WVA_TARGET_NS="$WVA_WATCH_NS" WVA_ROLE_NAME="$role" yq '
            select(.kind == "Role" and (.metadata.name | test("manager-role$")))
            | .metadata.name = strenv(WVA_ROLE_NAME)
            | .metadata.namespace = strenv(WVA_TARGET_NS)') || true
    if [ -z "$rendered" ]; then
        log_error "Could not find the manager Role in the rendered overlay, so the controller would have no permissions in $WVA_WATCH_NS. Refusing to install a controller that cannot read the namespace it manages."
    fi

    log_info "Granting the controller its permissions in $WVA_WATCH_NS (it runs in $WVA_NS)"
    printf '%s
' "$rendered" | kubectl apply -f - > /dev/null
    kubectl apply -f - > /dev/null <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${binding}
  namespace: ${WVA_WATCH_NS}
  labels:
    app.kubernetes.io/managed-by: workload-variant-autoscaler
subjects:
- kind: ServiceAccount
  name: wva-controller-manager
  namespace: ${WVA_NS}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ${role}
EOF
    log_success "  Role/$role and RoleBinding/$binding in $WVA_WATCH_NS"
}

deploy_wva_controller() {
    log_info "Deploying Workload-Variant-Autoscaler..."
    log_info "Using image: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"

    # Select the Kustomize overlay by install SCOPE (WVA_SCOPE=cluster|namespace)
    # and platform. Both scopes work on both platforms; the default preserves the
    # historical inference. See wva_overlay_dir.
    local kustomize_overlay
    kustomize_overlay="$(wva_overlay_dir)"

    # Build a throw-away overlay that pins the image without modifying tracked files.
    local tmp_overlay
    tmp_overlay=$(mktemp -d)
    trap 'rm -rf "$tmp_overlay"' EXIT

    # Shared with the preflight and the undeploy, which render the SAME shape.
    # See wva_prepare_overlay_base.
    wva_prepare_overlay_base "$tmp_overlay"

    # Symlink prometheus-alerts component if needed
    if [ "${DEPLOY_ALERTING_RULES:-false}" = "true" ]; then
        ln -s "$WVA_PROJECT/config/components/prometheus-alerts" "$tmp_overlay/prometheus-alerts"
    fi

    # NOTE: the FMA launcher PodMonitor is deliberately NOT wired in here. This
    # overlay renders into the CONTROLLER's namespace, and launcher pods live in
    # the workload namespace — which differs whenever WVA_WATCH_NS is set, and is
    # plural for a cluster-scoped install. It is applied per workload namespace
    # instead; see deploy_fma_launcher_podmonitor.

    # config/base/manager/kustomization.yaml transforms the base image name "controller"
    # to the published release image. The overlay must match the POST-transform name.
    local base_image
    base_image=$(grep 'newName:' "$WVA_PROJECT/config/base/manager/kustomization.yaml" | awk '{print $2}' | head -1)
    # A MOVING tag has to be re-pulled, or a redeploy silently runs old code.
    #
    # config/base/manager/deployment.yaml pins imagePullPolicy: IfNotPresent,
    # which is correct for a digest or a release tag and wrong for :main. A node
    # that cached :main once keeps it forever: measured on pokprod, a controller
    # restarted on 2026-08-19 was still running an image built 2026-08-14, six
    # days and many commits stale. Two log lines that had been added in between
    # were simply absent from the binary, which reads exactly like a metrics
    # signal that never arrives -- and cost a long investigation to tell apart.
    #
    # Not forced to Always unconditionally: the kind e2e builds the image and
    # loads it into the cluster under this same :main tag, with no registry
    # behind it, so Always there would pull the REMOTE image over the one under
    # test. That path sets WVA_IMAGE_PULL_POLICY=IfNotPresent explicitly.
    local pull_policy="${WVA_IMAGE_PULL_POLICY:-}"
    if [ -z "$pull_policy" ]; then
        case "$WVA_IMAGE_TAG" in
            main|latest|dev|nightly) pull_policy=Always ;;
            *)                       pull_policy=IfNotPresent ;;
        esac
    fi

    cat > "$tmp_overlay/pull-policy-patch.yaml" <<EOF
- op: replace
  path: /spec/template/spec/containers/0/imagePullPolicy
  value: $pull_policy
EOF

    cat > "$tmp_overlay/kustomization.yaml" <<EOF
namespace: $WVA_NS
resources:
- ./base
images:
- name: $base_image
  newName: $WVA_IMAGE_REPO
  newTag: "$WVA_IMAGE_TAG"
EOF

    # Include Prometheus alerting rules component if requested
    if [ "${DEPLOY_ALERTING_RULES:-false}" = "true" ]; then
        log_info "Including Prometheus alerting rules component..."
        printf 'components:\n' >> "$tmp_overlay/kustomization.yaml"
        printf -- '- ./prometheus-alerts\n' >> "$tmp_overlay/kustomization.yaml"
    fi

    # Give this install its own ClusterRoleBinding names.
    #
    # Every overlay applies these under FIXED names, and an apply replaces a
    # ClusterRoleBinding's subject list — so a second install anywhere on the
    # cluster repoints them at its own namespace and leaves the first controller's
    # ServiceAccount with no permissions. Silently: no error, no restart, no event,
    # just every API call failing afterwards.
    #
    # This was already done here, but only for OpenShift, on the theory that shared
    # clusters were an OpenShift problem. They are not: the same apply does the same
    # thing on any cluster, and it is reproducible on kind in one command. Suffixing
    # unconditionally makes concurrent installs merely unwise rather than
    # destructive — check_single_installation still stops them by default.
    #
    # Upgrades of an install that predates this keep working: the old un-suffixed
    # binding still names the same ServiceAccount, so nothing is lost while it
    # lingers, and `undeploy` removes both.
    wva_append_crb_name_patches "$tmp_overlay/kustomization.yaml" "$WVA_NS"

    # Appended here, not in the heredoc above: wva_append_crb_name_patches writes
    # its own `patches:` key, and a second one is a duplicate mapping key that
    # kustomize resolves by keeping the last -- which silently dropped this patch
    # and left a stale image running while the deploy reported success.
    grep -q '^patches:' "$tmp_overlay/kustomization.yaml" || printf 'patches:
' >> "$tmp_overlay/kustomization.yaml"
    cat >> "$tmp_overlay/kustomization.yaml" <<EOF
- path: pull-policy-patch.yaml
  target:
    kind: Deployment
    labelSelector: control-plane=controller-manager
EOF

    # Prune on INSTALL as well as uninstall. Waiting for an uninstall would leave
    # the grant in place on every cluster that already has it, since upgrading is
    # what people actually do -- and the whole reason the binding was dropped is
    # that it handed out access nobody needed.
    wva_delete_legacy_crbs "$WVA_NS"

    # WVA_REPLICAS runs the controller with a warm standby.
    #
    # The manifest already sets --leader-elect=true, and cmd/main.go exposes the
    # lease/renew/retry durations, so the capability was there — the Deployment just
    # had no replicas field (defaulting to 1) and the installer no way to set one.
    #
    # This is FAILOVER, not throughput: only the leader runs the optimization loops,
    # and the standbys sit on the lease doing nothing until it lapses. Two replicas
    # turn a node drain or a crash from "no scaling decisions until the pod is
    # rescheduled" into a lease timeout.
    if [ -n "${WVA_REPLICAS:-}" ] && [ "${WVA_REPLICAS}" != "1" ]; then
        log_info "Controller replicas: $WVA_REPLICAS (leader-elected; standbys take over on failure, they do not share the work)"
        cat >> "$tmp_overlay/kustomization.yaml" <<EOF
- patch: |-
    - op: add
      path: /spec/replicas
      value: ${WVA_REPLICAS}
  target:
    kind: Deployment
    name: wva-controller-manager
EOF
    fi

    # WVA_WATCH_NS separates the namespace the controller MANAGES from the one it
    # RUNS IN.
    #
    # Without it a namespace-scoped controller manages its own namespace, so
    # bounding a tenant means running the controller inside the tenant's namespace —
    # where the tenant owns this Deployment, and can therefore edit its args, its
    # env, or its image. No setting carried by a Deployment its subject controls can
    # bind that subject. Put the controller in a namespace the admin owns and point
    # it at the tenant's, and the bound holds because the tenant cannot reach it.
    if [ -n "${WVA_WATCH_NS:-}" ]; then
        if [ "$(wva_install_scope)" != "namespace" ]; then
            log_error "WVA_WATCH_NS only applies to a namespace-scoped install (WVA_SCOPE=namespace). A cluster-scoped controller manages every namespace."
        fi
        if ! kubectl get namespace "$WVA_WATCH_NS" >/dev/null 2>&1; then
            log_error "WVA_WATCH_NS=$WVA_WATCH_NS does not exist. Create the tenant namespace before installing."
        fi
        if [ "$WVA_WATCH_NS" = "$WVA_NS" ]; then
            log_warning "WVA_WATCH_NS equals WVA_NS: the controller runs in the namespace it manages. If that namespace belongs to a tenant, they own this Deployment and can lift any limit set on them."
        else
            log_info "Controller runs in $WVA_NS and manages $WVA_WATCH_NS"
        fi
        # Strategic merge, which matches env entries by NAME. A JSON-patch index
        # into the env array would silently rewrite whichever variable happened to
        # sit at that position the day someone added one above it.
        #
        # The explicit `target:` is required, not decoration. Without one, kustomize
        # derives the target from the patch's own metadata — including a namespace
        # the patch does not carry — while the overlay's namespace transformer has
        # already stamped one on the Deployment. The two never match, and kustomize
        # fails the whole build with "no matches for Id
        # Deployment.v1.apps/wva-controller-manager.[noNs]", so the install of the
        # one configuration that puts the controller beyond a tenant's reach was the
        # one configuration that could not be installed.
        cat >> "$tmp_overlay/kustomization.yaml" <<EOF
- patch: |-
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: wva-controller-manager
    spec:
      template:
        spec:
          containers:
          - name: manager
            env:
            - name: WVA_WATCH_NAMESPACE
              value: "${WVA_WATCH_NS}"
  target:
    kind: Deployment
    name: wva-controller-manager
EOF
    fi


    # WVA_APPLY_SCOPE splits one overlay across the two install phases:
    #
    #   prereqs     only the admin-owned kinds (see WVA_PREREQ_KINDS)
    #   controller  everything else — what a namespace admin may create once the
    #               prereqs exist
    #   all         both, which is the single-command install and the default
    #
    # Rendering once and filtering, rather than maintaining two overlays, is what
    # keeps the phases from drifting apart: a resource added to the base lands in
    # exactly one phase, decided by its kind, with nothing to keep in sync.
    # Before the apply, and only for the phases that carry the bindings.
    case "${WVA_APPLY_SCOPE:-all}" in
        all|prereqs) wva_repair_immutable_rolerefs ;;
    esac

    case "${WVA_APPLY_SCOPE:-all}" in
        all|prereqs) wva_apply_watch_namespace_rbac "$tmp_overlay" ;;
    esac

    # A private registry needs the pull secret on the ServiceAccount, not on the
    # Deployment: patching the SA covers the controller and anything else that
    # runs as it, and survives a re-apply of the Deployment from the overlay.
    # Applied before the Deployment so the first pod schedules with it rather
    # than starting in ImagePullBackOff and recovering later.
    # A private registry needs the pull secret on the ServiceAccount rather than
    # the Deployment: the SA covers the controller and anything else running as
    # it, and survives a re-apply of the Deployment from the overlay. Applied
    # after the objects exist, below — this only validates it now, so a missing
    # secret is an error before anything is created rather than an
    # ImagePullBackOff afterwards.
    if [ -n "${WVA_IMAGE_PULL_SECRET:-}" ] && [ "${WVA_APPLY_SCOPE:-all}" != "prereqs" ]; then
        kubectl get secret "$WVA_IMAGE_PULL_SECRET" -n "$WVA_NS" >/dev/null 2>&1 || \
            log_error "WVA_IMAGE_PULL_SECRET=$WVA_IMAGE_PULL_SECRET does not exist in $WVA_NS. Create it first:
    kubectl create secret docker-registry $WVA_IMAGE_PULL_SECRET -n $WVA_NS \\
        --docker-server=<registry> --docker-username=<user> --docker-password=<token>"
    fi

    log_info "Applying Kustomize overlay: $kustomize_overlay (scope: ${WVA_APPLY_SCOPE:-all})"
    case "${WVA_APPLY_SCOPE:-all}" in
        all)
            kubectl apply -k "$tmp_overlay"
            ;;
        prereqs)
            kubectl kustomize "$tmp_overlay" \
                | yq "$(wva_prereq_kind_filter select)" \
                | kubectl apply -f -
            ;;
        controller)
            kubectl kustomize "$tmp_overlay" \
                | yq "$(wva_prereq_kind_filter exclude)" \
                | kubectl apply -f -
            ;;
        *)
            log_error "WVA_APPLY_SCOPE must be all, prereqs or controller (got '${WVA_APPLY_SCOPE}')"
            ;;
    esac

    # The ServiceAccount exists now, so the pull secret can go on it. The pod may
    # already be starting; a Deployment restart makes the next pod pick it up
    # rather than waiting out an ImagePullBackOff.
    if [ -n "${WVA_IMAGE_PULL_SECRET:-}" ] && [ "${WVA_APPLY_SCOPE:-all}" != "prereqs" ]; then
        log_info "Image pull secret: $WVA_IMAGE_PULL_SECRET (on ServiceAccount wva-controller-manager)"
        kubectl patch serviceaccount wva-controller-manager -n "$WVA_NS" \
            -p "{\"imagePullSecrets\":[{\"name\":\"${WVA_IMAGE_PULL_SECRET}\"}]}" >/dev/null || \
            log_warning "  could not patch the ServiceAccount — the controller will not be able to pull from a private registry"
        kubectl rollout restart deployment/wva-controller-manager -n "$WVA_NS" >/dev/null 2>&1 || true
    fi

    # Everything below this point patches namespaced ConfigMaps that the
    # controller phase owns and the prereqs phase has not created yet.
    if [ "${WVA_APPLY_SCOPE:-all}" = "prereqs" ]; then
        log_success "Admin-owned resources applied for $WVA_NS: $(printf '%s ' "${WVA_PREREQ_KINDS[@]}")"
        return 0
    fi

    # Clean up a previously-deployed PrometheusRule when alerting rules are disabled.
    # The `get` guard keeps the common path quiet (only logs/acts when a rule actually
    # exists) and tolerates the PrometheusRule CRD being absent — `--ignore-not-found`
    # only covers a missing object, not a missing resource type.
    if [ "${DEPLOY_ALERTING_RULES:-false}" = "false" ] && \
        kubectl get prometheusrule controller-manager-alerts -n "$WVA_NS" &> /dev/null; then
        log_info "DEPLOY_ALERTING_RULES=false - removing existing PrometheusRule controller-manager-alerts"
        kubectl delete prometheusrule controller-manager-alerts -n "$WVA_NS" --ignore-not-found=true
    fi

# wva_reconcile_prometheus_scheme makes the rest of the Prometheus settings agree
# with the URL's scheme.
#
# WVA refuses a plain-HTTP Prometheus unless told to allow it, refuses to send a
# bearer token over one, and refuses TLS settings alongside one — three separate
# checks, all of them right. The installer wrote the URL and left the other three
# as they were, so pointing at a plain-HTTP Prometheus produced a ConfigMap the
# controller would not start on.
#
# It did not fail the install, which is the part that made it dangerous: the pod
# had already started on the previous value and kept running on it. The install
# reported success, the controller ran for days, and the CrashLoopBackOff arrived
# with the next unrelated restart — a node drain, an upgrade, an eviction — long
# after anyone would connect it to an install-time setting.
#
# So the settings move together with the scheme, or not at all.
wva_reconcile_prometheus_scheme() {
    local url="$1"
    case "$url" in
        http://*) : ;;
        *) return 0 ;;
    esac

    log_warning "This Prometheus speaks plain HTTP, so WVA will read metrics over an unencrypted connection."
    log_warning "  Enabling PROMETHEUS_ALLOW_HTTP and dropping the TLS settings, which WVA rejects alongside an http:// URL."
    log_warning "  Pass PROMETHEUS_URL=https://<host>:<port> to use a TLS endpoint instead."
    patch_manager_config '.PROMETHEUS_ALLOW_HTTP = "true"
        | .PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = "false"
        | del(.PROMETHEUS_CA_CERT_PATH)'

    # The token path is Deployment env, and env beats the ConfigMap: leaving it set
    # trips "refusing to send bearer token authentication over plain HTTP" even
    # once the ConfigMap agrees. Emptied by NAME through a strategic merge, so it
    # does not depend on where in the list it happens to sit.
    kubectl patch deployment wva-controller-manager -n "$WVA_NS" --type=strategic -p '{
        "spec":{"template":{"spec":{"containers":[{"name":"manager",
        "env":[{"name":"PROMETHEUS_TOKEN_PATH","value":""}]}]}}}}' > /dev/null 2>&1 || true
}

    # Point the controller at THIS cluster's Prometheus.
    #
    # PROMETHEUS_URL used to be accepted, logged, and then dropped: the shipped
    # ConfigMap hardcodes a kube-prometheus-stack address in the monitoring
    # namespace and nothing ever overwrote it. Installing against a Prometheus this
    # script did not deploy therefore produced a controller that could not reach
    # one — and WVA exits on that ("CRITICAL: Failed to connect to Prometheus"), so
    # it presented as CrashLoopBackOff, reading like a cluster fault rather than a
    # setting that was silently ignored.
    # Nobody set one: the value in PROMETHEUS_URL is the platform default, which
    # names the Prometheus this installer would deploy. On a cluster that already
    # has its own, that is wrong, and WVA exits on a Prometheus it cannot reach —
    # arriving as CrashLoopBackOff, which reads like a broken image rather than a
    # setting nobody was asked for. So look before falling back to it.
    if [ -z "${PROMETHEUS_URL_EXPLICIT:-}" ]; then
        local detected
        detected="$(wva_detect_prometheus_url)"
        if [ -n "$detected" ] && [ "$detected" != "${PROMETHEUS_URL:-}" ]; then
            log_info "Using the Prometheus already on this cluster: $detected"
            log_info "  (the default names the one this installer would deploy; pass PROMETHEUS_URL=<url> to override)"
            PROMETHEUS_URL="$detected"
        fi
    fi

    if [ -n "${PROMETHEUS_URL:-}" ]; then
        log_info "Pointing WVA at Prometheus: $PROMETHEUS_URL"
        patch_manager_config ".PROMETHEUS_BASE_URL = \"$PROMETHEUS_URL\""
        wva_reconcile_prometheus_scheme "$PROMETHEUS_URL"
    fi
    if [ -n "${PROMETHEUS_TLS_INSECURE_SKIP_VERIFY:-}" ]; then
        patch_manager_config ".PROMETHEUS_TLS_INSECURE_SKIP_VERIFY = \"${PROMETHEUS_TLS_INSECURE_SKIP_VERIFY}\""
    fi

    if [ "${ENABLE_SCALE_TO_ZERO:-false}" = "true" ]; then
        log_info "Enabling scale-to-zero in WVA ConfigMap (ENABLE_SCALE_TO_ZERO=true)..."
        patch_manager_config '.WVA_SCALE_TO_ZERO = "true"'
    fi

    # Declare a GPU limiter, if asked.
    #
    # The shipped config declares NONE, deliberately: a GPU-aware optimizer can
    # only allocate to a variant whose accelerator it resolves, so turning it on
    # by default would silently freeze every workload that sets no GPU
    # nodeSelector. It is an install-time CHOICE for a fleet that is ready for it —
    # see docs/concepts/gpu-capacity-accounting.md.
    #
    # Patches the "default" entry rather than shipping a second copy of it: the
    # entry carries every other default too, and a duplicate would drift.
    case "${WVA_LIMITER:-none}" in
        none) ;;
        gpu-inventory|quota)
            log_info "Declaring the ${WVA_LIMITER} limiter in the scaling-policy ConfigMap ..."
            local policy_cm current_default updated_default
            # `|| true` because a no-match is grep exit 1, which pipefail turns into
            # a failed assignment and set -e turns into an exit — before the
            # log_error below can say which ConfigMap is missing.
            policy_cm="$(kubectl get configmap -n "$WVA_NS" -o name 2>/dev/null \
                | grep -E "configmap/(wva-)?scaling-policy-config$" | head -1 | cut -d/ -f2 || true)"
            if [ -z "$policy_cm" ]; then
                log_error "No scaling-policy ConfigMap found in $policy_ns"
            fi
            current_default=$(kubectl get configmap "$policy_cm" -n "$WVA_NS" -o jsonpath='{.data.default}')
            if [ -z "$current_default" ]; then
                log_error "ConfigMap $policy_cm has no data['default'] entry"
            fi
            # Idempotent, and it REPLACES rather than appends: re-running with a
            # different WVA_LIMITER must not leave both declared, since a quota
            # entry would then win over the gpu-inventory one by mode precedence.
            updated_default=$(echo "$current_default" | yq ".limiters = [{\"type\": \"${WVA_LIMITER}\"}]")
            kubectl patch configmap "$policy_cm" -n "$WVA_NS" --type=merge \
                -p "$(jq -n --arg d "$updated_default" '{data:{"default":$d}}')"
            log_warning "Scaling is now bounded by the ${WVA_LIMITER} limiter (declared in ${WVA_NS}/${policy_cm})."
            # A GPU-aware optimizer allocates out of per-accelerator pools, so a
            # workload whose accelerator cannot be resolved gets no budget and stops
            # scaling up — silently. Say so at install, and say how to check, because
            # the symptom (a workload that simply never grows) looks like anything.
            if [ "$WVA_LIMITER" = "gpu-inventory" ]; then
                local gpu_products
                # Space-separated on one line, then split — a {"\n"} inside the
                # jsonpath is fragile to quote through the layers this runs under.
                gpu_products=$(kubectl get nodes \
                    -o jsonpath='{.items[*].metadata.labels.nvidia\.com/gpu\.product}' 2>/dev/null \
                    | tr ' ' '\n' | grep -v '^$' | sort -u | wc -l || true)
                if [ "${gpu_products:-0}" -gt 1 ]; then
                    log_warning "This cluster advertises ${gpu_products} different GPU products. Every managed workload MUST pin one with a nodeSelector/nodeAffinity GPU product key — an unpinned workload is charged to no accelerator pool, receives no budget, and will not scale up. Check with:"
                    log_warning "    kubectl get nodes -L nvidia.com/gpu.product"
                    log_warning "    kubectl get events -A --field-selector reason=AcceleratorNotResolved"
                else
                    log_info "Single GPU product detected: an unpinned workload is deduced onto it, so no nodeSelector is required."
                fi
            fi
            ;;
        *)
            log_error "WVA_LIMITER must be 'none', 'gpu-inventory' or 'quota', got '${WVA_LIMITER}'"
            ;;
    esac

    # Wait for WVA to be ready.
    #
    # `kubectl wait --for=condition=Ready pod -l ...` asks about ANY pod carrying
    # the label — and during a rolling update the PREVIOUS pod is still there and
    # still ready. An upgrade whose new pod cannot start therefore satisfied this
    # instantly. Ask the rollout instead: it is the only check that requires the
    # UPDATED replicas to be the ready ones.
    log_info "Waiting for WVA controller to be ready..."
    local wva_deploy
    wva_deploy=$(kubectl get deployment -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
        -o name 2>/dev/null | head -1)
    if [ -z "$wva_deploy" ]; then
        log_warning "No WVA Deployment found in $WVA_NS to wait for"
    elif kubectl rollout status "$wva_deploy" -n "$WVA_NS" --timeout=60s; then
        :
    else
        log_warning "WVA controller is not ready yet - check 'kubectl get pods -n $WVA_NS'"
    fi

    log_success "WVA deployment complete"
}

# Shared namespace creation loop for deploy/*/install.sh environment plugins.
# Platform adapter provides materialize_namespace(ns), then calls this helper.
# patch_manager_config applies one yq expression to data["config.yaml"] of the
# manager ConfigMap — the file the controller actually reads, mounted at
# /etc/wva/config.yaml.
#
# Patches that one key rather than replacing the object, so nothing echoes back
# server-managed metadata (resourceVersion, managedFields). yq rather than sed
# because the edit must be idempotent and survive quoting and whitespace variance:
# sed would silently fail to match an unquoted or single-quoted value and leave the
# setting untouched, which is the failure mode hardest to notice.
patch_manager_config() {
    local expr="$1" current updated
    current=$(kubectl get configmap wva-manager-config -n "$WVA_NS" \
        -o jsonpath='{.data.config\.yaml}')
    if [ -z "$current" ]; then
        log_error "ConfigMap wva-manager-config has no data['config.yaml'] key"
    fi
    updated=$(echo "$current" | yq "$expr")
    kubectl patch configmap wva-manager-config -n "$WVA_NS" --type=merge \
        -p "$(jq -n --arg cfg "$updated" '{data:{"config.yaml":$cfg}}')"
}

create_namespaces_shared_loop() {
    log_info "Creating namespaces..."

    # Only namespaces this run actually puts something in. Creating the others
    # leaves empty namespaces behind on a cluster that already had its own —
    # llm-d's workloads are somewhere else, and its Prometheus is not ours to
    # place — and an empty llm-d namespace is worse than absent: it looks like the
    # place to deploy models, and WVA is not watching it.
    local wanted="$WVA_NS"
    # Ask the same question the monitoring step will ask, rather than reading the
    # flag: DEPLOY_PROMETHEUS=true only MEANS "deploy one" when the cluster has no
    # Prometheus of its own. Reading the flag alone created an empty monitoring
    # namespace on every existing-llm-d install — the exact litter the comment
    # above is about — and made DEPLOY_PROMETHEUS=false something users had to
    # pass by hand to avoid it.
    # The monitoring namespace is cluster-scoped to create, so gate it on WHO is
    # running, not on what WVA will manage. Those are different questions: scope
    # says which workloads the controller watches, phase says whether this command
    # is the cluster-admin half or the tenant half.
    #
    # The tenant half (INSTALL_PHASE=wva) must not try. A namespace admin is
    # Forbidden from reading the Prometheus CRD, so foreign_prometheus() reports
    # "none found", and the install died creating a namespace it was never allowed
    # to create — after `--check` had said permissions were sufficient.
    #
    # But gating on scope alone was too strict the other way: `make setup-prereqs`
    # IS the cluster-admin phase, and in namespace scope it skipped the namespace
    # and then deployed Prometheus into it anyway. Reproduced on kind: exit 2 after
    # "Creating Kubernetes secret for Prometheus TLS", with no error printed.
    if [ "${DEPLOY_PROMETHEUS:-true}" = "true" ] \
        && [ "${INSTALL_PHASE:-all}" != "wva" ] \
        && [ -z "$(foreign_prometheus)" ]; then
        wanted="$wanted $MONITORING_NAMESPACE"
    fi
    [ "${DEPLOY_LLMD_NS:-false}" = "true" ] && wanted="$wanted $NAMESPACE"

    for ns in $wanted; do
        local ns_exists=false
        local ns_terminating=false

        if kubectl get namespace $ns &> /dev/null; then
            ns_exists=true
            local ns_status
            ns_status=$(kubectl get namespace $ns -o jsonpath='{.status.phase}' 2>/dev/null)
            if [ "$ns_status" = "Terminating" ]; then
                ns_terminating=true
            fi
        fi

        if [ "$ns_exists" = true ] && [ "$ns_terminating" = false ]; then
            log_info "Namespace $ns already exists"
            continue
        elif [ "$ns_terminating" = true ]; then
            log_info "Namespace $ns is terminating, forcing deletion..."
            kubectl get namespace $ns -o json | \
                jq '.spec.finalizers = []' | \
                kubectl replace --raw "/api/v1/namespaces/$ns/finalize" -f - 2>/dev/null || true
            kubectl wait --for=delete namespace/$ns --timeout=120s 2>/dev/null || true
        fi

        materialize_namespace "$ns"
        log_success "Namespace $ns created"
    done
}

delete_namespaces_kube_like() {
    log_info "Deleting namespaces..."

    # Only namespaces THIS install created, and only ones nothing else lives in.
    #
    # NAMESPACE needs its own opt-in rather than mirroring DEPLOY_LLMD_NS, because
    # that flag describes the INSTALL and an uninstall never restates it —
    # `make undeploy-wva` forwards only WVA_NS and WVA_SCOPE, so the flag would
    # read as its default (true) and delete anyway. It holds the model servers;
    # deleting it is never something to infer.
    #
    # The same protection has to cover WVA_NS, because in the documented tenant
    # install they are the SAME namespace: a namespace-scoped controller is
    # deliberately placed in the namespace holding the model servers, so
    # `WVA_NS = NAMESPACE`. Reached through the WVA_NS slot that namespace was
    # deleted outright — every Deployment, Secret and PVC in it — while the
    # NAMESPACE slot right beside it refused to. One uninstall flag, two paths to
    # the same namespace, and only one of them guarded.
    #
    # MONITORING_NAMESPACE is shared by every WVA on the cluster and carries a
    # fixed, unsuffixed name, so it takes UNDEPLOY_SHARED — the same gate the rest
    # of the uninstall uses for shared things. DEPLOY_PROMETHEUS says what a fresh
    # install WOULD deploy, never whether this one created what is there.
    local ns seen=""
    for ns in $NAMESPACE $WVA_NS $MONITORING_NAMESPACE; do
        [ -n "$ns" ] || continue
        case " $seen " in *" $ns "*) continue ;; esac
        seen="$seen $ns"
        kubectl get namespace "$ns" &> /dev/null || continue

        local skip=""
        if [ "$ns" = "$WVA_NS" ] && [ "$DEPLOY_WVA" = "false" ]; then
            skip="this install did not create it"
        elif [ "$ns" = "$NAMESPACE" ] || [ "$ns" = "${WVA_WATCH_NS:-}" ]; then
            [ "${DELETE_LLMD_NS:-false}" = "true" ] ||                 skip="it holds the model servers (DELETE_LLMD_NS=true to remove it anyway)"
        elif [ "$ns" = "$MONITORING_NAMESPACE" ]; then
            if [ "$DEPLOY_PROMETHEUS" = "false" ]; then
                skip="this install did not create it"
            elif [ "${UNDEPLOY_SHARED:-false}" != "true" ]; then
                skip="it is shared with every other WVA on this cluster (UNDEPLOY_SHARED=true to remove it anyway)"
            fi
        fi

        if [ -n "$skip" ]; then
            log_info "Keeping namespace $ns — $skip"
            continue
        fi
        log_info "Deleting namespace $ns..."
        kubectl delete namespace "$ns" 2>/dev/null ||             log_warning "Failed to delete namespace $ns"
    done

    log_success "Namespaces deleted"
}

# Shared WVA prerequisites for Kubernetes-like environments.
# Optional:
deploy_wva_prerequisites_kube_like() {
    log_info "Deploying Workload-Variant-Autoscaler prerequisites for Kubernetes..."

    # InferencePool CRDs must exist before the controller starts or its initial
    # watch fails. install-epp.sh installs them again later (idempotent).
    install_inference_crds

    # Extract the Prometheus CA, if this cluster's Prometheus is one we recognise.
    #
    # Guarded, because this script runs under `set -e -o pipefail`: on a cluster
    # whose Prometheus predates the install — different namespace, different secret
    # name, or plain HTTP — the unguarded pipe aborted the whole installation over
    # a certificate that is optional to begin with.
    if kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$MONITORING_NAMESPACE" &> /dev/null; then
        log_info "Extracting Prometheus TLS certificate"
        kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$MONITORING_NAMESPACE" \
            -o jsonpath='{.data.tls\.crt}' | base64 -d > "$PROM_CA_CERT_PATH"
    else
        log_warning "No secret $PROMETHEUS_SECRET_NAME in $MONITORING_NAMESPACE — skipping CA extraction. Expected when using a Prometheus this install did not deploy. Point PROMETHEUS_SECRET_NAME/PROMETHEUS_SECRET_NS at yours, or set PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true to connect without verifying it."
    fi

    # LeaderWorkerSet (WVA dependency; see upstream chart / #910).
    if [ "${DEPLOY_LWS:-true}" = "true" ]; then
        if kubectl get crd leaderworkersets.leaderworkerset.x-k8s.io &> /dev/null; then
            log_info "LeaderWorkerSet CRD already installed, skipping LWS deployment"
        else
            log_info "Installing LeaderWorkerSet version ${LWS_CHART_VERSION} into ${LWS_NAMESPACE} namespace"
            helm upgrade -i lws oci://registry.k8s.io/lws/charts/lws \
                --version="${LWS_CHART_VERSION}" \
                --namespace "${LWS_NAMESPACE}" \
                --create-namespace \
                --wait --timeout 300s
        fi
    else
        log_info "Skipping LeaderWorkerSet installation (DEPLOY_LWS=false)"
    fi

    log_success "WVA prerequisites complete"
}

# OpenShift-specific CA extraction used by deploy/openshift/install.sh.
extract_openshift_prometheus_ca() {
    # Extract OpenShift Service CA certificate for Thanos verification
    # Note: For OpenShift service certificates, we need the Service CA that signed the server cert,
    # not the server certificate itself. The server cert is in thanos-querier-tls, but we need the CA.
    log_info "Extracting OpenShift Service CA certificate for Thanos verification"

    # Method 1: Extract Service CA from openshift-service-ca.crt ConfigMap (preferred)
    # This is the actual CA certificate that signs OpenShift service certificates
    if kubectl get configmap openshift-service-ca.crt -n "$PROMETHEUS_SECRET_NS" &> /dev/null; then
        log_info "Extracting Service CA from openshift-service-ca.crt ConfigMap"
        kubectl get configmap openshift-service-ca.crt -n "$PROMETHEUS_SECRET_NS" -o jsonpath='{.data.service-ca\.crt}' > "$PROM_CA_CERT_PATH" 2>/dev/null || true
        if [ -s "$PROM_CA_CERT_PATH" ]; then
            log_success "Extracted Service CA from openshift-service-ca.crt ConfigMap"
        fi
    fi

    # Method 2: Extract Service CA from openshift-config namespace
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_info "Trying to extract Service CA from openshift-config namespace"
        kubectl get configmap openshift-service-ca -n openshift-config -o jsonpath='{.data.service-ca\.crt}' > "$PROM_CA_CERT_PATH" 2>/dev/null || true
        if [ -s "$PROM_CA_CERT_PATH" ]; then
            log_success "Extracted Service CA from openshift-config namespace"
        fi
    fi

    # Method 3: Fallback to thanos-querier-tls secret
    # Note: This extracts the server certificate, which may work if the cert chain includes the CA
    # but it's not ideal - we should use the Service CA instead.
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_warning "Service CA not found, falling back to server certificate from thanos-querier-tls"
        log_warning "This may cause TLS verification issues - Service CA is preferred"
        if kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$PROMETHEUS_SECRET_NS" &> /dev/null; then
            log_info "Extracting certificate from thanos-querier-tls secret"
            kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$PROMETHEUS_SECRET_NS" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$PROM_CA_CERT_PATH"
            if [ -s "$PROM_CA_CERT_PATH" ]; then
                log_success "Extracted certificate from thanos-querier-tls secret"
            fi
        fi
    fi

    # Verify we have a valid certificate
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_error "Failed to extract OpenShift Service CA certificate"
        log_error "Tried: openshift-service-ca.crt ConfigMap, openshift-config ConfigMap, and thanos-querier-tls secret"
        exit 1
    fi

    # Verify the certificate is valid PEM format
    if ! openssl x509 -in "$PROM_CA_CERT_PATH" -text -noout &> /dev/null; then
        log_warning "Certificate file may not be in valid PEM format, but continuing..."
        log_warning "If TLS errors occur, verify the certificate format is correct"
    else
        # Log certificate details for debugging
        local cert_subject
        cert_subject=$(openssl x509 -in "$PROM_CA_CERT_PATH" -noout -subject 2>/dev/null | sed 's/subject=//' || echo "unknown")
        log_info "Certificate subject: $cert_subject"
    fi
}

