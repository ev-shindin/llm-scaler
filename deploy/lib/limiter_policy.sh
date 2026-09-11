#!/usr/bin/env bash
#
# The scaling-policy documents shared datapath: building a `limiters:` list, and
# merging it into a policy without losing the rest of it.
#
# Its own file because TWO commands write that list and they load differently:
# the installer (deploy/install.sh, via infra_wva.sh) for one deployment's own
# policy, and `make enable-physical-limiter` (physical_limiter.sh, sourced by the
# Makefile with only common.sh beside it) for the cluster policy every WVA reads.
# Both used to build the list inline, and both built the SAME invalid quota entry
# -- see limiter_entry_yaml for what that costs.
#
# Requires funcs: log_error.
# Requires vars: none unconditionally; the quota path reads WVA_QUOTAS,
# WVA_QUOTA_SCOPE, WVA_SCOPE, WVA_WATCH_NS and WVA_NS. The namespace a
# namespace-scoped budget is keyed on is an ARGUMENT, never an environment
# variable -- it is the highest-precedence input to the document, and one that
# could arrive from an operator's exported shell would be unvalidatable.

# limiter_entry_yaml emits the `limiters:` list that WVA_LIMITER declares, as
# YAML on stdout. Pure: it reads WVA_LIMITER-related variables and writes no
# cluster state, so hack/check-limiter-declaration.sh can execute it offline.
#
# It exists because the installer used to emit `[{"type": "quota"}]` and call
# that "bounded by the quota limiter". The controller rejects it —
#
#     Invalid saturation scaling config entry ... "limiters: entry[0]: name must
#     not be empty"
#
# — and rejecting the entry throws away the WHOLE `default` policy, thresholds
# included, then falls back to no limiter at all: "scaling is UNCONSTRAINED". The
# install printed success either way, so WVA_LIMITER=quota had never once bounded
# anything, and the one line that said so was an ERROR in the controller log.
#
# A quota entry needs a budget from the operator; there is no safe default. An
# empty one is not "unlimited" — QuotaForNamespace returns an empty map for a
# namespace it does not list, callers read that as zero, and the fleet freezes.
# So WVA_QUOTAS is required, and a missing or malformed one stops the install
# before the ConfigMap is touched.
limiter_entry_yaml() {
    local ltype="$1" ns_key="${2:-}"

    if [ "$ltype" = "gpu-inventory" ] || [ "$ltype" = "inventory" ]; then
        # A budget handed to the PHYSICAL limiter is discarded, so say so rather
        # than drop it: the operator typed caps and got an entry carrying none,
        # with nothing anywhere reporting the difference.
        if [ -n "${WVA_QUOTAS:-}" ]; then
            log_warning "WVA_QUOTAS is set, but the ${ltype} limiter takes no budget -- it bounds by the GPUs that physically exist. The caps you passed ('${WVA_QUOTAS}') are being IGNORED. Pass WVA_LIMITER=quota to declare them instead."
        fi
        # Physical capacity comes from the GPU operator, so there is nothing to
        # declare. A name is allowed but unused — NewLimiterFromConfig names the
        # inventory limiter itself — and quota fields are REJECTED on this type.
        printf -- '- type: %s\n' "$ltype"
        return 0
    fi
    if [ "$ltype" != "quota" ]; then
        log_error "limiter_entry_yaml: unknown limiter type '$ltype'"
    fi

    local scope="${WVA_QUOTA_SCOPE:-namespace}"
    case "$scope" in
        namespace|cluster) ;;
        *) log_error "WVA_QUOTA_SCOPE must be 'namespace' or 'cluster', got '$scope'" ;;
    esac
    # WVA_SCOPE is validated HERE, not only inside the namespace branch below.
    # Reached with WVA_QUOTA_SCOPE=cluster, the namespace branch never runs, so
    # `WVA_SCOPE=bogus` printed its refusal -- from the subshell -- and the entry
    # was emitted anyway, exit 0, "Scaling is now bounded". Latent in the real
    # installer, which dies earlier on the overlay, but an input this function
    # reads is an input this function checks.
    if [ -n "${WVA_SCOPE:-}" ] && declare -F wva_install_scope >/dev/null; then
        wva_install_scope >/dev/null || exit 1
    fi

    if [ -z "${WVA_QUOTAS:-}" ]; then
        log_error "WVA_LIMITER=quota needs WVA_QUOTAS, a per-accelerator budget: WVA_QUOTAS='H200=8 A100=4'.
    There is no default to fall back on. A quota entry with no budget is not unlimited — an accelerator
    the entry does not name gets zero, so every managed workload stops scaling up. Use -1 for no cap on
    a type ('H100=-1'). The accelerator name is the one WVA resolves, which the controller logs per
    variant:
        kubectl logs -n ${WVA_NS:-<the controller namespace>} deploy/wva-controller-manager | grep accelerator"
    fi

    # Accept commas or whitespace between entries, so both of the shapes an
    # operator reaches for work: 'H200=8,A100=4' and 'H200=8 A100=4'.
    local pairs pair name value
    pairs="$(printf '%s' "$WVA_QUOTAS" | tr ',' ' ')"
    local types=""
    # Word-splitting on $pairs is what this loop is for; PATHNAME expansion is
    # not. Unguarded, WVA_QUOTAS='*' expands against the current directory and a
    # GPU budget is synthesised out of filenames -- exit 0, and a policy nobody
    # wrote. Restored rather than left off: this function is called from scripts
    # that glob afterwards.
    local reset_glob=1 pairs_seen=0
    case "$-" in *f*) reset_glob=0 ;; esac
    set -f
    for pair in $pairs; do
        name="${pair%%=*}"
        value="${pair#*=}"
        if [ "$name" = "$pair" ] || [ -z "$name" ] || [ -z "$value" ]; then
            [ "$reset_glob" = 1 ] && set +f
            log_error "WVA_QUOTAS entry '$pair' is not TYPE=N (for example 'H200=8'). Whole value: '$WVA_QUOTAS'"
        fi
        # `*[!0-9]*`, not `[0-9][0-9]*`. The second is a GLOB: its `*` matches
        # anything, so every value whose first two characters are digits passed
        # -- 'H200=12abc', 'H200=16Gi', 'H200=12.5'. The arithmetic test below
        # then failed with "integer expression expected", which is not fatal
        # inside an `if`, and the entry was emitted anyway. yq accepts
        # `H200: 12abc` as a string, so the install patched it and reported
        # success, and the controller threw away the whole policy on read: the
        # exact failure this file exists to prevent, reintroduced by a glob.
        case "$value" in
            -1) ;;
            ''|*[!0-9]*)
                [ "$reset_glob" = 1 ] && set +f
                log_error "WVA_QUOTAS entry '$pair': the budget must be a whole number of GPUs, or -1 for no cap on that type" ;;
            0|[1-9]*) ;;
            *)
                # A LEADING ZERO. All digits, so the case above lets it past, and
                # go-yaml then reads it as OCTAL: `H200: 010` becomes 8. The
                # operator asks for ten GPUs, gets eight, and nothing anywhere
                # says so.
                [ "$reset_glob" = 1 ] && set +f
                log_error "WVA_QUOTAS entry '$pair': drop the leading zero -- YAML reads 0-prefixed numbers as octal, so '$value' would become a different budget" ;;
        esac
        # LENGTH before arithmetic. `[ "$value" -gt ... ]` on a 20-digit number
        # errors with "integer expression expected" -- again not fatal inside an
        # `if` -- and the entry was emitted, after which go-yaml refuses to
        # unmarshal it into an int and the whole policy is discarded. Seven
        # digits cannot exceed the cap by enough to matter, and cannot overflow.
        if [ "${#value}" -gt 7 ]; then
            [ "$reset_glob" = 1 ] && set +f
            log_error "WVA_QUOTAS entry '$pair' is far above the maximum quota of 1048576 GPUs"
        fi
        # MaxQuotaValue in internal/config/quota_limiter.go. Above it the
        # controller rejects the entry, which costs the whole policy again.
        if [ "$value" -gt 1048576 ]; then
            [ "$reset_glob" = 1 ] && set +f
            log_error "WVA_QUOTAS entry '$pair' exceeds the maximum quota of 1048576 GPUs"
        fi
        # A REPEATED accelerator emits the key twice under one map. yq writes it
        # happily; go-yaml refuses the document ("mapping key already defined"),
        # which discards the whole policy -- the same catastrophic path as an
        # unparseable value, from a plausible typo.
        case "
$types" in
            *"
    ${name}: "*)
                [ "$reset_glob" = 1 ] && set +f
                log_error "WVA_QUOTAS names '$name' twice. One budget per accelerator type; two would emit a duplicate YAML key and the controller would reject the entire policy." ;;
        esac
        # An EXPLICIT zero is accepted -- an operator may mean "this type is
        # denied here" -- but it is said out loud. The IMPLICIT empty map gets a
        # five-line refusal for exactly the same outcome, and treating the two
        # differently in silence is how `H200=0` came to read as a budget.
        if [ "$value" = "0" ]; then
            log_warning "WVA_QUOTAS gives '$name' a budget of 0, which DENIES it: any workload on '$name' will not scale up. Use -1 for no cap."
        fi
        types="${types}    ${name}: ${value}
"
        pairs_seen=$((pairs_seen + 1))
    done
    [ "$reset_glob" = 1 ] && set +f
    # ZERO pairs is not an empty WVA_QUOTAS, and the check for one did not catch
    # it: `WVA_QUOTAS="   "` and `WVA_QUOTAS=","` are non-empty, the loop runs no
    # iterations, and the entry came out with an accelerator map containing
    # NOTHING -- structurally valid, accepted by validateLimiters, and read by
    # QuotaForNamespace as a budget of zero for every type. Every managed
    # workload then stops scaling up, which is the precise failure that requiring
    # WVA_QUOTAS exists to prevent.
    if [ "$pairs_seen" -eq 0 ]; then
        log_error "WVA_QUOTAS is set but names no accelerator: '$WVA_QUOTAS'.
    An entry with an empty budget map is not unlimited -- it is zero for every accelerator, and every
    managed workload stops scaling up. Give it a budget: WVA_QUOTAS='H200=8'"
    fi

    # Namespace scope is keyed by namespace. `default` is the reserved
    # fall-through key and means "this much PER unlisted namespace", not a shared
    # pool — so a cluster-scoped controller managing ten tenants hands out ten
    # budgets, not one. Name the managed namespace instead whenever there is
    # exactly one, which is every namespace-scoped install.
    #
    # Resolved BEFORE the first line is printed. Nothing here may emit a partial
    # document: a caller that reads stdout and a failing status separately would
    # otherwise hold three valid-looking lines of an entry this function refused.
    local key="" install_scope
    if [ "$scope" = "namespace" ]; then
        if [ -n "$ns_key" ]; then
            # Given by a caller that KNOWS the key, and the cluster-policy path is
            # the one that does: `make enable-physical-limiter` publishes ONE
            # policy that every controller on the cluster reads, so keying it on
            # a single namespace would give that namespace the budget and every
            # other one zero. It sets `default`, the reserved per-unlisted-
            # namespace key, which is the only correct answer for a policy with
            # many readers.
            key="$ns_key"
            # Validated like every other input, because it is the HIGHEST-
            # precedence one: it goes straight into the document ahead of the
            # scope rules. As an environment variable it was neither validated
            # nor validatable -- `WVA_QUOTA_NS_KEY='evil: 1'` emitted YAML no
            # parser accepts, and a merely WRONG one (`oops`) left the managed
            # namespace unlisted with no `default` to fall through to, which
            # reads as a budget of zero and stops every workload. An ARGUMENT
            # cannot arrive from an operator's exported shell at all.
            case "$key" in
                *[!a-z0-9-]*|-*|*-|"")
                    log_error "the namespace key '$key' is not a Kubernetes namespace name (lowercase letters, digits and '-'). It is written verbatim into the policy, so an invalid one either breaks the document or silently lists a namespace nothing matches." ;;
            esac
        else
            # The SAME answer the rest of the installer gets. `${WVA_SCOPE:-cluster}`
            # stood here and took the opposite default: everywhere else an unset
            # WVA_SCOPE means `namespace` (wva_install_scope, and configuration.md
            # says so in as many words), so `deploy/install.sh` run directly with
            # no WVA_SCOPE keyed its quota on `default` while the comment below
            # said it would name the managed namespace.
            #
            # `|| exit 1`, because wva_install_scope reports a bad WVA_SCOPE
            # through log_error and that exit dies in the subshell. Unchecked, a
            # typo left install_scope empty, fell through to `default`, and the
            # cluster path -- which has no `set -e` -- published a policy under
            # the reserved key while printing the refusal and then SUCCESS.
            if declare -F wva_install_scope >/dev/null; then
                install_scope="$(wva_install_scope)" || exit 1
            else
                install_scope="${WVA_SCOPE:-namespace}"
            fi
            if [ "$install_scope" = "namespace" ]; then
                key="${WVA_WATCH_NS:-${WVA_NS:-}}"
            else
                key="default"
            fi
        fi
        # An empty key renders as a bare `:` and yq refuses the document -- and
        # the cluster-policy caller is not under `set -e`, so the refusal became
        # an EMPTY policy written to every target namespace, under "The quota
        # limiter is now in force for every WVA on this cluster."
        if [ -z "$key" ]; then
            log_error "a namespace-scoped quota needs the namespace to key on, and neither WVA_WATCH_NS nor WVA_NS is set.
    Set one, or pass WVA_QUOTA_SCOPE=cluster for a budget that is not keyed by namespace."
        fi
    fi

    # One line per argument rather than one format with embedded newlines: a `\n`
    # followed by a space is what the line-continuation guard in
    # lint-deploy-scripts looks for, because that is the shape a collapsed line
    # continuation leaves behind.
    printf '%s\n' '- name: install-quota' '  type: quota' "  scope: ${scope}"
    if [ "$scope" = "cluster" ]; then
        # Cluster scope caps the SUM across every namespace, so it is keyed by
        # accelerator type alone. Indented one level less than the namespace form.
        printf '  quotas:\n'
        printf '%s' "$types"
        return 0
    fi
    printf '%s\n' '  namespaceQuotas:' "    ${key}:"
    printf '%s' "$types" | sed 's/^/  /'
}

# warn_unadvertised_accelerators says when WVA_QUOTAS names an accelerator no
# node advertises.
#
# A budget keyed on a name nothing matches is not an error ANYWHERE: the entry
# validates, the controller accepts it, the "GPU limiter constructed" line
# prints -- and every accelerator the cluster does have is then unlisted, which
# QuotaForNamespace reads as a budget of zero. Measured: `WVA_QUOTAS='H20=8'` on
# a cluster of H200s installs clean and stops all scaling.
#
# Shared, because both write paths need it and only one had it. Warns; never
# refuses -- a cluster whose nodes this caller cannot list, or that labels its
# GPUs some way not listed here, must not be blocked from declaring a budget.
warn_unadvertised_accelerators() {
    [ -n "${WVA_QUOTAS:-}" ] || return 0
    local node_products unmatched=""
    # EVERY product key, not just the GPU Feature Discovery one.
    # internal/constants carries seven NVIDIA spellings plus AMD and
    # Habana, and on CoreWeave, GKE, EKS or any AMD cluster the GFD
    # key is absent -- so reading it alone left this check inert
    # exactly where the accelerator name is least predictable.
    node_products="$(kubectl get nodes -o json 2>/dev/null | jq -r '
        .items[].metadata.labels
        | (."nvidia.com/gpu.product", ."gpu.nvidia.com/model",
           ."cloud.google.com/gke-accelerator", ."eks.amazonaws.com/instance-gpu-name",
           ."amd.com/gpu.device-id", ."habana.ai/gaudi", ."accelerator")
        | select(. != null and . != "")' 2>/dev/null | sort -u || true)"
    if [ -n "$node_products" ]; then
        local q_pair q_name reset_q_glob=1
        case "$-" in *f*) reset_q_glob=0 ;; esac
        set -f
        for q_pair in $(printf '%s' "$WVA_QUOTAS" | tr ',' ' '); do
            q_name="${q_pair%%=*}"
            [ -n "$q_name" ] || continue
            # A whole TOKEN of the product name, not a substring.
            # Substring matching was silent on the very typo this
            # exists for: 'H20' IS a substring of 'NVIDIA-H200', and
            # 'A10' of 'A100' -- a different, real GPU. Node labels
            # are separated by - _ . ('NVIDIA-H100-80GB-HBM3') and WVA
            # resolves a short name out of them, so comparing against
            # each token keeps every correct name quiet while a typo
            # matches nothing. -x anchors it; -F keeps a name
            # containing a regex character from being one.
            printf '%s\n' "$node_products" | tr '\055_.' '\n\n\n' \
                | grep -qixF -- "$q_name" || unmatched="$unmatched $q_name"
        done
        [ "$reset_q_glob" = 1 ] && set +f
    fi
    if [ -n "$unmatched" ]; then
        log_warning "WVA_QUOTAS names accelerators this cluster does not advertise:${unmatched}"
        log_warning "  A budget keyed on a name nothing matches is not an error anywhere -- the entry is valid, the controller accepts it, and every accelerator you DO have is then unlisted, which the quota limiter reads as a budget of ZERO. Every managed workload stops scaling up."
        log_warning "  This cluster advertises:"
        printf '%s\n' "$node_products" | sed 's/^/      /' >&2
        log_warning "  WVA resolves a SHORT name from these; check what it logged: kubectl logs -n ${WVA_NS} deploy/wva-controller-manager | grep accelerator"
    fi
}

# policy_with_limiters <policy yaml> <limiters yaml> -- the policy with its
# limiters: list REPLACED by the given one, on stdout.
#
# Replaced, not merged: re-running the install with a different WVA_LIMITER must
# not leave both declared, because EffectiveLimiterMode collapses the list to one
# mode (quota wins) and the other entry is then silently unenforced.
#
# strenv, not env. `env()` parses the value as YAML and a multi-line document
# makes it fail -- `Error: EOF`, with no mention of the variable -- which aborted
# the install after it had already re-applied the shipped ConfigMap, leaving the
# policy with no limiters at all. Paired with limiter_entry_yaml here so the
# offline check exercises the same transform the install runs.
policy_with_limiters() {
    local out
    out="$(printf '%s\n' "$1" | LIMITERS_YAML="$2" yq '.limiters = (strenv(LIMITERS_YAML) | from_yaml)')" || {
        log_error "could not merge the limiter entry into the policy. The entry was:
$2"
    }
    # Empty is a failure too, and a worse one: the callers write what comes back,
    # so an empty result BLANKS data.default. Before this, a yq refusal on the
    # cluster-policy path -- which runs without set -e -- wrote an empty policy
    # to every target namespace and then reported the limiter in force.
    if [ -z "$out" ]; then
        log_error "merging the limiter entry produced an empty policy; refusing to write it"
    fi
    printf '%s\n' "$out"
}

# policy_declared_limiters <limiter type> <current policy yaml> -- the policy with this
# install's limiter declared, in POLICY_DECLARED. It does not RETURN on failure:
# every refusal goes through log_error, which exits -- which is the whole design
# below, and the reason a caller has no status to forget.
#
# A GLOBAL, not stdout, and that is the whole point of the function.
#
# Both halves above report failure through log_error, whose `exit 1` ends the
# SUBSHELL when they are called as `$(...)` -- so every caller had to remember
# `|| exit 1`, on every one of four call sites, and a caller that forgot got an
# empty string it then wrote over the policy. That is not hypothetical: it is
# the bug this file was created to fix, it was found again in review after the
# fix, and an audit got past two separate source-pattern assertions written to
# catch it. A caller cannot forget a check it does not have to make.
#
# So the composition happens HERE, in the caller's own shell, where log_error's
# exit is the caller's exit. What a caller gets is a variable that is set and
# correct, or a process that has already stopped.
POLICY_DECLARED=""
# The entry itself, so a caller can SHOW what it declared without rebuilding it.
LIMITER_ENTRY_DECLARED=""
policy_declared_limiters() {
    local ltype="$1" current="$2" ns_key="${3:-}" entry
    POLICY_DECLARED=""
    LIMITER_ENTRY_DECLARED=""
    # The type is an ARGUMENT, not read from the environment. The two callers
    # name it in different variables -- WVA_LIMITER for an install's own policy,
    # WVA_LIMITER_TYPE for the cluster policy -- and reading either here would
    # let one leak into the other's path: an admin with WVA_LIMITER exported in
    # their shell would silently redirect `make enable-physical-limiter`.
    entry="$(limiter_entry_yaml "$ltype" "$ns_key")" || exit 1
    if [ -z "$entry" ]; then
        log_error "the limiter entry came out empty; refusing to write it over the policy"
    fi
    LIMITER_ENTRY_DECLARED="$entry"
    POLICY_DECLARED="$(policy_with_limiters "$current" "$entry")" || exit 1
    if [ -z "$POLICY_DECLARED" ]; then
        log_error "the merged policy came out empty; refusing to write it"
    fi
}
