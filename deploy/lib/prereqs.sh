#!/usr/bin/env bash
#
# Prerequisite checks for deploy/install.sh.
# Requires vars: REQUIRED_TOOLS.
# Requires funcs: log_info/log_success/log_error.
#

# Minimum versions. Only tools whose minimum actually matters are listed; a tool
# absent from here is checked for presence alone, because inventing a floor
# nobody has verified turns a working install into a refused one.
readonly MIN_KUBECTL_VERSION="1.24"
readonly MIN_HELM_VERSION="3.8"
# The overlay filters use go-yq v4 syntax (strenv(), -o=json). yq v3 takes a
# different command form entirely, and the python "yq" is a jq wrapper that
# accepts the expressions and emits JSON. Both pass a presence check and then
# behave differently, so the version is gated, not just the name.
readonly MIN_YQ_VERSION="4.0"

# version_at_least compares dotted versions without sort -V, which BSD/macOS
# lacks in the form this needs. Compares major.minor only: patch levels have
# never been the thing that breaks a deploy here.
version_at_least() {
    local have="$1" want="$2"
    local have_major="${have%%.*}" want_major="${want%%.*}"
    local have_rest="${have#*.}" want_rest="${want#*.}"
    local have_minor="${have_rest%%.*}" want_minor="${want_rest%%.*}"
    # Non-numeric (a dev build, an unparsed string) is treated as new enough:
    # refusing to install on a version we could not read is the worse failure.
    [[ "$have_major$have_minor$want_major$want_minor" =~ ^[0-9]+$ ]] || return 0
    (( have_major > want_major )) && return 0
    (( have_major < want_major )) && return 1
    (( have_minor >= want_minor ))
}

# tool_version prints a tool's major.minor, or nothing if it cannot be read.
tool_version() {
    local tool="$1" raw=""
    case "$tool" in
        kubectl) raw=$(kubectl version --client 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+' | head -1) ;;
        helm)    raw=$(helm version --short 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+' | head -1) ;;
        yq)      raw=$(yq --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+' | head -1) ;;
        *)       return 0 ;;
    esac
    echo "${raw#v}"
}

# append_conditional_tools adds, to the caller's `tools` array, what this
# particular invocation needs on top of REQUIRED_TOOLS. They are conditional
# because requiring them always would refuse installs that never touch them —
# and, worse, NOT checking them at all lets an install fail halfway, after it has
# already created namespaces and RBAC.
#
# It appends to a caller-declared array rather than echoing into mapfile, because
# mapfile is bash 4+ and macOS still ships bash 3.2.
append_conditional_tools() {
    # yq and jq edit the shipped ConfigMaps in place, and the undeploy filters the
    # rendered overlay with yq. Both are effectively unconditional: the install
    # always patches PROMETHEUS_BASE_URL, so patch_manager_config always runs.
    # They were listed as conditional, which let the preflight pass and the install
    # then die after namespaces, RBAC and the Deployment already existed.
    tools+=("yq" "jq")
    # column formats the ScaledObject plan.
    if [ "${WVA_DEFAULT_SO:-false}" != "false" ]; then
        tools+=("column")
    fi
    # openssl mints the self-signed cert for a Prometheus this install deploys.
    if [ "${DEPLOY_PROMETHEUS:-true}" = "true" ]; then
        tools+=("openssl")
    fi
    if [ "${ENVIRONMENT:-kubernetes}" = "openshift" ]; then
        tools+=("oc")
    fi
}

check_prerequisites() {
    log_info "Checking prerequisites..."

    local missing_tools=() outdated_tools=()
    local tools=("${REQUIRED_TOOLS[@]}")
    append_conditional_tools

    for tool in "${tools[@]}"; do
        [ -n "$tool" ] || continue
        if ! command -v "$tool" &> /dev/null; then
            missing_tools+=("$tool")
            continue
        fi
        local have want=""
        have=$(tool_version "$tool")
        case "$tool" in
            kubectl) want="$MIN_KUBECTL_VERSION" ;;
            helm)    want="$MIN_HELM_VERSION" ;;
            yq)      want="$MIN_YQ_VERSION" ;;
        esac
        if [ -n "$want" ] && [ -n "$have" ] && ! version_at_least "$have" "$want"; then
            outdated_tools+=("$tool $have (need >= $want)")
        fi
    done

    # Keep deploy paths deterministic: fail fast instead of prompting for installs.
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}. Install them on PATH, or set SKIP_CHECKS=true to bypass this check (not recommended)."
    fi
    if [ ${#outdated_tools[@]} -ne 0 ]; then
        log_error "Tools too old: ${outdated_tools[*]}. Upgrade them, or set SKIP_CHECKS=true to bypass this check (not recommended)."
    fi

    # A reachable cluster is as much a prerequisite as a binary, and finding out
    # here beats finding out after the first namespace has been created.
    # `kubectl get --raw /version`, not `kubectl cluster-info`: cluster-info LISTS
    # SERVICES IN kube-system, which a namespace tenant may not do. So the very
    # first check in the tenant path failed for every real tenant, and told them
    # their kubeconfig was wrong when it was correct and working. /version is
    # served to any authenticated caller and needs no RBAC, which is exactly the
    # question being asked here: can this kubeconfig reach a cluster at all.
    if ! kubectl get --raw /version > /dev/null 2>&1; then
        log_error "Cannot reach a Kubernetes cluster with the current context ($(kubectl config current-context 2>/dev/null || echo 'none set')). Check your kubeconfig, or set SKIP_CHECKS=true to bypass this check."
    fi

    log_success "All prerequisites met (tools: ${tools[*]})"
}

# check_permissions verifies the caller can create what the install produces, and
# that the controller will be able to read what its configuration requires.
#
# `make check-prereqs` used to answer only "are the binaries here", and passed for a
# user who could not create a single object the install makes. A preflight that
# cannot fail on the most likely failure is not a preflight.
#
# The node rule is the interesting one, and it is conditional by design:
#
#   physical limiter ON   nodes are REQUIRED. A variant whose accelerator cannot be
#                         resolved is charged to no pool, receives no GPU budget and
#                         NEVER SCALES UP — silently, because nothing errors. An
#                         install that cannot read nodes is therefore broken, and
#                         this is an error.
#   physical limiter OFF  the install still works, degraded: accelerators stay
#                         unresolved, so metrics lose the accelerator label and the
#                         capacity model cannot reuse learned capacity. A warning,
#                         not a refusal — and it says what changes if the limiter is
#                         turned on later.
# can_i_as answers a permission question ABOUT ANOTHER SUBJECT, distinguishing
# "no" from "could not ask".
#
# `kubectl auth can-i --as` impersonates, which the CALLER must be allowed to do.
# A cluster admin can; plenty of legitimate installers cannot. Both cases exit
# non-zero, so treating non-zero as "denied" turned "I lack impersonate" into
# "the controller cannot read nodes" — a hard, wrong refusal to install, and one
# that gets worse the less privileged the user is.
#
# Echoes: yes | no | unknown
can_i_as() {
    local subject="$1"; shift
    local out rc
    out=$(kubectl auth can-i "$@" --as "$subject" 2>&1)
    rc=$?
    case "$out" in
        yes*) echo yes ;;
        no*)  [ $rc -ne 0 ] && echo no || echo unknown ;;
        *)    echo unknown ;;
    esac
}

# wva_cluster_policy_ns echoes the namespace this install's controller will read
# limiters and quotas from, or nothing when none is published.
#
# It resolves the way the CONTROLLER does, because a preflight that consults a
# different source than the thing it is checking is worse than no preflight:
#
#   1. a wva.llmd.ai/policy-namespace label on the namespace being managed
#   2. the default namespace hardcoded in WVA
#
# Step 2's name is read OUT OF THE BINARY rather than repeated here. One
# definition, in Go; a bash copy would be free to drift, and the drift shows up as
# a preflight that passes while the controller reads a different namespace.
wva_cluster_policy_ns() {
    local managed="${WVA_WATCH_NS:-$WVA_NS}" pointed="" out rc
    # Namespaces are cluster-scoped, so a tenant holding only namespaced rights
    # cannot read the object their own namespace is. Folding that denial into "no
    # label" answered with the built-in default policy namespace when the truth
    # was that the question could not be asked — and the label may point
    # somewhere else entirely. "unknown" is echoed so the caller can say so.
    out=$(kubectl get namespace "$managed" \
        -o jsonpath='{.metadata.labels.wva\.llmd\.ai/policy-namespace}' 2>&1); rc=$?
    if [ $rc -ne 0 ]; then
        printf '%s' "$out" | grep -qi 'forbidden' && { echo unknown; return 0; }
        return 0
    fi
    pointed="$out"
    [ -n "$pointed" ] || return 0
    kubectl get namespace "$pointed" >/dev/null 2>&1 || return 0
    echo "$pointed"
}

# wva_cluster_policy_limiter echoes the limiter type cluster policy declares, or
# nothing. Reads the same ConfigMap and the same `default` entry the controller
# does.
# Echoes the limiter cluster policy declares, or nothing.
#
# Echoes the literal "unknown" when the ConfigMap cannot be READ, which is not
# the same as no limiter being declared. Reporting Forbidden as "none" told a
# tenant "scaling is UNBOUNDED" while a gpu-inventory limiter was in force — the
# opposite of the truth, and in the direction that sounds harmless.
wva_cluster_policy_limiter() {
    local policy_ns="$1" cm="" listing rc
    [ -n "$policy_ns" ] || return 0
    listing=$(kubectl get configmap -n "$policy_ns" -o name 2>&1); rc=$?
    if [ $rc -ne 0 ]; then
        printf '%s' "$listing" | grep -qi 'forbidden' && echo "unknown"
        return 0
    fi
    cm="$(printf '%s\n' "$listing" | grep -E "configmap/(wva-)?scaling-policy-config$" | head -1 | cut -d/ -f2 || true)"
    [ -n "$cm" ] || return 0
    kubectl get configmap "$cm" -n "$policy_ns" -o jsonpath='{.data.default}' 2>/dev/null \
        | yq -r '.limiters[0].type // ""' 2>/dev/null || true
}

# check_tenant_install enforces the contract for a scope that creates no
# cluster-scoped objects.
#
# Limiters and quotas are CLUSTER-defined. A tenant reads them; they do not write
# them. So the only question at install time is whether this install can honour
# what the cluster already decided:
#
#   physical limiter declared   the controller must be able to list nodes, which
#                               only a cluster admin can grant. Without it every
#                               variant is charged to no accelerator pool, gets no
#                               budget and NEVER SCALES UP — so the install FAILS
#                               here rather than succeeding into that state.
#   anything else               install proceeds with a warning saying what is and
#                               is not bounding it.
check_tenant_install() {
    local sa="system:serviceaccount:${WVA_NS}:wva-controller-manager"
    local policy_ns limiter node_read

    policy_ns="$(wva_cluster_policy_ns)"
    if [ "$policy_ns" = "unknown" ]; then
        log_warning "Could not read the ${WVA_WATCH_NS:-$WVA_NS} namespace object, so which cluster policy governs this install is unknown — a namespace is cluster-scoped, and namespaced rights do not reach it."
        log_warning "  If that policy declares the gpu-inventory limiter, this controller also needs node read access, and stays NotReady without it. A cluster admin can tell you, or 'make setup-prereqs' grants it either way."
        return 0
    fi
    if [ -z "$policy_ns" ]; then
        # No label on the managed namespace. The controller will fall back to its
        # built-in default policy namespace, whose NAME this script deliberately
        # does not know — one definition, in Go, so the two cannot drift. Which
        # means the limiter cannot be resolved from here, and the check moves to
        # where the name lives: the controller refuses to become ready if cluster
        # policy demands node access it does not have, and verify_deployment turns
        # that into a failed install.
        log_info "This namespace carries no wva.llmd.ai/policy-namespace label, so the controller will use its built-in default policy namespace."
        log_warning "  If no cluster policy exists there either, scaling here is bounded only by each workload's maxReplicas. A ResourceQuota on ${WVA_NS} is the way to bound it."
        return 0
    fi

    limiter="$(wva_cluster_policy_limiter "$policy_ns")"
    log_info "Cluster policy: ${policy_ns} (limiter: ${limiter:-none})"

    if [ "$limiter" = "unknown" ]; then
        # Forbidden, not absent. Reporting it as a limiter named "unknown" said
        # the install was bounded when nobody had established that it was — and
        # skipped the node-access check that a gpu-inventory policy makes
        # load-bearing, so a variant that can never scale up would look healthy.
        log_warning "Could not read the cluster policy, so whether a limiter bounds this install is unknown. If it declares the gpu-inventory limiter, this controller also needs node read access, or every variant it manages stays at its current size."
        return 0
    fi
    if [ "$limiter" != "gpu-inventory" ]; then
        if [ -z "$limiter" ]; then
            log_warning "Cluster policy declares no limiter, so scaling is UNBOUNDED. A ResourceQuota on ${WVA_NS} is the only thing that will bound it."
        else
            log_info "Cluster policy bounds this install with the '${limiter}' limiter, which needs no cluster-scoped access"
        fi
        return 0
    fi

    node_read="$(can_i_as "$sa" list nodes)"
    case "$node_read" in
        yes)
            log_success "Cluster policy declares the gpu-inventory limiter, and ${sa} can list nodes"
            ;;
        *)
            log_error "Cluster policy declares the gpu-inventory limiter, and ${sa} cannot list nodes.

A GPU-aware limiter allocates out of per-accelerator pools. Without node access a
variant's accelerator never resolves, it is charged to no pool, it receives no
budget, and it NEVER SCALES UP — with nothing in the logs to say why. Installing
into that state would look like it worked.

Reading nodes is cluster-scoped, so only a cluster admin can grant it. Ask for:

  kubectl create clusterrole wva-node-reader --verb=get,list,watch --resource=nodes
  kubectl create clusterrolebinding wva-node-reader-${WVA_NS} \\
    --clusterrole=wva-node-reader --serviceaccount=${WVA_NS}:wva-controller-manager

Or ask them to install WVA for you (WVA_SCOPE=namespace), which grants it."
            ;;
    esac
}

# wva_api_resource echoes "<resource> <namespaced>" for a Kind, as THIS cluster
# defines it. Empty when the cluster has no such kind.
#
# The mapping is asked for rather than derived, because deriving it is wrong in
# ways that are silent: `kubectl auth can-i create networkpolicys` is not a
# denial, it is a question about a resource that does not exist, and it answers
# "no". A false denial refuses a legitimate install.
#
# Columns are NAME SHORTNAMES APIVERSION NAMESPACED KIND, and SHORTNAMES is often
# empty — so the ends are read, never a fixed column index.
wva_api_resource() {
    local kind="$1"
    kubectl api-resources --no-headers 2>/dev/null \
        | awk -v k="$kind" '$NF == k { print $1, $(NF-1); exit }' || true
}

# wva_autoselect_namespace points a namespace-scoped install at the namespace
# that actually runs llm-d, when nobody said which.
#
# Naming the namespace was never information the installer lacked — the model
# servers are labelled, so the cluster already knows. Asking for it anyway is how
# someone installs into the default namespace, gets a healthy controller managing
# nothing, and has no error to go on.
#
# It only ever acts when NOTHING was chosen: WVA_NS_SOURCE=default means the
# caller set neither WVA_NS nor NAMESPACE. Anything explicit is left alone.
#
# With several llm-d namespaces it REFUSES rather than picking. A namespace-scoped
# controller manages exactly one, and choosing someone's namespace for them is not
# a guess worth making silently.
wva_autoselect_namespace() {
    [ "${WVA_NS_SOURCE:-}" = "default" ] || return 0
    [ "$(wva_install_scope)" = "namespace" ] || return 0

    # Searching means listing workloads cluster-wide, which a NAMESPACE ADMIN may
    # not do. Say that, rather than falling back to the default namespace as
    # though the cluster genuinely had no llm-d in it: one is "tell me which
    # namespace", the other is "deploy llm-d first", and they are not the same
    # instruction.
    if ! kubectl auth can-i list deployments -A >/dev/null 2>&1; then
        log_info "Cannot search the cluster for llm-d model servers (listing workloads cluster-wide is not permitted for you), so the namespace cannot be found for you."
        log_info "  Name it:  NAMESPACE=<your namespace>   — llm-d's own variable, so if you followed one of its guides it is already exported."
        return 0
    fi

    local found count
    found="$(wva_namespaces_with_model_servers || true)"
    count="$(printf '%s' "$found" | grep -c . || true)"

    if [ "${count:-0}" -eq 1 ]; then
        WVA_NS="$(printf '%s' "$found" | tr -d '[:space:]')"
        WVA_NS_SOURCE=discovered
        export WVA_NS WVA_NS_SOURCE
        return 0
    fi

    if [ "${count:-0}" -gt 1 ]; then
        log_error "Several namespaces run llm-d model servers, and a namespace-scoped WVA manages exactly one:

$(printf '  - %s\n' $found)
Say which:

    NAMESPACE=<one of the above> make deploy-wva

Or install one WVA for all of them:

    make deploy-wva SCOPE=cluster"
    fi

    # None found, or not allowed to look. Either way, keep the default and let
    # wva_report_namespace explain which of the two it is.
    return 0
}

# wva_report_namespace says which namespace this install will use, how that was
# decided, and whether it holds anything to scale.
#
# The last part is the one worth running: a namespace-scoped controller manages
# exactly the namespace it runs in, so pointing it at the wrong one produces a
# controller that installs cleanly, reports healthy, and manages nothing. There is
# no error anywhere in that sequence — the only symptom is that nothing ever
# scales, which is indistinguishable from an idle cluster.
wva_report_namespace() {
    local ns="${WVA_NS}" scope managed count
    scope="$(wva_install_scope)"
    managed="${WVA_WATCH_NS:-$ns}"

    case "${WVA_NS_SOURCE:-}" in
        discovered)
            log_success "Namespace: $ns  (found: it is the only namespace running llm-d model servers)"
            log_info "  Override with WVA_NS=<ns> if you meant a different one." ;;
        llmd-ns)  log_info "Namespace: $ns  (from NAMESPACE)" ;;
        namespace-env)
            log_info "Namespace: $ns  (from NAMESPACE — the variable llm-d's own guides export)" ;;
        explicit) log_info "Namespace: $ns  (you set WVA_NS)" ;;
        *)        log_info "Namespace: $ns$([ "$ns" = "workload-variant-autoscaler-system" ] && echo '  (the default)')" ;;
    esac

    if [ "$scope" = "cluster" ]; then
        log_info "  The controller runs there and manages EVERY namespace."
        return 0
    fi

    if [ "$managed" != "$ns" ]; then
        log_info "  It runs in $ns and manages $managed (WVA_WATCH_NS)."
    else
        log_info "  It runs in $ns and manages that same namespace — nothing outside it."
    fi

    # Does the managed namespace actually contain model servers?
    local ns_err=""
    if ! ns_err="$(kubectl get namespace "$managed" 2>&1 >/dev/null)"; then
        # FORBIDDEN IS NOT ABSENT, and this is the one place it matters most.
        # `namespaces` is cluster-scoped, so a namespace-scoped tenant is routinely
        # denied a read of their OWN namespace -- measured on pokprod001:
        #
        #   Error from server (Forbidden): namespaces "evgensh-wva-test" is forbidden:
        #   User "system:serviceaccount:evgensh-wva-test:builder" cannot get resource
        #   "namespaces" in API group "" in the namespace "evgensh-wva-test"
        #
        # Treating that as "does not exist" refuses the install of the exact persona
        # this scenario is written for, and tells them their namespace name is wrong
        # when it is right. There is a second signal that needs no cluster-scoped
        # right: the model-server count below is namespaced, so let it answer.
        case "$ns_err" in
            *[Ff]orbidden*)
                log_info "  Cannot read the namespace object for $managed (that is a cluster-scoped read, and you are namespace-scoped) -- judging it by what is running in it instead."
                ;;
            *)
        # STOP. The caller named this namespace -- NAMESPACE is mandatory in this
        # scope -- and in the supported scenario llm-d is already running in it, so
        # the namespace exists. A name that resolves to nothing is a typo, a stale
        # export, or the wrong cluster, and creating it would produce exactly the
        # thing this preflight exists to prevent: a healthy controller with nothing
        # to scale.
        #
        # kind-emulator is exempt: the e2e path provisions llm-d and WVA in one
        # sequence and owns its own ordering. SKIP_CHECKS=true is the bypass for
        # anyone who means it.
        if [ "${ENVIRONMENT:-}" != "kind-emulator" ]; then
            log_error "  Refusing to continue: namespace $managed does not exist.

You named it, and in this scenario llm-d is already running in it — so the name is
almost certainly wrong: a typo, a stale NAMESPACE export, or the wrong cluster.

  name the namespace llm-d serves from:  NAMESPACE=<namespace> make <target>
  or install without the checks:         SKIP_CHECKS=true"
        fi
        log_info "  $managed does not exist yet; the install creates it."
        return 0
                ;;
        esac
    fi
    count="$(wva_model_server_count "$managed")"
    if [ "$count" -gt 0 ] 2>/dev/null; then
        log_success "  $managed holds $count llm-d model server(s) for it to manage."
    else
        # Terse by design: the STOP itself is the actionable message, and every
        # rationale that used to live here (why an empty namespace is a silent
        # failure rather than a warning, the "where else are they" convenience,
        # the permission-scoped exemption from an earlier revision) now lives once
        # in the guide's "How it works" section, not repeated at every call site.
        #
        # kind-emulator is exempt: the e2e path provisions llm-d and WVA in one
        # sequence and owns the ordering itself. SKIP_CHECKS=true is the bypass.
        # Cluster scope never reaches here: it returns above.
        if [ "${ENVIRONMENT:-}" != "kind-emulator" ]; then
            log_error "  No llm-d found in $managed. Please install llm-d on $managed."
            log_error "    SKIP_CHECKS=true to continue anyway — see docs/deployment/operations.md#first-line-troubleshooting"
        fi
    fi
}

# wva_model_server_count counts llm-d model servers in one namespace, by the same
# marker and on the same pod-template basis the ScaledObject scan uses — `-l` on
# the object matches nothing, because llm-d labels the pod template.
wva_model_server_count() {
    local ns="$1" total=0 resource pod n
    for resource in deployments:"$SO_POD_PATH_DEPLOYMENT" leaderworkersets:"$SO_POD_PATH_LWS"; do
        pod="${resource#*:}"; resource="${resource%%:*}"
        # Keep exactly one integer. `|| echo 0` on a pipeline that already printed
        # a count appends a SECOND value — the resource kind may be absent (no LWS
        # CRD) or unreadable, and then `$((total + 0\n0))` is a syntax error that
        # takes the preflight down mid-report. Last line, digits only.
        n="$(kubectl get "$resource" -n "$ns" -o json 2>/dev/null \
            | jq --argjson p "$pod" --argjson markers "$(so_serving_markers_json)" '
                [ .items[]
                  | ((getpath($p + ["metadata","labels"]) // {}) + (.metadata.labels // {}))
                  | select(to_entries
                           | any(.key + "=" + (.value|tostring) as $kv
                                 | ($markers | index($kv)) != null))
                ] | length' 2>/dev/null | tail -1 | tr -cd '0-9' || true)"
        total=$((total + ${n:-0}))
    done
    echo "$total"
}

check_permissions() {
    log_info "Checking permissions..."

    local ns="${WVA_NS}" denied=() sa="system:serviceaccount:${WVA_NS}:wva-controller-manager"
    local kinds kind resource namespaced cluster_denied="" unknown=()

    # What this install creates, read from the RENDERED overlay rather than
    # inferred from the scope name. The scope does not decide it: the
    # namespace-scoped overlay creates eight cluster-scoped objects on Kubernetes
    # (four ClusterRoles and four ClusterRoleBindings) and ten on OpenShift, where
    # the platform's monitoring wiring — cluster-monitoring-view for Thanos, among
    # others — adds two more bindings that components/tenant-installable does not
    # subtract.
    #
    # Inferring it meant a non-admin passed `--check` on OpenShift and then failed
    # partway through `kubectl apply -k`, in exactly the half-installed state this
    # preflight exists to prevent. It also under-checked BOTH scopes: neither
    # branch asked about Secrets or ServiceMonitors, and the cluster branch never
    # asked about Roles or RoleBindings.
    # Only the kinds THIS PHASE creates.
    #
    # INSTALL_PHASE=wva is the tenant's half, and every cluster-scoped object in
    # the overlay belongs to the admin's half — already created, by someone else,
    # before the tenant runs anything. Checking "can you create a ClusterRole"
    # there asks the one question whose answer is no by design, and this check
    # exits on a no. So the documented tenant path failed for exactly the person
    # it was written for, with advice telling them to run the command that had
    # just failed. Existence of those objects is checked separately, by
    # require_prereqs_present, which is the right question for this phase.
    kinds="$(wva_rendered_kinds || true)"
    if [ "${INSTALL_PHASE:-all}" = "wva" ] && [ -n "$kinds" ]; then
        kinds="$(printf '%s
' "$kinds" | grep -vxF -f <(printf '%s
' "${WVA_PREREQ_KINDS[@]}") || true)"
    fi
    if [ -z "$kinds" ]; then
        log_warning "Could not render the install overlay, so this check falls back to the kinds each scope is expected to create. It may miss something the overlay adds; the install itself renders the same overlay and will fail if it is broken."
        kinds=$'ConfigMap\nDeployment\nRole\nRoleBinding\nSecret\nService\nServiceAccount\nServiceMonitor'
        wva_scope_is_tenant || kinds="$kinds"$'\nClusterRole\nClusterRoleBinding\nNamespace'
    fi

    for kind in $kinds; do
        set -- $(wva_api_resource "$kind")
        resource="${1:-}"; namespaced="${2:-}"
        if [ -z "$resource" ]; then
            # A kind this cluster does not define. NOT a failure: this check runs
            # before the install deploys anything, and the install itself may
            # create the CRD later in the same run — ServiceMonitor when
            # DEPLOY_PROMETHEUS=true is exactly that. Unanswerable, so it is
            # reported rather than either failed or hidden.
            unknown+=("$kind")
            continue
        fi
        if [ "$namespaced" = "true" ]; then
            kubectl auth can-i create "$resource" -n "$ns" >/dev/null 2>&1 \
                || denied+=("create $resource in $ns")
            continue
        fi
        # The namespace itself, only when it does not exist yet — requiring
        # namespace creation from someone installing into a namespace an admin
        # already made for them would refuse a perfectly good install.
        # The Namespace is handled after this loop: the installer creates WVA_NS
        # imperatively whatever the overlay contains, so the render is not the
        # authority on it.
        [ "$kind" = "Namespace" ] && continue
        if ! kubectl auth can-i create "$resource" >/dev/null 2>&1; then
            denied+=("create $resource (cluster-scoped)")
            cluster_denied=1
        fi
    done

    # WVA_NS, asked separately from the render and at BOTH scopes.
    #
    # create_namespaces materializes WVA_NS imperatively whenever it is missing,
    # so the namespace-scoped overlay not containing a Namespace object means
    # nothing here — components/tenant-installable deletes it precisely because a
    # tenant installs into a namespace that already exists. Reading only the
    # render therefore passed a preflight that the install then failed on its
    # first write, which is what this whole check exists to prevent.
    #
    # Only when it is missing: requiring namespace creation from someone
    # installing into a namespace an admin already made for them would refuse a
    # perfectly good install. On OpenShift a self-provisioner creates namespaces
    # through projectrequests rather than namespaces, and either is enough.
    if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
        if ! kubectl auth can-i create namespaces >/dev/null 2>&1 \
            && ! kubectl auth can-i create projectrequests >/dev/null 2>&1; then
            denied+=("create namespace $ns (it does not exist yet)")
            cluster_denied=1
        fi
    fi

    if [ ${#unknown[@]} -ne 0 ]; then
        log_info "Not checked, because this cluster has no such resource yet: ${unknown[*]}. The install may create it (a ServiceMonitor needs the Prometheus Operator CRDs, which DEPLOY_PROMETHEUS=true installs). If it does not, the apply fails on the missing kind."
    fi

    if [ ${#denied[@]} -ne 0 ]; then
        local advice="Ask for the objects above, or have an admin run the install."
        if [ -n "$cluster_denied" ] && wva_scope_is_tenant; then
            advice="WVA_SCOPE=namespace narrows what the CONTROLLER reads. It does not make the
install self-service on every platform, and this overlay creates cluster-scoped
objects:

  $(wva_overlay_dir)

On OpenShift that is by design — the Thanos and user-workload-monitoring wiring
is cluster-scoped, and without it the controller cannot reach Prometheus at all.

You do not need those rights to run the controller. Have an admin create them
once for this namespace:

    NAMESPACE=$ns make setup-prereqs$([ "$(wva_install_scope)" = cluster ] && echo " SCOPE=cluster")

then install with 'NAMESPACE=$ns make deploy-wva INSTALL_PHASE=wva', which needs none of them."
        fi
        log_error "You cannot create everything this install produces. Denied:
$(printf '  - %s\n' "${denied[@]}")

$advice"
    fi

    # A tenant install has its own contract for what the CONTROLLER will need, and
    # it is not the WVA_LIMITER question below: limiters and quotas are
    # CLUSTER-defined, and a tenant reads them rather than declaring them.
    if wva_scope_is_tenant; then
        check_tenant_install
        log_success "Permissions look sufficient"
        return 0
    fi

    # What the CONTROLLER will need once it is running.
    local node_read
    node_read="$(can_i_as "$sa" list nodes)"
    if [ "${WVA_LIMITER:-none}" = "gpu-inventory" ]; then
        case "$node_read" in
            yes)
                log_success "The controller can list nodes, which the gpu-inventory limiter requires"
                ;;
            no)
                log_error "WVA_LIMITER=gpu-inventory needs the controller to list nodes, and $sa cannot.

A GPU-aware limiter allocates out of per-accelerator pools. Without nodes, a
variant's accelerator never resolves, it is charged to no pool, it receives no
budget, and it NEVER SCALES UP — with nothing in the logs to say why.

Either grant the node read, or install without the limiter (WVA_LIMITER=none)."
                ;;
            *)
                # Cannot impersonate, so this is unanswerable from here. It is not a
                # reason to refuse: the install itself grants the node read, and the
                # controller publishes wva_node_access_denied if it turns out not to
                # have it. Say what to watch instead of guessing.
                log_warning "Could not verify that $sa may list nodes (checking needs impersonate permission, which you do not have)."
                log_warning "  WVA_LIMITER=gpu-inventory requires it. After the install, confirm with: kubectl get --raw /metrics | grep wva_node_access_denied — 1 means every variant is getting no GPU budget and will not scale up."
                ;;
        esac
    else
        case "$node_read" in
            no)
                log_warning "$sa cannot list nodes. Without a physical limiter this is survivable but degraded: accelerators stay unresolved, so metrics lose the accelerator label and the capacity model cannot reuse learned capacity across variants."
                log_warning "  It stops being fine the moment someone sets a gpu-inventory limiter: every variant would then get no GPU budget and stop scaling up. Watch wva_node_access_denied."
                ;;
            unknown)
                log_info "Skipped the controller's node-read check (needs impersonate permission). No limiter is configured, so nothing depends on it yet."
                ;;
        esac
    fi

    log_success "Permissions look sufficient"
}

# wva_report_scaledobjects says what already registers workloads with WVA. It never
# fails, and the distinction it rests on is the one worth stating, because an earlier
# revision of this file got it backwards and failed the guide's own flow.
#
# Two different questions:
#
#   objects to scale        the llm-d model servers. WVA's PREMISE. None of them is
#                           fatal -- wva_report_namespace stops for it -- because the
#                           scenario is "llm-d already works here", and without them
#                           it does not.
#   KEDA ScaledObject CRs   the registrations. WVA's OWN OUTPUT. None yet is the BEST
#                           state: it is exactly what a namespace looks like the
#                           moment before the registration step, which creates them.
#                           Some already present is fine too -- the plan offers
#                           `adopt` per entry, so the operator decides each one.
#
# So a controller with zero ScaledObjects is not a fault. It is either the documented
# mid-flow state between `deploy-wva` and `scaledobjects-apply`, or a forgotten
# registration step -- and NOTHING IN CLUSTER STATE DISTINGUISHES THEM.
wva_report_scaledobjects() {
    local ns="${WVA_WATCH_NS:-$WVA_NS}" count

    # `|| count=0` is the "never fails" contract above, not decoration: under the
    # caller's `set -e -o pipefail`, a non-zero exit anywhere in this pipe -- e.g.
    # the ScaledObject CRD not installed yet, an entirely ordinary state for a
    # preflight to run in -- would otherwise abort the whole script right here,
    # in the one function on this path that promises it can't.
    count=$(kubectl get scaledobject -n "$ns" -o json 2>/dev/null | jq -r '
        [ .items[]?
          | select([ .spec.triggers[]?.metadata.scalerAddress // ""
                     | select(startswith("wva-external-scaler.")) ] | length > 0) ] | length' 2>/dev/null) || count=0
    if [ "${count:-0}" -gt 0 ]; then
        log_success "  ScaledObjects: $count workload(s) in $ns already registered with WVA."
        log_info "    The plan will offer 'adopt' for each, so you choose what happens to them."
        return 0
    fi
    # Deliberately log_info, not a warning: this is the expected state here.
    log_info "  ScaledObjects: none in $ns yet — registration is the next step, and nothing scales until it runs:"
    log_info "      make scaledobjects-plan WVA_DEFAULT_SO_PLAN=wva-plan.yaml   # then edit it"
    log_info "      make scaledobjects-apply WVA_DEFAULT_SO_PLAN=wva-plan.yaml"
}
