#!/usr/bin/env bash
# Two models, two EPPs, anti-phase bursts, with the warm pool on and off.
#
# THE QUESTION THIS ANSWERS
# -------------------------
# A warm pool is insurance: every pool Pod holds its accelerators continuously,
# lending or idle, so a pool of N lowers the maximum fleet by N. The case for
# paying that is that ONE pool covers MANY models -- an argument nothing in this
# repo measures. Every warm-pool number taken so far is one model bridging its
# own scale-up, where the pool is pure overhead at steady state and the only
# question is whether the bridge beats the ramp.
#
# With two models whose bursts do not coincide, the pool is shared: it lends to
# whichever model is rising and is handed back in time for the other. One GPU of
# insurance covers two ramps. If that does not show up here, the shared-pool
# argument is wrong and this run is how we find out.
#
# WHY ANTI-PHASE, SPECIFICALLY
# ----------------------------
# Because it isolates the sharing. The SUM of the two models' demand is flat, so
# a cluster that could see the whole picture would hold its total fleet constant
# and move replicas between the models. Nothing does see it -- each model scales
# on its own signal -- so without a pool both models pay a full cold load on
# every rise while the other model is giving capacity back.
#
# FOR THE COMPARISON TO MEAN ANYTHING
# -----------------------------------
# The two arms must differ in ONE thing. Four ways they silently did not, each
# of which produces a complete and plausible table:
#
#   * The pool arm was allowed MORE accelerators. Insurance must LOWER the
#     ceiling, so the pool arm runs at MAX_REPLICAS - POOL_REPLICAS per model
#     and the nopool arm at MAX_REPLICAS. `run` sets this; it does not hope.
#   * The fleet was not reset. With scale-down stabilization at 300s and nopool
#     always first, the second arm starts on an already-scaled fleet and pays no
#     cold load at all. `reset` pins both models back to MIN_REPLICAS, waits for
#     it, and only then releases them.
#   * The pool was COLD. Then the first burst pays a model load INTO the pool on
#     top of the replica's own, and the arm reports the cost of a pool nobody
#     would operate that way. `warm` pins both models resident and waits.
#   * The load differed. Arrival times are pre-computed from a per-model seed,
#     so both arms send byte-identical traffic.
#
# STEP BY STEP, DELIBERATELY. The expensive failures here are the ones found at
# minute 40 of a 45-minute run.
#
#   two_model_pool.sh preflight     what the run needs, before anything exists
#   two_model_pool.sh standup       ONE standup, two stacks, one gateway
#   two_model_pool.sh verify        both models answer; two EPPs; pool state
#   two_model_pool.sh pool-create   the shared warm pool
#   two_model_pool.sh pool-delete   remove it
#   two_model_pool.sh warm          pin BOTH models resident, and wait
#   two_model_pool.sh reset         both models back to MIN_REPLICAS, quiesced
#   two_model_pool.sh run <arm>     drive the load, sample GPUs, collect
#   two_model_pool.sh report        compare the arms
#   two_model_pool.sh status        what exists, and what holds accelerators
#   two_model_pool.sh teardown      remove everything this created
#
# Env: BENCHMARK_NAMESPACE (required); STACK_A/STACK_B and MODEL_A/MODEL_B must
# match the scenario spec; the load shape below. KUBECONFIG/KUBE_CONTEXT as
# usual.
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

NS="${BENCHMARK_NAMESPACE:-}"
# The scenario spec defines both stacks. Its stack NAMES are the path prefixes
# the shared HTTPRoute routes on (`/{stack}/v1/...`), and its per-stack
# `model.shortName` is what every object in that stack is named after -- both
# pinned there so nothing here has to reproduce the harness's name hash.
BENCH_SPEC="${BENCH_SPEC:-guides/two-model-warm-pool}"
STACK_A="${STACK_A:-llama-31-8b}"
STACK_B="${STACK_B:-qwen3-8b}"
MODEL_A="${MODEL_A:-unsloth/Meta-Llama-3.1-8B-Instruct}"
MODEL_B="${MODEL_B:-Qwen/Qwen3-8B}"

# The load shape. Defaults give a ~34 minute run: 2 min lead-in, then four
# 8-minute phases.
#
# A PHASE MUST OUTLAST SCALE-DOWN STABILIZATION, which this repo sets to 300s
# (fast up, slow down -- under-provisioning costs TTFT irrecoverably, over-
# provisioning only costs money). At 360s, model B still holds the replicas it
# grew during its own burst for most of model A's rise, so A never actually
# competes for capacity and the anti-phase premise is not exercised. 480s leaves
# ~3 minutes of genuinely contended time per phase.
PHASE_SECONDS="${PHASE_SECONDS:-480}"
CYCLES="${CYCLES:-2}"
LEAD_IN="${LEAD_IN:-120}"
LOW_RPS="${LOW_RPS:-2}"
HIGH_RPS="${HIGH_RPS:-12}"
INPUT_TOKENS="${INPUT_TOKENS:-1000}"
OUTPUT_TOKENS="${OUTPUT_TOKENS:-200}"
REQUEST_TIMEOUT="${REQUEST_TIMEOUT:-300}"
SEED="${SEED:-1729}"

MAX_REPLICAS="${MAX_REPLICAS:-3}"
MIN_REPLICAS="${MIN_REPLICAS:-1}"
GPUS_PER_REPLICA="${GPUS_PER_REPLICA:-1}"
POOL_NAME="${POOL_NAME:-twomodel}"
POOL_REPLICAS="${POOL_REPLICAS:-2}"
# A pool with replicas == reserve can never warm anything: the reserve is what
# it refuses to lend.
POOL_RESERVE="${POOL_RESERVE:-1}"
GPU_SAMPLE_SECONDS="${GPU_SAMPLE_SECONDS:-5}"
OUT_ROOT="${OUT_ROOT:-$ROOT/two-model-results}"
LOAD_IMAGE="${LOAD_IMAGE:-}"
CACHE_CLAIM="${CACHE_CLAIM:-model-pvc}"
WVA_NS="${WVA_NS:-$NS}"
MONITORING_NS="${MONITORING_NS:-}"
ACCELERATOR="${ACCELERATOR:-}"
RESET_TIMEOUT="${RESET_TIMEOUT:-600}"

BLUE=$'\033[0;34m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; RED=$'\033[0;31m'; NC=$'\033[0m'
info()  { echo "${BLUE}[INFO]${NC} $*" >&2; }
ok()    { echo "${GREEN}[OK]${NC} $*" >&2; }
warn()  { echo "${YELLOW}[WARN]${NC} $*" >&2; }
# EXITS. Never call it from inside `$( )` -- the exit ends the subshell and the
# caller carries on with an empty string. hack/check-refusals.sh enforces that.
die()   { echo "${RED}[ERROR]${NC} $*" >&2; exit 1; }

k()  { kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} -n "$NS" "$@"; }
kc() { kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} "$@"; }

need_ns() { [ -n "$NS" ] || die "BENCHMARK_NAMESPACE is required."; }

# The accelerators this run peaks at, and it is the SAME in both arms by
# construction -- that is the point.
#
#   nopool: 2 models x MAX_REPLICAS
#   pool:   2 models x (MAX_REPLICAS - the pool's share) + the pool itself
#
# "Insurance lowers your maximum fleet by N" is the whole cost argument for a
# warm pool, so an arm that holds the pool AND the same model ceiling is not
# the pool being compared -- it is a bigger cluster being compared.
pool_share_per_model() { echo $(( (POOL_REPLICAS + 1) / 2 )); }

peak_gpus() { echo $(( 2 * MAX_REPLICAS * GPUS_PER_REPLICA )); }

arm_max_replicas() {
    case "$1" in
        pool)
            local m=$(( MAX_REPLICAS - $(pool_share_per_model) ))
            [ "$m" -lt "$MIN_REPLICAS" ] && m="$MIN_REPLICAS"
            echo "$m" ;;
        *) echo "$MAX_REPLICAS" ;;
    esac
}

# ---------------------------------------------------------------------------
# preflight
# ---------------------------------------------------------------------------
verb_preflight() {
    need_ns
    local rc=0
    for t in kubectl jq python3; do
        command -v "$t" >/dev/null 2>&1 || { warn "$t is not on PATH"; rc=1; }
    done
    kc get ns "$NS" >/dev/null 2>&1 || { warn "namespace $NS does not exist"; rc=1; }

    # FREE accelerators, on nodes that could actually take a Pod. Counting
    # cordoned or NotReady nodes overstates it, and the run then spends its
    # bursts Pending -- which measures the scheduler, identically in both arms,
    # and hides whatever the pool did.
    local total used free want
    total="$(kc get nodes -o json 2>/dev/null | jq '
        [.items[]
         | select(.spec.unschedulable != true)
         | select([.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length > 0)
         | .status.allocatable["nvidia.com/gpu"] // "0" | tonumber] | add // 0')"
    used="$(kc get pods -A -o json 2>/dev/null | jq '
        [.items[] | select(.status.phase=="Running" or .status.phase=="Pending")
         | .spec.containers[].resources.requests["nvidia.com/gpu"] // "0" | tonumber] | add // 0')"
    free=$(( ${total:-0} - ${used:-0} ))
    want="$(peak_gpus)"
    info "accelerators: ${total:-?} schedulable, ${used:-?} requested, ${free} free; this run peaks at ${want}"
    info "  nopool arm: 2 models x ${MAX_REPLICAS} replicas"
    info "  pool arm:   2 models x $(arm_max_replicas pool) replicas + a ${POOL_REPLICAS}-Pod pool -- the same peak, which is what makes the arms comparable"
    if [ "$free" -lt "$want" ]; then
        warn "only ${free} free and the run peaks at ${want}. Lower MAX_REPLICAS, or wait."
        rc=1
    fi
    if [ "$(arm_max_replicas pool)" -le "$MIN_REPLICAS" ]; then
        warn "with MAX_REPLICAS=$MAX_REPLICAS and a ${POOL_REPLICAS}-Pod pool, the pool arm's ceiling"
        warn "  collapses to the floor ($MIN_REPLICAS): that arm cannot scale at all, so the run would"
        warn "  compare autoscaling against a pinned fleet. Raise MAX_REPLICAS to at least $(( MIN_REPLICAS + $(pool_share_per_model) + 1 ))."
        rc=1
    fi

    # ONE accelerator kind. A warm copy is only reusable on the accelerator it
    # was loaded on, so a pool pinned to the wrong product is never eligible to
    # lend -- and nothing fails: the run completes and reports that the pool did
    # nothing. Picking `[0]` of several was exactly that silent mismatch.
    local products count
    products="$(kc get nodes -o json 2>/dev/null | jq -r '
        [.items[].metadata.labels
         | (."nvidia.com/gpu.product", ."gpu.nvidia.com/model", ."gpu.nvidia.com/class",
            ."cloud.google.com/gke-accelerator", ."eks.amazonaws.com/instance-gpu-name")
         | select(. != null and . != "")] | unique | .[]')"
    count="$(printf '%s\n' "$products" | grep -c . || true)"
    if [ -n "$ACCELERATOR" ]; then
        info "accelerator: $ACCELERATOR (from ACCELERATOR)"
    elif [ "${count:-0}" -eq 1 ]; then
        info "accelerator: $products"
    elif [ "${count:-0}" -eq 0 ]; then
        warn "no node advertises an accelerator product label this knows; set ACCELERATOR=<product> or the pool lands anywhere"
        rc=1
    else
        warn "this cluster advertises ${count} accelerator products:"
        printf '%s\n' "$products" | sed 's/^/      /' >&2
        warn "  A pool serves ONE of them. Set ACCELERATOR=<product> so the pool and the models agree."
        rc=1
    fi

    # The model cache. A pool Pod loads its warm copies through the SAME claim
    # the models use; pointed at a claim they do not use, the engine gets a
    # --model path that is not in the Pod, never answers, and the controller
    # waits out its whole admission timeout before reporting only that the port
    # did not respond.
    if k get pvc "$CACHE_CLAIM" >/dev/null 2>&1; then
        ok "model cache claim $CACHE_CLAIM exists"
    else
        warn "no PVC named $CACHE_CLAIM in $NS -- it is created by standup; run this again after"
    fi

    # The phase has to outlast scale-down stabilization or the premise fails.
    if [ "$PHASE_SECONDS" -lt 420 ]; then
        warn "PHASE_SECONDS=$PHASE_SECONDS is shorter than scale-down stabilization (300s) plus a ramp."
        warn "  A model then holds the replicas it grew through most of the OTHER model's rise, so"
        warn "  the two never compete and the anti-phase premise is not exercised."
        rc=1
    fi

    [ "$rc" -eq 0 ] && ok "preflight passed" || warn "preflight found problems (above)"
    return "$rc"
}

# ---------------------------------------------------------------------------
# standup -- ONE standup, two stacks
#
# The harness supports this directly: N stacks behind ONE gateway, each with its
# own EPP, InferencePool and decode Deployment, multiplexed by a shared
# HTTPRoute on a path prefix per stack.
#
# Two SEPARATE standups was the first design and it was wrong twice over: the
# single-model guide uses `gateway.className: epponly`, which deploys no Gateway
# at all and which the renderer REFUSES for a multi-stack scenario -- so there
# would have been no shared address to drive, and whatever the driver found
# would have been one model's stack answering for both.
# ---------------------------------------------------------------------------
verb_standup() {
    need_ns
    [ -f "$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml" ] || \
        die "no scenario at hack/benchmark/scenarios/$BENCH_SPEC.yaml"
    # BOTH halves. `--spec` resolves a SPECIFICATION
    # (config/specification/<name>.yaml.j2) and never a scenario, so a scenario
    # shipped without one is invisible to the CLI: it reports
    # "Specification '<name>' not found" and prints the list it does know, which
    # reads like a missing file while the scenario sits in the clone. Measured,
    # on the first run of this standup.
    [ -f "$ROOT/hack/benchmark/scenarios/$BENCH_SPEC.yaml.j2" ] || \
        die "no specification at hack/benchmark/scenarios/$BENCH_SPEC.yaml.j2.
    The scenario exists, but --spec resolves the SPECIFICATION, so the CLI cannot see it."
    # RE-RENDERING OUR OWN STACKS IS FINE; clobbering someone else's is not.
    #
    # The standup refuses outright when it finds an EPP, because a second
    # standup applies its own EPP, Service, InferencePool and PodMonitor over
    # whatever is there and the pods keep answering -- silent damage to a stack
    # somebody may be using. But this scenario is edited and re-run (an EPP
    # feature gate, a replica count), and every re-run trips that guard.
    #
    # So the override is taken only when the namespace's shared route already
    # carries BOTH of this scenario's stacks -- which is what "these EPPs are
    # ours" looks like from outside. Anything else is left to the guard.
    local allow_reuse=false epps
    epps="$(k get deploy -o name 2>/dev/null | grep -c -- '-epp' || true)"
    if [ "${epps:-0}" -gt 0 ]; then
        if [ -n "$(stack_backend "$STACK_A")" ] && [ -n "$(stack_backend "$STACK_B")" ]; then
            allow_reuse=true
            info "re-rendering: the route already carries $STACK_A and $STACK_B, so the ${epps} EPP(s) here are this scenario's"
        else
            die "$NS already runs ${epps} EPP(s), and the shared route does not carry both of this scenario's stacks -- so they belong to something else. A standup would apply its own EPP, Service, InferencePool and PodMonitor over them, silently. Use a clean namespace."
        fi
    fi

    info "standing up both stacks from $BENCH_SPEC"
    ( cd "$ROOT" && \
        BENCHMARK_ALLOW_EPP_REUSE="$allow_reuse" \
        BENCHMARK_NAMESPACE="$NS" \
        BENCHMARK_SPEC="$BENCH_SPEC" \
        BENCHMARK_MODEL_ID= \
        BENCHMARK_DECODE_REPLICAS="$MIN_REPLICAS" \
        BENCHMARK_KEDA_MIN_REPLICAS="$MIN_REPLICAS" \
        BENCHMARK_KEDA_MAX_REPLICAS="$MAX_REPLICAS" \
        BENCHMARK_SKIP_SMOKETEST=true \
        make --no-print-directory benchmark-standup ) || die "standup failed"
    # The smoketest is skipped because it cannot pass here -- its readiness poll
    # asks the gateway ROOT for /v1/models, which a path-prefixed multi-model
    # stack does not route, so it 404s for its whole 1800s timeout while both
    # models answer at their own prefixes. `verify` does the same job correctly:
    # it asks each model on the path the load will actually use.
    info "the standup's own smoketest was skipped (it cannot see a path-prefixed stack); 'verify' is the check that replaces it"
    verb_status
    ok "stacks stood up. Run 'verify' before any load."
}

# ---------------------------------------------------------------------------
# endpoints -- one gateway, one path per stack
# ---------------------------------------------------------------------------
gateway_host() {
    # REFUSES on ambiguity rather than taking the first. Two gateway Services in
    # one namespace means something else is deployed here, and driving the wrong
    # one produces a run of 404s that reads as a pool result.
    #
    # The HTTP port BY NAME, never ports[0]. Measured on CoreWeave: the istio
    # gateway Service lists 15021 (status-port) first and 80 second, so ports[0]
    # sent every request at the readiness port -- which answers, with 404, for
    # the whole run.
    local svcs count
    svcs="$(k get svc -o json 2>/dev/null | jq -r '
        [.items[] | select(.metadata.name | test("inference-gateway|-gateway$"))
         | .metadata.name as $n
         | (.spec.ports[] | select(.name == "http" or .name == "http2" or .port == 80) | .port) as $p
         | $n + ":" + ($p|tostring)] | unique | .[]')"
    count="$(printf '%s\n' "$svcs" | grep -c . || true)"
    [ "${count:-0}" -eq 1 ] || return 1
    printf '%s' "$svcs"
}

# The stack's backend, read from the HTTPRoute the gateway actually routes on.
#
# NOT from the scenario's `model.shortName`: the harness IGNORES that and
# generates `{first8}-{sha256(namespace/model)[:8]}-{last8}` regardless --
# measured, `unsloth/Meta-Llama-3.1-8B-Instruct` in this namespace became
# `unsloth--244120d9-instruct`. Matching the stack name against Deployment names
# therefore finds nothing, silently, and `verify` would report a stack that is
# plainly serving as absent.
#
# The route is also the RIGHT source: it is the same table the load generator's
# requests follow, so what this resolves and what the run measures cannot drift.
stack_backend() {
    k get httproute -o json 2>/dev/null | jq -r --arg p "/$1" '
        [.items[].spec.rules[]?
         | select([.matches[]?.path.value] | index($p))
         | .backendRefs[0].name] | .[0] // ""'
}

decode_deploy_for() {
    local backend prefix
    backend="$(stack_backend "$1")"
    [ -n "$backend" ] || return 0
    # The modelservice chart names the pool `<shortName>-router` and the
    # Deployment `<shortName>-decode`.
    prefix="${backend%-router}"
    k get deploy "${prefix}-decode" -o name >/dev/null 2>&1 && printf '%s' "${prefix}-decode"
}

endpoint_for_stack() {
    local hostport="$1" stack="$2"
    printf 'http://%s.%s.svc.cluster.local:%s/%s/v1/completions' \
        "${hostport%%:*}" "$NS" "${hostport##*:}" "$stack"
}

verb_verify() {
    need_ns
    local hostport
    if ! hostport="$(gateway_host)"; then
        die "did not find exactly one inference gateway Service in $NS. Both models are reached through one; driving the wrong one is a run of 404s that reads as a result.
    $(k get svc -o name 2>/dev/null | sed 's/^/      /')"
    fi
    info "gateway: $hostport"

    local rc=0 d ready
    for s in "$STACK_A" "$STACK_B"; do
        d="$(decode_deploy_for "$s")"
        if [ -z "$d" ]; then
            warn "no decode Deployment matching stack '$s'"
            rc=1
            continue
        fi
        ready="$(k get deploy "$d" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
        info "  $s -> $d (ready: ${ready:-0})"
        [ "${ready:-0}" -ge 1 ] || { warn "  $s has no ready replica"; rc=1; }
    done
    [ "$rc" -eq 0 ] || die "both stacks must be serving before anything is measured"

    # Two EPPs, which is the topology this scenario is about. One EPP serving
    # both pools would still run, and the run would be measuring something else.
    local epps
    epps="$(k get deploy -o name 2>/dev/null | grep -c -- '-epp' || true)"
    [ "${epps:-0}" -ge 2 ] || \
        die "expected an EPP per model and found ${epps:-0}. The standup rendered one stack, not two."
    ok "$epps EPP deployments, one per model"

    # A real request per model, through its own path, before half an hour of
    # load. A 404 here is a name; a 404 at minute 12 is a wasted run.
    local probe_rc=0
    verify_probe "$(endpoint_for_stack "$hostport" "$STACK_A")" "$MODEL_A" || probe_rc=1
    verify_probe "$(endpoint_for_stack "$hostport" "$STACK_B")" "$MODEL_B" || probe_rc=1
    [ "$probe_rc" -eq 0 ] || die "at least one model did not answer a single request"

    # IS THE CONTROLLER ACTUALLY READING? This is the failure that produces two
    # flat runs which agree perfectly and mean nothing: a Prometheus that does
    # not scrape this namespace leaves every variant looking idle, so NEITHER
    # arm ever scales and the report compares two straight lines. Nothing else
    # in this scenario would notice -- the stacks serve, the pool lends nothing
    # because nothing ever asks, and both arms finish.
    #
    # Warned, not fatal: a controller that is still starting is normal at this
    # point, and refusing here would block a standup that is merely young.
    local wva_ready
    wva_ready="$(k get deploy -l app.kubernetes.io/name=workload-variant-autoscaler \
        -o jsonpath='{.items[*].status.readyReplicas}' 2>/dev/null)"
    if [ -z "${wva_ready:-}" ] || [ "${wva_ready:-0}" = "0" ]; then
        warn "no READY WVA controller in $NS. Nothing will scale, and both arms would be flat."
    else
        ok "WVA controller ready"
    fi
    local not_ready
    not_ready="$(k get scaledobject -o json 2>/dev/null | jq -r '
        [.items[]
         | select([.spec.triggers[]?.metadata.warmPoolName] | all(. == null))
         | select([.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length == 0)
         | .metadata.name + " (" + ([.status.conditions[]? | select(.type=="Ready") | .message // "no Ready condition"] | join("; ")) + ")"]
        | .[]' 2>/dev/null)"
    if [ -n "$not_ready" ]; then
        warn "ScaledObjects that are not Ready -- these models cannot scale, so the run would compare two flat lines:"
        printf '%s\n' "$not_ready" | sed 's/^/      /' >&2
    else
        ok "every model ScaledObject reports Ready"
    fi
    local errs
    errs="$(k logs -l app.kubernetes.io/name=workload-variant-autoscaler --tail=400 2>/dev/null \
        | grep -iE 'prometheus|metrics' | grep -iE 'error|refused|denied|timeout|no such host' | tail -5)"
    if [ -n "$errs" ]; then
        warn "the controller is logging metric-read errors; it may be pointed at a Prometheus that does not scrape this namespace:"
        printf '%s\n' "$errs" | sed 's/^/      /' >&2
    fi

    if k get deploy "wva-warm-pool-$POOL_NAME" >/dev/null 2>&1; then
        info "pool '$POOL_NAME':"
        k get pods -l "llm-d.ai/warm-pool=$POOL_NAME" -o wide 2>/dev/null | sed 's/^/    /' >&2
    else
        info "no pool present -- this is the 'nopool' arm"
    fi
    ok "verify passed"
}

# One real request, from a Pod, RETRIED.
#
# Not `kubectl run --rm -i`: that ties the result to an attach, and on this
# cluster it fails two ways that have nothing to do with the model --
# "timed out waiting for the condition" when the attach loses the race with the
# container, and "Temporary failure in name resolution" when the Pod's DNS is
# not up yet (a service mesh adds a sidecar the application does not wait for).
# Both were reported as "the model did NOT answer", about a model that was
# answering when asked again a second later.
#
# So: create, wait for it to FINISH, read its log, delete. And try more than
# once, because the first attempt after a rollout legitimately loses that race.
verify_probe() {
    local url="$1" model="$2" out pod attempt rc
    info "probing $model at $url"
    for attempt in 1 2 3; do
        pod="probe-$(date +%s)-$RANDOM"
        k run "$pod" --restart=Never --quiet --image="$(load_image)" --command -- \
            python3 -c "
import json,time,urllib.request
# Retried INSIDE the Pod. A fresh Pod here cannot resolve DNS for the first
# half-minute or so -- a mesh sidecar starts alongside the container and the
# application does not wait for it -- and one attempt reports a serving model as
# dead. Measured: attempts 1 and 2 failed to resolve, attempt 3 returned 200.
body=json.dumps({'model':'$model','prompt':'hello','max_tokens':4}).encode()
deadline=time.time()+180
last=''
while time.time()<deadline:
    try:
        req=urllib.request.Request('$url', data=body, headers={'Content-Type':'application/json'})
        r=urllib.request.urlopen(req, timeout=120)
        print('HTTP', r.status)
        break
    except Exception as e:
        last=str(e)
        time.sleep(5)
else:
    print('FAILED', last)
" >/dev/null 2>&1 || true
        # A Pod that runs for a second is never observed Ready, so wait on the
        # phase instead of on a condition.
        local waited=0
        while [ "$waited" -lt 180 ]; do
            case "$(k get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" in
                Succeeded|Failed) break ;;
            esac
            sleep 5
            waited=$(( waited + 5 ))
        done
        out="$(k logs "$pod" 2>&1)"
        k delete pod "$pod" --ignore-not-found --wait=false >/dev/null 2>&1
        case "$out" in
            *"HTTP 200"*) ok "  $model answered (attempt $attempt)"; return 0 ;;
            *"name resolution"*|*"timed out waiting"*|"")
                warn "  attempt $attempt could not reach the network from the probe Pod (${out:-no output}); retrying"
                sleep 10
                continue ;;
            *) warn "  $model did NOT answer: $out"; return 1 ;;
        esac
    done
    warn "  $model: three probe Pods failed to reach the network. That is the probe, not the model -- check DNS/mesh readiness in $NS."
    return 1
}

# The image the load Pod runs, resolved from a Deployment already in the
# namespace so the nodes have pulled it -- a multi-GB pull at the start of a
# timed run puts the registry inside the measurement.
load_image() {
    if [ -n "$LOAD_IMAGE" ]; then printf '%s' "$LOAD_IMAGE"; return 0; fi
    local img
    img="$(k get deploy -o json 2>/dev/null | jq -r '
        [.items[] | select(.metadata.name|test("epp|decode"))
         | .spec.template.spec.containers[0].image] | .[0] // ""')"
    [ -n "$img" ] || { printf 'ghcr.io/llm-d/llm-d-benchmark:v0.7.8'; return 0; }
    printf '%s' "$img"
}

# ---------------------------------------------------------------------------
# the pool
# ---------------------------------------------------------------------------
verb_pool_create() {
    need_ns
    local accel="$ACCELERATOR"
    if [ -z "$accel" ]; then
        accel="$(kc get nodes -o json 2>/dev/null | jq -r '
            [.items[].metadata.labels
             | (."nvidia.com/gpu.product", ."gpu.nvidia.com/model")
             | select(. != null and . != "")] | unique | if length == 1 then .[0] else "" end')"
    fi
    [ -n "$accel" ] || die "set ACCELERATOR=<product as the node label spells it>: this cluster does not advertise exactly one, and a pool on the wrong accelerator is never eligible to lend."
    info "creating pool '$POOL_NAME': ${POOL_REPLICAS} Pods, reserve ${POOL_RESERVE}, ${GPUS_PER_REPLICA} GPU each, on ${accel}"
    # --max EQUAL to --replicas: the pool's own ScaledObject must not resize it
    # during a run, or the pool's accelerators move under the measurement and
    # the arm's GPU-seconds stop meaning "what the insurance cost".
    # --models 2 --model-size 8B sizes the Pod's memory limit, which IS the
    # warm-set budget: it decides how many models a Pod can hold, and it is what
    # lets one pool serve both of these.
    "$ROOT/deploy/warmpool.sh" create \
        -n "$NS" \
        --name "$POOL_NAME" \
        --type bridge \
        --replicas "$POOL_REPLICAS" \
        --max "$POOL_REPLICAS" \
        --reserve "$POOL_RESERVE" \
        --gpus "$GPUS_PER_REPLICA" \
        --models 2 \
        --model-size 8B \
        --cache-claim "$CACHE_CLAIM" \
        --wva-namespace "$WVA_NS" \
        --accelerator "$accel" \
        ${MONITORING_NS:+--monitoring-namespace "$MONITORING_NS"} \
        || die "pool creation failed"
    ok "pool created. An idle pool Pod reports NotReady ON PURPOSE -- that is what keeps it out of the InferencePool. Do not wait on readyReplicas."
}

verb_pool_delete() {
    need_ns
    if ! k get deploy "wva-warm-pool-$POOL_NAME" >/dev/null 2>&1; then
        info "no pool named $POOL_NAME in $NS"
        return 0
    fi
    "$ROOT/deploy/warmpool.sh" delete -n "$NS" --name "$POOL_NAME" || die "pool deletion failed"
    ok "pool deleted"
}

# ---------------------------------------------------------------------------
# warm -- BOTH models resident before the load starts
# ---------------------------------------------------------------------------
scaledobject_for_model() {
    k get scaledobject -o json 2>/dev/null | jq -r --arg m "$1" '
        [.items[] | select([.spec.triggers[]?.metadata.modelID] | index($m)) | .metadata.name] | .[0] // ""'
}

known_model_ids() {
    k get scaledobject -o json 2>/dev/null | jq -r '
        [.items[].spec.triggers[]?.metadata.modelID // empty] | unique | join(", ")'
}

pin_warm_copy() {
    local model="$1" so patch
    so="$(scaledobject_for_model "$model")"
    if [ -z "$so" ]; then
        warn "no ScaledObject carries modelID=$model, so nothing declares it to WVA."
        warn "  The modelIDs that DO exist here: $(known_model_ids)"
        return 1
    fi
    # The WHOLE trigger list is sent: a merge patch replaces a list rather than
    # merging into it, so patching one element would drop the others -- the
    # scaler address among them, which is what makes the model exist to WVA.
    patch="$(k get scaledobject "$so" -o json 2>/dev/null | jq -c --arg m "$model" --arg p "$POOL_NAME" '
        {spec: {triggers: [ .spec.triggers[]
            | if .metadata.modelID == $m
              then .metadata += {"warmPoolCopies": "1", "warmPool": $p}
              else . end ]}}')"
    [ -n "$patch" ] || { warn "could not build a trigger patch for $so"; return 1; }
    k patch scaledobject "$so" --type=merge -p "$patch" >/dev/null || {
        warn "could not patch $so"
        return 1
    }
    ok "  $model: pinned one warm copy in pool '$POOL_NAME' (ScaledObject $so)"
}

pool_pods() {
    k get pods -l "llm-d.ai/warm-pool=$POOL_NAME" \
        -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null
}

# Asks each pool Pod's supervisor what it is holding, over LOOPBACK from inside
# the Pod.
#
# Not from a probe Pod: the pool's own NetworkPolicy admits :8001 only from the
# WVA controller, and deliberately so -- deploy/warmpool.sh says a bare
# podSelector "would put :8001 one kubectl run away from anyone who can create a
# Pod here". A probe Pod therefore hangs until its timeout and reports a warm
# pool as cold. `exec` into the container that already owns the port has no such
# problem, and that container runs `python3 /app/launcher.py`, so python3 is
# there by construction.
pool_residency() {
    local pod out
    for pod in $(pool_pods); do
        out="$(k exec "$pod" -c inference-server -- python3 -c "
import urllib.request
try:
    print(urllib.request.urlopen('http://127.0.0.1:8001/v2/vllm/instances', timeout=10).read().decode('utf-8','replace'))
except Exception as e:
    print('ERR', e)
" 2>/dev/null)"
        printf '%s\n' "$pod: $out"
    done
}

wait_resident() {
    local timeout="${1:-900}" pods deadline blob have_a have_b
    pods="$(pool_pods)"
    if [ -z "$pods" ]; then
        warn "pool '$POOL_NAME' has no Pods"
        return 1
    fi
    deadline=$(( $(date +%s) + timeout ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        blob="$(pool_residency)"
        have_a=0; have_b=0
        # Matched on the MODEL ID and on the stack short name, because the
        # supervisor keys instances on the variant and which of the two spellings
        # that is has never been checked against a live pool. Whichever matches,
        # matches; when neither does, the raw payload is printed rather than a
        # bare timeout, so the next run can be told what the identity really is.
        case "$blob" in *"$MODEL_A"*|*"$STACK_A"*) have_a=1 ;; esac
        case "$blob" in *"$MODEL_B"*|*"$STACK_B"*) have_b=1 ;; esac
        info "  resident: A=$have_a B=$have_b"
        if [ "$have_a" = 1 ] && [ "$have_b" = 1 ]; then
            return 0
        fi
        sleep 10
    done
    warn "timed out. What the supervisors actually reported:"
    printf '%s\n' "$blob" | sed 's/^/      /' >&2
    return 1
}

verb_warm() {
    need_ns
    k get deploy "wva-warm-pool-$POOL_NAME" >/dev/null 2>&1 || \
        die "no pool named $POOL_NAME. Run pool-create first -- there is nothing to warm."
    local rc=0
    pin_warm_copy "$MODEL_A" || rc=1
    pin_warm_copy "$MODEL_B" || rc=1
    [ "$rc" -eq 0 ] || die "could not pin a warm copy for both models; the pool would rank one of them out exactly when its burst arrives."
    if [ "${SKIP_WARM_GATE:-0}" = "1" ]; then
        warn "SKIP_WARM_GATE=1: not waiting for residency. The arm will measure whatever the pool happens to hold."
        return 0
    fi
    wait_resident "${WARM_TIMEOUT:-900}" || \
        die "the pool did not become resident in both models. Starting the pool arm now would measure a COLD pool -- the first burst paying a model load INTO the pool on top of the replica's own -- and report it as the pool's cost."
    ok "both models are resident. The pool arm can start."
}

# ---------------------------------------------------------------------------
# reset -- the fleet back to the floor, before every arm
# ---------------------------------------------------------------------------
pause_at() {
    # KEDA's documented pin. Scaling the Deployment directly does not hold: the
    # HPA KEDA generates puts it back within a cycle.
    local so="$1" n="$2"
    k annotate scaledobject "$so" "autoscaling.keda.sh/paused-replicas=$n" --overwrite >/dev/null
}

unpause() {
    k annotate scaledobject "$1" "autoscaling.keda.sh/paused-replicas-" >/dev/null 2>&1 || true
}

verb_reset() {
    need_ns
    local sos d n waited
    sos="$(k get scaledobject -o json 2>/dev/null | jq -r '
        [.items[] | select([.spec.triggers[]?.metadata.warmPoolName] | all(. == null)) | .metadata.name] | .[]')"
    [ -n "$sos" ] || die "no model ScaledObjects in $NS; there is nothing to reset."
    for so in $sos; do
        pause_at "$so" "$MIN_REPLICAS" || warn "could not pin $so"
    done
    info "pinned every model ScaledObject at $MIN_REPLICAS; waiting for the fleet to settle"
    waited=0
    while [ "$waited" -lt "$RESET_TIMEOUT" ]; do
        local settled=1
        for s in "$STACK_A" "$STACK_B"; do
            d="$(decode_deploy_for "$s")"
            [ -n "$d" ] || { settled=0; continue; }
            n="$(k get deploy "$d" -o jsonpath='{.status.replicas}' 2>/dev/null)"
            local ready; ready="$(k get deploy "$d" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
            [ "${n:-0}" = "$MIN_REPLICAS" ] && [ "${ready:-0}" = "$MIN_REPLICAS" ] || settled=0
        done
        if [ "$settled" = 1 ]; then
            for so in $sos; do unpause "$so"; done
            ok "both models are at $MIN_REPLICAS and ready; autoscaling released"
            return 0
        fi
        sleep 10
        waited=$(( waited + 10 ))
    done
    for so in $sos; do unpause "$so"; done
    die "the fleet did not settle at $MIN_REPLICAS within ${RESET_TIMEOUT}s. Starting an arm from a fleet the previous arm left scaled makes the two incomparable."
}

set_arm_ceiling() {
    local arm="$1" ceiling sos
    ceiling="$(arm_max_replicas "$arm")"
    sos="$(k get scaledobject -o json 2>/dev/null | jq -r '
        [.items[] | select([.spec.triggers[]?.metadata.warmPoolName] | all(. == null)) | .metadata.name] | .[]')"
    for so in $sos; do
        k patch scaledobject "$so" --type=merge \
            -p "{\"spec\":{\"maxReplicaCount\":$ceiling}}" >/dev/null || \
            warn "could not set maxReplicaCount on $so"
    done
    # The pool's Pods only exist in the pool arm, and saying otherwise here
    # misreports the one number that makes the arms comparable.
    local held=0
    [ "$arm" = "pool" ] && held="$POOL_REPLICAS"
    info "arm '$arm': each model may reach ${ceiling} replicas; the pool holds ${held} Pod(s) -- ceiling $(( 2 * ceiling * GPUS_PER_REPLICA + held * GPUS_PER_REPLICA )) accelerators"
    echo "$ceiling"
}

# ---------------------------------------------------------------------------
# run
# ---------------------------------------------------------------------------
sample_gpus() {
    # Every RUNNING Pod that holds an accelerator, with the labels that say what
    # it is doing. Pending Pods are excluded: they have requested an accelerator
    # and hold none, so counting them charges an arm for capacity it never got
    # -- and the arm that queues more is the one that would be charged.
    #
    # A pool Pod that has been LENT carries the borrowing model's labels, so
    # lend and return are visible here without any metrics plumbing, which
    # matters on a cluster where Prometheus may not be reachable from this shell.
    local out="$1" until_ts="$2"
    : > "$out"
    while [ "$(date +%s)" -lt "$until_ts" ]; do
        k get pods -o json 2>/dev/null | jq -c --argjson now "$(date +%s)" '
          {ts: $now,
           pods: [ .items[]
             | select(.status.phase=="Running")
             | {name: .metadata.name,
                ready: ([.status.containerStatuses[]?.ready] | all),
                node: (.spec.nodeName // ""),
                gpus: ([.spec.containers[].resources.requests["nvidia.com/gpu"]//"0"|tonumber]|add),
                model: (.metadata.labels["llm-d.ai/model"] // ""),
                pool:  (.metadata.labels["llm-d.ai/warm-pool"] // ""),
                serving: (.metadata.labels["llm-d.ai/inferenceServing"] // "")}
             | select(.gpus > 0) ]}' >> "$out" 2>/dev/null || true
        sleep "$GPU_SAMPLE_SECONDS"
    done
}

RUN_JOB=""
RUN_SAMPLER=""
run_cleanup() {
    # The JOB is the leak that matters: left behind it drives 12 rps at the
    # models' ceiling for the rest of the schedule, on a shared cluster, with
    # nobody watching. The sampler is a polling loop against the API server.
    [ -n "$RUN_SAMPLER" ] && kill "$RUN_SAMPLER" 2>/dev/null
    if [ -n "$RUN_JOB" ]; then
        kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} -n "$NS" \
            delete job "$RUN_JOB" --ignore-not-found >/dev/null 2>&1
    fi
}
run_interrupted() {
    warn "interrupted -- deleting the load Job and stopping the sampler"
    run_cleanup
    exit 130
}

verb_run() {
    need_ns
    local arm="${1:-}"
    case "$arm" in
        pool|nopool) ;;
        *) die "run takes an arm: pool or nopool (got '${arm:-<none>}')" ;;
    esac
    local have_pool=0
    k get deploy "wva-warm-pool-$POOL_NAME" >/dev/null 2>&1 && have_pool=1
    if [ "$arm" = "pool" ] && [ "$have_pool" -eq 0 ]; then
        die "arm 'pool' but no pool named $POOL_NAME exists. Run pool-create first."
    fi
    if [ "$arm" = "nopool" ] && [ "$have_pool" -eq 1 ]; then
        die "arm 'nopool' but pool $POOL_NAME is present and holding accelerators. Run pool-delete first."
    fi
    # A pool arm on a COLD pool is the worst result this scenario can produce:
    # it runs to completion, produces a full table, and reports the cost of a
    # pool nobody would operate that way. Re-checked here rather than trusted
    # from an earlier `warm`, because the pool can lose a copy in between.
    if [ "$arm" = "pool" ] && [ "${SKIP_WARM_GATE:-0}" != "1" ]; then
        wait_resident "${WARM_GATE_TIMEOUT:-120}" || \
            die "the pool is not resident in both models, so this arm would measure a cold pool. Run 'warm' first, or set SKIP_WARM_GATE=1 if a cold pool is deliberately what you are measuring."
    fi

    local hostport
    hostport="$(gateway_host)" || die "did not find exactly one inference gateway Service in $NS"
    local url_a url_b
    url_a="$(endpoint_for_stack "$hostport" "$STACK_A")"
    url_b="$(endpoint_for_stack "$hostport" "$STACK_B")"

    local ceiling; ceiling="$(set_arm_ceiling "$arm")"

    local out_dir="$OUT_ROOT/$arm"
    mkdir -p "$out_dir"
    printf '{"arm":"%s","max_replicas_per_model":%s,"pool_replicas":%s,"gpus_per_replica":%s}\n' \
        "$arm" "$ceiling" "$([ "$arm" = pool ] && echo "$POOL_REPLICAS" || echo 0)" "$GPUS_PER_REPLICA" \
        > "$out_dir/budget.json"

    local total=$(( LEAD_IN + 2 * CYCLES * PHASE_SECONDS ))
    info "arm=$arm  ${total}s of load"

    # The generator goes in as a ConfigMap rather than an image: it is standard
    # library, and building and pushing an image would put a registry between an
    # edit and a run.
    k delete configmap wva-two-model-load --ignore-not-found >/dev/null 2>&1
    k create configmap wva-two-model-load --from-file=two_model_load.py="$HERE/two_model_load.py" >/dev/null \
        || die "could not create the loader ConfigMap"

    RUN_JOB="wva-two-model-load-$arm"
    trap run_interrupted INT TERM
    k delete job "$RUN_JOB" --ignore-not-found >/dev/null 2>&1
    render_load_job "$RUN_JOB" "$url_a" "$url_b" "$arm" > "$out_dir/job.yaml"
    [ -s "$out_dir/job.yaml" ] || die "the load Job rendered empty"
    k apply -f "$out_dir/job.yaml" >/dev/null || die "could not create the load Job"

    local until_ts=$(( $(date +%s) + total + 300 ))
    sample_gpus "$out_dir/gpus.jsonl" "$until_ts" &
    RUN_SAMPLER=$!

    # Waited on by the loader's OWN line, not by the Job's completion. The Pod
    # stays alive afterwards on purpose: `kubectl cp` is exec+tar and cannot run
    # in a terminated container, so waiting for Completed and then copying loses
    # the results of every successful run -- which is what the first version did.
    info "waiting for the loader to report its results (up to $(( total + 600 ))s)..."
    local deadline=$(( $(date +%s) + total + 600 )) pod="" done=0
    while [ "$(date +%s)" -lt "$deadline" ]; do
        pod="$(k get pods -l "job-name=$RUN_JOB" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
        if [ -n "$pod" ]; then
            if k logs "$pod" 2>/dev/null | grep -q 'RESULTS-WRITTEN'; then done=1; break; fi
            # A loader that died in its first second must not cost the full
            # schedule before anyone is told.
            case "$(k get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" in
                Failed) warn "the loader Pod failed"; break ;;
            esac
        fi
        sleep 15
    done
    kill "$RUN_SAMPLER" 2>/dev/null; RUN_SAMPLER=""

    if [ -n "$pod" ]; then
        k logs "$pod" > "$out_dir/loader.log" 2>&1 || true
    fi
    if [ "$done" -eq 1 ]; then
        k cp "$pod:/results/requests.jsonl" "$out_dir/requests.jsonl" >/dev/null 2>&1 || \
            warn "could not copy requests.jsonl out of $pod"
        k cp "$pod:/results/requests.jsonl.meta.json" "$out_dir/meta.json" >/dev/null 2>&1 || \
            warn "could not copy the meta out of $pod -- the report refuses without it"
    fi
    trap - INT TERM
    run_cleanup
    RUN_JOB=""
    [ -s "$out_dir/requests.jsonl" ] || \
        die "no requests were recorded for arm $arm. See $out_dir/loader.log"
    ok "arm $arm: $(wc -l < "$out_dir/requests.jsonl") requests, $(wc -l < "$out_dir/gpus.jsonl" 2>/dev/null || echo 0) GPU samples in $out_dir"
}

render_load_job() {
    local job="$1" url_a="$2" url_b="$3" arm="$4"
    local total=$(( LEAD_IN + 2 * CYCLES * PHASE_SECONDS ))
    cat <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: $job
  namespace: $NS
  labels:
    app.kubernetes.io/name: wva-two-model-load
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app.kubernetes.io/name: wva-two-model-load
    spec:
      restartPolicy: Never
      containers:
        - name: load
          image: $(load_image)
          command: ["/bin/sh", "-c"]
          # The sleep is not idleness: kubectl cp is exec+tar and cannot run in
          # a terminated container, so the Pod has to outlive the write. The
          # driver deletes the Job as soon as it has copied.
          args:
            - |
              python3 /loader/two_model_load.py \
                --endpoint-a='$url_a' \
                --endpoint-b='$url_b' \
                --model-a='$MODEL_A' \
                --model-b='$MODEL_B' \
                --phase-seconds=$PHASE_SECONDS \
                --cycles=$CYCLES \
                --lead-in=$LEAD_IN \
                --low-rps=$LOW_RPS \
                --high-rps=$HIGH_RPS \
                --input-tokens=$INPUT_TOKENS \
                --output-tokens=$OUTPUT_TOKENS \
                --timeout=$REQUEST_TIMEOUT \
                --seed=$SEED \
                --arm='$arm' \
                --out=/results/requests.jsonl
              rc=\$?
              echo "loader exited \$rc; holding the Pod open for collection"
              sleep 1800
          resources:
            requests:
              cpu: "2"
              memory: "2Gi"
            limits:
              cpu: "4"
              memory: "4Gi"
          volumeMounts:
            - name: loader
              mountPath: /loader
            - name: results
              mountPath: /results
      volumes:
        - name: loader
          configMap:
            name: wva-two-model-load
        - name: results
          emptyDir: {}
YAML
}

# ---------------------------------------------------------------------------
# report / status / teardown
# ---------------------------------------------------------------------------
verb_report() {
    local a="$OUT_ROOT/nopool" b="$OUT_ROOT/pool"
    [ -s "$a/requests.jsonl" ] || die "no nopool results in $a -- run both arms before reporting"
    [ -s "$b/requests.jsonl" ] || die "no pool results in $b -- run both arms before reporting"
    python3 "$HERE/two_model_report.py" \
        --nopool "$a" --pool "$b" \
        --model-a "$MODEL_A" --model-b "$MODEL_B" \
        || die "the report failed"
}

verb_status() {
    need_ns
    echo "--- deployments" >&2
    k get deploy -o wide 2>/dev/null | sed 's/^/  /' >&2
    echo "--- scaledobjects" >&2
    k get scaledobject 2>/dev/null | sed 's/^/  /' >&2
    echo "--- accelerators held in $NS" >&2
    k get pods -o json 2>/dev/null | jq -r '
      [.items[]|select(.status.phase=="Running")
       |{n:.metadata.name, g:([.spec.containers[].resources.requests["nvidia.com/gpu"]//"0"|tonumber]|add)}
       |select(.g>0)] as $p
      | "  " + (([$p[].g]|add // 0)|tostring) + " GPU(s) in " + (($p|length)|tostring) + " pod(s)",
        ($p[] | "    " + .n + "  " + (.g|tostring))' 2>/dev/null >&2
}

verb_teardown() {
    need_ns
    warn "tearing down everything this scenario created in $NS"
    verb_pool_delete || true
    k delete job -l app.kubernetes.io/name=wva-two-model-load --ignore-not-found >/dev/null 2>&1
    k delete configmap wva-two-model-load --ignore-not-found >/dev/null 2>&1
    ( cd "$ROOT" && BENCHMARK_NAMESPACE="$NS" make --no-print-directory benchmark-teardown ) || \
        warn "benchmark-teardown reported a problem; check for leftover GPU pods below"
    verb_status
    ok "teardown done. Anything still holding a GPU above is a leak -- this is a shared cluster."
}

usage() { sed -n '2,/^set -u/p' "$0" | sed 's/^# \{0,1\}//; $d'; }

case "${1:-}" in
    preflight)   shift; verb_preflight "$@" ;;
    standup)     shift; verb_standup "$@" ;;
    verify)      shift; verb_verify "$@" ;;
    pool-create) shift; verb_pool_create "$@" ;;
    pool-delete) shift; verb_pool_delete "$@" ;;
    warm)        shift; verb_warm "$@" ;;
    reset)       shift; verb_reset "$@" ;;
    run)         shift; verb_run "$@" ;;
    report)      shift; verb_report "$@" ;;
    status)      shift; verb_status "$@" ;;
    teardown)    shift; verb_teardown "$@" ;;
    residency)   shift; need_ns; pool_residency ;;
    -h|--help|"") usage ;;
    *)           usage; die "unknown verb '$1'" ;;
esac
