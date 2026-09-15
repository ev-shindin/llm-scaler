#!/usr/bin/env bash
#
# Executes the guards in hack/benchmark/two_model_pool.sh, and runs the
# schedule/report self-test beside it.
#
# The scenario's value is that its two arms differ in ONE thing. Every guard
# here protects that, and each protects it against a mistake that produces a
# complete, plausible, WRONG result rather than an error -- at 34 minutes of GPU
# on a shared cluster per arm:
#
#   * `run pool` with no pool writes a directory labelled `pool` containing the
#     nopool arm, and the report believes the label.
#   * `run nopool` with a pool still holding accelerators does the same in the
#     other direction, charging the pool's GPUs to the arm that exists to show
#     life without one.
#   * `run pool` against a COLD pool measures the first burst paying a model
#     load INTO the pool, and reports it as the pool's cost.
#   * a standup that rendered ONE stack leaves one EPP; the other model 404s for
#     the whole run and the failures read as a result.
#   * two gateway Services, or none, and the driver would pick one and drive
#     every request at a stack that does not serve both models.
#
# kubectl is stubbed: these are argument-and-state guards, and every one must
# fire before anything is created. A guard that fires after the Job exists has
# already spent the accelerators.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/hack/benchmark/two_model_pool.sh"
FAIL=0
CASES=0

case_begin() { CASES=$((CASES + 1)); CASE_FAIL_AT="$FAIL"; }
fail() { echo "FAIL $*"; FAIL=$((FAIL + 1)); }
ok()   { [ "$FAIL" -eq "${CASE_FAIL_AT:-0}" ] || return 0; echo "ok   $*"; }

[ -f "$SCRIPT" ] || { echo "FAIL $SCRIPT is missing"; exit 1; }

for tool in python3 jq; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "FATAL: $tool is required by this check and is missing. Nothing below would be about the code."
        exit 2
    }
done

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
STUB="$WORK/bin"; mkdir -p "$STUB"

# A kubectl that answers from environment variables and RECORDS every call, so a
# case can tell "refused" from "refused after creating the Job".
cat > "$STUB/kubectl" <<'STUBEOF'
#!/usr/bin/env bash
printf 'CALL[%s]\n' "$*" >> "${KCALLS:-/dev/null}"
args="$*"
case "$args" in
  *"get deploy wva-warm-pool-"*)
      [ "${POOL_EXISTS:-0}" = "1" ] && exit 0
      echo 'Error from server (NotFound): deployments.apps "wva-warm-pool-x" not found' >&2
      exit 1 ;;
  *"get svc -o json"*)
      case "${GATEWAYS:-1}" in
        0) echo '{"items":[]}' ;;
        1) echo '{"items":[{"metadata":{"name":"infra-llmdbench-inference-gateway"},"spec":{"ports":[{"port":80}]}}]}' ;;
        # BOTH on the HTTP port, or it is not an ambiguity: a second gateway
        # that exposes no port 80 is correctly resolvable, and the case would
        # pass while proving nothing.
        *) echo '{"items":[{"metadata":{"name":"infra-llmdbench-inference-gateway"},"spec":{"ports":[{"name":"http","port":80}]}},
                            {"metadata":{"name":"other-inference-gateway"},"spec":{"ports":[{"name":"http","port":80}]}}]}' ;;
      esac
      exit 0 ;;
  *"get svc "*"jsonpath={.spec.clusterIP}"*) echo "${GW_IP-10.16.1.221}"; exit 0 ;;
  *"get deploy -o name"*)
      i=0
      while [ "$i" -lt "${EPP_COUNT:-2}" ]; do echo "deployment.apps/model$i-epp"; i=$((i+1)); done
      exit 0 ;;
  *"get deploy -o json"*)
      echo '{"items":[
        {"metadata":{"name":"llama-31-8b-decode"},"status":{"readyReplicas":1,"replicas":1},
         "spec":{"template":{"spec":{"containers":[{"image":"ghcr.io/example/vllm:v1"}]}}}},
        {"metadata":{"name":"qwen3-8b-decode"},"status":{"readyReplicas":1,"replicas":1},
         "spec":{"template":{"spec":{"containers":[{"image":"ghcr.io/example/vllm:v1"}]}}}}]}'
      exit 0 ;;
  # A single Deployment's status, which is how verify and reset read the fleet.
  # Without this branch the readiness check reads empty and verify refuses for
  # the wrong reason -- which is how the EPP case first passed while proving
  # nothing about EPPs.
  *"get deploy "*"-o jsonpath={.status.readyReplicas}"*) echo "${READY:-1}"; exit 0 ;;
  *"get deploy "*"-o jsonpath={.status.replicas}"*) echo "${READY:-1}"; exit 0 ;;
  # The routing table, which is how a stack name resolves to its backend. Names
  # are the HASH form the harness actually generates, not the scenario's pinned
  # shortName -- which it ignores.
  *"get httproute -o json"*)
      if [ "${ROUTES:-1}" = "1" ]; then
        echo '{"items":[{"metadata":{"name":"multi-model-route"},"spec":{"rules":[
          {"matches":[{"path":{"value":"/llama-31-8b"}}],"backendRefs":[{"name":"unsloth--244120d9-instruct-router"}]},
          {"matches":[{"path":{"value":"/qwen3-8b"}}],"backendRefs":[{"name":"qwen-qwe-6e036fd5-qwen3-8b-router"}]}]}}]}'
      else
        echo '{"items":[]}'
      fi
      exit 0 ;;
  *"get deploy "*"-o name"*) [ "${DEPLOY_EXISTS:-1}" = "1" ] && exit 0; exit 1 ;;
  # The probe Pod's lifecycle. Without these the probe polls for its Pod to
  # finish, against a stub that never says it did -- 180s per attempt, three
  # attempts, two models, and the check takes twenty minutes to say nothing.
  *"get pod probe-"*"jsonpath={.status.phase}"*) echo "Succeeded"; exit 0 ;;
  *"logs probe-"*) echo "${PROBE_RESULT:-HTTP 200}"; exit 0 ;;
  # The controller's own Prometheus URL, and the flow-control probe. FC_MODELS
  # is what Prometheus answers: the model_name labels that actually have a
  # flow-control series.
  *"get cm wva-manager-config"*) echo "PROMETHEUS_BASE_URL: \"${PROM_URL-https://prom.example:9090}\""; exit 0 ;;
  *"get pod fcheck-"*"jsonpath={.status.phase}"*) echo "Succeeded"; exit 0 ;;
  *"logs fcheck-"*)
      # FCHECK_OUT stands in for whatever the probe printed, so a query that
      # never answered can be told apart from one that answered "no series".
      if [ -n "${FCHECK_OUT:-}" ]; then echo "$FCHECK_OUT"; else
        echo "MODELS ${FC_MODELS-unsloth/Meta-Llama-3.1-8B-Instruct Qwen/Qwen3-8B}"; fi
      exit 0 ;;
  *"rollout restart"*|*"rollout status"*) exit 0 ;;
  *"get pvc"*) [ "${HAS_PVC:-1}" = "1" ] && exit 0; exit 1 ;;
  *"get pods -l llm-d.ai/warm-pool"*)
      [ "${POOL_EXISTS:-0}" = "1" ] && echo "wva-warm-pool-twomodel-abc"
      exit 0 ;;
  *"get pods -o json"*) echo '{"items":[]}' ; exit 0 ;;
  *exec*)
      # The supervisor answers, and holds NOTHING -- the cold pool.
      echo "${RESIDENT_JSON:-[]}"
      exit 0 ;;
  *"get nodes -o json"*)
      n="${NODE_GPUS:-8}"
      if [ "${MIXED_ACCEL:-0}" = "1" ]; then
        echo '{"items":[
          {"spec":{},"status":{"allocatable":{"nvidia.com/gpu":"'"$n"'"},"conditions":[{"type":"Ready","status":"True"}]},
           "metadata":{"labels":{"gpu.nvidia.com/model":"H200"}}},
          {"spec":{},"status":{"allocatable":{"nvidia.com/gpu":"'"$n"'"},"conditions":[{"type":"Ready","status":"True"}]},
           "metadata":{"labels":{"gpu.nvidia.com/model":"A100"}}}]}'
      else
        echo '{"items":[{"spec":{},"status":{"allocatable":{"nvidia.com/gpu":"'"$n"'"},"conditions":[{"type":"Ready","status":"True"}]},
           "metadata":{"labels":{"gpu.nvidia.com/model":"H200"}}}]}'
      fi
      exit 0 ;;
  *"get pods -A -o json"*) echo '{"items":[]}' ; exit 0 ;;
  *) exit 0 ;;
esac
STUBEOF
chmod +x "$STUB/kubectl"

# run_verb <verb...> -- the script with the stub first on PATH, into $OUT/$RC.
run_verb() {
    CALLS="$WORK/calls"; : > "$CALLS"
    OUT="$(PATH="$STUB:$PATH" KCALLS="$CALLS" BENCHMARK_NAMESPACE=ns-under-test \
        POOL_EXISTS="${POOL_EXISTS:-0}" EPP_COUNT="${EPP_COUNT:-2}" \
        GATEWAYS="${GATEWAYS:-1}" HAS_PVC="${HAS_PVC:-1}" \
        NODE_GPUS="${NODE_GPUS:-8}" MIXED_ACCEL="${MIXED_ACCEL:-0}" \
        RESIDENT_JSON="${RESIDENT_JSON:-[]}" \
        WARM_GATE_TIMEOUT=1 \
        bash "$SCRIPT" "$@" 2>&1)"
    RC=$?
}

# ---------------------------------------------------------------------------
# The arm must match the cluster
# ---------------------------------------------------------------------------
case_begin
POOL_EXISTS=0 run_verb run pool
if [ "$RC" -eq 0 ]; then
    fail "'run pool' was accepted with no pool present; the results would be the nopool arm under a pool label"
elif ! printf '%s' "$OUT" | grep -q 'pool-create'; then
    fail "'run pool' refused without saying what to do about it: $OUT"
elif grep -q 'CALL\[apply' "$CALLS" 2>/dev/null; then
    fail "'run pool' created something before refusing: $(cat "$CALLS")"
else
    ok "'run pool' with no pool is refused, before anything is created"
fi

case_begin
POOL_EXISTS=1 run_verb run nopool
if [ "$RC" -eq 0 ]; then
    fail "'run nopool' was accepted while a pool held accelerators; its GPU-seconds would carry the pool's cost into the arm that exists to show life without one"
elif ! printf '%s' "$OUT" | grep -q 'pool-delete'; then
    fail "'run nopool' refused without naming the fix: $OUT"
else
    ok "'run nopool' with a pool present is refused"
fi

# A COLD pool is the worst result this scenario can produce: the arm runs to
# completion and reports the cost of a pool nobody would operate that way.
case_begin
POOL_EXISTS=1 RESIDENT_JSON='[]' run_verb run pool
if [ "$RC" -eq 0 ]; then
    fail "'run pool' started against a pool holding neither model; it would measure a cold pool and report it as the pool's cost"
elif ! printf '%s' "$OUT" | grep -qi 'cold pool'; then
    fail "'run pool' refused without naming the cold pool as the reason: $OUT"
elif ! printf '%s' "$OUT" | grep -q 'warm'; then
    fail "'run pool' refused without naming the step that fixes it: $OUT"
elif grep -q 'CALL\[apply' "$CALLS" 2>/dev/null; then
    fail "'run pool' created the load Job before refusing: $(cat "$CALLS")"
else
    ok "'run pool' against a pool that holds neither model is refused"
fi

case_begin
POOL_EXISTS=0 run_verb run
[ "$RC" -eq 0 ] && fail "'run' with no arm was accepted" || ok "'run' requires an arm"

case_begin
POOL_EXISTS=0 run_verb run sideways
[ "$RC" -eq 0 ] && fail "'run sideways' was accepted" || ok "an unknown arm is refused"

# ---------------------------------------------------------------------------
# verify -- the guards that make a 34-minute run worth starting
# ---------------------------------------------------------------------------
case_begin
EPP_COUNT=1 run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed with ONE EPP; the standup rendered one stack, and the missing model would 404 for the whole run"
elif ! printf '%s' "$OUT" | grep -qi 'epp'; then
    fail "verify refused without naming the EPP count: $OUT"
else
    ok "one EPP means one stack, and verify says so"
fi

case_begin
GATEWAYS=0 run_verb verify
[ "$RC" -eq 0 ] && fail "verify passed with no inference gateway Service" \
    || ok "no gateway Service is refused"

case_begin
GATEWAYS=2 run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed with TWO gateway Services; the driver would pick one and send both models' traffic at whichever stack owns it"
else
    ok "an ambiguous gateway is refused rather than picked"
fi

# ---------------------------------------------------------------------------
# preflight -- the accelerator arithmetic that makes the arms comparable
# ---------------------------------------------------------------------------
case_begin
run_verb preflight
if ! printf '%s' "$OUT" | grep -q 'peaks at 6'; then
    fail "preflight did not report the peak this run needs (2 models x 3 replicas = 6): $OUT"
elif ! printf '%s' "$OUT" | grep -q 'pool arm:.*2 replicas'; then
    fail "preflight did not state the pool arm's LOWER model ceiling, which is what makes the two arms comparable: $OUT"
else
    ok "preflight states both arms' budgets and that they match"
fi

case_begin
NODE_GPUS=4 run_verb preflight
if [ "$RC" -eq 0 ]; then
    fail "preflight passed with 4 free accelerators and a peak of 6; the run would spend its bursts Pending and both arms would measure the scheduler"
else
    ok "a peak larger than the free accelerators is refused"
fi

case_begin
MIXED_ACCEL=1 run_verb preflight
if [ "$RC" -eq 0 ]; then
    fail "preflight passed on a cluster advertising TWO accelerator products; a warm copy is only reusable on the one it was loaded on, so the pool would never be eligible to lend and the run would complete reporting that it did nothing"
elif ! printf '%s' "$OUT" | grep -q 'ACCELERATOR'; then
    fail "preflight refused without naming the variable that resolves it: $OUT"
else
    ok "an ambiguous accelerator is refused, naming ACCELERATOR"
fi

case_begin
OUT="$(PATH="$STUB:$PATH" KCALLS=/dev/null BENCHMARK_NAMESPACE=ns PHASE_SECONDS=300 \
    NODE_GPUS=8 bash "$SCRIPT" preflight 2>&1)"; RC=$?
if [ "$RC" -eq 0 ]; then
    fail "preflight passed with PHASE_SECONDS=300, which is shorter than scale-down stabilization: each model holds the replicas it grew through most of the OTHER model's rise, so the anti-phase premise is never exercised"
else
    ok "a phase shorter than scale-down stabilization is refused"
fi

# ---------------------------------------------------------------------------
# A namespace is never optional -- this drives load and creates Jobs.
# ---------------------------------------------------------------------------
case_begin
OUT="$(PATH="$STUB:$PATH" BENCHMARK_NAMESPACE= bash "$SCRIPT" status 2>&1)"; RC=$?
[ "$RC" -eq 0 ] && fail "a verb ran with no BENCHMARK_NAMESPACE" \
    || ok "every verb requires a namespace"

# ---------------------------------------------------------------------------
# Both halves of the spec must exist, or the CLI cannot see it at all.
#
# `--spec` resolves config/specification/<name>.yaml.j2 and NEVER a scenario, so
# a scenario shipped on its own is invisible: the CLI reports
# "Specification '<name>' not found" and prints the list it does know, which
# reads like a missing file while the scenario is plainly sitting in the clone.
# That cost one standup.
# ---------------------------------------------------------------------------
case_begin
SPEC_DIR="$ROOT/hack/benchmark/scenarios/guides"
if [ ! -f "$SPEC_DIR/two-model-warm-pool.yaml" ]; then
    fail "the scenario two-model-warm-pool.yaml is missing"
elif [ ! -f "$SPEC_DIR/two-model-warm-pool.yaml.j2" ]; then
    fail "two-model-warm-pool.yaml exists but its SPECIFICATION (.yaml.j2) does not, so --spec cannot resolve it and the standup dies after installing the scenario"
elif ! grep -q 'scenario_file' "$SPEC_DIR/two-model-warm-pool.yaml.j2"; then
    fail "the specification does not name a scenario_file; the CLI would render the default one"
elif ! grep -q 'scenarios/guides/two-model-warm-pool.yaml' "$SPEC_DIR/two-model-warm-pool.yaml.j2"; then
    fail "the specification points at a scenario other than its own: $(grep -A1 scenario_file "$SPEC_DIR/two-model-warm-pool.yaml.j2")"
else
    ok "the scenario and its specification both exist, and the specification names it"
fi

case_begin
# The stack names are the HTTPRoute path prefixes AND the driver's defaults --
# one fact in two files. Drift makes every request 404 for a whole run.
if ! grep -q 'STACK_A="${STACK_A:-llama-31-8b}"' "$SCRIPT"; then
    fail "the driver's STACK_A default changed; it must match a stack name in the scenario"
elif ! grep -qE '^  - name: "llama-31-8b"' "$SPEC_DIR/two-model-warm-pool.yaml"; then
    fail "the scenario has no stack named llama-31-8b, which the driver builds model A's endpoint from"
elif ! grep -qE '^  - name: "qwen3-8b"' "$SPEC_DIR/two-model-warm-pool.yaml"; then
    fail "the scenario has no stack named qwen3-8b, which the driver builds model B's endpoint from"
elif ! grep -q 'STACK_B="${STACK_B:-qwen3-8b}"' "$SCRIPT"; then
    fail "the driver's STACK_B default changed; it must match a stack name in the scenario"
else
    ok "both stack names are the driver's defaults and the scenario's, which is what the HTTPRoute keys on"
fi

# The stack -> Deployment mapping must come from the ROUTE, never from a name.
# The harness IGNORES model.shortName and generates a namespace-salted hash --
# measured on CoreWeave: unsloth/Meta-Llama-3.1-8B-Instruct became
# `unsloth--244120d9-instruct`, which shares no substring with the stack name
# `llama-31-8b`. Matching names finds nothing, silently, and verify then reports
# a stack that is plainly serving as absent.
case_begin
DEPLOY_EXISTS=1 ROUTES=1 run_verb verify
if printf '%s' "$OUT" | grep -q 'unsloth--244120d9-instruct-decode'; then
    ok "a stack resolves to its Deployment through the HTTPRoute, not through its name"
elif printf '%s' "$OUT" | grep -q 'no decode Deployment'; then
    fail "the stack did not resolve to a Deployment. It has to be looked up through the route's backendRef, because the harness's generated name shares no substring with the stack name."
else
    fail "verify did not name the Deployment it resolved, so this case cannot tell how it was found: $OUT"
fi

case_begin
ROUTES=0 run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed with NO HTTPRoute: there is no path for either model, and every request in a 34-minute run would 404"
else
    ok "a namespace with no HTTPRoute is refused"
fi

# THE ONE THAT COST AN ENTIRE A/B. `featureGates: [flowControl]` was present and
# correct in both EPP ConfigMaps, and the metric it gates was never emitted: the
# EPP reads that config ONCE at startup, the Helm update did not change the pod
# template, and process_start_time_seconds showed both EPPs still running the
# config they loaded 35 minutes before the gate existed. WVA's scheduler-queue
# query returned no series for either model through two 34-minute arms, and
# nothing said so. A config is a statement of intent; the series is the fact.
case_begin
FC_MODELS="" run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed while the EPP flow-control queue had NO series: WVA's scheduler-queue signal would be absent for the whole run and the autoscaler would scale on a metric that does not exist"
elif ! printf '%s' "$OUT" | grep -q 'rollout restart'; then
    fail "verify refused without naming the fix (restart the EPPs so they re-read the config): $OUT"
else
    ok "a flow-control queue with no series is refused, naming the EPP restart"
fi

case_begin
FC_MODELS="Qwen/Qwen3-8B" run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed with a flow-control series for only ONE of the two models"
else
    ok "flow control live for one model but not the other is refused"
fi

# NOT KNOWING IS NOT PASSING. Both of these used to `return 0`, which is the
# same outcome the 34-minute arms got: the check ran, looked at nothing, and
# said nothing was wrong.
case_begin
PROM_URL="" run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed without being able to read PROMETHEUS_BASE_URL; it checked nothing and reported success, which is how two full arms ran on a metric that was never emitted"
elif ! printf '%s' "$OUT" | grep -q 'WVA_NS'; then
    fail "verify refused without naming the knob that fixes it (WVA_NS, when the controller is elsewhere): $OUT"
else
    ok "an unreadable Prometheus URL is refused, not skipped"
fi

case_begin
FCHECK_OUT="QUERYFAIL timeout" run_verb verify
if [ "$RC" -eq 0 ]; then
    fail "verify passed when the Prometheus query never answered; an unanswered query says nothing about whether the signal exists"
elif ! printf '%s' "$OUT" | grep -q 'SKIP_FLOW_CONTROL_CHECK'; then
    fail "verify refused without naming the deliberate way past: $OUT"
else
    ok "a Prometheus query that did not answer is refused, not skipped"
fi

# ---------------------------------------------------------------------------
# The load Job, rendered. Load comes from inference-perf -- the llm-d harness's
# own generator -- so what the Pod is handed decides whether a run happens at
# all, and every one of these mistakes produces a plausible-looking directory
# rather than an error.
# ---------------------------------------------------------------------------
# Sourced rather than invoked: these are about what the render FUNCTIONS
# produce, and reaching them through a verb would need a cluster.
( . "$SCRIPT" ) >/dev/null 2>&1 || true
# shellcheck disable=SC1090
. "$SCRIPT" >/dev/null 2>&1
NS=ns-under-test
# The stub has to be on PATH here too: these functions call kubectl, and against
# the REAL one they fail and take a fallback -- which is a pass for the wrong
# reason on any case about what the normal path produces.
PATH="$STUB:$PATH"

case_begin
url="$(base_url_for_stack gw-svc:80 llama-31-8b)"
if printf '%s' "$url" | grep -q '/v1'; then
    fail "base_url_for_stack returned '$url'. inference-perf appends the route itself, so every request would go to /v1/completions/v1/completions and the gateway would 404 the whole run"
elif ! printf '%s' "$url" | grep -q '/llama-31-8b$'; then
    fail "base_url_for_stack returned '$url', which does not end at the stack's path prefix; llm-d routes a multi-model stack by path and the requests would reach the wrong stack"
else
    ok "the harness base_url is the gateway plus the stack prefix, with no route"
fi

# The DATA PATH must not resolve anything. Measured twice on CoreWeave at only
# ~10 rps: 5.7% of requests died of ClientConnectorDNSError, spread across the
# whole run rather than bunched at startup -- aiohttp resolves per connection,
# and ndots:5 costs four lookups for each. That is the driver's own loss, three
# times the report's threshold, and it voids the arm.
case_begin
if ! printf '%s' "$url" | grep -qE '^http://[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:'; then
    fail "base_url is '$url', a name the load Pod would resolve on every connection. The gateway's ClusterIP is knowable before the run, and resolving nothing is the point"
else
    ok "the load addresses the gateway by ClusterIP, so the data path resolves nothing"
fi

case_begin
fallback="$(GW_IP="" base_url_for_stack gw-svc:80 llama-31-8b)"
if printf '%s' "$fallback" | grep -qE '^http://[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:'; then
    fail "with no ClusterIP readable the fallback still produced an address: $fallback"
elif ! printf '%s' "$fallback" | grep -q 'svc\.cluster\.local\.:'; then
    fail "the DNS fallback '$fallback' is not an ABSOLUTE name. Without the trailing dot, ndots:5 makes the resolver try every search domain first -- four lookups per connection, which is the failure this is falling back from"
else
    ok "an unreadable ClusterIP falls back to an absolute name, not a searched one"
fi

case_begin
JOB_YAML="$(MODEL_A=model-a MODEL_B=model-b LOAD_IMAGE=ghcr.io/llm-d/llm-d-benchmark:vX \
    render_load_job wva-two-model-load-pool pool 1770000000 2>&1)"
n_containers="$(printf '%s\n' "$JOB_YAML" | grep -c '^        - name: load-')"
n_starts="$(printf '%s\n' "$JOB_YAML" | grep -c '1770000000')"
if [ "$n_containers" -ne 2 ]; then
    fail "the load Job rendered $n_containers load container(s), not 2. Only one model would be driven, and the other's rows would be an empty half of an anti-phase run"
elif [ "$n_starts" -ne 2 ]; then
    fail "the two containers do not share one start barrier ($n_starts of 2 carry it); their ladders would be offset by however far apart the containers happened to start, which is the anti-phase this scenario measures"
elif ! printf '%s\n' "$JOB_YAML" | grep -q '/profiles/run.sh'; then
    fail "the load containers do not run the harness wrapper, so nothing waits on the barrier or holds the Pod open for collection"
else
    ok "one Pod, two containers, one start barrier"
fi

# Each arm otherwise refetches both tokenizers across the public internet, and a
# blip there costs the arm: measured, a CAS Client Error from the Hugging Face
# CDN took model A down while model B fetched fine, and the run was refused
# after five minutes of preload grace with both models' accelerators held.
case_begin
if ! printf '%s\n' "$JOB_YAML" | grep -A2 '^        - name: hf$' | grep -q 'persistentVolumeClaim'; then
    fail "the tokenizer cache is not on the shared claim, so every arm refetches both tokenizers from the internet and a CDN blip costs a whole arm: $(printf '%s\n' "$JOB_YAML" | grep -A2 '^        - name: hf$')"
elif ! printf '%s\n' "$JOB_YAML" | grep -q 'subPath: benchmark-tokenizer-cache'; then
    fail "the cache mount has no subPath, so it would write into the same tree the models read their weights from"
else
    ok "tokenizers cache on the shared claim, under their own subPath"
fi

case_begin
img="$(LOAD_IMAGE="" ROOT=/nonexistent load_image)"
if ! printf '%s' "$img" | grep -q 'llm-d-benchmark'; then
    fail "load_image resolved to '$img', which is not the harness image. inference-perf lives only in llm-d-benchmark; any other image starts, finds no such command, and the arm ends with an empty results directory"
else
    ok "the load image is the llm-d-benchmark harness ($img)"
fi

# ---------------------------------------------------------------------------
# The schedule and the report, executed.
# ---------------------------------------------------------------------------
case_begin
if python3 "$ROOT/hack/benchmark/two_model_selftest.py"; then
    ok "schedule and report self-test"
else
    fail "the schedule/report self-test failed (above)"
fi

case_begin
CASES_EXPECTED=29
if [ "$CASES" -ne "$CASES_EXPECTED" ]; then
    fail "$CASES cases ran, not $CASES_EXPECTED. Update CASES_EXPECTED deliberately rather than letting coverage drift out."
else
    ok "all $CASES cases ran"
fi

if [ "$FAIL" -ne 0 ]; then
    echo "two-model scenario check FAILED"
    exit 1
fi
echo "two-model scenario check OK"
