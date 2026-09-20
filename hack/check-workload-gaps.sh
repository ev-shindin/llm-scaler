#!/usr/bin/env bash
#
# Executes the workload-readiness path in deploy/lib/scaledobject.sh against
# fixture pod specs.
#
# It exists because `bash -n` is the only check these libraries had, and every
# defect it pins down parses cleanly: an engine chosen by name that landed on a
# routing proxy, a preStop check that counted hooks pod-wide, a "weights persist"
# test satisfied by a PVC mounted anywhere, and an unreadable object reported as
# healthy. None are syntax errors and all silently answer wrongly about a running
# workload.
#
# WRITING A CASE HERE: every case needs at least one POSITIVE assertion. An
# earlier version of this file was mutation-tested by deleting the library
# outright, and 10 of its 25 cases still printed `ok` -- `assert_not_contains`
# and `[ -z "$out" ]` are both satisfied by a function that does not exist. Where
# a case is about an absence, anchor it to something that must be PRESENT in the
# same output.
#
# kubectl is stubbed, so the assertions are about what the functions CONCLUDE.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Sourced through a CR-stripped copy. These files are LF in git, but a Windows
# checkout with core.autocrlf writes them CRLF, and bash then reports
# `$'\r': command not found` and stops at the first function definition -- which
# looks exactly like every function being missing. Same reason the repo verifies
# gofmt against an LF-normalised copy rather than the working tree.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh scaledobject.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
# common.sh's header says it plainly: "Requires vars: BLUE, GREEN, YELLOW, RED,
# NC." install.sh defines them; a harness that sources the library directly has
# to as well, or the first log_warning dies on an unbound variable under `set -u`
# -- which inside a command substitution kills only the subshell, so it surfaces
# as one test silently returning nothing rather than as an error.
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/scaledobject.sh"

# yq is not optional. The library cannot index or trim a patch document without
# it, and skipping those cases meant the run stayed green on a machine where the
# feature could not work at all.
for tool in jq yq; do
    command -v "$tool" >/dev/null 2>&1 || {
        printf 'FATAL: %s is required to run these checks.\n' "$tool" >&2
        exit 1
    }
done

FIXTURE=""        # JSON for `kubectl get <kind> <name>`
FIXTURE_RC=0      # or the failure it returns instead
LIST_FIXTURE=''   # JSON for `kubectl get <kind> -n <ns>`
LIST_RC=0
CRD_RC=1          # no LeaderWorkerSet CRD unless a case says otherwise
PATCH_CALLS=0
PATCH_RC=0
PATCH_BODY=""     # the last --patch-file content kubectl was handed
PVC_FIXTURE='{"items":[]}'   # JSON for `kubectl get pvc -n <ns>`
PVC_RC=0
DS_FIXTURE=""     # JSON for `kubectl get daemonset <name>`; empty: none
DS_RC=0           # or the failure the read returns instead
# Per-NAME object fixtures, for the cases about a namespace holding more than
# one workload. A single FIXTURE cannot express "this one is readable and that
# one is not", nor tell two patch documents apart. "__RC1__" means the read
# fails for that name.
declare -A FIXTURE_MAP=()

kubectl() {
    local a f
    case "${1:-}" in
        get)
            case "${2:-}" in
                crd) return "$CRD_RC" ;;
                pvc|persistentvolumeclaims)
                    [ "$PVC_RC" -eq 0 ] || return "$PVC_RC"
                    # `get pvc -n <ns>` is the listing; `get pvc <name> -n <ns>`
                    # is one claim, and the real client answers NotFound (rc 1)
                    # for a name the listing does not carry. The stub used to
                    # print the listing for both, so "does this claim exist?"
                    # was yes for every name -- and a patch that asked it
                    # reported a claim that was never created as reused.
                    case "${3:-}" in ""|-*) : ;; *)
                        # ...and with -o json it is that one object, as the
                        # real client prints it, not the listing.
                        printf '%s' "$PVC_FIXTURE" \
                            | jq -e --arg n "$3" '[.items[]? | select(.metadata.name == $n)] | length > 0' >/dev/null 2>&1 \
                            || return 1
                        printf '%s' "$PVC_FIXTURE" | jq -c --arg n "$3" '[.items[] | select(.metadata.name == $n)] | first'
                        return 0 ;;
                    esac
                    printf '%s' "$PVC_FIXTURE"
                    ;;
                daemonset|daemonsets|ds)
                    # --ignore-not-found semantics: an absent object is empty
                    # output with rc 0; DS_RC is any other failure (Forbidden,
                    # a timeout), which the library must not read as absent.
                    [ "$DS_RC" -eq 0 ] || return "$DS_RC"
                    printf '%s' "$DS_FIXTURE"
                    ;;
                deployments|leaderworkersets)
                    # `get <kind> -n <ns>` is a listing; `get <kind> <name> …` is
                    # one object. The stub has to tell them apart or a case can
                    # pass on a response the real client would never give.
                    if [ "${3:-}" = "-n" ]; then
                        [ "$LIST_RC" -eq 0 ] || return "$LIST_RC"
                        printf '%s' "$LIST_FIXTURE"
                    elif [ "${#FIXTURE_MAP[@]}" -gt 0 ] \
                         && [ -n "${FIXTURE_MAP[${3:-}]+set}" ]; then
                        [ "${FIXTURE_MAP[${3:-}]}" = "__RC1__" ] && return 1
                        printf '%s' "${FIXTURE_MAP[${3:-}]}"
                    else
                        [ "$FIXTURE_RC" -eq 0 ] || return "$FIXTURE_RC"
                        printf '%s' "$FIXTURE"
                    fi
                    ;;
                *) return 0 ;;
            esac
            ;;
        patch)
            PATCH_CALLS=$((PATCH_CALLS + 1))
            for a in "$@"; do
                case "$a" in
                    --patch-file=*) f="${a#--patch-file=}"
                                    PATCH_BODY="$(cat "$f" 2>/dev/null)" ;;
                esac
            done
            return "$PATCH_RC"
            ;;
        *) return 0 ;;
    esac
}

POD_DEPLOY='["spec","template"]'
POD_LWS='["spec","leaderWorkerTemplate","leaderTemplate"]'
FAILED=0
CASE=""
RAN=0
# Counted from the file rather than hand-maintained, so adding a case cannot
# leave the tally stale. It is the number of `CASE=` labels with a name; the
# tally at the bottom checks that every one of them reached an `ok`.
EXPECT_CASES="$(grep -c '^CASE="[^"]' "${BASH_SOURCE[0]}")"

fail() {
    printf 'FAIL  %s\n      %s\n' "$CASE" "$1" >&2
    FAILED=$((FAILED + 1))
}

ok() { RAN=$((RAN + 1)); printf 'ok    %s\n' "$CASE"; }

# gaps_field reads one field the way PRODUCTION reads it: `IFS read`, not `cut`.
# The difference is the whole point of one of the cases below -- `cut` is
# line-oriented and re-emits a delimiterless line whole, so it cannot see the
# truncation that `read` suffers when a field still contains newlines.
gaps_field() {
    local idx="$1" gaps f1 f2 f3 f4 f5 f6 f7
    gaps="$(so_workload_gaps ns deployments w "$POD_DEPLOY")" || return 1
    IFS=$'\037' read -r f1 f2 f3 f4 f5 f6 f7 <<< "$gaps"
    case "$idx" in
        1) printf '%s' "$f1" ;; 2) printf '%s' "$f2" ;; 3) printf '%s' "$f3" ;;
        4) printf '%s' "$f4" ;; 5) printf '%s' "$f5" ;; 6) printf '%s' "$f6" ;;
        7) printf '%s' "$f7" ;;
    esac
}

assert_field() {
    local idx="$1" want="$2" got
    if ! got="$(gaps_field "$idx")"; then
        fail "so_workload_gaps returned non-zero"
        return
    fi
    [ "$got" = "$want" ] || fail "field $idx: want '$want', got '$got'"
}

assert_contains() {
    case "$1" in
        *"$2"*) : ;;
        *) fail "expected to contain: $2" ;;
    esac
}

assert_not_contains() {
    case "$1" in
        *"$2"*) fail "expected NOT to contain: $2" ;;
        *) : ;;
    esac
}

assert_eq() { [ "$1" = "$2" ] || fail "${3:-value}: want '$2', got '$1'"; }

# --- fixtures ---------------------------------------------------------------

f_vllm_plain() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

f_sglang_proxy_has_hook() {
    # The engine is called `server` and the container that HAS a preStop hook is
    # the proxy. A name regex picks the proxy; counting hooks pod-wide reports
    # this pod as already draining.
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[
          {"name":"routing-proxy","image":"quay.io/x/proxy:1",
           "lifecycle":{"preStop":{"exec":{"command":["sleep","10"]}}}},
          {"name":"server","image":"lmsysorg/sglang:latest",
           "args":["python3","-m","sglang.launch_server","--model-path","meta-llama/Llama-3.1-8B"]}]}}}}'
}

f_mount_without_hf_home() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "command":["/bin/sh","-c"],
                       "args":["vllm serve Qwen/Qwen3-0.6B --port 8000"]}]}}}}'
}

# Nothing wrong with it: drains, weights on the claim, and the engine caches
# on it too (a second cache half is the same claim under another directory,
# which is the layout the benchmark harness uses).
f_cached_properly() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"},
                              {"name":"VLLM_CACHE_ROOT","value":"/model-cache/engine-cache/vllm"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# A PVC mounted at one path while the engine downloads to another. This is the
# misconfiguration the weights check exists to catch, and the one a `startswith`
# predicate that compared a value with itself reported as solved.
f_cache_mounted_elsewhere() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/root/.cache/huggingface"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/workspace"}],
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# --download-dir overrides HF_HOME, and HF_HUB_CACHE is read alongside it.
f_download_dir_on_volume() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HUB_CACHE","value":"/root/.cache"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "args":["vllm","serve","Qwen/Qwen3-0.6B","--download-dir","/model-cache/dl"]}]}}}}'
}

f_local_path() {
    FIXTURE='{"spec":{"template":{"spec":{
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "command":["/bin/sh","-c"],
                       "args":["vllm serve /models/qwen3-0.6b"]}]}}}}'
}

f_long_grace() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":300,
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

f_no_engine() {
    FIXTURE='{"spec":{"template":{"spec":{
        "containers":[{"name":"sidecar","image":"quay.io/x/proxy:1","args":["--listen",":80"]}]}}}}'
}

# An LWS-shaped object: the pod template lives under leaderWorkerTemplate. A
# Deployment body read through the LWS path yields {} and lands on the "no engine"
# branch, so a case using one proves nothing about the LWS skip.
f_lws() {
    FIXTURE='{"spec":{"leaderWorkerTemplate":{"leaderTemplate":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[{"name":"vllm","image":"vllm/vllm-openai:glm52",
                       "args":["vllm","serve","zai-org/GLM-4.5"]}]}}}}}'
}

# Copied from a running llm-d deployment on OpenShift (modelservice chart,
# uriProtocol: pvc). Two things about it broke assumptions the invented fixtures
# could not: the command is a multi-line block scalar, and the weights are served
# FROM the volume by local path while --served-model-name advertises a repo id.
f_llmd_real() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}},
                   {"name":"dshm"},{"name":"shared-config"}],
        "containers":[{"name":"vllm","image":"docker.io/vllm/vllm-openai:v0.26.0",
                       "env":[{"name":"HF_HOME","value":"/tmp/huggingface"}],
                       "volumeMounts":[{"name":"dshm","mountPath":"/dev/shm"},
                                       {"name":"shared-config","mountPath":"/shared-config"},
                                       {"name":"model-storage","mountPath":"/model-cache"}],
                       "command":["/bin/bash","-c"],
                       "args":["/bin/true ; vllm serve /model-cache/models/Qwen/Qwen3-0.6B \\\n  --host 0.0.0.0 \\\n  --served-model-name Qwen/Qwen3-0.6B \\\n  --port 8000\n"]}]}}}}'
}

# The same block-scalar shape, genuinely fetching from the Hub.
f_llmd_real_downloads() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"dshm"}],
        "containers":[{"name":"vllm","image":"docker.io/vllm/vllm-openai:v0.26.0",
                       "env":[{"name":"HF_HOME","value":"/tmp/huggingface"}],
                       "volumeMounts":[{"name":"dshm","mountPath":"/dev/shm"}],
                       "command":["/bin/bash","-c"],
                       "args":["/bin/true ; vllm serve Qwen/Qwen3-0.6B \\\n  --host 0.0.0.0 \\\n  --served-model-name my-alias \\\n  --port 8000\n"]}]}}}}'
}

f_unreadable() { FIXTURE=""; FIXTURE_RC=1; }

reset() {
    FIXTURE=""; FIXTURE_RC=0; LIST_FIXTURE=''; LIST_RC=0; CRD_RC=1
    PATCH_CALLS=0; PATCH_RC=0; PATCH_BODY=""
    PVC_FIXTURE='{"items":[]}'; PVC_RC=0; DS_FIXTURE=""; DS_RC=0
    FIXTURE_MAP=()
}

# --- canary -----------------------------------------------------------------
#
# Proves the harness can report a failure at all. Without it, a bookkeeping
# mistake that suppressed every `fail` would look exactly like a clean run.
CASE="the harness itself reports failures (canary)"
before=$FAILED
# stderr is dropped for this one call: the assertion is MEANT to fail, and
# printing its FAIL line would train a reader to ignore FAIL lines.
assert_eq "left" "right" "canary" 2>/dev/null
if [ "$FAILED" -eq $((before + 1)) ]; then
    FAILED="$before"      # expected; un-count it
    ok
else
    FAILED=$((before + 1))
    printf 'FAIL  %s\n      assert_eq did not register a failure\n' "$CASE" >&2
fi

# --- detection --------------------------------------------------------------

CASE="plain vLLM: engine, no hook, default grace, nothing cached"
reset; f_vllm_plain
before=$FAILED
assert_field 1 "vllm"
assert_field 2 "0"
assert_field 3 "30"
assert_field 5 "0"
[ "$FAILED" -eq "$before" ] && ok

CASE="SGLang: engine is 'server' by image, not the proxy that has the hook"
reset; f_sglang_proxy_has_hook
before=$FAILED
assert_field 1 "server"
assert_field 2 "0"      # the ENGINE's hook count, which is what decides
[ "$FAILED" -eq "$before" ] && ok

CASE="a PVC mounted with no HF_HOME does not make weights persist"
reset; f_mount_without_hf_home
before=$FAILED
assert_field 4 "1"      # a PVC is present...
assert_field 5 "0"      # ...and the engine still downloads outside it
[ "$FAILED" -eq "$before" ] && ok

CASE="HF_HOME under a mounted volume counts as cached"
reset; f_cached_properly
before=$FAILED
assert_field 5 "1"
assert_field 2 "1"
[ "$FAILED" -eq "$before" ] && ok

CASE="a PVC mounted somewhere ELSE does not count as cached"
reset; f_cache_mounted_elsewhere
before=$FAILED
# The predicate must compare the download dir against each MOUNT. Comparing it
# with itself -- `$dldir | startswith(.)` -- is true for every mount, and made
# this exact misconfiguration report as solved.
assert_field 5 "0"
assert_field 4 "1"
[ "$FAILED" -eq "$before" ] && ok

CASE="--download-dir overrides HF_HUB_CACHE and is matched against the mounts"
reset; f_download_dir_on_volume
before=$FAILED
assert_field 5 "1"      # /model-cache/dl is under the /model-cache mount
[ "$FAILED" -eq "$before" ] && ok

CASE="unreadable object returns non-zero and prints nothing"
reset; f_unreadable
before=$FAILED
out="$(so_workload_gaps ns deployments w "$POD_DEPLOY")" && fail "expected non-zero"
[ -z "$out" ] || fail "expected no output, got: $out"
# Positive anchor: the SAME call on a readable object must produce a record, so
# a deleted function cannot satisfy this case.
reset; f_vllm_plain
assert_contains "$(so_workload_gaps ns deployments w "$POD_DEPLOY")" "vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="a multi-line block scalar keeps its later flags (read stops at newline 1)"
reset; f_llmd_real
before=$FAILED
argv="$(gaps_field 6)" || fail "so_workload_gaps returned non-zero"
assert_contains "$argv" "--served-model-name"
assert_contains "$argv" "--port"
[ "$FAILED" -eq "$before" ] && ok

# --- the model source -------------------------------------------------------

CASE="so_model_source reads the source, not the advertised name"
before=$FAILED
assert_eq "$(so_model_source 'vllm serve /model-cache/models/Qwen --served-model-name Qwen/Qwen3-0.6B')" \
          "/model-cache/models/Qwen" "served-model-name must not win"
assert_eq "$(so_model_source 'vllm serve Qwen/Qwen3-0.6B')" "Qwen/Qwen3-0.6B" "positional"
assert_eq "$(so_model_source 'vllm --model=Qwen/Qwen3-0.6B')" "Qwen/Qwen3-0.6B" "--model="
# SGLang. Omitting --model-path gave every SGLang engine a silent pass.
assert_eq "$(so_model_source 'python3 -m sglang.launch_server --model-path meta-llama/Llama-3.1-8B')" \
          "meta-llama/Llama-3.1-8B" "--model-path"
[ "$FAILED" -eq "$before" ] && ok

CASE="so_model_source does not glob against the working directory"
before=$FAILED
# `for tok in $args` splits AND globs. Run from a directory with files in it and
# a bare glob inside someone else's entrypoint becomes the answer.
tmpd="$(mktemp -d)"; : > "$tmpd/decoy.log"
assert_eq "$(cd "$tmpd" && so_model_source 'sh -c rm -f *.log ; vllm serve Qwen/Qwen3-0.6B')" \
          "Qwen/Qwen3-0.6B" "glob leaked into the parse"
rm -rf "$tmpd"
[ "$FAILED" -eq "$before" ] && ok

# --- notes ------------------------------------------------------------------

CASE="drain note fires for the proxy-has-a-hook pod"
reset; f_sglang_proxy_has_hook
before=$FAILED
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "server declares no preStop hook"
[ "$FAILED" -eq "$before" ] && ok

CASE="drain note is silent when the engine has a hook"
reset; f_cached_properly
before=$FAILED
note="$(so_drain_note ns deployments w "$POD_DEPLOY")"
[ -z "$note" ] || fail "expected silence, got: $note"
# Anchor: the same function must still speak for a pod that needs it.
reset; f_vllm_plain
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "no preStop hook"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights note fires for the sh -c form (used to match on /bin/sh)"
reset; f_mount_without_hf_home
before=$FAILED
assert_contains "$(so_weights_note ns deployments w "$POD_DEPLOY")" "Weights do not persist"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights note fires for an SGLang engine launched by --model-path"
reset; f_sglang_proxy_has_hook
before=$FAILED
assert_contains "$(so_weights_note ns deployments w "$POD_DEPLOY")" "meta-llama/Llama-3.1-8B"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights note is silent for a local model path"
reset; f_local_path
before=$FAILED
note="$(so_weights_note ns deployments w "$POD_DEPLOY")"
[ -z "$note" ] || fail "expected silence, got: $note"
reset; f_mount_without_hf_home
assert_contains "$(so_weights_note ns deployments w "$POD_DEPLOY")" "Weights do not persist"
[ "$FAILED" -eq "$before" ] && ok

CASE="an unreadable object is reported as unknown, never as healthy"
reset; f_unreadable
before=$FAILED
note="$(so_drain_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "unknown"
assert_not_contains "$note" "no preStop hook"
[ "$FAILED" -eq "$before" ] && ok

# --- emitted patch ----------------------------------------------------------

CASE="patch names the engine container, both halves, HF_HOME with the mount"
reset; f_mount_without_hf_home
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "- name: vllm"
assert_contains "$doc" "preStop"
assert_contains "$doc" "HF_HOME"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="patch never lowers an existing longer grace period"
reset; f_long_grace
before=$FAILED
assert_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" \
    "terminationGracePeriodSeconds: 300"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights served BY LOCAL PATH need no cache, whatever name is advertised"
reset; f_llmd_real
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "preStop"          # the drain half is still owed
assert_not_contains "$doc" "claimName: model-pvc"    # ...but not a second cache
assert_not_contains "$doc" "HF_HOME"
assert_not_contains "$doc" "#   cache:"
[ "$FAILED" -eq "$before" ] && ok

CASE="a re-downloading workload is warned about ON THE CONSOLE, not just in the file"
reset; f_llmd_real_downloads
before=$FAILED
# The emitted file always carried this; the console said only "1 workload(s)
# need something". A cost that only shows up the moment a replica is added, and
# reads as the autoscaler being slow, has to be said where someone will see it.
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_contains "$warn" "RE-DOWNLOADS ITS WEIGHTS"
assert_contains "$warn" "Qwen/Qwen3-0.6B"
[ "$FAILED" -eq "$before" ] && ok

CASE="a workload whose weights DO persist is not warned about"
reset; f_cached_properly
before=$FAILED
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_not_contains "$warn" "RE-DOWNLOADS"
# Anchor, so a silenced warning cannot pass this pair by never firing at all.
reset; f_llmd_real_downloads
assert_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)" "RE-DOWNLOADS"
[ "$FAILED" -eq "$before" ] && ok

CASE="the emitted file carries a PVC to fill in, and still parses"
reset; f_llmd_real_downloads
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "kind: PersistentVolumeClaim"
assert_contains "$doc" "ReadWriteMany"
# Commented, not a live document: a claim with a placeholder size would be
# rejected, and a claim we guessed could break scale-up outright.
d2="$(mktemp)"; printf '%s\n' "$doc" > "$d2"
yq eval '.' "$d2" >/dev/null 2>&1 || fail "the emitted file no longer parses as YAML"
assert_eq "$(yq eval-all '[.] | length' "$d2" 2>/dev/null | tail -1)" "1" "documents in the file"
rm -f "$d2"
[ "$FAILED" -eq "$before" ] && ok

CASE="the same shape genuinely downloading from the Hub does get a cache"
reset; f_llmd_real_downloads
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName"
assert_contains "$doc" "HF_HOME"
[ "$FAILED" -eq "$before" ] && ok

CASE="patch emits no cache half for a local model path"
reset; f_local_path
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "preStop"          # anchor: a document was produced
assert_not_contains "$doc" "claimName: model-pvc"
assert_not_contains "$doc" "HF_HOME"
assert_not_contains "$doc" "#   cache:"
[ "$FAILED" -eq "$before" ] && ok

CASE="nothing is emitted for a workload that is already fine"
reset; f_cached_properly
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
[ -z "$doc" ] || fail "expected no document, got: $doc"
reset; f_vllm_plain
assert_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" "apiVersion"
[ "$FAILED" -eq "$before" ] && ok

CASE="no identifiable engine is skipped (rc 2), not patched at container 0"
reset; f_no_engine
before=$FAILED
doc_rc=0
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" || doc_rc=$?
assert_eq "$doc_rc" "2" "return code"
[ -z "$doc" ] || fail "expected no document, got: $doc"
[ "$FAILED" -eq "$before" ] && ok

CASE="LeaderWorkerSet is skipped (rc 2) on an LWS-shaped object"
reset; f_lws
before=$FAILED
# The detector must be able to READ this object -- otherwise the case would pass
# through the "no engine" branch and prove nothing about the LWS skip.
assert_contains "$(so_workload_gaps ns leaderworkersets w "$POD_LWS")" "vllm"
doc_rc=0
doc="$(so_workload_patch_doc ns leaderworkersets w "$POD_LWS" 2>/dev/null)" || doc_rc=$?
assert_eq "$doc_rc" "2" "return code"
[ -z "$doc" ] || fail "expected no document, got: $doc"
[ "$FAILED" -eq "$before" ] && ok

CASE="an unreadable object emits nothing and returns rc 3"
reset; f_unreadable
before=$FAILED
doc_rc=0
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" || doc_rc=$?
assert_eq "$doc_rc" "3" "return code"
[ -z "$doc" ] || fail "expected no document, got: $doc"
[ "$FAILED" -eq "$before" ] && ok

# --- the patch index --------------------------------------------------------

CASE="the index lists every document as two readable fields"
before=$FAILED
multi="$(mktemp)"
cat > "$multi" <<'YAML'
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: a
  namespace: ns1
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: b
  namespace: ns2
YAML
# Counting lines is not enough. yq does not decode a  escape the way jq
# does, and emitted one unsplittable field per line -- two lines, correct count,
# and `read -r ns name` left the name EMPTY, so the apply loop ran zero times.
pairs=""
while read -r i_ns i_name; do
    [ -n "$i_name" ] || fail "index line did not split into two fields"
    pairs="$pairs[$i_ns/$i_name]"
done < <(so_workload_patch_index "$multi")
assert_eq "$pairs" "[ns1/a][ns2/b]" "index pairs"
rm -f "$multi"
[ "$FAILED" -eq "$before" ] && ok

# --- dropping the weights half ----------------------------------------------

CASE="drop_cache keeps the drain half and removes the weights half"
before=$FAILED
d="$(mktemp)"
cat > "$d" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: w, namespace: ns1}
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 120
      containers:
      - name: vllm
        lifecycle:
          preStop:
            exec:
              command: ["/bin/sh", "-c", "sleep 45"]
        env:
        - name: HF_HOME
          value: /model-cache/huggingface
        volumeMounts:
        - name: model-storage
          mountPath: /model-cache
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
YAML
so_workload_patch_drop_cache "$d" || fail "expected the drain half to survive"
left="$(cat "$d")"
assert_contains "$left" "terminationGracePeriodSeconds: 120"
assert_contains "$left" "preStop"
assert_not_contains "$left" "claimName"
assert_not_contains "$left" "HF_HOME"
assert_not_contains "$left" "volumeMounts"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

CASE="a cache-only document is skipped, not applied as an empty patch"
before=$FAILED
d="$(mktemp)"
cat > "$d" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: w, namespace: ns1}
spec:
  template:
    spec:
      containers:
      - name: vllm
        env:
        - name: HF_HOME
          value: /model-cache/huggingface
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
YAML
drop_rc=0
so_workload_patch_drop_cache "$d" || drop_rc=$?
# EXACTLY 1, not merely non-zero. A function that does not exist returns 127,
# and this case passed against a zero-byte library on `&& fail` alone.
assert_eq "$drop_rc" "1" "return code"
assert_contains "$(cat "$d")" "HF_HOME"   # anchor: the file was left intact
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

# --- which claim the patch names --------------------------------------------
#
# A namespace that already has a shared cache under another name would otherwise
# be told to use `model-pvc`, and the obvious response is to provision a SECOND
# terabyte for weights that are already on the cluster.

pvc_list() {
    # $1..$n: "<name>:<accessMode>[:<phase>]"
    local items="" spec name mode phase
    for spec in "$@"; do
        name="${spec%%:*}"; mode="${spec#*:}"; phase="Bound"
        case "$mode" in *:*) phase="${mode#*:}"; mode="${mode%%:*}" ;; esac
        items="${items:+$items,}{\"metadata\":{\"name\":\"$name\"},\"spec\":{\"accessModes\":[\"$mode\"]},\"status\":{\"phase\":\"$phase\"}}"
    done
    printf '{"items":[%s]}' "$items"
}

CASE="an existing shared claim is reused instead of proposing a second one"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list llm-d-model-cache:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: llm-d-model-cache"
assert_contains "$doc" "already exists in ns"
assert_not_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="the only RWX claim is not assumed to be a model cache"
reset; f_llmd_real_downloads
before=$FAILED
# A benchmark namespace has exactly one RWX claim and it is the RESULTS volume.
# Mounting that at /model-cache with HF_HOME inside it is wrong, and with the
# weights opt-in it is wrong on a live workload. One candidate is not the same
# as one candidate that is a model cache.
PVC_FIXTURE="$(pvc_list llm-d-benchmark-results:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_not_contains "$doc" "llm-d-benchmark-results"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="a ReadOnlyMany claim is not proposed for a path we write to"
reset; f_llmd_real_downloads
before=$FAILED
# The emitted patch points HF_HOME INTO the mount. A ROX volume mounts
# read-only, so reusing one turns a re-download into a crash on first write.
PVC_FIXTURE="$(pvc_list shared-model-datasets:ReadOnlyMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_not_contains "$doc" "shared-model-datasets"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="a discovered claim is never followed by instructions to create one"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list llm-d-model-cache:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: llm-d-model-cache"
# The emitted file used to mount the discovered claim and then tell the reader
# to create `model-pvc` -- the second claim this feature exists to prevent, and
# not even the one the patch mounts.
assert_not_contains "$doc" "kind: PersistentVolumeClaim"
assert_not_contains "$doc" "make model-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="a ReadWriteOnce claim is not proposed as a shared cache"
reset; f_llmd_real_downloads
before=$FAILED
# RWO binds to one node, so reusing it would produce replicas that cannot
# schedule -- the failure this whole area exists to avoid.
PVC_FIXTURE="$(pvc_list scratch:ReadWriteOnce)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: model-pvc"
assert_contains "$doc" "does NOT exist yet"
[ "$FAILED" -eq "$before" ] && ok

CASE="two candidate claims are ambiguous, so neither is guessed"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list alpha:ReadWriteMany beta:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="among several, exactly one named for the job is taken"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list results:ReadWriteMany model-cache:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: model-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="an unbound claim is not reused"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list somewhere:ReadWriteMany:Pending)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="an explicit WVA_MODEL_PVC_NAME beats anything discovered"
reset; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list llm-d-model-cache:ReadWriteMany)"
doc="$(WVA_MODEL_PVC_NAME=mine so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: mine"
[ "$FAILED" -eq "$before" ] && ok

CASE="a forbidden PVC listing falls back rather than claiming none exist"
reset; f_llmd_real_downloads
before=$FAILED
PVC_RC=1
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: model-pvc"
assert_contains "$doc" "preStop"    # anchor: a document was still produced
[ "$FAILED" -eq "$before" ] && ok

# --- plan notes -------------------------------------------------------------
#
# A workload usually has more than one thing wrong with it, and the notes used to
# be joined into a single run-on sentence -- so the second problem, often the
# expensive one, sat mid-paragraph where nobody reads.

CASE="one plan note renders as one line"
before=$FAILED
# The case that caught the real bug: `printf '%s' | while read` never runs its
# body for a final line with no terminator, so a SINGLE note produced no output
# at all. Both counts have to be asserted; only the pair is meaningful.
n="$(so_plan_entry yes ns Deployment w model 1 10 10 "" "" "" "only note" | grep -c '# note:')"
assert_eq "$n" "1" "note lines"
[ "$FAILED" -eq "$before" ] && ok

CASE="two plan notes render as two lines, not one paragraph"
before=$FAILED
two="first note"$'\036'"second note"
out="$(so_plan_entry yes ns Deployment w model 1 10 10 "" "" "" "$two")"
assert_eq "$(printf '%s' "$out" | grep -c '# note:')" "2" "note lines"
assert_contains "$out" "# note: first note"
assert_contains "$out" "# note: second note"
[ "$FAILED" -eq "$before" ] && ok

CASE="an entry with no note renders none"
before=$FAILED
assert_eq "$(so_plan_entry yes ns Deployment w model 1 10 10 "" "" "" "" | grep -c '# note:')" "0" "note lines"
# Anchor: the entry itself must still be produced.
assert_contains "$(so_plan_entry yes ns Deployment w model 1 10 10 "" "" "" "")" "namespace: ns"
[ "$FAILED" -eq "$before" ] && ok

# --- the verify-* health checks ----------------------------------------------
#
# These report on a live cluster and are the kind of code that passes review by
# inspection and fails in the field: every branch is a printf and a counter, and
# the interesting ones are the branches nobody has on their cluster.

# The stub is SAVED and restored below rather than retyped: a hand-written copy
# silently dropped the per-name FIXTURE_MAP branch and broke a later case.
orig_kubectl="$(declare -f kubectl)"

CASE="verify-fma: a namespace that cannot be listed is not an empty one"
reset
before=$FAILED
# Piping a failed `kubectl get` into `wc -l` yields 0, which read as "no launcher
# pods here" -- so a Forbidden listing was reported as nothing to check and the
# run still exited 0.
kubectl() {
    case "${1:-}${2:-}" in
        getpods) return 1 ;;          # the listing is refused
        *) return 0 ;;
    esac
}
out=""; rc=0
out="$(WVA_DEFAULT_SO_NS=ns1 so_verify_fma 2>&1)" || rc=$?
assert_contains "$out" "UNREADABLE"
assert_contains "$out" "NOT a report that they are scraped"
assert_eq "$rc" "1" "return code"
assert_not_contains "$out" "0          -"
[ "$FAILED" -eq "$before" ] && ok

CASE="verify-fma: a genuinely empty namespace is reported as such, and passes"
reset
before=$FAILED
kubectl() {
    case "${1:-}${2:-}" in
        getpods) printf '' ;;         # listing succeeds, no launchers
        *) return 0 ;;
    esac
}
out=""; rc=0
out="$(WVA_DEFAULT_SO_NS=ns1 so_verify_fma 2>&1)" || rc=$?
assert_eq "$rc" "0" "return code"
assert_contains "$out" "with no launcher pods"
assert_not_contains "$out" "UNREADABLE"
[ "$FAILED" -eq "$before" ] && ok

# Restore the fixture stub for anything after this.
unset -f kubectl
eval "$orig_kubectl"

# --- the driver -------------------------------------------------------------
#
# wva_workload_patch's return code is load-bearing: benchmark-standup runs it as
# `... || echo WARNING`, and an earlier version returned 0 on every path, so the
# warning could not fire for the failure it named.

DRIVER_RC=0; DRIVER_OUT=""; DRIVER_LOG=""

serving_list() {
    printf '%s' '{"items":[{"metadata":{"name":"w","labels":{"llm-d.ai/role":"decode"}}}]}'
}

run_driver() {
    local dir; dir="$(mktemp -d)"
    DRIVER_OUT="$dir/patch.yaml"; DRIVER_LOG="$dir/log"
    DRIVER_RC=0
    WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_FILE="$DRIVER_OUT" \
        wva_workload_patch >"$DRIVER_LOG" 2>&1 || DRIVER_RC=$?
}

CASE="a namespace that cannot be listed returns 1, not 'all clean'"
reset; LIST_RC=1
before=$FAILED
run_driver
assert_eq "$DRIVER_RC" "1" "return code"
[ -f "$DRIVER_OUT" ] && fail "wrote a patch file for a namespace it could not read"
assert_contains "$(cat "$DRIVER_LOG")" "not being reported as clean"
[ "$FAILED" -eq "$before" ] && ok

CASE="an object that cannot be READ is not reported as healthy"
reset; LIST_FIXTURE="$(serving_list)"; FIXTURE_RC=1
before=$FAILED
run_driver
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$DRIVER_LOG")" "NOT a report that they are healthy"
assert_not_contains "$(cat "$DRIVER_LOG")" "already drain on scale-down"
[ "$FAILED" -eq "$before" ] && ok

CASE="an empty namespace says nothing was checked, not that all is well"
reset; LIST_FIXTURE='{"items":[]}'
before=$FAILED
run_driver
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$DRIVER_LOG")" "No model servers found in scope"
[ "$FAILED" -eq "$before" ] && ok

CASE="a converged workload reports the all-clear and clears its own stale file"
reset; LIST_FIXTURE="$(serving_list)"; f_cached_properly
before=$FAILED
stale="$(mktemp)"; so_workload_patch_header > "$stale"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_FILE="$stale" wva_workload_patch >/dev/null 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
[ -f "$stale" ] && { fail "a file this tool wrote should have been removed"; rm -f "$stale"; }
[ "$FAILED" -eq "$before" ] && ok

CASE="a file this tool did NOT write is never deleted"
reset; LIST_FIXTURE="$(serving_list)"; f_cached_properly
before=$FAILED
mine="$(mktemp)"; printf 'hand written, do not delete\n' > "$mine"
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_FILE="$mine" \
    wva_workload_patch >"$WORK/nd.log" 2>&1 || true
# Anchor: the tool must have REACHED the decision and said so. Without it a
# library that does not run at all also leaves the file untouched -- this case
# printed `ok` against a zero-byte library.
assert_contains "$(cat "$WORK/nd.log")" "was not written by this tool"
assert_contains "$(cat "$mine" 2>/dev/null || echo GONE)" "hand written"
rm -f "$mine"
[ "$FAILED" -eq "$before" ] && ok

CASE="one workload with gaps is written, with the do-not-apply header"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
run_driver
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "name: w"
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "wva-workload-patch: generated"
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "Do NOT \`kubectl apply -f\`"
[ "$FAILED" -eq "$before" ] && ok

# --- apply mode -------------------------------------------------------------

CASE="apply patches the drain half and never sends a volume"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "preStop"
# A strategic merge cannot replace a volume of the same name; sending one gets
# the WHOLE object rejected, drain half included.
assert_not_contains "$PATCH_BODY" "persistentVolumeClaim"
assert_not_contains "$PATCH_BODY" "volumeMounts"
# ...while the emitted file still carries it, for whoever owns the storage.
assert_contains "$(cat "$dir/p.yaml")" "claimName: model-pvc"
assert_contains "$(cat "$dir/log")" "REPLACES PODS"
[ "$FAILED" -eq "$before" ] && ok

CASE="a rejected patch is counted as a failure and makes the run non-zero"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain; PATCH_RC=1
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "were NOT patched"
assert_not_contains "$(cat "$dir/log")" "Patched 1 of 1"
[ "$FAILED" -eq "$before" ] && ok

CASE="a workload needing only the weights volume is deferred, not failed"
reset; LIST_FIXTURE="$(serving_list)"; f_cache_mounted_elsewhere
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
# It already has a preStop hook? No -- it has none, so it gets the drain half.
# What matters is that the emitted document keeps the weights half and the patch
# sent to the API server does not.
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/p.yaml")" "claimName: model-pvc"
assert_not_contains "$PATCH_BODY" "claimName"
[ "$FAILED" -eq "$before" ] && ok

CASE="a missing tool is reported, not silently treated as nothing-to-do"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
# `command` is a builtin, but a function of the same name shadows it for the
# unqualified call the library makes -- which is the only way to simulate an
# absent binary from inside a harness that needs those binaries itself.
command() {
    if [ "${1:-}" = "-v" ] && [ "${2:-}" = "yq" ]; then return 1; fi
    builtin command "$@"
}
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
unset -f command
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "yq is not installed"
# The silence this replaces: yq missing meant an empty index, zero patch calls,
# and a [SUCCESS] line.
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
[ "$FAILED" -eq "$before" ] && ok

# --- applying the weights half ----------------------------------------------
#
# Its own opt-in, separate from APPLY: adding a hook cannot conflict with
# anything, while mounting storage can be refused by the API server outright and
# changes where an engine reads its weights from.

apply_weights_run() {
    local dir; dir="$(mktemp -d)"
    DRIVER_RC=0; DRIVER_LOG="$dir/log"; DRIVER_OUT="$dir/p.yaml"
    WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true \
        WVA_WORKLOAD_PATCH_APPLY_WEIGHTS=true WVA_WORKLOAD_PATCH_FILE="$DRIVER_OUT" \
        wva_workload_patch >"$DRIVER_LOG" 2>&1 || DRIVER_RC=$?
}

CASE="with the opt-in and no collision, the weights volume IS applied"
reset; LIST_FIXTURE="$(serving_list)"; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list model-pvc:ReadWriteMany)"
apply_weights_run
assert_contains "$PATCH_BODY" "claimName"
assert_contains "$PATCH_BODY" "preStop"
[ "$FAILED" -eq "$before" ] && ok

CASE="a volume of the same name blocks the weights half, NOT the drain half"
reset; LIST_FIXTURE="$(serving_list)"
before=$FAILED
# The 422 that made this unconditional once: `volumes` merges with retainKeys,
# which `kubectl patch --type=strategic` does not generate, so a same-named
# volume gets the WHOLE object rejected -- drain hook included.
FIXTURE='{"spec":{"template":{"spec":{
    "terminationGracePeriodSeconds":30,
    "volumes":[{"name":"model-storage","emptyDir":{}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                   "env":[{"name":"HF_HOME","value":"/root/.cache/huggingface"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
PVC_FIXTURE="$(pvc_list model-pvc:ReadWriteMany)"
apply_weights_run
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "claimName"
assert_contains "$(cat "$DRIVER_LOG")" "already exists"
# ...and the emitted file still carries it, for whoever owns the chart.
assert_contains "$(cat "$DRIVER_OUT")" "claimName"
[ "$FAILED" -eq "$before" ] && ok

CASE="a missing claim blocks the weights half rather than stranding pods Pending"
reset; LIST_FIXTURE="$(serving_list)"; f_llmd_real_downloads
before=$FAILED
PVC_RC=1     # the claim cannot be read
apply_weights_run
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "claimName"
assert_contains "$(cat "$DRIVER_LOG")" "does not exist"
[ "$FAILED" -eq "$before" ] && ok

CASE="without the opt-in the weights half is never applied, claim or no claim"
reset; LIST_FIXTURE="$(serving_list)"; f_llmd_real_downloads
before=$FAILED
PVC_FIXTURE="$(pvc_list model-pvc:ReadWriteMany)"
dir="$(mktemp -d)"
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || true
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "claimName"
[ "$FAILED" -eq "$before" ] && ok

CASE="candidate StorageClasses are ranked by observed RWX use"
before=$FAILED
PVC_FIXTURE='{"items":[
  {"spec":{"accessModes":["ReadWriteMany"],"storageClassName":"shared-fs"},"status":{"phase":"Bound"}},
  {"spec":{"accessModes":["ReadWriteMany"],"storageClassName":"shared-fs"},"status":{"phase":"Bound"}},
  {"spec":{"accessModes":["ReadWriteMany"],"storageClassName":"other-fs"},"status":{"phase":"Bound"}},
  {"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"block"},"status":{"phase":"Bound"}},
  {"spec":{"accessModes":["ReadWriteMany"],"storageClassName":"unbound-fs"},"status":{"phase":"Pending"}}]}'
out="$(so_model_cache_classes)"
assert_contains "$out" "shared-fs (2 bound RWX claim(s))"
assert_contains "$out" "other-fs (1 bound RWX claim(s))"
# A class only ever seen backing RWO is not evidence that it can do RWX.
assert_not_contains "$out" "block"
assert_not_contains "$out" "unbound-fs"
assert_eq "$(printf '%s' "$out" | head -1)" "shared-fs (2 bound RWX claim(s))" "most-used first"
[ "$FAILED" -eq "$before" ] && ok

CASE="an unwritable destination keeps the patch instead of deleting it"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_FILE=/nonexistent-dir/p.yaml \
    wva_workload_patch >"$WORK/uw.log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$WORK/uw.log")" "kept at"
kept="$(sed -n 's/.*kept at \([^ ]*\).*/\1/p' "$WORK/uw.log" | tail -1)"
[ -n "$kept" ] && [ -s "$kept" ] || fail "the patch it says it kept is not there"
[ -n "$kept" ] && rm -f "$kept"
[ "$FAILED" -eq "$before" ] && ok


# --- ADDED BY MUTATION TESTING ------------------------------------------------

# The engine identified by IMAGE alone -- the command is a wrapper script.
f_engine_by_image_only() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[
          {"name":"routing-proxy","image":"quay.io/x/proxy:1","args":["--listen",":80"]},
          {"name":"server","image":"quay.io/x/vllm-openai:v0.11",
           "command":["/opt/entrypoint.sh"]}]}}}}'
}

# The engine identified by COMMAND alone -- the image name says nothing.
f_engine_by_command_only() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[
          {"name":"routing-proxy","image":"quay.io/x/proxy:1","args":["--listen",":80"]},
          {"name":"engine","image":"registry.example.com/ai/runtime:1.2",
           "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# The SIDECAR mounts the cache; the engine does not.
f_sidecar_mounts_cache() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[
          {"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
           "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"}],
           "args":["vllm","serve","Qwen/Qwen3-0.6B"]},
          {"name":"sidecar","image":"quay.io/x/proxy:1",
           "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}]}]}}}}'
}

# An engine that already drains but re-downloads -- the DEFERRAL shape.
f_hooked_but_downloads() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

CASE="the engine is found by IMAGE when the command is a wrapper script"
reset; f_engine_by_image_only
before=$FAILED
assert_field 1 "server"
[ "$FAILED" -eq "$before" ] && ok

CASE="the engine is found by COMMAND when the image name says nothing"
reset; f_engine_by_command_only
before=$FAILED
assert_field 1 "engine"
[ "$FAILED" -eq "$before" ] && ok

CASE="a cache the SIDECAR mounts is not a cache the engine can use"
reset; f_sidecar_mounts_cache
before=$FAILED
assert_field 1 "vllm"
assert_field 5 "0"
[ "$FAILED" -eq "$before" ] && ok

CASE="the weights note is silent when the download dir IS on a mount"
reset; f_cached_properly
before=$FAILED
note="$(so_weights_note ns deployments w "$POD_DEPLOY")"
[ -z "$note" ] || fail "expected silence, got: $note"
reset; f_cache_mounted_elsewhere
assert_contains "$(so_weights_note ns deployments w "$POD_DEPLOY")" "Weights do not persist"
[ "$FAILED" -eq "$before" ] && ok

CASE="hasPVC counts PVC-backed volumes, not every volume"
reset; f_llmd_real
before=$FAILED
assert_field 1 "vllm"
assert_field 4 "1"
[ "$FAILED" -eq "$before" ] && ok

CASE="a bare glob as the model argument does not become the answer"
before=$FAILED
tmpd="$(mktemp -d)"; : > "$tmpd/decoy.log"
assert_eq "$(cd "$tmpd" && so_model_source 'vllm serve * --port 8000')" "*" "glob after serve"
assert_eq "$(cd "$tmpd" && so_model_source 'vllm --model *')" "*" "glob as --model"
rm -rf "$tmpd"
[ "$FAILED" -eq "$before" ] && ok

CASE="the emitted preStop is a shell sleep of the configured length"
reset; f_vllm_plain
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" 'command: ["/bin/sh", "-c", "sleep 45"]'
assert_contains "$(WVA_DRAIN_SLEEP_SECONDS=90 so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" \
    '"sleep 90"'
[ "$FAILED" -eq "$before" ] && ok

CASE="a workload that already drains but re-downloads is deferred, not patched"
reset; LIST_FIXTURE="$(serving_list)"; f_hooked_but_downloads
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$dir/log")" "need only a volume"
assert_not_contains "$(cat "$dir/log")" "were NOT patched"
assert_contains "$(cat "$dir/p.yaml")" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="each workload is patched with ITS OWN document, not the whole file"
reset
LIST_FIXTURE='{"items":[{"metadata":{"name":"w1","labels":{"llm-d.ai/role":"decode"}}},
                        {"metadata":{"name":"w2","labels":{"llm-d.ai/role":"decode"}}}]}'
before=$FAILED
f_vllm_plain
FIXTURE_MAP=([w1]="$FIXTURE" [w2]="$FIXTURE")
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "2" "kubectl patch calls"
assert_contains "$PATCH_BODY" "name: w2"
assert_not_contains "$PATCH_BODY" "name: w1"
[ "$FAILED" -eq "$before" ] && ok

CASE="a file IS written for the readable half, and the run is still non-zero"
reset
LIST_FIXTURE='{"items":[{"metadata":{"name":"good","labels":{"llm-d.ai/role":"decode"}}},
                        {"metadata":{"name":"bad","labels":{"llm-d.ai/role":"decode"}}}]}'
before=$FAILED
f_vllm_plain
FIXTURE_MAP=([good]="$FIXTURE" [bad]="__RC1__")
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_contains "$(cat "$dir/p.yaml" 2>/dev/null)" "name: good"
assert_eq "$DRIVER_RC" "1" "return code"
[ "$FAILED" -eq "$before" ] && ok

CASE="a listing that cannot be PARSED is not an empty namespace"
reset; LIST_FIXTURE='{"items":['
before=$FAILED
run_driver
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$DRIVER_LOG")" "not being reported as clean"
assert_not_contains "$(cat "$DRIVER_LOG")" "No model servers found in scope"
[ "$FAILED" -eq "$before" ] && ok

CASE="an index that reads back empty is a failure, not nothing-to-do"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
so_index_orig="$(declare -f so_workload_patch_index)"
so_workload_patch_index() { :; }
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
unset -f so_workload_patch_index
[ -n "$so_index_orig" ] && eval "$so_index_orig"
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "NOTHING was applied"
assert_contains "$(cat "$dir/log")" "were NOT patched"
[ "$FAILED" -eq "$before" ] && ok

CASE="LeaderWorkerSets are looked at only where the CRD exists"
reset; LIST_FIXTURE="$(serving_list)"; f_lws; CRD_RC=1
before=$FAILED
run_driver
assert_not_contains "$(cat "$DRIVER_LOG")" "Skipping LeaderWorkerSet"
reset; LIST_FIXTURE="$(serving_list)"; f_lws; CRD_RC=0
run_driver
assert_contains "$(cat "$DRIVER_LOG")" "Skipping LeaderWorkerSet"
[ "$FAILED" -eq "$before" ] && ok

# --- the engine-cache half --------------------------------------------------
#
# The same question as the weights, one layer down: vLLM keeps its compile
# caches under VLLM_CACHE_ROOT, and a replica that does not find them warm
# compiles from nothing -- measured, 14 s of torch.compile against 3 s. The
# detector answers where the caches LAND, as the weights one does: unset is
# /tmp, an emptyDir dies with the pod, and only a claim or a hostPath counts.

f_engine_cache_on_claim() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}},
                   {"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"},
                              {"name":"VLLM_CACHE_ROOT","value":"/engine-cache/vllm"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"},
                                       {"name":"engine-cache","mountPath":"/engine-cache"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# The cache variable set, and pointing at an emptyDir: every llm-d pod mounts
# one at /dev/shm, and a cache there is a cache for one replica, which is none.
f_engine_cache_on_emptydir() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"dshm","emptyDir":{"medium":"Memory"}},
                   {"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"},
                              {"name":"VLLM_CACHE_ROOT","value":"/dev/shm/vllm-cache"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"},
                                       {"name":"dshm","mountPath":"/dev/shm"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# A hostPath counts: it is the node's disk, and it outlives the pod.
f_engine_cache_on_hostpath() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"local","hostPath":{"path":"/mnt/local/caches"}},
                   {"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"},
                              {"name":"VLLM_CACHE_ROOT","value":"/mnt/caches/vllm"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"},
                                       {"name":"local","mountPath":"/mnt/caches"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

# Drains, weights on a volume, no cache variable at all: the engine-cache half
# and nothing else.
f_engine_cache_only_gap() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

CASE="engine cache: unset VLLM_CACHE_ROOT is reported as unset"
reset; f_vllm_plain
before=$FAILED
assert_field 7 "unset"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: on a claim the engine mounts counts as on a volume"
reset; f_engine_cache_on_claim
before=$FAILED
assert_field 7 "volume:/engine-cache/vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: on a hostPath counts as on a volume"
reset; f_engine_cache_on_hostpath
before=$FAILED
assert_field 7 "volume:/mnt/caches/vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: on an emptyDir is ephemeral, not a volume"
reset; f_engine_cache_on_emptydir
before=$FAILED
assert_field 7 "ephemeral:/dev/shm/vllm-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a name listed twice in env resolves to its LAST value, as the kubelet does"
reset
before=$FAILED
# Seen live on the benchmark stand: the harness lists VLLM_CACHE_ROOT twice,
# its default /tmp/vllm and then the mount. The kubelet takes the last; a
# detector reading the first reported every harness engine as cold.
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","value":"/tmp/vllm"},
                          {"name":"VLLM_CACHE_ROOT","value":"/engine-cache/vllm"}],
                   "volumeMounts":[{"name":"engine-cache","mountPath":"/engine-cache"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "volume:/engine-cache/vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a claim the SIDECAR mounts is not one the engine can use"
reset
before=$FAILED
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}}],
    "containers":[
      {"name":"routing-proxy","image":"quay.io/x/proxy:1",
       "volumeMounts":[{"name":"engine-cache","mountPath":"/engine-cache"}]},
      {"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
       "env":[{"name":"VLLM_CACHE_ROOT","value":"/engine-cache/vllm"}],
       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "ephemeral:/engine-cache/vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: SGLang has no such variable, so the field is empty (no opinion)"
reset; f_sglang_proxy_has_hook
before=$FAILED
assert_field 1 "server"      # anchor: the engine was found
assert_field 7 ""
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "preStop"                  # anchor: a document was produced
assert_not_contains "$doc" "engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the field is LAST, so an un-updated reader keeps a clean argv"
reset; f_vllm_plain
before=$FAILED
gaps="$(so_workload_gaps ns deployments w "$POD_DEPLOY")"
IFS=$'\037' read -r e1 e2 e3 e4 e5 e6 e7 e8 <<< "$gaps"
assert_eq "$e6" "vllm serve Qwen/Qwen3-0.6B" "argv (field 6)"
assert_eq "$e7" "unset" "cachevol (field 7)"
assert_eq "$e8" "" "an eighth field"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the patch carries the three variables, the mount and the claim"
reset; f_engine_cache_only_gap
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "#   engine-cache:"
assert_contains "$doc" "- name: VLLM_CACHE_ROOT"
assert_contains "$doc" "value: /engine-cache/vllm"
assert_contains "$doc" "- name: FLASHINFER_WORKSPACE_DIR"
assert_contains "$doc" "- name: TRITON_CACHE_DIR"
assert_contains "$doc" "mountPath: /engine-cache"
assert_contains "$doc" "claimName: engine-cache"
# ...and only that half: no drain, no weights.
assert_not_contains "$doc" "preStop"
assert_not_contains "$doc" "HF_HOME"
assert_not_contains "$doc" "#   cache:"
# The guard travels with it: the command belongs to the chart, so the patch
# cannot add it, and without it a read-only cache directory is a lost replica.
assert_contains "$doc" 'unset $v'
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: with the weights half too, env/volumeMounts/volumes are each ONE list"
reset; f_vllm_plain
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "HF_HOME"
assert_contains "$doc" "VLLM_CACHE_ROOT"
assert_contains "$doc" "preStop"
assert_eq "$(printf '%s\n' "$doc" | grep -c '^        env:$')" "1" "env lists"
assert_eq "$(printf '%s\n' "$doc" | grep -c '^        volumeMounts:$')" "1" "volumeMounts lists"
assert_eq "$(printf '%s\n' "$doc" | grep -c '^      volumes:$')" "1" "volumes lists"
# ...and the document is YAML yq reads back with BOTH volumes in it, which a
# second `volumes:` key would silently have replaced.
d="$(mktemp)"; printf '%s\n' "$doc" > "$d"
assert_eq "$(yq eval '.spec.template.spec.volumes | length' "$d" 2>/dev/null)" "2" "volumes read back"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env | length' "$d" 2>/dev/null)" "4" "env read back"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: an ephemeral cache is warned about ON THE CONSOLE, with its path"
reset; f_engine_cache_on_emptydir
before=$FAILED
warn="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1 >/dev/null)"
assert_contains "$warn" "COMPILES FROM NOTHING"
assert_contains "$warn" "/dev/shm/vllm-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a cache on a claim is not warned about, and gets no half"
reset; f_engine_cache_on_claim
before=$FAILED
out="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>&1)"
[ -z "$out" ] || fail "expected nothing, got: $out"
reset; f_engine_cache_only_gap    # anchor: the same call does produce one here
assert_contains "$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)" "engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a missing claim is said to be missing, with what a node directory must be"
reset; f_engine_cache_only_gap
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "engine-cache does NOT exist yet in ns"
assert_contains "$doc" "make engine-cache NAMESPACE=ns ENGINE_CACHE_PATH=<node dir>"
assert_contains "$doc" "ENGINE_CACHE_SEED_CLAIM"      # the seed is part of the same instruction
assert_contains "$doc" "at least two components"
assert_contains "$doc" "not the node's own"
assert_contains "$doc" "one directory per trust domain"
assert_contains "$doc" "PersistentVolumes"
assert_contains "$doc" "WEIGHTS_NODE_SELECTOR"
# ...and the way to do without one.
assert_contains "$doc" "Without a node directory"
assert_contains "$doc" "WVA_ENGINE_CACHE_CLAIM"
assert_not_contains "$doc" "already exists"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: an existing claim is reused, not followed by instructions to create one"
reset; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "engine-cache already exists in ns"
assert_contains "$doc" "engine-cache-status"
assert_not_contains "$doc" "does NOT exist"
assert_not_contains "$doc" "ENGINE_CACHE_PATH"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: WVA_ENGINE_CACHE_CLAIM names a shared claim, with its subPath"
reset; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list shared-cache:ReadWriteMany)"
doc="$(WVA_ENGINE_CACHE_CLAIM=shared-cache:caches so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: shared-cache"
assert_contains "$doc" "subPath: caches"
assert_contains "$doc" "shared-cache already exists in ns"
assert_not_contains "$doc" "claimName: engine-cache"
# ...and the document parses with the subPath on the mount, not the volume.
d="$(mktemp)"; printf '%s\n' "$doc" > "$d"
assert_eq "$(yq eval '.spec.template.spec.containers[0].volumeMounts[0].subPath' "$d" 2>/dev/null)" "caches" "subPath read back"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the plan note fires for an unset cache and names the fix"
reset; f_vllm_plain
before=$FAILED
note="$(so_enginecache_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "compiles from nothing"
assert_contains "$note" "VLLM_CACHE_ROOT is unset"
assert_contains "$note" "make workload-patch"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the plan note is silent on a claim, on SGLang, and on an unreadable spec"
reset; f_engine_cache_on_claim
before=$FAILED
assert_eq "$(so_enginecache_note ns deployments w "$POD_DEPLOY")" "" "note for a cache on a claim"
reset; f_sglang_proxy_has_hook
assert_eq "$(so_enginecache_note ns deployments w "$POD_DEPLOY")" "" "note for SGLang"
reset; f_unreadable
assert_eq "$(so_enginecache_note ns deployments w "$POD_DEPLOY")" "" "note for an unreadable spec (the drain note says so)"
assert_contains "$(so_drain_note ns deployments w "$POD_DEPLOY")" "unknown"   # anchor
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the summary counts the workloads that compile from nothing"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/log")" "1 model server(s) COMPILE FROM NOTHING"
assert_not_contains "$(cat "$dir/log")" "RE-DOWNLOAD"
assert_contains "$(cat "$dir/p.yaml")" "claimName: engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: without its opt-in the half is emitted, never applied"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany model-pvc:ReadWriteMany)"
dir="$(mktemp -d)"
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || true
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "VLLM_CACHE_ROOT"
assert_not_contains "$PATCH_BODY" "engine-cache"
assert_not_contains "$PATCH_BODY" "env:"
assert_contains "$(cat "$dir/log")" "engine-cache half is emitted only"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: with its opt-in and the claim, the half IS applied -- and the weights half is not"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany model-pvc:ReadWriteMany)"
dir="$(mktemp -d)"
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" wva_workload_patch >"$dir/log" 2>&1 || true
assert_contains "$PATCH_BODY" "preStop"
assert_contains "$PATCH_BODY" "VLLM_CACHE_ROOT"
assert_contains "$PATCH_BODY" "claimName: engine-cache"
assert_not_contains "$PATCH_BODY" "HF_HOME"
assert_not_contains "$PATCH_BODY" "claimName: model-pvc"
assert_not_contains "$PATCH_BODY" "model-storage"
# The body is a document the API server would take: one env list of three.
d="$(mktemp)"; printf '%s\n' "$PATCH_BODY" > "$d"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env | length' "$d" 2>/dev/null)" "3" "env read back"
assert_eq "$(yq eval '.spec.template.spec.volumes | length' "$d" 2>/dev/null)" "1" "volumes read back"
rm -f "$d"
# ...and the guard's absence is said at the time.
assert_contains "$(cat "$dir/log")" "guard is NOT applied"
assert_contains "$(cat "$dir/log")" "WITHOUT the guard"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: both opt-ins keep both halves"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany model-pvc:ReadWriteMany)"
dir="$(mktemp -d)"
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    WVA_WORKLOAD_PATCH_APPLY_WEIGHTS=true WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || true
assert_contains "$PATCH_BODY" "claimName: engine-cache"
assert_contains "$PATCH_BODY" "claimName: model-pvc"
assert_contains "$PATCH_BODY" "HF_HOME"
assert_contains "$PATCH_BODY" "TRITON_CACHE_DIR"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a missing claim blocks the half rather than stranding pods Pending"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"        # nothing left to send
assert_contains "$(cat "$dir/log")" "does not exist"
assert_contains "$(cat "$dir/log")" "need only a volume"
assert_contains "$(cat "$dir/p.yaml")" "claimName: engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a mount already at /engine-cache blocks the half, NOT the drain half"
reset; LIST_FIXTURE="$(serving_list)"
before=$FAILED
FIXTURE='{"spec":{"template":{"spec":{
    "terminationGracePeriodSeconds":30,
    "volumes":[{"name":"scratch","emptyDir":{}},
               {"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"}],
                   "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"},
                                   {"name":"scratch","mountPath":"/engine-cache"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
dir="$(mktemp -d)"
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" wva_workload_patch >"$dir/log" 2>&1 || true
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "engine-cache"
assert_contains "$(cat "$dir/log")" "already mounted at /engine-cache"
assert_contains "$(cat "$dir/p.yaml")" "claimName: engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a workload with everything on a volume reports the all-clear"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_on_claim
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/log")" "no vLLM among them compiles from nothing"
[ -f "$dir/p.yaml" ] && fail "no file should be written for a converged workload"
[ "$FAILED" -eq "$before" ] && ok

CASE="trim: the weights half can be kept while the engine-cache half goes, and back"
before=$FAILED
mk_both() {
cat > "$1" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: w, namespace: ns1}
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 120
      containers:
      - name: vllm
        lifecycle:
          preStop:
            exec:
              command: ["/bin/sh", "-c", "sleep 45"]
        env:
        - name: HF_HOME
          value: /model-cache/huggingface
        - name: VLLM_CACHE_ROOT
          value: /engine-cache/vllm
        - name: FLASHINFER_WORKSPACE_DIR
          value: /engine-cache/flashinfer
        - name: TRITON_CACHE_DIR
          value: /engine-cache/triton
        volumeMounts:
        - name: model-storage
          mountPath: /model-cache
        - name: engine-cache
          mountPath: /engine-cache
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
      - name: engine-cache
        persistentVolumeClaim:
          claimName: engine-cache
YAML
}
d="$(mktemp)"; mk_both "$d"
so_workload_patch_trim "$d" 1 0 || fail "expected the weights half to survive"
left="$(cat "$d")"
assert_contains "$left" "HF_HOME"
assert_contains "$left" "claimName: model-pvc"
assert_contains "$left" "preStop"
assert_not_contains "$left" "VLLM_CACHE_ROOT"
assert_not_contains "$left" "engine-cache"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env | length' "$d")" "1" "env left"
assert_eq "$(yq eval '.spec.template.spec.volumes | length' "$d")" "1" "volumes left"
mk_both "$d"
so_workload_patch_trim "$d" 0 1 || fail "expected the engine-cache half to survive"
left="$(cat "$d")"
assert_contains "$left" "claimName: engine-cache"
assert_contains "$left" "TRITON_CACHE_DIR"
assert_not_contains "$left" "HF_HOME"
assert_not_contains "$left" "model-storage"
assert_eq "$(yq eval '.spec.template.spec.containers[0].env | length' "$d")" "3" "env left"
mk_both "$d"
so_workload_patch_trim "$d" 0 0 || fail "expected the drain half to survive"
left="$(cat "$d")"
assert_not_contains "$left" "env:"
assert_not_contains "$left" "volumes:"
assert_contains "$left" "preStop"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

CASE="trim: a document with only the two storage halves and neither kept is skipped (rc 1)"
before=$FAILED
d="$(mktemp)"
cat > "$d" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: w, namespace: ns1}
spec:
  template:
    spec:
      containers:
      - name: vllm
        env:
        - name: HF_HOME
          value: /model-cache/huggingface
        - name: VLLM_CACHE_ROOT
          value: /engine-cache/vllm
        volumeMounts:
        - name: model-storage
          mountPath: /model-cache
        - name: engine-cache
          mountPath: /engine-cache
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
      - name: engine-cache
        persistentVolumeClaim:
          claimName: engine-cache
YAML
trim_rc=0
so_workload_patch_trim "$d" 0 0 || trim_rc=$?
assert_eq "$trim_rc" "1" "return code"
assert_contains "$(cat "$d")" "VLLM_CACHE_ROOT"   # anchor: the file was left intact
# ...while keeping one half makes it worth sending.
so_workload_patch_trim "$d" 0 1 || fail "expected the engine-cache half to be worth sending"
assert_contains "$(cat "$d")" "claimName: engine-cache"
rm -f "$d"
[ "$FAILED" -eq "$before" ] && ok

# --- review round: the detector's edges, the claim's kind, the preparer ------

CASE="engine cache: a value from a ConfigMap or Secret is no opinion, never unset"
reset
before=$FAILED
# A strategic merge of `value` into an env entry that has `valueFrom` is
# rejected by the API server -- the whole document, drain hook included -- so
# a detector that called this "unset" and emitted the half took the drain
# half down with it on apply.
FIXTURE='{"spec":{"template":{"spec":{
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","valueFrom":{"configMapKeyRef":{"name":"c","key":"k"}}}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 1 "vllm"      # anchor: the engine was found
assert_field 7 ""
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "preStop"
assert_not_contains "$doc" "VLLM_CACHE_ROOT"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: an empty value is unset, not a cache at ''"
reset
before=$FAILED
FIXTURE='{"spec":{"template":{"spec":{
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","value":""}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "unset"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a mount is matched by path component, not string prefix"
reset
before=$FAILED
# /engine-cache2/vllm is not under a claim mounted at /engine-cache; a prefix
# test said it was, and gave an emptyDir cache a clean bill.
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}},
               {"name":"scratch","emptyDir":{}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","value":"/engine-cache2/vllm"}],
                   "volumeMounts":[{"name":"engine-cache","mountPath":"/engine-cache"},
                                   {"name":"scratch","mountPath":"/engine-cache2"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "ephemeral:/engine-cache2/vllm"
# ...and the same rule for the weights: HF_HOME=/model-cache2/hf is not on
# the claim at /model-cache.
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"HF_HOME","value":"/model-cache2/hf"}],
                   "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 5 "0"
# ...while the mount path itself, and a path under it, still count.
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"engine-cache","persistentVolumeClaim":{"claimName":"engine-cache"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","value":"/engine-cache"}],
                   "volumeMounts":[{"name":"engine-cache","mountPath":"/engine-cache"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "volume:/engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: an inline nfs volume outlives the pod and counts"
reset
before=$FAILED
FIXTURE='{"spec":{"template":{"spec":{
    "volumes":[{"name":"nas","nfs":{"server":"filer","path":"/export/caches"}}],
    "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                   "env":[{"name":"VLLM_CACHE_ROOT","value":"/cache/vllm"}],
                   "volumeMounts":[{"name":"nas","mountPath":"/cache"}],
                   "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
assert_field 7 "volume:/cache/vllm"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a NAMED claim that is missing is not answered with make engine-cache"
reset; f_engine_cache_only_gap
before=$FAILED
# `make engine-cache` creates a claim called engine-cache, never the one the
# operator named, so that remedy would leave every pod Pending on the name the
# patch above actually mounts -- the same defect the weights half once had.
doc="$(WVA_ENGINE_CACHE_CLAIM=my-rwx so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName: my-rwx"
assert_contains "$doc" "my-rwx (WVA_ENGINE_CACHE_CLAIM) does NOT exist in ns"
assert_contains "$doc" "drop WVA_ENGINE_CACHE_CLAIM"
assert_not_contains "$doc" "ENGINE_CACHE_PATH="
assert_not_contains "$doc" "make engine-cache NAMESPACE"
# ...and what it asks of that claim is said: the trust it takes.
assert_contains "$doc" "runs code in every"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the shared-claim alternative states what it must be trusted as"
reset; f_engine_cache_only_gap
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "Without a node directory"
assert_contains "$doc" "runs code in every engine, on every node at"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the all-clear does not vouch for an engine the detector has no opinion on"
reset; LIST_FIXTURE="$(serving_list)"
before=$FAILED
# An SGLang server that drains and keeps its weights: the all-clear must not
# say its compile caches are on a volume, because nothing looked.
FIXTURE='{"spec":{"template":{"spec":{
    "terminationGracePeriodSeconds":120,
    "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
    "containers":[{"name":"server","image":"lmsysorg/sglang:latest",
                   "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"}],
                   "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                   "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                   "args":["python3","-m","sglang.launch_server","--model-path","meta-llama/Llama-3.1-8B"]}]}}}}'
dir="$(mktemp -d)"
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" wva_workload_patch >"$dir/log" 2>&1 || true
assert_contains "$(cat "$dir/log")" "already drain on scale-down"
assert_contains "$(cat "$dir/log")" "no vLLM among them compiles from nothing"
assert_not_contains "$(cat "$dir/log")" "compile caches on a volume"
[ "$FAILED" -eq "$before" ] && ok

CASE="a claim that is not Bound blocks the half, with its phase"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany:Pending)"
dir="$(mktemp -d)"
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" wva_workload_patch >"$dir/log" 2>&1 || true
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$dir/log")" "is not Bound (Pending)"
assert_contains "$(cat "$dir/p.yaml")" "claimName: engine-cache"   # anchor: emitted still
[ "$FAILED" -eq "$before" ] && ok

CASE="a ReadWriteOnce claim blocks the half: it binds to one node"
reset; LIST_FIXTURE="$(serving_list)"; f_llmd_real_downloads
before=$FAILED
# The weights half shares the check, so the second replica of a workload
# whose cache is RWO is refused there too.
PVC_FIXTURE="$(pvc_list model-pvc:ReadWriteOnce)"
apply_weights_run
assert_contains "$PATCH_BODY" "preStop"
assert_not_contains "$PATCH_BODY" "claimName"
assert_contains "$(cat "$DRIVER_LOG")" "not ReadWriteMany"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: an unprepared node blocks the half, and is counted"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":16,"numberReady":14}}'
dir="$(mktemp -d)"
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" wva_workload_patch >"$dir/log" 2>&1 || true
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$dir/log")" "2 of 16 accelerator node(s) are not prepared"
assert_contains "$(cat "$dir/log")" "engine-cache-status"
# ...and with every node prepared the same run applies it.
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":16,"numberReady":16}}'
WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
    WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" wva_workload_patch >"$dir/log" 2>&1 || true
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "claimName: engine-cache"
assert_contains "$(cat "$dir/log")" "WITHOUT the guard"
[ "$FAILED" -eq "$before" ] && ok

CASE="a volume or claim name that is not a DNS-1123 label is refused at entry"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
dir="$(mktemp -d)"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_ENGINE_CACHE_VOLUME_NAME='engine"cache' WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "WVA_ENGINE_CACHE_VOLUME_NAME must be a DNS-1123 label"
[ -f "$dir/p.yaml" ] && fail "nothing should be written"
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_ENGINE_CACHE_CLAIM='shared:../etc' WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "is not a subPath"
# ...and a well-formed one passes the gate (anchor).
DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_ENGINE_CACHE_CLAIM='shared-cache:engine-cache' WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "0" "return code"
assert_contains "$(cat "$dir/p.yaml")" "claimName: shared-cache"
[ "$FAILED" -eq "$before" ] && ok

# --- review round 4: the preparer gate, and the names -----------------------

engine_apply_run() {  # the engine-cache opt-in against $FIXTURE, results in DRIVER_*
    DRIVER_OUT="$(mktemp -d)/p.yaml"; DRIVER_LOG="$(mktemp)"; DRIVER_RC=0
    WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_APPLY=true WVA_WORKLOAD_PATCH_APPLY_ENGINE_CACHE=true \
        WVA_WORKLOAD_PATCH_FILE="$DRIVER_OUT" wva_workload_patch >"$DRIVER_LOG" 2>&1 || DRIVER_RC=$?
}

CASE="engine cache: a preparer that cannot be READ refuses the half (Forbidden is not absent)"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_RC=1                      # a token that may not list DaemonSets, or a timeout
engine_apply_run
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$DRIVER_LOG")" "preparer DaemonSet could not be read"
assert_contains "$(cat "$DRIVER_OUT")" "claimName: engine-cache"   # emitted still
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: no preparer at all (NotFound) is not a refusal -- the claim was checked"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE=""; DS_RC=0       # --ignore-not-found: empty, rc 0
engine_apply_run
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "claimName: engine-cache"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: a preparer with no status yet (0 of 0) is not 'every node prepared'"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list engine-cache:ReadWriteMany)"
DS_FIXTURE='{"status":{}}'
engine_apply_run
assert_eq "$PATCH_CALLS" "0" "kubectl patch calls"
assert_contains "$(cat "$DRIVER_LOG")" "0 of 0 accelerator node(s) are not prepared"
[ "$FAILED" -eq "$before" ] && ok

CASE="engine cache: the gate is for the claim make engine-cache makes, not a namesake DaemonSet"
reset; LIST_FIXTURE="$(serving_list)"; f_engine_cache_only_gap
before=$FAILED
PVC_FIXTURE="$(pvc_list my-rwx:ReadWriteMany)"
DS_FIXTURE='{"status":{"desiredNumberScheduled":3,"numberReady":2}}'   # an unrelated DaemonSet called my-rwx
WVA_ENGINE_CACHE_CLAIM=my-rwx engine_apply_run
assert_eq "$PATCH_CALLS" "1" "kubectl patch calls"
assert_contains "$PATCH_BODY" "claimName: my-rwx"
assert_not_contains "$(cat "$DRIVER_LOG")" "not prepared"
[ "$FAILED" -eq "$before" ] && ok

CASE="the two volume names may not be the same"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
dir="$(mktemp -d)"; DRIVER_RC=0
WVA_DEFAULT_SO_NS=ns1 WVA_ENGINE_CACHE_VOLUME_NAME=model-storage WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" \
    wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?
assert_eq "$DRIVER_RC" "1" "return code"
assert_contains "$(cat "$dir/log")" "the two volumes need two names"
[ -f "$dir/p.yaml" ] && fail "nothing should be written"
[ "$FAILED" -eq "$before" ] && ok

CASE="the claim reference is checked as claim and subPath, each by its own rule"
reset; LIST_FIXTURE="$(serving_list)"; f_vllm_plain
before=$FAILED
dir="$(mktemp -d)"
run_ref() { DRIVER_RC=0; WVA_DEFAULT_SO_NS=ns1 WVA_ENGINE_CACHE_CLAIM="$1" WVA_WORKLOAD_PATCH_FILE="$dir/p.yaml" wva_workload_patch >"$dir/log" 2>&1 || DRIVER_RC=$?; rm -f "$dir/p.yaml"; }
# refused: a slash, an underscore, upper case or a leading dot in the CLAIM part
for bad in a/b a_b Shared .a a..b; do
    run_ref "$bad"; assert_eq "$DRIVER_RC" "1" "rc for claim '$bad'"; assert_contains "$(cat "$dir/log")" "is not a claim name"
done
# refused: an absolute, empty or escaping SUBPATH
for bad in shared:/abs shared: shared:.. shared:../x shared:a/../b; do
    run_ref "$bad"; assert_eq "$DRIVER_RC" "1" "rc for '$bad'"; assert_contains "$(cat "$dir/log")" "is not a subPath"
done
# accepted: a dotted claim, a mixed-case and nested subPath (what --seed-claim takes too)
for good in shared-cache shared.cache:engine-cache shared-cache:Models/qwen_3; do
    run_ref "$good"; assert_eq "$DRIVER_RC" "0" "rc for '$good'"
done
[ "$FAILED" -eq "$before" ] && ok

# ----------------------------------------------------------------------------

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d workload-readiness assertion(s) failed.\n' "$FAILED" >&2
    exit 1
fi
# Every `ok` above increments RAN. A run that ends early -- a library that
# aborts, a case whose command substitution dies under `set -u`, a case deleted
# by accident -- otherwise exits 0 having checked less than it claims.
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf '\n%d case(s) reported ok, %d expected -- the run did not finish.\n' \
        "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi
printf '\nWorkload readiness checks passed.\n'
