#!/usr/bin/env bash
#
# Optional install step: create a default KEDA ScaledObject for each llm-d model
# server, so a fresh install actually autoscales something.
#
# Requires vars: WVA_NS, WVA_DEFAULT_SO, WVA_DEFAULT_SO_NS,
#                WVA_DEFAULT_SO_PLAN, WVA_DEFAULT_SO_MIN, WVA_DEFAULT_SO_MAX.
# Requires funcs: log_info/log_success/log_warning/log_error, wva_install_scope.
#
# Why this exists: a ScaledObject is not decoration, it is the REGISTRATION. WVA
# has no watch and no listing — it learns which workloads it manages from the KEDA
# calls it receives. So an install with no ScaledObject anywhere is a controller
# that will never be asked about anything, sitting idle and looking healthy.
#
# It is built as plan-then-apply rather than one shot, because creating autoscaling
# objects across a cluster is not something to discover the shape of afterwards.
# The plan is a YAML file that carries its own documentation: every field it
# accepts is explained in the comments it is written with, so editing it needs
# nothing open next to it. It was a TSV, and a table has nowhere to say what a
# column means or which values a column accepts — so the file could only be edited
# by someone who had already read the docs, and the docs had to repeat it all.
#
# The same file is the interchange for the interactive path and the scripted one,
# so there is no capability that needs a terminal — which matters, because these
# install scripts are otherwise non-interactive and CI depends on that. It is
# printed as a table as well: YAML is what you edit, a table is what you read.
#
# WVA_DEFAULT_SO:
#   false (default)  do nothing
#   plan             discover, print the table, write the plan, STOP
#   edit             plan, open $EDITOR, confirm, apply     (needs a terminal)
#   true             discover and apply it all, no questions
# WVA_DEFAULT_SO_PLAN=<file>
#   With an existing file: skip discovery and apply exactly that, edits included.
#   Otherwise: where the generated plan is written (default: a temp file).
# WVA_DEFAULT_SO_NS: a namespace, "wva" for WVA's own, or "all" for every namespace
#   holding model servers. Defaults to what this install can reach: "all" when
#   cluster-scoped, its own namespace when namespace-scoped.
#

readonly SO_PLAN_HEADER=$'APPLY\tNAMESPACE\tKIND\tNAME\tMODELID\tMIN\tMAX\tCOST\tPOLICY'

# The variantCost an entry gets when nothing else decides one. It is WVA's own
# default for a trigger that omits the field, so writing it into every entry
# changes no behaviour — it makes the number visible and editable, which is what
# matters the first time a model gets a second variant.
readonly SO_DEFAULT_VARIANT_COST=10.0

# so_plan_preamble writes the plan's header comment: what every field means, and
# which values `apply` takes. It is generated with the plan and read in the editor,
# which is the whole point of the file being YAML.
so_plan_preamble() {
    cat <<'EOF'
# WVA ScaledObject plan. Nothing here has been applied yet.
#
# Edit this file, then apply exactly what you leave in it:
#
#     make scaledobjects-apply WVA_DEFAULT_SO_PLAN=<this file>
#
# Commented lines are never read back. Anything informational is written as a
# comment for exactly that reason: editing it would change what you were told,
# not what gets applied. Each entry's `apply:` line carries the values that entry
# actually accepts — `adopt` only appears where there is something to adopt.
#
# apply          Required. One of:
#                  yes    create a ScaledObject for this workload
#                  no     leave the workload alone
#                  adopt  it already has a ScaledObject — repoint that one at WVA
#                         instead of adding a second. Two ScaledObjects on one
#                         target is two HPAs writing the same replica count.
#                         Offered only for workloads that have one.
#
# modelID        Required. What the container serves: --served-model-name, or
#                --model where there is no served name. It is also the grouping
#                key — entries sharing a modelID are sized against each other, so
#                a wrong one mis-scales both of them.
#
# minReplicas    The bounds KEDA holds this workload between; WVA decides within
# maxReplicas    them. Defaults 1 and 10. minReplicas: 0 parks the workload when
#                idle and costs its next request a cold start. maxReplicas is the
#                only ceiling on this workload unless a GPU limiter is configured.
#
# variantCost    The relative price of one replica of this variant. Only the ratio
#                between variants of the same model matters, so with one variant
#                per model it changes nothing and 10.0 is as good as any number.
#                Where a model has two, this is what makes WVA prefer one: give
#                the cheaper variant the smaller cost.
#
# scalingPolicy  Optional, and commented out because absent is the right value for
#                most installs. It names a reusable TIER — "interactive",
#                "standard", "batch" — defined in the scaling-policy ConfigMap,
#                whose thresholds this variant scales under. Leave it out and the
#                cluster's own default applies, which an admin can change for
#                every workload at once; name one here and this workload stops
#                following that. A name no tier matches falls back to the default
#                silently, so an entry naming one is a claim that can go stale.
#
# namespace      Where the workload is; with kind and name, what the ScaledObject
# kind           will target.
# name
#
# The comments each entry carries:
#
#   scaledObject   the ScaledObject already scaling this workload, if any. Its
#                  name is what `apply: adopt` would repoint.
#   inferencePool  the EPP queue this workload sits behind, resolved by matching
#                  pod labels against each pool's selector — the same way WVA
#                  resolves it. "(none)" means no pool selects these pods.
#
# After the plan entries there is a second section, `warmPools:`. It groups the
# workloads above by what a warm pool would have to provide and suggests one per
# group; applying an entry there creates that pool. Everything in it is written
# `apply: "no"`, because a pool holds its accelerators from the moment it exists.
# Each field carries its own comment. Nothing is created unless you say yes.
EOF
}

# so_plan_entry writes one plan entry, with its note (if any) as the comment above
# it. Notes are comments rather than a column so that they survive editing and can
# be a sentence: the reason a row says `no` is the thing a reader most needs.
so_plan_entry() {
    local apply="$1" ns="$2" kind="$3" name="$4" model="$5" min="$6" max="$7" cost="$8"
    local policy="$9" pool="${10}" existing="${11}" note="${12:-}"
    # The values this entry accepts, not the values the field accepts: offering
    # `adopt` for a workload with nothing to adopt invites a choice that can only
    # be a mistake, and offering it where there is something is the one place
    # anybody would look for it.
    local choices="yes | no"
    [ -z "$existing" ] || choices="yes | no | adopt"
    echo ""
    # One line per note. Two unrelated problems joined by a space read as one
    # long sentence, and the second half -- often the expensive one -- is where a
    # reader gives up.
    #
    # RS-separated RECORDS, not US: this file already uses US as the FIELD
    # separator for plan rows, so a note containing one splits the row it travels
    # in and loses everything after it.
    # The plan is read back through `yq -o=json`, which drops comments, so the
    # number of comment lines above an entry is free.
    if [ -n "$note" ]; then
        # printf '%s\n', not '%s': `while read` returns non-zero on a final line
        # with no terminator and so never runs its body for it. Without the \n a
        # single note produced NO output at all, and two produced only the first
        # -- the last note was always the one silently dropped.
        printf '%s\n' "$note" | tr '\036' '\n' | while IFS= read -r so_note_line; do
            [ -n "$so_note_line" ] || continue
            printf '  # note: %s\n' "$so_note_line"
        done
    fi
    # apply and modelID are quoted: YAML reads a bare yes/no/on/off as a boolean
    # under some parsers, and a model called `3.5` or `1e6` as a number. Both would
    # arrive here as something that is not the text anybody typed.
    printf '  - apply: "%s"  # %s\n' "$apply" "$choices"
    printf '    namespace: %s\n' "$ns"
    printf '    kind: %s\n' "$kind"
    printf '    name: %s\n' "$name"
    printf '    modelID: "%s"\n' "$model"
    printf '    minReplicas: %s\n' "$min"
    printf '    maxReplicas: %s\n' "$max"
    # Quoted, so an edit keeps what you typed: unquoted 10.0 is a YAML float, and
    # a float that round-trips through JSON comes back as "10".
    printf '    variantCost: "%s"\n' "$cost"
    if [ -n "$policy" ]; then
        printf '    scalingPolicy: "%s"\n' "$policy"
    else
        printf '    # scalingPolicy: "standard"\n'
    fi
    # Informational, and comments so they stay that way: neither is read back,
    # so changing one cannot change what happens behind the reader's back.
    [ -z "$existing" ] || printf '    # scaledObject: %s\n' "$existing"
    printf '    # inferencePool: %s\n' "${pool:-(none)}"
    # Remember what was offered, so the warm-pool block can be grouped from the
    # same workloads rather than re-discovering them. Appended to a FILE because
    # every caller runs inside a `while read` loop, and a variable set there does
    # not reliably survive back to so_discover.
    [ -z "${SO_WARMPOOL_ROWS:-}" ] || printf '%s|%s|%s\n' "$ns" "$kind" "$name" >> "$SO_WARMPOOL_ROWS"
}

# so_plan_rows echoes one row per plan entry, fields separated by US (\037): the
# file is YAML for whoever edits it, and a record stream for everything that
# consumes it.
#
# NOT tab-separated. Tab is an IFS *whitespace* character, so `IFS=$'\t' read`
# collapses a run of them into one delimiter and an empty field simply vanishes —
# every later field then arrives one place to the left. A workload whose model
# could not be read has an empty modelID, so it got a ScaledObject whose modelID
# was its minReplicas: the value 1, registered as a model name, silently. US is
# not IFS whitespace, so empty fields survive; values are scrubbed of it below,
# though nothing a Kubernetes object can hold contains one.
#
# yq converts and jq selects, rather than yq doing both: expressing "this field or
# this default, joined" in yq's filter language needs quoting that is its own
# hazard.
#
# An unparseable plan is fatal and says why. The alternative — skipping what could
# not be read — would apply part of a plan, and on this file a partial apply means
# creating autoscaling objects for a subset nobody chose.
so_plan_rows() {
    local file="$1" json
    if ! json=$(yq -o=json '.' "$file" 2>&1); then
        log_error "$file is not valid YAML, so nothing was applied: $json"
    fi
    # An EMPTY plan is not a broken plan. so_write_plan writes `plan:` with no
    # entries when the namespace holds no model servers yet — the ordinary state
    # of a namespace where the controller was installed before the workloads —
    # and yq parses that to null. Treating null as unreadable made the documented
    # sequence print an ERROR from the read-only planning step and then fail
    # `scaledobjects-apply` with exit 2 on the very file it had just written.
    # Absent key: still an error, because that file is not a plan at all.
    case "$(printf '%s' "$json" | jq -r 'if (.plan | type) == "array" then "ok" elif (has("plan") and .plan == null) then "empty" else "bad" end' 2>/dev/null)" in
        ok)    : ;;
        empty) return 0 ;;
        *)     log_error "$file has no 'plan:' list. It must keep a top-level plan: key holding one entry per workload — see the comments at the top of the file." ;;
    esac
    printf '%s' "$json" | jq -r --arg cost "$SO_DEFAULT_VARIANT_COST" '
        .plan[]
        | [ (if (.apply | type) == "boolean"
             then (if .apply then "yes" else "no" end)
             else (.apply // "" | tostring) end | ascii_downcase),
            (.namespace // ""), (.kind // ""), (.name // ""), (.modelID // ""),
            (.minReplicas // 1 | tostring), (.maxReplicas // 10 | tostring),
            (.variantCost // $cost | tostring), (.scalingPolicy // "") ]
        | map(tostring | gsub("[
	]"; " "))
        | join("")'
}

# What marks a workload as an llm-d model server.
#
# It is matched against the POD TEMPLATE's labels, and the object's own only as a
# fallback — NOT with `kubectl get -l`, which only ever looks at the object. llm-d
# puts these labels where they do the work: on the pod template, because that is
# what the InferencePool's EPP selects on, and on the selector. The Deployment
# itself carries none of them. So `-l llm-d.ai/inferenceServing=true` matched
# nothing on a real install, and the scan reported "no llm-d model servers found"
# — an install that then autoscales nothing, and says only that it found nothing
# to autoscale.
readonly SO_SERVING_MARKER='llm-d.ai/inferenceServing=true'

# ...and llm-d's own guides do not set that label at all.
#
# guides/recipes/modelserver/base/single-host/default/decode-deployment.yaml sets
# `llm-d.ai/role: decode` and nothing else; guides/optimized-baseline adds
# llm-d.ai/model, llm-d.ai/guide and the accelerator pair. inferenceServing
# appears nowhere in that render path, so on a namespace built from the guide the
# WVA install guide points readers at, discovery matched NOTHING: preflight said
# "holds NO llm-d model servers", the plan came out empty, and the install
# reported healthy while scaling nothing. The label does exist on workloads from
# other deployment paths, which is why this went unnoticed.
#
# The role IS the reliable marker, and llm-d sets it deliberately -- the same
# reasoning SO_FMA_ROLE_MARKER already relies on one label below.
#
# Only the two roles that hold an engine and serve. `requester` is deliberately
# absent: an FMA requester holds no engine, and so_discover_fma_requesters
# handles it separately and on purpose.
readonly SO_SERVING_ROLE_MARKERS='llm-d.ai/role=decode llm-d.ai/role=prefill'

# so_serving_markers echoes every label that marks a workload as an llm-d model
# server, one per line. ANY one of them is enough.
so_serving_markers() {
    printf '%s\n' "$SO_SERVING_MARKER"
    printf '%s\n' $SO_SERVING_ROLE_MARKERS
}

# so_serving_markers_json is the same list as a JSON array, for the jq scans.
so_serving_markers_json() {
    so_serving_markers | jq -R . | jq -s -c .
}

# so_is_serving succeeds when any marker appears in the labels it is given.
# Takes the label strings already flattened to "k=v k=v" by the callers.
so_is_serving() {
    local labels=" $* " marker
    while IFS= read -r marker; do
        case "$labels" in
            *" $marker "*) return 0 ;;
        esac
    done <<EOF
$(so_serving_markers)
EOF
    return 1
}

# Fast Model Actuation splits a model server across two pods: a REQUESTER
# Deployment that carries the llm-d identity, and LAUNCHER pods -- owned by a
# LauncherConfig -- that hold the GPU and run vLLM. The dual-pods controller
# pairs them, stamping the serving labels onto whichever launcher is currently
# bound (the requester grows a dual-pods.llm-d.ai/dual label naming it).
#
# The launchers really do serve. Measured on pokprod001 during a benchmark, one
# launcher ran at 143 requests running, 61 waiting, KV cache 99.9% full, while
# the decode pod took a fraction of the traffic: at peak EPP dispatched 27.4
# req/s across nine launchers against 5.9 req/s to the decode pod, so roughly
# 82% of the load landed where WVA cannot see it.
#
# WVA cannot use any of it. A launcher pod's ownerReferences lead to a
# LauncherConfig, not to a Deployment or LWS under a ScaledObject, so the walk in
# CollectReplicaMetrics ends without a scale target and the pod is skipped --
# counted in wva_pod_mapping_miss_total, and otherwise silent. Point a
# ScaledObject at the requester and the controller says so every cycle:
#
#   has 1 ready pod(s) but none attributed
#
# That is the durable limitation: an FMA variant has no workload WVA can both
# scale and measure. The requester can be scaled but reports nothing; the
# launchers report everything but belong to no scale target.
#
# Scraping is a second, independent failure, and it is easy to cause by accident.
# A launcher declares no container ports and serves metrics on :8000 (the decode
# pod uses :8200), so a PodMonitor that selects its endpoint by port NAME
# resolves nothing and generates no target at all -- not a failing target, no
# target. llm-d-benchmark gets this right when a scenario sets `fma.enabled`: it
# renders the endpoint with a relabeling that forces __address__ to <podIP>:8000.
# But that PodMonitor is named vllm-<model>, and a later standup of a NON-FMA
# guide into the same namespace renders the same name with `port: metrics` and
# overwrites it. The FMA stack keeps serving; its metrics just stop being
# collected. That is what happened on pokprod001 -- an FMA guide at 09:25Z, a
# workload-autoscaling guide over the top at 09:26Z, and from then on `up{}`
# listed the decode pod, EPP and WVA, and no launcher.
#
# Consequence for anyone reading a plan: in a namespace where FMA is present, WVA
# measures only the decode Deployment, and the GPUs behind the launchers are
# counted nowhere. See docs/deployment/operations.md, "FMA launcher pods".
#
# The requester is not found by SO_SERVING_MARKER: it carries the hyphenated
# llm-d.ai/inference-serving, not the camelCase llm-d.ai/inferenceServing that
# marks a model server. Its role label is the reliable marker, and it is what
# llm-d sets deliberately.
readonly SO_FMA_ROLE_MARKER='llm-d.ai/role=requester'

# so_fma_requester echoes the FMA requester Deployment serving $2's model in $1,
# or nothing. Matched on the role label AND the model label, so a namespace
# running two models retargets each to its own requester rather than to whichever
# one happens to be listed first.
so_fma_requester() {
    local ns="$1" model_label="$2"
    [ -n "$model_label" ] || return 0
    kubectl get deployments -n "$ns" -o json 2>/dev/null \
      | jq -r --arg m "$model_label" '
          .items[]
          | (.spec.template.metadata.labels // {}) as $l
          | select($l["llm-d.ai/role"] == "requester" and $l["llm-d.ai/model"] == $m)
          | .metadata.name' \
      | head -1
}

# Where each kind keeps its pod template, as a jq path. An LWS may omit
# leaderTemplate entirely, and plenty of workloads have a container with no args
# — getpath and // handle both, which a go-template does not: `range` over a
# missing field is an execution error that empties the WHOLE listing, and this
# scan reads every workload in the namespace, not only the model servers.
readonly SO_POD_PATH_DEPLOYMENT='["spec","template"]'
readonly SO_POD_PATH_LWS='["spec","leaderWorkerTemplate","leaderTemplate"]'

# so_fma_isc_model echoes the model ID an FMA requester Deployment ($2 in $1)
# serves, or nothing.
#
# A requester runs `/app/requester` with no args, so the container-args route that
# works for every other workload finds nothing here. The model is one hop away:
# the requester's POD TEMPLATE carries an annotation naming its
# InferenceServerConfig, and the ISC's modelServerConfig.options is the vLLM
# command line the launcher will run.
#
#   Deployment/fma-requester-x
#     .spec.template.metadata.annotations["dual-pods.llm-d.ai/inference-server-config"]
#       -> InferenceServerConfig/y
#            .spec.modelServerConfig.options = "--model Qwen/Qwen3-0.6B --enable-sleep-mode …"
#
# Those options are parsed by so_model_id, the same parser used on container args,
# so --served-model-name still wins over --model and both spellings are accepted.
#
# Every step degrades to empty rather than to a guess, and the caller records that
# as `apply: no`. In particular the ISC is read with `get ... 2>/dev/null`, which
# covers the two cases that would otherwise be indistinguishable from a real
# answer: the CRD not being installed at all, and the annotation naming an ISC
# that no longer exists. Note llm-d.ai/model on the pod is NOT a fallback — it is
# a sanitized DNS-safe form (`qwen-qwe-694d2b87-en3-0-6b`, not `Qwen/Qwen3-0.6B`),
# and a ScaledObject carrying it would group the variant under a model no metric
# series reports.
so_fma_isc_model() {
    local ns="$1" deploy="$2" isc options
    [ -n "$ns" ] && [ -n "$deploy" ] || return 0

    # jq, not jsonpath. The annotation key carries dots AND a slash
    # (dual-pods.llm-d.ai/inference-server-config), which jsonpath needs escaped
    # in a way that is easy to get subtly wrong and that fails by returning
    # nothing rather than by erroring — indistinguishable, here, from a workload
    # that genuinely has no annotation. A jq index takes the key literally.
    isc=$(kubectl get deployment -n "$ns" "$deploy" -o json 2>/dev/null \
        | jq -r '.spec.template.metadata.annotations["dual-pods.llm-d.ai/inference-server-config"] // empty' 2>/dev/null)
    [ -n "$isc" ] || return 0

    options=$(kubectl get inferenceserverconfig -n "$ns" "$isc" -o json 2>/dev/null \
        | jq -r '.spec.modelServerConfig.options // empty' 2>/dev/null \
        | tr '\n\r' '  ')
    [ -n "$options" ] || return 0

    so_model_id "$options"
}

# so_serving_workload_for_model echoes the name of a modelservice workload in $1
# serving the model LABEL $2 — the decode/prefill half — or nothing.
#
# Discovery uses it to tell two namespace shapes apart. When a modelservice
# Deployment and an FMA requester both serve one model, the modelservice one is
# the target and the requester must not produce a second entry pointed at the same
# model. When only the requester exists, there is nothing else to scale and the
# FMA entry is the only one that can be made.
#
# Matched on the serving marker AND the model label, both on the pod template,
# which is where llm-d puts them and where the main discovery loop reads them.
so_serving_workload_for_model() {
    local ns="$1" model_label="$2" kind resource pod
    [ -n "$model_label" ] || return 0
    for kind in Deployment LeaderWorkerSet; do
        resource='deployments'; pod="$SO_POD_PATH_DEPLOYMENT"
        if [ "$kind" = "LeaderWorkerSet" ]; then
            resource='leaderworkersets'; pod="$SO_POD_PATH_LWS"
        fi
        kubectl get "$resource" -n "$ns" -o json 2>/dev/null \
          | jq -r --argjson p "$pod" --arg m "$model_label" \
                 --argjson markers "$(so_serving_markers_json)" '
              .items[]
              | . as $o
              | (getpath($p) // {}) as $t
              | (($t.metadata.labels // {}) + ($o.metadata.labels // {})) as $l
              | select($l | to_entries
                       | any((.key + "=" + (.value|tostring)) as $kv
                             | ($markers | index($kv)) != null))
              | select(($l["llm-d.ai/model"] // "") == $m)
              | $o.metadata.name'
    done | head -1
}

# so_model_id echoes the model a serving container runs: --served-model-name where
# the workload sets one (it is the name clients and the EPP use), else --model,
# else the positional model of `vllm serve <model>`.
# Both "--flag value" and "--flag=value" are accepted; both appear in the wild.
#
# The positional form is not an edge case: it is what llm-d's own guides emit.
# guides/optimized-baseline renders
#
#     command: ["vllm", "serve"]
#     args: ["Qwen/Qwen3-32B", "--tensor-parallel-size=2", ...]
#
# so neither flag is present, and reading flags alone returned empty for the
# flagship guide -- every workload in the plan written `apply: no`, "the model
# could not be read", on a namespace where the model is perfectly well defined.
# The caller therefore passes command AND args, so `serve` is always visible to
# anchor on rather than guessed around.
#
# Empty output means the model could not be determined. The caller must record that
# and skip rather than guess: a ScaledObject with the wrong modelID groups a
# workload with a model it does not serve, and mis-scales both.
so_model_id() {
    local args="$1" flag tok next take
    for flag in --served-model-name --model; do
        take=""
        for tok in $args; do
            if [ -n "$take" ]; then
                case "$tok" in
                    --*) : ;;
                    *) echo "$tok"; return ;;
                esac
                take=""
            fi
            case "$tok" in
                "$flag"=*) next="${tok#*=}"; [ -n "$next" ] && { echo "$next"; return; } ;;
                "$flag")   take=1 ;;
            esac
        done
    done
    # The token after `serve`. This covers both shapes llm-d emits, because the
    # caller passes command AND args: `command: ["vllm","serve"]` with the model
    # first in args, and `sh -c "vllm serve <model> ..."` from the modelservice
    # chart, where the model sits several words into a single args string.
    #
    # Anchoring on `serve` is the whole point. A bare "first non-flag token"
    # fallback was tried and removed: it answered `python` for
    # `python -m sglang.launch_server --model-path X`, `/bin/sh` for an
    # entrypoint wrapper and `ray` for a ray head -- all of them workloads that
    # DO carry a serving label now that role markers are matched, so all of them
    # would have been given a confidently wrong modelID. Empty is the correct
    # answer there, and the caller already knows what to do with it.
    take=""
    for tok in $args; do
        if [ -n "$take" ]; then
            case "$tok" in
                -*) : ;;
                *) echo "$tok"; return 0 ;;
            esac
            take=""
        fi
        # Not `[ ... ] && take=1`: as the last command in the loop body that
        # leaves the loop -- and so this function -- exiting non-zero whenever
        # the final token is not `serve`. The caller assigns from a command
        # substitution under `set -e`, so an unreadable model took the whole
        # install down instead of being recorded as `apply: no`.
        if [ "$tok" = "serve" ]; then take=1; fi
    done
    # Nothing matched. Empty output, exit 0: "could not be determined" is a
    # normal outcome the plan reports, not a failure.
    return 0
}

# so_model_source echoes WHERE a serving container gets its weights: the argument
# of --model, or the positional model of `vllm serve <model>`.
#
# Deliberately NOT so_model_id, and the difference is not cosmetic.  so_model_id
# prefers --served-model-name, because that is the name clients and the EPP use --
# but it is an ADVERTISED NAME, not a source.  llm-d's own chart, with
# uriProtocol: pvc, renders
#
#     vllm serve /model-cache/models/Qwen/Qwen3-0.6B --served-model-name Qwen/Qwen3-0.6B
#
# so the served name is a repository id while the weights are already on the
# volume.  Asking so_model_id whether this workload downloads its weights gets the
# answer "yes, from Qwen/Qwen3-0.6B", and the patch then mounts a second cache
# into a pod that reads from one -- on the single most common llm-d layout there
# is.  The source is the only field that answers the question.
so_model_source() {
    local args="$1" tok next take result="" had_f=0
    # Pathname expansion off. `for tok in $args` splits AND globs, and these are
    # someone else's shell scripts: a `rm -f /tmp/*.log` inside an entrypoint
    # expands against whatever directory make was run from, and a filename from
    # the repo root becomes the answer to "where do the weights come from".
    case $- in *f*) had_f=1 ;; *) set -f ;; esac
    take=""
    for tok in $args; do
        if [ -n "$take" ]; then
            case "$tok" in
                --*) : ;;
                *) result="$tok"; break ;;
            esac
            take=""
        fi
        case "$tok" in
            # --model-path is SGLang. Leaving it out gave every SGLang engine a
            # silent pass on the weights question -- worse than the code this
            # replaced, and on an engine so_workload_gaps goes out of its way to
            # detect by image and by sglang.launch_server.
            --model=*|--model-path=*) next="${tok#*=}"; [ -n "$next" ] && { result="$next"; break; } ;;
            --model|--model-path)     take=1 ;;
        esac
    done
    if [ -z "$result" ]; then
        # Falls through to the same `serve` anchor so_model_id uses, and for the
        # same reasons -- see the comment there.
        take=""
        for tok in $args; do
            if [ -n "$take" ]; then
                case "$tok" in
                    -*) : ;;
                    *) result="$tok"; break ;;
                esac
                take=""
            fi
            if [ "$tok" = "serve" ]; then take=1; fi
        done
    fi
    [ "$had_f" -eq 1 ] || set +f
    [ -n "$result" ] && printf '%s\n' "$result"
    return 0
}

# so_pool echoes the InferencePool whose selector matches a workload's pod labels,
# which is how WVA itself resolves it — the pool is derived, never declared. Shown
# in the plan for orientation only: it tells you which EPP queue a workload sits
# behind, and an empty column is a workload no pool has adopted.
so_pool() {
    local ns="$1" labels="$2" pool selector matched kv key value
    # Two selector shapes: inference.networking.k8s.io/v1 nests it under
    # matchLabels, the older x-k8s.io group had a bare map. Read both, as the
    # controller does — a plan that showed no pool because of an API version would
    # look exactly like a workload no pool has adopted.
    local tmpl='{{range .items}}{{.metadata.name}} {{if .spec.selector.matchLabels}}{{range $k,$v := .spec.selector.matchLabels}}{{$k}}={{$v}},{{end}}{{else}}{{range $k,$v := .spec.selector}}{{$k}}={{$v}},{{end}}{{end}}{{"\n"}}{{end}}'
    while read -r pool selector; do
        [ -n "$pool" ] || continue
        matched=yes
        for kv in $(echo "$selector" | tr ',' ' '); do
            [ -n "$kv" ] || continue
            key="${kv%%=*}"; value="${kv#*=}"
            case " $labels " in
                *" $key=$value "*) : ;;
                *) matched=""; break ;;
            esac
        done
        [ -n "$matched" ] && { echo "$pool"; return 0; }
    done < <(kubectl get inferencepools -n "$ns" -o go-template="$tmpl" 2>/dev/null)
    # No pool adopted this workload. That is a normal answer — the column is shown
    # for orientation — so it must not be a non-zero status: the caller assigns
    # this in a command substitution under `set -e`, and a "no match" would abort
    # the whole scan on the first workload no pool selects.
    return 0
}

# so_target_namespaces echoes the namespaces to scan, one per line.
#
# stdout here is DATA, consumed by `for ns in $(so_target_namespaces)`. Anything
# else written to it becomes a namespace name — which is what happened while the
# log helpers wrote to stdout: the warning below was split into words and each was
# used as a namespace, so every lookup failed into 2>/dev/null and the scan
# reported "no model servers found".
#
# The DEFAULT follows the install's scope, because the scope already decides what
# this controller can reach:
#
#   cluster-scoped    every namespace holding model servers — it can manage them all
#   namespace-scoped  its own namespace — it restricts its cache to it and can
#                     manage nothing else, so scanning anywhere else would only
#                     produce ScaledObjects it will be called about and cannot read
#
# It used to default to NAMESPACE, which was wrong in both directions: it scanned one
# namespace on a cluster-scoped install that could have managed them all, and it
# scanned a namespace a namespace-scoped install cannot see.
# so_drain_note reports whether a scale target can lose a replica without cutting
# live requests, and returns a note when it cannot.
#
# Reported against a real run: four scale-downs produced four bursts of
# ClientPayloadError, each ending 25-30s after its reduction -- 39 truncated
# streams in one benchmark. The serving Deployment had the default 30s grace
# period and no preStop hook, so a removed replica took its open responses with
# it. Long generations are the normal case here, which is what makes this worth
# saying out loud rather than leaving to be discovered.
#
# A note, not a refusal, and not a patch. The pod spec belongs to the model
# server's chart -- on llm-d it is `managed-by: Helm`, release <model>-ms -- so
# anything this installer wrote there would be reverted by the next
# `helm upgrade`, and WVA does not own the workloads it scales. The plan file is
# the right place to say it: registering a ScaledObject is the moment scale-down
# becomes possible, and it is where the operator is already deciding apply:
# yes|no|adopt.
# so_weights_note reports when a scale target will re-download its weights on
# every scale-up, and returns a note when it will.
#
# Registering a ScaledObject is the moment new replicas start appearing on their
# own, so it is the moment this stops being a one-off install cost. A model
# served by repo id with no persistent volume fetches its weights from Hugging
# Face for every pod, every restart and every scale-up -- slow, rate-limitable,
# and a hard dependency on an external service being reachable from a cluster
# that may not want one.
#
# The signal is the served model path, not the presence of a volume. Weights can
# legitimately be baked into the image, in which case there is no PVC and no
# download either, so "no PVC" alone would cry wolf. A LOCAL PATH means the
# weights are already on the node; a repo id (org/name) means vLLM resolves it
# through the Hub. Only the second case with no persistent volume anywhere is
# worth a note.
#
# llm-d's modelservice chart already does the right thing by default --
# uriProtocol: pvc, mounted at /model-cache -- so on a stock llm-d install this
# stays silent. It fires for a workload that was pointed at the Hub directly.
so_weights_note() {
    local ns="$1" resource="$2" name="$3" pod="$4"
    local gaps engine prestop grace pvcs onvol argv model

    # One detector, shared with the patch emitter. This function used to carry
    # its own copy and the copies disagreed: it accepted "a PVC exists anywhere
    # in the pod" as proof weights persist, and its local-path test matched any
    # absolute path in the command line -- so the `/bin/sh -c "vllm serve ..."`
    # form the modelservice chart emits matched on `/bin/sh` and suppressed the
    # warning for every deployment the chart produces.
    if ! gaps="$(so_workload_gaps "$ns" "$resource" "$name" "$pod")"; then
        printf '%s' "Could not read the pod spec, so whether weights persist across scale-ups is unknown -- this is not a report that they do."
        return 0
    fi
    IFS=$'\037' read -r engine prestop grace pvcs onvol argv <<< "$gaps"
    [ -n "$engine" ] || return 0
    [ "${onvol:-0}" -eq 1 ] && return 0

    # A local path is already-resident weights. Parsed by so_model_source, which
    # reads the SOURCE rather than the advertised name, and anchors on `serve`
    # rather than looking for a slash anywhere.
    model="$(so_model_source "$argv")" || model=""
    case "$model" in
        ""|/*) return 0 ;;
    esac

    printf '%s' "Weights do not persist: ${engine} downloads outside any volume it has mounted, so every scale-up re-fetches ${model} from Hugging Face. That is a per-replica cost on the path this ScaledObject is about to start exercising, and a dependency on the Hub being reachable. llm-d's modelservice chart mounts a model cache by default (uriProtocol: pvc at /model-cache); see docs/deployment/operations.md#weights-and-the-model-cache."
    return 0
}

# ---------------------------------------------------------------------------
# Workload readiness: the pod-spec settings that decide whether autoscaling this
# workload is safe, reported by the plan and emitted here as a patch.
#
# WVA does not write them. The pod spec belongs to the model server's chart --
# on llm-d, `managed-by: Helm`, release <model>-ms -- so anything this installer
# applied there is reverted by the next `helm upgrade`, silently, and the
# workload goes back to truncating streams with nothing to say so. Emitting the
# patch puts the change where ownership already is.
#
# WVA_WORKLOAD_PATCH_APPLY=true applies it anyway, for a cluster where that is
# the trade someone wants. It says what it is doing and why it may not last.
# ---------------------------------------------------------------------------

# so_workload_patch_index lists the documents in an emitted patch file as
# "<namespace> <name>", one per line.
#
# A SPACE, not the \037 used elsewhere in this file. Both fields are RFC 1123
# labels -- lowercase alphanumerics and hyphens -- so a space cannot appear in
# either, and yq does not read the  escape the way jq does: it produced one
# unsplittable field per line, the consumer's `read` left the name empty, and the
# apply loop ran zero times while still reporting the file it had written.
#
# `yq eval`, NOT `eval-all`. eval-all evaluates the expression ONCE across the
# whole stream, so `[...] | @tsv` returns a single line with every document's
# fields concatenated -- the consumer's `read` then ran once, patched only the
# first workload, and the success message reported the full count anyway. Four
# reviewers found it independently and it was invisible in testing because the
# default benchmark scenario runs one decode Deployment.
so_workload_patch_index() {
    yq eval '[.metadata.namespace, .metadata.name] | join(" ")' "$1" 2>/dev/null \
        | grep -v '^$' || true
}

# so_workload_patch_one applies the DRAIN half of one document, and returns
# non-zero when it does not. 2 means it deliberately applied nothing.
#
# THE WEIGHTS HALF IS NEVER APPLIED, only emitted. Three reasons, and the first
# is fatal on its own:
#
#   * A strategic merge cannot REPLACE a volume that already exists under the
#     same name. `volumes` has patchStrategy `merge,retainKeys`, and the
#     retainKeys directive is generated by `kubectl apply`, not by
#     `kubectl patch --type=strategic` -- so the patch merges INTO the existing
#     volume and the API server rejects the whole object:
#         spec.template.spec.volumes[0].persistentVolumeClaim: Forbidden:
#         may not specify more than 1 volume type
#     `model-storage` at `/model-cache` is llm-d's own naming, so an llm-d render
#     with an emptyDir cache -- exactly the layout the weights half exists for --
#     failed every time, and took the DRAIN half down with it, leaving the
#     workload with no hook at all. That is the one outcome this must never
#     produce.
#   * It needs a PersistentVolumeClaim that may not exist, and pods whose claim
#     is missing stay Pending. Size and storage class are cluster decisions.
#   * It changes where an engine reads its weights from. That belongs to whoever
#     owns the chart, not to a command someone ran to fix draining.
#
# The drain half has none of these properties: `containers` merges on `name`, and
# adding a lifecycle hook and a grace period cannot conflict with anything.
# so_workload_weights_safe reports whether the weights half of a document can be
# applied to the live object without destroying it.
#
# The blanket refusal this replaces was right about the danger and wrong to be
# unconditional. `volumes` merges with `retainKeys`, and only `kubectl apply`
# generates that directive -- so `kubectl patch --type=strategic` MERGES into a
# volume of the same name rather than replacing it, and the API server rejects
# the whole object:
#     spec.template.spec.volumes[0].persistentVolumeClaim: Forbidden:
#     may not specify more than 1 volume type
# taking the drain half down with it.
#
# But that only happens when the name is ALREADY TAKEN. A workload with no such
# volume takes the patch cleanly, and that is the workload the weights half is
# for. So the test is the collision, not the operation.
#
# $1 the live object JSON, $2 the volume name, $3 the mount path, $4 the claim.
so_workload_weights_safe() {
    local live="$1" volname="$2" mountpath="$3" claim="$4" ns="$5" reason
    reason="$(printf '%s' "$live" | jq -r --arg v "$volname" --arg m "$mountpath" '
        (.spec.template.spec // {}) as $t
        | [ ($t.volumes[]? | select(.name == $v) | "a volume named \($v) already exists"),
            ($t.containers[]?.volumeMounts[]? | select(.mountPath == $m) | "something is already mounted at \($m)")
          ] | first // ""' 2>/dev/null)" || return 1
    if [ -n "$reason" ]; then
        printf '%s' "$reason"
        return 1
    fi
    # The claim has to exist, or every pod the rollout creates stays Pending on a
    # volume that cannot be attached -- an outage in place of a warning.
    if ! kubectl get pvc "$claim" -n "$ns" >/dev/null 2>&1; then
        printf '%s' "PersistentVolumeClaim $claim does not exist (or cannot be read)"
        return 1
    fi
    return 0
}

# so_workload_weights_can_apply answers so_workload_weights_safe for a live
# workload, and says out loud why the answer is no.
so_workload_weights_can_apply() {
    local ns="$1" name="$2" doc="$3" live volname mountpath claim why
    volname="$(yq eval '.spec.template.spec.volumes[0].name // ""' "$doc" 2>/dev/null)"
    [ -n "$volname" ] || return 1          # no weights half in this document
    mountpath="$(yq eval '.spec.template.spec.containers[0].volumeMounts[0].mountPath // ""' "$doc" 2>/dev/null)"
    claim="$(yq eval '.spec.template.spec.volumes[0].persistentVolumeClaim.claimName // ""' "$doc" 2>/dev/null)"
    live="$(kubectl get deployments "$name" -n "$ns" -o json 2>/dev/null)" || return 1
    if why="$(so_workload_weights_safe "$live" "$volname" "$mountpath" "$claim" "$ns")"; then
        log_info "  $ns/$name: applying the weights volume too (WVA_WORKLOAD_PATCH_APPLY_WEIGHTS=true)."
        return 0
    fi
    log_warning "  $ns/$name: the weights volume is emitted, NOT applied -- $why."
    return 1
}

so_workload_patch_one() {
    local file="$1" ns="$2" name="$3" doc rc=0
    doc="$(mktemp)" || return 1
    # shellcheck disable=SC2016
    yq eval-all "select(.metadata.namespace == \"$ns\" and .metadata.name == \"$name\")" \
        "$file" > "$doc" 2>/dev/null
    if [ ! -s "$doc" ]; then
        rm -f "$doc"
        log_warning "  Could not isolate the patch for $ns/$name."
        return 1
    fi
    # WVA_WORKLOAD_PATCH_APPLY_WEIGHTS is its own opt-in, separate from
    # APPLY. Adding a hook cannot conflict with anything; mounting storage
    # changes where an engine reads its weights from, and can be refused by the
    # API server outright. They are different decisions and get different flags.
    if [ "${WVA_WORKLOAD_PATCH_APPLY_WEIGHTS:-false}" = "true" ] \
       && so_workload_weights_can_apply "$ns" "$name" "$doc"; then
        : # keep the weights half in the document
    elif ! so_workload_patch_drop_cache "$doc"; then
        rm -f "$doc"
        # 2, not 1: a DELIBERATE skip. Counting it as a failure made every re-run
        # of the standup announce "scale-downs in this run will truncate
        # in-flight requests" about a workload that already had its drain hook.
        log_info "  $ns/$name needs only the weights volume -- emitted, not applied."
        return 2
    fi
    # `kubectl patch --type=strategic`, never `kubectl apply`. These objects were
    # created by Helm and carry no last-applied-configuration annotation, so apply
    # both warns and writes one, adopting a resource this installer does not own
    # into kubectl's bookkeeping. Server-side apply is refused outright:
    # "conflict with helm ... .spec.template.spec.terminationGracePeriodSeconds".
    kubectl patch deployments "$name" -n "$ns" --type=strategic --patch-file="$doc" || rc=$?
    rm -f "$doc"
    return "$rc"
}

# so_workload_patch_drop_cache rewrites a patch document in place, removing the
# weights half. It returns non-zero when nothing actionable is left, so the
# caller can skip a document that was only ever about the cache.
so_workload_patch_drop_cache() {
    local doc="$1" stripped keep
    stripped="$(mktemp)" || return 1
    yq eval 'del(.spec.template.spec.volumes)
             | del(.spec.template.spec.containers[].volumeMounts)
             | del(.spec.template.spec.containers[].env)' "$doc" > "$stripped" 2>/dev/null
    # What remains is worth applying only if it still carries a drain change. A
    # container entry reduced to its own name patches nothing, and sending it
    # would report a success that changed no field.
    keep="$(yq eval '[.spec.template.spec.terminationGracePeriodSeconds,
                      .spec.template.spec.containers[].lifecycle]
                     | map(select(. != null)) | length' "$stripped" 2>/dev/null)"
    if [ ! -s "$stripped" ] || [ "${keep:-0}" -eq 0 ]; then
        rm -f "$stripped"
        return 1
    fi
    mv "$stripped" "$doc" || { rm -f "$stripped"; return 1; }
    return 0
}

# so_workload_patch_header writes the preamble of an emitted patch file.
#
# The file used to open straight into `---`, which reads exactly like something
# to `kubectl apply -f` -- the one thing a reader must not do with it. It also
# carries the marker line that makes deleting a stale file safe: only a file this
# tool wrote is ever removed.
so_workload_patch_header() {
    cat <<'HDR'
# Generated by `make workload-patch`. Regenerate rather than editing in place.
# wva-workload-patch: generated
#
# These are STRATEGIC MERGE PATCHES, not manifests.
#
# Do NOT `kubectl apply -f` this file. These objects belong to a Helm release and
# carry no last-applied-configuration annotation; apply writes one, adopting a
# resource this installer does not own into kubectl's bookkeeping.
#
# The durable fix is to copy these fields into the values of the chart that owns
# the pod spec -- on llm-d, your modelservice values. Anything applied directly
# is reverted by the next `helm upgrade`, silently.
#
# To apply one document to a live object anyway:
#
#     kubectl patch deployment <name> -n <namespace> \
#         --type=strategic --patch-file=<one document from this file>
#
# A weights volume below names a PersistentVolumeClaim that MUST ALREADY EXIST,
# or every pod the rollout creates stays Pending. Its size and storage class are
# cluster decisions, which is why nothing here creates it.
#
HDR
}

# so_workload_patch_clear_stale removes a previous patch file that the workloads
# being fixed have made obsolete.
#
# Only a file THIS TOOL WROTE is removed, identified by the marker in its header.
# WVA_WORKLOAD_PATCH_FILE is an operator-supplied path, and an unconditional
# delete there removed a hand-edited file and called it "out of date".
so_workload_patch_clear_stale() {
    local out="$1"
    [ -f "$out" ] || return 0
    if ! head -5 "$out" 2>/dev/null | grep -q '^# wva-workload-patch: generated'; then
        log_info "  $out was not written by this tool, so it was left alone (it may now be out of date)."
        return 0
    fi
    rm -f "$out" && log_info "  Removed the previous $out, which is now out of date."
    return 0
}

# wva_workload_patch walks the model servers the plan discovers and writes a patch
# for each one that needs it.
#
# Emit is the default and the point. These fields belong to the model server's
# chart, and the API server says so: a server-side apply is REFUSED with
# "conflict with helm". The emitted patch goes where ownership already is.
#
# WVA_WORKLOAD_PATCH_APPLY=true additionally applies the drain half to the live
# objects, and says plainly what that costs.
#
# Returns non-zero when anything could not be read or patched. A caller's
# `|| echo WARNING` depends on that, so every path that ends without knowing the
# answer has to say so rather than fall through to the reassuring one.
wva_workload_patch() {
    local out="${WVA_WORKLOAD_PATCH_FILE:-wva-workload-patch.yaml}"
    local apply="${WVA_WORKLOAD_PATCH_APPLY:-false}"
    local ns resource pod kind name found=0 tmp rc=0
    local unreadable=0 skipped=0 scanned=0 patched=0 failed=0 deferred=0 needs_cache=0
    local listing names doc_rc index_lines tool doc_ns doc_name write_rc

    # The tool check this path never had. `make workload-patch` sources the
    # libraries directly instead of going through deploy/install.sh, so
    # check_prerequisites never runs -- and both consumers below swallow their own
    # errors, so a missing binary produced silence and exit 0 rather than a
    # complaint.
    for tool in jq $([ "$apply" = "true" ] && echo yq); do
        if ! command -v "$tool" >/dev/null 2>&1; then
            log_warning "$tool is not installed, so nothing was checked. Install it and re-run."
            return 1
        fi
    done

    tmp="$(mktemp)" || return 1
    # Cleared before the success path returns, so it only fires on the error
    # paths. It does NOT cover Ctrl-C -- a RETURN trap does not run on a signal --
    # and an INT trap here would clobber the installer's own.
    trap "rm -f '$tmp'" RETURN

    for ns in $(so_target_namespaces); do
        for kind in Deployment LWS; do
            if [ "$kind" = "Deployment" ]; then
                resource=deployments; pod="$SO_POD_PATH_DEPLOYMENT"
            else
                kubectl get crd leaderworkersets.leaderworkerset.x-k8s.io >/dev/null 2>&1 || continue
                resource=leaderworkersets; pod="$SO_POD_PATH_LWS"
            fi
            # A failed listing is NOT an empty namespace. Reported, and it makes
            # the whole run non-zero, because "nothing to patch" is a claim about
            # pods and must not be made about pods nobody could read.
            if ! listing="$(kubectl get "$resource" -n "$ns" -o json 2>/dev/null)"; then
                log_warning "  Could not list $resource in $ns -- it is not being reported as clean."
                unreadable=1
                continue
            fi
            # The same rule one stage further on. This pipeline used to end in
            # `|| true`, so a jq that failed -- or was not installed at all -- was
            # indistinguishable from a namespace with no model servers in it.
            if ! names="$(printf '%s' "$listing" \
                | jq -r --argjson p "$pod" --argjson markers "$(so_serving_markers_json)" '
                    def serving_labels: to_entries
                        | any((.key + "=" + (.value|tostring)) as $kv
                              | ($markers | index($kv)) != null);
                    .items[]?
                    | . as $o
                    | (getpath($p) // {}) as $t
                    | select((($t.metadata.labels // {}) + ($o.metadata.labels // {})) | serving_labels)
                    | $o.metadata.name')"; then
                log_warning "  Could not read the $resource listing in $ns -- it is not being reported as clean."
                unreadable=1
                continue
            fi
            while IFS= read -r name; do
                [ -n "$name" ] || continue
                scanned=$((scanned + 1))
                doc_rc=0
                so_workload_patch_doc "$ns" "$resource" "$name" "$pod" >> "$tmp" || doc_rc=$?
                case "$doc_rc" in
                    0) : ;;
                    2) skipped=$((skipped + 1)) ;;      # LWS, or no engine to name
                    *) unreadable=1 ;;                  # could not be read
                esac
            done <<< "$names"
        done
    done

    found=$(grep -c '^apiVersion:' "$tmp" 2>/dev/null) || found=0

    if [ "$found" -eq 0 ]; then
        # Four different answers, and only one of them is the good news. They
        # used to share a single log_success, which is how an all-clear came to be
        # printed one line under "it was not readable".
        if [ "$unreadable" -eq 1 ]; then
            log_warning "Nothing was written: some workloads could not be read (see above). This is NOT a report that they are healthy."
            return 1
        fi
        if [ "$scanned" -eq 0 ]; then
            log_warning "No model servers found in scope, so nothing was checked."
            return 0
        fi
        if [ "$skipped" -eq "$scanned" ]; then
            log_warning "Nothing was written: all $scanned workload(s) were skipped (see above)."
            return 0
        fi
        log_success "$((scanned - skipped)) model server(s) already drain on scale-down and keep their weights on a volume."
        [ "$skipped" -gt 0 ] && log_warning "  $skipped other workload(s) were skipped and were not checked."
        so_workload_patch_clear_stale "$out"
        return 0
    fi

    # Counted from the documents, so it survives whatever apply mode does or
    # does not do. It is stated separately from the patch count because it is a
    # different kind of problem: the drain hook is something this tool can fix,
    # and the cache is something only the cluster's owner can.
    needs_cache=$(grep -c '^#   cache:' "$tmp" 2>/dev/null) || needs_cache=0
    if [ "$needs_cache" -gt 0 ]; then
        log_warning "$needs_cache model server(s) will RE-DOWNLOAD their weights every time a replica is added."
        log_warning "  Each scale-up then waits on Hugging Face instead of on the GPU, and fails outright"
        log_warning "  where the cluster has no egress. It is the largest avoidable cost in a scale-up and"
        log_warning "  the easiest to mistake for the autoscaler being slow."
        log_warning "  $out has the pod-spec half, and says which claim to use or create."
    fi

    if [ "$apply" = "true" ]; then
        log_warning "WVA_WORKLOAD_PATCH_APPLY=true: patching $found workload(s) directly."
        log_warning "  This REPLACES PODS. Each patch is a new pod template, so every model server"
        log_warning "  patched here rolls -- and it rolls BEFORE it has the hook, so requests in"
        log_warning "  flight during that first rollout are still cut. With the default strategy at"
        log_warning "  replicas: 1 the new pod must schedule before the old one goes, so on a full"
        log_warning "  GPU cluster the rollout can stall with the old pod still serving."
        log_warning "  These fields belong to the model server's chart. The next \`helm upgrade\` reverts them,"
        log_warning "  silently, and the workload goes back to what it was. Put them in your chart values to keep them."
        if [ "${WVA_WORKLOAD_PATCH_APPLY_WEIGHTS:-false}" = "true" ]; then
            log_info "  The weights half is applied too, where it cannot break the workload -- see $out."
        else
            log_info "  The weights half is emitted only -- see $out. Add WVA_WORKLOAD_PATCH_APPLY_WEIGHTS=true to apply it."
        fi
        index_lines=0
        while read -r doc_ns doc_name; do
            [ -n "$doc_name" ] || continue
            index_lines=$((index_lines + 1))
            doc_rc=0
            so_workload_patch_one "$tmp" "$doc_ns" "$doc_name" || doc_rc=$?
            case "$doc_rc" in
                0) patched=$((patched + 1)) ;;
                2) deferred=$((deferred + 1)) ;;
                *) failed=$((failed + 1))
                   log_warning "  Patching $doc_ns/$doc_name failed; the emitted file still has it." ;;
            esac
        done < <(so_workload_patch_index "$tmp")
        # An empty index against a non-empty file is not "nothing to do" -- it
        # means the file could not be read back, and every patch was skipped
        # without one being attempted.
        if [ "$index_lines" -eq 0 ]; then
            log_warning "  Could not read back the patch documents, so NOTHING was applied."
            failed="$found"
        fi
        # Counted from patches that SUCCEEDED, not documents emitted. The old
        # message printed the emitted count unconditionally -- it reported
        # "Patched 3" with yq missing and zero patch calls made.
        [ "$patched" -gt 0 ] && log_success "Patched $patched of $found workload(s)."
        [ "$deferred" -gt 0 ] && log_info "$deferred workload(s) need only the weights volume, which is emitted rather than applied."
        if [ "$failed" -gt 0 ]; then
            log_warning "$failed of $found workload(s) were NOT patched."
            rc=1
        fi
    fi

    # `|| write_rc=$?`, NOT `if ! { ...; } > "$out"`. A redirection failure on a
    # GROUP COMMAND does not reach the `if` condition -- bash reports the error
    # and the condition still evaluates as success -- so the whole branch below
    # was unreachable and an unwritable destination produced a success message
    # with no file. The `||` form does observe it. The `-s` check covers the rest:
    # a destination that accepts the redirect and writes nothing.
    write_rc=0
    { so_workload_patch_header; cat "$tmp"; } > "$out" 2>/dev/null || write_rc=$?
    if [ "$write_rc" -ne 0 ] || [ ! -s "$out" ]; then
        # The file is what the operator needs most when the destination turns out
        # to be unwritable, and the RETURN trap was about to delete the only copy.
        trap - RETURN
        log_warning "Could not write $out. The patch is complete and kept at $tmp instead."
        return 1
    fi
    rm -f "$tmp"
    trap - RETURN
    log_success "Wrote $out ($found workload(s) need something)."
    log_info "  Apply it where the pod spec is owned -- your modelservice values, or the chart that made it."
    [ "$apply" != "true" ] && log_info "  To patch the live objects anyway: WVA_WORKLOAD_PATCH_APPLY=true make workload-patch"
    [ "$unreadable" -eq 1 ] && rc=1
    return "$rc"
}

# so_workload_gaps reports the pod-spec facts that decide whether autoscaling a
# workload is safe, as \037-separated fields:
#
#   engine \037 hasPreStop \037 grace \037 hasPVC \037 weightsOnVolume \037 argv
#
# Returns 1 and prints NOTHING when the object cannot be read. That distinction is
# the point: an empty result used to mean both "no gaps" and "not allowed to
# look", and the caller reported the reassuring one. This file already carries
# the rule, above wva_namespaces_with_model_servers -- "callers that care must ask
# which, because they are different problems" -- and this is a caller that cares.
#
# \037 rather than tab, for the reason given at the top of this file: an empty
# field in a tab stream shifts every later field left, and `engine` is empty
# exactly when the rest matters least.
#
# THE ENGINE CONTAINER IS IDENTIFIED BY IMAGE AND COMMAND, not by name. A name
# regex was tried and is wrong: an SGLang engine is routinely called `server`,
# and falling back to containers[0] lands on a routing proxy where one exists --
# putting the drain hook on the sidecar and leaving the engine to be SIGKILLed,
# which is the exact failure the hook exists to prevent. The rule below mirrors
# isSGLangContainer in internal/inferenceengine/engine.go: the image names the
# engine, or the joined command+args carries a documented launch form. Empty when
# no container matches, and the caller then skips the workload rather than
# guessing.
so_workload_gaps() {
    local ns="$1" resource="$2" name="$3" pod="$4" out
    out=$(kubectl get "$resource" "$name" -n "$ns" -o json 2>/dev/null) || return 1
    [ -n "$out" ] || return 1
    printf '%s' "$out" | jq -r --argjson p "$pod" '
        (getpath($p) // {}) as $t
        | ([ $t.spec.containers[]?
             | ((.command // []) + (.args // []) | join(" ") | ascii_downcase) as $cmd
             | select(((.image // "") | ascii_downcase | test("vllm|sglang"))
                      or ($cmd | test("vllm serve|sglang\\.launch_server|sglang serve")))
           ] | first) as $e
        # Where the engine would find weights already on disk: every path it has
        # mounted. Used below to decide whether a cache is REAL, rather than
        # whether a volume merely exists somewhere in the pod.
        | ([ $e.volumeMounts[]?.mountPath ] // []) as $mounts
        # The directory the engine actually downloads into. HF_HOME and its
        # relatives are where vLLM puts weights; --download-dir overrides them.
        | ( [ $e.env[]? | select(.name == "HF_HOME" or .name == "HF_HUB_CACHE"
                                 or .name == "TRANSFORMERS_CACHE") | .value ] | first ) as $envdir
        # gsub, for the same reason discovery does it below. NOTE: no apostrophes
        # in this comment. The jq program is inside a single-quoted shell string,
        # so one apostrophe ends the quoting and the next reopens it -- the text
        # between them is handed to the shell, and quoting REBALANCES, so bash -n
        # reports no error while jq gets a truncated program.
        #
        # The modelservice chart writes the command as a multi-line block scalar.
        # The consumer reads this record with `read`, which STOPS AT THE FIRST
        # NEWLINE, so every flag on a later line was invisible -- on real chart
        # output that hid --served-model-name and would hide --download-dir. CR
        # goes with it: a chart authored on Windows carries CRLF inside the
        # scalar, and CR is not in bash IFS, so a value would keep an invisible
        # trailing character.
        | ( ((($e.command // []) + ($e.args // []))
             | map(tostring | gsub("[\n\r]"; " ")) | join(" ")) ) as $argv
        | ( ($argv | capture("--download-dir[= ]+(?<d>[^ ]+)").d) // $envdir ) as $dldir
        | [ ($e.name // ""),
            (if ($e | not) then 0 elif ($e.lifecycle.preStop != null) then 1 else 0 end),
            ($t.spec.terminationGracePeriodSeconds // 30),
            ([ $t.spec.volumes[]? | select(.persistentVolumeClaim != null) ] | length),
            # weightsOnVolume: the download directory lies under something the
            # ENGINE has mounted. A PVC mounted anywhere else does not make
            # downloads persist, which is why "a PVC exists" is not the test --
            # a mount with no HF_HOME silences the check and fixes nothing.
            # `. as $m | $dldir | startswith($m)`, NOT `$dldir | startswith(.)`.
            # The pipe re-roots `.` to $dldir, so the second form asks whether
            # $dldir starts with ITSELF -- true for every mount. That made this
            # answer "cached" for any engine that set HF_HOME and mounted
            # anything at all; every llm-d model server mounts /dev/shm, so the
            # cache half was dead for the whole population it targets, and the
            # misconfiguration it exists to catch (PVC at /workspace, HF_HOME in
            # the image layer) read as solved.
            (if ($dldir | not) then 0
             elif ([ $mounts[] | select(. != null)
                     | select(. as $m | $dldir | startswith($m)) ] | length) > 0 then 1
             else 0 end),
            $argv
          ] | join("\u001f")' 2>/dev/null || return 1
}

# so_model_claim echoes a PersistentVolumeClaim in $1 that weights can share,
# or nothing when there is no obvious one.
#
# The point is not to save typing. A namespace that already has a shared cache
# under some other name -- `llm-d-model-cache`, `models`, whatever the chart
# called it -- would otherwise be told to use `model-pvc`, and the obvious
# response is to provision a SECOND terabyte for weights that are already on the
# cluster. Naming what is there is the difference between one claim and two.
#
# Only RWX/ROX claims qualify. A ReadWriteOnce claim binds to one node, so
# proposing it as a shared model cache would produce replicas that cannot
# schedule -- the failure this whole area exists to avoid.
#
# Silent on ambiguity, and silent on Forbidden: with two candidates, guessing
# picks someone else's data, and a 403 says nothing about what exists. The
# caller falls back to the documented default name, which is a claim the
# operator then creates deliberately.
# so_model_cache_classes lists StorageClasses this cluster is OBSERVED to serve
# ReadWriteMany from, as "<class> (<n> bound RWX claim(s))".
#
# Evidence, not capability. A StorageClass object does not declare whether its
# provisioner can do RWX -- there is no field for it -- so the only honest answer
# is which classes are already backing RWX claims somewhere on this cluster.
# That is exactly the question an operator is trying to answer ("what do I put
# here?"), and it is answered from what already works rather than from a table of
# provisioner names that would go stale.
#
# Empty output is a normal answer: a cluster with no RWX claims yet, or a token
# that cannot list claims cluster-wide. The caller says so rather than inventing
# a suggestion.
so_model_cache_classes() {
    kubectl get pvc -A -o json 2>/dev/null \
        | jq -r '[ .items[]?
                   | select(.status.phase == "Bound")
                   | select((.spec.accessModes // []) | any(. == "ReadWriteMany" or . == "ReadOnlyMany"))
                   | .spec.storageClassName // "" ]
                 | map(select(. != ""))
                 | group_by(.) | map({class: .[0], n: length})
                 | sort_by(-.n)[]
                 | "\(.class) (\(.n) bound RWX claim(s))"' 2>/dev/null || true
}

# wva_model_cache creates the weights claim, or reports why it will not.
#
# It asks for the two fields it refuses to guess and then does the rest: creates
# the claim, waits for it to bind, and -- the part that matters -- checks what
# the cluster ACTUALLY GRANTED. A provisioner may accept a ReadWriteMany request
# and bind a volume that is not shareable, and the difference does not surface
# until a second replica fails to schedule, long after this command returned.
wva_model_cache() {
    local ns="${1:-$WVA_NS}"
    local claim="${WVA_MODEL_PVC_NAME:-model-pvc}"
    local size="${WVA_MODEL_PVC_SIZE:-}"
    local class="${WVA_MODEL_PVC_CLASS:-}"
    local timeout="${WVA_MODEL_PVC_TIMEOUT:-120}"
    local existing modes phase suggestions waited

    if [ -z "$ns" ]; then
        log_warning "No namespace given. Use: make model-cache NAMESPACE=<ns> WVA_MODEL_PVC_SIZE=<size> WVA_MODEL_PVC_CLASS=<class>"
        return 1
    fi

    # Already there? Then this is a report, not a creation. Re-running must be
    # safe: an operator who is unsure whether they created it should be able to
    # type this again without risking their weights.
    if existing="$(kubectl get pvc "$claim" -n "$ns" -o json 2>/dev/null)" && [ -n "$existing" ]; then
        modes="$(printf '%s' "$existing" | jq -r '.spec.accessModes | join(",")' 2>/dev/null)"
        phase="$(printf '%s' "$existing" | jq -r '.status.phase' 2>/dev/null)"
        log_success "$ns/$claim already exists ($phase, $modes). Nothing to do."
        so_model_cache_warn_modes "$ns" "$claim" "$modes"
        return 0
    fi

    if [ -z "$size" ] || [ -z "$class" ]; then
        log_warning "Both a size and a StorageClass are required, and neither is guessed:"
        log_warning "  WVA_MODEL_PVC_SIZE   how much: every model that will share this claim, plus room."
        log_warning "                       0.6B is ~1.2GiB of weights; a 70B is ~140GB."
        log_warning "  WVA_MODEL_PVC_CLASS  which class: it MUST support ReadWriteMany, or replicas on a"
        log_warning "                       second node cannot mount it and never schedule."
        suggestions="$(so_model_cache_classes)"
        if [ -n "$suggestions" ]; then
            log_info "  Classes already serving RWX on this cluster:"
            printf '%s\n' "$suggestions" | while IFS= read -r line; do
                [ -n "$line" ] && log_info "    $line"
            done
        else
            log_info "  No RWX claims exist on this cluster yet, so there is no evidence to go on."
            log_info "  Ask whoever runs the cluster which class is a shared filesystem (NFS, CephFS,"
            log_info "  EFS, Filestore, Spectrum Scale). A default block-storage class is NOT one."
        fi
        log_info "  Then: make model-cache NAMESPACE=$ns WVA_MODEL_PVC_SIZE=<size> WVA_MODEL_PVC_CLASS=<class>"
        return 1
    fi

    # Warn BEFORE creating, when the cluster has never bound an RWX claim on this
    # class. ReadWriteMany is a REQUEST: the API server accepts it against a class
    # that cannot provide it, returns 0, and the claim then sits Pending forever.
    # On a WaitForFirstConsumer class -- kind's default, and many plain-Kubernetes
    # defaults -- Pending is also the NORMAL state, so the two are
    # indistinguishable until a pod mounts it and hangs as well.
    #
    # Evidence, not a capability list: a class already backing RWX claims here can
    # do it, whatever its provisioner is called. Silence is not proof of the
    # opposite (a fresh cluster has no claims yet, and a namespace-scoped token
    # cannot list them), so this warns and continues rather than refusing.
    if [ -n "$(so_model_cache_classes)" ] \
       && ! so_model_cache_classes | grep -q "^$class "; then
        log_warning "No ReadWriteMany claim has ever bound on class '$class' on this cluster."
        log_warning "  RWX is a request, not a guarantee: if $class cannot provide it, the claim is"
        log_warning "  accepted and then stays Pending, and every pod that mounts it stays Pending too."
        log_info "  Classes that HAVE bound RWX here:"
        so_model_cache_classes | while IFS= read -r line; do
            [ -n "$line" ] && log_info "    $line"
        done
    fi

    log_info "Creating $ns/$claim: $size, ReadWriteMany, class $class"
    if ! kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${claim}
  namespace: ${ns}
  labels:
    app.kubernetes.io/managed-by: workload-variant-autoscaler
    app.kubernetes.io/component: model-cache
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${class}
  resources:
    requests:
      storage: ${size}
EOF
    then
        log_warning "Could not create $ns/$claim (see above)."
        return 1
    fi

    # WaitForFirstConsumer is checked BEFORE waiting, not after. Such a class
    # cannot bind until a pod mounts the claim, so the wait below can only ever
    # time out -- two minutes of nothing, ending in a warning about a claim that
    # is behaving exactly as designed. (Found by running this on kind, whose
    # default class is WaitForFirstConsumer.)
    if kubectl get storageclass "$class" -o jsonpath='{.volumeBindingMode}' 2>/dev/null | grep -q WaitForFirstConsumer; then
        log_success "Created $ns/$claim."
        log_info "  Class $class binds on first use, so Pending is expected until a pod mounts it."
        log_info "  That also means this command cannot tell you whether $class can actually do"
        log_info "  ReadWriteMany -- you find out when the first pod mounts it. If that pod stays"
        log_info "  Pending, the class is the reason:  kubectl describe pvc $claim -n $ns"
        log_info "  Next: make workload-patch NAMESPACE=$ns"
        return 0
    fi

    # Bound, not created. A claim that never binds is the failure this command
    # exists to surface early.
    waited=0
    while [ "$waited" -lt "$timeout" ]; do
        phase="$(kubectl get pvc "$claim" -n "$ns" -o jsonpath='{.status.phase}' 2>/dev/null)"
        [ "$phase" = "Bound" ] && break
        sleep 5
        waited=$((waited + 5))
    done

    if [ "$phase" != "Bound" ]; then
        if kubectl get storageclass "$class" -o jsonpath='{.volumeBindingMode}' 2>/dev/null | grep -q WaitForFirstConsumer; then
            log_info "$ns/$claim is $phase: class $class binds on first use, so it will bind when a pod mounts it."
            return 0
        fi
        log_warning "$ns/$claim is still $phase after ${timeout}s. Check: kubectl describe pvc $claim -n $ns"
        return 1
    fi

    modes="$(kubectl get pvc "$claim" -n "$ns" -o jsonpath='{.spec.accessModes[*]}' 2>/dev/null)"
    log_success "$ns/$claim is Bound ($modes)."
    so_model_cache_warn_modes "$ns" "$claim" "$modes"
    log_info "  Now point the model servers at it:  make workload-patch NAMESPACE=$ns"
    return 0
}

# so_model_cache_warn_modes says so when a claim cannot be shared across nodes.
so_model_cache_warn_modes() {
    local ns="$1" claim="$2" modes="$3"
    case "$modes" in
        *ReadWriteMany*|*ReadOnlyMany*) return 0 ;;
    esac
    log_warning "  $ns/$claim is $modes, so only pods on ONE NODE can mount it."
    log_warning "  Scale-up will leave replicas Pending on other nodes. That is not a slow cache;"
    log_warning "  it is autoscaling that cannot work. Use a class that supports ReadWriteMany."
    return 0
}

so_model_claim() {
    local ns="$1" out
    out="$(kubectl get pvc -n "$ns" -o json 2>/dev/null)" || return 0
    printf '%s' "$out" | jq -r '
        [ .items[]?
          | select(.status.phase == "Bound")
          # ReadWriteMany only. ReadOnlyMany is shareable but the emitted patch
          # points HF_HOME INTO the mount, which is a write destination -- a
          # ROX volume mounts read-only, so every replica would crash on the
          # download rather than merely re-doing it.
          | select((.spec.accessModes // []) | any(. == "ReadWriteMany"))
          # The name test applies here, not only when there are several. A
          # namespace with exactly ONE RWX claim does not thereby have a model
          # cache: in a benchmark namespace that one claim is the RESULTS
          # volume, and mounting it at /model-cache with HF_HOME inside it is
          # both wrong and, with the weights opt-in, applied to a live workload.
          # Reusing the wrong claim is worse than proposing a new one.
          | select(.metadata.name | test("model|weight|cache"; "i"))
          | .metadata.name ] as $named
        # Exactly one candidate, or nothing. With two, guessing means picking
        # data that belongs to something else. NOTE: no apostrophes in this
        # comment -- see the note in so_workload_gaps.
        | if ($named | length) == 1 then $named[0] else "" end' 2>/dev/null || true
}

so_workload_patch_doc() {
    local ns="$1" resource="$2" name="$3" pod="$4"
    local gaps engine prestop grace pvcs onvol argv model
    local want_drain=0 want_cache=0 grace_want mountpath claim claim_found=0

    # LWS is skipped, deliberately and loudly. Three things would each have to be
    # solved and none is: the pod template is spec.leaderWorkerTemplate, not
    # spec.template, so a Deployment-shaped patch writes a field the type does not
    # have; the API server does not accept a strategic merge for a custom
    # resource, so applying it is an unconditional 415; and draining a multi-node
    # group means the WORKER template too, since killing a worker aborts the
    # leader's in-flight generations whatever hook the leader carries. Emitting a
    # confident, wrong document is worse than emitting nothing.
    if [ "$resource" = "leaderworkersets" ]; then
        log_warning "  Skipping LeaderWorkerSet $ns/$name: draining a multi-node group needs the worker template too, and is not implemented."
        return 2
    fi

    # 3, not 0. Returning 0 here meant the caller counted an object it could not
    # read as one with nothing wrong, printed the all-clear two lines below this
    # very warning, and exited 0.
    if ! gaps="$(so_workload_gaps "$ns" "$resource" "$name" "$pod")"; then
        log_warning "  Could not read $resource/$name in $ns -- skipped. It is not being reported as healthy; it was not readable."
        return 3
    fi
    IFS=$'\037' read -r engine prestop grace pvcs onvol argv <<< "$gaps"

    # No identifiable engine means no idea which container to patch, and the
    # fallback that guessed container 0 is what put hooks on routing proxies.
    if [ -z "$engine" ]; then
        log_warning "  No vLLM or SGLang container found in $resource/$name ($ns) -- skipped rather than guessing which one serves."
        return 2
    fi

    [ "${prestop:-0}" -eq 0 ] && want_drain=1

    # The cache half asks whether WEIGHTS LAND ON A VOLUME, not whether a volume
    # exists. so_model_source reads where the weights COME FROM -- which is not
    # the same as the name the workload advertises, and only the source answers
    # this question. A local path needs no cache; an unreadable model is left
    # alone, because a confidently wrong answer here mounts a PVC into a workload
    # that had no use for one.
    model="$(so_model_source "$argv")" || model=""
    case "$model" in
        ""|/*) : ;;
        *) if [ "${onvol:-0}" -eq 0 ]; then
               want_cache=1
               # NOT just a line in the emitted file. This is the single largest
               # avoidable cost in a scale-up, it is invisible until the moment a
               # replica is added, and it looks like the autoscaler being slow.
               log_warning "  $ns/$name RE-DOWNLOADS ITS WEIGHTS on every scale-up: $engine fetches $model from Hugging Face, outside every volume it mounts."
           fi ;;
    esac

    [ "$want_drain" -eq 1 ] || [ "$want_cache" -eq 1 ] || return 0

    cat <<HDR
---
# ${resource}/${name} in ${ns}   (engine container: ${engine})
HDR
    if [ "$want_drain" -eq 1 ]; then
        # Never LOWER an existing grace period. A long grace with no hook is
        # exactly the configuration not to shorten, and the emitted document used
        # to print the old value in a comment while quietly reducing it.
        grace_want="${WVA_DRAIN_GRACE_SECONDS:-120}"
        [ "${grace:-30}" -gt "$grace_want" ] && grace_want="$grace"
        cat <<DRAINWHY
#   drain: ${engine} declares no preStop hook; terminationGracePeriodSeconds is ${grace:-30}.
#          A replica removed mid-stream cuts the responses it is still writing.
DRAINWHY
    fi
    if [ "$want_cache" -eq 1 ]; then
        cat <<CACHEWHY
#   cache: ${model} is a repository id and the engine downloads outside any
#          mounted volume, so every scale-up fetches the weights again.
CACHEWHY
    fi
    cat <<HEAD
#
# Apply where the pod spec is OWNED -- your modelservice values, or the chart that
# produced this object. Applying it directly works until the next
# \`helm upgrade\` reverts it.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${name}
  namespace: ${ns}
spec:
  template:
    spec:
HEAD
    if [ "$want_drain" -eq 1 ]; then
        cat <<GRACE
      # The kubelet sends SIGKILL when this expires, hook or not, so it is the
      # total budget rather than the drain window. The drain window is the
      # preStop sleep below; this only has to be comfortably larger.
      terminationGracePeriodSeconds: ${grace_want}
GRACE
    fi
    cat <<CONTAINERS
      containers:
      - name: ${engine}
CONTAINERS
    if [ "$want_drain" -eq 1 ]; then
        cat <<PRESTOP
        lifecycle:
          preStop:
            exec:
              # Two things share this window, which is why it is not merely "long
              # enough to finish a request": the endpoint's removal from the
              # Service propagates asynchronously through kube-proxy and the EPP,
              # so part of it is covering arrivals that are still being routed
              # here, and the rest is letting what is already in flight finish.
              command: ["/bin/sh", "-c", "sleep ${WVA_DRAIN_SLEEP_SECONDS:-45}"]
PRESTOP
    fi
    if [ "$want_cache" -eq 1 ]; then
        mountpath="${WVA_MODEL_CACHE_PATH:-/model-cache}"
        # An existing shared claim beats the default name. WVA_MODEL_PVC_NAME
        # still wins over both when it is set explicitly: an operator naming a
        # claim is making a decision, not asking for a suggestion.
        if [ -n "${WVA_MODEL_PVC_NAME:-}" ]; then
            claim="$WVA_MODEL_PVC_NAME"
            # Asked, not assumed. An operator naming a claim may be naming one
            # they have or one they intend to create, and the emitted file used
            # to tell them their existing claim "does not exist yet".
            kubectl get pvc "$claim" -n "$ns" >/dev/null 2>&1 && claim_found=1
        else
            claim="$(so_model_claim "$ns")"
            if [ -n "$claim" ]; then claim_found=1; else claim="model-pvc"; fi
        fi
        cat <<CACHE
        # The mount alone changes nothing: vLLM downloads to HF_HOME, so without
        # this the weights land inside the container and the volume sits unused
        # while every replica fetches them again.
        env:
        - name: HF_HOME
          value: ${mountpath}/huggingface
        volumeMounts:
        - name: ${WVA_MODEL_VOLUME_NAME:-model-storage}
          mountPath: ${mountpath}
      volumes:
      - name: ${WVA_MODEL_VOLUME_NAME:-model-storage}
        persistentVolumeClaim:
          claimName: ${claim}
CACHE
        # Two different situations, and the file used to print the second one in
        # both: with a claim discovered, it still said "create model-pvc" -- the
        # second claim this feature exists to prevent, and not even the one the
        # patch above mounts.
        if [ "$claim_found" -eq 1 ]; then
            cat <<CACHEHAVE
#
#   ${claim} already exists in ${ns} and is shareable, so it is reused rather
#   than duplicated. Nothing to create.
CACHEHAVE
        else
            cat <<CACHENEED
#
#   ${claim} does NOT exist yet. Create it before applying this, or every pod
#   the rollout makes stays Pending. Easiest:
#
#     make model-cache NAMESPACE=${ns}    # lists classes serving RWX here
#     make model-cache NAMESPACE=${ns} WVA_MODEL_PVC_SIZE=<size> WVA_MODEL_PVC_CLASS=<class>
#
#   The two fields are asked for because neither can be guessed, and both decide
#   whether the cache helps or breaks scale-up:
#
#     accessModes: ReadWriteMany is the point -- many replicas reading one copy.
#       A default StorageClass is often RWO block storage, and a ReadWriteOnce
#       claim binds to ONE node: the second replica then cannot schedule at all,
#       which turns a slow scale-up into a failed one.
#     storage: a function of how many models share it and how large they are.
#       0.6B is ~1.2GiB of weights; a 70B is ~140GB. Real llm-d installs run
#       these in the hundreds of GiB to 1Ti.
#
#   Or by hand:
#
#     apiVersion: v1
#     kind: PersistentVolumeClaim
#     metadata:
#       name: ${claim}
#       namespace: ${ns}
#     spec:
#       accessModes: [ReadWriteMany]
#       storageClassName: <an RWX-capable class on this cluster>
#       resources:
#         requests:
#           storage: <size>
CACHENEED
        fi
        cat <<CACHEEND
CACHEEND
    fi
}

so_drain_note() {
    local ns="$1" resource="$2" name="$3" pod="$4"
    local gaps engine prestop grace pvcs onvol argv

    # preStop is checked on the ENGINE, not pod-wide. The previous copy counted
    # containers with a hook across the whole pod, so an llm-d deployment whose
    # routing proxy has one reported clean while the engine -- the container
    # actually holding the streams -- had none.
    if ! gaps="$(so_workload_gaps "$ns" "$resource" "$name" "$pod")"; then
        printf '%s' "Could not read the pod spec, so whether scale-down drains cleanly is unknown -- this is not a report that it does."
        return 0
    fi
    IFS=$'\037' read -r engine prestop grace pvcs onvol argv <<< "$gaps"
    [ -n "$engine" ] || return 0
    # A hook of any shape counts. Whether it drains for long enough is the
    # operator's judgement, and guessing at a threshold would produce a warning
    # nobody can act on.
    [ "${prestop:-0}" -gt 0 ] && return 0
    printf '%s' "Scale-down will cut in-flight requests: ${engine} declares no preStop hook, and terminationGracePeriodSeconds is ${grace:-30}. A replica removed mid-stream terminates the responses it is still writing. The pod spec belongs to the model server's chart rather than to WVA -- see docs/deployment/operations.md#draining-before-scale-down."
    return 0
}

so_target_namespaces() {
    local scope="${WVA_DEFAULT_SO_NS:-}"
    if [ -z "$scope" ]; then
        if [ "$(wva_install_scope)" = "cluster" ]; then scope=all; else scope=wva; fi
    fi
    case "$scope" in
        # The namespace this controller MANAGES, which is its own unless it was
        # installed to watch another. Scanning WVA_NS unconditionally meant that an
        # admin-owned controller — the whole point of which is to sit outside the
        # namespace it manages — planned against its own empty namespace and
        # reported that there was nothing to autoscale.
        wva) echo "${WVA_WATCH_NS:-$WVA_NS}"; return ;;
        all) : ;;
        *)   echo "$scope"; return ;;
    esac
    # Cluster-wide. Only meaningful for a cluster-scoped WVA: a namespace-scoped
    # install restricts its cache to its own namespace, so it could not read a
    # workload anywhere else and a
    # ScaledObject there would call a scaler that refuses it.
    if [ "$(wva_install_scope)" != "cluster" ]; then
        log_warning "WVA_DEFAULT_SO_NS=all requested, but this is a namespace-scoped install — it restricts its cache to $WVA_NS and cannot read a workload anywhere else. Scanning $WVA_NS only."
        echo "$WVA_NS"
        return
    fi
    wva_namespaces_with_model_servers
}

# wva_namespaces_with_model_servers echoes every namespace holding an llm-d model
# server, one per line. Empty output means either none or "not allowed to look" —
# callers that care must ask which, because they are different problems.
wva_namespaces_with_model_servers() {
    { so_namespaces_of deployments "$SO_POD_PATH_DEPLOYMENT"
      so_namespaces_of leaderworkersets "$SO_POD_PATH_LWS"
    } | sort -u
}

# so_namespaces_of echoes the namespace of every workload of one kind that carries
# the model-server marker, on its pod template or on the object itself.
so_namespaces_of() {
    local resource="$1" pod="$2"
    # `|| true` is load-bearing. A cluster-wide list is exactly what a NAMESPACE
    # ADMIN may not do, and a denied kubectl exits non-zero: under the callers'
    # `set -e` with pipefail, that took the whole preflight down without printing
    # a single line — a tenant running `make check-prereqs SCOPE=namespace` got
    # silence. Not finding namespaces is a legitimate answer here; the caller
    # decides what it means.
    kubectl get "$resource" -A -o json 2>/dev/null \
        | jq -r --argjson p "$pod" --argjson markers "$(so_serving_markers_json)" '
            .items[]
            | ((getpath($p + ["metadata","labels"]) // {}) + (.metadata.labels // {}))
              as $labels
            | select($labels | to_entries
                     | any((.key + "=" + (.value|tostring)) as $kv
                           | ($markers | index($kv)) != null))
            | .metadata.namespace' 2>/dev/null || true
}

# so_existing_info echoes `name min max cost policy is_wva` for the ScaledObject
# already targeting a workload, if any. Adoption has to patch THAT object:
# creating our own alongside it would put two ScaledObjects on one target,
# which is two HPAs writing the same replica count — the exact failure the
# skip-by-default (below, for a non-WVA existing object) exists to avoid.
#
# is_wva is "true"/"false": whether a trigger already names a
# wva-external-scaler. Different risk levels — an object already assigned to
# WVA has nothing to protect by defaulting to no, while one some other scaler
# owns (hand-tuned, GitOps-managed) does.
#
# The bounds come with the name because an `adopt` row must show the bounds the
# workload actually has, not the ones a fresh install would have picked. A plan
# that showed 1-10 for a workload someone had pinned to 4-4 would quietly widen it
# on apply, and the file gave no hint that it was about to.
#
# jq, not a go-template: `{{if .spec.minReplicaCount}}` is false for 0, so
# scale-to-zero workloads would read as "unset" and come back as 1. The `//`
# defaults below are KEDA's own for an absent field.
so_existing_info() {
    local ns="$1" target="$2"
    # US-separated (\037), like so_plan_rows -- NOT a plain space. scalingPolicy
    # is legitimately empty on most objects, and `IFS=' ' read` collapses a run
    # of spaces into one delimiter, silently vanishing an empty field and
    # shifting every later field one place left. See so_plan_rows's own comment
    # for the incident this class of bug caused.
    #
    # Last field: the same scalerAddress prefix test used everywhere else a
    # trigger is checked for already being WVAs (see so_pause_rows,
    # so_repoint_stale). "Already assigned to WVA" and "some other scaler
    # entirely" are different risk levels for defaulting apply: below, and
    # only a scalerAddress already naming a wva-external-scaler proves the
    # former.
    kubectl get scaledobject -n "$ns" -o json 2>/dev/null \
        | jq -r --arg t "$target" '
            .items[]
            | select(.spec.scaleTargetRef.name == $t)
            | [ .metadata.name,
                (.spec.minReplicaCount // 0 | tostring),
                (.spec.maxReplicaCount // 100 | tostring),
                ([ .spec.triggers[]? | select(.type | startswith("external"))
                   | .metadata.variantCost // empty ] | first // "10.0"),
                ([ .spec.triggers[]? | select(.type | startswith("external"))
                   | .metadata.scalingPolicy // empty ] | first // ""),
                (([ .spec.triggers[]?.metadata.scalerAddress // ""
                    | select(startswith("wva-external-scaler.")) ] | length > 0)
                 | tostring) ]
            | join("")' 2>/dev/null \
        | head -1
}

# so_existing_name echoes just the name, for callers that only ask whether one is
# there. Re-read at apply time: a plan can be applied long after it was written.
so_existing_name() {
    so_existing_info "$1" "$2" | awk -F'\037' '{print $1}'
}

# so_discover writes the plan to stdout: the documented preamble, then one entry
# per candidate workload. An entry is marked "no" when it should not be applied,
# with the reason in its note, rather than dropped — the file you were shown is
# then the whole truth about what was found, and turning a "no" into a "yes" is a
# deliberate act rather than an undiscoverable one.
so_discover() {
    local ns name args labels objlabels model pool kind apply note drain weights
    local existing existing_name existing_min existing_max existing_cost existing_policy existing_is_wva
    local min max cost policy
    so_plan_preamble
    echo "plan:"
    # Collected by so_plan_entry, consumed by so_warmpool_block at the end.
    SO_WARMPOOL_ROWS=$(mktemp)
    for ns in $(so_target_namespaces); do
        for kind in Deployment LeaderWorkerSet; do
            local resource='deployments' pod="$SO_POD_PATH_DEPLOYMENT"
            if [ "$kind" = "LeaderWorkerSet" ]; then
                resource='leaderworkersets'
                pod="$SO_POD_PATH_LWS"
            fi
            # args LAST: a serving container's args can legitimately contain a
            # pipe (a shell string, a chat template), and `read` gives the final
            # variable the whole remainder — so only a trailing field is safe.
            while IFS='|' read -r name labels objlabels args; do
                [ -n "$name" ] || continue
                # The marker, on the pod template or on the object. Kept separate
                # from $labels, which is POD labels only: so_pool matches those
                # against InferencePool selectors, and folding the object's in
                # could claim a pool that does not actually select these pods.
                so_is_serving "$labels" "$objlabels" || continue
                apply=yes; note=""
                min="${WVA_DEFAULT_SO_MIN:-1}"; max="${WVA_DEFAULT_SO_MAX:-10}"
                # Reset every per-workload variable here, not only the ones this
                # workload sets: existing_name is assigned inside a conditional, so
                # a stale one would offer `adopt` on the next workload and name an
                # object that scales something else entirely.
                cost="$SO_DEFAULT_VARIANT_COST"; policy=""; existing_name=""
                model=$(so_model_id "$args")
                if [ -z "$model" ]; then
                    apply=no
                    note="no --served-model-name, --model, or 'vllm serve <model>' on the container, so the model could not be read. Fill in modelID and set apply: yes to include it."
                fi
                # FMA: report it, do NOT retarget.
                #
                # An earlier version pointed the plan at the requester, on the
                # scenario's word that "launcher pods host vLLM and the requester
                # is the serving path". Measured on pokprod001, retargeting is
                # still wrong -- the requester holds no engine and WVA said so
                # once per cycle, "has 1 ready pod(s) but none attributed" -- but
                # the reason recorded here used to be wrong too. It claimed the
                # launchers "answer 0 vLLM series on :8000". They answer 364. A
                # launcher scraped mid-benchmark reported 143 running, 61 waiting
                # and KV 99.9% full; the earlier check had caught a sleeping one.
                #
                # What is true is that the launchers are unusable to WVA for a
                # different reason: they are owned by a LauncherConfig, so the
                # attribution walk reaches no scale target and drops them. See the
                # note above SO_FMA_ROLE_MARKER for the full shape.
                #
                # So the decode Deployment stays the target -- it is the workload
                # WVA can both scale and measure -- and the operator gets told, at
                # warning level, that a second serving tier exists which WVA is
                # not measuring. Silence here reads as "FMA is handled".
                if [ "$kind" = "Deployment" ]; then
                    local model_label fma_requester fma_launchers
                    model_label=$(printf '%s' "$labels" | tr ' ' '\n' \
                        | sed -n 's/^llm-d\.ai\/model=//p' | head -1)
                    fma_requester=$(so_fma_requester "$ns" "$model_label")
                    if [ -n "$fma_requester" ] && [ "$fma_requester" != "$name" ]; then
                        fma_launchers=$(kubectl get pods -n "$ns" \
                            -l app.kubernetes.io/component=launcher \
                            --no-headers 2>/dev/null | wc -l | tr -d ' ')
                        log_info "FMA detected in $ns for model '$model_label' (requester: $fma_requester). Targeting $name by default; the requester is written below as a second variant with apply: no — set it to yes to autoscale both halves of this model."
                        log_warning "FMA launcher pods (${fma_launchers:-?} in $ns) run vLLM and serve traffic. WVA follows the dual-pods pairing to attribute a BOUND launcher's metrics, but only if something scrapes them — launchers declare no container ports, so a port-name PodMonitor generates no target. Check with: kubectl get podmonitor -n $ns. See docs/deployment/operations.md, 'FMA launcher pods'."
                        log_warning "GPU accounting in $ns is a LOWER BOUND: a launcher keeps its vLLM instance resident after unbinding, holding a real GPU while requesting none, and the annotations naming that GPU are stripped at unbind — so neither ResourceQuota nor the WVA limiter can see it. Subtract the warm pool by hand when planning capacity. See docs/deployment/gpu-limiter.md, 'FMA namespaces'."
                        note="${note:+$note}FMA topology: requester $fma_requester and ${fma_launchers:-?} launcher pod(s) also serve this model. This entry is the modelservice half and is the default target. The FMA half is written below as a second variant with apply: no — turn it on to have WVA size both."
                    fi
                fi
                existing=$(so_existing_info "$ns" "$name")
                if [ -n "$existing" ]; then
                    # Its own bounds are carried into the entry either way, so
                    # adopting or re-applying unedited changes only who decides
                    # the count, never the count itself.
                    IFS=$'\037' read -r existing_name existing_min existing_max existing_cost existing_policy existing_is_wva <<< "$existing"
                    min="$existing_min"; max="$existing_max"; cost="$existing_cost"
                    policy="$existing_policy"
                    if [ "$existing_is_wva" = "true" ]; then
                        # Already assigned to WVA -- nothing to protect by
                        # defaulting to no here. Re-applying just refreshes the
                        # object in place (this is exactly the modelID-drift
                        # repair path: so_verify_scaledobjects finds the same
                        # entry, and the fix is to re-run this plan unedited).
                        apply=adopt
                        note="${note:+$note}Already targets WVA (ScaledObject $existing_name, min $existing_min, max $existing_max) — apply: adopt refreshes it in place. Set apply: no to leave it untouched."
                    else
                        # Leave it alone by default: it may be hand-tuned or
                        # GitOps-managed by something that is not WVA.
                        # `apply: adopt` is how you say you want it pointed at
                        # WVA instead — the case when you are adding WVA to a
                        # cluster whose workloads something else already scales.
                        apply=no
                        # Appended, not assigned: a workload can both be scaled by
                        # something else and have no readable model, and adopting it
                        # would then point a ScaledObject at an empty modelID.
                        note="${note:+$note}Already scaled by ScaledObject $existing_name (min $existing_min, max $existing_max). Set apply: adopt to point that one at WVA instead of adding a second."
                    fi
                fi
                pool=$(so_pool "$ns" "$labels")
                # Appended last: it is true of the workload regardless of what
                # else the entry says, including an adopt.
                drain=$(so_drain_note "$ns" "$resource" "$name" "$pod")
                [ -n "$drain" ] && note="${note:+$note}$drain"
                weights=$(so_weights_note "$ns" "$resource" "$name" "$pod")
                [ -n "$weights" ] && note="${note:+$note}$weights"
                so_plan_entry "$apply" "$ns" "$kind" "$name" "$model" \
                    "$min" "$max" "$cost" "$policy" "$pool" "$existing_name" "$note"
            done < <(kubectl get "$resource" -n "$ns" -o json 2>/dev/null \
                | jq -r --argjson p "$pod" '
                    .items[]
                    | . as $o
                    | (getpath($p) // {}) as $t
                    | [ $o.metadata.name,
                        (($t.metadata.labels // {}) | to_entries
                         | map(.key + "=" + (.value|tostring)) | join(" ")),
                        (($o.metadata.labels // {}) | to_entries
                         | map(.key + "=" + (.value|tostring)) | join(" ")),
                        # Newlines collapsed to spaces: read below takes one
                        # record per line, and the llm-d modelservice chart
                        # wraps vllm serve as a single multi-line args string (a
                        # bash -c script), which would otherwise split one
                        # record across many lines and hide its
                        # --served-model-name past the first line. \r goes with
                        # it: a chart authored on Windows carries CRLF inside
                        # the block scalar, and CR is not in bash IFS, so the
                        # modelID would keep an invisible trailing character and
                        # match no metric series.
                        # command AND args: the model of `vllm serve <model>` is
                        # positional, and the llm-d guides split the two across
                        # the fields -- command: ["vllm","serve"] with the model
                        # first in args. Reading args alone leaves no `serve`
                        # token to anchor on, which is what forced an earlier
                        # version to fall back to "first non-flag token" and so
                        # to answer `python` for an SGLang server. With command
                        # folded in the anchor is always present, and anything
                        # that is not a `serve` command correctly yields nothing.
                        #
                        # The ENGINE container, not containers[0]. Same rule as
                        # so_workload_gaps, and the same reason: an llm-d
                        # deployment with a routing proxy lists the proxy first,
                        # so reading container 0 found no model on exactly the
                        # layout the chart produces -- the plan then marked the
                        # workload `apply: no` and asked the operator to type in
                        # a modelID that was written on the next container down.
                        # Falls back to containers[0] when nothing identifies as
                        # an engine, which preserves the answer for a
                        # single-container workload the image name does not
                        # match.
                        ((([ $t.spec.containers[]?
                             | ((.command // []) + (.args // []) | map(tostring)
                                | join(" ") | ascii_downcase) as $cmd
                             | select(((.image // "") | ascii_downcase
                                       | test("vllm|sglang"))
                                      or ($cmd | test("vllm serve|sglang\\.launch_server|sglang serve")))
                           ] | first) // $t.spec.containers[0]) as $c
                         | (($c.command // []) + ($c.args // []))
                         | map(tostring | gsub("[\n\r]"; " ")) | join(" "))
                      ] | join("|")')
        done
        # FMA requesters last, and only where nothing above already covers the
        # model. They carry no serving marker, so the loop above cannot see them.
        so_discover_fma_requesters "$ns"
    done
    so_warmpool_block
}

# so_warmpool_block appends the plan's `warmPools:` section: which of the
# workloads just listed could SHARE a warm pool, and one suggestion per group.
#
# Grouped from the WORKLOADS, not from ScaledObjects, because at plan time the
# ScaledObjects do not exist yet -- this file is what creates them. The
# standalone `deploy/warmpool.sh plan` answers the same question for a namespace
# already running, where the triggers are the better evidence.
#
# Never fatal. A namespace whose shapes cannot be read still gets a usable
# ScaledObject plan, and losing that to a missing pool suggestion would be a bad
# trade.
so_warmpool_block() {
    local rows="${SO_WARMPOOL_ROWS:-}"
    SO_WARMPOOL_ROWS=""
    [ -n "$rows" ] || return 0
    if [ -s "$rows" ]; then
        if ! python3 "$(dirname "${BASH_SOURCE[0]}")/warmpool_plan.py" \
                --from-workloads < "$rows"; then
            echo ""
            echo "# Warm pools: could not be worked out, so none are suggested here."
            echo "# Run: deploy/warmpool.sh plan -n <namespace>"
        fi
    fi
    rm -f "$rows"
}

# so_discover_fma_requesters writes plan entries for FMA requester Deployments in
# $1 that no modelservice workload already covers.
#
# It handles the one namespace shape the main loop cannot see at all. Discovery
# finds workloads by SO_SERVING_MARKER on the pod template, and a requester does
# not carry it — it has the hyphenated llm-d.ai/inference-serving and role=requester
# instead. So a namespace running FMA and nothing else produced an empty plan and
# the message "no llm-d model servers found": an install that autoscales nothing
# and says only that it found nothing to autoscale.
#
# Three shapes, three answers:
#
#   modelservice only   the main loop handles it, unchanged
#   requester only      this function, since nothing else can be scaled
#   both                the main loop wins and warns; this function skips, so one
#                       model never yields two entries fighting over it
#
# Entries are written with apply: no when the FMA path cannot work yet — no model
# resolvable, or nothing scraping the launchers — rather than omitted. An absent
# entry makes a fixable monitoring problem look like discovery finding nothing,
# which is the failure this whole function exists to remove.
so_discover_fma_requesters() {
    local ns="$1" name model_label model apply note pool
    local existing existing_name existing_min existing_max existing_cost existing_policy existing_is_wva
    local min max cost policy sibling scrapers launchers

    while IFS='|' read -r name model_label; do
        [ -n "$name" ] || continue

        # `both`: a modelservice workload already serves this model and was
        # emitted above. The FMA half is still written out, defaulted OFF.
        #
        # It used to be dropped silently, which meant the plan did not show a
        # workload that was serving traffic -- the one thing a plan is for. It is
        # now an entry you can turn on, using the same `apply:` switch as
        # everything else rather than a second switch that could disagree with it:
        #
        #   leave it              scale the modelservice half only (unchanged)
        #   set apply: yes        scale BOTH as two variants of one model
        #   set it yes, other no  scale the FMA half only
        #
        # Both entries carry the same modelID on purpose. That is what makes WVA
        # treat them as two variants of one model and allocate replicas between
        # them by variantCost, rather than sizing two unrelated models.
        sibling=$(so_serving_workload_for_model "$ns" "$model_label")

        apply=yes; note=""
        min="${WVA_DEFAULT_SO_MIN:-1}"; max="${WVA_DEFAULT_SO_MAX:-10}"
        cost="$SO_DEFAULT_VARIANT_COST"; policy=""; existing_name=""

        model=$(so_fma_isc_model "$ns" "$name")
        if [ -z "$model" ]; then
            apply=no
            note="FMA requester, but its model could not be read: no InferenceServerConfig annotation on the pod template, or the ISC it names is gone or has no --model in modelServerConfig.options. Fill in modelID and set apply: yes to include it."
        fi

        # The launchers must be scraped or this entry scales blind: the requester
        # runs no engine, so every metric for this variant comes from a launcher
        # pod. A ScaledObject that cannot measure its workload is worse than none
        # -- it holds the workload at minReplicaCount and reports healthy.
        launchers=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
            --no-headers 2>/dev/null | wc -l | tr -d ' ')

        # Cap maxReplicas at the number of launcher pods present, when that is
        # lower than the default. A requester replica becomes capacity only if a
        # launcher can bind to it; past that the pod stays Pending, and a pending
        # replica still counts toward anticipated supply, so it suppresses the
        # scale-up it was supposed to provide. Writing the generic default here
        # would have contradicted this entry's own note.
        #
        # It is a ceiling on launchers only, not on GPUs -- the true bound is
        # min(warm slots, free GPUs) and the second half is not knowable from
        # here. So this is the safe direction, not the whole answer, and the note
        # says as much.
        if [ "${launchers:-0}" -gt 0 ] && [ "${launchers:-0}" -lt "$max" ]; then
            max="$launchers"
        fi

        scrapers=$(wva_launcher_scrapers "$ns" | paste -sd, -)
        if [ -z "$scrapers" ]; then
            apply=no
            note="${note:+$note}No PodMonitor generates a scrape target for the ${launchers:-0} launcher pod(s) in $ns, so this variant would have no metrics at all -- launchers declare no container ports, and a port-name endpoint resolves nothing. Fix with: kubectl apply -k config/fma-launcher-metrics -n $ns (or WVA_FMA_LAUNCHER_METRICS=true at install), then set apply: yes."
        else
            note="${note:+$note}FMA variant: the requester is the scale target, and its engine metrics come from launcher pods scraped by $scrapers. WVA follows the dual-pods pairing to attribute them."
        fi

        # Defaulted off when the modelservice half already covers this model, so
        # an existing install's plan applies exactly what it applied before and
        # turning the second variant on stays a deliberate act.
        if [ -n "$sibling" ]; then
            apply=no
            note="${note:+$note}SECOND VARIANT: $sibling already serves this model and is the entry above. Set apply: yes to autoscale this FMA half alongside it — same modelID, so WVA treats the two as variants of one model and allocates replicas between them by variantCost. Left as no, only $sibling is scaled and the traffic EPP sends to launchers is unmeasured. maxReplicas is capped at the ${launchers:-0} launcher pod(s) present, which bounds warm slots but NOT free GPUs — check both before raising it, since past either ceiling requester pods stay Pending and suppress further scale-up."
        fi

        existing=$(so_existing_info "$ns" "$name")
        if [ -n "$existing" ]; then
            IFS=$'\037' read -r existing_name existing_min existing_max existing_cost existing_policy existing_is_wva <<< "$existing"
            min="$existing_min"; max="$existing_max"; cost="$existing_cost"
            policy="$existing_policy"
            if [ "$existing_is_wva" = "true" ]; then
                apply=adopt
                note="${note:+$note}Already targets WVA (ScaledObject $existing_name, min $existing_min, max $existing_max) — apply: adopt refreshes it in place. Set apply: no to leave it untouched."
            else
                apply=no
                note="${note:+$note}Already scaled by ScaledObject $existing_name (min $existing_min, max $existing_max). Set apply: adopt to point that one at WVA instead of adding a second."
            fi
        fi

        # Empty for a requester, and correctly so: an InferencePool selects the
        # LAUNCHER, which carries the serving labels at bind time. The column is
        # for orientation, so an honest blank beats a guess.
        pool=$(so_pool "$ns" "")

        so_plan_entry "$apply" "$ns" "Deployment" "$name" "$model" \
            "$min" "$max" "$cost" "$policy" "$pool" "$existing_name" "$note"
    done < <(kubectl get deployments -n "$ns" -o json 2>/dev/null \
        | jq -r --argjson p "$SO_POD_PATH_DEPLOYMENT" --arg marker "$SO_FMA_ROLE_MARKER" '
            ($marker | split("=")) as $mk
            | .items[]
            | . as $o
            | (getpath($p) // {}) as $t
            | ($t.metadata.labels // {}) as $l
            | select(($l[$mk[0]] // "" | tostring) == $mk[1])
            | [ $o.metadata.name, ($l["llm-d.ai/model"] // "") ] | join("|")')
}

# so_show_plan prints the plan as a table. The file stays the thing you edit; this
# is the thing you read before deciding to.
#
# Empty fields become "-": `column -t` folds consecutive delimiters into one, so a
# workload with no readable model would print with every later column shifted one
# place left, and the table would say its min was its model.
so_show_plan() {
    local file="$1" rows count
    # `|| exit 1`, not `|| true`: so_plan_rows dies on a plan it cannot read, and
    # that death happens inside this command substitution's subshell. Without this
    # the caller would print an empty table and carry on to apply nothing, having
    # already shown the user the reason and then contradicted it.
    rows=$(so_plan_rows "$file") || exit 1
    count=$(printf '%s' "$rows" | grep -c . || true)
    echo ""
    echo "  Discovered llm-d model servers ($count):"
    echo ""
    { echo "$SO_PLAN_HEADER"
      # `$1 = $1` forces the record to be rebuilt with OFS. Without it awk only
      # rebuilds when a field is assigned, so a row that happened to have no empty
      # field printed with its original separators still in it — and the table
      # showed that row as one unsplit run of text while its neighbours lined up.
      printf '%s\n' "$rows" \
        | awk -F'\037' -v OFS='\t' '{for (i = 1; i <= NF; i++) if ($i == "") $i = "-"; $1 = $1; print}'
    } | column -t -s $'\t' | sed 's/^/    /'
    echo ""
    grep -E '^[[:space:]]*# note:' "$file" 2>/dev/null | sed -E 's/^[[:space:]]*# note:/    note:/' || true
    echo ""
}

# so_apply_plan acts on every entry: creating for yes, repointing for adopt.
so_apply_plan() {
    local file="$1" scaler_addr="wva-external-scaler.${WVA_NS}.svc.cluster.local:9090"
    local apply ns kind name model min max cost policy existing rows
    local created=0 adopted=0 skipped=0 unresolved=0

    # Read the whole plan before touching anything, and stop if it could not be
    # read. so_plan_rows exits on an unparseable file, but that exit happens in
    # this subshell — without `|| exit 1` the loop saw no rows, applied nothing,
    # and reported a clean "0 created" for a file whose error it had just printed.
    rows=$(so_plan_rows "$file") || exit 1

    # An empty plan applies nothing, and says so in those words. It reaches here
    # whenever the namespace has no model servers yet, which is not an error and
    # must not read like one.
    if [ -z "$rows" ]; then
        log_info "The plan has no entries, so nothing was applied. Deploy the model servers, re-run 'make scaledobjects-plan', and apply that."
        return 0
    fi

    while IFS=$'\037' read -r apply ns kind name model min max cost policy; do
        [ -n "$name" ] || continue
        case "$apply" in
            yes|adopt) : ;;
            no) skipped=$((skipped + 1)); continue ;;
            '') log_warning "  $ns/$name: no apply: field — skipping. It must be yes, no or adopt."
                skipped=$((skipped + 1)); continue ;;
            *)  log_warning "  $ns/$name: apply: $apply is not one of yes, no, adopt — skipping."
                skipped=$((skipped + 1)); continue ;;
        esac
        # No modelID, no object. It is the grouping key and the only thing tying a
        # ScaledObject to what the container actually serves: an object created
        # without it registers a variant of a model nobody runs, and WVA then sizes
        # a group that does not exist. Guessing is worse than refusing.
        if [ -z "$model" ]; then
            log_warning "  $ns/$name: apply: $apply, but modelID is empty — NOT creating anything for it. Set the model the container serves (--served-model-name, else --model, else the positional model of 'vllm serve <model>') and run this again."
            unresolved=$((unresolved + 1)); skipped=$((skipped + 1)); continue
        fi
        # Bounds are edited by hand, and reach kubectl as JSON numbers. Anything
        # that is not one falls back to the default rather than being passed
        # through: a `minReplicas: two` would otherwise produce an invalid patch
        # against a live object, which is a worse way to find out.
        case "$min" in ''|*[!0-9]*) log_warning "  $ns/$name: minReplicas '$min' is not a number — using 1."; min=1 ;; esac
        case "$max" in ''|*[!0-9]*) log_warning "  $ns/$name: maxReplicas '$max' is not a number — using 10."; max=10 ;; esac
        case "$cost" in
            ''|*[!0-9.]*|*.*.*)
                log_warning "  $ns/$name: variantCost '$cost' is not a number — using $SO_DEFAULT_VARIANT_COST."
                cost="$SO_DEFAULT_VARIANT_COST" ;;
        esac

        existing=$(so_existing_name "$ns" "$name")
        if [ "$apply" = "yes" ] && [ -n "$existing" ]; then
            # Not silently adopted instead: "yes" asked for a new object, and
            # creating one beside $existing is two ScaledObjects on one target,
            # which is two HPAs writing one replica count. Adoption is a different
            # act, which is why it has its own value.
            log_warning "  $ns/$name: apply: yes, but ScaledObject $existing already targets it — skipping. Use apply: adopt to point that one at WVA."
            skipped=$((skipped + 1)); continue
        fi
        if [ "$apply" = "adopt" ] && [ -z "$existing" ]; then
            log_warning "  $ns/$name: apply: adopt, but nothing targets it any more — creating one instead."
            apply=yes
        fi

        if [ "$apply" = "adopt" ]; then
            # Triggers and bounds, and nothing else: the rest of that object —
            # polling interval, fallback, stabilization, whoever's labels — stays
            # as its author left it. The bounds are included because the plan
            # showed them, carried over from this very object, so applying them
            # unedited is a no-op and editing them is how you change them.
            if kubectl patch scaledobject "$existing" -n "$ns" --type=merge \
                -p "$(so_adopt_patch "$model" "$scaler_addr" "$min" "$max" "$cost" "$policy")" > /dev/null; then
                log_success "  $ns/$name ($kind) -> ScaledObject $existing now scales on WVA (modelID: $model, $min-$max)"
                adopted=$((adopted + 1))
            else
                log_warning "  $ns/$name: failed to update ScaledObject $existing"
            fi
            continue
        fi

        if render_scaledobject "$ns" "$kind" "$name" "$model" "$scaler_addr" \
            "$min" "$max" "$cost" "$policy" | kubectl apply -f - > /dev/null; then
            log_success "  $ns/$name ($kind) -> ScaledObject ${name}-wva (modelID: $model, $min-$max)"
            created=$((created + 1))
        else
            log_warning "  $ns/$name: failed to create its ScaledObject"
        fi
    done <<< "$rows"
    log_success "ScaledObjects: $created created, $adopted adopted, $skipped not applied"

    # Warm pools next, and only after the ScaledObjects exist: a pool is useless
    # until something can borrow from it, and creating one first would hold
    # accelerators for a namespace that is not yet being scaled at all.
    so_apply_warmpools "$file"

    # Non-zero, after the rest of the plan has been applied: an entry asking to be
    # created with no model is a mistake in the file, and a caller that scripts
    # this — the installer, CI — must not read "some of what you asked for" as
    # success. The entries that were complete are already applied; nothing here
    # needs undoing, and running it again after filling in the model is safe.
    if [ "$unresolved" -gt 0 ]; then
        if [ "$unresolved" = 1 ]; then
            log_error "1 entry asked to be applied with no modelID and was skipped. Fill in its modelID, or set apply: no."
        fi
        log_error "$unresolved entries asked to be applied with no modelID and were skipped. Fill in their modelID, or set apply: no."
    fi
}

# so_apply_warmpools creates the pools a plan asks for. Absent section, absent
# entries, or every entry `apply: no` all mean "no pools", which is the ordinary
# case and says nothing.
#
# A pool holds its accelerators from the moment it exists, so this only ever acts
# on an explicit yes. Everything the planner writes is "no".
so_apply_warmpools() {
    local file="$1" json rows made=0
    json=$(yq -o=json '.' "$file" 2>/dev/null) || return 0
    # `// empty` on the section, not on each field: a plan written before this
    # existed has no warmPools key at all, and that is not a defect in it.
    rows=$(printf '%s' "$json" | jq -r '
        (.warmPools // [])[]
        | select((.apply // "no" | tostring | ascii_downcase) as $a
                 | $a == "yes" or $a == "true")
        | [ (.namespace // ""), (.name // ""), (.accelerator // ""),
            (.gpus // 1 | tostring), (.models // 4 | tostring),
            (.modelSize // "8B" | tostring), (.replicas // 2 | tostring),
            (.max // 6 | tostring), (.reserve // 1 | tostring) ]
        | join("\u001f")' 2>/dev/null) || return 0
    [ -n "$rows" ] || return 0

    # The one value a plan cannot know: the pool proxy is an image somebody built
    # and pushed. WARMPOOL_PROXY_IMG is the same variable the Makefile builds it
    # under, so the common case needs no extra input.
    local image="${WARMPOOL_PROXY_IMG:-}"
    if [ -z "$image" ]; then
        log_warning "Warm pools were requested, but no proxy image is set, so none were created."
        log_warning "  The pool Pod runs an image this repo builds:"
        log_warning "    make docker-build-warmpool-proxy docker-push-warmpool-proxy"
        log_warning "  Then re-apply with WARMPOOL_PROXY_IMG=<ref>."
        return 0
    fi

    local wp_ns wp_name wp_acc wp_gpus wp_models wp_size wp_replicas wp_max wp_reserve
    while IFS=$'\037' read -r wp_ns wp_name wp_acc wp_gpus wp_models wp_size wp_replicas wp_max wp_reserve; do
        [ -n "$wp_name" ] || continue
        # An accelerator is not optional and must not be defaulted: a pool that
        # cannot name its GPU cannot prove any model fits it, and one named for
        # the wrong GPU is the silent mismatch the whole design exists to avoid.
        if [ -z "$wp_acc" ]; then
            log_warning "  warm pool $wp_name: no accelerator, so it was not created."
            continue
        fi
        if bash "$(dirname "${BASH_SOURCE[0]}")/../warmpool.sh" create \
                -n "$wp_ns" --name "$wp_name" --gpus "$wp_gpus" \
                --accelerator "$wp_acc" --models "$wp_models" --model-size "$wp_size" \
                --replicas "$wp_replicas" --max "$wp_max" --reserve "$wp_reserve" \
                --proxy-image "$image" --wva-namespace "${WVA_NS:-$wp_ns}"; then
            made=$((made + 1))
        else
            log_warning "  warm pool $wp_name: could not be created."
        fi
    done <<< "$rows"
    [ "$made" -eq 0 ] || log_success "Warm pools: $made created"
    if [ "$made" -gt 0 ]; then
        log_info "  A pool holds its accelerators from now on, warm model in it or not."
        log_info "  Point a model at it with the warmPool trigger key; remove it with:"
        log_info "    deploy/warmpool.sh delete -n <ns> --name <pool>"
    fi
}

install_default_scaledobjects() {
    local mode="${WVA_DEFAULT_SO:-false}"
    [ "$mode" != "false" ] || return 0

    local plan="${WVA_DEFAULT_SO_PLAN:-}"

    # An existing plan file is authoritative: this is the "edit the list and
    # continue" path, and it works with no terminal, which is what makes the
    # interactive capability available to scripts and CI as well.
    #
    # Except in `plan` mode, which promises to apply nothing. Without that
    # exception, running `make scaledobjects-plan WVA_DEFAULT_SO_PLAN=<file>` a
    # second time — the obvious thing to do after editing the file, and what a
    # scripted caller does on every run — APPLIED it. A command whose entire
    # contract is "look, do not touch" created autoscaling objects instead.
    if [ "$mode" != "plan" ] && [ -n "$plan" ] && [ -f "$plan" ]; then
        log_info "Applying the ScaledObject plan from $plan"
        so_show_plan "$plan"
        so_apply_plan "$plan"
        return 0
    fi

    [ -n "$plan" ] || plan=$(mktemp -t wva-scaledobject-plan.XXXXXX.yaml)
    log_info "Scanning for llm-d model servers..."
    so_discover > "$plan"

    if ! so_plan_rows "$plan" | grep -q .; then
        log_warning "No llm-d model servers found (label $SO_SERVING_MARKER, or $(printf '%s' "$SO_SERVING_ROLE_MARKERS" | tr ' ' '/')) in: $(so_target_namespaces | tr '\n' ' '). Deploy them first, then run 'make scaledobjects-apply'. Until a ScaledObject exists, WVA is never called and scales nothing."
        return 0
    fi

    so_show_plan "$plan"

    case "$mode" in
        plan)
            log_success "Plan written to $plan — nothing applied."
            log_info "Edit it — apply: yes/no/adopt, the modelID, the replica bounds — then:"
            log_info "    make scaledobjects-apply WVA_DEFAULT_SO_PLAN=$plan"
            log_info "Every field it takes is explained in the comments at the top of that file."
            return 0
            ;;
        edit)
            if [ ! -t 0 ]; then
                log_error "WVA_DEFAULT_SO=edit needs a terminal. Use WVA_DEFAULT_SO=plan, edit the file it writes, then apply it with WVA_DEFAULT_SO_PLAN=<file>."
            fi
            log_info "Opening the plan in ${EDITOR:-vi}. Set each apply: to yes, no or adopt; the comments at the top explain every field."
            read -r -p "  Press Enter to edit, or Ctrl-C to stop with the plan at $plan " _
            ${EDITOR:-vi} "$plan"
            so_show_plan "$plan"
            read -r -p "  Apply this plan? [y/N] " reply
            case "$reply" in
                [yY]*) ;;
                *) log_info "Nothing applied. The plan is at $plan"; return 0 ;;
            esac
            ;;
        true) : ;;
        *) log_error "WVA_DEFAULT_SO must be one of: false, plan, edit, true (got '$mode')" ;;
    esac

    so_apply_plan "$plan"
}

# so_adopt_patch prints the merge patch that repoints an existing ScaledObject at
# WVA: its triggers, and the bounds and cost the plan carried. Everything else on that
# object — polling interval, cooldown, fallback, advanced behavior, its labels —
# stays as whoever tuned it left it.
#
# `triggers` is a list, so a merge patch REPLACES it wholesale, which is what is
# wanted. An object scaled by a prometheus or cpu trigger must stop being scaled by
# it, or two scalers feed one HPA and the larger answer silently wins.
so_adopt_patch() {
    local model="$1" scaler_addr="$2" min="$3" max="$4" cost="$5" policy="${6:-}"
    # scalingPolicy is added only when the entry names one: an empty value would
    # be a metadata key whose value says "the default", which is what its absence
    # already says, and it would survive in the object long after anybody knew why.
    jq -nc --arg m "$model" --arg a "$scaler_addr" --arg c "$cost" --arg p "$policy" \
        --argjson min "$min" --argjson max "$max" \
        '{spec:{minReplicaCount:$min, maxReplicaCount:$max,
          triggers:[{type:"external-push",name:"wva-external-scaler",
          metadata:({scalerAddress:$a, modelID:$m, variantCost:$c}
                    + (if $p == "" then {} else {scalingPolicy:$p} end))}]}}'
}

# so_repoint_stale repoints ScaledObjects that ask for WVA by name but name a WVA
# that is not there, at the install that is.
#
# The samples hardcode the default namespace in scalerAddress, which is right for
# `deploy-e2e-infra` and wrong for a namespace-scoped install. The symptom is
# quiet -- KEDA cannot fetch a metric spec, falls back to CPU, and the HPA reads
# `cpu: <unknown>/80%` while WVA scales nothing -- and the manual remedy was a
# three-step plan/edit/apply dance to change one string.
#
# It repairs only what is provably broken. A candidate must satisfy BOTH:
#
#   1. It already asks for WVA: a trigger whose scalerAddress is
#      wva-external-scaler.<ns>... So an object scaled by prometheus or cpu is
#      never converted -- that is adoption, it is a real decision, and it stays
#      with `scaledobjects-plan` where the operator sees the bounds first.
#   2. The address it names resolves to NOTHING: no external-scaler Service in
#      that namespace.
#
# (2) is the load-bearing one. Two WVA installs on one cluster is a supported
# shape, and a ScaledObject pointing at the other one is correct, not stale --
# repointing it would hijack another team's workload, on a cluster where that is
# exactly the thing not to do. A live Service there means "deliberate", and
# deliberate is left alone and said out loud.
#
# Only scalerAddress is rewritten. modelID, variantCost and scalingPolicy are
# whatever the operator tuned, and a repoint is not the moment to reset them --
# so unlike so_adopt_patch, which replaces `triggers` wholesale because adoption
# must displace a competing scaler, this rewrites the list in place.
so_repoint_stale() {
    local dest want self
    dest=$(so_repoint_destination) || return 1
    want="wva-external-scaler.${dest}.svc.cluster.local:9090"
    self="wva-external-scaler.${dest}."

    local rows
    rows=$(so_repoint_candidates "$self") || return 1
    if [ -z "$rows" ]; then
        log_success "No ScaledObject names a WVA other than the one in $dest — nothing to repoint."
        return 0
    fi

    local ns name addr named_ns triggers
    local fixed=0 left=0
    while read -r ns name addr; do
        [ -n "$name" ] || continue
        # wva-external-scaler.<namespace>.svc...:port -- the second dotted field of
        # the host. The port is cut FIRST because the in-cluster short form
        # `wva-external-scaler.<ns>:9090` is legal and leaves the port stuck to the
        # namespace, which would make a live install look dead and get its
        # workload taken away.
        named_ns=$(printf '%s' "$addr" | cut -d: -f1 | cut -d. -f2)
        if [ -n "$named_ns" ] && kubectl get svc wva-external-scaler -n "$named_ns" > /dev/null 2>&1; then
            log_info "  $ns/$name: points at a WVA that is running in $named_ns — leaving it alone. Repointing it here would take it off that install."
            left=$((left + 1))
            continue
        fi

        triggers=$(kubectl get scaledobject "$name" -n "$ns" -o json 2>/dev/null \
            | jq -c --arg a "$want" '[ .spec.triggers[]?
                | if ((.metadata.scalerAddress // "") | startswith("wva-external-scaler."))
                  then .metadata.scalerAddress = $a else . end ]' 2>/dev/null)
        if [ -z "$triggers" ] || [ "$triggers" = "null" ]; then
            log_warning "  $ns/$name: could not read its triggers — skipping."
            continue
        fi

        if kubectl patch scaledobject "$name" -n "$ns" --type merge \
            -p "{\"spec\":{\"triggers\":$triggers}}" > /dev/null 2>&1; then
            log_success "  $ns/$name: $addr -> $want"
            fixed=$((fixed + 1))
        else
            log_warning "  $ns/$name: patch failed — it may be managed by GitOps, which will put the old address back. Change it at the source."
        fi
    done <<< "$rows"

    log_success "Repointed $fixed at $dest; left $left pointing at a live scaler elsewhere"
}

# so_repoint_destination echoes the namespace to repoint AT, or fails loudly.
#
# It is discovered rather than defaulted. WVA_NS falls back to
# workload-variant-autoscaler-system, which is the one namespace this command
# must never assume: a sample naming it and no controller there is the whole
# fault being repaired, so a stale default would rewrite addresses to another
# dead end and report success.
#
# An explicitly passed WVA_NS is still checked for a running scaler, because the
# destination being live is what makes the rewrite a fix rather than a different
# breakage.
so_repoint_destination() {
    local found n
    if [ -n "${WVA_NS_EXPLICIT:-}" ]; then
        if ! kubectl get svc wva-external-scaler -n "$WVA_NS_EXPLICIT" > /dev/null 2>&1; then
            log_error "WVA_NS=$WVA_NS_EXPLICIT has no wva-external-scaler Service, so nothing would answer at that address. Install WVA there, or drop WVA_NS and let this find the install."
            return 1
        fi
        echo "$WVA_NS_EXPLICIT"
        return 0
    fi

    found=$(wva_scaler_namespaces)
    n=$(printf '%s' "$found" | grep -c . || true)
    if [ "$n" -eq 0 ]; then
        log_error "No WVA external-scaler Service found on this cluster, so there is no install to point at. Install WVA first; if it is in a namespace you cannot list, pass WVA_NS=<namespace>."
        return 1
    fi

    # Prefer installs with a ready backend. A Service with no Endpoints is what a
    # half-removed install leaves behind, and choosing one would repoint every
    # workload at an address nothing answers -- which looks exactly like the
    # fault being repaired.
    local ready ns rn
    ready=""
    while read -r ns; do
        [ -n "$ns" ] || continue
        wva_scaler_has_endpoints "$ns" && ready="${ready}${ns}"$'\n'
    done <<< "$found"
    # Normalize once, so the blank the accumulator leaves behind never reaches a
    # count or a message.
    ready=$(printf '%s' "$ready" | grep . || true)
    rn=$(printf '%s' "$ready" | grep -c . || true)

    if [ "$rn" -eq 1 ]; then
        log_info "Repointing at the WVA install in $ready"
        echo "$ready"
        return 0
    fi
    if [ "$rn" -gt 1 ]; then
        log_error "Several WVA installs are running ($(printf '%s' "$ready" | tr '\n' ' ')). Which one a workload belongs to is your decision, not this command's — re-run with WVA_NS=<namespace>."
        return 1
    fi

    # None are ready. One candidate is still unambiguous -- WVA installed but not
    # up yet is a real state, and the address will start answering once it is.
    if [ "$n" -eq 1 ]; then
        log_warning "The WVA external-scaler Service in $found has no ready endpoints — the controller is not running yet. Repointing at it anyway; KEDA stays READY False until it comes up."
        echo "$found"
        return 0
    fi
    log_error "Several WVA external-scaler Services exist ($(printf '%s' "$found" | tr '\n' ' ')) and none has a ready endpoint, so there is no install to prefer. Bring one up, or re-run with WVA_NS=<namespace>."
    return 1
}

# so_repoint_candidates echoes `namespace name address` for every ScaledObject
# whose WVA trigger names something other than $1.
#
# Cluster-wide first, then the install's own namespace: the objects needing
# repair are in the WORKLOAD namespaces, which for a cluster-scoped install are
# not WVA_NS. A namespace admin who cannot list cluster-wide still gets their own
# namespace scanned, and WVA_DEFAULT_SO_NS narrows it deliberately.
so_repoint_candidates() {
    local self="$1" out
    local filter='
        .items[]
        | .metadata.namespace as $ns | .metadata.name as $n
        | [ .spec.triggers[]?.metadata.scalerAddress // ""
            | select(startswith("wva-external-scaler.") and (startswith($self) | not)) ]
        | select(length > 0)
        | $ns + " " + $n + " " + .[0]'

    if [ -n "${WVA_DEFAULT_SO_NS:-}" ]; then
        local ns
        for ns in $(so_target_namespaces); do
            [ -n "$ns" ] || continue
            kubectl get scaledobject -n "$ns" -o json 2>/dev/null \
                | jq -r --arg self "$self" "$filter" 2>/dev/null || true
        done
        return 0
    fi

    if out=$(kubectl get scaledobject -A -o json 2>/dev/null); then
        printf '%s' "$out" | jq -r --arg self "$self" "$filter" 2>/dev/null || true
        return 0
    fi
    log_info "Cannot list ScaledObjects cluster-wide — scanning $WVA_NS only. Pass WVA_DEFAULT_SO_NS=<namespace> to scan somewhere else."
    kubectl get scaledobject -n "$WVA_NS" -o json 2>/dev/null \
        | jq -r --arg self "$self" "$filter" 2>/dev/null || true
}

# render_scaledobject prints one ScaledObject: the shipped shape, or yours.
#
# WVA_DEFAULT_SO_TEMPLATE=<file> substitutes your own template instead, so a fleet
# with house conventions — fallback policy, stabilization windows, labels its
# tooling expects — gets those rather than a shape it then has to edit back.
# Placeholders, all optional:
#
#   {{NAMESPACE}} {{NAME}} {{KIND}} {{APIVERSION}} {{MODEL_ID}}
#   {{SCALER_ADDRESS}} {{MIN}} {{MAX}} {{VARIANT_COST}}
#
# Substitution is literal, so a template is also just a valid manifest with the
# placeholders written in — you can `kubectl apply` it by hand to check the shape
# before letting the installer fill it in for every model server you have.
render_scaledobject() {
    local ns="$1" kind="$2" target="$3" model="$4" scaler_addr="$5" min="$6" max="$7" cost="${8:-}"
    local policy="${9:-}"
    local api="apps/v1"

    # The last gate before a manifest exists. The caller checks this too, and the
    # caller's check was once defeated by a field-splitting bug that shifted a
    # replica count into this argument — so the check that matters is the one next
    # to the thing being built, where no amount of upstream plumbing can skip it.
    [ -n "$model" ] || log_error "refusing to build a ScaledObject for $ns/$target with an empty modelID"
    [ "$kind" = "LeaderWorkerSet" ] && api="leaderworkerset.x-k8s.io/v1"

    local tmpl="${WVA_DEFAULT_SO_TEMPLATE:-}"
    if [ -n "$tmpl" ]; then
        if [ ! -f "$tmpl" ]; then
            log_error "WVA_DEFAULT_SO_TEMPLATE=$tmpl does not exist"
        fi
        sed -e "s|{{NAMESPACE}}|${ns}|g" \
            -e "s|{{NAME}}|${target}|g" \
            -e "s|{{KIND}}|${kind}|g" \
            -e "s|{{APIVERSION}}|${api}|g" \
            -e "s|{{MODEL_ID}}|${model}|g" \
            -e "s|{{SCALER_ADDRESS}}|${scaler_addr}|g" \
            -e "s|{{MIN}}|${min}|g" \
            -e "s|{{MAX}}|${max}|g" \
            -e "s|{{VARIANT_COST}}|${cost:-$SO_DEFAULT_VARIANT_COST}|g" \
            -e "s|{{SCALING_POLICY}}|${policy}|g" \
            "$tmpl"
        return
    fi
    render_default_scaledobject "$ns" "$kind" "$target" "$model" "$scaler_addr" "$min" "$max" "$cost" "$policy"
}

# render_default_scaledobject prints one ScaledObject.
#
# external-push, not external: KEDA then holds a StreamIsActive stream open and WVA
# pushes activation the moment it decides, which is what lets a workload parked at
# zero wake in about the detection interval instead of a poll period.
#
# min defaults to 1 even where scale-to-zero is enabled: parking a model costs its
# next request a cold start, and that is a decision about that workload's users,
# not one an installer should make for them.
render_default_scaledobject() {
    local ns="$1" kind="$2" target="$3" model="$4" scaler_addr="$5" min="$6" max="$7" cost="${8:-}"
    local policy="${9:-}"
    local api="apps/v1"
    [ "$kind" = "LeaderWorkerSet" ] && api="leaderworkerset.x-k8s.io/v1"

    # Written only when the entry names a tier — an absent key and an empty one
    # mean the same thing to WVA, and only one of them says so to a reader.
    local policy_line=""
    [ -z "$policy" ] || policy_line=$'\n        scalingPolicy: "'"$policy"'"'

    cat <<EOF
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: ${target}-wva
  namespace: ${ns}
  labels:
    app.kubernetes.io/managed-by: workload-variant-autoscaler
    app.kubernetes.io/component: default-scaledobject
  annotations:
    llm-d.ai/created-by: "deploy/lib/scaledobject.sh"
spec:
  scaleTargetRef:
    apiVersion: ${api}
    kind: ${kind}
    name: ${target}
  pollingInterval: 5
  cooldownPeriod: 30
  minReplicaCount: ${min}
  maxReplicaCount: ${max}
  advanced:
    restoreToOriginalReplicaCount: true
    # Scaling behaviour, stated rather than inherited.
    #
    # Kubernetes' own defaults are already scaleUp stabilization 0 and scaleDown
    # 300, so on a stock cluster this changes only the policy periods. It is
    # written out anyway because those defaults are the API server's, not ours:
    # a cluster that changes them would silently change how every WVA-created
    # workload scales, and nothing here would say so.
    #
    # The asymmetry is the point. Scale-up acts on the current recommendation
    # (stabilization 0) because a decode replica measured 61s to Ready and the
    # wait is already paid in cold start. Scale-down waits for 300s of sustained
    # low demand, because removing a replica too eagerly costs that cold start
    # on the next request while keeping one too long only costs money.
    #
    # The period is a rate limit, not a delay, and with "Percent 100" one step
    # may move the whole way -- so the conservatism lives in the window above,
    # not here. Nothing reacts faster than the HPA control loop either, which
    # re-evaluates on kube-controller-manager's sync period (15s by default).
    #
    # Override per install with WVA_SO_SCALE_{UP,DOWN}_{PERIOD,STABILIZATION},
    # or replace the shape entirely with WVA_DEFAULT_SO_TEMPLATE.
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          stabilizationWindowSeconds: ${WVA_SO_SCALE_UP_STABILIZATION:-0}
          policies:
            - type: Percent
              value: 100
              periodSeconds: ${WVA_SO_SCALE_UP_PERIOD:-5}
        scaleDown:
          stabilizationWindowSeconds: ${WVA_SO_SCALE_DOWN_STABILIZATION:-300}
          policies:
            - type: Percent
              value: 100
              periodSeconds: ${WVA_SO_SCALE_DOWN_PERIOD:-120}
  triggers:
    - type: external-push
      name: wva-external-scaler
      metadata:
        scalerAddress: ${scaler_addr}
        modelID: "${model}"
        variantCost: "${cost:-$SO_DEFAULT_VARIANT_COST}"${policy_line}
EOF
}

# --- Parking and freezing -----------------------------------------------------
#
# Idling a WVA-managed workload is not `kubectl scale --replicas=0`: the HPA KEDA
# owns enforces minReplicaCount and puts it straight back. The three obvious moves
# all fail, one of them dangerously:
#
#   kubectl scale --replicas=0     HPA restores minReplicaCount within seconds.
#   maxReplicaCount: 0             not a valid state — KEDA hands it to the HPA as
#                                  the ceiling, and a maximum of zero leaves no
#                                  room to scale up.
#   scale the WVA controller to 0  WORSE THAN NOTHING. KEDA cannot fetch a metric
#                                  spec from a scaler that is gone, so it FALLS
#                                  BACK TO A CPU METRIC and keeps scaling the
#                                  workload between min and max — see
#                                  "cpu: <unknown>/80%" in
#                                  docs/guides/install-in-namespace/README.md.
#                                  The workload keeps its GPUs and is now sized by
#                                  the wrong signal, while everything reads healthy.
#
# The supported lever is on the ScaledObject, and the controller can stay running:
# it holds no GPU and keeping it up keeps the install verifiable.
#
#   park    autoscaling.keda.sh/paused-replicas="<n>"  drives the target to n
#           (0 by default), then freezes the scale loop. KEDA also removes the HPA,
#           so nothing can raise the count while it is parked.
#   freeze  autoscaling.keda.sh/paused="true"          holds whatever is running
#           now. For maintenance, where dropping to zero is not wanted.
#   resume  remove both.
#
# What this is NOT: autonomous idling. `idleReplicaCount: 0` (or minReplicaCount: 0)
# scales to zero when triggers go inactive and wakes on demand — a policy, not an
# operation — and on llm-d it needs the EPP's flowControl feature gate, without
# which it parks and never wakes. Parking is deterministic and needs no gate.

# so_pause_marker is what identifies a ScaledObject as WVA's: a trigger addressing
# the external scaler by name. Matched on the address rather than on
# app.kubernetes.io/managed-by, because an `apply: adopt` object was created by
# something else and carries that label from whatever made it — but its trigger
# names WVA, which is the thing that makes it WVA's to park.
readonly SO_PAUSE_ANN_REPLICAS='autoscaling.keda.sh/paused-replicas'
readonly SO_PAUSE_ANN_PAUSED='autoscaling.keda.sh/paused'

# so_pause_rows echoes one TSV row per WVA-triggered ScaledObject in scope:
#
#   namespace  name  kind  target  min  max  state  replicas  gpus
#
# state is park:<n>, freeze or live. gpus is what the workload is holding right
# now — per-replica limits times current replicas — because that is the number
# anyone parking a workload on a shared cluster actually wants to see.
so_pause_rows() {
    local ns rows
    for ns in $(so_target_namespaces); do
        [ -n "$ns" ] || continue
        rows=$(kubectl get scaledobject -n "$ns" -o json 2>/dev/null | jq -r --arg pr "$SO_PAUSE_ANN_REPLICAS" --arg pp "$SO_PAUSE_ANN_PAUSED" '
            .items[]?
            | select([ .spec.triggers[]?.metadata.scalerAddress // ""
                       | select(startswith("wva-external-scaler.")) ] | length > 0)
            | [ .metadata.namespace,
                .metadata.name,
                (.spec.scaleTargetRef.kind // "Deployment"),
                (.spec.scaleTargetRef.name // "-"),
                (.spec.minReplicaCount // 0 | tostring),
                (.spec.maxReplicaCount // 100 | tostring),
                (if (.metadata.annotations[$pr] // "") != "" then "park:" + .metadata.annotations[$pr]
                 elif (.metadata.annotations[$pp] // "") == "true" then "freeze"
                 else "live" end)
              ] | @tsv' 2>/dev/null) || continue
        [ -n "$rows" ] || continue
        # Replicas and GPUs come from the scale target, not the ScaledObject, so
        # they are read per row rather than in the query above.
        printf '%s\n' "$rows" | while IFS=$'\t' read -r rns name kind target min max state; do
            [ -n "$name" ] || continue
            local reps gpu
            reps=$(so_pause_target_replicas "$rns" "$kind" "$target")
            gpu=$(so_pause_target_gpus "$rns" "$kind" "$target")
            printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
                "$rns" "$name" "$kind" "$target" "$min" "$max" "$state" "${reps:-0}" \
                "$(( ${reps:-0} * ${gpu:-0} ))"
        done
    done
}

# so_pause_target_replicas echoes the scale target's current replica count.
so_pause_target_replicas() {
    local ns="$1" kind="$2" name="$3"
    kubectl get "$kind" "$name" -n "$ns" -o jsonpath='{.spec.replicas}' 2>/dev/null \
        | tr -cd '0-9' | sed 's/^$/0/'
}

# so_pause_target_gpus echoes the GPUs ONE replica of the target holds, summed over
# its containers. nvidia.com/gpu only: that is the limit llm-d's guides set, and a
# wrong guess here would misreport what parking frees.
so_pause_target_gpus() {
    local ns="$1" kind="$2" name="$3"
    kubectl get "$kind" "$name" -n "$ns" -o json 2>/dev/null | jq -r '
        # Same arithmetic as the controller, so the number an operator reads here is
        # the number WVA is working from. See GetTotalGPUsPerReplica in
        # internal/utils/scaletarget/{deployment,lws}.go:
        #   Deployment  sum over the pod templates containers.
        #   LWS         leader_GPUs + (size - 1) * worker_GPUs, size defaulting to 1.
        #               .spec.replicas is the GROUP count, so this is per group --
        #               reading workerTemplate alone ignored both the leader and the
        #               group size and under-reported every LWS workload.
        #   either      0 means "no explicit request", which for an inference
        #               workload usually means one GPU rather than none. The
        #               controller defaults to 1 there and so must this, or parking
        #               such a workload reports freeing nothing.
        # Every vendor resource the controller counts (internal/constants), not
        # nvidia alone. requests is what the controller reads; limits is a fallback,
        # and Kubernetes constrains the two equal for extended resources.
        def gpus($cs):
          [ $cs[]? | .resources as $r
            | ( [ "nvidia.com/gpu", "amd.com/gpu", "habana.ai/gaudi",
                  "gpu.intel.com/i915", "gpu.intel.com/xe" ]
                | map(. as $k | (($r.requests[$k] // $r.limits[$k] // 0) | tonumber))
                | add ) ] | add // 0;
        ( if .spec.leaderWorkerTemplate then
            .spec.leaderWorkerTemplate as $t
            | (if $t.leaderTemplate then gpus($t.leaderTemplate.spec.containers) else 0 end)
              + ((($t.size // 1) - 1) * gpus($t.workerTemplate.spec.containers))
          else
            gpus(.spec.template.spec.containers)
          end )
        | if . <= 0 then 1 else . end' 2>/dev/null | tr -cd '0-9' | sed 's/^$/0/'
}

# SO_PAUSE_NOTHING_TO_DO is a sentinel exit code, not an error: it means the
# caller asked for nothing to happen (no ScaledObject in scope, or an empty
# answer at the prompt), and so_park/so_freeze/so_resume should quietly return 0.
#
# It has to be a code, not just "any nonzero": so_pause_select is invoked as
# `r="$(so_pause_select ...)"`. Bash preserves the inner exit status correctly
# through that substitution -- log_error's `exit 1` really does end up as $?=1 in
# the caller -- but a genuinely QUIET "nothing to do" (this file, return 1) and a
# genuine `log_error` fatal (deploy/lib/common.sh, also exit 1) were both plain
# `1`, so `|| return 0` could not tell "nothing selected" from "SO=<typo>, refused"
# apart -- both were silently reported as success. `make so-park SO=typo` exited
# 0 having parked nothing, which is the one outcome the SO= parameter exists to
# prevent going unnoticed in CI.
readonly SO_PAUSE_NOTHING_TO_DO=3

# so_pause_select echoes the rows the caller should act on.
#
# Interactive by default, because "which workload" is the one question a script
# cannot answer for someone parking GPUs on a shared cluster, and picking wrong
# takes down a workload that was meant to keep serving. SO=<name>[,<name>] or
# SO=all skips the prompt, for CI and for anyone who already knows.
so_pause_select() {
    local verb="$1" rows
    rows="$(so_pause_rows)"
    if [ -z "$rows" ]; then
        log_warning "No ScaledObject in $(so_target_namespaces | paste -sd, -) names WVA's external scaler, so there is nothing to $verb."
        log_info "  A workload nothing has registered with WVA is not autoscaled by it — 'kubectl scale' works on that normally."
        return "$SO_PAUSE_NOTHING_TO_DO"
    fi

    # Named explicitly: no prompt.
    if [ -n "${SO:-}" ]; then
        if [ "$SO" = "all" ]; then printf '%s\n' "$rows"; return 0; fi
        local want found=""
        for want in $(printf '%s' "$SO" | tr ',' ' '); do
            local match ns_want=""
            # SO=<namespace>/<name> disambiguates; SO=<name> alone is a footgun
            # for a cluster-scoped install watching several namespaces
            # (WVA_DEFAULT_SO_NS=all) -- ScaledObject names are only unique WITHIN
            # a namespace, so a bare name can match the same-named object in two
            # different namespaces, and applying to both is very likely not what
            # was meant. Checked below rather than assumed away.
            case "$want" in
                */*) ns_want="${want%%/*}"; want="${want#*/}" ;;
            esac
            if [ -n "$ns_want" ]; then
                match=$(printf '%s\n' "$rows" | awk -F'\t' -v n="$ns_want" -v w="$want" '$1 == n && $2 == w')
            else
                match=$(printf '%s\n' "$rows" | awk -F'\t' -v w="$want" '$2 == w')
                local match_ns_count
                match_ns_count=$(printf '%s\n' "$match" | awk -F'\t' 'NF{print $1}' | sort -u | grep -c .)
                if [ "${match_ns_count:-0}" -gt 1 ]; then
                    log_error "SO=$want matches a ScaledObject of that name in more than one namespace: $(printf '%s\n' "$match" | awk -F'\t' 'NF{print $1}' | sort -u | paste -sd, -). ScaledObject names are unique only within a namespace, not across the cluster. Say which:  SO=<namespace>/$want"
                fi
            fi
            if [ -z "$match" ]; then
                log_error "SO=${ns_want:+$ns_want/}$want is not a WVA-triggered ScaledObject in $(so_target_namespaces | paste -sd, -). Run 'make so-list' to see what is there."
            fi
            found="${found}${match}
"
        done
        printf '%s' "$found" | grep -v '^$'
        return 0
    fi

    so_pause_show "$rows" >&2
    if [ ! -t 0 ]; then
        log_error "Which ScaledObject to $verb needs a terminal, or naming one: SO=<name> (comma-separated for several), or SO=all."
    fi

    local reply
    printf '\n' >&2
    read -r -p "  Which to $verb? number(s) separated by spaces, 'a' for all, or Ctrl-C to stop: " reply
    [ -n "$reply" ] || { log_warning "Nothing selected — no change made."; return "$SO_PAUSE_NOTHING_TO_DO"; }
    if [ "$reply" = "a" ] || [ "$reply" = "all" ]; then printf '%s\n' "$rows"; return 0; fi

    local n out=""
    for n in $reply; do
        case "$n" in
            ''|*[!0-9]*) log_error "'$n' is not a number. Nothing has been changed." ;;
        esac
        local row
        row=$(printf '%s\n' "$rows" | sed -n "${n}p")
        [ -n "$row" ] || log_error "There is no $n in that list. Nothing has been changed."
        out="${out}${row}
"
    done
    printf '%s' "$out" | grep -v '^$'
}

# so_pause_show prints the numbered table the prompt refers to.
so_pause_show() {
    local rows="$1" nscount
    # ScaledObject names are unique only WITHIN a namespace. SO= already refuses a
    # bare name matching in several, but this numbered list had no namespace
    # column -- so two same-named objects rendered as two identical rows and the
    # prompt asked the operator to choose between them blind. Choosing wrong is
    # exactly what the prompt exists to prevent. Shown only when the scope really
    # spans more than one namespace, since the kind column was already dropped
    # (below) to keep GPUS and STATE on the width of a normal terminal.
    nscount=$(printf '%s\n' "$rows" | awk -F'\t' 'NF{print $1}' | sort -u | grep -c .)
    printf '\n  WVA-managed workloads in %s:\n\n' "$(so_target_namespaces | paste -sd, -)"
    # The kind is shown only when it is not a Deployment: it is "Deployment" for
    # almost every row, and spelling it out pushed the columns that carry the
    # decision -- GPUS and STATE -- off the width of a normal terminal.
    if [ "${nscount:-0}" -gt 1 ]; then
        printf '    #  %-22s %-38s %-30s %4s %4s %5s %s\n' NAMESPACE NAME TARGET MIN MAX GPUS STATE
        printf '%s\n' "$rows" | awk -F'\t' '{
            t = ($3 == "Deployment") ? $4 : $3 "/" $4
            printf "    %d  %-22s %-38s %-30s %4s %4s %5s %s\n", NR, $1, $2, t, $5, $6, $9, $7
        }'
    else
        printf '    #  %-46s %-40s %4s %4s %5s %s\n' NAME TARGET MIN MAX GPUS STATE
        printf '%s\n' "$rows" | awk -F'\t' '{
            t = ($3 == "Deployment") ? $4 : $3 "/" $4
            printf "    %d  %-46s %-40s %4s %4s %5s %s\n", NR, $2, t, $5, $6, $9, $7
        }'
    fi
}

# so_pause_apply parks, freezes or resumes the rows on stdin.
so_pause_apply() {
    local mode="$1" rows="$2" replicas="${PARK_REPLICAS:-0}"
    local ns name state gpus changed=0 freed=0

    while IFS=$'\t' read -r ns name _kind _target _min _max state _reps gpus; do
        [ -n "$name" ] || continue
        case "$mode" in
            park)
                if [ "$state" = "park:$replicas" ]; then
                    log_info "  $ns/$name is already parked at $replicas — leaving it."
                    continue
                fi
                # One annotation, not both. KEDA honours paused-replicas first when
                # both are present, so carrying a stale `paused` alongside it only
                # makes the object's state ambiguous to read.
                kubectl annotate scaledobject "$name" -n "$ns" \
                    "$SO_PAUSE_ANN_REPLICAS=$replicas" --overwrite >/dev/null 2>&1 || {
                    log_warning "  $ns/$name: could not annotate — skipping."; continue; }
                kubectl annotate scaledobject "$name" -n "$ns" "${SO_PAUSE_ANN_PAUSED}-" >/dev/null 2>&1 || true
                log_success "  $ns/$name parked at $replicas (was $state, holding ${gpus:-0} GPU)"
                changed=$((changed + 1)); freed=$((freed + ${gpus:-0}))
                ;;
            freeze)
                if [ "$state" = "freeze" ]; then
                    log_info "  $ns/$name is already frozen — leaving it."
                    continue
                fi
                # Freezing something already PARKED would hold it at the count it is
                # parked to, which is what parking already does — so it changes
                # nothing an operator wants and leaves the object describing itself
                # two different ways. Refuse rather than do it: whoever asked
                # almost certainly wants it serving again first.
                case "$state" in
                    park:*)
                        log_warning "  $ns/$name is parked at ${state#park:} — not freezing it. Freeze holds the CURRENT count, so this would just hold it at ${state#park:}."
                        log_info "    To have it serving and then frozen:  make so-resume SO=$ns/$name   (wait for it to be ready)   make so-freeze SO=$ns/$name"
                        continue ;;
                esac
                kubectl annotate scaledobject "$name" -n "$ns" \
                    "$SO_PAUSE_ANN_PAUSED=true" --overwrite >/dev/null 2>&1 || {
                    log_warning "  $ns/$name: could not annotate — skipping."; continue; }
                kubectl annotate scaledobject "$name" -n "$ns" "${SO_PAUSE_ANN_REPLICAS}-" >/dev/null 2>&1 || true
                log_success "  $ns/$name frozen at its current size (was $state)"
                changed=$((changed + 1))
                ;;
            resume)
                if [ "$state" = "live" ]; then
                    log_info "  $ns/$name is not parked or frozen — leaving it."
                    continue
                fi
                kubectl annotate scaledobject "$name" -n "$ns" \
                    "${SO_PAUSE_ANN_REPLICAS}-" "${SO_PAUSE_ANN_PAUSED}-" >/dev/null 2>&1 || {
                    log_warning "  $ns/$name: could not remove the annotations — skipping."; continue; }
                log_success "  $ns/$name resumed (was $state) — KEDA recreates its HPA and WVA sizes it from live metrics"
                changed=$((changed + 1))
                ;;
        esac
    done <<< "$rows"

    [ "$changed" -gt 0 ] || { log_info "Nothing changed."; return 0; }
    case "$mode" in
        park)
            [ "$freed" -gt 0 ] && log_success "Parked $changed workload(s), releasing $freed GPU(s) once the pods terminate."
            [ "$freed" -eq 0 ] && log_success "Parked $changed workload(s)."
            log_info "  Resume with: make so-resume"
            ;;
        freeze)
            log_success "Froze $changed workload(s) at their current size. They keep their GPUs; only autoscaling stopped."
            log_info "  Resume with: make so-resume" ;;
        resume)
            log_success "Resumed $changed workload(s). Each returns to at least its minReplicaCount, and a model server takes a few minutes to load."
            log_info "  Watch: kubectl get scaledobject,hpa -n $(so_target_namespaces | head -1)" ;;
    esac
}

# so_park / so_freeze / so_resume are what the make targets call.
# so_pause_run is the shared body of so_park/so_freeze/so_resume: capture
# so_pause_select's rows, and tell its sentinel "nothing to do" apart from
# everything else nonzero, which is a fatal `log_error` that already printed why
# and must not be reported as success.
so_pause_run() {
    local mode="$1" verb="$2" r rc
    r="$(so_pause_select "$verb")"; rc=$?
    if [ "$rc" -eq "$SO_PAUSE_NOTHING_TO_DO" ]; then
        return 0
    elif [ "$rc" -ne 0 ]; then
        return 1
    fi
    so_pause_apply "$mode" "$r"
}
so_park()   { so_pause_run park   park; }
so_freeze() { so_pause_run freeze freeze; }
so_resume() { so_pause_run resume resume; }

# so_list just shows the table, so "what is parked" needs no guessing.
so_list() {
    local rows
    rows="$(so_pause_rows)"
    if [ -z "$rows" ]; then
        log_warning "No ScaledObject in $(so_target_namespaces | paste -sd, -) names WVA's external scaler."
        return 0
    fi
    so_pause_show "$rows"
}

# so_verify_scaledobjects reports whether every discovered model server's
# ScaledObject still targets the model its container actually serves.
#
# Built after a real incident: a Deployment's serving model was changed by
# hand without also updating the ScaledObject already scaling it. Nothing
# re-syncs that automatically -- this file only ever writes modelID once, at
# creation -- so WVA kept evaluating decisions for a model no scraped metric
# ever matched, and applied ZERO scaling decisions for an entire run. Silently:
# the HPA still read a healthy ratio, and the controller logged nothing wrong.
#
# Read-only: reuses install_default_scaledobjects in plan mode, so this goes
# through the same discovery any real fix runs through -- deriving modelID
# from a live container's actual `vllm serve` args, the code behind
# `make scaledobjects-apply ... apply: adopt` -- rather than a second,
# divergent implementation. Never calls so_apply_plan, never mutates anything.
so_verify_scaledobjects() {
    local plan_file
    # X's at the END of the template, and no `-t`: GNU mktemp deprecates -t, and
    # BSD/macOS -t takes a PREFIX rather than a template, so
    # `-t name.XXXXXX.yaml` means different things on the two platforms this repo
    # is developed on. An explicit directory plus a trailing-X template is the
    # one form both accept.
    plan_file=$(mktemp "${TMPDIR:-/tmp}/wva-verify-plan.XXXXXX") || return 1
    # Expanded at trap-set time, not read from $plan_file when the trap fires: a
    # RETURN trap runs after the function locals are gone. (A `log_error` inside
    # so_plan_rows exits the shell outright, and no RETURN trap covers that --
    # that path still leaves one file behind.)
    trap "rm -f '$plan_file'" RETURN

    # Silenced on both streams: log_info/log_success write to stderr, and this
    # call's own "plan written, edit it" messaging is about a temp file this
    # function deletes before returning -- irrelevant to what verify reports.
    WVA_DEFAULT_SO=plan WVA_DEFAULT_SO_PLAN="$plan_file" install_default_scaledobjects >/dev/null 2>&1

    echo
    echo "=== modelID drift check ==="
    printf '%-55s %-20s %s\n' "WORKLOAD" "SERVES" "STATUS"

    local drift=0 unregistered=0 unresolved=0 ok=0
    local apply p_ns kind name model minr maxr cost policy existing configured
    while IFS=$'\037' read -r apply p_ns kind name model minr maxr cost policy; do
        [ -n "$name" ] || continue
        if [ -z "$model" ]; then
            printf '%-55s %-20s %s\n' "$p_ns/$name" "(unreadable)" "SKIP"
            unresolved=$((unresolved + 1))
            continue
        fi
        existing=$(so_existing_name "$p_ns" "$name")
        if [ -z "$existing" ]; then
            printf '%-55s %-20s %s\n' "$p_ns/$name" "$model" "UNREGISTERED"
            unregistered=$((unregistered + 1))
            continue
        fi
        configured=$(kubectl get scaledobject -n "$p_ns" "$existing" -o json 2>/dev/null \
            | jq -r '[.spec.triggers[]? | select(.type | startswith("external")) | .metadata.modelID // empty] | first // empty')
        if [ "$configured" = "$model" ]; then
            printf '%-55s %-20s %s\n' "$p_ns/$name" "$model" "OK"
            ok=$((ok + 1))
        else
            printf '%-55s %-20s %s\n' "$p_ns/$name" "$model" "DRIFT ($existing: ${configured:-(empty)})"
            drift=$((drift + 1))
        fi
    done < <(so_plan_rows "$plan_file")
    rm -f "$plan_file"

    echo
    echo "verify-scaledobjects: $ok ok, $drift drift, $unregistered unregistered, $unresolved unresolved"
    if [ "$drift" -gt 0 ]; then
        log_warning "  DRIFT: a ScaledObject's modelID no longer matches what its container serves."
        log_warning "    WVA silently applies zero decisions for that workload -- it never matches a"
        log_warning "    scraped metric. Fix through the code that owns this config, not by hand-patching:"
        log_warning "      make scaledobjects-plan   # edit that entry's apply: to adopt"
        log_warning "      make scaledobjects-apply WVA_DEFAULT_SO_PLAN=<edited file>"
    fi
    if [ "$unregistered" -gt 0 ]; then
        log_warning "  UNREGISTERED: a discovered model server has no ScaledObject at all --"
        log_warning "    it is never autoscaled by WVA. Review with:  make scaledobjects-plan"
    fi
    # Said out loud. An unresolved workload is one this check COULD NOT VERIFY,
    # and it used to be counted and then passed over in silence -- so a drifted
    # ScaledObject on a workload whose model is unreadable produced a clean bill
    # of health. Not a hard failure, because a legitimate non-vLLM workload (the
    # inference simulator the e2e runs) has no readable model and would fail
    # every run; but silence is the wrong answer to "is this right?".
    if [ "$unresolved" -gt 0 ]; then
        log_warning "  UNRESOLVED: $unresolved workload(s) serve a model this check could not read,"
        log_warning "    so their ScaledObjects were NOT checked for drift. This is not a report that"
        log_warning "    they are correct. Expected for a non-vLLM/SGLang server; otherwise look at"
        log_warning "    the container's args:  make scaledobjects-plan"
    fi

    [ "$drift" -eq 0 ] && [ "$unregistered" -eq 0 ]
}

# so_verify_fma reports whether every FMA launcher pod, in every target
# namespace, is actually covered by a scrape target.
#
# Reuses wva_launcher_scrapers -- the same "would this monitor generate a
# target" test so_discover_fma_requesters already runs at line ~868 while
# building a ScaledObject plan -- as its own read-only check, so it also
# catches a namespace with no FMA requester ScaledObject planned this run (an
# apply: no entry, or none discovered at all) where the plan output would
# otherwise never mention launcher scraping.
#
# A namespace with no launcher pods is reported, not skipped: on a
# cluster-scoped install it says plainly which namespaces run no FMA at all,
# rather than leaving their absence from the table looking like an oversight.
so_verify_fma() {
    echo
    echo "=== FMA launcher scrape check ==="
    printf '%-40s %-10s %s\n' "NAMESPACE" "LAUNCHERS" "STATUS"

    local ns launchers scrapers unscraped=0 none=0 ok=0 unreadable=0
    for ns in $(so_target_namespaces); do
        # The listing status is checked, not just its line count. Piping a failed
        # `kubectl get` into `wc -l` yields 0, which read as "this namespace has
        # no launcher pods" -- so a Forbidden listing was reported as nothing to
        # check, and the run still exited 0. Forbidden is not absent.
        local pods_out
        if ! pods_out=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
                        --no-headers 2>/dev/null); then
            printf '%-40s %-10s %s\n' "$ns" "?" "UNREADABLE"
            unreadable=$((unreadable + 1))
            continue
        fi
        launchers=$(printf '%s' "$pods_out" | grep -c . || true)
        if [ "${launchers:-0}" -eq 0 ]; then
            printf '%-40s %-10s %s\n' "$ns" "0" "-"
            none=$((none + 1))
            continue
        fi
        scrapers=$(wva_launcher_scrapers "$ns" | paste -sd, -)
        if [ -n "$scrapers" ]; then
            printf '%-40s %-10s %s\n' "$ns" "$launchers" "OK ($scrapers)"
            ok=$((ok + 1))
        else
            printf '%-40s %-10s %s\n' "$ns" "$launchers" "UNSCRAPED"
            unscraped=$((unscraped + 1))
        fi
    done

    echo
    echo "verify-fma: $ok ok, $unscraped unscraped, $none with no launcher pods, $unreadable unreadable"
    if [ "$unreadable" -gt 0 ]; then
        log_warning "  UNREADABLE: $unreadable namespace(s) could not be listed, so nothing is known"
        log_warning "    about their launcher pods. This is NOT a report that they are scraped."
    fi
    if [ "$unscraped" -gt 0 ]; then
        log_warning "  UNSCRAPED: launcher pods present but nothing generates a scrape target for them."
        log_warning "    That FMA variant scales blind -- WVA never sees its engine metrics, and the HPA"
        log_warning "    still reads a healthy ratio the whole time. Fix:"
        log_warning "      kubectl apply -k config/fma-launcher-metrics -n <ns>   # or WVA_FMA_LAUNCHER_METRICS=true at install"
    fi

    # Unreadable counts against the result here, unlike an unresolved model
    # above: not being allowed to look at a namespace is a problem with the run,
    # not a property of the workload.
    [ "$unscraped" -eq 0 ] && [ "$unreadable" -eq 0 ]
}
