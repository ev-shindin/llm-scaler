#!/usr/bin/env bash
#
# `make dashboard`: a private Grafana instance for one namespace, with WVA's
# operational dashboard already imported.
#
# Productizes what a local `config/grafana-private/` experiment proved by hand. That
# directory has never been in this repository, so the measurements it rests on are
# recorded in docs/developer-guide/guide-review-install-in-namespace.md and inline
# below, not behind a path you cannot open. Three decisions carry over unchanged:
#
#   grafana-operator, not the shared cluster Grafana   a shared instance's
#     dashboards are read-only, and its sidecar was measured watching only its own
#     namespace on the cluster this was built against.
#   Thanos via the TENANCY port (9092), not cluster-monitoring-view   the usual
#     recipe for this pattern grants a cluster-scoped role that lets a "private"
#     dashboard read every tenant's metrics. Measured: 9092 + a namespace query
#     param returns exactly this namespace's data with a NAMESPACED RoleBinding,
#     zero cluster-scoped grants.
#   a GrafanaDashboard CR as the bridge   grafana-operator ignores the
#     grafana_dashboard=1 labelled ConfigMap WVA already publishes (that is the
#     k8s-sidecar convention, a different mechanism) -- it only imports what a
#     GrafanaDashboard CR's configMapRef names.
#
# Requires vars: WVA_PROJECT, IMG (both already required by install_operational_dashboard).
# Requires funcs: log_info/log_success/log_warning/log_error,
#   install_operational_dashboard, wva_is_openshift, wva_serving_workload_count
#   (infra_monitoring.sh); so_serving_markers_json (scaledobject.sh).

# WVA_GRAFANA_NAME is the Grafana CR's name, fixed rather than derived from the
# namespace: it is namespace-scoped already (one instance per namespace this runs
# against), so there is nothing to disambiguate, and a fixed name is what let every
# other object below (Service, Route, admin-credentials Secret) be found by name
# rather than re-discovered -- the operator derives all of them from this one.
readonly WVA_GRAFANA_NAME=wva-grafana
readonly WVA_GRAFANA_DASHBOARD_UID=wva-op-dash-v2
readonly WVA_GRAFANA_DASHBOARD_SLUG=wva-operational-dashboard

# WVA_GRAFANA_DS_SA is the identity the DATASOURCE authenticates as -- separate
# from, and NOT `${WVA_GRAFANA_NAME}-sa`, which collides with grafana-operator's
# own auto-created ServiceAccount for the Grafana POD itself (owned by the Grafana
# CR, labelled app.kubernetes.io/managed-by=grafana-operator). Measured: applying a
# ServiceAccount under that name did not break anything -- kubectl apply's 3-way
# merge preserved the operator's labels and ownerReferences -- but it silently
# layered a second, unrelated purpose (datasource auth) onto an object the operator
# considers its own, which is the kind of thing that breaks on the operator's next
# upgrade. The experiment avoided this by naming it plain `grafana-sa`;
# this generalizes that choice with an explicit, unambiguous suffix instead.
readonly WVA_GRAFANA_DS_SA=${WVA_GRAFANA_NAME}-datasource

# wva_dashboard_require_crds stops unless grafana-operator's CRDs are installed.
#
# A fatal check, not a report: everything below is a CR, and applying one against
# an absent CRD fails on the FIRST object with a message that names neither the
# operator nor where to get it. Installing the operator itself is a cluster-admin
# action (it is cluster-scoped, watching every namespace) and stays out of scope
# here for the same reason WVA does not install Prometheus operators either --
# this tool creates namespaced objects only.
wva_dashboard_require_crds() {
    local crd missing=()
    for crd in grafanas.grafana.integreatly.org \
               grafanadatasources.grafana.integreatly.org \
               grafanadashboards.grafana.integreatly.org; do
        kubectl get crd "$crd" >/dev/null 2>&1 || missing+=("$crd")
    done
    [ ${#missing[@]} -eq 0 ] && return 0
    log_error "grafana-operator is not installed on this cluster (missing CRD(s): ${missing[*]}).

This is a cluster-admin action -- installing an operator is cluster-scoped, watching
every namespace, which is exactly what this tool does not do on your behalf. Ask a
cluster admin to install the Grafana Operator (OperatorHub, or
https://github.com/grafana/grafana-operator), then re-run 'make dashboard'."
}

# wva_dashboard_apply creates or updates everything a private Grafana in $1 needs.
# Idempotent: every object is applied with `kubectl apply`, which is a no-op when
# nothing changed, and the two ordering-sensitive steps (RBAC before the SA needs
# it; the dashboard ConfigMap before the CR that reads it) are ordered accordingly.
wva_dashboard_apply() {
    local ns="$1"

    log_info "Applying Grafana RBAC in $ns..."
    # Namespaced only, deliberately -- see the file header on why cluster-monitoring-view
    # is not here. `view` covers ordinary namespaced reads a dashboard might touch;
    # the second Role is the one the tenancy port's kube-rbac-proxy ACTUALLY checks
    # (metrics.k8s.io/pods, verb `create` because Grafana's Prometheus datasource POSTs
    # and kube-rbac-proxy derives the verb from the HTTP method -- `view` grants only
    # get/list there, which is why it alone measured as insufficient).
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${WVA_GRAFANA_DS_SA}
  namespace: ${ns}
---
apiVersion: v1
kind: Secret
metadata:
  name: ${WVA_GRAFANA_DS_SA}-token
  namespace: ${ns}
  annotations:
    kubernetes.io/service-account.name: ${WVA_GRAFANA_DS_SA}
type: kubernetes.io/service-account-token
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${WVA_GRAFANA_DS_SA}-view
  namespace: ${ns}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: view
subjects:
- kind: ServiceAccount
  name: ${WVA_GRAFANA_DS_SA}
  namespace: ${ns}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ${WVA_GRAFANA_DS_SA}-thanos-tenancy
  namespace: ${ns}
rules:
- apiGroups: ["metrics.k8s.io"]
  resources: ["pods"]
  verbs: ["get", "list", "create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${WVA_GRAFANA_DS_SA}-thanos-tenancy
  namespace: ${ns}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ${WVA_GRAFANA_DS_SA}-thanos-tenancy
subjects:
- kind: ServiceAccount
  name: ${WVA_GRAFANA_DS_SA}
  namespace: ${ns}
EOF

    log_info "Applying the Grafana instance in $ns..."
    cat <<EOF | kubectl apply -f -
apiVersion: grafana.integreatly.org/v1beta1
kind: Grafana
metadata:
  name: ${WVA_GRAFANA_NAME}
  namespace: ${ns}
  labels:
    dashboards: ${WVA_GRAFANA_NAME}
spec:
  version: "12.1.1"
  config:
    log:
      mode: console
    auth:
      disable_login_form: "false"
  route:
    spec:
      # The SERVICE's port name (grafana), not the container's (grafana-http).
      # Naming the container port here still admits the Route, which then serves
      # 503 with no endpoint behind it -- looks exactly like Grafana failing to
      # start, and is not.
      port:
        targetPort: grafana
      tls:
        termination: edge
        insecureEdgeTerminationPolicy: Redirect
      to:
        kind: Service
        name: ${WVA_GRAFANA_NAME}-service
        weight: 100
EOF

    log_info "Applying the Thanos tenancy datasource in $ns..."
    # tlsSkipVerify, still -- an attempt to tighten this FAILED, verified rather
    # than assumed, and is worth recording so nobody re-tries it blind:
    #
    #   tried tlsAuthWithCACert: true + secureJsonData.tlsCACert, sourced first
    #   from the openshift-service-ca.crt ConfigMap every namespace gets
    #   auto-injected (the same CA WVA's own ServiceMonitor mounts, for this
    #   exact certificate), then from a Secret instead in case this operator
    #   version only resolves one valuesFrom source kind for secureJsonData.
    #   Both failed identically: "tls: failed to verify certificate: x509:
    #   certificate signed by unknown authority". The CA itself is right --
    #   `openssl verify -CAfile <that CA> <thanos-querier-tls's tls.crt>` = OK --
    #   and the request authenticates (rejected on TLS, not on auth), so this is
    #   not a wrong CA or a bad token. Restarted the Grafana pod to rule out a
    #   cached HTTP client from before the change; same error after a clean
    #   start. That restart surfaced something else worth knowing: this Grafana
    #   has NO PVC, so a restart wipes ALL datasources and dashboards until
    #   grafana-operator's next reconcile re-pushes them -- automatic, but not
    #   instant. Re-running `make dashboard` is the reliable way to confirm they
    #   came back; its apply is already idempotent for exactly this reason.
    #
    # Left open rather than guessed at further. tlsSkipVerify is what the
    # reference configuration on this cluster already does for this same
    # certificate, and it is what is proven to work end to end.
    cat <<EOF | kubectl apply -f -
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDatasource
metadata:
  name: thanos-tenancy
  namespace: ${ns}
spec:
  instanceSelector:
    matchLabels:
      dashboards: ${WVA_GRAFANA_NAME}
  valuesFrom:
    - targetPath: secureJsonData.httpHeaderValue1
      valueFrom:
        secretKeyRef:
          name: ${WVA_GRAFANA_DS_SA}-token
          key: token
  datasource:
    name: Thanos (${ns})
    type: prometheus
    access: proxy
    isDefault: true
    editable: true
    # Port 9092, the TENANCY port, not 9091 -- 9091 serves every namespace and
    # needs the cluster-scoped grant this design exists to avoid.
    url: https://thanos-querier.openshift-monitoring.svc.cluster.local:9092
    jsonData:
      httpHeaderName1: Authorization
      timeInterval: 15s
      # The tenancy port REJECTS a query with no namespace param (400), so this is
      # mandatory, not a filter for convenience.
      customQueryParameters: namespace=${ns}
      tlsSkipVerify: true
    secureJsonData:
      httpHeaderValue1: "Bearer \${token}"
EOF

    log_info "Publishing the operational dashboard ConfigMap into $ns..."
    # Reuses the existing installer function rather than duplicating its version
    # comparison and namespace-scoping logic. DASHBOARD_NS targets this namespace
    # directly, so it does not need the INSTALL_PHASE=prereqs workaround the
    # by-hand version needed (install_operational_dashboard is only reachable
    # through deploy_monitoring_stack there; called directly here, it just runs).
    DASHBOARD_NS="$ns" install_operational_dashboard

    log_info "Importing the dashboard into Grafana..."
    cat <<EOF | kubectl apply -f -
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDashboard
metadata:
  name: wva-operational
  namespace: ${ns}
spec:
  instanceSelector:
    matchLabels:
      dashboards: ${WVA_GRAFANA_NAME}
  resyncPeriod: 5m
  configMapRef:
    name: wva-operation-dashboard
    key: operational-dashboard.json
EOF
}

# wva_dashboard_report prints how to reach the dashboard: the direct URL and the
# password-retrieval command. Runs EVERY time -- on first creation and on every
# later re-run -- because these are the two things asked for on every invocation,
# not only when something changed.
wva_dashboard_report() {
    local ns="$1" host secret_name="${WVA_GRAFANA_NAME}-admin-credentials"

    echo ""
    echo "=========================================="
    echo " WVA dashboard: $ns"
    echo "=========================================="
    echo ""

    host=$(kubectl get route "${WVA_GRAFANA_NAME}-route" -n "$ns" \
        -o jsonpath='{.spec.host}' 2>/dev/null)
    if [ -n "$host" ]; then
        echo "  Dashboard: https://${host}/d/${WVA_GRAFANA_DASHBOARD_UID}/${WVA_GRAFANA_DASHBOARD_SLUG}?var-namespace=${ns}"
        echo "  Grafana:   https://${host}"
    else
        # Not fatal: the operator may still be reconciling the Route. Give the
        # port-forward fallback, which works even on a cluster with no Route
        # support, rather than a blank line where a URL should be.
        log_warning "  No Route yet for ${WVA_GRAFANA_NAME} in $ns (grafana-operator may still be starting it)."
        echo "  In the meantime:  kubectl port-forward -n $ns svc/${WVA_GRAFANA_NAME}-service 3000:3000"
        echo "  Then:             http://localhost:3000/d/${WVA_GRAFANA_DASHBOARD_UID}/${WVA_GRAFANA_DASHBOARD_SLUG}?var-namespace=${ns}"
    fi

    echo ""
    echo "  Username: admin"
    if kubectl get secret "$secret_name" -n "$ns" >/dev/null 2>&1; then
        echo "  Password: kubectl get secret $secret_name -n $ns -o jsonpath='{.data.GF_SECURITY_ADMIN_PASSWORD}' | base64 -d; echo"
    else
        log_warning "  Secret $secret_name not created yet (grafana-operator may still be starting it) -- re-run 'make dashboard' in a moment."
    fi
    echo ""
}

# wva_dashboard_require_openshift stops on any cluster that is not OpenShift.
#
# Everything below assumes one: the Grafana CR asks for a Route, and the
# datasource is thanos-querier in openshift-monitoring on the tenancy port. The
# CRD check above does not cover this -- grafana-operator installs happily on
# plain Kubernetes -- so without this the whole thing SUCCEEDS there and produces
# a Grafana whose every panel is empty: no Route (the report falls back to
# port-forward, so that part still reads fine), and a datasource pointing at a
# Service that does not exist. Nothing errors, and nothing says why.
#
# Refused rather than adapted. A vanilla-Kubernetes equivalent needs a different
# datasource (there is no tenancy port to enforce the namespace, so the isolation
# this design rests on would have to be re-established some other way) and an
# Ingress instead of a Route. That is a second design, not a flag.
wva_dashboard_require_openshift() {
    wva_is_openshift && return 0
    log_error "'make dashboard' needs OpenShift, and this cluster is not one.

It stands up a Grafana that reads Thanos through the openshift-monitoring tenancy
port (9092) and publishes itself through a Route -- neither exists on vanilla
Kubernetes, and grafana-operator installs there quite happily, so this would have
built a Grafana with no route and a datasource that can never resolve.

On plain Kubernetes, import deploy/grafana/operational-dashboard.json into your own
Grafana and point it at the Prometheus WVA already uses:
    make check-prereqs        # prints the Prometheus it detected"
}

# wva_dashboard_resolve_ns echoes the namespace this dashboard is FOR, and refuses
# a namespace with no model servers in it.
#
# The namespace matters twice, and both uses are silent when it is wrong. The
# datasource pins customQueryParameters=namespace=<ns>, which the tenancy port
# ENFORCES as a label matcher; and install_operational_dashboard pins the
# dashboard's `namespace` variable to WVA_NS when the scope reads `namespace`,
# with includeAll off.
#
# `make dashboard` cannot take the scope at its word: SCOPE defaults to `namespace`
# in the Makefile whatever the cluster actually runs, so after a CLUSTER-WIDE
# install this resolves to the controller's own namespace and pins the view to it.
# The dashboard compares that against exported_namespace -- the WORKLOAD's
# namespace, equal to the controller's only for a namespace-scoped install (see
# docs/reference/operations.md) -- so it can never match. The result is a
# correctly built, permanently empty dashboard.
#
# So ask the question that actually decides it: are the model servers here? That
# is the same count the preflight reports, on the same markers discovery uses.
wva_dashboard_resolve_ns() {
    local ns="${WVA_WATCH_NS:-$WVA_NS}" watched elsewhere

    [ "$(wva_serving_workload_count "$ns")" != 0 ] && { printf '%s' "$ns"; return 0; }

    # The deployed controller knows what it watches. A real name here means a
    # namespace-scoped install pointed elsewhere; the literal $(POD_NAMESPACE) is
    # the base default and means "its own", so it answers nothing.
    watched="$(kubectl get deploy -n "$WVA_NS" -l app.kubernetes.io/name=workload-variant-autoscaler         -o jsonpath='{.items[0].spec.template.spec.containers[0].env[?(@.name=="WVA_WATCH_NAMESPACE")].value}' 2>/dev/null || true)"
    case "$watched" in
        ''|'$(POD_NAMESPACE)'|"$ns") watched="" ;;
    esac
    if [ -n "$watched" ] && [ "$(wva_serving_workload_count "$watched")" != 0 ]; then
        log_info "No model servers in $ns; the controller there watches $watched, which has them. Building the dashboard for $watched."
        printf '%s' "$watched"
        return 0
    fi

    elsewhere="$(wva_dashboard_serving_namespaces | paste -sd' ' -)"
    log_error "No llm-d model servers in $ns, so a dashboard built for it would be empty.

The namespace is not decoration here: the datasource enforces it (the tenancy port
rejects a query without one) and the dashboard's namespace variable is pinned to it.
Point at the namespace running the models:

    make dashboard NAMESPACE=<namespace>
${elsewhere:+
Namespaces with llm-d model servers: ${elsewhere}}"
}

# wva_dashboard_serving_namespaces echoes every namespace holding model servers,
# for the message above. Best-effort by design: a namespace-scoped caller cannot
# list Deployments cluster-wide, and being unable to name alternatives is not a
# reason to withhold the rest of the error.
wva_dashboard_serving_namespaces() {
    kubectl get deployments -A -o json 2>/dev/null         | jq -r --argjson markers "$(so_serving_markers_json)" '
            [ .items[]?
              | select((.spec.template.metadata.labels // {}) | to_entries
                       | any((.key + "=" + (.value|tostring)) as $kv
                             | ($markers | index($kv)) != null))
              | .metadata.namespace ] | unique | .[]' 2>/dev/null || true
}

# wva_dashboard is the whole target: check, apply, report -- called by `make dashboard`.
wva_dashboard() {
    # Only install.sh sets this, and `make dashboard` does not go through it.
    # install_operational_dashboard reads it (via wva_choose_dashboard_ns and
    # wva_grafana_dashboard_search_ns) and DASHBOARD_NS covers the first, so the
    # gap is invisible until someone adds `set -u` or drops DASHBOARD_NS. On
    # OpenShift -- the only platform this target now accepts -- the answer is not
    # in doubt.
    : "${MONITORING_NAMESPACE:=openshift-user-workload-monitoring}"

    local ns
    wva_dashboard_require_openshift
    wva_dashboard_require_crds
    ns="$(wva_dashboard_resolve_ns)"
    wva_dashboard_apply "$ns"
    wva_dashboard_report "$ns"
}
