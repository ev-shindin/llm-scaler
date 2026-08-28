# Image URL to use all building/pushing image targets
# This branch's manifests pass --external-scaler-bind-address, a flag no
# RELEASED image has: ghcr.io/llm-d/...:latest crash-loops on it with "unknown
# flag", and the install has nothing to run. The default therefore points at a
# build of THIS tree. Change it back when the external scaler ships upstream.
IMAGE_TAG_BASE ?= ghcr.io/ev-shindin
# `main`, built by ci-main-image on every push to main -- reproducible from a
# commit and multi-arch, unlike a tag pushed by hand from a laptop. Rebuilt
# automatically, so it cannot silently go stale the way a personal tag does.
IMG_TAG ?= main
IMG ?= $(IMAGE_TAG_BASE)/llm-scaler:$(IMG_TAG)
# The warm-pool proxy ships as its own image because it runs in the pool's Pod,
# not the controller's. Pin a digest in the manifest; this tag is for building.
WARMPOOL_PROXY_IMG ?= $(IMAGE_TAG_BASE)/warmpool-proxy:$(IMG_TAG)
KIND_ARGS ?= -t mix -n 3 -g 2   # Default: 3 nodes, 2 GPUs per node, mixed vendors
CLUSTER_GPU_TYPE ?= nvidia-mix
CLUSTER_NODES ?= 3
CLUSTER_GPUS ?= 4
KUBECONFIG ?= $(HOME)/.kube/config
K8S_VERSION ?= v1.32.0

# Install namespace. Any namespace works, at either scope: the install applies a
# Kustomize namespace transform and the controller reads its own namespace from
# POD_NAMESPACE, so ClusterRoleBinding subjects and the system-namespace lookups
# follow.
WVA_NS              ?= workload-variant-autoscaler-system
# Where llm-d runs. NAMESPACE is llm-d's OWN variable — its well-lit path guides
# say `export NAMESPACE=…` and pass `-n ${NAMESPACE}` to every command — so a
# reader arriving from one already has it set, and this repo no longer invents a
# second name for it. It used to: LLMD_NS, ours alone, now a deprecated alias
# accepted by deploy/install.sh.
NAMESPACE           ?= llm-d-optimized-baseline
# Install scope: cluster | namespace. Empty means the historical default —
# namespace on OpenShift, cluster elsewhere. See deploy/lib/common.sh.
WVA_SCOPE           ?=
# Declare a GPU limiter at install: none | gpu-inventory | quota. Default none,
# matching the shipped config — see deploy/README.md "Bounding scaling".
WVA_LIMITER         ?= none
# A ScaledObject is how a workload registers with WVA. WVA_DEFAULT_SO=true has the
# installer create one per llm-d model server; WVA_DEFAULT_SO_NS picks the
# namespace, or "all" for every namespace holding one (cluster-scoped installs).
WVA_DEFAULT_SO      ?= false
WVA_DEFAULT_SO_NS   ?=
# Where the e2e controller runs -- and it is the namespace the models are in.
#
# A NAMESPACE-SCOPED controller manages the namespace it lives in, so the suite's
# controller and its model servers belong together. They were split
# (workload-variant-autoscaler-system vs llm-d-sim), which only works cluster-
# scoped: kind used to infer that scope, namespace is now the default everywhere,
# and the split install could then neither watch nor read the workloads --
#   scaledobjects.keda.sh "stz-rt-so" is forbidden
# so an idle model was never evaluated and never parked.
#
# Aligned rather than given cluster scope, or split further with WVA_WATCH_NS:
# namespace scope is the supported shape, cluster scope now warns that it is WIP,
# and the suite should exercise what people actually run. One variable feeds both
# the install (WVA_NS) and what the tests read (WVA_NAMESPACE), so they cannot
# drift apart.
CONTROLLER_NAMESPACE ?= $(E2E_EMULATED_LLMD_NAMESPACE)
MONITORING_NAMESPACE ?= openshift-user-workload-monitoring
LLMD_NAMESPACE       ?= llm-d-optimized-baseline
GATEWAY_NAME         ?= # discovered automatically in e2es
MODEL_ID             ?= e2ewva/dummy-model
DEPLOYMENT           ?= # discovered automatically in e2es
REQUEST_RATE         ?= 10
# How long a workload drives load for, substituted into __MAX_DURATION__ the
# same way REQUEST_RATE is. A profile that hardcodes its seconds is unaffected.
MAX_DURATION         ?= 600
NUM_PROMPTS          ?= 3000

# E2E test configuration (for test/e2e/ suite)
ENVIRONMENT                 ?= kind-emulator
USE_SIMULATOR               ?= true
SCALE_TO_ZERO_ENABLED       ?= false
# Sized for the DEFAULT run, which skips ~46 specs at runtime. Enabling the
# gates (SCALE_TO_ZERO_ENABLED=true) adds inherently slow scale-from-zero
# specs and 35m is not enough -- the suite panics with "test timed out"
# mid-run, which looks like a hang rather than a budget.
E2E_TIMEOUT                 ?= $(if $(filter true,$(SCALE_TO_ZERO_ENABLED)),120m,35m)
# go test's timeout must be LARGER than ginkgo's, not equal to it.
#
# Ginkgo's own suite timeout defaults to one hour, so raising only go test's
# limit bought nothing: a SCALE_TO_ZERO_ENABLED=true run died at exactly 3600s
# with `FAIL! - Suite Timeout Elapsed`, 30 of 96 specs run.
#
# Setting them EQUAL was worse. go test won the race and panicked the binary, so
# ginkgo never printed a summary at all -- 75 minutes of work and the only output
# was a goroutine dump and `FAIL ... 4500.036s`. A run you cannot read is worth
# less than one that stops early and says where it got to.
#
# So ginkgo gets E2E_TIMEOUT and go test gets a margin on top: ginkgo hits its
# limit first, tears down cleanly, and reports which specs passed.
#
# 120m from measurement, not taste: the 75m run completed 56 specs in 4432s and
# was still going, with individual scale-from-zero specs taking 720s, 605s and
# 600s. Those are inherently slow -- they wait on real parking and waking.
E2E_GO_TIMEOUT              ?= $(if $(filter true,$(SCALE_TO_ZERO_ENABLED)),130m,45m)
# Passed to BOTH `go test -timeout` and `-ginkgo.timeout`. Ginkgo keeps its own
# suite timeout, which defaults to ONE HOUR, so raising only go test's limit
# bought nothing: a SCALE_TO_ZERO_ENABLED=true run died at exactly 3600s with
# `FAIL! - Suite Timeout Elapsed`, 30 of 96 specs run and 66 never started --
# which reads as eight product failures rather than a suite that was cut off.
DEPLOY_ALERTING_RULES       ?= false
SCALER_BACKEND              ?= keda  # keda (ScaledObject) or none (skip, use pre-installed backend)
LLM_D_ROUTER_VERSION        ?= v0.9.0
GAIE_VERSION                ?= v1.5.0
SCALE_UP_THRESHOLD          ?=
SCALE_DOWN_BOUNDARY         ?=
E2E_MONITORING_NAMESPACE    ?= workload-variant-autoscaler-monitoring
E2E_EMULATED_LLMD_NAMESPACE ?= llm-d-sim
E2E_KEDA_NAMESPACE          ?= keda-system
E2E_WVA_SECONDARY_OVERLAY_PATH ?= $(CURDIR)/test/e2e/testdata/secondary-controller
# llm-d-benchmark CLI configuration
# Ensure brew-installed tools (helm >=3.19) take precedence over Rancher Desktop.
#
# Scoped to the benchmark targets, which is what it was written for. It used to
# be a plain `export PATH :=`, which applies to EVERY target -- so it also
# outranked whatever the caller had put first. An operator working across two
# clusters put a kubectl wrapper pinned to one of them at the front of PATH; this
# line silently won, and `make check-prereqs` ran against the other cluster and
# reapplied prerequisites there. A successful-looking preflight for the wrong
# cluster is the worst possible failure mode for a command that mutates one.
benchmark-%: export PATH := /opt/homebrew/bin:$(PATH)
BENCHMARK_REPO_URL   ?= https://github.com/llm-d/llm-d-benchmark.git
BENCHMARK_REPO_DIR   ?= $(CURDIR)/llm-d-benchmark
BENCHMARK_DIRECT_KEDA ?= false
# v0.7.8, not v0.7.0: guidellm moved to a nested profile schema
# (spec.backend.kind, constraints[], data[]) and upstream converted its profiles
# to match in 00e1516 "[Run] Update guidellm profiles (compatibility with
# v0.7.3)". v0.7.0's profiles are still flat while the harness IMAGE it pulls is
# v0.7.3, so every guidellm run from a v0.7.0 checkout dies with
#   Error: Invalid value for '--backend' ...: Field required (at
#   'backend.openai_http.target'); Extra inputs are not permitted (at 'target')
# -- including on llm-d-benchmark's own shipped profiles.
BENCHMARK_REPO_REF   ?= $(if $(filter true,$(BENCHMARK_DIRECT_KEDA)),main,v0.7.8)
BENCHMARK_SPEC       ?= $(if $(filter true,$(BENCHMARK_DIRECT_KEDA)),guides/epp-keda-saturation,guides/workload-autoscaling)
BENCHMARK_NAMESPACE  ?= # set via BENCHMARK_NAMESPACE=<namespace>
BENCHMARK_GATEWAY_URL ?= http://infra-llmdbench-inference-gateway-istio.$(BENCHMARK_NAMESPACE).svc.cluster.local:80
BENCHMARK_WORKSPACE  ?= $(CURDIR)
BENCHMARK_HARNESS    ?= guidellm
BENCHMARK_WORKLOAD   ?= prefill_heavy
BENCHMARK_FORCE      ?= true
BENCHMARK_MONITORING ?= true
BENCHMARK_UV         ?= false
BENCHMARK_SCENARIOS_DIR ?= $(CURDIR)/test/benchmark/scenarios
# The model WVA is benchmarked against, forwarded to the benchmark CLI as `-m`,
# which OVERRIDES the model the scenario names.
#
# Qwen3-0.6B by default, deliberately: what these runs measure is WVA's scaling
# behaviour, and that exercises the same path — discovery, the ScaledObject
# plan, the scale decision, the report — whatever the model's size. The
# scenario's own Qwen/Qwen3-32B pulls a far larger image and holds several GPUs
# per replica for the whole run, which is a poor default on a shared cluster.
# Pass MODEL_ID=Qwen/Qwen3-32B for the heavy run, or BENCHMARK_MODEL_ID= (empty)
# to defer to whatever the scenario names.
#
# It must NOT fall back to MODEL_ID's own default, e2ewva/dummy-model: that
# model exists solely in the kind emulator, and defaulting to it meant the exact
# command the benchmarking guide documents (BENCHMARK_NAMESPACE and IMG, no
# MODEL_ID) benchmarked the emulator's dummy on a real GPU cluster. The
# kind-emulator targets pass MODEL_ID= into their sub-makes, so origin reads
# "command line" there and they keep the dummy.
BENCHMARK_MODEL_ID   ?= $(if $(filter command line environment,$(origin MODEL_ID)),$(MODEL_ID),Qwen/Qwen3-0.6B)

# The fraction of each GPU vLLM may use, substituted into the scenario.
#
# 0.90, not the scenarios' 0.95 and not a small-model special case.
#
# 0.95 is knife-edge on a SHARED card and was measured failing: gpu_memory_
# utilization is a fraction of TOTAL, not of free, so vLLM budgets 75.2 of 79.18
# GiB and leaves 4 GiB -- less than the memory an FMA launcher was already
# holding on that node. Launchers request ZERO GPUs while running the engine, so
# the scheduler cannot see that memory and places the model there anyway. Result:
# `torch.OutOfMemoryError: tried to allocate 594MiB, 500MiB free`, and because it
# only happens when a co-tenant is present it reads as flakiness. There are 23
# such launcher pods on this cluster.
#
# 0.90 leaves 7.9 GiB, which absorbs that and more, and still gives a replica
# 651k tokens of KV -- 41 requests at the full 16k context.
#
# Deliberately NOT lowered for small models, though that is what first stopped the
# crash. Per-replica capacity IS the KV cache size (saturation_v2/analyzer.go:174,
# k1 = TotalKvCapacityTokens x KvCacheThreshold), so shrinking KV to make a
# benchmark scale sooner is doing the autoscaler's job in the wrong layer: it
# throws away GPU you paid for to get an effect kvCacheThreshold produces for
# free. Size the card for serving; tune when a replica counts as full in the
# scaling policy.
GPU_MEM_UTIL         ?= 0.90
BENCHMARK_DECODE_REPLICAS ?= 1
BENCHMARK_KEDA_MIN_REPLICAS ?= 1
BENCHMARK_KEDA_MAX_REPLICAS ?= 10
# How often each scaling POLICY may act. This is a rate-limit window, not a
# delay: the scenario's policy is "Percent 100", so 5s permits doubling every
# 5s. It cannot make scaling faster than the HPA control loop, which
# re-evaluates on kube-controller-manager's sync period (15s by default) --
# below that, a smaller number changes nothing.
# The UID the benchmark harness pod runs as. 0 because its entrypoint writes to
# /usr/local/bin; see the note beside the yq call in benchmark-standup.
BENCHMARK_HARNESS_RUN_AS_USER ?= 0

BENCHMARK_KEDA_SCALE_UP_PERIOD ?= 5
BENCHMARK_KEDA_SCALE_DOWN_PERIOD ?= 120

# The stabilization window is what actually delays a scale-up: HPA takes the
# most conservative recommendation across it, so load must persist this long
# before replicas are added. The scenario ships 120s, which dominates every
# other latency in the loop -- KEDA polls every 5s, WVA recomputes every 15s,
# and a decode replica measured 61s to Ready on pokprod001. Benchmarking WVA's
# reaction with a 120s window measures the window.
#
# 0 is a VALID value here (unlike periodSeconds), meaning "act on the current
# recommendation". Empty means "leave the scenario's own value alone".
# Scale-down carries the conservatism, and it belongs in the STABILIZATION
# window rather than the policy period: HPA takes the maximum recommendation
# across this window, so 300s is what makes a replica survive a momentary dip.
# The period is only a rate limit, and with "Percent 100" a single step may
# remove everything anyway -- a long period there looks cautious and prevents
# almost nothing, which is why these two are the way round they are.
#
# The asymmetry is deliberate: removing a replica too eagerly costs a 61s cold
# start (measured on pokprod001) on the next request, while keeping one too
# long only costs money.
BENCHMARK_KEDA_SCALE_UP_STABILIZATION ?= 0
BENCHMARK_KEDA_SCALE_DOWN_STABILIZATION ?= 300
# WVA under benchmark is installed from THIS repo, after standup, into the
# benchmark namespace at namespace scope. It used to come from the published
# Helm chart named in the scenario — a released binary, not the code under test,
# and a chart this repo no longer has. BENCHMARK_WVA_DEPLOY=false runs the
# scenario with no autoscaler; direct-KEDA mode skips it on its own.
BENCHMARK_WVA_DEPLOY ?= true
# Never install prometheus-adapter: it and KEDA both register the
# external.metrics.k8s.io APIService, of which a cluster has exactly one, so it
# would take that group away from the metrics server every ScaledObject's HPA
# queries. It was there for the HPA path the external scaler replaced.
BENCHMARK_SKIP_PROMETHEUS_ADAPTER ?= true

# Standing a guide up over an EPP that is already serving replaces objects the
# running stack depends on, and the damage is silent -- see benchmark-standup.
# Set true to proceed anyway (re-rendering the same guide, or accepting it).
BENCHMARK_ALLOW_EPP_REUSE ?= false
# Where benchmark-deploy-wva writes its ScaledObject plan. A named file, not a
# temp path, so a run that scaled something unexpected can be explained after it.
BENCHMARK_SO_PLAN ?= $(CURDIR)/benchmark-scaledobject-plan.yaml
# Which install target the benchmark reuses. ASKED OF THE CLUSTER, not inferred
# from ENVIRONMENT, which defaults to kind-emulator and is left unset by anyone
# running a benchmark against a real cluster.
#
# Inferring it picked deploy-wva-on-k8s on an OpenShift cluster, and the
# Kubernetes overlay omits config/components/openshift/manager-cluster-monitoring
# -view-clusterrolebinding.yaml. The controller then came up without
# cluster-monitoring-view and crash-looped on
#   Prometheus API validation failed ... "error": "client_error: client error: 403"
# against thanos-querier:9091 -- a failure that says nothing about the overlay
# that caused it. Evaluated inside the recipe so it costs one API call when a
# benchmark runs, not on every make invocation.
BENCHMARK_WVA_TARGET = $(if $(filter openshift,$(ENVIRONMENT)),deploy-wva-on-openshift,deploy-wva-on-k8s)
BENCHMARK_WVA_UNDEPLOY_TARGET = $(if $(filter openshift,$(ENVIRONMENT)),undeploy-wva-on-openshift,undeploy-wva-on-k8s)
# Where the installed WVA reads metrics. Empty lets deploy/install.sh detect the
# cluster's existing Prometheus, which is the usual case for a benchmark cluster.
BENCHMARK_PROMETHEUS_URL ?= $(PROMETHEUS_URL)

# Flags for deploy/install.sh (e2e / CI-style cluster infra; no chart VA/HPA).
CREATE_CLUSTER    ?= false
DELETE_CLUSTER    ?= false
DELETE_NAMESPACES ?= false


# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
#
# Guarded on go being installed. This block is evaluated for EVERY target, so
# without the guard a machine with no Go on PATH prints
# "make: go: No such file or directory" three times before running a target that
# has nothing to do with Go — which is what the benchmark post-processing targets
# did, making a plotting problem look like a broken toolchain.
ifneq (,$(shell command -v go 2>/dev/null))
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration and ClusterRole objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role webhook paths="./..." \
		output:rbac:artifacts:config=config/base/rbac
	# controller-gen writes `role.yaml`; rename to match the
	# (<app>-)?<kind>.yaml convention used under config/.
	mv config/base/rbac/role.yaml config/base/rbac/manager-clusterrole.yaml

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: dashboards-check
dashboards-check: ## Fail if a Grafana dashboard has overlapping panels, duplicate ids/titles or the wrong namespace label (CI).
	python3 hack/check-dashboards.py

.PHONY: test
test: manifests generate fmt vet setup-envtest helm ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" PATH="$(LOCALBIN):$(PATH)" go test $$(go list ./... | grep -v /e2e | grep -v /benchmark) -coverprofile cover.out

# Creates a multi-node Kind cluster
# Adds emulated GPU labels and capacities per node
.PHONY: create-kind-cluster
create-kind-cluster:
	export KIND=$(KIND) KUBECTL=$(KUBECTL) && \
		deploy/kind-emulator/setup.sh -t $(CLUSTER_GPU_TYPE) -n $(CLUSTER_NODES) -g $(CLUSTER_GPUS)

# Destroys the Kind cluster created by `create-kind-cluster`
.PHONY: destroy-kind-cluster
destroy-kind-cluster:
	export KIND=$(KIND) KUBECTL=$(KUBECTL) && \
        deploy/kind-emulator/teardown.sh


##@ Guides

.PHONY: guides-render
guides-render: ## Regenerate the command blocks in docs/guides/*/README.md from their guide.yaml.
	python3 hack/render-guides.py

.PHONY: guides-check
guides-check: ## Fail if any guide README is out of date with its guide.yaml, or a doc link is broken (CI).
	python3 hack/render-guides.py --check
	python3 hack/check-doc-links.py

.PHONY: warmpool-render
warmpool-render: ## Regenerate the pool Pod spec in deploy/warmpool.sh from config/warmpool.
	python3 hack/render-warmpool-spec.py

.PHONY: warmpool-check
warmpool-check: ## Fail if deploy/warmpool.sh's Pod spec is out of date with config/warmpool (CI).
	python3 hack/render-warmpool-spec.py --check

.PHONY: docs-links-external
docs-links-external: ## Resolve every external doc link over the network (not in CI: needs egress).
	python3 hack/check-doc-links.py --external

##@ Cluster-admin actions (advanced; they affect every WVA on the cluster)

# Where cluster policy is published. The default is the name the controller looks
# for, and a tenant cannot point their WVA at a more permissive one.
WVA_POLICY_NS       ?= wva-policy
# gpu-inventory (bounded by GPUs actually free) or quota (bounded by declared caps).
WVA_LIMITER_TYPE    ?= gpu-inventory
# Namespaces whose controllers to grant. Empty = every controller on the cluster,
# which is what you want: the hazard is the install you forgot.
WVA_LIMITER_TARGETS ?=

.PHONY: enable-physical-limiter
enable-physical-limiter: ## CLUSTER ADMIN: bound every WVA by real GPUs. Publishes cluster policy and grants each controller the node read it then requires. WVA_LIMITER_TYPE=gpu-inventory|quota, WVA_POLICY_NS, WVA_LIMITER_TARGETS.
	@WVA_POLICY_NS=$(WVA_POLICY_NS) WVA_LIMITER_TYPE=$(WVA_LIMITER_TYPE) \
		WVA_LIMITER_TARGETS="$(WVA_LIMITER_TARGETS)" \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/physical_limiter.sh; enable_physical_limiter'

.PHONY: disable-physical-limiter
disable-physical-limiter: ## CLUSTER ADMIN: remove the limiter from cluster policy. Scaling becomes unbounded for every WVA that reads it.
	@WVA_POLICY_NS=$(WVA_POLICY_NS) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/physical_limiter.sh; disable_physical_limiter'

##@ Install, in three phases

# SCOPE picks what the controller manages. NAMESPACE scope is the default because
# it is the common case — one team, one namespace, and the only shape a namespace
# admin can own. `SCOPE=cluster` gives one controller for every namespace.
#
# Only the make targets default this way; deploy/install.sh keeps its historical
# platform default for anything calling it directly (the e2e does).
SCOPE ?= $(if $(WVA_SCOPE),$(WVA_SCOPE),namespace)

# The phases split where the PERMISSIONS split, so a namespace admin can own the
# controller without holding cluster-scoped rights:
#
#   1. check-prereqs                 read-only. Renders the overlay this install
#                                    would apply, asks whether you may create each
#                                    kind in it, and reports the namespace and
#                                    Prometheus it resolved.
#   2. setup-prereqs                 CLUSTER ADMIN, once per namespace: the
#                                    namespace, cluster-scoped RBAC, the
#                                    ServiceMonitor, and Prometheus / KEDA if the
#                                    cluster has none.
#
# Phases 1 and 2 take the platform as ENVIRONMENT=kubernetes|openshift; only
# phase 3 also has -on-k8s / -on-openshift spellings. This block used to write
# all three as <phase>-on-<platform>, naming two targets that do not exist.
#   3. deploy-wva-on-<platform>      everything (the default). Set
#                                    INSTALL_PHASE=wva for the controller alone,
#                                    which is what a namespace owner runs once an
#                                    admin has done phase 2.
#
# The namespace comes from NAMESPACE — llm-d's own variable — or is found on the
# cluster. See deploy/README.md.
#
# wva_phase: $(1)=phase $(2)=ENVIRONMENT
define wva_phase
	@echo "Phase '$(if $(1),$(1),auto)', $(SCOPE)-scoped$(if $(2), on $(2),)"
	$(if $(filter prereqs,$(1)),,@echo "Image: $(IMG)")
	$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) IMG=$(IMG) WVA_SCOPE=$(SCOPE) WVA_LIMITER=$(WVA_LIMITER) $(if $(1),INSTALL_PHASE=$(1),) $(if $(2),ENVIRONMENT=$(2),) WVA_DEFAULT_SO=$(WVA_DEFAULT_SO) $(if $(WVA_DEFAULT_SO_NS),WVA_DEFAULT_SO_NS=$(WVA_DEFAULT_SO_NS),) $(if $(PROMETHEUS_URL),PROMETHEUS_URL=$(PROMETHEUS_URL),) ./deploy/install.sh
endef

# wva_check: $(1)=ENVIRONMENT
define wva_check
	$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) WVA_LIMITER=$(WVA_LIMITER) $(if $(1),ENVIRONMENT=$(1),) ./deploy/install.sh --check
endef

##@ Install and remove

# INSTALL_PHASE=all is the default: one command does prerequisites and controller,
# which is right when you are a cluster admin installing for yourself. A namespace
# owner whose admin has already run setup-prereqs uses INSTALL_PHASE=wva, which
# needs no cluster-scoped rights.
INSTALL_PHASE ?= all
# Empty unless the caller named a phase. An empty phase lets the install work out
# which half is left to do — see wva_missing_prereqs — instead of the Makefile's
# own default silently answering a question the cluster can answer.
INSTALL_PHASE_ARG = $(if $(filter command line environment,$(origin INSTALL_PHASE)),$(INSTALL_PHASE),)

# ENVIRONMENT picks the platform, the way llm-d's guides do it: a variable with
# declared values feeding the path, rather than a target per platform
# (INFRA_PROVIDER: {default: base, values: [base, gke]} is their equivalent).
# The -on-k8s / -on-openshift targets below are thin aliases kept because CI, the
# benchmark and existing docs name them.
# Only when YOU set it. This Makefile's own ENVIRONMENT default is kind-emulator,
# for the e2e config — leaking that into an install would point a real cluster's
# deploy at the emulator. Unset, install.sh DETECTS OpenShift (API discovery,
# which any authenticated user may read) and uses kubernetes otherwise.
ENVIRONMENT_INSTALL = $(if $(filter command line environment,$(origin ENVIRONMENT)),$(ENVIRONMENT),)

.PHONY: setup-prereqs
setup-prereqs: manifests kustomize ## Phase 2 (CLUSTER ADMIN). ENVIRONMENT=kubernetes|openshift, SCOPE=namespace|cluster.
	$(call wva_phase,prereqs,$(ENVIRONMENT_INSTALL))

.PHONY: deploy-wva
deploy-wva: manifests kustomize ## Install WVA. ENVIRONMENT=kubernetes|openshift, SCOPE=namespace|cluster, INSTALL_PHASE=all|prereqs|wva, IMG=<your build>.
	$(call wva_phase,$(INSTALL_PHASE_ARG),$(ENVIRONMENT_INSTALL))

.PHONY: undeploy-wva
undeploy-wva: ## Remove WVA. Pass the same ENVIRONMENT, SCOPE and namespace you installed with.
	export KIND=$(KIND) KUBECTL=$(KUBECTL) $(if $(ENVIRONMENT_INSTALL),ENVIRONMENT=$(ENVIRONMENT_INSTALL),) $(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) WVA_SCOPE=$(SCOPE) && 		deploy/install.sh --undeploy

.PHONY: deploy-wva-on-k8s
deploy-wva-on-k8s: manifests kustomize ## Install WVA on Kubernetes. SCOPE=namespace|cluster, INSTALL_PHASE=all|prereqs|wva, IMG=<your build>. Prometheus and the namespace are detected.
	$(call wva_phase,$(INSTALL_PHASE_ARG),kubernetes)

.PHONY: deploy-wva-on-openshift
deploy-wva-on-openshift: manifests kustomize ## Install WVA on OpenShift. SCOPE=namespace|cluster, INSTALL_PHASE=all|prereqs|wva, IMG=<your build>.
	$(call wva_phase,$(INSTALL_PHASE_ARG),openshift)

## Removing. Pass the SAME SCOPE and namespace you installed with — an uninstall
## resolves the overlay exactly as the install did, so a mismatch leaves behind
## precisely the resources the other overlay owns.
.PHONY: undeploy-wva-on-k8s
undeploy-wva-on-k8s: ## Remove WVA from Kubernetes. Pass the same SCOPE and NAMESPACE/WVA_NS you installed with.
	@echo ">>> Undeploying workload-variant-autoscaler from Kubernetes"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=kubernetes $(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) WVA_SCOPE=$(SCOPE) && 		deploy/install.sh --undeploy

.PHONY: undeploy-wva-on-openshift
undeploy-wva-on-openshift: ## Remove WVA from OpenShift. Pass the same SCOPE and NAMESPACE/WVA_NS you installed with.
	@echo ">>> Undeploying workload-variant-autoscaler from OpenShift"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=openshift $(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) WVA_SCOPE=$(SCOPE) && 		deploy/install.sh --undeploy

## Check that everything a deploy needs is present, without deploying anything.
## Runs the SAME check the install runs (deploy/lib/prereqs.sh), so a pass here
## and a prerequisite failure mid-install is not a reachable state.
.PHONY: check-prereqs
check-prereqs: ## Phase 1, read-only: tools, permissions, the namespace and the Prometheus it found. ENVIRONMENT=kubernetes|openshift, SCOPE=namespace|cluster.
	$(call wva_check,$(ENVIRONMENT_INSTALL))

## Health check: is the WVA controller running, and did model-server metrics
## actually arrive? Runs the exact same check deploy-wva runs at the end of
## every install (deploy/lib/verify.sh), exposed here to re-run any time with
## no install attached. `kubectl rollout status` returns immediately when the
## rollout is already complete, so this doesn't sit around waiting on a
## controller that has been healthy for days.
.PHONY: verify-deployment
verify-deployment: ## Is the controller running, and are metrics flowing? Read-only. WVA_NS=<ns>, NAMESPACE=<ns>.
	@$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/constants.sh; source deploy/lib/infra_monitoring.sh; source deploy/lib/verify.sh; \
			export DEPLOY_PROMETHEUS=$${DEPLOY_PROMETHEUS:-true}; export DEPLOY_OPERATIONAL_DASHBOARD=$${DEPLOY_OPERATIONAL_DASHBOARD:-true}; export SCALER_BACKEND=$${SCALER_BACKEND:-keda}; export MONITORING_NAMESPACE=$${MONITORING_NAMESPACE:-workload-variant-autoscaler-monitoring}; \
			wva_bootstrap_env; verify_deployment'

## List the llm-d model servers WVA would create ScaledObjects for, and stop.
## Writes an editable YAML plan; nothing is applied. A ScaledObject is how a
## workload REGISTERS with WVA, so this is the step between "installed" and
## "scaling". The plan documents its own fields in the comments it is written with.
.PHONY: scaledobjects-plan
scaledobjects-plan: ## List llm-d model servers and write an editable ScaledObject plan. WVA_DEFAULT_SO_NS=<ns>|wva|all, WVA_DEFAULT_SO_PLAN=<file>.
	@$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) 		WVA_DEFAULT_SO=plan $(if $(WVA_DEFAULT_SO_NS),WVA_DEFAULT_SO_NS=$(WVA_DEFAULT_SO_NS),) 		$(if $(WVA_DEFAULT_SO_PLAN),WVA_DEFAULT_SO_PLAN=$(WVA_DEFAULT_SO_PLAN),) 		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; install_default_scaledobjects'

## Report the pod-spec settings that decide whether autoscaling a workload is
## safe, as a patch you can apply where the pod spec is owned.
## Create the shared weights cache the model servers read from.
##
## Deliberately asks for the two fields it will not guess. Size depends on how
## many models share the claim and how large they are; the StorageClass must
## support ReadWriteMany, because a claim that binds to one node leaves every
## replica on any other node Pending -- autoscaling that cannot work, rather
## than a cache that is merely slow. Run with neither and it lists the classes
## this cluster is already serving RWX from.
.PHONY: model-cache
model-cache: ## Create the weights PVC. NAMESPACE=<ns> WVA_MODEL_PVC_SIZE=<size> WVA_MODEL_PVC_CLASS=<rwx-class>. With no size/class, lists candidate classes.
	@# An explicit NAMESPACE wins, and is passed as the ARGUMENT rather than left to
	@# wva_resolve_namespace, which only copies NAMESPACE into WVA_NS at namespace
	@# scope -- so under SCOPE=cluster this created the claim in the controllers own
	@# namespace while the operator watched it name theirs.
	@#
	@# These comments sit BEFORE the recipe, not inside it: a \ line placed after
	@# a backslash continuation is not a recipe line at all, it is part of the
	@# command, and the shell tried to run it -- "@#: command not found".
	@$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) \
		$(if $(WVA_MODEL_PVC_NAME),WVA_MODEL_PVC_NAME=$(WVA_MODEL_PVC_NAME),) \
		$(if $(WVA_MODEL_PVC_SIZE),WVA_MODEL_PVC_SIZE=$(WVA_MODEL_PVC_SIZE),) \
		$(if $(WVA_MODEL_PVC_CLASS),WVA_MODEL_PVC_CLASS=$(WVA_MODEL_PVC_CLASS),) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; wva_model_cache "$(if $(filter command line environment,$(origin NAMESPACE)),$(NAMESPACE),$${WVA_NS})"'

.PHONY: workload-patch
workload-patch: ## Write a patch for model servers that do not drain on scale-down, or download weights outside every volume they mount. NAMESPACE=<ns> scopes it; WVA_WORKLOAD_PATCH_APPLY=true applies the drain half live (add WVA_WORKLOAD_PATCH_APPLY_WEIGHTS=true for the volume, after `make model-cache`).
	@# NAMESPACE pins the SCAN, not just the connection. Without the
	@# WVA_DEFAULT_SO_NS fallback below, `make workload-patch NAMESPACE=x` under a
	@# cluster-scoped install still walked every namespace on the cluster:
	@# so_target_namespaces reads WVA_DEFAULT_SO_NS, else the install scope, and
	@# never NAMESPACE. With APPLY=true that is a rolling restart of every model
	@# server on the cluster, from a command that reads as scoped to one.
	@$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) \
			$(if $(WVA_DEFAULT_SO_NS),WVA_DEFAULT_SO_NS=$(WVA_DEFAULT_SO_NS),$(if $(filter command line environment,$(origin NAMESPACE)),WVA_DEFAULT_SO_NS=$(NAMESPACE),)) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; wva_workload_patch'

## Apply a ScaledObject plan. With WVA_DEFAULT_SO_PLAN=<file> it applies exactly
## that file, edits included, and needs no terminal. Without one it re-discovers
## and applies everything found.
.PHONY: scaledobjects-apply
scaledobjects-apply: ## Apply a ScaledObject plan (this is what makes WVA scale anything). Per entry: apply: yes|no|adopt. WVA_DEFAULT_SO_PLAN=<edited file>, WVA_DEFAULT_SO_TEMPLATE=<file>.
	@$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) 		WVA_DEFAULT_SO=true $(if $(WVA_DEFAULT_SO_NS),WVA_DEFAULT_SO_NS=$(WVA_DEFAULT_SO_NS),) 		$(if $(WVA_DEFAULT_SO_PLAN),WVA_DEFAULT_SO_PLAN=$(WVA_DEFAULT_SO_PLAN),) 		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; install_default_scaledobjects'

## Review the list in $EDITOR, then apply what you confirm. Needs a terminal;
## use scaledobjects-plan + scaledobjects-apply where there is none.
.PHONY: scaledobjects-edit
scaledobjects-edit: ## Review the discovered model servers in $$EDITOR and apply what you confirm.
	@$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) 		WVA_DEFAULT_SO=edit $(if $(WVA_DEFAULT_SO_NS),WVA_DEFAULT_SO_NS=$(WVA_DEFAULT_SO_NS),) 		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; install_default_scaledobjects'

## Repair ScaledObjects that ask for WVA by name but name a namespace where no
## scaler is running -- what the samples do when you install anywhere other than
## the namespace they hardcode. Only rewrites scalerAddress, and only when the
## address it names resolves to nothing; one pointing at a second, live WVA
## install is deliberate and is left alone. Idempotent.
##
## Takes no arguments: it finds the running install and scans the cluster. Pass
## WVA_NS=<ns> only to disambiguate several installs, WVA_DEFAULT_SO_NS=<ns> only
## to narrow the scan.
.PHONY: scaledobjects-repoint
scaledobjects-repoint: ## Repoint ScaledObjects naming a missing WVA at the install that is running. Usually needs no arguments.
	@$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) \
		$(if $(WVA_DEFAULT_SO_NS),WVA_DEFAULT_SO_NS=$(WVA_DEFAULT_SO_NS),) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; so_repoint_stale'

## Health check: does every registered ScaledObject's modelID still match what
## its container actually serves? Catches a hand-changed serving model that
## nothing re-syncs -- WVA then silently applies zero decisions for that
## workload, forever, with the HPA reading a healthy ratio the whole time.
.PHONY: verify-scaledobjects
verify-scaledobjects: ## Rescan model servers and check every ScaledObject's modelID for drift. Read-only.
	@$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) \
		$(if $(WVA_DEFAULT_SO_NS),WVA_DEFAULT_SO_NS=$(WVA_DEFAULT_SO_NS),) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; so_verify_scaledobjects'

## Health check: is every FMA launcher pod actually being scraped? A launcher
## declares no container ports, so a monitor that merely SELECTS one produces
## no target -- and that variant's engine metrics never arrive while the HPA
## still reads healthy. Reuses the same detection so_discover_fma_requesters
## already runs while building a ScaledObject plan.
.PHONY: verify-fma
verify-fma: ## Check every FMA launcher pod has a scrape target. Read-only. WVA_NS=<ns>, NAMESPACE=<ns>.
	@$(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) \
		$(if $(WVA_DEFAULT_SO_NS),WVA_DEFAULT_SO_NS=$(WVA_DEFAULT_SO_NS),) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; so_verify_fma'

# The environment the so-* targets share. Same shape the scaledobjects-* targets
# pass inline: WVA_NS and NAMESPACE only when the CALLER set them, so an unset
# variable stays unset and the resolution in wva_bootstrap_env still runs.
WVA_SO_ENV = $(if $(filter command line environment,$(origin WVA_NS)),WVA_NS=$(WVA_NS),) $(if $(filter command line environment,$(origin NAMESPACE)),NAMESPACE=$(NAMESPACE),) WVA_SCOPE=$(SCOPE) $(if $(WVA_DEFAULT_SO_NS),WVA_DEFAULT_SO_NS=$(WVA_DEFAULT_SO_NS),)

## Parking a WVA-managed workload, and putting it back.
##
## `kubectl scale --replicas=0` does NOT idle one: the HPA that KEDA owns enforces
## minReplicaCount and restores it within seconds. maxReplicaCount=0 is not a valid
## state either. And scaling the CONTROLLER down is worse than doing nothing --
## KEDA cannot reach the scaler, falls back to a CPU metric, and keeps sizing the
## workload by the wrong signal while everything reads healthy. The supported lever
## is on the ScaledObject; the controller can stay up, it holds no GPU.
##
## All three ASK which ScaledObject, because picking the wrong one takes down a
## workload that was meant to keep serving. SO=<name>[,<name>] or SO=all skips the
## prompt, which is also what CI and any non-interactive caller must pass.
.PHONY: so-list
so-list: ## Show WVA-managed workloads with their replicas, GPUs and park/freeze state.
	@$(WVA_SO_ENV) bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; so_list'

.PHONY: so-park
so-park: ## Park workloads at 0 replicas and stop autoscaling, releasing their GPUs. PARK_REPLICAS=<n> to park at n. SO=<name>|all.
	@$(WVA_SO_ENV) $(if $(PARK_REPLICAS),PARK_REPLICAS=$(PARK_REPLICAS),) $(if $(SO),SO=$(SO),) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; so_park'

.PHONY: so-freeze
so-freeze: ## Stop autoscaling but hold the current replica count (keeps GPUs). For maintenance. SO=<name>|all.
	@$(WVA_SO_ENV) $(if $(SO),SO=$(SO),) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; so_freeze'

.PHONY: so-resume
so-resume: ## Undo so-park or so-freeze: WVA sizes the workload again from live metrics. SO=<name>|all.
	@$(WVA_SO_ENV) $(if $(SO),SO=$(SO),) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; wva_bootstrap_env; so_resume'

## A private Grafana for one namespace, with WVA's operational dashboard already
## imported -- see deploy/lib/dashboard.sh for
## the design this carries over (grafana-operator, the Thanos TENANCY port instead
## of cluster-monitoring-view, a GrafanaDashboard CR as the import bridge).
##
## Idempotent: safe to re-run, and it prints the dashboard URL and the
## password-retrieval command every time, not only on first creation.
.PHONY: dashboard
dashboard: ## OpenShift only: stand up (or re-report) a private Grafana + WVA dashboard in NAMESPACE. Needs grafana-operator.
	@$(WVA_SO_ENV) IMG=$(IMG) WVA_PROJECT=$(CURDIR) \
		bash -c 'source deploy/lib/common.sh; source deploy/lib/scaledobject.sh; source deploy/lib/infra_monitoring.sh; source deploy/lib/dashboard.sh; wva_bootstrap_env; wva_dashboard'

# E2E tests on Kind cluster for saturation-based autoscaling
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# Supports FOCUS and SKIP variables for ginkgo test filtering.
# Setup options:
# - CERT_MANAGER_INSTALL_SKIP=true: Skip certManager installation during test setup.
# - IMAGE_BUILD_SKIP=true: Skip building the WVA docker image during test setup.
# - INFRA_SETUP_SKIP=true: Skip setting up the llm-d and the WVA controller manager during test setup. Reload the docker image if necessary.
# - INFRA_TEARDOWN_SKIP=true: Skip tearing down the Kind cluster during test teardown.

# Consolidated e2e test targets (environment-agnostic)
# These targets use the test/e2e/ suite that works on any Kubernetes cluster
# Supports FOCUS and SKIP variables for ginkgo test filtering.

# Deploys WVA + monitoring + scaler (install.sh), then EPP (install-epp.sh). No model server or VA/HPA.
# Works for all environments: kind-emulator (default), openshift, kubernetes.
# For OpenShift/Kubernetes: ENVIRONMENT=openshift NAMESPACE=<your-ns> make deploy-e2e-infra
# If IMG is set, builds the image locally first (unless SKIP_BUILD=true).
.PHONY: deploy-e2e-infra
deploy-e2e-infra: ## Deploy e2e test infrastructure (WVA + EPP; no model server or VA/HPA). Works for kind-emulator, openshift, kubernetes.
	@# CLUSTER-scoped, deliberately, even though the controller now lives with the
	@# models. smoke_keda_test.go and limiter_test.go create ISOLATED NAMESPACES at
	@# runtime, and a namespace-scoped controller cannot serve a ScaledObject in a
	@# namespace that did not exist when it was installed -- KEDA reported the
	@# ScaledObject Ready=False in smoke-keda-basic-fjr5d because nothing watched it.
	@# kind used to INFER cluster scope; #24 made namespace the default everywhere and
	@# nothing here said otherwise. Aligning WVA_NS with the model namespace stays:
	@# it is the SCOPE that has to be cluster, not the controller's location.
	@# Installed through `make deploy-wva` -- the target a USER runs -- not by
	@# calling deploy/install.sh with a hand-assembled environment.
	@#
	@# The suite shared the script but not the invocation, so it never passed
	@# WVA_SCOPE, NAMESPACE, WVA_LIMITER or INSTALL_PHASE. When the scope
	@# default changed from platform-inferred to `namespace`, the e2e silently
	@# changed shape and nothing in the suite covered the path users take: a
	@# change to install semantics could land green here and break `make
	@# deploy-wva`. Now the same macro serves both.
	@#
	@# WVA_NS and NAMESPACE are both PINNED. The suite puts model servers in
	@# LLMD_NAMESPACE and the controller in CONTROLLER_NAMESPACE, so the namespace
	@# WVA MANAGES has to be named -- that is what WVA_WATCH_NS is for, and
	@# wva_apply_watch_namespace_rbac renders the manager Role into it.
	@#
	@# Aligning them beats reaching for cluster scope: namespace scope is the
	@# supported shape, cluster scope now warns that it is WIP, and the suite
	@# should exercise what users run. Unset, the install watched only its own
	@# namespace and took namespace-scoped RBAC with it:
	@#   scaledobjects.keda.sh "stz-rt-so" is forbidden
	@#   Could not read the ScaledObject for a registered workload
	@# so the workload was never enriched and never parked.
	@#
	@# WVA_NS is PINNED. Without it the install lands wherever namespace discovery
	@# points, and the suite builds scalerAddress from CONTROLLER_NAMESPACE -- so a
	@# controller that moves is one KEDA can never reach.
	@#
	@# It moved when the scope default changed: wva_autoselect_namespace runs only
	@# in NAMESPACE scope, which used to be inferred as `cluster` on kind, so it was
	@# skipped. Now that namespace is the default everywhere it runs, finds the one
	@# namespace with model servers (llm-d-sim) and installs the controller there.
	@# Measured: the controller came up in llm-d-sim while every ScaledObject named
	@# wva-external-scaler.workload-variant-autoscaler-system, so no decision ever
	@# reached KEDA and four specs sat at one replica for their full 600s.
	@echo "Deploying e2e test infrastructure..."
	@if [ -n "$(IMG)" ]; then \
		echo "IMG is set to '$(IMG)'"; \
		if [ "$(SKIP_BUILD)" != "true" ]; then \
			echo "Building local image (SKIP_BUILD not set)..."; \
			$(MAKE) docker-build IMG=$(IMG); \
		else \
			echo "Skipping image build (SKIP_BUILD=true) - assuming image already exists"; \
		fi; \
		echo "Extracting image repo and tag from IMG..."; \
		if echo "$(IMG)" | grep -q ":"; then \
			IMAGE_REPO=$$(echo $(IMG) | cut -d: -f1); \
			IMAGE_TAG=$$(echo $(IMG) | cut -d: -f2); \
		else \
			IMAGE_REPO="$(IMG)"; \
			IMAGE_TAG="latest"; \
		fi; \
		echo "Using local image: $$IMAGE_REPO:$$IMAGE_TAG"; \
		SCALER_BACKEND=$(SCALER_BACKEND) \
		ENABLE_SCALE_TO_ZERO=$(SCALE_TO_ZERO_ENABLED) \
		DEPLOY_ALERTING_RULES=$(DEPLOY_ALERTING_RULES) \
		WVA_IMAGE_REPO=$$IMAGE_REPO \
		WVA_IMAGE_TAG=$$IMAGE_TAG \
		WVA_IMAGE_PULL_POLICY=IfNotPresent \
		$(MAKE) deploy-wva \
			ENVIRONMENT=$(ENVIRONMENT) \
			WVA_NS=$(CONTROLLER_NAMESPACE) \
			NAMESPACE=$(CONTROLLER_NAMESPACE) \
			SCOPE=cluster \
			IMG=$(IMG); \
	else \
		echo "IMG not set - using default image from registry (latest)"; \
		SCALER_BACKEND=$(SCALER_BACKEND) \
		ENABLE_SCALE_TO_ZERO=$(SCALE_TO_ZERO_ENABLED) \
		DEPLOY_ALERTING_RULES=$(DEPLOY_ALERTING_RULES) \
		$(MAKE) deploy-wva \
			ENVIRONMENT=$(ENVIRONMENT) \
			WVA_NS=$(CONTROLLER_NAMESPACE) \
			NAMESPACE=$(CONTROLLER_NAMESPACE) \
			SCOPE=cluster; \
	fi
	@ENVIRONMENT=$(ENVIRONMENT) \
		LLM_D_ROUTER_VERSION=$(LLM_D_ROUTER_VERSION) \
		GAIE_VERSION=$(GAIE_VERSION) \
		NAMESPACE=$${NAMESPACE:-$(E2E_EMULATED_LLMD_NAMESPACE)} \
		WVA_PROJECT=$(CURDIR) \
		ENABLE_SCALE_TO_ZERO=$(SCALE_TO_ZERO_ENABLED) \
		./deploy/install-epp.sh
	@NS=$${WVA_NS:-workload-variant-autoscaler-system}; \
	if [ -n "$(SCALE_UP_THRESHOLD)$(SCALE_DOWN_BOUNDARY)" ]; then \
		echo "Applying optional WVA scaling-band overrides (SCALE_UP_THRESHOLD / SCALE_DOWN_BOUNDARY)..."; \
		cur=$$($(KUBECTL) get configmap wva-scaling-policy-config -n "$$NS" -o jsonpath="{.data.default}" 2>/dev/null); \
		if [ -z "$$cur" ]; then \
			echo "  ERROR: no wva-scaling-policy-config/default in $$NS. Install WVA before overriding its thresholds."; \
			exit 1; \
		fi; \
		if [ -n "$(SCALE_UP_THRESHOLD)" ]; then \
			cur=$$(printf "%s" "$$cur" | sed "s|^scaleUpThreshold:.*|scaleUpThreshold: $(SCALE_UP_THRESHOLD)|"); \
		fi; \
		if [ -n "$(SCALE_DOWN_BOUNDARY)" ]; then \
			cur=$$(printf "%s" "$$cur" | sed "s|^scaleDownBoundary:.*|scaleDownBoundary: $(SCALE_DOWN_BOUNDARY)|"); \
		fi; \
		$(KUBECTL) patch configmap wva-scaling-policy-config \
			-n "$$NS" --type=merge \
			-p "$$(jq -n --arg d "$$cur" "{data:{default:\$$d}}")"; \
	fi


# Runs the smoke subset of the e2e suite. KEDA is the only scaler backend.
.PHONY: test-e2e-smoke
test-e2e-smoke: ## Run smoke e2e tests
	@echo "Running smoke e2e tests..."
	$(eval FOCUS_ARGS := $(if $(FOCUS),-ginkgo.focus="$(FOCUS)",))
	$(eval SKIP_ARGS := $(if $(SKIP),-ginkgo.skip="$(SKIP)",))
	@# The suite reaches Prometheus at https://localhost:9090
	@# (utils.DefaultPrometheusURL) and creates no port-forward of its own, so
	@# without one every Prometheus-dependent assertion calls Skip. Nothing else
	@# created it either -- not CI, not this Makefile -- so in CI those assertions
	@# have never run. The suite still goes green, because a Skip is not a failure:
	@#
	@#   [SKIPPED] Prometheus is not reachable from the test host: timed out
	@#   waiting for the condition (port-forward svc/kube-prometheus-stack-prometheus
	@#   9090:9090 for the metric assertions)
	@#
	@# 17 skip sites across the suite mention Prometheus. Started here rather than in
	@# the workflow so a local run gets the same coverage as CI, and torn down after.
	@# Absence is still a warning rather than an error: the assertion that proves
	@# scale-to-zero -- the deployment reaching zero -- needs no Prometheus at all.
	PF_PID=""; \
	if kubectl get svc -n $(E2E_MONITORING_NAMESPACE) kube-prometheus-stack-prometheus >/dev/null 2>&1; then \
		kubectl port-forward -n $(E2E_MONITORING_NAMESPACE) svc/kube-prometheus-stack-prometheus 9090:9090 >/dev/null 2>&1 & \
		PF_PID=$$!; \
		for i in $$(seq 1 20); do \
			curl -sk --max-time 2 https://localhost:9090/-/ready >/dev/null 2>&1 && break; \
			sleep 1; \
		done; \
		if curl -sk --max-time 2 https://localhost:9090/-/ready >/dev/null 2>&1; then \
			echo "Prometheus port-forward ready on https://localhost:9090"; \
		else \
			echo "WARNING: Prometheus port-forward did not become ready -- metric assertions will Skip"; \
		fi; \
	else \
		echo "WARNING: svc/kube-prometheus-stack-prometheus not found in $(E2E_MONITORING_NAMESPACE) -- metric assertions will Skip"; \
	fi; \
	KUBECONFIG=$(KUBECONFIG) \
	ENVIRONMENT=$(ENVIRONMENT) \
	WVA_NAMESPACE=$(CONTROLLER_NAMESPACE) \
	LLMD_NAMESPACE=$(E2E_EMULATED_LLMD_NAMESPACE) \
	MONITORING_NAMESPACE=$(E2E_MONITORING_NAMESPACE) \
	WVA_E2E_SECONDARY_OVERLAY_PATH=$${WVA_E2E_SECONDARY_OVERLAY_PATH:-$(E2E_WVA_SECONDARY_OVERLAY_PATH)} \
	USE_SIMULATOR=$(USE_SIMULATOR) \
	SCALE_TO_ZERO_ENABLED=$(SCALE_TO_ZERO_ENABLED) \
	DEPLOY_ALERTING_RULES=$(DEPLOY_ALERTING_RULES) \
	SCALER_BACKEND=keda \
	MODEL_ID=$(MODEL_ID) \
	go test ./test/e2e/ -timeout $(E2E_GO_TIMEOUT) -v -ginkgo.v -ginkgo.timeout=$(E2E_TIMEOUT) \
		-ginkgo.label-filter="smoke" $(FOCUS_ARGS) $(SKIP_ARGS); \
	TEST_EXIT_CODE=$$?; \
	[ -n "$$PF_PID" ] && kill $$PF_PID 2>/dev/null || true; \
	echo ""; \
	echo "=========================================="; \
	echo "Test execution completed. Exit code: $$TEST_EXIT_CODE"; \
	echo "=========================================="; \
	exit $$TEST_EXIT_CODE

# Runs the complete e2e test suite (KEDA backend, excluding smoke and flaky tests).
.PHONY: test-e2e-full
test-e2e-full: ## Run full e2e test suite
	@echo "Running full e2e test suite..."
	$(eval FOCUS_ARGS := $(if $(FOCUS),-ginkgo.focus="$(FOCUS)",))
	$(eval SKIP_ARGS := $(if $(SKIP),-ginkgo.skip="$(SKIP)",))
	@# The suite reaches Prometheus at https://localhost:9090
	@# (utils.DefaultPrometheusURL) and creates no port-forward of its own, so
	@# without one every Prometheus-dependent assertion calls Skip. Nothing else
	@# created it either -- not CI, not this Makefile -- so in CI those assertions
	@# have never run. The suite still goes green, because a Skip is not a failure:
	@#
	@#   [SKIPPED] Prometheus is not reachable from the test host: timed out
	@#   waiting for the condition (port-forward svc/kube-prometheus-stack-prometheus
	@#   9090:9090 for the metric assertions)
	@#
	@# 17 skip sites across the suite mention Prometheus. Started here rather than in
	@# the workflow so a local run gets the same coverage as CI, and torn down after.
	@# Absence is still a warning rather than an error: the assertion that proves
	@# scale-to-zero -- the deployment reaching zero -- needs no Prometheus at all.
	PF_PID=""; \
	if kubectl get svc -n $(E2E_MONITORING_NAMESPACE) kube-prometheus-stack-prometheus >/dev/null 2>&1; then \
		kubectl port-forward -n $(E2E_MONITORING_NAMESPACE) svc/kube-prometheus-stack-prometheus 9090:9090 >/dev/null 2>&1 & \
		PF_PID=$$!; \
		for i in $$(seq 1 20); do \
			curl -sk --max-time 2 https://localhost:9090/-/ready >/dev/null 2>&1 && break; \
			sleep 1; \
		done; \
		if curl -sk --max-time 2 https://localhost:9090/-/ready >/dev/null 2>&1; then \
			echo "Prometheus port-forward ready on https://localhost:9090"; \
		else \
			echo "WARNING: Prometheus port-forward did not become ready -- metric assertions will Skip"; \
		fi; \
	else \
		echo "WARNING: svc/kube-prometheus-stack-prometheus not found in $(E2E_MONITORING_NAMESPACE) -- metric assertions will Skip"; \
	fi; \
	KUBECONFIG=$(KUBECONFIG) \
	ENVIRONMENT=$(ENVIRONMENT) \
	WVA_NAMESPACE=$(CONTROLLER_NAMESPACE) \
	WVA_E2E_SECONDARY_OVERLAY_PATH=$${WVA_E2E_SECONDARY_OVERLAY_PATH:-$(E2E_WVA_SECONDARY_OVERLAY_PATH)} \
	USE_SIMULATOR=$(USE_SIMULATOR) \
	SCALE_TO_ZERO_ENABLED=$(SCALE_TO_ZERO_ENABLED) \
	DEPLOY_ALERTING_RULES=$(DEPLOY_ALERTING_RULES) \
	SCALER_BACKEND=keda \
	KEDA_NAMESPACE=$(E2E_KEDA_NAMESPACE) \
	MODEL_ID=$(MODEL_ID) \
	go test ./test/e2e/ -timeout $(E2E_GO_TIMEOUT) -v -ginkgo.v -ginkgo.timeout=$(E2E_TIMEOUT) \
		-ginkgo.label-filter="full && !smoke && !flaky" $(FOCUS_ARGS) $(SKIP_ARGS); \
	TEST_EXIT_CODE=$$?; \
	[ -n "$$PF_PID" ] && kill $$PF_PID 2>/dev/null || true; \
	echo ""; \
	echo "=========================================="; \
	echo "Test execution completed. Exit code: $$TEST_EXIT_CODE"; \
	echo "=========================================="; \
	exit $$TEST_EXIT_CODE

# Convenience targets for local e2e testing

# Convenience target that deploys KEDA infra + runs smoke tests.
# Set DELETE_CLUSTER=true to delete Kind cluster after tests (default: keep cluster for debugging).
.PHONY: test-e2e-smoke-with-setup
test-e2e-smoke-with-setup:
	$(MAKE) deploy-e2e-infra DEPLOY_ALERTING_RULES=true SCALER_BACKEND=keda
	$(MAKE) test-e2e-smoke DEPLOY_ALERTING_RULES=true

# Runs only the multi-controller (dual namespace-scoped) e2e tests.
.PHONY: test-e2e-multi-controller
test-e2e-multi-controller: ## Run multi-controller e2e tests
	@echo "Running multi-controller e2e tests..."
	$(eval FOCUS_ARGS := $(if $(FOCUS),-ginkgo.focus="$(FOCUS)",))
	$(eval SKIP_ARGS := $(if $(SKIP),-ginkgo.skip="$(SKIP)",))
	KUBECONFIG=$(KUBECONFIG) \
	ENVIRONMENT=$(ENVIRONMENT) \
	WVA_NAMESPACE=$(CONTROLLER_NAMESPACE) \
	LLMD_NAMESPACE=$(E2E_EMULATED_LLMD_NAMESPACE) \
	MONITORING_NAMESPACE=$(E2E_MONITORING_NAMESPACE) \
	WVA_E2E_SECONDARY_OVERLAY_PATH=$${WVA_E2E_SECONDARY_OVERLAY_PATH:-$(E2E_WVA_SECONDARY_OVERLAY_PATH)} \
	USE_SIMULATOR=$(USE_SIMULATOR) \
	SCALE_TO_ZERO_ENABLED=$(SCALE_TO_ZERO_ENABLED) \
	DEPLOY_ALERTING_RULES=$(DEPLOY_ALERTING_RULES) \
	SCALER_BACKEND=$(SCALER_BACKEND) \
	MODEL_ID=$(MODEL_ID) \
	go test ./test/e2e/ -timeout $(E2E_GO_TIMEOUT) -v -ginkgo.v -ginkgo.timeout=$(E2E_TIMEOUT) \
		-ginkgo.label-filter="multi-controller" $(FOCUS_ARGS) $(SKIP_ARGS); \
	TEST_EXIT_CODE=$$?; \
	echo ""; \
	echo "=========================================="; \
	echo "Test execution completed. Exit code: $$TEST_EXIT_CODE"; \
	echo "=========================================="; \
	exit $$TEST_EXIT_CODE

# Convenience target that deploys infra + runs multi-controller tests.
.PHONY: test-e2e-multi-controller-with-setup
test-e2e-multi-controller-with-setup: deploy-e2e-infra test-e2e-multi-controller

# Convenience target that deploys KEDA infra + runs full test suite.
# Set DELETE_CLUSTER=true to delete Kind cluster after tests (default: keep cluster for debugging).
# LWS is installed because the full suite includes LeaderWorkerSet scale-from-zero tests.
.PHONY: test-e2e-full-with-setup
test-e2e-full-with-setup:
	DEPLOY_LWS=true SCALER_BACKEND=keda $(MAKE) deploy-e2e-infra
	$(MAKE) test-e2e-full


##@ llm-d-benchmark CLI (standup / run / teardown)

# llmdbenchmark binary from the benchmark repo venv
BENCHMARK_VENV       = $(BENCHMARK_REPO_DIR)/.venv
LLMDBENCHMARK        = $(shell command -v llmdbenchmark 2>/dev/null || echo $(BENCHMARK_VENV)/bin/llmdbenchmark)

# Common llmdbenchmark flags (spec + workspace + base dir for config resolution)
BENCHMARK_CLI_FLAGS = --spec $(BENCHMARK_SPEC) --workspace $(BENCHMARK_WORKSPACE) --base-dir $(BENCHMARK_REPO_DIR)

.PHONY: benchmark-install
benchmark-install: ## Clone llm-d-benchmark at BENCHMARK_REPO_REF (default v0.7.0) and install the llmdbenchmark CLI
	@if [ ! -d "$(BENCHMARK_REPO_DIR)" ]; then \
		echo "Cloning llm-d-benchmark @ $(BENCHMARK_REPO_REF)..."; \
		git clone --branch $(BENCHMARK_REPO_REF) $(BENCHMARK_REPO_URL) $(BENCHMARK_REPO_DIR); \
	else \
		echo "llm-d-benchmark already cloned at $(BENCHMARK_REPO_DIR); checking out $(BENCHMARK_REPO_REF)..."; \
		cd $(BENCHMARK_REPO_DIR) && git fetch --tags && git checkout $(BENCHMARK_REPO_REF); \
	fi
	@# install.sh reaches for sudo when a prerequisite is missing: it apt-installs
	@# pip when python3 has no ensurepip, and drops helmfile/helm/kubectl into
	@# /usr/local/bin. Under a non-interactive shell that sudo prompt has no
	@# terminal to read from, and the step HANGS -- silently, because this recipe
	@# is @-prefixed. Observed on WSL: no venv, no output, no error, forever.
	@#
	@# So say what is missing before running it. Closing stdin is NOT enough on
	@# its own -- sudo reads the password from /dev/tty, not stdin, so
	@# `install.sh </dev/null` still hung on `sudo apt-get update` for 50
	@# minutes. It runs under setsid below, which detaches the controlling
	@# terminal so sudo has no tty to prompt on and fails immediately.
	@# Everything suggested below installs into $$HOME and needs no admin rights.
	@missing=""; \
	command -v helmfile >/dev/null 2>&1 || missing="$$missing helmfile"; \
	if ! command -v uv >/dev/null 2>&1 && ! python3 -c 'import ensurepip' >/dev/null 2>&1; then \
		missing="$$missing uv-or-python3-venv"; \
	fi; \
	if [ -n "$$missing" ]; then \
		echo "Missing prerequisites:$$missing"; \
		echo ""; \
		echo "llm-d-benchmark's install.sh would try to install these with sudo, and"; \
		echo "in a non-interactive shell that blocks on a password prompt forever."; \
		echo "Install them into your home directory instead -- no admin rights needed:"; \
		echo ""; \
		echo "  # uv (brings its own Python, avoids the missing-ensurepip apt path)"; \
		echo "  curl -LsSf https://astral.sh/uv/install.sh | sh"; \
		echo ""; \
		echo "  # helmfile -- the version llm-d-benchmark pins (tool_version_for)"; \
		echo "  HF=$$(sed -n 's/.*helmfile)[[:space:]]*echo[[:space:]]*\"\([0-9.]*\)\".*/\1/p' \"$(BENCHMARK_REPO_DIR)/install.sh\" 2>/dev/null | head -1); HF=$${HF:-1.5.1}; \\"; \
		echo "  curl -sSL \"https://github.com/helmfile/helmfile/releases/download/v\$$HF/helmfile_\$${HF}_linux_amd64.tar.gz\" \\"; \
		echo "    | tar -xz -C \"\$$HOME/bin\" helmfile && chmod +x \"\$$HOME/bin/helmfile\""; \
		echo ""; \
		echo "Then re-run: make benchmark-install BENCHMARK_UV=true"; \
		exit 1; \
	fi
	@# Already working? Then do not run install.sh at all. It runs
	@# `sudo apt-get update` even when every tool it needs is present, and a
	@# standup re-runs this target on every invocation.
	@# Skip only when the installed CLI matches the CHECKOUT. The venv is an
	@# editable install, so its code follows the tree but its package VERSION is
	@# frozen at install time -- and the version resolver picks the harness image
	@# tag from that version. After moving the ref v0.7.0 -> v0.7.8 the CLI still
	@# reported 0.7.0, so the run used ghcr.io/llm-d/llm-d-benchmark:v0.7.0 while
	@# copying v0.7.8's harness scripts into it. Those scripts call `guidellm
	@# run`; the v0.7.0 image's guidellm only has `benchmark`, so every treatment
	@# died with "Error: No such command 'run'".
	@# Already working? Then do not run install.sh at all. It runs
	@# `sudo apt-get update` even when every tool it needs is present, and a
	@# standup re-runs this target on every invocation.
	@#
	@# The CLI is an editable install, so its CODE follows the checkout when the
	@# ref moves; only its package VERSION is frozen, and that version is not
	@# worth reinstalling for -- upstream ships pyproject 0.7.0 at tag v0.7.8, so
	@# it says 0.7.0 on every ref anyway. The one thing that version used to
	@# decide, the harness image tag, is now pinned explicitly in
	@# benchmark-standup and benchmark-run instead of being inferred from it.
	@# Build the venv HERE rather than delegating to install.sh, which runs
	@# `sudo apt-get update` unconditionally -- even when every tool it needs is
	@# already present. On a box without passwordless sudo that means a FRESH
	@# CLONE can never bootstrap: the guard above reports prerequisites fine,
	@# install.sh then dies on "sudo: a password is required", and the standup
	@# fails several minutes in having said nothing useful. Observed on WSL
	@# against pokprod.
	@#
	@# The CLI is just an editable install of the checkout, so a plain venv plus
	@# `pip install -e .` produces the same thing install.sh would, in $HOME, with
	@# no admin rights. install.sh stays as the fallback for whatever else it does
	@# on a machine that has not got this far.
	@if [ -x "$(LLMDBENCHMARK)" ] && "$(LLMDBENCHMARK)" --version >/dev/null 2>&1; then \
		echo "llmdbenchmark present at $(LLMDBENCHMARK) — skipping install.sh."; \
		echo "Force a reinstall with: rm -rf $(BENCHMARK_VENV)"; \
	else \
		echo "Building the llmdbenchmark venv without sudo..."; \
		# The CLI imports `planner`, which is a SEPARATE package install.sh pulls
		# from a git URL -- `pip install -e .` alone produces a venv whose
		# llmdbenchmark dies on `ModuleNotFoundError: No module named planner`.
		# The pin is read out of their install.sh, the same way this file already
		# reads the helm-diff and helmfile versions, so it cannot drift from the
		# version the standup was tested against.
		planner=$$(sed -n "s|^PLANNER_GIT=\"\(git+[^\"]*\)\"|\1|p" \
			"$(BENCHMARK_REPO_DIR)/install.sh" 2>/dev/null | head -1); \
		planner=$${planner:-git+https://github.com/llm-d-incubation/llm-d-planner.git@v0.1.0}; \
		# uv FIRST: this box has no python3-venv (no ensurepip), so `python3 -m venv`
		# fails with "You may need to use sudo with that command" -- the very thing
		# being avoided. uv brings its own Python and needs no admin rights.
		if command -v uv >/dev/null 2>&1; then \
			(cd $(BENCHMARK_REPO_DIR) && uv venv "$(BENCHMARK_VENV)" >/tmp/llmdbench-venv.log 2>&1 \
			 && VIRTUAL_ENV="$(BENCHMARK_VENV)" uv pip install -q -e . >>/tmp/llmdbench-venv.log 2>&1 \
			 && VIRTUAL_ENV="$(BENCHMARK_VENV)" uv pip install -q "$$planner" >>/tmp/llmdbench-venv.log 2>&1) || true; \
		else \
			(cd $(BENCHMARK_REPO_DIR) && python3 -m venv "$(BENCHMARK_VENV)" >/tmp/llmdbench-venv.log 2>&1 \
			 && "$(BENCHMARK_VENV)/bin/pip" install -q -e . >>/tmp/llmdbench-venv.log 2>&1 \
			 && "$(BENCHMARK_VENV)/bin/pip" install -q "$$planner" >>/tmp/llmdbench-venv.log 2>&1) || true; \
		fi; \
		if [ -x "$(LLMDBENCHMARK)" ] && "$(LLMDBENCHMARK)" --version >/dev/null 2>&1; then \
			echo "  built $$("$(LLMDBENCHMARK)" --version)"; \
		else \
			echo "  could not build it here; falling back to llm-d-benchmark install.sh."; \
			echo "  (that script uses sudo, so it fails fast on a box without it -- see /tmp/llmdbench-venv.log)"; \
			cd $(BENCHMARK_REPO_DIR) && \
			setsid ./install.sh $(if $(filter true,$(BENCHMARK_UV)),--uv,--no-uv) </dev/null; \
		fi; \
	fi
	@# helm-diff has to match helm's MAJOR version, and this step assumed Helm 4:
	@#   - v3.15.10's plugin.yaml uses `platformHooks`, a Helm 4 schema field.
	@#     Installing it under helm 3 leaves a plugin that loads on every helm
	@#     invocation and fails -- `helm plugin list`, and helmfile with it, stop
	@#     working until the directory is deleted by hand.
	@#   - `--verify` is a Helm 4 flag. Under helm 3 the command fails outright
	@#     with "unknown flag: --verify", so this step could never have run.
	@# Verified on helm v3.16.3, where it did both.
	@# The version comes from llm-d-benchmark's own tool_version_for(), so this
	@# cannot drift from the version the standup was tested against. Hardcoding a
	@# second copy is how this line came to pin a Helm 4 build on a helm 3 box.
	@diff_ver=$$(sed -n 's/.*helm-diff)[[:space:]]*echo[[:space:]]*"\(v[0-9.]*\)".*/\1/p' \
		"$(BENCHMARK_REPO_DIR)/install.sh" 2>/dev/null | head -1); \
	diff_ver=$${diff_ver:-v3.13.0}; \
	echo "Installing helm-diff $$diff_ver (pinned by llm-d-benchmark)..."; \
	helm plugin uninstall diff >/dev/null 2>&1 || true; \
	helm plugin install https://github.com/databus23/helm-diff --version $$diff_ver >/tmp/helm-diff-install.log 2>&1 \
		|| helm plugin install https://github.com/databus23/helm-diff --version $$diff_ver --verify=false >>/tmp/helm-diff-install.log 2>&1 \
		|| { echo "helm-diff install failed. helmfile uses it to diff; applies still work."; \
		     echo "Its own words:"; sed 's/^/  /' /tmp/helm-diff-install.log | tail -6; \
		     echo "  A helm older than the one llm-d-benchmark pins is the usual cause"; \
		     echo "  (helm $$(helm version --short 2>/dev/null) here)."; }; \
	helm plugin list 2>/dev/null | awk '$$1=="diff"{print "  installed: helm-diff " $$2}'
	@$(MAKE) --no-print-directory benchmark-patch

.PHONY: benchmark-patch
benchmark-patch: ## Reapply our fixes to the llm-d-benchmark clone (idempotent; run automatically by benchmark-install and benchmark-run)
	@# The clone is gitignored and benchmark-install re-checks it out to a pinned
	@# tag, so edits made in it are invisible to review and vanish on the next
	@# install. Our fixes therefore live in hack/benchmark/patch_harness.sh and
	@# are reapplied here. Two upstream bugs, both present in v0.7.8 AND on
	@# origin/main, so there is no release to upgrade to:
	@#
	@#   process_epp_logs.py  -- EPP writes "ts" as a float epoch; the parser
	@#     assumes an ISO string and dies on the FIRST entry, discarding the
	@#     whole log. That is why "Avg queue depth (EPP)" was "?" on every run
	@#     we ever took. Fixed properly; 127k-200k entries now parse.
	@#
	@#   guidellm-analyze_results.sh -- benchmark-report wants a top-level
	@#     "args" that guidellm no longer emits, so conversion always fails and
	@#     the pod exits non-zero, marking a perfectly good run FAILED. Made
	@#     non-fatal, NOT faked: see the script for why aliasing "config" onto
	@#     "args" would produce a silently wrong report.
	@#
	@# This reaches the cluster because step_06 builds the harness ConfigMap
	@# from the checked-out tree, by design ("so a run can use a new/updated
	@# harness with an older benchmark image").
	@if [ -d "$(BENCHMARK_REPO_DIR)" ]; then \
		GPU_MEM_UTIL=$(GPU_MEM_UTIL) bash "$(CURDIR)/hack/benchmark/patch_harness.sh" "$(BENCHMARK_REPO_DIR)"; \
	else \
		echo "benchmark-patch: no clone at $(BENCHMARK_REPO_DIR); run 'make benchmark-install' first"; \
	fi

BENCHMARK_SPECS_DIR ?= $(CURDIR)/hack/benchmark/scenarios/guides

# How many replicas' worth of FMA warm capacity to keep. 0 disables hot start
# entirely (the warmup step becomes a no-op), so it costs nothing on non-FMA
# runs. Deliberately expressed in REPLICAS, not the per-node launcherCount the
# populator actually enforces: this is the one number that may later be produced
# by something else -- WVA already computes desired and peak replicas -- and a
# future producer should not need node topology to say "keep 6 warm".
# hack/benchmark/warm_pool.sh owns the translation.
WARM_REPLICAS ?= 0

# Gate benchmark-run on the model actually serving. See hack/benchmark/wait_serving.sh.
WAIT_SERVING          ?= true
WAIT_SERVING_TIMEOUT  ?= 600

ACTUATION_TARGET ?=
ACTUATION_TRIALS ?= 5

# FMA controller version applied by benchmark-fma-fixups. The standup pins
# v0.6.0-alpha.13, which drops reconcile notifications (upstream #696) and leaves
# dead endpoints in the InferencePool.
FMA_VERSION ?= v0.6.4

# Pin the FMA controllers to specific IMAGES rather than to a version of the
# upstream registry. Needed to run a fork: a fork's build lives at its own path
# (ours is ghcr.io/ev-shindin/dual-pods-controller), which shares no prefix with
# upstream, so FMA_VERSION cannot reach it.
#
# Set this whenever a forked controller is under test. benchmark-standup re-runs
# benchmark-fma-fixups, so without it a standup mid-experiment silently restores
# the stock controller and the run measures unmodified behaviour:
#
#   make benchmark-fma-fixups BENCHMARK_NAMESPACE=<ns> \
#     FMA_CONTROLLER_IMAGE=ghcr.io/ev-shindin/dual-pods-controller:aa072ef
FMA_CONTROLLER_IMAGE ?=
FMA_POPULATOR_IMAGE  ?=

.PHONY: benchmark-fma-fixups
benchmark-fma-fixups: ## Re-apply the FMA fixes a standup undoes: launcher RBAC + controller version (set BENCHMARK_NAMESPACE)
	@# Run this after every FMA standup. Without it, FMA benchmarking does not
	@# produce bad numbers -- it produces NO numbers, and the failure looks like a
	@# load-generator crash rather than an FMA misconfiguration:
	@#
	@#   launcher SA cannot patch pods (403, retried forever)
	@#     -> a launcher whose requester was deleted keeps inferenceServing=true
	@#     -> a dead endpoint stays in the InferencePool
	@#     -> EPP dispatches to it, ~20% of requests 503
	@#     -> guidellm fails its one backend-validation probe and every worker dies
	@#     -> no results.json, every metric "?"
	@#
	@# Both fixes are namespaced; the cluster-scoped CRDs are untouched.
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required"; exit 1; \
	fi
	@FMA_CONTROLLER_IMAGE="$(FMA_CONTROLLER_IMAGE)" FMA_POPULATOR_IMAGE="$(FMA_POPULATOR_IMAGE)" \
		bash hack/benchmark/fma_fixups.sh $(BENCHMARK_NAMESPACE) $(FMA_VERSION)

.PHONY: benchmark-verify-warm-affinity
benchmark-verify-warm-affinity: ## Prove fma.warmAffinity beats default scheduler spreading (any cluster with >=3 nodes; no GPUs needed)
	@# warmAffinity is a PREFERRED podAffinity, so whether it actually wins is a
	@# question about scheduler scoring, not about YAML -- unreadable from the
	@# template. This runs both arms and compares. Measured on kind: 2/6 replicas
	@# on the launcher node without it, 6/6 with it.
	@bash -c 'command -v python3 >/dev/null || { echo "python3 required"; exit 1; }'
	@python3 hack/benchmark/verify_warm_affinity.py

.PHONY: benchmark-fma-verify
benchmark-fma-verify: ## Report where FMA launchers and requesters actually landed, and fail if the scenario's placement is inert (set BENCHMARK_NAMESPACE)
	@# The question this answers is "will the next scale-up wake a sleeper or
	@# rebuild from scratch", and it is a placement question: dual-pods binds
	@# node-locally, so a requester on a node with no launcher is a ~50-90s cold
	@# build rather than a ~3s wake. Run it against an FMA somebody else stood up
	@# -- benchmark-run does the same check automatically for its own stack.
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-fma-verify BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@bash hack/benchmark/fma_placement.sh verify $(BENCHMARK_NAMESPACE) \
		"$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml"

.PHONY: benchmark-actuation
benchmark-actuation: ## Measure how fast capacity arrives after a scale-up (set ACTUATION_TARGET=<deployment>; BENCHMARK_NAMESPACE required)
	@# The claim FMA makes is that capacity arrives sooner -- not that tokens are
	@# faster. This measures exactly that and nothing else: scale up, time each
	@# new replica from creation to Ready, scale back, repeat.
	@#
	@# It needs no load generator, no router and no Prometheus, which is the
	@# point. Every load-benchmark failure we hit came from that surrounding
	@# stack rather than from the thing under test: a report converter that could
	@# not read its own input, and a backend still returning 503 when guidellm
	@# validated it. This runs in minutes and is repeatable, so a regression in
	@# actuation shows up as a number instead of being inferred from a slow run.
	@if [ -z "$(BENCHMARK_NAMESPACE)" ] || [ -z "$(ACTUATION_TARGET)" ]; then \
		echo "ERROR: set BENCHMARK_NAMESPACE and ACTUATION_TARGET=<deployment>"; \
		echo "  e.g. make benchmark-actuation BENCHMARK_NAMESPACE=ns ACTUATION_TARGET=my-decode"; \
		exit 1; \
	fi
	@ACTUATION_NS=$(BENCHMARK_NAMESPACE) \
		bash hack/benchmark/actuation.sh $(ACTUATION_TARGET) $(ACTUATION_TRIALS)

.PHONY: benchmark-scenarios
benchmark-scenarios: ## Copy our scenario specs into the llm-d-benchmark clone (idempotent; run automatically by benchmark-run)
	@# Same reasoning as benchmark-patch, and the same trap the workload
	@# profiles already avoid: the clone is gitignored and benchmark-install
	@# re-checks it out to a pinned tag, so a spec edited in place is invisible
	@# to review and gone on the next install. An unversioned maxReplicas sat in
	@# that clone for weeks doing nothing, and a wva.enabled=false left over from
	@# one comparison would have silently disabled autoscaling for every later
	@# run. Ours are versioned in hack/benchmark/scenarios/guides/ and copied in
	@# before every run. Edit them there, never in the clone.
	@if [ ! -d "$(BENCHMARK_REPO_DIR)/config/scenarios/guides" ]; then \
		echo "benchmark-scenarios: no clone at $(BENCHMARK_REPO_DIR); run 'make benchmark-install' first"; \
	else \
		n=0; \
		for f in $(BENCHMARK_SPECS_DIR)/*.yaml; do \
			[ -f "$$f" ] || continue; \
			dest="$(BENCHMARK_REPO_DIR)/config/scenarios/guides/$$(basename $$f)"; \
			sed -e 's/__WARM_REPLICAS__/$(WARM_REPLICAS)/g' -e 's/^\([ 	]*\)gpuMemoryUtilization:.*/\1gpuMemoryUtilization: $(GPU_MEM_UTIL)/' "$$f" > "$$dest"; \
			if grep -q '__WARM_REPLICAS__' "$$dest"; then \
				echo "ERROR: unsubstituted token left in $$dest"; exit 1; \
			fi; \
			echo "  installed scenario $$(basename $$f) (WARM_REPLICAS=$(WARM_REPLICAS), gpuMemoryUtilization=$(GPU_MEM_UTIL))"; \
			n=$$((n+1)); \
		done; \
		[ "$$n" -gt 0 ] || echo "  no specs in $(BENCHMARK_SPECS_DIR)"; \
	fi

.PHONY: benchmark-standup
benchmark-standup: ## Stand up the benchmark environment, then install WVA from this repo (set BENCHMARK_NAMESPACE=<namespace>, MODEL_ID=<model>, IMG=<your build>; BENCHMARK_DIRECT_KEDA=true for controller-free EPP+KEDA autoscaling instead of WVA)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-standup BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@# Standing a guide up into a namespace that already runs FMA silently breaks
	@# it. Both render a PodMonitor named vllm-<model>, and a scenario without
	@# `fma.enabled` renders the port-NAME form, which generates no target for a
	@# launcher (they declare no container ports). The FMA stack keeps serving and
	@# its metrics just stop being collected, with no error anywhere. That is
	@# exactly what happened on pokprod001 -- an FMA guide at 09:25Z, another guide
	@# over the top at 09:26Z -- and it is why a benchmark once showed a variant
	@# flat at one replica through a 155-deep queue.
	@launchers=$$(kubectl get pods -n "$(BENCHMARK_NAMESPACE)" -l app.kubernetes.io/component=launcher --no-headers 2>/dev/null | wc -l | tr -d ' '); \
	if [ "$${launchers:-0}" -gt 0 ]; then \
		echo ""; \
		echo "WARNING: $(BENCHMARK_NAMESPACE) already runs Fast Model Actuation ($$launchers launcher pod(s))."; \
		echo "         This standup renders a PodMonitor named vllm-<model>, the same name the FMA guide uses."; \
		echo "         If this scenario does not set fma.enabled, it will OVERWRITE the FMA-aware one and the"; \
		echo "         launchers stop being scraped -- silently. Metrics for most of the traffic then vanish."; \
		echo "         Either use a scenario with fma.enabled, or stand this up in a clean namespace, or"; \
		echo "         re-apply scraping afterwards:  kubectl apply -k config/fma-launcher-metrics -n $(BENCHMARK_NAMESPACE)"; \
		echo "         See docs/deployment/operations.md, 'FMA launcher pods'."; \
		echo ""; \
	fi
	@# An EPP already serving in this namespace is a stack somebody is using. The
	@# guide renders its own EPP Deployment, Service, InferencePool and a PodMonitor
	@# named vllm-<model>, and applies them unconditionally -- so a second standup
	@# silently REPLACES the first one's objects. The failure is invisible: the pods
	@# keep running and answering while the scrape config and pool membership change
	@# underneath them. Same class of fault as the FMA PodMonitor collision above,
	@# which cost a benchmark that read a variant flat at one replica through a
	@# 155-deep queue. prometheus-adapter already declines to overwrite someone
	@# else's object; the EPP did not.
	@epps=$$(kubectl get deploy -n "$(BENCHMARK_NAMESPACE)" --no-headers 2>/dev/null | grep -c -- '-epp' || true); \
	if [ "$${epps:-0}" -gt 0 ] && [ "$(BENCHMARK_ALLOW_EPP_REUSE)" != "true" ]; then \
		echo ""; \
		echo "ERROR: $(BENCHMARK_NAMESPACE) already runs an EPP ($$epps deployment(s))."; \
		echo "       This standup renders its own EPP, Service, InferencePool and a PodMonitor"; \
		echo "       named vllm-<model>, and applies them over whatever is there. The pods keep"; \
		echo "       running and answering, so the damage is SILENT -- scrape config and pool"; \
		echo "       membership change underneath a stack somebody may be using."; \
		echo ""; \
		echo "       Choose one:"; \
		echo "         - stand up in a clean namespace (recommended), or"; \
		echo "         - benchmark what is already there:  make benchmark-run BENCHMARK_NAMESPACE=$(BENCHMARK_NAMESPACE)"; \
		echo "         - re-render deliberately:           BENCHMARK_ALLOW_EPP_REUSE=true"; \
		echo ""; \
		kubectl get deploy -n "$(BENCHMARK_NAMESPACE)" --no-headers 2>/dev/null | grep -- '-epp' | sed 's/^/         /'; \
		echo ""; \
		exit 1; \
	fi
	@# FMA placement that cannot take effect. Everything under `fma:` is read by
	@# code that first checks whether FMA is a deployed method (standup step_06)
	@# or reads fma.enabled directly (run step_02a_fma_warmup_hotstart), so a
	@# placement or warmup setting under fma.enabled=false is decoration -- and
	@# the run then measures the COLD path (~50-90s per wake) while reporting a
	@# warm config. That is not hypothetical: `launcherNodeSelection.enabled:
	@# true` sat under `enabled: false` in our own scenario, and every
	@# FMA-vs-non-FMA number taken before 2026-08-18 is cold-path because of it.
	@# Checked here, before anything is applied, because the cost of finding out
	@# afterwards is a whole benchmark run.
	@bash hack/benchmark/fma_placement.sh check \
		"$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml"
	@if [ "$(BENCHMARK_DIRECT_KEDA)" = "true" ]; then \
		echo "Direct-KEDA mode: this feature isn't in a released llm-d-benchmark tag yet — upgrading the llm-d-benchmark checkout to '$(BENCHMARK_REPO_REF)' (unreleased)..."; \
		if ! kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then \
			echo "ERROR: KEDA is not installed on this cluster (scaledobjects.keda.sh CRD not found)."; \
			echo "Install KEDA first (e.g. 'make deploy-e2e-infra SCALER_BACKEND=keda ENVIRONMENT=$(ENVIRONMENT)', or your platform's KEDA operator) and re-run."; \
			exit 1; \
		fi; \
		echo "KEDA ScaledObject CRD found — proceeding with direct-KEDA standup (no WVA controller)."; \
	fi
	@if [ -d "$(BENCHMARK_REPO_DIR)" ]; then \
		cd $(BENCHMARK_REPO_DIR) && git checkout -- config/scenarios config/specification config/templates 2>/dev/null || true; \
	fi
	@$(MAKE) benchmark-install BENCHMARK_REPO_REF=$(BENCHMARK_REPO_REF)
	@# AFTER benchmark-install, not before. That target git-checkouts the clone to
	@# the pinned tag, so scenarios installed first are discarded -- which is how a
	@# standup kept deploying the upstream 0.95 while the log said 0.90 had been
	@# installed. Only benchmark-run called this before, and run happens after any
	@# install, which is why the ordering never mattered there.
	@$(MAKE) --no-print-directory benchmark-scenarios
	@cd $(BENCHMARK_REPO_DIR) && git reset --hard origin/$(BENCHMARK_REPO_REF) 2>/dev/null || true
	@if [ -f "$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml" ]; then \
		echo "Copying local scenario: hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml -> $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml"; \
		mkdir -p "$(BENCHMARK_REPO_DIR)/config/scenarios/$$(dirname $(BENCHMARK_SPEC))"; \
		sed -e 's/__WARM_REPLICAS__/$(WARM_REPLICAS)/g' -e 's/^\([ 	]*\)gpuMemoryUtilization:.*/\1gpuMemoryUtilization: $(GPU_MEM_UTIL)/' "$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml" \
		   > "$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml"; \
	fi
	@if [ -f "$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml.j2" ]; then \
		echo "Copying local specification: hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml.j2 -> $(BENCHMARK_REPO_DIR)/config/specification/$(BENCHMARK_SPEC).yaml.j2"; \
		mkdir -p "$(BENCHMARK_REPO_DIR)/config/specification/$$(dirname $(BENCHMARK_SPEC))"; \
		cp "$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml.j2" \
		   "$(BENCHMARK_REPO_DIR)/config/specification/$(BENCHMARK_SPEC).yaml.j2"; \
	fi
	@# Lives in a script, not inline here, so its branches can actually be run.
	@# Inline it was reachable only by driving a whole standup, which is why it
	@# shipped reviewed-but-never-executed -- and why the "ours from an earlier
	@# run" case went unnoticed reporting "something else created it".
	@if [ "$(BENCHMARK_SKIP_PROMETHEUS_ADAPTER)" = "true" ]; then \
		bash hack/benchmark/pa_clusterrole.sh stub $(WVA_MONITORING_NAMESPACE); \
	fi
	@echo "Injecting PYTORCH_ALLOC_CONF, decode replicas, and KEDA config into scenario YAML ($(BENCHMARK_SPEC).yaml)..."
	@sed -i.bak 's/extraEnvVars: \[\]/extraEnvVars:\n        - name: PYTORCH_ALLOC_CONF\n          value: "expandable_segments:True"/' \
		$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml
	@sed -i.bak 's/replicas: 2$$/replicas: $(BENCHMARK_DECODE_REPLICAS)/' \
		$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml
	@# The replica bounds and scaling periods, set by PATH rather than by
	@# pattern-matching indented text.
	@#
	@# This was an awk block anchored on `scaledObject:` and `periodSeconds:
	@# 180`/`300`. Neither string occurs in the scenario -- not in v0.7.8, and
	@# not in v0.7.0 either (checked with grep -c: zero in both). So
	@# BENCHMARK_KEDA_MIN_REPLICAS, BENCHMARK_KEDA_MAX_REPLICAS and both period
	@# variables silently did nothing, for every run this repo has ever made.
	@# The bounds live at .wva.hpa.*, and the periods under
	@# .wva.hpa.behavior.scale{Up,Down}.policies[].
	@#
	@# yq addresses them directly, so a key that moves again fails loudly at the
	@# path instead of quietly matching no lines.
	@# One yq call per key, deliberately. A `\` continuation inside the
	@# single-quoted expression is NOT removed by the shell -- it reaches yq as
	@# literal backslash-newline and dies with
	@#   Error: 1:65: lexer: invalid input text "\\\n        (.scen..."
	@# Separate calls also name which key failed, instead of one opaque error.
	@yq -i '(.scenario[] | select(has("wva")) | .wva.hpa.minReplicas) = $(BENCHMARK_KEDA_MIN_REPLICAS)' $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml
	@yq -i '(.scenario[] | select(has("wva")) | .wva.hpa.maxReplicas) = $(BENCHMARK_KEDA_MAX_REPLICAS)' $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml
	@# The periods are applied only when > 0. BENCHMARK_KEDA_SCALE_UP_PERIOD
	@# defaults to 0, and HPA rejects periodSeconds: 0 ("must be greater than
	@# zero") -- so writing the default would produce a ScaledObject the API
	@# refuses. That default was harmless only while the old awk silently matched
	@# nothing; applying it for real makes 0 mean "keep the scenario's own value".
	@if [ "$(BENCHMARK_KEDA_SCALE_UP_PERIOD)" -gt 0 ] 2>/dev/null; then \
		yq -i '(.scenario[] | select(has("wva")) | .wva.hpa.behavior.scaleUp.policies[].periodSeconds) = $(BENCHMARK_KEDA_SCALE_UP_PERIOD)' \
			$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml; \
	fi
	@if [ "$(BENCHMARK_KEDA_SCALE_DOWN_PERIOD)" -gt 0 ] 2>/dev/null; then \
		yq -i '(.scenario[] | select(has("wva")) | .wva.hpa.behavior.scaleDown.policies[].periodSeconds) = $(BENCHMARK_KEDA_SCALE_DOWN_PERIOD)' \
			$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml; \
	fi
	@# Stabilization windows. Applied when NON-EMPTY, not when > 0: unlike
	@# periodSeconds, 0 is a legal and meaningful value here ("act now").
	@if [ -n "$(BENCHMARK_KEDA_SCALE_UP_STABILIZATION)" ]; then \
		yq -i '(.scenario[] | select(has("wva")) | .wva.hpa.behavior.scaleUp.stabilizationWindowSeconds) = $(BENCHMARK_KEDA_SCALE_UP_STABILIZATION)' \
			$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml; \
	fi
	@if [ -n "$(BENCHMARK_KEDA_SCALE_DOWN_STABILIZATION)" ]; then \
		yq -i '(.scenario[] | select(has("wva")) | .wva.hpa.behavior.scaleDown.stabilizationWindowSeconds) = $(BENCHMARK_KEDA_SCALE_DOWN_STABILIZATION)' \
			$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml; \
	fi
	@echo "Scaling behaviour: scaleUp period=$(BENCHMARK_KEDA_SCALE_UP_PERIOD)s stabilization=$(BENCHMARK_KEDA_SCALE_UP_STABILIZATION)s, scaleDown period=$(BENCHMARK_KEDA_SCALE_DOWN_PERIOD)s stabilization=$(if $(BENCHMARK_KEDA_SCALE_DOWN_STABILIZATION),$(BENCHMARK_KEDA_SCALE_DOWN_STABILIZATION),scenario default)"
	@echo "KEDA bounds set: min=$(BENCHMARK_KEDA_MIN_REPLICAS) max=$(BENCHMARK_KEDA_MAX_REPLICAS), periods up=$(BENCHMARK_KEDA_SCALE_UP_PERIOD)s down=$(BENCHMARK_KEDA_SCALE_DOWN_PERIOD)s"
	@# Turn OFF the scenario's own WVA install. This repo installs WVA from
	@# deploy/ in benchmark-deploy-wva, and benchmark-deploy-wva refuses to run
	@# beside a controller it did not install -- two controllers decide the same
	@# replica count. The scenario's copy is also the RELEASED chart
	@# (ghcr.io/llm-d/...), which rejects --external-scaler-bind-address, and it
	@# pulls kustomize resources from
	@# github.com/llm-d/llm-d/guides/workload-autoscaling/wva-config/platform/ocp?ref=main,
	@# a path that does not currently exist -- standup died there on pokprod.
	@# Restored with the rest of the scenario from the .bak below.
	@yq -i '(.scenario[] | select(has("wva")) | .wva.enabled) = false' \
		$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml && \
		echo "Scenario's own WVA install disabled (this repo installs it from deploy/)."
	@# Run the harness as root, or it cannot start on OpenShift.
	@#
	@# Its entrypoint copies its scripts into /usr/local/bin and chmods them.
	@# v0.7.0's pod template hardcoded runAsUser: 0 so that always worked;
	@# v0.7.8 made it conditional on harness.runAsUser being set, and a scenario
	@# that does not set it now gets no securityContext at all. OpenShift then
	@# assigns a UID from the namespace range and the copy dies with
	@#   cp: cannot create regular file '/usr/local/bin/...': Permission denied
	@# followed by analyze_results failing on a results.json that was never
	@# written. Observed on pokprod001 with the v0.7.8 template.
	@#
	@# Granting anyuid to the harness ServiceAccount does NOT fix it there:
	@# openshift-ai-llminferenceservice-multi-node-scc has priority 11 against
	@# anyuid's 10, so it wins for any SA that can use it and forces
	@# MustRunAsRange. Asking for UID 0 is what makes that SCC unable to admit
	@# the pod, so selection falls to anyuid and the harness gets root.
	@yq -i '(.scenario[] | select(has("harness")) | .harness.runAsUser) = $(BENCHMARK_HARNESS_RUN_AS_USER)' \
		$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml && \
		echo "Harness runAsUser=$(BENCHMARK_HARNESS_RUN_AS_USER) (v0.7.8 stopped forcing it; OpenShift needs it)."
	@# Pin the harness IMAGE to the ref we checked out.
	@#
	@# defaults.yaml hardcodes llm-d-benchmark_version: v0.7.0 -- in v0.7.8 as
	@# well as v0.7.0 -- so the image never follows the checkout. The scripts
	@# always do: they are copied from the checkout into the pod. v0.7.8's
	@# guidellm-llm-d-benchmark.sh calls `guidellm run`, and the v0.7.0 image's
	@# guidellm only has `benchmark`, so every treatment died with
	@#   Error: No such command 'run'.
	@# after the runAsUser fix let those scripts land -- the permission failure
	@# had been hiding the mismatch by leaving the image's own scripts in place.
	@$(eval BENCHMARK_IMAGE_TAG ?= $(BENCHMARK_REPO_REF))
	@yq -i '(.scenario[] | select(has("common")) | .common.images.benchmark.tag) = "$(BENCHMARK_IMAGE_TAG)"' \
		$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml && \
		echo "Harness image tag=$(BENCHMARK_IMAGE_TAG) (defaults.yaml pins v0.7.0 regardless of ref)."
	$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) standup \
		-p $(BENCHMARK_NAMESPACE) \
		$(if $(BENCHMARK_MODEL_ID),-m $(BENCHMARK_MODEL_ID),) \
		$(if $(filter true,$(BENCHMARK_MONITORING)),--monitoring,); \
	rc=$$?; \
	mv $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml.bak \
	   $(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml; \
	if [ $$rc -eq 0 ] && [ "$(BENCHMARK_MONITORING)" = "true" ]; then \
		echo "Enabling user-workload monitoring for namespace $(BENCHMARK_NAMESPACE)..."; \
		oc label namespace $(BENCHMARK_NAMESPACE) openshift.io/user-workload-monitoring=enabled --overwrite 2>/dev/null && \
		echo "✅ Monitoring label applied. Prometheus will begin scraping ServiceMonitors in this namespace."; \
	fi; \
	exit $$rc
	@if [ "$(BENCHMARK_DIRECT_KEDA)" = "true" ]; then \
		echo "Direct-KEDA mode: not installing WVA (that is the point of this mode)."; \
	elif [ "$(BENCHMARK_WVA_DEPLOY)" != "true" ]; then \
		echo "BENCHMARK_WVA_DEPLOY=$(BENCHMARK_WVA_DEPLOY): not installing WVA. The model servers will not be autoscaled."; \
	else \
		$(MAKE) benchmark-deploy-wva BENCHMARK_NAMESPACE=$(BENCHMARK_NAMESPACE); \
	fi
	@# A standup reinstalls FMA from llm-d-benchmark, which reintroduces both
	@# defects benchmark-fma-fixups exists to correct: the launcher SA that cannot
	@# patch pods, and controllers old enough to drop the reconcile that would
	@# clear the resulting stale binding. Re-apply them here so standing the
	@# environment up does not quietly restore a configuration in which FMA
	@# benchmarking yields no data. Skipped when FMA is not present.
	@if kubectl get deploy -n $(BENCHMARK_NAMESPACE) -o name 2>/dev/null | grep -q dual-pods-controller; then \
		$(MAKE) --no-print-directory benchmark-fma-fixups BENCHMARK_NAMESPACE=$(BENCHMARK_NAMESPACE); \
	fi

	@# The model servers a standup deploys carry no preStop hook and the default
	@# 30s grace period, so every scale-down during the run cuts the requests in
	@# flight. Measured on a real run: 39 ClientPayloadError failures in four
	@# bursts, each ending 25-30s after one of the four pod removals.
	@#
	@# Those failures are an artefact of how the harness deploys, not a property
	@# of the autoscaler under test -- and they land in the same TTFT percentiles
	@# and error counts the run exists to measure. A comparison between two
	@# scaling policies is not measuring scaling if one of them happened to remove
	@# more pods.
	@#
	@# Patched rather than reported, because unlike an operator's workloads these
	@# are OURS: this target deployed them. It costs one rollout, taken here and
	@# waited for, so the run starts against pods that are already settled rather
	@# than mid-restart.
	@echo "Making the model servers drain on scale-down..."
	@# SCOPE and WVA_NS are PINNED, not inherited. workload-patch honours WVA_SCOPE
	@# from the environment, and anyone running a cluster-scoped install has that
	@# exported -- which would make this line walk every namespace on the cluster
	@# and apply a live patch, restarting model servers that have nothing to do
	@# with the benchmark. The justification for applying rather than emitting is
	@# that these workloads are ours; that holds only inside BENCHMARK_NAMESPACE.
	@$(MAKE) --no-print-directory workload-patch \
		NAMESPACE=$(BENCHMARK_NAMESPACE) WVA_NS=$(BENCHMARK_NAMESPACE) \
		SCOPE=namespace WVA_SCOPE=namespace WVA_DEFAULT_SO_NS=$(BENCHMARK_NAMESPACE) \
		WVA_WORKLOAD_PATCH_APPLY=true \
		WVA_WORKLOAD_PATCH_FILE=$(BENCHMARK_WORKSPACE)/wva-workload-patch.yaml || \
		echo "  WARNING: could not patch the model servers; scale-downs in this run will truncate in-flight requests."
	@for d in $$(kubectl get deploy -n $(BENCHMARK_NAMESPACE) -o name 2>/dev/null | grep -E 'decode|prefill'); do \
		kubectl rollout status $$d -n $(BENCHMARK_NAMESPACE) --timeout=$(BENCHMARK_DRAIN_ROLLOUT_TIMEOUT)s || \
			echo "  WARNING: $$d did not settle; the run may start against restarting pods."; \
	done
	@# The model cache, reported from what the cluster ACTUALLY bound rather than
	@# from what the scenario asked for. Whether the engine downloads outside it is
	@# workload-patch's question, answered above; this is the other one, which only
	@# the claim can answer: can more than one node mount it?
	@#
	@# It is checked here because it fails LATE and quietly. An RWO cache stands up
	@# perfectly -- one replica, one node, everything green -- and then the first
	@# scale-up leaves the new pod Pending on a volume it cannot attach. The run
	@# then measures an autoscaler that appears not to work.
	@# `.SHELLFLAGS = -ec`, so a bare `x=$$(kubectl ...)` assignment carries the
	@# command status and `set -e` kills the whole recipe -- silently, because
	@# 2>/dev/null ate the reason. That aborted benchmark-standup at the very end
	@# of a long run if a token could list PVCs but not get one by name (separate
	@# RBAC verbs). Every substitution below therefore ends in `|| true`.
	@#
	@# It also reports what BOUND, not what was requested: a claim can ask for RWX
	@# and sit Pending forever on a cluster with no RWX class, and the accessModes
	@# field still reads ReadWriteMany because it is the request.
	@echo "Model cache:"
	@found=0; listed=$$(kubectl get pvc -n $(BENCHMARK_NAMESPACE) \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>&1) || listed="__ERR__$$listed"; \
	case "$$listed" in \
		__ERR__*) \
			echo "  WARNING: could not list PersistentVolumeClaims in $(BENCHMARK_NAMESPACE)."; \
			echo "           This is NOT a report that there is no cache -- it is a report that"; \
			echo "           nobody could look: $$(printf '%s' "$${listed#__ERR__}" | tail -1)"; \
			found=-1 ;; \
	esac; \
	if [ "$$found" = 0 ]; then \
		for pvc in $$(printf '%s\n' "$$listed" | grep -iE 'model|weight|cache' || true); do \
			found=1; \
			modes=$$(kubectl get pvc $$pvc -n $(BENCHMARK_NAMESPACE) -o jsonpath='{.spec.accessModes[*]}' 2>/dev/null || true); \
			phase=$$(kubectl get pvc $$pvc -n $(BENCHMARK_NAMESPACE) -o jsonpath='{.status.phase}' 2>/dev/null || true); \
			size=$$(kubectl get pvc $$pvc -n $(BENCHMARK_NAMESPACE) -o jsonpath='{.status.capacity.storage}' 2>/dev/null || true); \
			sc=$$(kubectl get pvc $$pvc -n $(BENCHMARK_NAMESPACE) -o jsonpath='{.spec.storageClassName}' 2>/dev/null || true); \
			echo "  $$pvc  $${size:-<unbound>}  [$${modes:-?}]  $${phase:-?}  class=$${sc:-<cluster default>}"; \
			if [ "$$phase" != Bound ]; then \
				echo "  WARNING: $$pvc is $${phase:-unreadable}, not Bound. Nothing can mount it, so the"; \
				echo "           model servers will not start. A ReadWriteMany request on a cluster"; \
				echo "           with no RWX StorageClass is accepted and then never binds."; \
			fi; \
			case "$$modes" in \
				*ReadWriteMany*|*ReadOnlyMany*) ;; \
				*) echo "  WARNING: $$pvc is $$modes, so it can be mounted on ONE NODE only."; \
				   echo "           The stack will stand up and the first scale-up will not: a decode"; \
				   echo "           replica scheduled on another node stays Pending on the volume, so this"; \
				   echo "           run cannot measure scaling. Give the model cache an RWX StorageClass."; ;; \
			esac; \
		done; \
	fi; \
	if [ "$$found" = 0 ]; then \
		echo "  WARNING: no model cache PVC found in $(BENCHMARK_NAMESPACE)."; \
		echo "           Every replica this run adds fetches its weights from Hugging Face, which"; \
		echo "           is charged to scale-up latency and fails outright without egress."; \
		echo "           Create one:  make model-cache NAMESPACE=$(BENCHMARK_NAMESPACE)"; \
	fi

## Install WVA from THIS repo into the benchmark namespace and register the model
## servers with it. Split out of benchmark-standup so a run whose stack is already
## up can (re)install the autoscaler without redeploying the model servers.
##
## IMG decides what is measured. It defaults to $(IMAGE_TAG_BASE)/...:$(IMG_TAG),
## a REGISTRY image built from this branch — which is NOT automatically the
## working copy. Pass IMG=<your build> after changing controller code, or the run
## measures whatever was last pushed under that tag. Benchmarking the wrong
## binary is what this target exists to stop happening silently, so it says which
## image it is installing.
.PHONY: benchmark-deploy-wva
benchmark-deploy-wva: ## Install WVA from deploy/ into BENCHMARK_NAMESPACE (namespace scope) and create its ScaledObjects. Set IMG=<your build>.
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-deploy-wva BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@# A scenario that still installs WVA itself (the upstream ones do) would leave
	@# two controllers optimizing the same namespace and writing the same replica
	@# counts. Refuse rather than produce a benchmark of the two of them fighting.
	@existing=$$(kubectl get deploy -n $(BENCHMARK_NAMESPACE) \
		-l app.kubernetes.io/name=workload-variant-autoscaler \
		-o name 2>/dev/null | grep -v '/wva-controller-manager$$' || true); \
	if [ -n "$$existing" ]; then \
		echo "ERROR: a WVA controller is already running in $(BENCHMARK_NAMESPACE):"; \
		echo "  $$existing"; \
		echo "It is not this install. Two controllers on one namespace both decide the"; \
		echo "same replica counts. Either remove it, or re-run with"; \
		echo "BENCHMARK_WVA_DEPLOY=false to benchmark that one instead."; \
		exit 1; \
	fi
	@# A controller THIS repo installed is excluded from the check above, so the
	@# install below re-applies over it and moves it to $(IMG). That is right when
	@# you are iterating on your own build and wrong when the namespace belongs to a
	@# run somebody else is watching -- the image changes underneath them with no
	@# warning. Say what is about to happen, and name the way out.
	@ours=$$(kubectl get deploy -n $(BENCHMARK_NAMESPACE) wva-controller-manager \
		-o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true); \
	if [ -n "$$ours" ]; then \
		echo ""; \
		echo "NOTE: $(BENCHMARK_NAMESPACE) already runs a WVA controller from this repo."; \
		echo "      running: $$ours"; \
		echo "      about to: $(IMG)"; \
		if [ "$$ours" != "$(IMG)" ]; then \
			echo "      This REPLACES the running controller's image. If someone is using this"; \
			echo "      namespace, reuse it instead:  make benchmark-standup ... BENCHMARK_WVA_DEPLOY=false"; \
		else \
			echo "      Same image -- this is a no-op re-apply."; \
		fi; \
		echo ""; \
	fi
	@echo ""
	@echo "Installing WVA from this repo into namespace $(BENCHMARK_NAMESPACE) (scope: namespace)"
	@echo "  image:  $(IMG)"
	@echo "  target: $(BENCHMARK_WVA_TARGET)  (ENVIRONMENT=$(ENVIRONMENT))"
	@echo ""
	@# NAMESPACE as well as WVA_NS. They answer different questions -- what WVA
	@# MANAGES versus where the controller is installed -- and NAMESPACE became
	@# mandatory in namespace scope, so passing only WVA_NS made the install
	@# refuse and print 105 candidate namespaces. Here the two are the same
	@# namespace by construction: the benchmark stands the model up and installs
	@# the controller beside it.
	@target=$(BENCHMARK_WVA_TARGET); \
	if [ "$(ENVIRONMENT)" != "openshift" ] && \
	   kubectl api-resources --api-group=route.openshift.io -o name >/dev/null 2>&1 && \
	   [ -n "$$(kubectl api-resources --api-group=route.openshift.io -o name 2>/dev/null)" ]; then \
		echo "OpenShift detected; installing with the OpenShift overlay (cluster-monitoring-view)."; \
		target=deploy-wva-on-openshift; \
	fi; \
	$(MAKE) $$target \
		WVA_NS=$(BENCHMARK_NAMESPACE) \
		NAMESPACE=$(BENCHMARK_NAMESPACE) \
		WVA_SCOPE=namespace \
		IMG=$(IMG) \
		$(if $(BENCHMARK_PROMETHEUS_URL),PROMETHEUS_URL=$(BENCHMARK_PROMETHEUS_URL),)
	@# The ScaledObject is the registration: WVA has no watch and no listing, so
	@# without one it is never called and scales nothing. Applied as a second step
	@# rather than via the install's WVA_DEFAULT_SO so the benchmark's replica
	@# bounds reach so_discover.
	@# Plan, then adopt, then apply. A benchmark scenario may bring its own
	@# ScaledObjects, and the plan marks those "no" by default — which would leave
	@# them pointed at whatever scaled them before, silently benchmarking something
	@# other than WVA. Adopting repoints them rather than adding a second object on
	@# the same target. Entries whose model could not be read keep "no": adopting
	@# one would register a variant of a model nobody runs.
	WVA_DEFAULT_SO_MIN=$(BENCHMARK_KEDA_MIN_REPLICAS) \
	WVA_DEFAULT_SO_MAX=$(BENCHMARK_KEDA_MAX_REPLICAS) \
	$(MAKE) scaledobjects-plan \
		WVA_NS=$(BENCHMARK_NAMESPACE) \
		WVA_SCOPE=namespace \
		WVA_DEFAULT_SO_NS=$(BENCHMARK_NAMESPACE) \
		WVA_DEFAULT_SO_PLAN=$(BENCHMARK_SO_PLAN)
	@yq -i '(.plan[] | select(.apply == "no" and .modelID != "") | .apply) = "adopt"' $(BENCHMARK_SO_PLAN)
	$(MAKE) scaledobjects-apply \
		WVA_NS=$(BENCHMARK_NAMESPACE) \
		WVA_SCOPE=namespace \
		WVA_DEFAULT_SO_PLAN=$(BENCHMARK_SO_PLAN)

.PHONY: benchmark-run
benchmark-run: ## Run a single benchmark workload (set BENCHMARK_NAMESPACE=<namespace>, MODEL_ID=<model>, BENCHMARK_HARNESS=guidellm|inference-perf)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-run BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@# Cheap and idempotent, and the clone is gitignored: a checkout done by
	@# anything other than benchmark-install would otherwise leave a run using
	@# an unpatched harness, and the symptom of that (a valid run reported
	@# FAILED, EPP metrics silently "?") reads as a cluster problem, not a
	@# tooling one.
	@$(MAKE) --no-print-directory benchmark-patch
	@$(MAKE) --no-print-directory benchmark-scenarios
	@mkdir -p "$(BENCHMARK_SCENARIOS_DIR)"
	@if [ "$(BENCHMARK_DIRECT_KEDA)" = "true" ] && [ -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)" ]; then \
		echo "Injecting external model endpoint for direct-KEDA mode..."; \
		sed -i.bak 's|base_url: .*|base_url: http://infra-llmdbench-inference-gateway.$(BENCHMARK_NAMESPACE).svc.cluster.local:80|' \
			"$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)"; \
		rm -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).bak"; \
	fi
	@# Fetch workload from inference-perf catalog if not found locally and harness is inference-perf
	@if [ "$(BENCHMARK_HARNESS)" = "inference-perf" ] && [ ! -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)" ] && [ ! -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).in" ]; then \
		echo "Fetching $(BENCHMARK_WORKLOAD) from inference-perf workload-catalog..."; \
		if curl -sfL "https://raw.githubusercontent.com/kubernetes-sigs/inference-perf/main/workload-catalog/$(BENCHMARK_WORKLOAD)/inference-perf.yaml" \
			-o "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)"; then \
			echo "Successfully fetched $(BENCHMARK_WORKLOAD)"; \
		else \
			echo "ERROR: Could not fetch $(BENCHMARK_WORKLOAD) from inference-perf workload-catalog"; \
			echo "Available workloads: interactive-chat, code-generation, deep-research, reasoning, batch-summarization-rag, batch-synthetic-data-generation"; \
			exit 1; \
		fi; \
	fi
	@# The profiles in this repo are named <workload>.yaml.in, and neither branch
	@# below looked for that: one wanted <workload>.in, the other bare
	@# <workload>. So nothing was ever copied, and the run died with
	@#   Profile 'prefill_heavy.yaml' not found in .../workload/profiles/guidellm
	@# -- naming the file it wanted while the file sat un-copied in
	@# test/benchmark/scenarios. The CLI is invoked with -w <workload>.yaml, so
	@# It must land as <workload>.yaml.in and NOT as <workload>.yaml. Every
	@# profile the harness ships is .yaml.in only; profile_renderer.py
	@# substitutes the REPLACE_ENV_* tokens in the template and produces the
	@# .yaml itself. Copying a .yaml too shadows that render with an unsubstituted
	@# file, and guidellm then dies on every option at once:
	@#   Error: Invalid value for '--backend' / '--model' / '--target' ...:
	@# with the values empty. The CLI is still invoked as -w <workload>.yaml; it
	@# resolves that to the template, exactly as it does for its own profiles.
	@# __REQUEST_RATE__ is OUR token, substituted here, before the file reaches the
	@# harness. The harness's own REPLACE_ENV_* tokens resolve from a registry in
	@# llm-d-benchmark, which benchmark-install re-checks-out to a pinned tag, so
	@# a token added there would not survive the next install.
	@#
	@# A template that still holds the token produces invalid YAML rather than a
	@# silent fallback: a run at the wrong request rate looks like a result, and
	@# would be one nobody could reproduce.
	@#
	@# Substituted by NAME, not by matching `rate:` lines. Staged ladders (bursty,
	@# sharegpt) carry several rates whose SHAPE is the scenario, and a pattern
	@# that rewrote every rate line would flatten them into one number.
	@if [ -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).yaml.in" ]; then \
		echo "Copying local workload $(BENCHMARK_WORKLOAD).yaml.in to the $(BENCHMARK_HARNESS) harness (REQUEST_RATE=$(REQUEST_RATE))..."; \
		sed -e 's/__REQUEST_RATE__/$(REQUEST_RATE)/g' -e 's/__MAX_DURATION__/$(MAX_DURATION)/g' \
		   "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).yaml.in" \
		   > "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).yaml.in"; \
		rm -f "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).yaml"; \
	elif [ -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).in" ]; then \
		cp "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).in" \
		   "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).in"; \
		cp "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD).in" \
		   "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD)"; \
	elif [ -f "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)" ]; then \
		echo "Copying local workload from $(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD) to harness..."; \
		cp "$(BENCHMARK_SCENARIOS_DIR)/$(BENCHMARK_WORKLOAD)" \
		   "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).yaml"; \
		if [ -n "$(BENCHMARK_MODEL_ID)" ]; then \
			echo "Injecting MODEL_ID=$(BENCHMARK_MODEL_ID) into workload profile..."; \
			sed -i.bak 's|model_name: .*|model_name: $(BENCHMARK_MODEL_ID)|' \
				"$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).yaml"; \
			rm -f "$(BENCHMARK_REPO_DIR)/workload/profiles/$(BENCHMARK_HARNESS)/$(BENCHMARK_WORKLOAD).yaml.bak"; \
		fi; \
	fi
	@# The harness UID has to be set HERE too, not only in benchmark-standup.
	@# run renders its own plan from the scenario, and standup restores the
	@# scenario from its .bak when it finishes -- so by the time the harness pod
	@# is created the patch is gone, config.yaml carries harness.runAsUser: null,
	@# and the pod is admitted with a namespace UID again. Observed: standup
	@# applied it, the run twenty minutes later did not.
	@yq -i '(.scenario[] | select(has("harness")) | .harness.runAsUser) = $(BENCHMARK_HARNESS_RUN_AS_USER)' \
		$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml 2>/dev/null || true
	@# Same reasoning as in benchmark-standup: run renders its own plan, and the
	@# image must match the ref whose scripts get copied into the pod.
	@yq -i '(.scenario[] | select(has("common")) | .common.images.benchmark.tag) = "$(if $(BENCHMARK_IMAGE_TAG),$(BENCHMARK_IMAGE_TAG),$(BENCHMARK_REPO_REF))"' \
		$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml 2>/dev/null || true
	@# Sample replica counts ourselves for the duration of the run.
	@#
	@# The harness writes replica_status_timeseries.json, and on every FMA run
	@# measured it came back with snapshots but NO controllers: collect_metrics.sh
	@# filters them by comparing LLMDBENCH_HARNESS_STACK_NAME -- a stack name,
	@# "inference-scheduling-wva" -- against the llm-d.ai/model label,
	@# "qwen-qwe-...". They never match, so every controller is dropped and the
	@# run reports no replicas and no cost. It cannot be corrected from here:
	@# run_only.sh writes that variable into the harness pod from
	@# endpoint_stack_name, which is also what it passes to --stack.
	@#
	@# Without this, a two-variant FMA run yields latency numbers and no way to
	@# normalise them by the capacity that produced them, which is the only
	@# comparison worth making.
	@# Do not start the load generator before the model is actually servable.
	@# Deployment readyReplicas is not that condition: the EndpointSlice, the
	@# InferencePool's view of it and the router all sit between them. guidellm
	@# validates its backend before generating any load, so a 503 in that window
	@# kills every worker at once and the run produces NO results.json -- every
	@# metric then reports "?", which reads as a tooling bug rather than "the
	@# model was not up yet". That cost two full runs before it was diagnosed.
	@# WAIT_SERVING=false skips it for a target that has no router.
	@if [ "$(WAIT_SERVING)" != "false" ]; then \
		bash hack/benchmark/wait_serving.sh $(BENCHMARK_NAMESPACE) $(WAIT_SERVING_TIMEOUT); \
	fi
	@# Placement decides warm vs cold, so check it against the CLUSTER before
	@# generating load -- the static check at standup cannot see whether a node
	@# actually carries the pin label or whether the requesters landed beside the
	@# warm pool. A selector matching zero nodes constrains nothing and reports
	@# success, which is how a cold run masquerades as a warm one. No-op when the
	@# namespace runs no launchers.
	@bash hack/benchmark/fma_placement.sh verify $(BENCHMARK_NAMESPACE) \
		"$(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml"
	@rm -f /tmp/wva_replica_samples.json /tmp/wva_replica_samples.json.pid
	@bash hack/benchmark/sample_replicas.sh start $(BENCHMARK_NAMESPACE) /tmp/wva_replica_samples.json || true
	-$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) run \
		-p $(BENCHMARK_NAMESPACE) \
		-l $(BENCHMARK_HARNESS) \
		-w $(BENCHMARK_WORKLOAD).yaml \
		$(if $(BENCHMARK_MODEL_ID),-m $(BENCHMARK_MODEL_ID),) \
		$(if $(filter true,$(BENCHMARK_MONITORING)),--monitoring,) \
		--wait-timeout $(BENCHMARK_WAIT_TIMEOUT)
	@# Stopped and filed even when the run above failed -- a run that errored in a
	@# post-processing step still produced measurements worth reading, and every
	@# FMA run so far has ended that way.
	@bash hack/benchmark/sample_replicas.sh stop /tmp/wva_replica_samples.json || true
	@LATEST=$$(ls -td $(BENCHMARK_WORKSPACE)/$${USER}-*/results/$(BENCHMARK_HARNESS)-*_* 2>/dev/null | head -1); \
	if [ -n "$$LATEST" ] && [ -s /tmp/wva_replica_samples.json ]; then \
		mkdir -p "$$LATEST/metrics/processed"; \
		cp /tmp/wva_replica_samples.json "$$LATEST/metrics/processed/wva_replica_samples.json"; \
		echo "Replica samples filed in $$LATEST/metrics/processed/wva_replica_samples.json"; \
	fi
	@echo ""
	@echo "========================================="
	@echo "  Generating benchmark report..."
	@echo "========================================="
	@$(MAKE) benchmark-report
	@$(MAKE) benchmark-plot-two-variant || true

## The one benchmark to run after installing: decode-heavy at 10 req/s for five
## minutes, then a dashboard snapshot over exactly that window and a pointer to it.
## Answers "is WVA actually scaling this?" -- not "how fast is this model".
.PHONY: benchmark-smoke
benchmark-smoke: ## Post-install check: drive decode-heavy load, snapshot the dashboard, print where it is (NAMESPACE=<ns>)
	@bash hack/benchmark/smoke.sh $(if $(NAMESPACE),$(NAMESPACE),$(BENCHMARK_NAMESPACE))

## Generate a markdown table from the latest benchmark results
.PHONY: benchmark-report
benchmark-report: ## Generate a markdown table from the latest benchmark results
	@LATEST_DIR=$$(ls -td $(BENCHMARK_WORKSPACE)/$${USER}-*/results/$(BENCHMARK_HARNESS)-*_* 2>/dev/null | head -1); \
	if [ -z "$$LATEST_DIR" ]; then \
		echo "ERROR: No benchmark results found in $(BENCHMARK_WORKSPACE)"; \
		exit 1; \
	fi; \
	echo "Results directory: $$LATEST_DIR"; \
	echo ""; \
	if [ -n "$(BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX)" ]; then \
		python3 $(CURDIR)/hack/benchmark/postprocess.py \
			--secondary-suffix $(BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX) \
			--scenario-yaml $(CURDIR)/hack/benchmark/scenarios/$(BENCHMARK_SPEC).yaml \
			--variant-config $(VARIANT_CONFIG) \
			$$LATEST_DIR; \
	else \
		python3 $(CURDIR)/hack/benchmark/postprocess.py $$LATEST_DIR; \
	fi

BENCHMARK_TWO_VARIANT_SECONDARY_SUFFIX ?= v2

.PHONY: benchmark-plot-two-variant
benchmark-plot-two-variant: ## Plot two-variant replica/latency/throughput graph from the latest results (no-op for single-variant runs)
	@LATEST_DIR=$$(ls -td $(BENCHMARK_WORKSPACE)/$${USER}-*/results/$(BENCHMARK_HARNESS)-*_* 2>/dev/null | head -1); \
	if [ -z "$$LATEST_DIR" ]; then \
		echo "No benchmark results found, skipping two-variant plot"; \
		exit 0; \
	fi; \
	python3 $(CURDIR)/hack/benchmark/plot_two_variant_pipeline.py \
		$$LATEST_DIR && \
	echo "Two-variant plot: $$LATEST_DIR/metrics/graphs/two_variant_v2_full_pipeline.png"

VARIANT_CONFIG ?= $(CURDIR)/hack/benchmark/scenarios/guides/variants/v2-tp1-cheaper.yaml
WVA_V2_SATURATION_CONFIGMAP ?= $(CURDIR)/hack/benchmark/scenarios/wva_threshold/wva_saturation_v2_config.yaml
# The name deploy/ installs: config/base names it controller-manager and every
# overlay applies namePrefix: wva-. The old value here was the deleted Helm
# chart's name, so every restart-controller call failed with NotFound.
WVA_CONTROLLER_DEPLOY ?= deploy/wva-controller-manager
WVA_ROLLOUT_TIMEOUT ?= 120s
WVA_MONITORING_NAMESPACE ?= workload-variant-autoscaler-monitoring

.PHONY: benchmark-add-variant
benchmark-add-variant: ## Add a secondary WVA variant to the running benchmark (set BENCHMARK_NAMESPACE=<namespace>, optional VARIANT_CONFIG=<path>)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-add-variant BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	python3 $(CURDIR)/hack/benchmark/add_variant.py \
		-n $(BENCHMARK_NAMESPACE) \
		--config $(VARIANT_CONFIG)
	@# The variant is a new model server, deployed the same way and with the same
	@# gap: no preStop hook. Without this the second model truncates streams on
	@# every scale-down while the first one does not, which is a difference
	@# between the two arms of whatever the run is comparing.
	@$(MAKE) --no-print-directory workload-patch \
		NAMESPACE=$(BENCHMARK_NAMESPACE) WVA_NS=$(BENCHMARK_NAMESPACE) \
		SCOPE=namespace WVA_SCOPE=namespace WVA_DEFAULT_SO_NS=$(BENCHMARK_NAMESPACE) \
		WVA_WORKLOAD_PATCH_APPLY=true \
		WVA_WORKLOAD_PATCH_FILE=$(BENCHMARK_WORKSPACE)/wva-workload-patch.yaml || \
		echo "  WARNING: the variant was added but not patched; its scale-downs will truncate in-flight requests."
	@# Waited for, for the same reason the standup waits: a patch replaces the pod
	@# template, so everything it touched is rolling when it returns. Adding a
	@# variant to a RUNNING benchmark and returning mid-restart puts the restart
	@# inside the measurement window.
	@for d in $$(kubectl get deploy -n $(BENCHMARK_NAMESPACE) -o name 2>/dev/null | grep -E 'decode|prefill'); do \
		kubectl rollout status $$d -n $(BENCHMARK_NAMESPACE) --timeout=$(BENCHMARK_DRAIN_ROLLOUT_TIMEOUT)s || \
			echo "  WARNING: $$d did not settle."; \
	done

.PHONY: benchmark-enable-v2-saturation
benchmark-enable-v2-saturation: ## Enable WVA saturation V2 analyzer (apply configmap + restart controller)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-enable-v2-saturation BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	kubectl apply -n $(BENCHMARK_NAMESPACE) -f $(WVA_V2_SATURATION_CONFIGMAP)
	$(MAKE) benchmark-restart-controller BENCHMARK_NAMESPACE=$(BENCHMARK_NAMESPACE)

.PHONY: benchmark-restart-controller
benchmark-restart-controller: ## Restart WVA controller to flush in-memory state (e.g., k2 history between runs)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-restart-controller BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	kubectl rollout restart -n $(BENCHMARK_NAMESPACE) $(WVA_CONTROLLER_DEPLOY)
	kubectl rollout status -n $(BENCHMARK_NAMESPACE) $(WVA_CONTROLLER_DEPLOY) --timeout=$(WVA_ROLLOUT_TIMEOUT)

BURSTY_WORKLOAD    ?= bursty.yaml
# How long to wait for the harness pod to COMPLETE. Must exceed the profile's own
# duration plus tokenizer download, warmup and drain -- the CLI default is 3600s,
# which a multi-stage profile can equal on load alone (burst_4k1000_6_14_6_14_6 is
# 5 x 720s = exactly 3600s). At the default such a run is killed mid-drain and
# writes no results, having consumed its full hour: measured 4599s against a
# 3600s wait. Raise this, not the profile, when adding longer workloads.
BENCHMARK_WAIT_TIMEOUT ?= 7200
# Bound for the one rollout benchmark-standup causes when it adds the drain hook.
# Deliberately NOT BENCHMARK_WAIT_TIMEOUT: that is the budget for standing a whole
# stack up, and spending two hours on a single stuck rollout would look exactly
# like a slow model load. A replica reloading its weights takes minutes.
BENCHMARK_DRAIN_ROLLOUT_TIMEOUT ?= 900
BENCHMARK_HARNESS_MEMORY ?= 40Gi

.PHONY: benchmark-run-bursty
benchmark-run-bursty: ## Run bursty traffic benchmark using inference-perf multi-stage rates (set BENCHMARK_NAMESPACE=<namespace>, MODEL_ID=<model>)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-run-bursty BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@# Same reason benchmark-run does it: the clone is gitignored and
	@# benchmark-install re-checks it out, so a re-run can otherwise drive an
	@# unpatched harness. This target skipped it, which is worse here than on
	@# benchmark-run -- a bursty profile is the long one, so a whole hour is
	@# spent before the missing fixes show up as a FAILED run with no report.
	@$(MAKE) --no-print-directory benchmark-patch
	@if [ -f "$(BENCHMARK_SCENARIOS_DIR)/$(BURSTY_WORKLOAD).in" ]; then \
		cp "$(BENCHMARK_SCENARIOS_DIR)/$(BURSTY_WORKLOAD).in" \
		   "$(BENCHMARK_REPO_DIR)/workload/profiles/inference-perf/$(BURSTY_WORKLOAD).in"; \
	fi
	@echo "Patching harness memory to $(BENCHMARK_HARNESS_MEMORY)..."
	@sed -i.bak 's/memory: 32Gi/memory: $(BENCHMARK_HARNESS_MEMORY)/' \
		$(BENCHMARK_REPO_DIR)/config/templates/values/defaults.yaml
	$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) run \
		-p $(BENCHMARK_NAMESPACE) \
		-l inference-perf \
		-w $(BURSTY_WORKLOAD) \
		-U $(BENCHMARK_GATEWAY_URL) \
		$(if $(BENCHMARK_MODEL_ID),-m $(BENCHMARK_MODEL_ID),) \
		$(if $(filter true,$(BENCHMARK_MONITORING)),--monitoring,) \
		--wait-timeout $(BENCHMARK_WAIT_TIMEOUT); \
	rc=$$?; \
	mv $(BENCHMARK_REPO_DIR)/config/templates/values/defaults.yaml.bak \
	   $(BENCHMARK_REPO_DIR)/config/templates/values/defaults.yaml; \
	exit $$rc

.PHONY: benchmark-run-all
benchmark-run-all: ## Run all scenarios: teardown → standup → run per scenario (set BENCHMARK_NAMESPACE=<namespace>, MODEL_ID=<model>)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-run-all BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@for scenario in $(BENCHMARK_SCENARIOS_DIR)/*.yaml.in; do \
		scenario_name=$$(basename "$$scenario" .in); \
		echo ""; \
		echo "=========================================="; \
		echo "[1/3] Tearing down before: $$scenario_name"; \
		echo "=========================================="; \
		$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) teardown \
			-p $(BENCHMARK_NAMESPACE) || true; \
		echo ""; \
		echo "=========================================="; \
		echo "[2/3] Standing up for: $$scenario_name"; \
		echo "=========================================="; \
		$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) standup \
			-p $(BENCHMARK_NAMESPACE) \
			$(if $(BENCHMARK_MODEL_ID),-m $(BENCHMARK_MODEL_ID),) \
			$(if $(filter true,$(BENCHMARK_MONITORING)),--monitoring,) || { \
			echo "ERROR: Standup failed for $$scenario_name"; \
			exit 1; \
		}; \
		echo ""; \
		echo "=========================================="; \
		echo "[3/3] Running scenario: $$scenario_name"; \
		echo "=========================================="; \
		$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) run \
			-p $(BENCHMARK_NAMESPACE) \
			-l $(BENCHMARK_HARNESS) \
			-w "$$scenario_name" \
			$(if $(BENCHMARK_MODEL_ID),-m $(BENCHMARK_MODEL_ID),) || { \
			echo "ERROR: Scenario $$scenario_name failed"; \
			exit 1; \
		}; \
	done
	@echo ""
	@echo "=========================================="
	@echo "All scenarios completed successfully"
	@echo "=========================================="

.PHONY: benchmark-teardown
benchmark-teardown: ## Tear down the benchmark environment (set BENCHMARK_NAMESPACE=<namespace>)
	@if [ -z "$(BENCHMARK_NAMESPACE)" ]; then \
		echo "ERROR: BENCHMARK_NAMESPACE is required. Usage: make benchmark-teardown BENCHMARK_NAMESPACE=<namespace>"; \
		exit 1; \
	fi
	@# Before the namespace goes: a namespace-scoped install still creates
	@# cluster-scoped RBAC, which deleting the namespace would leave behind.
	@if [ "$(BENCHMARK_DIRECT_KEDA)" != "true" ] && [ "$(BENCHMARK_WVA_DEPLOY)" = "true" ]; then \
		$(MAKE) $(BENCHMARK_WVA_UNDEPLOY_TARGET) \
			WVA_NS=$(BENCHMARK_NAMESPACE) WVA_SCOPE=namespace || \
		echo "WARNING: WVA undeploy reported a failure; check for leftovers with 'kubectl get clusterrole,clusterrolebinding | grep wva'"; \
	fi
	@# The controller is removed by the step ABOVE, and that step is authoritative.
	@#
	@# The harness teardown below tries to uninstall it a second time, through the
	@# scenario's own WVA install -- which pulls kustomize resources from
	@#   github.com/llm-d/llm-d/guides/workload-autoscaling/wva-config/platform/ocp?ref=main
	@# a path that does not exist. benchmark-standup disables the scenario's WVA for
	@# exactly that reason, then RESTORES the scenario from its .bak, so by teardown
	@# time the flag is back on and the harness dies on it:
	@#   [01] uninstall_helm: FAILED - Helm uninstall had errors
	@#   accumulating resources from '...wva-config/platform/ocp?ref=main'
	@# Measured tearing a guide namespace down on pokprod. The namespace deleted
	@# cleanly and left no cluster-scoped RBAC behind, so this reads as a failed
	@# teardown that in fact tore everything down -- the worst kind to debug.
	@#
	@# Disabling it loses nothing: there is nothing left for it to uninstall.
	@if [ -f "$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml" ]; then \
		yq -i '(.scenario[] | select(has("wva")) | .wva.enabled) = false' \
			"$(BENCHMARK_REPO_DIR)/config/scenarios/$(BENCHMARK_SPEC).yaml" 2>/dev/null || true; \
	fi
	$(LLMDBENCHMARK) $(BENCHMARK_CLI_FLAGS) teardown \
		-p $(BENCHMARK_NAMESPACE)

.PHONY: benchmark-full
benchmark-full: benchmark-standup benchmark-run-all benchmark-teardown ## Full lifecycle: standup -> run all scenarios -> teardown

# Stub for llm-d nightly reusable workflows (test_target=nightly-test-llm-d)
# No-op; temporarily satisfies nightly CI make invocation
# TODO: add nightly guide tests here
.PHONY: nightly-test-llm-d
nightly-test-llm-d: ## Nightly CI: noop; use as test_target instead of empty string
	@:

# Canonical target for llm-d-infra nightly reusables: ENVIRONMENT=openshift|kubernetes
# Deploys WVA + monitoring + scaler backend only. llm-d model serving is deployed separately
# by the nightly workflow's custom_deploy_script (kustomize + GAIE helm from llm-d/llm-d guide).
.PHONY: nightly-deploy-wva-guide
nightly-deploy-wva-guide: ## Nightly: WVA controller + monitoring stack from job env (WVA_NS <- WVA_NAMESPACE or CONTROLLER_NAMESPACE)
	# Note: CKS callers with resource constraints should disable nodeExporter by patching kube-prometheus-stack post-install.
	# NAMESPACE is passed explicitly, defaulting to the controller namespace.
	# NAMESPACE (what WVA manages) is now mandatory in namespace scope, and this
	# target only ever set WVA_NS (where the controller is installed) -- it relied on
	# the old fallback that let one variable stand for both. Naming it here keeps this
	# job's behaviour exactly as it was, rather than leaving it to a default that no
	# longer exists.
	@WVA_NS="$${WVA_NS:-$${WVA_NAMESPACE:-$${CONTROLLER_NAMESPACE:-}}}" \
	NAMESPACE="$${NAMESPACE:-$${WVA_NS:-$${WVA_NAMESPACE:-$${CONTROLLER_NAMESPACE:-}}}}" \
	ENVIRONMENT="$${ENVIRONMENT:-openshift}" \
	./deploy/install.sh

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-deploy-scripts
lint-deploy-scripts: ## Run bash -n for deploy/install.sh, deploy/lib/*.sh, and deploy plugins
	@echo "Syntax-checking deploy shell scripts..."
	@bash -n deploy/install.sh
	@bash -n deploy/install-epp.sh
	@for script in deploy/lib/*.sh; do bash -n "$$script"; done
	@for script in deploy/*/install.sh; do if [ -f "$$script" ]; then bash -n "$$script"; fi; done
	@for script in deploy/kind-emulator/*.sh; do if [ -f "$$script" ]; then bash -n "$$script"; fi; done
	@echo "Checking for apostrophes inside quoted jq/yq programs..."
	@# `bash -n` catches an ODD number and misses an EVEN one, which
	@# rebalances the quoting and hands jq a truncated program. Both have
	@# happened here; the even one shipped.
	@bash hack/check-quoted-programs.sh
	@echo "Checking the jq marker predicates..."
	@bash hack/check-jq-predicates.sh
	@echo "Checking workload readiness detection..."
	@# Executes the real functions against fixture pod specs. `bash -n` above
	@# cannot see any of what this covers: choosing the wrong container, counting
	@# preStop hooks pod-wide, or reporting an unreadable workload as healthy all
	@# parse perfectly and are all wrong about a running cluster.
	@bash hack/check-workload-gaps.sh
	@echo "Checking model identification..."
	@# Same shape as the check above, on the parsers that name the model every
	@# ScaledObject is written for. `bash -n` sees none of it: a --served-model-name
	@# read as the literal $$MODEL_NAME, or an env value whose newline splits one
	@# workload record into two, both parse perfectly.
	@bash hack/check-scaledobject-parsers.sh
	@echo "Checking what warmpool.sh actually emits..."
	@# Renders the pool manifests and asserts their shape. Every bug this covers
	@# parses fine: one container with the proxy image under the supervisor's
	@# name, a missing ScaledObject (so the pool is never discovered), a worker
	@# template carrying the proxy (so the group never becomes Ready).
	@bash hack/check-warmpool-manifests.sh
	@echo "Checking for mangled line continuations..."
	@# `bash -n` cannot catch this: `cmd \n | grep ...` is SYNTACTICALLY VALID —
	@# the \n becomes a literal argument. It shipped once, in the limiter path,
	@# where it turned WVA_LIMITER=gpu-inventory into an install that aborted.
	@# A real continuation is a backslash at END of line; a mangled one has command
	@# text after it.
	@if grep -rnE '[^"'"'"']\\n[[:space:]]' deploy/install.sh deploy/install-epp.sh deploy/lib/*.sh deploy/*/install.sh 2>/dev/null | grep -vE ':[0-9]+:[[:space:]]*#'; then \
		echo "ERROR: literal '\\n' inside a command — a line continuation was collapsed by an edit."; \
		exit 1; \
	fi
	@echo "deploy script line continuations OK"
	@echo "Checking for make comments inside a continued recipe..."
	@bash hack/check-make-recipes.sh
	@echo "deploy script syntax OK"

.PHONY: smoke-deploy-scripts
smoke-deploy-scripts: lint-deploy-scripts ## Non-interactive deploy script smoke check (source order + arg parsing)
	@echo "Running deploy script smoke check..."
	@SKIP_CHECKS=true ENVIRONMENT=kubernetes ./deploy/install.sh --help >/dev/null
	@echo "deploy script smoke OK"

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-build-warmpool-proxy
docker-build-warmpool-proxy: ## Build the warm-pool proxy image. WARMPOOL_PROXY_IMG=<ref>
	$(CONTAINER_TOOL) build -t ${WARMPOOL_PROXY_IMG} -f Dockerfile.warmpool-proxy .

.PHONY: docker-push-warmpool-proxy
docker-push-warmpool-proxy: ## Push the warm-pool proxy image.
	$(CONTAINER_TOOL) push ${WARMPOOL_PROXY_IMG}

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64
BUILDER_NAME ?= workload-variant-autoscaler-builder

.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name workload-variant-autoscaler-builder
	$(CONTAINER_TOOL) buildx use workload-variant-autoscaler-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm workload-variant-autoscaler-builder
	rm Dockerfile.cross

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif


##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
HELM ?= $(LOCALBIN)/helm

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.17.2
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.8.0
HELM_VERSION ?= v3.17.1

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))


.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	@[ -f "$(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)" ] || { \
	set -e; \
	echo "Downloading golangci-lint $(GOLANGCI_LINT_VERSION)"; \
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION); \
	if [ -f "$(LOCALBIN)/golangci-lint" ]; then \
		mv $(LOCALBIN)/golangci-lint $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION); \
	fi; \
	} ;\
	ln -sf golangci-lint-$(GOLANGCI_LINT_VERSION) $(GOLANGCI_LINT)

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	@[ -f "$(LOCALBIN)/helm-$(HELM_VERSION)" ] || { \
	set -e; \
	echo "Downloading helm $(HELM_VERSION)"; \
	curl -sSfL https://get.helm.sh/helm-$(HELM_VERSION)-$(shell go env GOOS)-$(shell go env GOARCH).tar.gz | tar xz --no-same-owner -C $(LOCALBIN) --strip-components=1 $(shell go env GOOS)-$(shell go env GOARCH)/helm; \
	mv $(LOCALBIN)/helm $(LOCALBIN)/helm-$(HELM_VERSION); \
	} ;\
	ln -sf helm-$(HELM_VERSION) $(HELM)

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

