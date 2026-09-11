#!/usr/bin/env bash
#
# Workload-Variant-Autoscaler infrastructure bootstrap: optional WVA controller,
# Prometheus monitoring stack, and scaler backend (KEDA).
#
# For llm-d (gateway, EPP, ModelService), see the llm-d project guides at https://github.com/llm-d/llm-d.
# For EPP setup (all environments), run deploy/install-epp.sh after this script.
#
# Prerequisites:
# - kubectl and helm installed
# - Cluster credentials configured
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
WVA_PROJECT=${WVA_PROJECT:-$PWD}

# Namespaces
#
# NAMESPACE is llm-d's own variable: its guides say `export NAMESPACE=…` and pass
# `-n ${NAMESPACE}` to every command. This repo used to call the same thing
# LLMD_NS — a second name it invented for a namespace llm-d already named — which
# is accepted here for one release so nobody's scripts break on the rename.
if [ -n "${LLMD_NS:-}" ] && [ -z "${NAMESPACE:-}" ]; then
    NAMESPACE="$LLMD_NS"
    echo -e "\033[1;33m[WARNING]\033[0m LLMD_NS is deprecated and will be removed; use NAMESPACE (llm-d's own variable). Using NAMESPACE=$NAMESPACE" >&2
fi
# What the CALLER actually set, captured before the defaults below make that
# unknowable. wva_resolve_namespace uses these: `export NAMESPACE=…` has to reach
# every entry point — install, undeploy and check alike — and it cannot do that
# from the make targets, which is where this resolution used to live. It was in
# only 12 of them, so `NAMESPACE=x make undeploy-wva-on-k8s` uninstalled from the
# DEFAULT namespace while the install had gone to x.
WVA_NS_EXPLICIT=${WVA_NS:-}
NAMESPACE_EXPLICIT=${NAMESPACE:-}
export WVA_NS_EXPLICIT NAMESPACE_EXPLICIT
NAMESPACE=${NAMESPACE:-"llm-d-optimized-baseline"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"workload-variant-autoscaler-monitoring"}
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}
PROMETHEUS_SECRET_NS=${PROMETHEUS_SECRET_NS:-$MONITORING_NAMESPACE}

# WVA Configuration (required when DEPLOY_WVA=true)
WVA_IMAGE_REPO=${WVA_IMAGE_REPO:-"ghcr.io/llm-d/llm-d-workload-variant-autoscaler"}
WVA_IMAGE_TAG=${WVA_IMAGE_TAG:-"latest"}
WVA_IMAGE_PULL_POLICY=${WVA_IMAGE_PULL_POLICY:-"Always"}
# An existing docker-registry Secret in WVA_NS, for an image in a private
# registry. Named rather than created here: a script that built the secret would
# have to be handed a password, and this way the credential is created once by
# whoever owns it and never passes through an install run.
WVA_IMAGE_PULL_SECRET=${WVA_IMAGE_PULL_SECRET:-}
SKIP_TLS_VERIFY=${SKIP_TLS_VERIFY:-"false"}
WVA_LOG_LEVEL=${WVA_LOG_LEVEL:-"info"}
# Optional: multi-controller isolation (sets controller_instance on metrics / selectors when non-empty).

ENABLE_SCALE_TO_ZERO=${ENABLE_SCALE_TO_ZERO:-true}

# Prometheus Configuration
PROM_CA_CERT_PATH=${PROM_CA_CERT_PATH:-"/tmp/prometheus-ca.crt"}
PROMETHEUS_SECRET_NAME=${PROMETHEUS_SECRET_NAME:-"prometheus-web-tls"}

# Flags for deployment steps
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-true}
# Create the llm-d namespace. Set false when llm-d already runs somewhere else:
# an empty llm-d namespace looks like the place to deploy models, and WVA would
# not be watching it.
# Creating an llm-d namespace is OFF by default. It only ever produced an EMPTY
# one — deploy/install-epp.sh creates its own when it deploys EPP — and an empty
# llm-d namespace is worse than none: it looks like the place to deploy models,
# and WVA is not watching it. Turning it on is for the demo path that wants the
# namespace to exist before anything is put in it.
DEPLOY_LLMD_NS=${DEPLOY_LLMD_NS:-false}
DEPLOY_OPERATIONAL_DASHBOARD=${DEPLOY_OPERATIONAL_DASHBOARD:-true}
DEPLOY_ALERTING_RULES=${DEPLOY_ALERTING_RULES:-false}
DEPLOY_WVA=${DEPLOY_WVA:-true}
SKIP_CHECKS=${SKIP_CHECKS:-false}

# WVA_KUBE_CONTEXT pins this run to one cluster.
#
# Everything here otherwise resolves the cluster from the ambient kubeconfig, so
# an operator working across two of them has no way to say which one they mean
# except by changing their global current-context -- and a preflight that picks
# the wrong cluster still reports success while mutating it. That happened.
#
# Implemented as shims rather than by threading a flag through every call site:
# the libraries invoke kubectl and helm by name in a few hundred places, and a
# flag reaches only the ones somebody remembered to change. `command` prevents
# the shim recursing into itself. Shell functions are inherited by subshells, so
# $(...) and process substitutions get them too.
#
# KUBECONFIG needs no shim: kubectl and helm both read it already. Point it at a
# throwaway file whose current-context is the cluster you mean and this variable
# is unnecessary.
if [ -n "${WVA_KUBE_CONTEXT:-}" ]; then
    kubectl() { command kubectl --context "$WVA_KUBE_CONTEXT" "$@"; }
    helm()    { command helm --kube-context "$WVA_KUBE_CONTEXT" "$@"; }
fi

# Scaler backend: keda | none.
# - keda on kubernetes: expects cluster CRD unless KEDA_HELM_INSTALL=true (then this script installs Helm KEDA).
# - keda on openshift: platform-managed KEDA only (no Helm install from this script).
# - none: skip scaler install (cluster already provides external metrics).
SCALER_BACKEND=${SCALER_BACKEND:-keda}
KEDA_NAMESPACE=${KEDA_NAMESPACE:-keda-system}
# Pinned for reproducible Helm installs (used when deploy_keda actually runs helm upgrade).
KEDA_CHART_VERSION=${KEDA_CHART_VERSION:-2.19.0}
# On kubernetes: default false (cluster-managed KEDA); kind-emulator flows often set true or use cluster path.
KEDA_HELM_INSTALL=${KEDA_HELM_INSTALL:-false}

# LeaderWorkerSet. Set true when LWS tests run (e.g. full e2e suite). Defaults false so smoke and benchmarks skip it.
DEPLOY_LWS=${DEPLOY_LWS:-false}
LWS_NAMESPACE=${LWS_NAMESPACE:-"lws-system"}
LWS_CHART_VERSION=${LWS_CHART_VERSION:-"0.8.0"}

# Environment-related variables
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Detected when unset. OpenShift changes the monitoring wiring and the default
# scope, and getting it wrong is not subtle — the controller keeps the Kubernetes
# Prometheus URL and exits on a Prometheus it cannot reach. API discovery answers
# it, and any authenticated user may read that (unlike the platform namespaces a
# tenant cannot see).
if [ -z "${ENVIRONMENT:-}" ]     && [ -n "$(kubectl api-resources --api-group=route.openshift.io -o name 2>/dev/null)" ]; then
    ENVIRONMENT=openshift
fi
ENVIRONMENT=${ENVIRONMENT:-"kubernetes"}
COMPATIBLE_ENV_LIST=("kubernetes" "openshift" "kind-emulator")
NON_EMULATED_ENV_LIST=("kubernetes" "openshift")
REQUIRED_TOOLS=("kubectl" "helm" "git")
DEPLOY_LIB_DIR="$SCRIPT_DIR/lib"

PRODUCTION_ENV_LIST=("openshift")

# Shared deploy helpers
# shellcheck source=lib/verify.sh
source "$DEPLOY_LIB_DIR/verify.sh"
# shellcheck source=lib/common.sh
source "$DEPLOY_LIB_DIR/common.sh"
# shellcheck source=lib/constants.sh
source "$DEPLOY_LIB_DIR/constants.sh"
# shellcheck source=lib/wait_helpers.sh
source "$DEPLOY_LIB_DIR/wait_helpers.sh"
# shellcheck source=lib/cli.sh
source "$DEPLOY_LIB_DIR/cli.sh"
# shellcheck source=lib/prereqs.sh
source "$DEPLOY_LIB_DIR/prereqs.sh"
# shellcheck source=lib/infra_scaler_backend.sh
source "$DEPLOY_LIB_DIR/infra_scaler_backend.sh"
# shellcheck source=lib/scaler_runtime.sh
source "$DEPLOY_LIB_DIR/scaler_runtime.sh"
# shellcheck source=lib/limiter_policy.sh
source "$DEPLOY_LIB_DIR/limiter_policy.sh"
# shellcheck source=lib/infra_wva.sh
source "$DEPLOY_LIB_DIR/infra_wva.sh"
# shellcheck source=lib/infra_epp.sh
source "$DEPLOY_LIB_DIR/infra_epp.sh"
# shellcheck source=lib/infra_monitoring.sh
source "$DEPLOY_LIB_DIR/infra_monitoring.sh"
# shellcheck source=lib/single_install.sh
source "$DEPLOY_LIB_DIR/single_install.sh"
# shellcheck source=lib/scaledobject.sh
source "$DEPLOY_LIB_DIR/scaledobject.sh"
# shellcheck source=lib/cleanup.sh
source "$DEPLOY_LIB_DIR/cleanup.sh"
# shellcheck source=lib/install_core.sh
source "$DEPLOY_LIB_DIR/install_core.sh"

# Captured BEFORE the platform script defaults it. Those defaults name the
# Prometheus this installer would deploy, so once they have run there is no way
# left to tell "the user asked for this URL" from "nobody was asked" — and the
# second case is the one that should go looking for the cluster's own Prometheus.
PROMETHEUS_URL_EXPLICIT=${PROMETHEUS_URL:-}
export PROMETHEUS_URL_EXPLICIT

UNDEPLOY=${UNDEPLOY:-false}
# CHECK_ONLY runs the prerequisite check and exits. Same code path the install
# takes, so "make check-prereqs" passing and the install then failing on a
# prerequisite is not a state these two can reach.
CHECK_ONLY=${CHECK_ONLY:-false}
# Which half of the install to run: prereqs (cluster admin) | wva (namespace
# admin) | all. The default keeps the single-command install unchanged.
# Captured before the default, like WVA_NS above: "the caller asked for all
# phases" and "the caller said nothing" are different, and only the second one
# may be resolved by looking at the cluster.
INSTALL_PHASE_EXPLICIT=${INSTALL_PHASE:-}
export INSTALL_PHASE_EXPLICIT
INSTALL_PHASE=${INSTALL_PHASE:-all}
# Allow a second WVA alongside an existing one. Refused by default because both
# would allocate from the same pool of free GPUs without seeing each other's
# claims. See deploy/lib/single_install.sh.

# Default ScaledObjects. A ScaledObject is the REGISTRATION — WVA is only asked
# about workloads KEDA calls it about — so an install with none anywhere is a
# controller that never scales anything and looks healthy doing it.
# WVA_DEFAULT_SO_NS: a namespace, "wva", or "all" for every namespace holding llm-d model
# servers (cluster-scoped installs only). Defaults to NAMESPACE.
# false | plan (list and stop) | edit (list, $EDITOR, confirm) | true (apply all)
WVA_DEFAULT_SO=${WVA_DEFAULT_SO:-false}
# Unset means "what this install can reach": every namespace holding model servers
# when cluster-scoped, its own namespace when namespace-scoped. Defaulting it to
# NAMESPACE here would defeat that, and did — the scope-derived default then applied
# only to the make targets, so the same variable behaved differently depending on
# how you invoked it.
WVA_DEFAULT_SO_NS=${WVA_DEFAULT_SO_NS:-}
# An existing file is applied as-is, edits included, with no terminal needed.
WVA_DEFAULT_SO_PLAN=${WVA_DEFAULT_SO_PLAN:-}
# Your own ScaledObject template instead of the shipped one. See
# config/samples/keda/external-scaler/scaledobject-template.yaml.
WVA_DEFAULT_SO_TEMPLATE=${WVA_DEFAULT_SO_TEMPLATE:-}
WVA_DEFAULT_SO_MIN=${WVA_DEFAULT_SO_MIN:-1}
WVA_DEFAULT_SO_MAX=${WVA_DEFAULT_SO_MAX:-10}
DELETE_NAMESPACES=${DELETE_NAMESPACES:-false}
# Delete the llm-d namespace too. Separate and explicit: it holds the model servers,
# and an uninstall never restates the install flags, so this must never be inferred.
DELETE_LLMD_NS=${DELETE_LLMD_NS:-false}
# With UNDEPLOY=true, also remove Prometheus, the scaler backend and EPP. Off by
# default: those are shared, this install may not have created them, and removing
# them takes out every other thing on the cluster that uses them.
UNDEPLOY_SHARED=${UNDEPLOY_SHARED:-false}
# Controller replicas. Leader-elected, so extras are warm standbys for failover,
# not extra throughput — only the leader runs the optimization loops.
WVA_REPLICAS=${WVA_REPLICAS:-1}

# Orchestration lives in deploy/lib/install_core.sh (keeps this entrypoint to variable defaults + sourcing only).
main "$@"
