#!/usr/bin/env bash
#
# Executes the model-identification path in deploy/lib/scaledobject.sh against
# fixture command lines and pod specs.
#
# Sibling of check-workload-gaps.sh, and it exists for the same reason: these
# parsers decide which model every ScaledObject is written for, every defect
# they have shipped parses cleanly, and each was found on a cluster one install
# at a time -- `python` answered as the model of an SGLang server, a CRLF that
# matched no metric series, a --served-model-name read as the literal
# "$MODEL_NAME". None is a syntax error and all of them silently point WVA at a
# model no metric series reports.
#
# WRITING A CASE HERE: an expected-EMPTY case proves nothing on its own -- a
# function that does not exist returns empty too. The guard below refuses to run
# unless both functions are defined, and EXPECT_CASES refuses to pass a run that
# ended early; between them an empty answer means the function decided on it.
#
# The last group is not a string test. It lifts the discovery jq query out of
# the library and runs it against a pod fixture, because the record that query
# builds is pipe-delimited and newline-framed, and the two ways to break that
# framing -- a field holding the delimiter, a field holding a newline -- are
# invisible from the shell functions alone. Lifted rather than copied, so it
# cannot pass against a stale duplicate of the query.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# jq is not optional: the record group below cannot run without it, and a group
# that skips is how a green run comes to mean nothing.
command -v jq >/dev/null 2>&1 || {
    printf 'FATAL: jq is required to run these checks.\n' >&2
    exit 1
}

# Sourced through a CR-stripped copy, for the reason check-workload-gaps.sh
# gives: a Windows checkout with core.autocrlf makes bash stop at the first
# function definition, which looks exactly like every function being missing.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh scaledobject.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
# common.sh requires these; without them the first log_warning dies under set -u.
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/scaledobject.sh"

for fn in so_model_id so_resolve_env_ref; do
    declare -F "$fn" >/dev/null || {
        printf 'FATAL: %s is not defined after sourcing the library.\n' "$fn" >&2
        exit 1
    }
done

EXPECT_CASES=24
RAN=0
FAILED=0

# check <name> <want> <got>
check() {
    if [ "$2" = "$3" ]; then
        RAN=$((RAN + 1))
        printf 'ok    %s\n' "$1"
    else
        FAILED=$((FAILED + 1))
        printf 'FAIL  %s\n        want [%s]\n        got  [%s]\n' "$1" "$2" "$3" >&2
    fi
}

# ----------------------------------------------------------------------------
# so_model_id: which token on a command line is the model
# ----------------------------------------------------------------------------
check 'so_model_id reads --served-model-name <v>' \
    'Qwen/Qwen3-0.6B' \
    "$(so_model_id 'vllm serve /model-cache/x --served-model-name Qwen/Qwen3-0.6B --port 8000')"

check 'so_model_id reads --served-model-name=<v>' \
    'Qwen/Qwen3-0.6B' \
    "$(so_model_id 'vllm serve --served-model-name=Qwen/Qwen3-0.6B')"

check 'so_model_id prefers the served name over --model' \
    'Qwen/Qwen3-0.6B' \
    "$(so_model_id '--model /model-cache/models/Qwen/Qwen3-0.6B --served-model-name Qwen/Qwen3-0.6B')"

check 'so_model_id falls back to --model' \
    '/model-cache/models/Qwen' \
    "$(so_model_id 'vllm serve --model /model-cache/models/Qwen --port 8000')"

# The llm-d flagship-guide shape: command ["vllm","serve"], model first in args.
check 'so_model_id reads the positional model after serve' \
    'Qwen/Qwen3-32B' \
    "$(so_model_id 'vllm serve Qwen/Qwen3-32B --tensor-parallel-size=2')"

# No `serve` to anchor on. Empty is the answer; the bare "first non-flag token"
# fallback that used to live here returned `python`.
check 'so_model_id answers nothing for an sglang launcher' \
    '' \
    "$(so_model_id 'python -m sglang.launch_server --model-path /model-cache/x')"

check 'so_model_id answers nothing for an empty command line' \
    '' \
    "$(so_model_id '')"

# ----------------------------------------------------------------------------
# so_resolve_env_ref: a model name that is a variable reference
# ----------------------------------------------------------------------------
ENVS='MODEL_PATH=/model-cache/qwen BASE=Qwen/Qwen3 MODEL_NAME=Qwen/Qwen3-0.6B'

check 'so_resolve_env_ref passes a real model name through' \
    'Qwen/Qwen3-0.6B' \
    "$(so_resolve_env_ref 'Qwen/Qwen3-0.6B' "$ENVS")"

# The shell-script spelling: customCommand runs under bash inside the container.
check 'so_resolve_env_ref answers $VARNAME' \
    'Qwen/Qwen3-0.6B' \
    "$(so_resolve_env_ref '$MODEL_NAME' "$ENVS")"

check 'so_resolve_env_ref answers ${VARNAME}' \
    'Qwen/Qwen3-0.6B' \
    "$(so_resolve_env_ref '${MODEL_NAME}' "$ENVS")"

# The Kubernetes spelling: expanded by the kubelet, so the pod spec keeps the
# literal. config/scenarios/cicd/kind.yaml and examples/sim.yaml both use it.
check 'so_resolve_env_ref answers $(VARNAME)' \
    'Qwen/Qwen3-0.6B' \
    "$(so_resolve_env_ref '$(MODEL_NAME)' "$ENVS")"

check 'so_resolve_env_ref drops an unset reference rather than echoing it raw' \
    '' \
    "$(so_resolve_env_ref '$NOT_SET_HERE' "$ENVS")"

# Compound tokens resolve to nothing on purpose: half a name is a modelID that
# looks right and matches no metric series.
check 'so_resolve_env_ref drops ${VAR}suffix' \
    '' \
    "$(so_resolve_env_ref '${BASE}-instruct' "$ENVS")"

check 'so_resolve_env_ref drops $(VAR)suffix' \
    '' \
    "$(so_resolve_env_ref '$(MODEL_NAME)x' "$ENVS")"

check 'so_resolve_env_ref drops an unclosed brace' \
    '' \
    "$(so_resolve_env_ref '${MODEL_NAME' "$ENVS")"

check 'so_resolve_env_ref drops a bare $' \
    '' \
    "$(so_resolve_env_ref '$' "$ENVS")"

# printf, not echo: echo would swallow this as an option and answer empty.
check 'so_resolve_env_ref keeps a token that begins with a dash' \
    '-n' \
    "$(so_resolve_env_ref '-n' "$ENVS")"

check 'so_resolve_env_ref answers nothing with no env list at all' \
    '' \
    "$(so_resolve_env_ref '$MODEL_NAME' '')"

# ----------------------------------------------------------------------------
# The discovery record: one workload per LINE, fields split on a pipe
# ----------------------------------------------------------------------------
# The program is the last one closed by the `] | join("|"))` that ends the
# record. Extraction failing is a FAILURE, not a skip.
prog="$(awk '
    /jq -r --argjson p/            { buf = ""; collecting = 1; next }
    collecting && /join\("\|"\)\)/ { buf = buf $0; print buf; exit }
    collecting                     { buf = buf $0 "\n" }
' "$WORK/scaledobject.sh")"
case "$prog" in
    *"')") prog="${prog%??}" ;;   # the shell quote and paren closing the call
    *)     prog="" ;;
esac

if [ -z "$prog" ]; then
    FAILED=$((FAILED + 1))
    printf 'FAIL  could not lift the discovery jq program out of the library\n' >&2
else
    # BLOCK is what a YAML block scalar (`value: |`) writes: a value with a
    # newline in it. Left in the record it splits this one workload across two
    # lines -- the real entry losing the args field that trails it, and with it
    # the model it is being read for, plus a phantom entry named after the tail.
    # SPACED would split the env list itself, PIPED the record fields. The proxy
    # container is first on purpose: the engine is container 1.
    fixture="$WORK/pods.json"
    cat > "$fixture" <<'JSON'
{"items":[{"metadata":{"name":"decode","labels":{}},"spec":{"template":{"metadata":{"labels":{"app":"d"}},"spec":{"containers":[
  {"name":"proxy","image":"routing-proxy","args":[]},
  {"name":"vllm","image":"vllm/vllm-openai",
   "env":[{"name":"BLOCK","value":"/a\nb"},
          {"name":"SPACED","value":"a b"},
          {"name":"PIPED","value":"a|b"},
          {"name":"FROM_SECRET"},
          {"name":"MODEL_NAME","value":"Qwen/Qwen3-0.6B"}],
   "command":["vllm","serve"],
   "args":["--served-model-name","$(MODEL_NAME)"]}]}}}}]}
JSON
    out="$(jq -r --argjson p '["spec","template"]' "$prog" "$fixture" 2>&1)"

    check 'discovery emits one line per workload' \
        '1' \
        "$(printf '%s\n' "$out" | wc -l | tr -d ' ')"

    IFS='|' read -r r_name r_labels _r_objlabels r_envs r_args <<<"$out"
    check 'discovery names the workload' 'decode' "$r_name"
    check 'discovery reads the pod labels' 'app=d' "$r_labels"
    check 'discovery keeps only single-token env values' \
        'MODEL_NAME=Qwen/Qwen3-0.6B' "$r_envs"
    check 'discovery reads args off the ENGINE container' \
        'vllm serve --served-model-name $(MODEL_NAME)' "$r_args"

    # What the whole chain is for.
    check 'discovery resolves to a usable modelID' \
        'Qwen/Qwen3-0.6B' \
        "$(so_resolve_env_ref "$(so_model_id "$r_args")" "$r_envs")"
fi

# ----------------------------------------------------------------------------

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d model-identification assertion(s) failed.\n' "$FAILED" >&2
    exit 1
fi
# Same guard as check-workload-gaps.sh: a run that ends early otherwise exits 0
# having checked less than it claims.
if [ "$RAN" -ne "$EXPECT_CASES" ]; then
    printf '\n%d case(s) reported ok, %d expected -- the run did not finish.\n' \
        "$RAN" "$EXPECT_CASES" >&2
    exit 1
fi
printf '\nModel identification checks passed.\n'
