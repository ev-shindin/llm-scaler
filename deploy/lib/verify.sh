#!/usr/bin/env bash
#
# Shared verification and deployment summary helpers for deploy/install.sh.
# Covers WVA / Prometheus / scaler backend only — llm-d is installed separately via llm-d project guides.
# Requires funcs: log_info/log_warning/log_success, containsElement().
# Uses constants: DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS and shared selectors.
#

# verify_deployment returns non-zero when the thing this script exists to install
# is not running. The caller MUST honour that: it used to compute `all_good` and
# then discard it, so an install whose controller never started printed
# "Deployment complete!" and exited 0 — which is what CI and every wrapper script
# reads. A silent false success is worse than a loud failure.
#
# Only WVA itself is fatal. Prometheus, Grafana and KEDA may be pre-existing,
# externally managed, or genuinely still pulling images; they stay warnings.
verify_deployment() {
    log_info "Verifying deployment..."

    local all_good=true

    # --- WVA
    log_info "Waiting for the WVA controller to become available..."
    # `kubectl get pods | grep Running` was not a readiness check: a pod stuck at
    # "0/1 Running" — crash-looping on a bad config, or unable to reach Prometheus —
    # matches it.
    #
    # Neither is `--for=condition=Available` on the Deployment, which is what this
    # used to ask. A rolling update keeps the PREVIOUS ReplicaSet's pod running
    # until the new one is ready, and one ready pod is enough to hold Available
    # true — so an upgrade to an image that cannot start reported success while
    # the new pod sat in CrashLoopBackOff. Verified on a real cluster: installing
    # this branch's manifests with the default (released) IMG crash-loops on
    # "unknown flag: --external-scaler-bind-address", and the install still said
    # "WVA controller is running and ready".
    #
    # `rollout status` is the check that means what this claims: it waits for the
    # UPDATED replicas to be available, and fails when they never are.
    local wva_deploy
    wva_deploy=$(kubectl get deployment -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
        -o name 2>/dev/null | head -1)

    # Nothing in $WVA_NS -- before reporting a rollout failure against a
    # namespace that may simply be the wrong one, check whether a WVA controller
    # is running somewhere else entirely. Only reachable when $WVA_NS was never
    # confirmed by the caller (an install always deploys into $WVA_NS first, so
    # its own verify_deployment call finds this branch already covered); this is
    # for a standalone `make verify-deployment` run against an unset or guessed
    # WVA_NS.
    if [ -z "$wva_deploy" ]; then
        if kubectl auth can-i list deployments -A >/dev/null 2>&1; then
            local elsewhere elsewhere_count
            elsewhere=$(kubectl get deployment -A -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
                -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null | sort -u)
            elsewhere_count=$(printf '%s\n' "$elsewhere" | grep -c . || true)
            if [ "${elsewhere_count:-0}" -eq 1 ]; then
                log_warning "No WVA controller in $WVA_NS -- but one is running in $elsewhere. Re-run with WVA_NS=$elsewhere (or NAMESPACE=$elsewhere for a namespace-scoped install, since that is what resolves WVA_NS by default)."
                return 1
            elif [ "${elsewhere_count:-0}" -gt 1 ]; then
                log_warning "No WVA controller in $WVA_NS, and more than one WVA runs on this cluster: $(printf '%s' "$elsewhere" | paste -sd, -). Pick one with WVA_NS=<ns>."
                return 1
            fi
            # Else: genuinely none anywhere. Fall through -- the diagnostics below
            # report that correctly, and there is nothing else to redirect to.
        else
            log_warning "Cannot confirm whether a WVA controller runs elsewhere -- listing Deployments cluster-wide is not permitted for this identity. If you know its namespace, pass WVA_NS=<ns> (or NAMESPACE=<ns> for a namespace-scoped install)."
        fi
    fi

    if [ -n "$wva_deploy" ] && kubectl rollout status "$wva_deploy" -n "$WVA_NS" \
        --timeout="${WVA_VERIFY_TIMEOUT:-180s}" >/dev/null 2>&1; then
        log_success "WVA controller is running and ready in $WVA_NS"
    else
        all_good=false
        log_warning "WVA controller did NOT become ready within ${WVA_VERIFY_TIMEOUT:-180s}."
        kubectl get pods -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" >&2 2>/dev/null || true

        # Say WHY, here, rather than printing two commands to run. The container's
        # own last words are almost always the answer and they are one API call
        # away — a released image rejecting a flag this branch's manifests pass
        # ("unknown flag: --external-scaler-bind-address") looks identical to a
        # network problem until you read them.
        local reason
        reason=$(kubectl get pods -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
            -o jsonpath='{range .items[*].status.containerStatuses[*]}{.state.waiting.reason}{" "}{.state.waiting.message}{.lastState.terminated.reason}{"\n"}{end}' 2>/dev/null \
            | grep -v '^[[:space:]]*$' | head -3)
        [ -n "$reason" ] && log_warning "  Container state: $(printf '%s' "$reason" | tr '\n' ';')"

        local logs
        logs=$(kubectl logs -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
            --tail=8 --all-containers 2>/dev/null \
            || kubectl logs -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
                --tail=8 --previous 2>/dev/null)
        if [ -n "$logs" ]; then
            log_warning "  Last lines from the container:"
            printf '%s\n' "$logs" | sed 's/^/      /' >&2
        fi

        log_warning "  Full logs: kubectl -n $WVA_NS logs -l $WVA_CONTROLLER_LABEL_SELECTOR --tail=50 --previous"
        log_warning "  Events:    kubectl -n $WVA_NS get events --sort-by=.lastTimestamp | tail -20"
        log_warning "  If it is rejecting a flag, IMG and these manifests are different versions: build and push this tree (make docker-build docker-push IMG=<ref>) and re-run with that IMG."
    fi

    # --- Did the model-server metrics actually arrive?
    #
    # The sanity check that closes the loop: a PodMonitor existing says nothing
    # about whether it produced a target. One naming a port no container declares
    # generates NO target at all rather than a failing one, so "applied" and
    # "working" are different claims and only the second one matters. This is the
    # half that carries the value — the installer does not create the object (see
    # infra_monitoring.sh), so reporting the outcome is all there is.
    #
    # Asked of WVA rather than of Prometheus, deliberately. The Prometheus URL is
    # in-cluster (on OpenShift, thanos-querier.openshift-monitoring.svc) and is not
    # reachable from wherever this script runs, so querying it would need a route,
    # a token, or a one-shot pod. WVA has already queried it, every optimization
    # interval, and says what it found: wva_saturation_metrics_up, and this log
    # line. One kubectl call, no cluster networking, and it reports the thing the
    # operator actually cares about.
    local wva_logs40 sat_missing
    wva_logs40=$(kubectl logs -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" --tail=40 2>/dev/null)
    sat_missing=$(grep -c "No saturation metrics available" <<< "$wva_logs40" || true)
    if [ "${sat_missing:-0}" -gt 0 ]; then
        log_warning "WVA is running but has NO model-server metrics to size from — it logged \"No saturation metrics available for model\" ${sat_missing}x in its last 40 lines."
        log_warning "  It will hold every workload at minReplicas. On the HPA that looks identical to a correct decision ('1/1 (avg)'), which is why this is checked here."
        log_warning "  Usually nothing is scraping the model servers. Check:  kubectl get podmonitor,servicemonitor -n ${WVA_WATCH_NS:-$WVA_NS}"
        log_warning "  That is llm-d's step 3, \"(Optional) Enable monitoring\" — optional for llm-d, required for WVA:"
        log_warning "      kubectl apply -n ${WVA_WATCH_NS:-$WVA_NS} -k \$REPO_ROOT/guides/recipes/modelserver/components/monitoring"
        log_warning "  or, with no llm-d checkout:  kubectl apply -n ${WVA_WATCH_NS:-$WVA_NS} -k config/modelserver-metrics"
        log_warning "  A model server with 0 replicas also produces no series; that case resolves itself when one starts."
    elif grep -q "scaling-decision" <<< "$wva_logs40"; then
        log_success "WVA is reading model-server metrics and emitting scaling decisions"
    fi

    # --- WVA's own metrics: was the ServiceMonitor ACCEPTED, not merely created?
    #
    # The prometheus-operator resolves a ServiceMonitor's bearerTokenSecret the
    # first time it sees the object. If that Secret is absent it REJECTS the
    # ServiceMonitor -- `reason=InvalidConfiguration`, "unable to get secret" --
    # and never reconsiders: a metadata write does not re-trigger it, and
    # re-applying identical content is a no-op, so `kubectl apply` says
    # "unchanged" and the operator does not look again. The rejection lasts as
    # long as the object does.
    #
    # A two-phase install used to create the ServiceMonitor in the admin phase and
    # its Secret in the controller phase, half an hour apart, and so produced
    # exactly that state: no `up` series, no `wva_*` series, and an install that
    # printed "All components verified successfully!" over the top of it. Both
    # kinds are in WVA_PREREQ_KINDS now (see common.sh) so they land together, but
    # an install whose admin phase predates that change still carries the rejected
    # object -- and only the operator's log or this check will ever say so.
    #
    # Checked by cause rather than by event: events expire, the missing Secret does
    # not. Not fatal -- WVA scales from llm-d's metrics, a different path entirely.
    # What is lost is WVA's own telemetry, which fails by rendering empty.
    # `|| true` because the ServiceMonitor CRD is absent on any cluster without
    # the prometheus-operator -- the common case on plain Kubernetes -- and kubectl
    # exits non-zero there. An assignment carries that status and `set -e` turns it
    # into an exit. Today the only caller is `verify_deployment || verified=false`,
    # which suppresses `set -e` for the whole body; this no longer depends on that.
    local sm_json sm_refs
    sm_json=$(kubectl get servicemonitor -n "$WVA_NS" -o json 2>/dev/null) || true
    if [ -n "$sm_json" ]; then
        local sm_missing
        sm_refs=$(printf '%s' "$sm_json" | jq -r '
            .items[]?
            | .metadata.name as $sm
            | .spec.endpoints[]?
            | select(.bearerTokenSecret.name != null)
            | "\($sm) \(.bearerTokenSecret.name)"' 2>/dev/null) || true
        sm_missing=$(printf '%s\n' "$sm_refs" \
          | while read -r sm secret; do
                [ -n "$secret" ] || continue
                kubectl get secret "$secret" -n "$WVA_NS" >/dev/null 2>&1 \
                    || echo "$sm -> $secret"
            done)
        # Two different failures, and the second is the one a naive check misses.
        #
        #   the Secret is absent      the cause. Deterministic, and the state a
        #                             fresh mis-ordered install lands in.
        #   the Secret is present     but the operator already rejected the
        #     and it was rejected     ServiceMonitor when it was not, and does not
        #                             re-evaluate. This is the RECOVERY state: an
        #                             install that creates the missing Secret is
        #                             still not scraped, and checking only for the
        #                             Secret would call that healthy.
        #
        # The rejection is only visible as an event, and events expire (an hour by
        # default), so this catches it within an install's own window and cannot
        # prove its absence later. That asymmetry is the right way round: a false
        # "still rejected" costs one spec touch, a false "healthy" costs all of
        # WVA's telemetry, silently.
        # `|| true`: no matching events is grep exit 1, which pipefail makes the
        # assignment's status and `set -e` makes an exit -- and NO rejection events
        # is the healthy case, so without this the check kills the rest of
        # verify_deployment on exactly the clusters where nothing is wrong.
        local sm_rejected
        # The MESSAGE, not just the name. A missing Secret is only one thing a
        # ServiceMonitor can fail to resolve, and assuming it was misdiagnoses every
        # other cause while prescribing a fix that cannot work. Seen on pokprod001:
        # the Secret existed and the rejection was a missing CA ConfigMap, for which
        # delete-and-re-apply runs straight back into the same missing object.
        sm_rejected=$(kubectl get events -n "$WVA_NS" \
            --field-selector reason=InvalidConfiguration \
            -o jsonpath='{range .items[*]}{.involvedObject.kind}/{.involvedObject.name}{"  "}{.message}{"\n"}{end}' 2>/dev/null \
            | grep '^ServiceMonitor/' | cut -c1-210 | sort -u) || true

        if [ -n "$sm_missing" ]; then
            log_warning "A ServiceMonitor names a bearerTokenSecret that does not exist, so the prometheus-operator has rejected it and WVA's own metrics are NOT being collected:"
            printf '%s\n' "$sm_missing" | sed 's/^/      /' >&2
            log_warning "  Scaling is unaffected (that reads llm-d's metrics), but no wva_* series will appear and the dashboard will be empty."
            log_warning "  Fix: create the Secret, then force ONE re-evaluation — the operator ignores metadata-only writes, so change the spec, or delete and re-apply the ServiceMonitor."
        elif [ -n "$sm_rejected" ]; then
            log_warning "A ServiceMonitor was rejected by the prometheus-operator (InvalidConfiguration), and it does not re-evaluate on its own. The reason it gave:"
            printf '%s\n' "$sm_rejected" | sed 's/^/      /' >&2
            log_warning "  Until it is re-evaluated there is no scrape target and no wva_* series — on the dashboard the WVA panels stay empty while the vllm:* ones fill. Fix what the message names FIRST, then force one re-evaluation:"
            log_warning "    kubectl -n $WVA_NS delete servicemonitor <name> && NAMESPACE=$WVA_NS make setup-prereqs"
            log_warning "  Then confirm a target exists — a re-apply of unchanged content will NOT do it."
        elif [ -n "$sm_refs" ]; then
            log_success "WVA metrics ServiceMonitor references resolve (its scrape config is valid)"
        else
            # Nothing here authenticates with a Secret, so there is no reference for
            # the operator to resolve and nothing for this check to confirm. Claiming
            # "valid" would be the same false green the check exists to remove: on
            # plain Kubernetes the ServiceMonitor uses bearerTokenFile -- a projected
            # token the kubelet supplies, which the operator never resolves -- and a
            # namespace with no ServiceMonitor at all reaches this branch too.
            log_info "No ServiceMonitor in $WVA_NS authenticates with a Secret, so there is nothing for the prometheus-operator to resolve or reject (only the OpenShift overlay uses bearerTokenSecret; Kubernetes uses bearerTokenFile)."
        fi
    fi

    # --- The EPP's scheduler queue, WVA's other input.
    #
    # Reported here as well as in the preflight, because a caller who runs
    # `make deploy-wva` without `make check-prereqs` would otherwise never be told,
    # and the gate being off is invisible in every other output: WVA still emits
    # decisions, the HPA still reads healthy, and wva_unmeasured_queue — which
    # exists to catch exactly this class of blindness — is itself sourced from the
    # queue that is missing, so it reads 0.
    wva_report_epp_flowcontrol || true

    # --- Monitoring
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        log_info "Checking Prometheus..."
        if wva_any_pod_ready "$MONITORING_NAMESPACE" app.kubernetes.io/name=prometheus; then
            log_success "Prometheus is running"
        else
            log_warning "Prometheus is not ready yet"
        fi
    fi

    if [ "$DEPLOY_OPERATIONAL_DASHBOARD" = "true" ]; then
        log_info "Checking Grafana..."
        # Namespace-scoped on purpose, unlike KEDA: Grafana is not a cluster
        # singleton, so finding someone else's on a shared cluster would prove
        # nothing about this install. That does mean it reports only on the
        # namespace it was told about, so it says which one.
        if wva_any_pod_ready "$MONITORING_NAMESPACE" app.kubernetes.io/name=grafana; then
            log_success "Grafana is running"
        elif wva_pods_exist "$MONITORING_NAMESPACE" app.kubernetes.io/name=grafana; then
            log_warning "Grafana is not ready yet"
        else
            log_info "No Grafana in $MONITORING_NAMESPACE. On OpenShift the dashboards are elsewhere (often the grafana namespace), and this install did not deploy one."
        fi
    fi

    # --- Scaler backend
    if [ "$SCALER_BACKEND" = "keda" ]; then
        log_info "Checking KEDA..."
        # Cluster-wide, not $KEDA_NAMESPACE. KEDA is a cluster singleton — one
        # operator owns the external-metrics APIService — and where it lives is
        # not ours to assume: on OpenShift it is platform-managed and sits in
        # openshift-keda, while KEDA_NAMESPACE says keda-system. Looking only
        # there reported "KEDA is not ready" on a cluster whose KEDA had been up
        # for ten days with six ScaledObjects READY.
        if wva_any_pod_ready -A "$KEDA_OPERATOR_LABEL_SELECTOR"; then
            log_success "KEDA is running"
        elif wva_pods_exist -A "$KEDA_OPERATOR_LABEL_SELECTOR"; then
            # Found and unhealthy. This one has earned the strong wording.
            log_warning "KEDA is not ready yet — until it is, ScaledObjects stay unready and nothing scales."
        else
            # Not found is not the same as broken, and saying "nothing scales"
            # here would be a false alarm on any cluster whose KEDA does not
            # carry this label -- which is exactly the platform-managed case.
            #
            # A stronger signal sits right in the managed namespace, once one
            # exists: a ScaledObject KEDA has actually reconciled to READY. Only
            # useful in namespace scope (cluster scope would need enumerating
            # every managed namespace, not just one), and only once a workload
            # has been registered -- so this is a no-op the first time
            # verify_deployment runs, as part of an install, before step 3
            # ("Register the workloads") has created anything. It becomes a real
            # confirmation on a later, standalone `make verify-deployment`.
            local keda_ns=""
            [ "$(wva_install_scope)" = "namespace" ] && keda_ns="${WVA_WATCH_NS:-$WVA_NS}"
            if [ -n "$keda_ns" ] && kubectl get scaledobject -n "$keda_ns" \
                    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null \
                    | grep -qx 'True'; then
                log_success "KEDA is running — confirmed by a READY ScaledObject in $keda_ns (its operator pod just carries no label this check recognizes; a platform-managed KEDA often does not)."
            elif [ -n "$keda_ns" ]; then
                log_info "No KEDA operator matched $KEDA_OPERATOR_LABEL_SELECTOR anywhere on the cluster, and no READY ScaledObject in $keda_ns confirms it that way either. A platform-managed KEDA may not carry that label — confirm with: kubectl get scaledobject -n $keda_ns"
            else
                log_info "No KEDA operator matched $KEDA_OPERATOR_LABEL_SELECTOR anywhere on the cluster. A platform-managed KEDA may not carry that label — confirm with: kubectl get scaledobject -A"
            fi
        fi
    elif [ "$SCALER_BACKEND" = "none" ]; then
        log_info "Scaler backend skipped (SCALER_BACKEND=none) — assuming external metrics API is pre-installed"
    fi

    if [ "$all_good" = true ]; then
        log_success "All components verified successfully!"
        return 0
    fi
    return 1
}

# print_summary is the most-read text this install produces: it is what someone
# looks at to find out whether it worked and what to do next. So it says the one
# thing that is true and easy to miss — an installed WVA scales nothing until a
# ScaledObject exists — and it reports the settings that will otherwise surprise
# someone later (no GPU budget, scale-to-zero, a named instance).
print_summary() {
    echo ""
    echo "=========================================="
    echo " Workload-Variant-Autoscaler installed"
    echo "=========================================="
    echo ""
    echo "  Controller:   $WVA_NS  ($WVA_IMAGE_REPO:$WVA_IMAGE_TAG)"
    echo "  Scope:        $(wva_install_scope)-scoped"
    if [ "$(wva_install_scope)" = "cluster" ]; then
        echo "  Manages:      every namespace"
    else
        echo "  Manages:      $WVA_NS only (its cache is restricted to it)"
    fi
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        echo "  Prometheus:   deployed in $MONITORING_NAMESPACE"
    else
        echo "  Prometheus:   ${PROMETHEUS_URL:-<the shipped default>} (not deployed by this install)"
    fi
    if [ "$SCALER_BACKEND" = "keda" ]; then
        echo "  KEDA:         installed or already present"
    else
        echo "  KEDA:         skipped (SCALER_BACKEND=$SCALER_BACKEND) — WVA needs KEDA to actuate"
    fi
    # Report where the dashboard ACTUALLY is, not what the flag asked for. This
    # line used to assert "deployed in $MONITORING_NAMESPACE" even when the publish
    # had been skipped by a Forbidden -- confident and wrong.
    if [ "$DEPLOY_OPERATIONAL_DASHBOARD" = "true" ]; then
        local dash_ns=""
        for candidate in "${DASHBOARD_NS:-}" "$WVA_NS" "$MONITORING_NAMESPACE"; do
            [ -n "$candidate" ] || continue
            if kubectl get configmap wva-operation-dashboard -n "$candidate" >/dev/null 2>&1; then
                dash_ns="$candidate"
                break
            fi
        done
        if [ -n "$dash_ns" ]; then
            echo "  Dashboard:    published in $dash_ns"
        else
            echo "  Dashboard:    not published — import deploy/grafana/operational-dashboard.json into Grafana"
        fi
    fi
    # CONTROLLER_INSTANCE is deliberately NOT reported here. No install path puts
    # it on the Deployment, so this line read the installer's shell environment and
    # then asserted a scoping rule the running controller was not applying — the
    # most misleading kind of summary, because it is specific and confident.
    echo ""

    # The step everyone misses. WVA has no watch and no listing: it is asked about
    # a workload by KEDA, or it never hears of it.
    echo "  NEXT: nothing scales yet."
    echo ""
    echo "  A ScaledObject is how a workload registers with WVA. Until one exists,"
    echo "  the controller is running and idle. To see what it would create:"
    echo ""
    echo "      make scaledobjects-plan"
    echo ""
    echo "  then apply it, or an edited copy of it:"
    echo ""
    echo "      make scaledobjects-apply"
    echo ""

    echo "  Worth knowing:"
    case "$(effective_limiter_summary)" in
        none) echo "    - No GPU limiter: scaling is UNBOUNDED. Set WVA_LIMITER=gpu-inventory or quota" ;;
        *)    echo "    - GPU limiter: $(effective_limiter_summary). A workload whose accelerator does not resolve gets no budget and will not scale up" ;;
    esac
    if [ "${ENABLE_SCALE_TO_ZERO:-false}" = "true" ]; then
        echo "    - Scale-to-zero is ON: an idle model can be parked at 0 and woken on demand"
    else
        echo "    - Scale-to-zero is OFF (ENABLE_SCALE_TO_ZERO=false)"
    fi
    echo ""

    echo "  Checking on it:"
    echo "    kubectl -n $WVA_NS logs -l app.kubernetes.io/name=workload-variant-autoscaler -f"
    echo "    kubectl get scaledobject -A          # what WVA has been told about"
    echo "    kubectl get hpa -A                   # KEDA acts through these"
    echo ""
    echo "  Guide: deploy/README.md"
    echo "=========================================="
}

# effective_limiter_summary echoes the limiter this install declared.
effective_limiter_summary() {
    echo "${WVA_LIMITER:-none}"
}
