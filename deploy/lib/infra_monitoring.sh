#!/usr/bin/env bash
#
# Shared monitoring orchestration helpers.
# Keeps install.sh main flow concise while delegating to environment/plugin functions.
# Requires funcs: deploy_prometheus_stack(), log_info/log_warning/log_success,
# wait_deployment_available_nonfatal().
# Requires vars: DEPLOY_PROMETHEUS.
#

# fma_launcher_scrape_conflict echoes the names of PodMonitors in $1 that ALREADY
# generate targets for FMA launcher pods, or nothing.
#
# It exists to stop the fma-launcher-metrics component from being applied on top
# of one. Two scrape configs on one pod produce two targets at the same
# (instance, pod) key, and the collector's additive queries -- the `sum by` ones
# behind dispatch rate and generation-token rate -- would DOUBLE-COUNT, while the
# `max by` ones would not. Capacity then looks right while throughput reads 2x,
# which is a hard failure to spot and a worse outcome than not scraping at all.
#
# The distinction between selecting a launcher and being able to scrape one is
# what makes this correct rather than merely plausible; wva_launcher_scrapers
# carries that reasoning, and discovery asks it the same question from the other
# direction. With no launcher pods present it reports nothing, which is right: the
# PodMonitor is inert until FMA appears and should be in place beforehand.
fma_launcher_scrape_conflict() {
    wva_launcher_scrapers "$1" fma-launcher-metrics
}

# deploy_fma_launcher_podmonitor applies the launcher PodMonitor into $1, the
# namespace whose launcher pods should be scraped.
#
# It takes a namespace rather than riding the WVA overlay because the two are not
# the same place. The overlay renders into WVA_NS, where the CONTROLLER runs;
# launcher pods live in the workload namespace, which is WVA_NS only for a plain
# namespace-scoped install, is WVA_WATCH_NS whenever that is set, and is any
# number of namespaces for a cluster-scoped one. A PodMonitor in the wrong
# namespace selects nothing and reports no error, which is the worst way for
# monitoring to fail.
#
# Returns 0 when it applied, or when it deliberately did not. A monitoring add-on
# must not fail an install.
deploy_fma_launcher_podmonitor() {
    local ns="$1" conflict launchers

    if ! kubectl get crd podmonitors.monitoring.coreos.com >/dev/null 2>&1; then
        log_warning "WVA_FMA_LAUNCHER_METRICS is set but the PodMonitor CRD is absent — install the Prometheus Operator first. Skipping."
        return 0
    fi

    conflict=$(fma_launcher_scrape_conflict "$ns" | paste -sd, -)
    if [ -n "$conflict" ]; then
        log_warning "Not applying the FMA launcher PodMonitor: $conflict already scrapes launcher pods in $ns. Two scrape configs on one pod give it two targets under the same (instance, pod) key, and WVA's additive queries would double-count throughput while capacity still looked correct. Remove the other PodMonitor first, or keep using it and leave WVA_FMA_LAUNCHER_METRICS unset."
        return 0
    fi

    if ! kubectl apply -k "$WVA_PROJECT/config/fma-launcher-metrics" -n "$ns" >/dev/null 2>&1; then
        log_warning "Failed to apply the FMA launcher PodMonitor in $ns; continuing without it."
        return 0
    fi

    launchers=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
        --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "${launchers:-0}" -gt 0 ]; then
        log_success "FMA launcher PodMonitor applied in $ns ($launchers launcher pod(s) present). Only launchers with a BOUND instance become targets — an unbound one carries no server-port annotation and is skipped, so warm spares never appear as idle replicas."
    else
        log_info "FMA launcher PodMonitor applied in $ns. No launcher pods there yet; it will start scraping if FMA is installed later."
    fi

    log_info "Cluster-scoped installs: this applied to $ns only. Repeat for every namespace that runs FMA launchers — kubectl apply -k config/fma-launcher-metrics -n <ns>"
}

# foreign_prometheus echoes any Prometheus on this cluster that this install did
# NOT put there, or nothing.
#
# The Prometheus Operator's CRDs are cluster-scoped and singular: a second
# kube-prometheus-stack install brings its own operator, which then watches the
# SAME Prometheus/ServiceMonitor resources as the first. Two operators reconciling
# one set of objects fight over them, and the visible symptom is somebody else's
# monitoring going intermittently blind — a bad way to find out you installed WVA.
#
# "Foreign" means outside MONITORING_NAMESPACE. A Prometheus we deployed earlier
# lives there, and re-running the install over it must still upgrade it: this
# script is meant to be idempotent, and the e2e re-deploys onto a live cluster
# every run. Detecting merely "a Prometheus exists" would have turned that upgrade
# into a silent skip.
# --- Model-server metrics -----------------------------------------------------
#
# WVA's capacity model is built entirely on vllm:* series — num_requests_waiting,
# kv_cache_usage_perc, time_per_output_token_seconds, request_prompt_tokens, and
# the rest (see internal/collector). Something has to scrape the model servers for
# any of it to exist.
#
# Providing that is arguably llm-d's job, and a full llm-d standup does provide it:
# a working optimized-baseline namespace carries a `decode` PodMonitor selecting
# llm-d.ai/role=decode on port `modelserver`. But a namespace built from the guide
# need not have one, and the failure is silent in the worst way. Measured on
# pokprod001: dhl-la-1708 ran four replicas for hours and produced ZERO vllm
# series, while `check-prereqs` reported "[SUCCESS] Prometheus: <url>" — true, and
# about the endpoint, not its contents — and the controller's own startup
# validation passed for the same reason. WVA then logged "No saturation metrics
# available for model, skipping analysis" every cycle and held at minReplicas,
# which on the HPA is indistinguishable from a correct decision: `1/1 (avg)`.
#
# So: verify it, and confirm afterwards that series actually arrived. The fix
# itself is applied by hand, never by this script — see wva_report_modelserver_metrics
# below for why creating the PodMonitor automatically was tried and reverted.

# wva_modelserver_scrape_conflict echoes any monitor in $1, other than WVA's own,
# that already scrapes the model servers.
#
# Same reasoning as fma_launcher_scrape_conflict: two scrape configs on one pod
# give it two targets, and WVA's additive queries would double-count throughput
# while per-replica capacity still looked right. Better to leave an existing,
# working scrape alone -- which makes a MISS here actively harmful rather than
# merely quiet: the report tells the reader to apply config/modelserver-metrics,
# and doing that on top of a scrape that already exists IS the double-count.
#
# Markers come from so_serving_markers, never a private list. Matching the
# serving ROLE alone missed the shape the llm-d modelservice path actually
# renders -- a PodMonitor selecting llm-d.ai/inferenceServing=true plus
# llm-d.ai/model, seen on pokprod as vllm-<model>-<hash> -- and llm-d attaches
# llm-d.ai/-prefixed labels to non-serving components too (gateway, EPP,
# requester), so the marker list is the only thing that separates the two. One
# definition for discovery, the preflight count and this check, or they disagree.
wva_modelserver_scrape_conflict() {
    local ns="$1" kind markers services
    markers="$(so_serving_markers_json)"

    # A ServiceMonitor selects SERVICES, not pods, so a pod label in its selector
    # proves nothing and a pod-label test against one is answering the wrong
    # question. Resolve the indirection instead: find the Services this monitor
    # selects, then ask whether THOSE target a model server. Fetched once.
    services="$(kubectl get services -n "$ns" -o json 2>/dev/null \
                | jq -c '[.items[]? | {labels: (.metadata.labels // {}),
                                       selector: (.spec.selector // {})}]' 2>/dev/null || true)"
    [ -n "$services" ] || services='[]'

    for kind in podmonitors servicemonitors; do
        # `|| true`: this whole function runs inside a command substitution whose
        # result is assigned, and under `set -o pipefail` a Forbidden or
        # CRD-absent kubectl makes the loop's status the function's status, which
        # then aborts the caller's assignment under `set -e`. Measured: with
        # ServiceMonitors Forbidden -- an ordinary namespace-scoped install --
        # check-prereqs died silently just before "Preflight passed".
        kubectl get "$kind" -n "$ns" -o json 2>/dev/null | jq -r \
            --arg kind "$kind" --argjson markers "$markers" --argjson services "$services" '
            # A label map is a model server when any of its k=v pairs is a marker.
            def serving_map($m): ($m | to_entries
                | any(.key + "=" + (.value|tostring) as $kv
                      | ($markers | index($kv)) != null));
            # matchExpressions form: key In [values], any pair of which is a marker.
            def serving_exprs($e): (($e // [])
                | any(. as $x
                      | (($x.values // [])
                         | any($x.key + "=" + (.|tostring) as $kv
                               | ($markers | index($kv)) != null))));
            .items[]?
            | select((.metadata.labels["app.kubernetes.io/component"] // "")
                     != "modelserver-metrics")
            | . as $m
            | (.spec.selector.matchLabels // {}) as $sel
            | select(
                if $kind == "podmonitors" then
                    serving_map($sel) or serving_exprs(.spec.selector.matchExpressions)
                else
                    # Service labels satisfy the monitor selector, and that
                    # Service in turn selects serving pods. An empty selector is
                    # not treated as "matches everything" here: it would call
                    # every namespace with a Service already-scraped and suppress
                    # the warning, which is the failure this check exists to catch.
                    ($sel | length) > 0
                    and ($services | any(
                            (.labels) as $svc
                            | ($sel | to_entries | all($svc[.key] == .value))
                              and serving_map(.selector)))
                end)
            | "\($kind|rtrimstr("s"))/\($m.metadata.name)"' 2>/dev/null || true
    done
    return 0
}

# wva_serving_workload_count echoes how many workloads in $1 are model servers,
# read from the pod template -- the same basis discovery uses, so it counts a
# workload scaled to zero.
#
# Markers and pod paths both come from scaledobject.sh instead of being restated.
# Undercounting does not produce a wrong number here, it produces NO CHECK: the
# caller returns early on zero, so a namespace whose servers carry
# llm-d.ai/inferenceServing but no role label would skip the warning entirely --
# reproducing, for that shape, the silent no-metrics failure this exists to catch.
#
# Both Deployment and LeaderWorkerSet, matching how scaledobject.sh scans:
# multi-node / disaggregated serving is a supported shape, and a Deployment-only
# count would silently no-op for a namespace whose decode/prefill pods are owned
# by a LWS.
wva_serving_workload_count() {
    local ns="$1" total=0 kind resource pod n markers
    markers="$(so_serving_markers_json)"
    for kind in Deployment LeaderWorkerSet; do
        resource='deployments'; pod="$SO_POD_PATH_DEPLOYMENT"
        if [ "$kind" = "LeaderWorkerSet" ]; then
            resource='leaderworkersets'; pod="$SO_POD_PATH_LWS"
        fi
        # `|| true` for the same reason as above -- most clusters have no
        # LeaderWorkerSet CRD, and that kubectl exits non-zero.
        n=$(kubectl get "$resource" -n "$ns" -o json 2>/dev/null \
            | jq --argjson p "$pod" --argjson markers "$markers" '
                [ .items[]?
                  | (getpath($p + ["metadata","labels"]) // {})
                  | select(to_entries
                           | any(.key + "=" + (.value|tostring) as $kv
                                 | ($markers | index($kv)) != null))
                ] | length' 2>/dev/null || true)
        total=$((total + ${n:-0}))
    done
    echo "$total"
}

# wva_report_modelserver_metrics is the read-only half, for the preflight: say
# whether anything scrapes the model servers, without applying anything.
wva_report_modelserver_metrics() {
    local ns="${WVA_WATCH_NS:-$WVA_NS}" conflict servers

    kubectl get crd podmonitors.monitoring.coreos.com >/dev/null 2>&1 || {
        log_info "No PodMonitor CRD on this cluster, so model-server scraping cannot be checked or created here."
        return 0
    }

    servers="$(wva_serving_workload_count "$ns")"
    [ "${servers:-0}" -gt 0 ] || return 0

    # Can this identity actually SEE monitors here? A Forbidden list is not an empty
    # one, and this check refuses installs -- so "I could not look" must not be
    # reported as "nothing scrapes them". Matches how the EPP gate above separates a
    # confirmed-absent signal from an unconfirmed one; the two gates refuse on the
    # same grounds or neither is trustworthy.
    if ! kubectl auth can-i list podmonitors -n "$ns" >/dev/null 2>&1; then
        log_warning "  Model-server metrics: cannot tell whether anything scrapes the $servers model server(s) in $ns — listing PodMonitors there is not permitted for this identity."
        log_warning "    Not treated as missing: refusing an install over a question this check was not allowed to ask would block a correctly monitored namespace."
        return 0
    fi

    conflict="$(wva_modelserver_scrape_conflict "$ns" | paste -sd, -)"
    if [ -n "$conflict" ]; then
        log_success "  Model-server metrics: $conflict already scrapes them."
        return 0
    fi
    if kubectl get podmonitor -n "$ns" -l app.kubernetes.io/component=modelserver-metrics \
         --no-headers 2>/dev/null | grep -q .; then
        log_success "  Model-server metrics: WVA's own PodMonitor is in place."
        return 0
    fi
    log_warning "  Monitoring not enabled for your llm-d servers; please enable Model-server metrics."
    log_warning "    This is llm-d's step 3, \"(Optional) Enable monitoring\" — optional for llm-d, required for WVA."
    log_warning "      From an llm-d checkout:"
    log_warning "        kubectl apply -n $ns -k \$REPO_ROOT/guides/recipes/modelserver/components/monitoring"
    log_warning "        (and .../components/monitoring-pd as well, for a prefill/decode split)"
    log_warning "      Or directly:"
    log_warning "        kubectl apply -n $ns -k config/modelserver-metrics"
    return 1
}

# wva_require_modelserver_metrics turns that report into a stop.
#
# Same class as the EPP gate, and for the same reason: llm-d's monitoring step is
# "(Optional) Enable monitoring" -- optional FOR LLM-D, required for WVA -- so a
# correctly-followed llm-d install can leave a namespace WVA cannot size anything in,
# and every symptom of that is silent. Refusing here asks the caller to finish llm-d,
# which is where the fix actually lives. Full rationale lives once in the guide's
# "How it works" section, not repeated here.
#
# SKIP_CHECKS=true is the bypass, and the only one. A per-check WVA_ALLOW_* flag was
# tried and removed: the scenario is already selected by WVA_SCOPE, and a second knob
# for the same decision is a second thing to get wrong.
#
# Worth revisiting later: rather than refusing, WVA could DEGRADE knowingly -- keep
# running with engine metrics only and say which capabilities are off. That is the
# better answer and a bigger change; refusing is the honest interim.
wva_require_modelserver_metrics() {
    wva_report_modelserver_metrics && return 0
    log_error "Refusing to continue — fix monitoring above, or SKIP_CHECKS=true (see docs/deployment/operations.md#first-line-troubleshooting)."
}

# There is deliberately no wva_ensure_modelserver_metrics — the installer does not
# create this object, it only reports that it is missing.
#
# An earlier version did create it, in the prereqs phase, and the reasoning for
# removing it is worth keeping:
#
#   ownership   the PodMonitor scrapes llm-d's pods and is DEFINED by llm-d
#               (guides/recipes/modelserver/components/monitoring). WVA creating it
#               means WVA owns an object about someone else's workload, in someone
#               else's namespace.
#   cleanup     and would then leak it. cleanup.sh deletes what the WVA overlay
#               renders; anything applied separately with `kubectl apply -k` into
#               the workload namespace survives `undeploy-wva`. The FMA launcher
#               PodMonitor above already has this problem; adding a second was the
#               wrong direction.
#   drift       our copy silently falls behind if llm-d changes the port name or
#               adds relabelings for a new server shape.
#
# The check is the half that carried the value anyway: this defect's whole cost was
# that nothing said the metrics were missing, not that the object was hard to
# create. Naming the problem and printing llm-d's own command is a ten-second fix
# for the reader, and leaves the manifest owned by the project that defines it.

foreign_prometheus() {
    kubectl get crd prometheuses.monitoring.coreos.com >/dev/null 2>&1 || return 0
    kubectl get prometheuses.monitoring.coreos.com -A \
        -o go-template="{{range .items}}{{if ne .metadata.namespace \"${MONITORING_NAMESPACE}\"}}{{.metadata.namespace}}/{{.metadata.name}} {{end}}{{end}}" 2>/dev/null || true
    return 0
}

# install_operational_dashboard publishes the WVA dashboard as a sidecar-labelled
# ConfigMap.
#
# It lives OUTSIDE the kube-prometheus-stack install because the dashboard and the
# Prometheus are independent decisions. It used to be a block inside that install,
# so the dashboard shipped only when this script also deployed Prometheus — meaning
# the case that needs it most, an existing cluster with its own monitoring, was the
# one case that never got it. Whoever operates WVA there was left reading raw
# PromQL for panels we already ship.
#
# The Grafana sidecar discovers dashboards by label, not by namespace ownership, so
# a plain labelled ConfigMap is all an existing Grafana needs. DASHBOARD_NS targets
# a Grafana living somewhere other than MONITORING_NAMESPACE.
# wva_grafana_dashboard_search_ns echoes the namespaces a Grafana dashboard
# sidecar watches, or nothing if that cannot be determined.
#
# This is the difference between a dashboard that appears and a ConfigMap nobody
# reads. The sidecar discovers dashboards by LABEL, but only inside the namespaces
# it is told to watch: kube-prometheus-stack passes searchNamespace through as the
# sidecar container's NAMESPACE env var, and its default is the sidecar's own
# namespace. Publishing a correctly-labelled ConfigMap anywhere else succeeds and
# is then silently ignored -- there is no error to notice, which is why this is
# detected rather than documented.
wva_grafana_dashboard_search_ns() {
    local ns="$1" out
    out=$(kubectl get deploy,statefulset -n "$ns" -l app.kubernetes.io/name=grafana \
            -o jsonpath='{range .items[*]}{range .spec.template.spec.containers[*]}{.name}{"~"}{range .env[*]}{.name}{"="}{.value}{","}{end}{"\n"}{end}{end}' \
            2>/dev/null) || return 0
    printf '%s\n' "$out" | awk -F'~' '/sc-dashboard/ {print $2}' \
        | tr ',' '\n' | sed -n 's/^NAMESPACE=//p' | head -1
}

# wva_dashboard_ns_is_watched reports whether $1 is covered by the watch list $2.
wva_dashboard_ns_is_watched() {
    local ns="$1" watched="$2"
    case "$watched" in
        ALL|all) return 0 ;;
        "")      return 1 ;;
    esac
    case ",${watched}," in
        *",${ns},"*) return 0 ;;
    esac
    return 1
}

# wva_choose_dashboard_ns picks the namespace to publish into.
#
# ONE SHARED OBJECT REMAINS THE DEFAULT, deliberately: the ConfigMap name is fixed
# and the dashboard is generic and variable-driven, so ten namespace-scoped installs
# applying the same object is better than ten identical entries in the picker. See
# the note below on pinning.
#
# This function therefore does NOT switch to a per-tenant copy on its own, even
# when Grafana would see it. Detection is used to make the ADVICE accurate -- to
# suggest DASHBOARD_NS only when it would actually render, and to warn when the
# chosen namespace is not watched -- not to change where the dashboard lands.
wva_choose_dashboard_ns() {
    printf '%s' "${DASHBOARD_NS:-$MONITORING_NAMESPACE}"
}

# wva_dashboard_manual_help prints the two routes that need no cluster rights.
# Printed at the moment of failure, because that is when it is read.
wva_dashboard_manual_help() {
    local ns="$1" watched
    watched="$(wva_grafana_dashboard_search_ns "$MONITORING_NAMESPACE")"
    if wva_dashboard_ns_is_watched "$WVA_NS" "$watched"; then
        log_info "  Grafana watches '${watched}', so this works with no admin at all:"
        log_info "    DASHBOARD_NS=$WVA_NS  (publishes a private copy into your own namespace)"
    fi
    log_info "  Fastest, and needs NO Kubernetes permission:"
    log_info "    In Grafana: Dashboards -> New -> Import -> upload"
    log_info "    deploy/grafana/operational-dashboard.json"
    log_info "  Or send a cluster admin this:"
    log_info "    kubectl create configmap wva-operation-dashboard -n $ns \\"
    log_info "      --from-file=operational-dashboard.json=deploy/grafana/operational-dashboard.json \\"
    log_info "      && kubectl label configmap wva-operation-dashboard -n $ns grafana_dashboard=1"
}

install_operational_dashboard() {
    [ "${DEPLOY_OPERATIONAL_DASHBOARD:-true}" = "true" ] || return 0

    local json="$WVA_PROJECT/deploy/grafana/operational-dashboard.json"
    local ns watched
    ns="$(wva_choose_dashboard_ns)"
    watched="$(wva_grafana_dashboard_search_ns "$MONITORING_NAMESPACE")"
    if [ ! -f "$json" ]; then
        log_warning "Operational dashboard JSON not found at $json — skipping"
        return 0
    fi
    # Forbidden is not absent. Publishing into a SHARED monitoring namespace is a
    # cluster-admin action, and a namespace admin running `make deploy-wva` will
    # be denied here -- telling them the namespace "does not exist" sends them to
    # create one, when what they need is either their admin or a copy in their
    # own namespace.
    local ns_err
    if ! ns_err=$(kubectl get namespace "$ns" 2>&1 >/dev/null); then
        case "$ns_err" in
            *[Ff]orbidden*)
                log_info "No permission to read namespace $ns — the shared dashboard belongs to whoever administers monitoring."
                log_info "  The install is unaffected; only this dashboard step is skipped."
                wva_dashboard_manual_help "$ns"
                return 0
                ;;
            *)
                log_warning "Namespace $ns does not exist — skipping the operational dashboard. Set DASHBOARD_NS to the namespace your Grafana watches."
                return 0
                ;;
        esac
    fi

    # ONE dashboard object serves every install on the cluster: the name is
    # fixed and the namespace is shared, so ten namespace-scoped installs apply
    # the same ConfigMap. That is deliberate -- the dashboard is generic and
    # driven by variables, and ten identical copies in the picker would be
    # worse -- but it means an OLDER install must not quietly revert a newer
    # one. The version it was published with is stamped on the object, and a
    # lower version leaves it alone and says so.
    local ours existing
    ours="${IMG##*:}"
    [ -n "$ours" ] && [ "$ours" != "$IMG" ] || ours="unknown"
    existing=$(kubectl get configmap wva-operation-dashboard -n "$ns" \
        -o jsonpath='{.metadata.annotations.wva\.llmd\.ai/dashboard-version}' 2>/dev/null || true)

    # Compare only when BOTH sides look like versions. sort -V on a tag like
    # `wva-ext`, `latest` or `main` orders it against v0.7.0 by ASCII accident --
    # `wva-ext` would win and block every release publish thereafter, which is a
    # silent way to freeze a cluster's dashboard on somebody's dev build.
    # Compare the NUMERIC core only. sort -V puts v0.8.0-rc4 after v0.8.0, the
    # opposite of what a pre-release means, so an rc install would block the GA
    # release's dashboard for good.
    local semver='^v?[0-9]+\.[0-9]+' existing_core ours_core
    existing_core=$(printf '%s' "$existing" | sed 's/^v//; s/-.*$//')
    ours_core=$(printf '%s' "$ours" | sed 's/^v//; s/-.*$//')
    if [ -n "$existing" ] && [ "$existing_core" != "$ours_core" ] && \
       printf '%s' "$existing" | grep -qE "$semver" && \
       printf '%s' "$ours" | grep -qE "$semver" && \
       [ "$(printf '%s\n%s\n' "$existing_core" "$ours_core" | sort -V | tail -1)" = "$existing_core" ]; then
        log_info "Dashboard in $ns was published by $existing, newer than this install ($ours) — leaving it alone."
        log_info "  Republish with: kubectl delete configmap wva-operation-dashboard -n $ns"
        return 0
    fi

    # Pinning the namespace variable is only safe when this dashboard is NOT the
    # shared one. The ConfigMap name is fixed, so if it lands in a shared
    # monitoring namespace then ten installs write the same object -- pinning it
    # to one tenant's namespace would mean everyone sees whoever installed last,
    # which is worse than the "All" it replaced. Pin only when the dashboard is
    # published into the install's OWN namespace, where nobody else can be
    # looking at it.
    local scope patched
    scope="$(wva_install_scope)"
    patched="$(mktemp)"
    if [ "$scope" = "namespace" ] && [ "$ns" != "$WVA_NS" ]; then
        cp "$json" "$patched"
        log_info "Dashboard published to the shared $ns — namespace left selectable."
        if wva_dashboard_ns_is_watched "$WVA_NS" "$watched"; then
            log_info "  DASHBOARD_NS=$WVA_NS would publish a copy pinned to this namespace instead."
        fi
    elif [ "$scope" = "namespace" ]; then
        yq -o=json -I2 \
            '(.templating.list[] | select(.name == "namespace") | .current) =
                 {"text": "'"$WVA_NS"'", "value": "'"$WVA_NS"'"} |
             (.templating.list[] | select(.name == "namespace") | .includeAll) = false' \
            "$json" > "$patched" 2>/dev/null || cp "$json" "$patched"
        log_info "Dashboard scoped to $WVA_NS (namespace-scoped install)"
    else
        cp "$json" "$patched"
        log_info "Dashboard left cluster-wide (cluster-scoped install)"
    fi

    if kubectl create configmap wva-operation-dashboard \
        --from-file=operational-dashboard.json="$patched" \
        -n "$ns" --dry-run=client -o yaml \
        | kubectl label --local -f - grafana_dashboard=1 -o yaml \
        | kubectl annotate --local -f - "wva.llmd.ai/dashboard-version=$ours" -o yaml \
        | kubectl apply -f - >/dev/null; then
        log_success "Operational dashboard published to $ns (ConfigMap wva-operation-dashboard, version $ours, label grafana_dashboard=1)"
        # Publishing is not the same as appearing. Say so now rather than leaving
        # someone to conclude the dashboard is broken.
        if [ -n "$watched" ] && ! wva_dashboard_ns_is_watched "$ns" "$watched"; then
            log_warning "Grafana's sidecar watches '${watched}', not $ns — this ConfigMap will NOT appear as a dashboard."
            log_info "  Ask for grafana.sidecar.dashboards.searchNamespace=ALL, or:"
            wva_dashboard_manual_help "$ns"
        fi
        # The tenant's scoping is a LINK, not a default. A variable default lives
        # in the dashboard JSON, and this object is shared by every install on
        # the cluster -- so there is no per-tenant default to set. A URL gives
        # each namespace its own entry point into the one dashboard, with no
        # collisions and nothing for the next install to overwrite.
        local uid slug
        uid=$(yq -r '.uid // "wva-operational"' "$json" 2>/dev/null)
        slug=$(yq -r '.title // "wva"' "$json" 2>/dev/null \
               | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]\+/-/g; s/^-//; s/-$//')
        log_info "  This namespace's view: <your-grafana>/d/$uid/$slug?var-namespace=$WVA_NS"
        log_info "  Cluster-wide view:     <your-grafana>/d/$uid/$slug"
    else
        # Same distinction on the write: a tenant who can READ the shared
        # namespace still cannot usually create ConfigMaps in it.
        local apply_err
        apply_err=$(kubectl create configmap wva-operation-dashboard \
            --from-file=operational-dashboard.json="$patched" \
            -n "$ns" --dry-run=client -o yaml 2>&1 \
            | kubectl apply --dry-run=server -f - 2>&1 >/dev/null || true)
        case "$apply_err" in
            *[Ff]orbidden*)
                log_info "No permission to write the dashboard into $ns — it is the monitoring namespace's to own."
                log_info "  The install is unaffected; only this dashboard step is skipped."
                wva_dashboard_manual_help "$ns"
                ;;
            *)
                log_warning "Could not publish the operational dashboard to $ns: ${apply_err:-unknown error}"
                ;;
        esac
    fi
    rm -f "$patched"
}

# wva_detect_prometheus_url echoes an in-cluster URL for a Prometheus that is
# already here, or nothing.
#
# The platform defaults name the Prometheus THIS INSTALLER WOULD DEPLOY. On a
# cluster that has its own — the common case, and the whole point of the
# existing-llm-d path — that default is silently wrong, and WVA exits on a
# Prometheus it cannot reach. It presents as CrashLoopBackOff, which reads like a
# broken image rather than a setting nobody was asked for.
#
# Detection order is most-specific first. Everything here is a READ; the caller
# decides whether to use the answer.
wva_detect_prometheus_url() {
    local ns name port scheme

    # OpenShift: THANOS, at a well-known address, and no lookup at all.
    #
    # The platform's aggregation point is thanos-querier in openshift-monitoring,
    # and that is a constant — not something to discover. Reading it would need
    # access to a platform namespace that a tenant does not have, so a version of
    # this that "detected" it reported NOTHING for exactly the reader who most
    # needs the answer. (Verified on a real cluster: a namespace admin gets
    # nothing back.) Knowing beats looking.
    if wva_is_openshift; then
        # Refine the port only if we happen to be able to read it. 9091 is the
        # TLS-terminated one the platform has served for years; the read is a
        # nicety, never a requirement.
        port="$(kubectl get svc thanos-querier -n openshift-monitoring \
            -o jsonpath='{.spec.ports[?(@.name=="web")].port}' 2>/dev/null || true)"
        echo "https://thanos-querier.openshift-monitoring.svc.cluster.local:${port:-9091}"
        return 0
    fi

    # A Prometheus Operator install: every Prometheus CR gets a `prometheus-operated`
    # Service in its own namespace, whatever the release is called — so this holds
    # for kube-prometheus-stack and for a hand-rolled operator install alike.
    while read -r ns name; do
        [ -n "$ns" ] || continue
        if kubectl get svc prometheus-operated -n "$ns" >/dev/null 2>&1; then
            port="$(kubectl get svc prometheus-operated -n "$ns" \
                -o jsonpath='{.spec.ports[?(@.name=="web")].port}' 2>/dev/null || true)"
            # The scheme comes from the Prometheus CR, not from the port number. A
            # Prometheus with spec.web.tlsConfig serves HTTPS on its ordinary 9090 —
            # which is what this repo's own kube-prometheus-stack install produces —
            # so calling it http:// pointed WVA at a TLS port in plaintext. Worse than
            # the reset that would cause: the controller refuses an http:// Prometheus
            # outright, while the pod that had already started kept running on the old
            # value. The install went green and the next restart CrashLoopBackOff-ed.
            scheme=http
            if [ -n "$(kubectl get prometheuses.monitoring.coreos.com "$name" -n "$ns" \
                -o jsonpath='{.spec.web.tlsConfig}' 2>/dev/null || true)" ]; then
                scheme=https
            fi
            echo "${scheme}://prometheus-operated.${ns}.svc.cluster.local:${port:-9090}"
            return 0
        fi
    done < <(kubectl get prometheuses.monitoring.coreos.com -A \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)

    # Last resort: a Service serving a Prometheus web port, in the namespaces this
    # install already knows about. Deliberately NOT `get svc -A`: on a large
    # shared cluster that is slow enough to look hung, and a tenant cannot list
    # services cluster-wide anyway, so it would be a long wait for a denial.
    for ns in "${MONITORING_NAMESPACE:-}" "${WVA_NS:-}" "${NAMESPACE:-}" monitoring prometheus; do
        [ -n "$ns" ] || continue
        while read -r name port; do
            [ -n "$name" ] || continue
            case "$name" in *prometheus*|*thanos-quer*) : ;; *) continue ;; esac
            scheme=http
            # The scheme follows the port: a TLS port answered over http gives
            # "connection reset", which is a far worse thing to debug than
            # "no Prometheus found".
            case "$port" in 9091|8443) scheme=https ;; esac
            echo "${scheme}://${name}.${ns}.svc.cluster.local:${port}"
            return 0
        done < <(kubectl get svc -n "$ns" \
            -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.ports[0].port}{"\n"}{end}' 2>/dev/null || true)
    done

    return 0
}

# wva_is_openshift reports whether this is an OpenShift cluster.
#
# ENVIRONMENT first, because the make targets set it and it costs nothing. The
# fallback asks API DISCOVERY, which every authenticated user may read — unlike
# the platform namespaces, which a tenant may not.
wva_is_openshift() {
    [ "${ENVIRONMENT:-}" = "openshift" ] && return 0
    kubectl api-resources --api-group=route.openshift.io -o name >/dev/null 2>&1 \
        && [ -n "$(kubectl api-resources --api-group=route.openshift.io -o name 2>/dev/null)" ]
}

# wva_report_prometheus tells the reader which Prometheus this install will use,
# and how that was decided. Called by the preflight, where "what do I pass for
# PROMETHEUS_URL?" is the question actually being asked.
wva_report_prometheus() {
    local detected
    if [ -n "${PROMETHEUS_URL_EXPLICIT:-}" ]; then
        log_info "Prometheus: $PROMETHEUS_URL_EXPLICIT (you set PROMETHEUS_URL)"
        return 0
    fi
    detected="$(wva_detect_prometheus_url)"
    if [ -n "$detected" ]; then
        log_success "Prometheus: $detected"
        if wva_is_openshift; then
            log_info "  OpenShift's Thanos Querier — a fixed address, so this needs no permission to know."
        fi
        log_info "  You do not need to pass PROMETHEUS_URL. Set it only to override this."
        return 0
    fi
    # Never fold "I may not look" into "there is none": one is a missing
    # permission and the other is a missing Prometheus, and they are fixed by
    # different people.
    if ! kubectl auth can-i list prometheuses.monitoring.coreos.com -A >/dev/null 2>&1; then
        log_warning "Could not look for a Prometheus (listing them cluster-wide is not permitted for you), so this cannot tell you the URL."
        log_warning "  Ask whoever runs your monitoring for it, and pass PROMETHEUS_URL=<url>."
        return 0
    fi
    log_warning "No Prometheus found on this cluster. The install will deploy one (DEPLOY_PROMETHEUS=true), or pass PROMETHEUS_URL=<url> to point at one outside the cluster."
}

deploy_monitoring_stack() {
    if [ "$DEPLOY_PROMETHEUS" != "true" ]; then
        log_info "Skipping Prometheus deployment (DEPLOY_PROMETHEUS=false) — WVA will read ${PROMETHEUS_URL:-the configured URL}"
        install_operational_dashboard
        return 0
    fi

    # DEPLOY_PROMETHEUS defaults to true because the new-cluster path needs a
    # Prometheus and most people running this have none. That default is wrong for
    # the OTHER common case — an existing llm-d cluster, which always has one — so
    # detect rather than obey. KEDA is handled the same way a few files over; this
    # was the one piece of shared infrastructure that still installed blind.
    local existing
    existing="$(foreign_prometheus)"
    if [ -n "$existing" ] && [ "${PROMETHEUS_FORCE_INSTALL:-false}" != "true" ]; then
        log_info "Prometheus is already on this cluster (${existing% }) — not installing a second one."
        log_info "  WVA will scrape ${PROMETHEUS_URL:-the shipped default}. If that is not the right endpoint, re-run with PROMETHEUS_URL=<url>."
        log_info "  Pass PROMETHEUS_FORCE_INSTALL=true to install anyway (two Prometheus Operators reconcile the same CRs and will fight)."
        # The dashboard is still ours to ship, and this is precisely the cluster
        # whose operator has a Grafana to put it in.
        install_operational_dashboard
        return 0
    fi
    if [ -n "$existing" ]; then
        log_warning "PROMETHEUS_FORCE_INSTALL=true with Prometheus already present (${existing% }). The two operators will contend over the same Prometheus and ServiceMonitor resources."
    fi

    deploy_prometheus_stack
}

# --- EPP flow control ---------------------------------------------------------
#
# WVA needs the EPP's flow-control queue, and the guide understates it as a
# scale-from-zero concern. Three things depend on it:
#
#   scale-from-zero   at 0 replicas there are no engine metrics, so the scheduler
#                     queue is the ONLY evidence anyone is asking for the model.
#                     Without it a parked workload never wakes.
#   arrival rate      domain/analyzer.go: "Zero when the metric is unavailable (EPP
#                     absent or no traffic yet)". Absence is indistinguishable from
#                     idleness, so requests queued but not yet dispatched are
#                     invisible to the demand model.
#   wva_unmeasured_queue
#                     the detector for "serving traffic through pods WVA cannot
#                     attribute" — an unscraped FMA launcher, a PodMonitor naming a
#                     port the pods do not declare, ownerReferences reaching no
#                     scale target. It is sourced from this queue PRECISELY because
#                     it does not depend on any engine pod being scraped. With the
#                     gate off it reads 0 forever, so the safety net is disabled by
#                     the same absence it exists to catch.
#
# The gate is not a container flag. It is declared in the EPP's own plugins config:
#
#     featureGates:
#       - flowControl
#
# Measured on pokprod001: the namespaces whose EPP config carries that list have
# inference_extension_flow_control_queue_size series; the one built from llm-d's
# optimized-baseline guide has no featureGates section and no series.
#
# WVA reads the queue by scraping the EPP directly with a token mounted at
# /var/run/secrets/epp-metrics/token, and falls back to the same metric in
# Prometheus. Either path is fine — but neither has anything to read while the gate
# is off, which is why this checks the gate rather than the plumbing.

# wva_epp_services echoes the EPP Service each InferencePool in $1 points at.
# Read from the pool's endpointPickerRef rather than guessed from a name, because
# that reference is what the gateway itself dials.
wva_epp_services() {
    local ns="$1" kind
    for kind in inferencepools.inference.networking.k8s.io inferencepools.inference.networking.x-k8s.io; do
        kubectl get "$kind" -n "$ns" -o json 2>/dev/null | jq -r '
            .items[]?
            | (.spec.endpointPickerRef // .spec.extensionRef // {})
            | select((.name // "") != "")
            | .name' 2>/dev/null
    done | sort -u
}

# wva_epp_flowcontrol_state echoes on|off|unknown for the EPP behind Service $2.
#
# Resolves the config the way the container does: the --config-file argument names
# a path, the volumeMount covering that path names a volume, and the volume names
# the ConfigMap and therefore the key. Guessing "<service>-plugins.yaml" would be
# right here and wrong for any guide that names its file differently.
wva_epp_flowcontrol_state() {
    local ns="$1" svc="$2" sel pod cfg key sub cm data
    sel=$(kubectl get svc "$svc" -n "$ns" -o json 2>/dev/null \
        | jq -r '(.spec.selector // {}) | to_entries | map(.key + "=" + .value) | join(",")')
    [ -n "$sel" ] || { echo unreadable; return 0; }
    # One fetch, reused for every field below.
    pod=$(kubectl get pods -n "$ns" -l "$sel" -o json 2>/dev/null | jq -c '.items[0] // empty')
    # No EPP pod RIGHT NOW is not a gate that is off: an EPP restarting, or one
    # scaled to zero, would otherwise refuse the install.
    [ -n "$pod" ] || { echo nopod; return 0; }

    # `--config-file <path>` and `--config-file=<path>` are both legal.
    cfg=$(printf '%s' "$pod" | jq -r '
        (.spec.containers[] | select((.args // []) | any(. == "--config-file"))
         | (.args | index("--config-file")) as $i | .args[$i + 1]),
        (.spec.containers[].args[]? | select(startswith("--config-file=")) | sub("^--config-file="; ""))
        | select(. != null and . != "")' 2>/dev/null | head -1)
    [ -n "$cfg" ] || { echo unreadable; return 0; }
    key="${cfg##*/}"
    # A subPath mount places ONE ConfigMap key at the exact file path, and that key
    # is the subPath -- which need not equal the file's basename. Read it when the
    # mount is a file mount rather than assuming the two agree.
    sub=$(printf '%s' "$pod" | jq -r --arg cfg "$cfg" '
        .spec.containers[].volumeMounts[]? | select(.mountPath == $cfg) | .subPath // empty' 2>/dev/null | head -1)
    [ -n "$sub" ] && key="$sub"

    # Resolved the way the container resolves it: the volumeMount covering the
    # config's directory names a volume, and that volume names the ConfigMap and
    # therefore the key. Guessing "<service>-plugins.yaml" is right for one guide and
    # wrong for the next.
    # Either shape of mount: the DIRECTORY, or the file itself via subPath, which
    # is a standard way to place a single config file and previously resolved to no
    # ConfigMap at all -- reported as unreadable, which now refuses the install.
    cm=$(printf '%s' "$pod" | jq -r --arg dir "${cfg%/*}" --arg cfg "$cfg" '
        (.spec.containers[].volumeMounts[]? | select(.mountPath == $dir or .mountPath == $cfg) | .name) as $v
        | .spec.volumes[]? | select(.name == $v) | .configMap.name // empty' 2>/dev/null | head -1)
    [ -n "$cm" ] || { echo unreadable; return 0; }

    data=$(kubectl get configmap "$cm" -n "$ns" -o jsonpath="{.data.${key//./\\.}}" 2>/dev/null)
    [ -n "$data" ] || { echo unreadable; return 0; }
    # The gate is an ENTRY in a featureGates list. Matched on the entry rather than on
    # the word appearing anywhere in the file, so a plugin merely named after flow
    # control does not read as the gate being on.
    #
    # Both YAML spellings of a list, because this now REFUSES an install: the block
    # form is what llm-d's guides/flow-control/tuning.md and this repo's own
    # epp-flow-control.values.yaml write, but `featureGates: [flowControl]` is the
    # same thing and is how this gate is described in prose. Quotes and a trailing
    # comment are stripped for the same reason. Reading a correctly enabled gate as
    # off would refuse a working cluster, and the only way past it -- SKIP_CHECKS=true
    # -- switches off every preflight check rather than correcting this one.
    # \042 and \047 are " and ', which would otherwise fight the shell quoting here.
    if printf '%s' "$data" | awk '
        # inline: featureGates: [flowControl, other]
        /^[[:space:]]*featureGates:[[:space:]]*\[/ {
            line = $0
            sub(/^[^[]*\[/, "", line); sub(/\].*$/, "", line)
            n = split(line, a, ",")
            for (i = 1; i <= n; i++) {
                gsub(/[[:space:]\042\047]/, "", a[i])
                if (a[i] == "flowControl") { found = 1; exit }
            }
            next
        }
        /^[[:space:]]*featureGates:/ { inlist = 1; next }
        inlist && /^[[:space:]]*#/ { next }
        inlist && /^[[:space:]]*-/ {
            item = $0
            sub(/^[[:space:]]*-[[:space:]]*/, "", item)
            sub(/[[:space:]]+#.*$/, "", item)
            gsub(/^[\042\047]|[\042\047]$/, "", item)
            sub(/[[:space:]]+$/, "", item)
            if (item == "flowControl") { found = 1; exit }
            next
        }
        inlist && /^[[:space:]]*[^-[:space:]]/ { inlist = 0 }
        END { exit !found }'; then
        echo on
    else
        echo off
    fi
}

# wva_epp_scrapers echoes any PodMonitor/ServiceMonitor in $1 whose matchLabels
# select the EPP pods behind Service $2 — i.e. something scrapes the EPP.
#
# Checked structurally rather than by querying Prometheus, because the Prometheus
# URL is in-cluster and unreachable from wherever the installer runs. matchLabels
# only: a matchExpressions selector is reported as "cannot tell" rather than
# guessed at, since a wrong "yes" here would suppress the warning that matters.
wva_epp_scrapers() {
    local ns="$1" svc="$2" svc_json sel pod svc_labels pod_labels

    # Each object is fetched ONCE. An earlier version re-read the Service twice and
    # the pod three times across this function and the gate check, which cost ~13s
    # per EPP on a real cluster and pushed the preflight past two minutes.
    svc_json=$(kubectl get svc "$svc" -n "$ns" -o json 2>/dev/null)
    [ -n "$svc_json" ] || return 0
    svc_labels=$(printf '%s' "$svc_json" | jq -c '.metadata.labels // {}')
    sel=$(printf '%s' "$svc_json" | jq -r '(.spec.selector // {}) | to_entries | map(.key + "=" + .value) | join(",")')

    pod_labels='{}'
    if [ -n "$sel" ]; then
        pod=$(kubectl get pods -n "$ns" -l "$sel" -o json 2>/dev/null \
            | jq -c '.items[0] // empty')
        [ -n "$pod" ] && pod_labels=$(printf '%s' "$pod" | jq -c '.metadata.labels // {}')
    fi

    # A ServiceMonitor selects SERVICES and a PodMonitor selects PODS, so each is
    # matched against the labels of the object it actually selects. Comparing both
    # against pod labels reported "nothing scrapes the EPP" for a namespace whose EPP
    # ServiceMonitor was working -- its selector names the SERVICE's
    # app.kubernetes.io/name and version, which the pods do not both carry.
    #
    # matchLabels is a subset test: every key/value the monitor names must be present
    # on the target. matchExpressions is not evaluated, so a monitor using only those
    # reads as "not scraping" -- the safe direction, since a wrong "yes" would
    # suppress the warning that matters.
    local subset='
        .items[]?
        | . as $m
        | (.spec.selector.matchLabels // {}) as $ml
        | select(($ml | length) > 0)
        | select([ $ml | to_entries[] | select($l[.key] == .value) ] | length == ($ml | length))
        | $kind + "/" + $m.metadata.name'

    # matchExpressions is still not evaluated, but a monitor using ONLY those is now
    # reported as unevaluated rather than counted as absent. Ignoring them was called
    # "the safe direction, since a wrong yes would suppress the warning that matters"
    # -- true while this was a warning. It now refuses the install, so a wrong NO
    # blocks a correctly monitored cluster, and the safe direction is to say "cannot
    # tell" out loud.
    # Each filter is PARENTHESISED where they are combined below. `|` binds looser
    # than `,` in jq, so "$subset, $unevaluated" parsed as
    #     .items[]? | ... | (name_string, .items[]? | ...)
    # -- the second filter applied to the STRING the first produced, which fails
    # with `Cannot index string with string ("spec")`. stderr is sent to /dev/null
    # here, so the function simply returned nothing, and the gate refused the
    # install of a namespace whose EPP ServiceMonitor was working. Measured on
    # pokprod001: the selector matched the Service 2/2 by hand, `$subset` alone
    # returned the monitor, and the combined program returned an error.
    local unevaluated='
        .items[]?
        | . as $m
        | select((.spec.selector.matchLabels // {} | length) == 0)
        | select((.spec.selector.matchExpressions // [] | length) > 0)
        | "unevaluated:" + $kind + "/" + $m.metadata.name'

    kubectl get servicemonitors -n "$ns" -o json 2>/dev/null         | jq -r --argjson l "$svc_labels" --arg kind servicemonitor "($subset), ($unevaluated)" 2>/dev/null
    kubectl get podmonitors -n "$ns" -o json 2>/dev/null         | jq -r --argjson l "$pod_labels" --arg kind podmonitor "($subset), ($unevaluated)" 2>/dev/null
}

# wva_report_epp_flowcontrol reports both EPP requirements and returns non-zero
# when either is missing, so the caller decides whether that is fatal.
#
# They are SEPARATE requirements with different fixes, measured on pokprod001:
#
#   inference_extension_scheduler_attempts_total   19 namespaces — published
#     (arrival rate -> throughput analyzer)        whenever the EPP is SCRAPED.
#                                                  No gate needed.
#   inference_extension_flow_control_queue_size     4 namespaces — only where the
#     (scale-from-zero, wva_unmeasured_queue)       flowControl GATE is on.
#
# The arrival-rate query is PromQL (registration/throughput_analyzer.go), so it
# needs Prometheus to scrape the EPP; WVA's direct EPP scrape covers only the queue
# fallback. So "the gate is on" and "the EPP is scraped" are both required, and
# neither implies the other.
wva_report_epp_flowcontrol() {
    local ns="${WVA_WATCH_NS:-$WVA_NS}" svc state scrapers unevaluated any=false missing=0 unconfirmed=0

    for svc in $(wva_epp_services "$ns"); do
        any=true

        # Called ONCE and split, not twice: this function issues four kubectl reads
        # per EPP, and calling it again to pick out the unevaluated monitors would
        # undo the fetch-once work above that took a real cluster from ~13s to ~9s.
        local monitors
        monitors="$(wva_epp_scrapers "$ns" "$svc")"
        scrapers="$(printf '%s
' "$monitors" | grep -v '^unevaluated:' | grep -v '^$' | paste -sd, -)"
        unevaluated="$(printf '%s
' "$monitors" | sed -n 's/^unevaluated://p' | paste -sd, -)"
        if [ -n "$scrapers" ]; then
            log_success "  EPP metrics: $svc is scraped by $scrapers (arrival rate, and so the throughput analyzer, depends on this)."
        elif [ -n "$unevaluated" ]; then
            unconfirmed=$((unconfirmed + 1))
            log_warning "  EPP metrics: cannot tell whether $svc is scraped — $unevaluated select by matchExpressions, which this check does not evaluate."
            log_warning "    Not treated as missing: refusing an install over a selector form this script declines to read would block a correctly monitored cluster."
            log_warning "    Confirm by hand:  kubectl get --raw /api/v1/namespaces/$ns/services/$svc:metrics/proxy/metrics | head"
        else
            missing=$((missing + 1))
            log_warning "  Router monitoring not enabled; please enable EPP metrics."
            log_warning "    llm-d ships the monitors: kubectl apply -n $ns -k \$REPO_ROOT/guides/recipes/observability"
        fi

        state=$(wva_epp_flowcontrol_state "$ns" "$svc")
        case "$state" in
            on)
                log_success "  EPP flow control: enabled on $svc (its config declares featureGates: [flowControl])." ;;
            off)
                missing=$((missing + 1))
                log_warning "  Router queue not enabled; please turn on flow control for EPP."
                log_warning "    Enable it in the EPP's EndpointPickerConfig:"
                log_warning "        featureGates:"
                log_warning "        - flowControl"
                log_warning "    llm-d documents it in guides/flow-control/tuning.md, and ships a ready router values file for this path:"
                log_warning "        guides/workload-autoscaling/keda-epp-queue/<guide>/router.values.yaml"
                log_warning "    Re-run your llm-d router install with that layered on, then restart the EPP." ;;
            nopod)
                # No EPP pod at this instant. A restarting or scaled-to-zero EPP is
                # not a gate that is off, and refusing the install for it would block
                # on a condition that resolves itself.
                unconfirmed=$((unconfirmed + 1))
                log_warning "  EPP flow control: no running pod behind $svc, so the gate cannot be read right now. Not treated as missing; re-run once the EPP is up." ;;
            *)
                # Fails CLOSED, deliberately: "cannot READ the config" is the case
                # where this kubectl identity can see the Service but not the Pods or
                # ConfigMaps -- check_permissions verifies only create rights on what
                # the install creates, never read rights on these -- and an unconfirmed
                # signal is exactly what this check exists to stop. Distinct from
                # `nopod` above, which is transient rather than a permissions gap.
                missing=$((missing + 1))
                log_warning "  EPP flow control: could not read $svc's plugins config, so this cannot confirm the gate is on. Check that its EndpointPickerConfig declares the flowControl feature gate, and that this kubectl identity can read Pods and ConfigMaps in $ns." ;;
        esac
    done

    if [ "$any" = false ]; then
        log_info "  EPP: no InferencePool in $ns names an endpoint picker, so there is no EPP to check yet."
        return 0
    fi
    [ "$unconfirmed" -eq 0 ] || log_info "  $unconfirmed EPP signal(s) could not be confirmed either way; these do not block the install."
    # Only CONFIRMED-absent signals are fatal. "I could not look" and "I looked and
    # it is not there" are different answers, and only the second is evidence about
    # the cluster.
    [ "$missing" -eq 0 ]
}

# wva_require_epp_metrics turns the report into a gate.
#
# Fatal by default, because every symptom of these being absent is silent: WVA
# still emits decisions, the HPA still reads a healthy ratio, and the one metric
# that would have flagged it is sourced from the missing signal. An install that
# cannot see demand is not a working install, and saying so at install time is the
# only place anybody is looking.
wva_require_epp_metrics() {
    wva_report_epp_flowcontrol && return 0
    log_error "Refusing to continue — fix the EPP above, or SKIP_CHECKS=true (see docs/deployment/operations.md#first-line-troubleshooting)."
}
