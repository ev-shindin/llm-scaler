#!/usr/bin/env bash
# Executes deploy/prepull.sh and hack/benchmark/engine_image.sh against
# fixtures and asserts what they do.
#
# Every defect this covers parses fine: a holder that requests an accelerator
# (it would then HOLD one, the opposite of the point), a placement that lands
# on every node or on none, two images colliding on one DaemonSet name, a pull
# policy that re-pulls a pinned tag on every restart, a holder handed a
# ServiceAccount token it never uses or every GPU on the node through the
# image's own NVIDIA_VISIBLE_DEVICES, a `status` that reads a pulled image as
# absent or an evicted holder as present, a comma list of images that reaches
# the script as one argument, an engine image read from the wrong key, or a
# scenario whose engine container quietly went back to Always.
#
# `status`, `delete` and the standup's pre-pull step are exercised offline: a
# stub kubectl on PATH answers `get nodes`, `get pods`, `get daemonset`,
# `get events`, `create namespace` and `apply` from JSON fixtures written
# below, the pattern hack/check-limiter-declaration.sh uses. A live cluster is
# not needed to know what the classification does with a RunContainerError or
# an Evicted holder; the fixtures are what a real cluster answered.
set -euo pipefail

# jq is not optional: `status` reads the node and pod lists with it, and
# without it every status assertion fails with nothing saying why.
command -v jq >/dev/null 2>&1 || {
    printf 'FATAL: jq is required to run these checks.\n' >&2
    exit 1
}
cd "$(dirname "$0")/.."

PY=${PYTHON:-python3}
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
FAILED=0
ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

# ---------------------------------------------------------------------------
# 1. The rendered DaemonSets.
# ---------------------------------------------------------------------------
LONG='registry.example.com:5000/Some-Org/A-Very-Long-Engine-Image-Name-That-Goes-On:V1.2.3-and-a-long-tag-suffix-too'
bash deploy/prepull.sh apply -n check-ns \
    --image docker.io/vllm/vllm-openai:v0.26.0 \
    --image ghcr.io/other/vllm-openai:v0.26.0 \
    --image 'docker.io/vllm/vllm-openai@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' \
    --image "$LONG" \
    --node-selector example.com/accelerator=h200 \
    --toleration example.com/dedicated \
    --dry-run > "$T/render.yaml"
# and once with the defaults: one toleration, the affinity over the product keys
bash deploy/prepull.sh apply -n check-ns --image docker.io/vllm/vllm-openai:v0.26.0 --dry-run > "$T/render-default.yaml"

"$PY" - "$T/render.yaml" "$T/render-default.yaml" "$LONG" deploy/lib/accelerator_labels.py <<'PYEOF' || FAILED=1
import re, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d]
default = [d for d in yaml.safe_load_all(open(sys.argv[2], encoding="utf-8")) if d]
long_ref = sys.argv[3]
# the product keys the rest of the repo resolves accelerator nodes through
ns = {}
exec(open(sys.argv[4], encoding="utf-8").read(), ns)
product_keys = ns["PRODUCT_KEYS"]
bad = []
def check(cond, msg):
    if not cond:
        bad.append(msg)

check(len(docs) == 4, "expected 4 DaemonSets, got %d" % len(docs))
names = [d["metadata"]["name"] for d in docs]
check(len(set(names)) == 4, "names collide: %s" % names)
label = re.compile(r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$")
for n in names:
    check(bool(label.match(n)), "not a DNS-1123 label (<=63, lowercase, no leading/trailing hyphen): %r" % n)
check("very-long-engine-image-name-that-goes" in names[3],
      "the long mixed-case reference is case-folded into its name, not scrubbed: %r" % names[3])
images = ["docker.io/vllm/vllm-openai:v0.26.0", "ghcr.io/other/vllm-openai:v0.26.0",
          "docker.io/vllm/vllm-openai@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", long_ref]
for d, image in zip(docs, images):
    check(d["kind"] == "DaemonSet", "kind %s" % d["kind"])
    check(d["metadata"]["namespace"] == "check-ns", "namespace")
    ann = d["metadata"]["annotations"]
    check(ann["wva.llmd.ai/prepull-image"] == image, "annotation names the image: %s" % ann)
    check(ann["wva.llmd.ai/prepull-node-selector"] == "example.com/accelerator=h200", "annotation records the selector: %s" % ann)
    tpl = d["spec"]["template"]
    ps = tpl["spec"]
    check(ps["nodeSelector"] == {"example.com/accelerator": "h200"}, "nodeSelector: %s" % ps["nodeSelector"])
    check("affinity" not in ps, "an explicit selector replaces the affinity, not adds to it")
    keys = {t["key"] for t in ps["tolerations"]}
    check({"nvidia.com/gpu", "example.com/dedicated"} <= keys, "tolerations: %s" % keys)
    check(ps.get("automountServiceAccountToken") is False, "the holder must not mount a ServiceAccount token")
    check((ps.get("securityContext") or {}).get("seccompProfile", {}).get("type") == "RuntimeDefault", "pod seccompProfile RuntimeDefault")
    sel = d["spec"]["selector"]["matchLabels"]
    check(all(tpl["metadata"]["labels"].get(k) == v for k, v in sel.items()), "selector does not match the template labels")
    check(tpl["metadata"]["labels"].get("app.kubernetes.io/managed-by") == "wva-prepull", "the pods carry managed-by, so the pod list is scoped like the DaemonSet list")
    cs = ps["containers"]
    check(len(cs) == 1, "one container")
    c = cs[0]
    check(c["image"] == image, "container image %s" % c["image"])
    check(c["imagePullPolicy"] == "IfNotPresent", "pull policy %s" % c["imagePullPolicy"])
    res = c.get("resources", {})
    for section in ("requests", "limits"):
        for k in res.get(section, {}):
            check("gpu" not in k and "/" not in k, "the holder requests an accelerator: %s" % k)
    check(res.get("limits", {}).get("memory"), "a memory limit, so a holder can never grow")
    env = {e["name"]: e.get("value") for e in c.get("env", [])}
    check(env.get("NVIDIA_VISIBLE_DEVICES") == "void",
          "NVIDIA_VISIBLE_DEVICES must be void: engine images bake in =all and the runtime injects every GPU into a container that requested none")
    check(c["command"][0] == "/bin/sh", "runs the image itself, asleep")
    check("$!" in c["command"][2] and "\\$" not in c["command"][2], "the sleep loop's $! reaches the pod unescaped")
    csc = c.get("securityContext", {})
    check(csc.get("allowPrivilegeEscalation") is False and "ALL" in csc.get("capabilities", {}).get("drop", []), "container drops all capabilities")
    check(csc.get("readOnlyRootFilesystem") is True, "the holder writes nothing; its root filesystem is read-only")
    check("terminationGracePeriodSeconds" in ps, "short termination so a holder yields its node quickly")

check(len(default) == 1, "one DaemonSet with defaults")
if default:
    d = default[0]
    ps = d["spec"]["template"]["spec"]
    check(ps["tolerations"] == [{"key": "nvidia.com/gpu", "operator": "Exists"}], "with no --toleration exactly nvidia.com/gpu is tolerated: %s" % ps["tolerations"])
    check("nodeSelector" not in ps, "no --node-selector: no nodeSelector (it cannot express OR)")
    terms = ps.get("affinity", {}).get("nodeAffinity", {}).get("requiredDuringSchedulingIgnoredDuringExecution", {}).get("nodeSelectorTerms", [])
    keys = []
    for t in terms:
        exprs = t.get("matchExpressions", [])
        ops = [(e.get("key"), e.get("operator"), e.get("values")) for e in exprs]
        check(len(exprs) == 2 and ops[0][1] == "Exists" and ops[1] == (ops[0][0], "NotIn", [""]),
              "each term is one key: Exists AND NotIn [empty] (an empty product label is a CPU node): %s" % t)
        keys.append(ops[0][0])
    check(keys == product_keys, "the default affinity is one term per product key, in order: %s vs %s" % (keys, product_keys))
    check(d["metadata"]["annotations"]["wva.llmd.ai/prepull-node-selector"] == "", "the default records an empty selector")

if bad:
    for b in bad:
        print("  FAIL " + b)
    sys.exit(1)
print("  ok   %d DaemonSets rendered: names, placement (selector or product-key affinity), tolerations, no accelerator, no SA token, no GPU injection, seccomp, IfNotPresent" % len(docs))
PYEOF

for badsel in notakeyvalue 'a=b"' 'a=b,c=d' '=v' 'a b=c'; do
    if bash deploy/prepull.sh apply -n check-ns --image a:1 --node-selector "$badsel" --dry-run >/dev/null 2>&1; then
        fail "a --node-selector of '$badsel' must be refused"
    fi
done
ok "a --node-selector that is not one KEY=VALUE of label characters is refused"
if bash deploy/prepull.sh apply -n check-ns --image a:1 --toleration '' --dry-run >/dev/null 2>&1; then
    fail "an empty --toleration (which tolerates every taint) must be refused"
else
    ok "an empty --toleration is refused"
fi

# ---------------------------------------------------------------------------
# 2. engine_image.sh: the harness's own pin, or nothing.
# ---------------------------------------------------------------------------
mkdir -p "$T/clone/config/templates/values"
cat > "$T/clone/config/templates/values/defaults.yaml" <<'EOF'
versions:
  vllm-openai_version: &vllm-openai_version v0.26.0
images:
  vllm:
    repository: docker.io/vllm/vllm-openai
    tag: *vllm-openai_version
EOF
# `|| true` on the assignments: a failing command substitution in a plain
# assignment ends the script under set -e, and the FAIL line it feeds would
# never print.
got="$(bash hack/benchmark/engine_image.sh "$T/clone" || true)"
if [ "$got" = "docker.io/vllm/vllm-openai:v0.26.0" ]; then ok "engine_image.sh reads repository:tag with the anchor resolved"; else fail "engine_image.sh printed '$got'"; fi
if out="$(bash hack/benchmark/engine_image.sh "$T/nonexistent" 2>/dev/null)"; then fail "engine_image.sh must fail on a missing clone"; else
    [ -z "$out" ] && ok "engine_image.sh prints nothing and fails on a missing clone" || fail "engine_image.sh printed on failure: '$out'"; fi
mkdir -p "$T/clone2/config/templates/values"
printf 'images:\n  vllm:\n    repository: docker.io/vllm/vllm-openai\n' > "$T/clone2/config/templates/values/defaults.yaml"
if bash hack/benchmark/engine_image.sh "$T/clone2" >/dev/null 2>&1; then fail "engine_image.sh must fail without a tag"; else ok "engine_image.sh fails without a tag rather than guessing"; fi

# ---------------------------------------------------------------------------
# 3. status, delete and apply, offline, against what a cluster answered.
#
# A stub kubectl on PATH (the pattern hack/check-limiter-declaration.sh and
# hack/check-two-model-scenario.sh use) answers from the fixtures, and it
# checks the arguments it is given: a call that dropped -n or -l would still
# hit the right branch of a stub keyed on the verb alone, and the offline
# check would pass while the real call read the wrong scope. What it records
# in $T/calls is asserted below for delete and for the standup step.
# ---------------------------------------------------------------------------
IMG=docker.io/vllm/vllm-openai:v0.26.0
LBL='app.kubernetes.io/component=image-prepull,app.kubernetes.io/managed-by=wva-prepull'
bash deploy/prepull.sh apply -n check-ns --image "$IMG" --dry-run > "$T/one.yaml"
DS="$(grep -m1 '^  name:' "$T/one.yaml" | awk '{print $2}' || true)"
[ -n "$DS" ] || fail "no DaemonSet name in the rendered manifest"
# Nodes carry product labels the way real clusters do: GFD on most, the
# CoreWeave key on one, and one CPU node with none, which the default
# placement must leave out of the count.
cat > "$T/nodes.json" <<EOF
{"items":[
 {"metadata":{"name":"node-running",  "labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[]}},
 {"metadata":{"name":"node-listed",   "labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[{"names":["docker.io/vllm/vllm-openai@sha256:abc","${IMG}"]}]}},
 {"metadata":{"name":"node-pulled",   "labels":{"gpu.nvidia.com/model":"H200"}},   "status":{"images":[]}},
 {"metadata":{"name":"node-crashloop","labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[]}},
 {"metadata":{"name":"node-completed","labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[]}},
 {"metadata":{"name":"node-evicted",  "labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[]}},
 {"metadata":{"name":"node-nopod",    "labels":{"nvidia.com/gpu.product":"H200"}}, "status":{"images":[{"names":["something/else:1"]}]}},
 {"metadata":{"name":"node-noimages", "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-cpu",      "labels":{"kubernetes.io/os":"linux"}},       "status":{"images":[{"names":["${IMG}"]}]}},
 {"metadata":{"name":"node-cpu-cw",   "labels":{"gpu.nvidia.com/model":"","gpu.nvidia.com/class":""}}, "status":{"images":[]}}
]}
EOF
cat > "$T/pods.json" <<EOF
{"items":[
 {"metadata":{"labels":{"wva.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-running"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-listed"},"status":{"phase":"Pending","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"ContainerCreating"}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-pulled"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"RunContainerError"}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-crashloop"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-completed"},"status":{"phase":"Succeeded","containerStatuses":[{"ready":false,"state":{"terminated":{"reason":"Completed"}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/prepull":"${DS}"}},"spec":{"nodeName":"node-evicted"},"status":{"phase":"Failed","reason":"Evicted","message":"The node had condition: [DiskPressure]."}},
 {"metadata":{"labels":{"wva.llmd.ai/prepull":"other-ds"}},"spec":{"nodeName":"node-nopod"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}}
]}
EOF
mkdir -p "$T/bin"
cat > "$T/bin/kubectl" <<EOF
#!/usr/bin/env bash
# stub: the reads and writes prepull.sh and the standup step make, answered
# from fixtures; every call is recorded and its scope arguments checked.
printf '%s\n' "\$*" >> "$T/calls"
ARGV=" \$* "   # the call's own argv; inside want/refuse \$* would be THEIR arguments
want() {  # the argv must contain this exact argument pair
    case "\$ARGV" in *" \$1 \$2 "*) ;; *) echo "stub kubectl: \$1 \$2 missing from:\$ARGV" >&2; exit 2 ;; esac
}
refuse() {  # the argv must NOT contain this argument
    case "\$ARGV" in *" \$1 "*) echo "stub kubectl: unexpected \$1 in:\$ARGV" >&2; exit 2 ;; esac
}
case "\$1 \$2" in
  "get nodes")
      if [ -n "\${STUB_SELECTOR:-}" ]; then want -l "\$STUB_SELECTOR"; else refuse -l; fi
      [ -n "\${STUB_NODES_FORBIDDEN:-}" ] && { echo 'Error from server (Forbidden): nodes is forbidden' >&2; exit 1; }
      cat "$T/nodes.json" ;;
  "get pods")      want -n check-ns; want -l "$LBL"; cat "$T/pods.json" ;;
  "get daemonset")
      want -n check-ns
      case " \$* " in
        *" -l "*) want -l "$LBL"; printf '%s\n' "\${STUB_DAEMONSETS-${IMG}}" ;;
        *) printf '%s' "\${STUB_DS_SELECTOR:-}" ;;
      esac ;;
  "get events")    want -n check-ns; printf '%s' "\${STUB_EVENT:-}" ;;
  "apply -n")      want -n check-ns; cat >/dev/null; echo "daemonset.apps/x configured" ;;
  "apply -f")      cat >/dev/null; echo "namespace/x configured" ;;
  "create namespace") echo "apiVersion: v1"; echo "kind: Namespace" ;;
  "delete daemonset") want -n check-ns; echo "daemonset.apps deleted" ;;
  *) echo "stub kubectl: unexpected \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/bin/kubectl"
STUB_PATH="$T/bin:$PATH"

set +e
PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns > "$T/status.out" 2>&1
rc=$?
set -e
expect_line() {  # node, IMAGE column
    if grep -qE "^  $1 +$2 " "$T/status.out"; then ok "status: $1 -> $2"; else fail "status: $1 expected $2; got: $(grep -E "^  $1 " "$T/status.out" || echo none)"; fi
}
expect_line node-running   present
expect_line node-listed    present
expect_line node-pulled    pulled
expect_line node-crashloop pulled
expect_line node-completed pulled
expect_line node-evicted   absent
expect_line node-nopod     absent
expect_line node-noimages  absent
grep -q '^  node-cpu ' "$T/status.out" && fail "status: a node with no product label was counted under the default placement" || ok "status: the default placement leaves a node with no product label out"
grep -q '^  node-cpu-cw ' "$T/status.out" && fail "status: a node whose product labels are EMPTY (a CoreWeave CPU node) was counted" || ok "status: the default placement leaves a node with empty product labels out"
grep -q 'node-evicted .*Evicted' "$T/status.out" && ok "status: the evicted holder shows its reason" || fail "status: Evicted reason missing"
grep -q 'node-pulled .*not held' "$T/status.out" && ok "status: a pulled-but-unheld image says so" || fail "status: pulled/not-held note missing"
grep -q '5/8 nodes have it (2 held, 3 pulled but not held); 3 do not' "$T/status.out" && ok "status: the tally separates held from pulled-not-held" || fail "status: tally wrong: $(grep 'nodes have' "$T/status.out")"
[ "$rc" -ne 0 ] && ok "status: exits non-zero while a node lacks the image" || fail "status: exit 0 with nodes lacking the image"
grep -q "^${IMG}  (nodes: any node carrying" "$T/status.out" && ok "status: with no --image the held images are discovered from the DaemonSets, with the recorded placement" || fail "status: discovery from DaemonSets failed: $(head -1 "$T/status.out")"
grep -q 'stub kubectl:' "$T/status.out" && fail "status: a kubectl call lacked its scope arguments: $(grep 'stub kubectl:' "$T/status.out" | head -1)" || ok "status: every kubectl call carried -n / -l, and none an -l for nodes under the default placement"

# the DaemonSet's recorded selector is what status lists nodes with; the flag overrides
: > "$T/calls"
STUB_DS_SELECTOR=example.com/accelerator=h200 STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns > "$T/recorded.out" 2>&1 || true
grep -q 'get nodes -l example.com/accelerator=h200' "$T/calls" && ok "status: lists the nodes the DaemonSet was applied for (its recorded selector)" || fail "status: recorded selector not used: $(grep 'get nodes' "$T/calls")"
: > "$T/calls"
STUB_DS_SELECTOR=example.com/accelerator=h200 STUB_SELECTOR=other/key=v PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" --node-selector other/key=v >/dev/null 2>&1 || true
grep -q 'get nodes -l other/key=v' "$T/calls" && ok "status: --node-selector overrides the recorded one" || fail "status: override not passed: $(grep 'get nodes' "$T/calls")"

# no DaemonSets and no --image: a clear refusal, not a silent empty walk
if STUB_DAEMONSETS="" PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns > "$T/none.out" 2>&1; then fail "status with nothing to check must fail"; else
    grep -q 'no pre-pull DaemonSets' "$T/none.out" && ok "status: no DaemonSets and no --image is refused with a reason" || fail "status: refusal message missing: $(tail -1 "$T/none.out")"; fi

# no holder anywhere: the DaemonSet's FailedCreate event is the explanation
printf '{"items":[]}' > "$T/pods-none.json"
cp "$T/pods.json" "$T/pods-all.json"; cp "$T/pods-none.json" "$T/pods.json"
if STUB_EVENT='pods "prepull-x-" is forbidden: violates PodSecurity "restricted:latest": runAsNonRoot != true' PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" > "$T/nopods.out" 2>&1; then fail "status with no holders must fail"; else
    grep -q 'cannot create its pods: pods "prepull-x-" is forbidden: violates PodSecurity' "$T/nopods.out" && ok "status: with no holder anywhere the DaemonSet's FailedCreate event is printed" || fail "status: FailedCreate event missing: $(tail -2 "$T/nopods.out")"; fi
cp "$T/pods-all.json" "$T/pods.json"

# a node list the caller may not read: an error alone, a warning after apply
if STUB_NODES_FORBIDDEN=1 PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" > "$T/forbidden.out" 2>&1; then fail "status must fail when nodes cannot be listed"; else
    grep -q 'cluster-reader' "$T/forbidden.out" && ok "status: a Forbidden node list fails with the permission named" || fail "status: Forbidden handling: $(tail -1 "$T/forbidden.out")"; fi
if STUB_NODES_FORBIDDEN=1 PATH="$STUB_PATH" bash deploy/prepull.sh apply -n check-ns --image "$IMG" > "$T/apply-forbidden.out" 2>&1; then
    grep -q 'cannot list nodes' "$T/apply-forbidden.out" && ! grep -q '0/ nodes' "$T/apply-forbidden.out" && ok "apply: a Forbidden node list warns and does not print a 0/ tally" || fail "apply on Forbidden: $(tail -2 "$T/apply-forbidden.out")"
else
    fail "apply must succeed when the report cannot list nodes"
fi

# no jq: apply still succeeds (the report is skipped with a warning), status refuses
mkdir -p "$T/nojq"
for tool in bash sed awk tr printf cat grep head cut mktemp rm sort uniq shasum sha256sum env dirname basename readlink date id uname wc true false; do
    p="$(command -v "$tool" 2>/dev/null || true)"; [ -n "$p" ] && ln -sf "$p" "$T/nojq/$tool"
done
if out="$(PATH="$T/bin:$T/nojq" bash deploy/prepull.sh apply -n check-ns --image "$IMG" 2>&1)"; then
    case "$out" in *"jq is not installed"*) ok "apply: without jq the apply succeeds and the report is skipped with a warning" ;; *) fail "apply without jq: $out" ;; esac
else
    fail "apply must not fail for a missing jq after the DaemonSet was applied: $out"
fi
if PATH="$T/bin:$T/nojq" bash deploy/prepull.sh status -n check-ns --image "$IMG" >/dev/null 2>&1; then fail "status without jq must fail"; else ok "status: without jq refuses with a reason"; fi

# no node matches: verdict mode exits, report mode (after apply) warns and continues
cp "$T/nodes.json" "$T/nodes-all.json"; printf '{"items":[]}' > "$T/nodes.json"
if PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" >/dev/null 2>&1; then fail "status with no matching node must fail"; else ok "status: no matching node is an error"; fi
if PATH="$STUB_PATH" bash deploy/prepull.sh apply -n check-ns --image "$IMG" > "$T/apply.out" 2>&1; then
    grep -q 'will run nowhere' "$T/apply.out" && ok "apply: a placement matching no node warns and still succeeds" || fail "apply: the no-node warning is missing"
else
    fail "apply must not fail because no node matched (log_error inside status ended the process)"
fi
cp "$T/nodes-all.json" "$T/nodes.json"

# the selector reaches kubectl as given
: > "$T/calls"
STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" bash deploy/prepull.sh status -n check-ns --image "$IMG" --node-selector example.com/accelerator=h200 >/dev/null 2>&1 || true
grep -q 'get nodes -l example.com/accelerator=h200' "$T/calls" && ok "status: --node-selector is what kubectl is asked with" || fail "status: selector not passed: $(grep 'get nodes' "$T/calls")"

# delete: by name for --image, by both labels for --all; --dry-run makes no call
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/prepull.sh delete -n check-ns --image "$IMG" >/dev/null 2>&1
grep -q "delete daemonset -n check-ns ${DS} --ignore-not-found" "$T/calls" && ok "delete --image: deletes the DaemonSet name_for derives, in the namespace" || fail "delete --image issued: $(grep delete "$T/calls")"
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/prepull.sh delete -n check-ns --all >/dev/null 2>&1
grep -q "delete daemonset -n check-ns -l ${LBL} --ignore-not-found" "$T/calls" && ok "delete --all: deletes by component AND managed-by, in the namespace" || fail "delete --all issued: $(grep delete "$T/calls")"
: > "$T/calls"
out="$(PATH="$STUB_PATH" bash deploy/prepull.sh delete -n check-ns --all --dry-run 2>&1 || true)"
if [ ! -s "$T/calls" ] && [ "${out#would run: kubectl delete daemonset}" != "$out" ]; then ok "delete --dry-run: prints the delete and makes no kubectl call"; else fail "delete --dry-run: calls=$(cat "$T/calls") out=$out"; fi
if bash deploy/prepull.sh apply -n check-ns --image "$IMG" --all --dry-run >/dev/null 2>&1; then fail "apply must refuse --all"; else ok "apply: --all is refused (a delete option)"; fi

# an image reference that is not one is refused before anything is rendered or applied
: > "$T/calls"
for badimg in 'a b' 'a"b' 'a,b' ''; do
    if PATH="$STUB_PATH" bash deploy/prepull.sh apply -n check-ns --image "$badimg" >/dev/null 2>&1; then fail "apply accepted image '$badimg'"; fi
done
[ ! -s "$T/calls" ] && ok "apply: a quote, a space, a comma or an empty --image is refused with no kubectl call" || fail "apply: kubectl was called for a refused image: $(cat "$T/calls")"
out="$(bash deploy/prepull.sh apply -n check-ns --image 2>&1 || true)"
case "$out" in *'needs a value'*) ok "apply: --image as the last argument is a usage error" ;; *) fail "apply: --image with no value is not refused cleanly: $out" ;; esac

# ---------------------------------------------------------------------------
# 4. The Makefile: the comma lists, the NAMESPACE guard, and the standup's
#    pre-pull step -- the recipe fragment itself, executed against the stub,
#    since `make -n` prints the same shape whatever BENCHMARK_PREPULL is.
# ---------------------------------------------------------------------------
line="$(make -n prepull IMAGES=a:1,b:2 NAMESPACE=ns PREPULL_TOLERATIONS=t1,t2 2>/dev/null | grep 'prepull.sh apply' || true)"
case "$line" in
    *"--image a:1 --image b:2"*) ok "make prepull: IMAGES=a,b becomes --image a --image b" ;;
    *) fail "make prepull expanded to: $line" ;;
esac
case "$line" in
    *"--toleration t1 --toleration t2"*) ok "make prepull: PREPULL_TOLERATIONS=a,b becomes --toleration a --toleration b" ;;
    *) fail "make prepull tolerations: $line" ;;
esac
case "$line" in
    *"--node-selector"*) fail "make prepull: an empty PREPULL_NODE_SELECTOR must pass no --node-selector: $line" ;;
    *) ok "make prepull: no PREPULL_NODE_SELECTOR means the product-key affinity (no --node-selector)" ;;
esac
line="$(make -n prepull IMAGES=a:1 NAMESPACE=ns PREPULL_NODE_SELECTOR=k=v 2>/dev/null | grep 'prepull.sh apply' || true)"
case "$line" in *'--node-selector "k=v"'*) ok "make prepull: PREPULL_NODE_SELECTOR is passed through" ;; *) fail "make prepull selector: $line" ;; esac
line="$(make -n prepull-delete NAMESPACE=ns 2>/dev/null | grep 'prepull.sh delete' || true)"
case "$line" in *"--all"*) ok "make prepull-delete with no IMAGES deletes every holder" ;; *) fail "prepull-delete: $line" ;; esac
composed="$(printf '%s' "a:1,b:2" | tr ',' '\n' | sed 's/^/--image /' | tr '\n' ' ')"
[ "$composed" = "--image a:1 --image b:2" ] && ok "standup: the tr/sed composition yields --image a --image b" || fail "standup composition: '$composed'"
# NAMESPACE has a Makefile default; the targets must not take it silently.
# Run, not `make -n`: the guard is a recipe line, and -n prints it unfired.
: > "$T/calls"
if PATH="$STUB_PATH" make prepull-status > "$T/make-nons.out" 2>&1; then fail "make prepull-status without NAMESPACE ran against the Makefile default"; else
    [ ! -s "$T/calls" ] && grep -q 'set NAMESPACE' "$T/make-nons.out" && ok "make prepull-status without NAMESPACE is refused before any kubectl call" || fail "make prepull-status without NAMESPACE: $(tail -1 "$T/make-nons.out"); calls: $(cat "$T/calls")"; fi
: > "$T/calls"
PATH="$STUB_PATH" make prepull-status NAMESPACE=check-ns > "$T/make-ns.out" 2>&1 || true
grep -q '^  node-running ' "$T/make-ns.out" && ok "make prepull-status NAMESPACE=<ns> runs the status against that namespace" || fail "make prepull-status NAMESPACE=check-ns: $(tail -2 "$T/make-ns.out")"

# the standup's pre-pull block, executed: the namespace is created before
# the apply, BENCHMARK_PREPULL=false skips, BENCHMARK_PREPULL_IMAGES overrides
block="$(sed -n '/The engine image on every accelerator node BEFORE/,/Lives in a script, not inline here/p' Makefile | sed '1d;$d' | grep -v '^	@#' | sed 's/^	@//; s/\$\$/$/g; s/\$(BENCHMARK_PREPULL)/${BENCHMARK_PREPULL}/g; s/\$(BENCHMARK_PREPULL_IMAGES)/${BENCHMARK_PREPULL_IMAGES}/g; s/\$(BENCHMARK_REPO_DIR)/${BENCHMARK_REPO_DIR}/g; s/\$(BENCHMARK_NAMESPACE)/${BENCHMARK_NAMESPACE}/g; s/\$(PREPULL_ARGS)//g')"
[ -n "$block" ] || fail "could not extract the benchmark-standup pre-pull block from the Makefile"
run_block() { env BENCHMARK_NAMESPACE=check-ns BENCHMARK_REPO_DIR="$T/clone" PATH="$STUB_PATH" "$@" bash -c "$block"; }
: > "$T/calls"
run_block BENCHMARK_PREPULL=true BENCHMARK_PREPULL_IMAGES= > "$T/standup.out" 2>&1 || fail "standup block failed: $(cat "$T/standup.out")"
if grep -q "^create namespace check-ns" "$T/calls" && grep -q "^apply -n check-ns" "$T/calls" \
   && [ "$(grep -n '^create namespace' "$T/calls" | head -1 | cut -d: -f1)" -lt "$(grep -n '^apply -n' "$T/calls" | head -1 | cut -d: -f1)" ]; then
    ok "standup: the namespace is created before the pre-pull apply"
else
    fail "standup: namespace create missing or after the apply: $(tr '\n' ';' < "$T/calls")"
fi
grep -q "Holding the engine image.*${IMG}" "$T/standup.out" && ok "standup: the image comes from the clone's defaults.yaml" || fail "standup: image not read from the clone: $(head -3 "$T/standup.out")"
: > "$T/calls"
run_block BENCHMARK_PREPULL=false BENCHMARK_PREPULL_IMAGES= > "$T/standup-skip.out" 2>&1 || true
[ ! -s "$T/calls" ] && ok "standup: BENCHMARK_PREPULL=false makes no kubectl call" || fail "standup: BENCHMARK_PREPULL=false still called kubectl: $(tr '\n' ';' < "$T/calls")"
: > "$T/calls"
run_block BENCHMARK_PREPULL=true BENCHMARK_PREPULL_IMAGES=override/engine:9 > "$T/standup-override.out" 2>&1 || true
grep -q 'Holding the engine image.*override/engine:9' "$T/standup-override.out" && ok "standup: BENCHMARK_PREPULL_IMAGES overrides the clone's pin" || fail "standup override: $(head -2 "$T/standup-override.out")"
: > "$T/calls"
run_block BENCHMARK_PREPULL=true BENCHMARK_PREPULL_IMAGES= BENCHMARK_REPO_DIR="$T/nonexistent" > "$T/standup-noclone.out" 2>&1 || true
grep -q 'WARNING: could not read the engine image' "$T/standup-noclone.out" && [ ! -s "$T/calls" ] && ok "standup: no clone means a warning and no apply" || fail "standup without a clone: $(head -2 "$T/standup-noclone.out")"

# ---------------------------------------------------------------------------
# 5. The scenarios: engine containers pull IfNotPresent, the harness's init
#    container is left as it was.
# ---------------------------------------------------------------------------
"$PY" - hack/benchmark/scenarios/guides/*.yaml <<'PYEOF' || FAILED=1
import sys, yaml
bad = []
for path in sys.argv[1:]:
    d = yaml.safe_load(open(path, encoding="utf-8"))
    engines = inits = 0
    def walk(o):
        global engines, inits
        if isinstance(o, dict):
            ecc = o.get("extraContainerConfig")
            if isinstance(ecc, dict) and "imagePullPolicy" in ecc:
                engines += 1
                if ecc["imagePullPolicy"] != "IfNotPresent":
                    bad.append("%s: engine container pulls %s" % (path, ecc["imagePullPolicy"]))
            for ic in o.get("initContainers") or []:
                if isinstance(ic, dict) and ic.get("name") == "preprocess":
                    inits += 1
                    if ic.get("imagePullPolicy") != "Always":
                        bad.append("%s: the preprocess init container was changed to %s" % (path, ic.get("imagePullPolicy")))
            for v in o.values():
                walk(v)
        elif isinstance(o, list):
            for v in o:
                walk(v)
    walk(d)
    if engines == 0:
        bad.append("%s: no engine container pull policy found (extraContainerConfig.imagePullPolicy)" % path)
if bad:
    for b in bad:
        print("  FAIL " + b)
    sys.exit(1)
print("  ok   every scenario's engine container pulls IfNotPresent; the preprocess init container still pulls Always")
PYEOF

if [ "$FAILED" -ne 0 ]; then
    echo "prepull checks: FAIL"
    exit 1
fi
echo "prepull checks OK"
