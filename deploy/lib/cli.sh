#!/usr/bin/env bash
#
# CLI help and argument parsing for deploy/install.sh.
# Requires vars: WVA_IMAGE_REPO, WVA_IMAGE_TAG, COMPATIBLE_ENV_LIST.
# Requires funcs: log_info/log_warning/log_error, containsElement().
#

print_help() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Bootstrap WVA (optional), monitoring, and scaler backend on a Kubernetes or OpenShift cluster.
For llm-d (gateway, EPP, ModelService), see the llm-d project's installation guides.

Options:
  -i, --wva-image IMAGE        Container image for WVA (default: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG)
  -c, --check                  Run the prerequisite check and exit without deploying
  -p, --phase PHASE            prereqs | wva | all (default: all)
                                 prereqs  what a CLUSTER ADMIN does once per namespace:
                                          the namespace, the cluster-scoped RBAC, the
                                          ServiceMonitor, and the shared infrastructure
                                          (Prometheus, KEDA) if the cluster has none
                                 wva      the controller only — no cluster-scoped rights
                                          needed once prereqs are in place
                                 all      both, in order
  -u, --undeploy               Undeploy WVA, monitoring, and scaler backend
  -e, --environment            kubernetes | openshift | kind-emulator (default: kubernetes)
  -h, --help                   Show this help and exit

Deprecated (ignored by install.sh; Helm chart removed):
  --release-name NAME          Helm release name (no-op; WVA now installed via Kustomize)
  --accelerator TYPE           Same as ACCELERATOR_TYPE env

Environment Variables:

 The install
  IMG                          WVA image as repo:tag (alternative to -i)
  WVA_NS                       Namespace to install the controller into (default: workload-variant-autoscaler-system)
  WVA_SCOPE                    cluster | namespace (default: cluster, or namespace on openshift)
                                 cluster   — manages every namespace, creates cluster-scoped RBAC,
                                             needs a cluster admin
                                 namespace — manages ONE namespace. Its cluster-scoped RBAC (metrics
                                             authn, EPP metrics, the node read gpu-inventory needs)
                                             is created once by an admin with --phase prereqs; the
                                             namespace's owner then installs the controller, and
                                             every later upgrade, with no cluster rights.
  WVA_WATCH_NS                 Namespace a namespace-scoped controller MANAGES, when that differs from
                               the one it runs in (default: its own)
  WVA_REPLICAS                 Controller replicas. >1 is leader-election failover, NOT more throughput (default: 1)
  DEPLOY_WVA                   Deploy WVA controller (default: true)
  SKIP_CHECKS                  Skip the prerequisite and permission checks (default: false)
  WVA_KUBE_CONTEXT             Pin every kubectl/helm call to this context, without
                               changing your global current-context (default: unset)

 Scaling behaviour
  WVA_LIMITER                  none (default) | gpu-inventory | quota. none means scaling is UNBOUNDED.
                               gpu-inventory requires the controller to be able to list nodes.
  ENABLE_SCALE_TO_ZERO         Allow parking idle models at 0 replicas (default: true)
  WVA_DEFAULT_SO               Create default ScaledObjects for what is already running:
                               false (default) | plan (print and stop) | edit (plan, \$EDITOR, apply) | true (apply all)
  WVA_DEFAULT_SO_PLAN          Plan file. If it exists, apply exactly it and skip discovery.
  WVA_DEFAULT_SO_NS            A namespace, "wva" for WVA's own, or "all" (default: derived from WVA_SCOPE)

 Monitoring
  DEPLOY_PROMETHEUS            Deploy the Prometheus stack (default: true). Skipped automatically when the
                               cluster already has a Prometheus outside MONITORING_NAMESPACE.
  PROMETHEUS_FORCE_INSTALL     Install even then (default: false). Two operators will contend over the same CRs.
  PROMETHEUS_URL               Where WVA reads metrics from. Required when Prometheus is not deployed here.
  PROMETHEUS_TLS_INSECURE_SKIP_VERIFY  Skip Prometheus certificate verification (default: false)
  DEPLOY_OPERATIONAL_DASHBOARD Publish the WVA Grafana dashboard (default: true). Works against an
                               existing Grafana — it is a sidecar-labelled ConfigMap.
  DASHBOARD_NS                 Namespace to publish the dashboard into (default: MONITORING_NAMESPACE)
  MONITORING_NAMESPACE         Namespace for the monitoring stack

 Warm pool
  WARMPOOL_PROXY_IMG           Override the pool proxy image. Optional: defaults to the published
                               one config/warmpool pins. Build your own with
                               make docker-build-warmpool-proxy docker-push-warmpool-proxy
  WARMPOOL_RUNTIME_CLASS       RuntimeClass for pool Pods (default: nvidia-legacy), or "none".
                               Cluster-specific: a name the cluster does not have fails admission,
                               and omitting one it needs gives containers with no GPU
                               (MONITORING_NAMESPACE above is reused to scrape lent pool Pods.
                               Without a scrape, a model reads as having LESS demand than it has
                               while a pool Pod is covering for it.)
                               Pools are only created for plan entries marked apply: yes; see
                               docs/guides/warm-pool/README.md

 Scaler backend and CRDs
  SCALER_BACKEND               keda (default) or none. WVA needs KEDA to actuate.
  KEDA_HELM_INSTALL            Install KEDA via Helm on kubernetes (default: false — assumes cluster KEDA)
  KEDA_NAMESPACE               Namespace for KEDA (default: keda-system)
  CRD_INSTALL                  if-missing (default) | always | never. Gateway API and GAIE CRDs are
                               cluster-scoped and shared; 'always' overwrites what other controllers use.
  DEPLOY_LWS                   Install the LeaderWorkerSet CRD if absent (default: false)

 Undeploy
  UNDEPLOY                     Undeploy mode (default: false)
  UNDEPLOY_SHARED              Also remove Prometheus, KEDA and EPP (default: false — they are shared
                               and this install may not have created them)
  DELETE_NAMESPACES            Delete WVA and monitoring namespaces afterwards (default: false)
  DELETE_LLMD_NS               Also delete NAMESPACE — it holds the model servers (default: false)

 Other
  NAMESPACE                      Namespace used for EPP/model-server setup by the e2e and sample paths.
                               It does NOT scope what WVA manages: WVA has no watch and no listing, and
                               learns of a workload only when KEDA calls it about one.

Examples:
  # New cluster, everything
  $(basename "$0")

  # Existing llm-d cluster: use its Prometheus and KEDA, touch neither
  PROMETHEUS_URL=https://prom.monitoring.svc:9090 DEPLOY_PROMETHEUS=false \\
    SCALER_BACKEND=keda CRD_INSTALL=never $(basename "$0")

  IMG=registry.example.com/wva:dev $(basename "$0") -e kind-emulator

  $(basename "$0") -e openshift
EOF
}

parse_args() {
  if [[ -n "$IMG" ]]; then
    log_info "Detected IMG environment variable: $IMG"
    if [[ "$IMG" == *":"* ]]; then
      IFS=':' read -r WVA_IMAGE_REPO WVA_IMAGE_TAG <<< "$IMG"
    else
      log_warning "IMG has wrong format, using default image"
    fi
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      -i|--wva-image)
        if [[ "$2" == *":"* ]]; then
          IFS=':' read -r WVA_IMAGE_REPO WVA_IMAGE_TAG <<< "$2"
        else
          WVA_IMAGE_REPO="$2"
        fi
        shift 2
        ;;
      -c|--check)             CHECK_ONLY=true; shift ;;
      -p|--phase)
        INSTALL_PHASE="$2" ; shift 2
        case "$INSTALL_PHASE" in
          prereqs|wva|all) ;;
          *) log_error "Invalid --phase: $INSTALL_PHASE. Valid options are: prereqs, wva, all" ;;
        esac
        ;;
      -u|--undeploy)          UNDEPLOY=true; shift ;;
      -e|--environment)
        ENVIRONMENT="$2" ; shift 2
        if ! containsElement "$ENVIRONMENT" "${COMPATIBLE_ENV_LIST[@]}"; then
          log_error "Invalid environment: $ENVIRONMENT. Valid options are: ${COMPATIBLE_ENV_LIST[*]}"
        fi
        ;;
      --accelerator)
        export ACCELERATOR_TYPE="$2"
        shift 2
        ;;
      --release-name)
        # Legacy CI/Helm — install.sh no longer installs via Helm; value is ignored.
        shift 2
        ;;
      -h|--help)              print_help; exit 0 ;;
      *)
        echo "Error: Unknown option: $1" >&2
        print_help
        exit 1
        ;;
    esac
  done
}
