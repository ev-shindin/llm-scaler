#!/usr/bin/env bash
# What `deploy/warmpool.sh create` actually EMITS.
#
# `bash -n` parses this script happily in every state it has been broken in, and
# every one of those states shipped:
#
#   - one container in the Pod, with the proxy image as `inference-server`, so
#     every pool was inert;
#   - the ScaledObject dropped by a refactor, so pools were created and never
#     discovered -- a pool is DECLARED by its trigger, so without it the
#     Deployment holds accelerators nobody can borrow;
#   - workers running the proxy, so a group never became Ready and held GPUs
#     while reporting pods=0;
#   - `log_error` inside `$( )`, which exits only the subshell and applied a
#     Deployment with no containers at all.
#
# None of these is visible without rendering the manifests and looking. That is
# all this does: --dry-run, then assert the shape. It needs no cluster.
set -u

SCRIPT="$(cd "$(dirname "$0")/.." && pwd)/deploy/warmpool.sh"
FAILED=0

ok()   { printf 'ok    %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; FAILED=1; }

# render prints the manifests for a pool, or nothing if the script failed.
render() {
  bash "$SCRIPT" create -n tenant --name pool \
    --proxy-image example.invalid/proxy:v1 --wva-namespace wva-system \
    --models 2 --model-size 8B --accelerator NVIDIA-H100-80GB-HBM3 \
    --dry-run "$@" 2>/dev/null
}

# count_kind counts documents of one kind. `kind:` at column 0 is only ever a
# top-level document field here; nested ones are indented.
count_kind() {
  printf '%s\n' "$1" | grep -c "^kind: $2\$"
}

# --- a single-Pod pool ------------------------------------------------------
POOL="$(render)"

if [ -z "$POOL" ]; then
  fail "create --dry-run produced nothing at all"
  exit 1
fi
ok "create --dry-run produces manifests"

# Parseable. A rendering bug usually shows up here first, and as a YAML error
# rather than as anything about pools.
if command -v python3 >/dev/null 2>&1; then
  if printf '%s\n' "$POOL" | python3 -c '
import sys, yaml
docs = list(yaml.safe_load_all(sys.stdin))
sys.exit(0 if all(d for d in docs) else 1)
' 2>/dev/null; then
    ok "every rendered document parses as YAML"
  else
    fail "a rendered document does not parse as YAML"
  fi
fi

[ "$(count_kind "$POOL" Deployment)" = 1 ] \
  && ok "one Deployment" || fail "expected exactly one Deployment"

# THE OTHER ONE THAT SHIPPED BROKEN, and the one nothing else could see. Every
# embedded Python program -- the preStop drain hook and both probes -- must
# still be the program the manifest says, after passing through an UNQUOTED
# heredoc.
#
# It was not. PyYAML folds a long double-quoted scalar by ending the line with a
# backslash, bash read that as a line continuation and swallowed the newline and
# the next line's indentation, and those spaces landed inside a string literal:
# `inst.get("          options")`. The hook found no instances, exited 0, and
# every pool Pod this script created was killed with its engine awake and
# requests in flight -- the one thing the hook exists to prevent.
#
# Nothing caught it. The YAML still parsed, `make warmpool-check` still found
# the rendered text identical to what the generator produced, the `preStop`
# assertion below still passed, and a drain that drains nothing says nothing. So
# this compares the DELIVERED program with the manifest's, character for
# character, which is the only comparison that could have failed.
if command -v python3 >/dev/null 2>&1; then
  if printf '%s\n' "$POOL" | python3 -c '
import ast, sys, yaml

MANIFEST = "config/warmpool/warmpool-deployment.yaml"


def programs(spec):
    """Every python3 -c program in a Pod spec, keyed by where it came from."""
    found = {}
    for c in spec.get("containers", []):
        places = {
            "preStop": (c.get("lifecycle") or {}).get("preStop") or {},
            "readinessProbe": c.get("readinessProbe") or {},
            "livenessProbe": c.get("livenessProbe") or {},
        }
        for name, place in places.items():
            cmd = (place.get("exec") or {}).get("command") or []
            if "python3" in cmd and "-c" in cmd:
                found["%s/%s" % (c["name"], name)] = cmd[-1]
    return found


want = {}
with open(MANIFEST) as fh:
    for doc in yaml.safe_load_all(fh):
        if doc and doc.get("kind") == "Deployment":
            want = programs(doc["spec"]["template"]["spec"])

got = {}
for doc in yaml.safe_load_all(sys.stdin):
    if doc and doc.get("kind") == "Deployment":
        got.update(programs(doc["spec"]["template"]["spec"]))

if not want or not got:
    print("no embedded programs found to compare", file=sys.stderr)
    sys.exit(1)

for where, program in sorted(got.items()):
    try:
        ast.parse(program)
    except SyntaxError as err:
        print("%s does not parse as Python: %s" % (where, err), file=sys.stderr)
        sys.exit(1)
    if where in want and program != want[where]:
        print("%s differs from the manifest" % where, file=sys.stderr)
        sys.exit(1)
'; then
    ok "the drain hook and probes survive the heredoc intact"
  else
    fail "an embedded python program was corrupted in rendering, or no longer matches the manifest"
  fi
fi

# THE ONE THAT SHIPPED BROKEN. A pool with no ScaledObject is not a pool: it is
# a Deployment holding accelerators that WVA reports as undeclared.
[ "$(count_kind "$POOL" ScaledObject)" = 1 ] \
  && ok "the ScaledObject that declares the pool" \
  || fail "no ScaledObject: the pool would be created and never discovered"

printf '%s\n' "$POOL" | grep -q 'warmPoolName: pool' \
  && ok "the trigger names the pool" \
  || fail "the trigger carries no warmPoolName, so nothing declares this pool"

# TWO containers, and the right images on each. The proxy is the traffic gate;
# the supervisor is the thing that holds the engines. One without the other is a
# Pod that either serves nothing or gates nothing.
[ "$(printf '%s\n' "$POOL" | grep -c '^      - name: inference-server$')" = 1 ] \
  && ok "the supervisor container" || fail "no inference-server container"
[ "$(printf '%s\n' "$POOL" | grep -c '^      - name: proxy$')" = 1 ] \
  && ok "the proxy container" || fail "no proxy container"

# The images must not be the same one. The bug was not a missing container but
# the proxy image running under the supervisor's NAME, which looks correct in
# every listing and answers nothing on :8001.
if printf '%s\n' "$POOL" | grep -A3 '^      - name: inference-server$' | grep -q 'example.invalid/proxy'; then
  fail "the supervisor container is running the proxy image"
else
  ok "the supervisor is not running the proxy image"
fi

# A pool Pod that dies while a model is awake must DRAIN first. Without the hook
# the launcher takes SIGTERM and every in-flight response is cut -- the failure
# the warm pool exists to avoid, arriving by a different route (the pool shrinks,
# a node is drained, the Deployment rolls).
printf '%s' "$POOL" | grep -q 'preStop'   && ok "the drain hook" || fail "no preStop hook: a removed Pod cuts its in-flight requests"

# And the grace period has to outlast that drain, or the kubelet SIGKILLs in the
# middle of it and the hook bought nothing.
printf '%s' "$POOL" | grep -q 'terminationGracePeriodSeconds'   && ok "a grace period sized for the drain"   || fail "no terminationGracePeriodSeconds: the default 30s cuts a slow generation"

# The boundary. Its absence leaves :8001 -- caller-supplied argv in a container
# mounting the shared model cache read-write -- open to anything that can route
# to the Pod IP.
[ "$(count_kind "$POOL" NetworkPolicy)" = 1 ] \
  && ok "the NetworkPolicy" || fail "no NetworkPolicy: :8001 would be open to the cluster"

printf '%s\n' "$POOL" | grep -q 'kubernetes.io/metadata.name: wva-system' \
  && ok "the policy admits the WVA namespace it was given" \
  || fail "the policy does not name --wva-namespace, so the controller is denied"

# Opting out must actually opt out, and must not disturb anything else.
OPTED="$(render --no-network-policy)"
[ "$(count_kind "$OPTED" NetworkPolicy)" = 0 ] \
  && ok "--no-network-policy omits it" || fail "--no-network-policy still emitted a policy"
[ "$(count_kind "$OPTED" Deployment)" = 1 ] \
  && ok "--no-network-policy leaves the Deployment alone" \
  || fail "--no-network-policy changed more than the policy"

# --- a pool of groups -------------------------------------------------------
# --launcher-image is REQUIRED for a group: the stock launcher runs every rank
# through vLLM's API server, which has no follower path, so an engine spanning
# Pods can never form. Rendering one without it is refused, and that refusal is
# asserted below.
GROUP="$(render --group-size 2 --launcher-image example.invalid/launcher:headless)"

[ "$(count_kind "$GROUP" LeaderWorkerSet)" = 1 ] \
  && ok "a group pool is a LeaderWorkerSet" || fail "--group-size did not produce a LeaderWorkerSet"

# The ScaledObject has to target the right KIND. KEDA scales what the ref names,
# and a group pool whose ref says Deployment silently scales nothing.
printf '%s\n' "$GROUP" | grep -q 'kind: LeaderWorkerSet' \
  && ok "the group's ScaledObject targets a LeaderWorkerSet" \
  || fail "the group's scaleTargetRef does not name LeaderWorkerSet"

# WORKERS DO NOT RUN THE PROXY. A worker with a proxy never passes its readiness
# probe, so the group never counts as a member and holds its GPUs reporting
# pods=0 -- which reads as a pool that is simply too small.
WORKER_SECTION="$(printf '%s\n' "$GROUP" | sed -n '/workerTemplate:/,$p')"
[ "$(printf '%s\n' "$WORKER_SECTION" | grep -c 'name: proxy')" = 0 ] \
  && ok "workers run no proxy" \
  || fail "a worker template carries the proxy: the group would never become Ready"

# And the other half of the same invariant. Warming a group asks EVERY rank to
# create its own instance, at that rank's own Pod IP -- so a worker with nothing
# listening on :8001 makes the fan-out unreachable, and the admission fails
# half-done while the group holds all its GPUs. The e2e fixture had exactly this
# shape (workers running `sleep infinity`) and could not exercise the fan-out at
# all, which is how it went untested.
[ "$(printf '%s\n' "$WORKER_SECTION" | grep -c 'name: inference-server')" = 1 ] \
  && ok "workers run the supervisor, which is what the fan-out talks to" \
  || fail "a worker template has no supervisor: warming the group cannot reach its ranks"

# A GROUP on the stock launcher is refused outright. It would schedule, go
# Ready, hold every accelerator and never form an engine -- the silent failure
# this script exists to keep operators out of.
if render --group-size 2 >/dev/null 2>&1; then
  fail "a group pool was accepted without --launcher-image: it could never form an engine"
else
  ok "a group pool without --launcher-image is refused"
fi

# --- guardrails -------------------------------------------------------------
# A ceiling at or below the reserve makes the admission budget zero forever: the
# pool holds accelerators and warms nothing, silently. It must refuse.
if render --max 2 --reserve 2 >/dev/null 2>&1; then
  fail "--max equal to --reserve was accepted: the pool could never admit anything"
else
  ok "--max at or below --reserve is refused"
fi

# --- scraping a lent Pod ----------------------------------------------------
# A bridge serves a variant's traffic. Unscraped, that load is invisible to the
# analyzer, so the variant's measured demand FALLS while the pool is covering its
# shortfall and reappears when the Pod goes back -- which reads as a spike
# arriving rather than capacity leaving. Nothing about that is visible on the
# cluster: the pool works, the model serves, and the number is quietly wrong.
MON="$(render --monitoring-namespace observability)"

[ "$(count_kind "$MON" PodMonitor)" = 1 ]   && ok "a monitoring namespace creates the pool scrape config"   || fail "no PodMonitor: a lent Pod would be scraped by nothing and its load would be invisible"

printf '%s
' "$MON" | grep -q '__meta_kubernetes_pod_ready'   && ok "only READY pool Pods become scrape targets"   || fail "the PodMonitor targets every pool Pod: a sleeping one has no engine behind its proxy, so an idle pool becomes a wall of DOWN targets"

printf '%s
' "$MON" | grep -q 'kubernetes.io/metadata.name: observability'   && ok "the monitoring namespace is admitted to the serving port"   || fail "the NetworkPolicy does not admit the scraper: the PodMonitor exists and every scrape is dropped"

# ...and NEITHER appears without one. Guessing a monitoring namespace would open
# the serving port to the wrong one, which is worse than leaving it shut: it
# reads as monitoring that is configured.
[ "$(count_kind "$POOL" PodMonitor)" = 0 ]   && ok "no scrape config without a monitoring namespace"   || fail "a PodMonitor was created for a pool that named no monitoring namespace"

printf '%s
' "$POOL" | grep -q 'kubernetes.io/metadata.name: observability'   && fail "a monitoring namespace was admitted though none was named"   || ok "no monitoring peer without a monitoring namespace"

if [ "$FAILED" -eq 0 ]; then
  echo "Warm pool manifest checks passed."
else
  echo "Warm pool manifest checks FAILED."
fi
exit "$FAILED"
