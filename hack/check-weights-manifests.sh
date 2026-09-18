#!/usr/bin/env bash
# Executes deploy/weights.sh and hack/benchmark/model_hostpath.sh against
# fixtures and asserts what they do.
#
# Every defect this covers parses fine: a volume without a claimRef (the
# first claim asking for the class takes it), a claim larger than its volume
# (never binds), a downloader that mounts a hostPath directly (Pod Security
# baseline refuses it; via the claim it is admitted), a readiness probe on
# the wrong path (Ready before the download ends, which is what the harness
# and `status` both read), a model id that reaches a Python string or a path
# unchecked, a `status` that reads a still-downloading node as present, and
# a scenario edit that lands the hostPath block somewhere the harness does
# not read or misses the second layout the scenarios use.
#
# `status`, `delete` and the scenario edit run offline against a stub
# kubectl on PATH, the pattern hack/check-prepull-manifests.sh uses.
set -euo pipefail

command -v jq >/dev/null 2>&1 || { printf 'FATAL: jq is required to run these checks.\n' >&2; exit 1; }
command -v yq >/dev/null 2>&1 || { printf 'FATAL: yq is required to run these checks (the scenario edit uses it).\n' >&2; exit 1; }
cd "$(dirname "$0")/.."

PY=${PYTHON:-python3}
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
FAILED=0
ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

# ---------------------------------------------------------------------------
# 1. The rendered volume, claim and downloader.
# ---------------------------------------------------------------------------
IMG=docker.io/vllm/vllm-openai:v0.26.0
bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" \
    --hf-token-secret hf-token/token --capacity 2Ti --node-selector example.com/accelerator=h200 --toleration example.com/dedicated \
    --dry-run > "$T/render.yaml"
bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" --dry-run > "$T/render-default.yaml"
# the same model in another namespace: a different volume, the same claim name
bash deploy/weights.sh apply -n other-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" --dry-run > "$T/render-other.yaml"

"$PY" - "$T/render.yaml" "$T/render-default.yaml" "$T/render-other.yaml" deploy/lib/accelerator_labels.py <<'PYEOF' || FAILED=1
import re, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d]
default = [d for d in yaml.safe_load_all(open(sys.argv[2], encoding="utf-8")) if d]
other = [d for d in yaml.safe_load_all(open(sys.argv[3], encoding="utf-8")) if d]
ns = {}
exec(open(sys.argv[4], encoding="utf-8").read(), ns)
product_keys = ns["PRODUCT_KEYS"]
bad = []
def check(cond, msg):
    if not cond:
        bad.append(msg)

check([d["kind"] for d in docs] == ["PersistentVolume", "PersistentVolumeClaim", "DaemonSet"], "kinds: %s" % [d["kind"] for d in docs])
pv, pvc, ds = docs
label = re.compile(r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$")
check(bool(label.match(pvc["metadata"]["name"])) and bool(label.match(ds["metadata"]["name"])), "claim/DaemonSet names are DNS-1123 labels")
check(pvc["metadata"]["name"] == ds["metadata"]["name"], "claim and DaemonSet share a name")
check(pv["spec"].get("claimRef") == {"namespace": "check-ns", "name": pvc["metadata"]["name"]}, "the volume is bound to this claim and no other: %s" % pv["spec"].get("claimRef"))
check(pvc["spec"]["volumeName"] == pv["metadata"]["name"], "the claim names the volume")
check(pv["spec"]["storageClassName"] == pvc["spec"]["storageClassName"] == "node-local-weights", "one synthetic storage class on both sides")
check(pv["spec"]["capacity"]["storage"] == pvc["spec"]["resources"]["requests"]["storage"] == "2Ti", "--capacity reaches both, equal (a larger request never binds)")
check(pv["spec"]["persistentVolumeReclaimPolicy"] == "Retain", "Retain: the files are the point")
check(pv["spec"]["hostPath"] == {"path": "/mnt/local/models", "type": "DirectoryOrCreate"}, "hostPath: %s" % pv["spec"]["hostPath"])
check(pv["spec"]["accessModes"] == ["ReadWriteMany"] == pvc["spec"]["accessModes"], "RWX on both: every node mounts the same claim")
for d in docs:
    check(d["metadata"]["annotations"]["wva.llmd.ai/weights-model"] == "Qwen/Qwen3-32B", "annotation names the model")
    check(d["metadata"]["labels"].get("app.kubernetes.io/managed-by") == "wva-weights", "managed-by on %s" % d["kind"])
check(ds["metadata"]["annotations"]["wva.llmd.ai/weights-node-selector"] == "example.com/accelerator=h200", "the DaemonSet records the selector")
tpl = ds["spec"]["template"]; ps = tpl["spec"]
check(tpl["metadata"]["labels"].get("app.kubernetes.io/managed-by") == "wva-weights", "pods carry managed-by")
check(all(tpl["metadata"]["labels"].get(k) == v for k, v in ds["spec"]["selector"]["matchLabels"].items()), "selector matches the template labels")
check(ps["nodeSelector"] == {"example.com/accelerator": "h200"} and "affinity" not in ps, "an explicit selector is a nodeSelector, no affinity")
check({t["key"] for t in ps["tolerations"]} == {"nvidia.com/gpu", "example.com/dedicated"}, "tolerations: %s" % ps["tolerations"])
check(ps.get("automountServiceAccountToken") is False, "no ServiceAccount token")
check((ps.get("securityContext") or {}).get("seccompProfile", {}).get("type") == "RuntimeDefault", "seccomp RuntimeDefault")
vols = ps["volumes"]
check(len(vols) == 1 and vols[0].get("persistentVolumeClaim", {}).get("claimName") == pvc["metadata"]["name"], "the downloader mounts the CLAIM, not a hostPath: %s" % vols)
check(all("hostPath" not in v for v in vols), "no hostPath volume in the pod (Pod Security baseline)")
c = ps["containers"][0]
check(c["image"] == "docker.io/vllm/vllm-openai:v0.26.0" and c["imagePullPolicy"] == "IfNotPresent", "downloads with the given image, IfNotPresent")
env = {e["name"]: e for e in c["env"]}
check(env["MODEL_ID"]["value"] == "Qwen/Qwen3-32B", "MODEL_ID")
check(env["TARGET_DIR"]["value"] == "/weights/models/Qwen/Qwen3-32B", "the model lands under <mount>/models/<id>, the harness's layout: %s" % env["TARGET_DIR"]["value"])
check(env["NVIDIA_VISIBLE_DEVICES"]["value"] == "void", "no GPU injected into the downloader")
check(env["HF_TOKEN"]["valueFrom"]["secretKeyRef"] == {"name": "hf-token", "key": "token"}, "the token comes from the named Secret and key: %s" % env["HF_TOKEN"])
mount = c["volumeMounts"][0]
check(mount["name"] == vols[0]["name"] and mount["mountPath"] == "/weights", "the claim is mounted at /weights")
probe = c["readinessProbe"]["exec"]["command"]
check(probe == ["test", "-f", "/weights/models/Qwen/Qwen3-32B/.download-complete"], "Ready is the completion marker under the mount: %s" % probe)
check(env["MARKER"]["value"] == ".download-complete", "the marker the script writes is the one the probe tests")
script = c["args"][0]
check("snapshot_download" in script and "$(" not in script and "$$" not in script, "the download script uses huggingface_hub and carries no $( or $$ (Kubernetes rewrites both)")
check(c["command"] == ["/bin/sh", "-c"], "sh -c with the script as args")
res = c.get("resources", {})
for section in ("requests", "limits"):
    for k in res.get(section, {}):
        check("gpu" not in k and "/" not in k, "the downloader requests an accelerator: %s" % k)
check(res.get("limits", {}).get("memory"), "a memory limit")
csc = c.get("securityContext", {})
check(csc.get("allowPrivilegeEscalation") is False and "ALL" in csc.get("capabilities", {}).get("drop", []), "drops all capabilities")

check(len(default) == 3, "defaults render three documents")
if len(default) == 3:
    dps = default[2]["spec"]["template"]["spec"]
    check("nodeSelector" not in dps, "no --node-selector: no nodeSelector")
    terms = dps.get("affinity", {}).get("nodeAffinity", {}).get("requiredDuringSchedulingIgnoredDuringExecution", {}).get("nodeSelectorTerms", [])
    keys = [t["matchExpressions"][0]["key"] for t in terms if len(t.get("matchExpressions", [])) == 2]
    check(keys == product_keys, "the default affinity is the product-key list: %s" % keys)
    check(default[1]["spec"]["resources"]["requests"]["storage"] == "1Ti", "default capacity 1Ti")
    check("HF_TOKEN" not in {e["name"] for e in dps["containers"][0]["env"]}, "no --hf-token-secret: no HF_TOKEN env")
    check(dps["tolerations"] == [{"key": "nvidia.com/gpu", "operator": "Exists"}], "default toleration")
if len(other) == 3:
    check(other[0]["metadata"]["name"] != default[0]["metadata"]["name"], "the same model in another namespace gets its own (cluster-scoped) volume")
    check(other[1]["metadata"]["name"] == default[1]["metadata"]["name"], "and the same claim name (namespace-scoped)")
    check(other[0]["spec"]["claimRef"]["namespace"] == "other-ns", "bound into its own namespace")

if bad:
    for b in bad:
        print("  FAIL " + b)
    sys.exit(1)
print("  ok   volume, claim and downloader rendered: bound pair, claim-mounted (no hostPath in the pod), marker probe, layout, placement, no GPU, no token")
PYEOF

for badmodel in 'a/b/c' '/x' 'x/' 'a b' 'a"b' ''; do
    if bash deploy/weights.sh apply -n check-ns --model "$badmodel" --path /p --image "$IMG" --dry-run >/dev/null 2>&1; then fail "a --model of '$badmodel' must be refused"; fi
done
ok "a model id that is not [org/]name of Hugging Face characters is refused"
for badpath in relative / 'a b' '/p"' ''; do
    if bash deploy/weights.sh apply -n check-ns --model m --path "$badpath" --image "$IMG" --dry-run >/dev/null 2>&1; then fail "a --path of '$badpath' must be refused"; fi
done
ok "a path that is not absolute and of path characters is refused"
if bash deploy/weights.sh apply -n check-ns --model m --path /p --image "$IMG" --hf-token-secret 'a b' --dry-run >/dev/null 2>&1; then fail "a Secret reference with a space must be refused"; else ok "a Secret reference that is not NAME[/KEY] is refused"; fi
if bash deploy/weights.sh apply -n check-ns --model m --path /p --image "$IMG" --capacity '1Ti; rm' --dry-run >/dev/null 2>&1; then fail "a capacity that is not a quantity must be refused"; else ok "a capacity that is not a quantity is refused"; fi
if bash deploy/weights.sh apply -n check-ns --model m --path /p --dry-run >/dev/null 2>&1; then fail "apply without --image must be refused"; else ok "apply without --image is refused"; fi
if bash deploy/weights.sh apply -n check-ns --model m --path /p --image "$IMG" --all --dry-run >/dev/null 2>&1; then fail "apply must refuse --all"; else ok "apply: --all is refused"; fi

# ---------------------------------------------------------------------------
# 2. status and delete, offline.
# ---------------------------------------------------------------------------
LBL='app.kubernetes.io/component=node-local-weights,app.kubernetes.io/managed-by=wva-weights'
NAME="$(yq -r 'select(.kind == "PersistentVolumeClaim") | .metadata.name' "$T/render-default.yaml")"
PVNAME="$(yq -r 'select(.kind == "PersistentVolume") | .metadata.name' "$T/render-default.yaml")"
[ -n "$NAME" ] && [ -n "$PVNAME" ] || fail "no claim/volume name in the rendered manifests"
cat > "$T/nodes.json" <<EOF
{"items":[
 {"metadata":{"name":"node-ready",   "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-loading", "labels":{"gpu.nvidia.com/model":"H200"}}},
 {"metadata":{"name":"node-failing", "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-nopod",   "labels":{"nvidia.com/gpu.product":"H200"}}},
 {"metadata":{"name":"node-cpu",     "labels":{"gpu.nvidia.com/model":""}}}
]}
EOF
cat > "$T/pods.json" <<EOF
{"items":[
 {"metadata":{"labels":{"wva.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-ready"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-loading"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"running":{}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-failing"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/weights":"other"}},"spec":{"nodeName":"node-nopod"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}}
]}
EOF
mkdir -p "$T/bin"
cat > "$T/bin/kubectl" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$T/calls"
ARGV=" \$* "
want() { case "\$ARGV" in *" \$1 \$2 "*) ;; *) echo "stub kubectl: \$1 \$2 missing from:\$ARGV" >&2; exit 2 ;; esac; }
refuse() { case "\$ARGV" in *" \$1 "*) echo "stub kubectl: unexpected \$1 in:\$ARGV" >&2; exit 2 ;; esac; }
case "\$1 \$2" in
  "get nodes")   if [ -n "\${STUB_SELECTOR:-}" ]; then want -l "\$STUB_SELECTOR"; else refuse -l; fi; cat "$T/nodes.json" ;;
  "get pods")    want -n check-ns; want -l "$LBL"; cat "$T/pods.json" ;;
  "get daemonset")
      want -n check-ns
      case "\$ARGV" in *" -l "*) want -l "$LBL"; printf '%s\n' "\${STUB_DAEMONSETS-Qwen/Qwen3-32B}" ;; *) printf '%s' "\${STUB_DS_SELECTOR:-}" ;; esac ;;
  "get pvc")
      want -n check-ns
      case "\$ARGV" in
        *"jsonpath={.status.phase}"*) printf '%s' "\${STUB_PVC_PHASE-Bound}" ;;
        *"jsonpath={range"*) printf '%s\n' "\${STUB_PVC_VOLUMES-${PVNAME}}" ;;
        *"model-pvc"*) printf '%s' "\${STUB_MODEL_PVC_CLASS:-}" ;;
        *) echo "stub kubectl: unexpected pvc read:\$ARGV" >&2; exit 2 ;;
      esac ;;
  "get events")  want -n check-ns; printf '%s' "\${STUB_EVENT:-}" ;;
  "apply -f")    cat >/dev/null; echo applied ;;
  "delete daemonset"|"delete pvc") want -n check-ns; echo deleted ;;
  "delete pv")   refuse -n; echo deleted ;;
  *) echo "stub kubectl: unexpected \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/bin/kubectl"
STUB_PATH="$T/bin:$PATH"

set +e
PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/status.out" 2>&1
rc=$?
set -e
expect_line() { if grep -qE "^  $1 +$2 " "$T/status.out"; then ok "status: $1 -> $2"; else fail "status: $1 expected $2; got: $(grep -E "^  $1 " "$T/status.out" || echo none)"; fi; }
expect_line node-ready   present
expect_line node-loading downloading
expect_line node-failing absent
grep -qF 'node-failing                 absent       Running (not ready) CrashLoopBackOff' "$T/status.out" && ok "status: a CrashLoopBackOff downloader is not 'downloading'" || fail "status: failing line: $(grep node-failing "$T/status.out")"
expect_line node-nopod   absent
grep -q '^  node-cpu ' "$T/status.out" && fail "status: a node with an empty product label was counted" || ok "status: the default placement leaves the empty-label node out"
grep -q 'node-failing .*CrashLoopBackOff' "$T/status.out" && ok "status: a failing downloader shows its reason" || fail "status: reason missing"
grep -q '1/4 nodes hold it; 3 do not' "$T/status.out" && ok "status: the tally counts only Ready (download complete) as holding" || fail "status: tally: $(grep 'nodes hold' "$T/status.out")"
grep -q "claim ${NAME}: Bound" "$T/status.out" && ok "status: reports the claim Bound" || fail "status: claim line: $(head -1 "$T/status.out")"
[ "$rc" -ne 0 ] && ok "status: exits non-zero while a node lacks the weights" || fail "status: exit 0 with nodes lacking the weights"
grep -q 'stub kubectl:' "$T/status.out" && fail "status: a kubectl call lacked its scope: $(grep 'stub kubectl:' "$T/status.out" | head -1)" || ok "status: every kubectl call carried -n / -l"
grep -q "^Qwen/Qwen3-32B  (claim" "$T/status.out" && ok "status: with no --model the models are discovered from the DaemonSets" || fail "status: discovery: $(head -1 "$T/status.out")"
if STUB_PVC_PHASE=Pending PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/pending.out" 2>&1; then fail "status must fail while the claim is not Bound"; else grep -q 'Pending' "$T/pending.out" && ok "status: an unbound claim is reported and fails" || fail "status: unbound claim: $(head -1 "$T/pending.out")"; fi
if STUB_DAEMONSETS="" PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/none.out" 2>&1; then fail "status with nothing to check must fail"; else grep -q 'no weights DaemonSets' "$T/none.out" && ok "status: no DaemonSets and no --model is refused with a reason" || fail "status: $(tail -1 "$T/none.out")"; fi
: > "$T/calls"
STUB_DS_SELECTOR=example.com/accelerator=h200 STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns >/dev/null 2>&1 || true
grep -q 'get nodes -l example.com/accelerator=h200' "$T/calls" && ok "status: lists the nodes the DaemonSet was applied for" || fail "status: recorded selector not used: $(grep 'get nodes' "$T/calls")"

# delete: DaemonSet and claim in the namespace, the volume by the name read off the claim
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --model Qwen/Qwen3-32B >/dev/null 2>&1
grep -q "^delete daemonset -n check-ns ${NAME} --ignore-not-found" "$T/calls" && grep -q "^delete pvc -n check-ns ${NAME} --ignore-not-found" "$T/calls" \
    && grep -q "^delete pv ${PVNAME} --ignore-not-found" "$T/calls" && ok "delete --model: DaemonSet, claim, and the volume read off the claim" || fail "delete --model issued: $(grep delete "$T/calls" | tr '\n' ';')"
[ "$(grep -n '^get pvc' "$T/calls" | head -1 | cut -d: -f1)" -lt "$(grep -n '^delete pvc' "$T/calls" | head -1 | cut -d: -f1)" ] && ok "delete: the volume name is read before the claim goes" || fail "delete: claim deleted before its volume name was read"
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --all >/dev/null 2>&1
grep -q "^delete daemonset -n check-ns -l ${LBL} --ignore-not-found" "$T/calls" && grep -q "^delete pvc -n check-ns -l ${LBL} --ignore-not-found" "$T/calls" && ok "delete --all: by both labels" || fail "delete --all issued: $(grep delete "$T/calls" | tr '\n' ';')"
: > "$T/calls"
STUB_PVC_VOLUMES="" PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --all >/dev/null 2>&1
grep -q '^delete pv ' "$T/calls" && fail "delete: with no claims there is no volume to delete" || ok "delete --all with nothing to delete issues no pv delete"
: > "$T/calls"
out="$(PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --all --dry-run 2>&1 || true)"
grep -q '^delete' "$T/calls" && fail "delete --dry-run deleted: $(grep delete "$T/calls")" || { case "$out" in *"would run: kubectl delete daemonset"*) ok "delete --dry-run prints and deletes nothing" ;; *) fail "delete --dry-run: $out" ;; esac; }

# ---------------------------------------------------------------------------
# 3. model_hostpath.sh: both scenario layouts, the guard, the placement.
# ---------------------------------------------------------------------------
cat > "$T/nested.yaml" <<'EOF'
# a scenario that keeps storage under scenario[].common
scenario:
  - name: one
    common:
      model:
        path: models/Qwen/Qwen3-0.6B
      storage:
        modelPvc:
          size: 40Gi   # the claim
          accessModes: [ReadWriteMany]
    harness:
      name: x
EOF
cat > "$T/shared.yaml" <<'EOF'
shared:
  storage:
    modelPvc:
      size: 32Gi
scenario:
  - name: a
    model: {path: models/a}
  - name: b
    model: {path: models/b}
EOF
cp "$T/nested.yaml" "$T/n1.yaml"
PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n1.yaml" /mnt/local/wva check-ns > "$T/hp1.out" 2>&1 || fail "model_hostpath on the nested layout failed: $(cat "$T/hp1.out")"
"$PY" - "$T/n1.yaml" deploy/lib/accelerator_labels.py <<'PYEOF' || FAILED=1
import sys, yaml
d = yaml.safe_load(open(sys.argv[1]))
ns = {}; exec(open(sys.argv[2]).read(), ns)
hp = d["scenario"][0]["common"]["storage"]["hostPath"]
bad = []
def check(c, m):
    if not c: bad.append(m)
check(hp["enabled"] is True and hp["path"] == "/mnt/local/wva", "enabled with the path: %s" % hp)
check(hp["capacity"] == "40Gi", "capacity is the claim's size (a larger request never binds): %s" % hp.get("capacity"))
check(hp["storageClassName"] == "node-local-weights", "storage class")
check("nodeSelector" not in hp, "no selector given: no nodeSelector")
terms = hp["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
check([t["matchExpressions"][0]["key"] for t in terms] == ns["PRODUCT_KEYS"], "the affinity is the product-key list")
check(all(t["matchExpressions"][1] == {"key": t["matchExpressions"][0]["key"], "operator": "NotIn", "values": [""]} for t in terms), "each term requires a non-empty label")
check(hp["tolerations"] == [{"key": "nvidia.com/gpu", "operator": "Exists"}], "tolerates the accelerator taint")
check(d["scenario"][0]["common"]["storage"]["modelPvc"]["size"] == "40Gi" and d["scenario"][0]["harness"] == {"name": "x"}, "nothing else changed")
if bad:
    [print("  FAIL " + b) for b in bad]; sys.exit(1)
print("  ok   model_hostpath: nested layout gets the block under common.storage with the affinity and the claim's size")
PYEOF
grep -q '^#' "$T/n1.yaml" && ok "model_hostpath: the scenario keeps its comments (yq, not a dump)" || fail "model_hostpath: comments lost"
cp "$T/shared.yaml" "$T/s1.yaml"
PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/s1.yaml" /mnt/local/wva check-ns example.com/accelerator=h200 > "$T/hp2.out" 2>&1 || fail "model_hostpath on the shared layout failed: $(cat "$T/hp2.out")"
sel="$(yq -r '.shared.storage.hostPath.nodeSelector["example.com/accelerator"] // ""' "$T/s1.yaml")"
[ "$sel" = h200 ] && [ "$(yq -r '.shared.storage.hostPath.affinity // "none"' "$T/s1.yaml")" = none ] && ok "model_hostpath: shared layout, and a KEY=VALUE becomes a nodeSelector with no affinity" || fail "model_hostpath shared: $(yq '.shared.storage.hostPath' "$T/s1.yaml")"
[ "$(yq -r '.shared.storage.hostPath.capacity' "$T/s1.yaml")" = 32Gi ] && ok "model_hostpath: capacity follows the shared claim's size" || fail "model_hostpath: shared capacity"
# the guard: an existing claim on another class is refused before any edit
cp "$T/nested.yaml" "$T/n2.yaml"
if STUB_MODEL_PVC_CLASS=shared-vast PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n2.yaml" /mnt/local/wva check-ns > "$T/hp3.out" 2>&1; then fail "model_hostpath must refuse a namespace whose model-pvc is on another class"; else
    grep -q "already has model-pvc on storage class 'shared-vast'" "$T/hp3.out" && cmp -s "$T/nested.yaml" "$T/n2.yaml" && ok "model_hostpath: an existing model-pvc on another class is refused, scenario untouched" || fail "model_hostpath guard: $(tail -1 "$T/hp3.out")"; fi
cp "$T/nested.yaml" "$T/n3.yaml"
if STUB_MODEL_PVC_CLASS=node-local-weights PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n3.yaml" /mnt/local/wva check-ns >/dev/null 2>&1; then ok "model_hostpath: an existing claim already on the node-local class is fine (a re-standup)"; else fail "model_hostpath refused a claim on its own class"; fi
for badp in relative / '/a b'; do
    cp "$T/nested.yaml" "$T/n4.yaml"
    if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n4.yaml" "$badp" check-ns >/dev/null 2>&1; then fail "model_hostpath accepted the path '$badp'"; fi
done
ok "model_hostpath: a relative, root or unusual path is refused"
cp "$T/nested.yaml" "$T/n5.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n5.yaml" /mnt/local/wva check-ns 'a=b"' >/dev/null 2>&1; then fail "model_hostpath accepted a selector with a quote"; else ok "model_hostpath: a selector that is not one KEY=VALUE is refused"; fi
printf 'scenario:\n  - name: x\n    common: {}\n' > "$T/n6.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n6.yaml" /mnt/local/wva check-ns >/dev/null 2>&1; then fail "model_hostpath must fail on a scenario with no model claim"; else ok "model_hostpath: a scenario with no modelPvc is refused, not silently left on the shared volume"; fi

# ---------------------------------------------------------------------------
# 4. The Makefile: the targets' argv and the standup's step.
# ---------------------------------------------------------------------------
# the recipe is one command over two lines; join them before matching
line="$(make -n weights WEIGHTS_MODEL=Qwen/Qwen3-32B WEIGHTS_PATH=/mnt/local/w WEIGHTS_IMAGE=i:1 WEIGHTS_HF_TOKEN_SECRET=hf NAMESPACE=ns PREPULL_TOLERATIONS=t1 2>/dev/null | tr -d '\\\n' | grep -o 'bash deploy/weights.sh apply.*' || true)"
case "$line" in
    *'--model "Qwen/Qwen3-32B" --path "/mnt/local/w" --image "i:1"'*'--hf-token-secret "hf"'*'--toleration t1'*) ok "make weights: model, path, image, secret and the PREPULL_* tolerations reach the script" ;;
    *) fail "make weights expanded to: $line" ;;
esac
case "$line" in *--node-selector*) fail "make weights: an empty selector must pass no --node-selector" ;; *) ok "make weights: no selector means the product-key affinity" ;; esac
line="$(make -n weights-delete NAMESPACE=ns 2>/dev/null | grep 'weights.sh delete' || true)"
case "$line" in *"--all"*) ok "make weights-delete with no WEIGHTS_MODEL deletes every model's holder" ;; *) fail "weights-delete: $line" ;; esac
: > "$T/calls"
if PATH="$STUB_PATH" make weights-status > "$T/make-nons.out" 2>&1; then fail "make weights-status without NAMESPACE ran against the Makefile default"; else
    [ ! -s "$T/calls" ] && ok "make weights-status without NAMESPACE is refused before any kubectl call" || fail "make weights-status without NAMESPACE called kubectl: $(cat "$T/calls")"; fi
: > "$T/calls"
if PATH="$STUB_PATH" make weights WEIGHTS_PATH=/p WEIGHTS_IMAGE=i NAMESPACE=ns >/dev/null 2>&1; then fail "make weights without WEIGHTS_MODEL must be refused"; else
    [ ! -s "$T/calls" ] && ok "make weights without WEIGHTS_MODEL is refused before any kubectl call" || fail "make weights without WEIGHTS_MODEL called kubectl"; fi
line="$(make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_MODEL_HOSTPATH=/mnt/local/w BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null | grep -A2 'model_hostpath.sh' || true)"
case "$line" in
    *'model_hostpath.sh'*'guides/pd-disaggregation.yaml'*'"/mnt/local/w" "ns"'*) ok "benchmark-standup: BENCHMARK_MODEL_HOSTPATH runs model_hostpath.sh on the scenario copy with the namespace" ;;
    *) fail "benchmark-standup model_hostpath step: $line" ;;
esac
n="$(make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_MODEL_HOSTPATH=/mnt/local/w BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null | grep -n 'model_hostpath.sh\|standup \\$' | head -2 | tr '\n' ' ')"
case "$n" in *model_hostpath*standup*) ok "benchmark-standup: the scenario edit precedes the harness standup" ;; *) fail "benchmark-standup ordering: $n" ;; esac

# ---------------------------------------------------------------------------
# 5. patch_harness.sh fix 11, run on a fixture carrying the upstream anchors:
#    the probe, the affinity, the best-effort chcon; idempotent; and the
#    earlier form (probe + affinity, chcon still fatal) completed.
# ---------------------------------------------------------------------------
sed -n '/fix 11 (download DaemonSet readiness + affinity) failed"/,/^PYEOF$/p' hack/benchmark/patch_harness.sh | sed '1d;$d' > "$T/fix11.py"
[ -s "$T/fix11.py" ] || fail "could not extract fix 11 from patch_harness.sh"
cat > "$T/03.j2" <<'EOF2'
      serviceAccountName: {{ serviceAccount.name }}
{% if storage.hostPath.tolerations is defined and storage.hostPath.tolerations %}
      tolerations:
{{ storage.hostPath.tolerations | toyaml | indent(8, true) }}
{% endif %}
      containers:
        - name: downloader
          image: {{ images.benchmark.repository }}:{{ images.benchmark.tag }}
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -e
              hf download "{{ model.huggingfaceId }}" --local-dir "${TARGET_DIR}"
              echo "Relabelling for non-spc_t readers..."
              chcon -R -t container_file_t "{{ storage.hostPath.path }}"
              touch "${MARKER}"
EOF2
cp "$T/03.j2" "$T/03-orig.j2"
out="$("$PY" "$T/fix11.py" "$T/03.j2" 2>&1 || true)"
case "$out" in *"applied"*) ok "fix 11: applies to the upstream anchors" ;; *) fail "fix 11 on the fixture: $out" ;; esac
grep -q 'readinessProbe' "$T/03.j2" && grep -q '"test", "-f", "{{ storage.hostPath.path }}/{{ model.path }}/.download-complete"' "$T/03.j2" && ok "fix 11: the readiness probe tests the marker under the hostPath" || fail "fix 11: probe missing or wrong"
grep -q 'storage.hostPath.affinity is defined' "$T/03.j2" && ok "fix 11: an optional affinity renders" || fail "fix 11: affinity block missing"
grep -q 'chcon -R -t container_file_t "{{ storage.hostPath.path }}" 2>/dev/null || echo' "$T/03.j2" && ok "fix 11: the SELinux relabel is best-effort (it ended the script on a node without SELinux, before the marker)" || fail "fix 11: chcon still fatal"
out="$("$PY" "$T/fix11.py" "$T/03.j2" 2>&1 || true)"
case "$out" in *"already applied"*) ok "fix 11: idempotent" ;; *) fail "fix 11 second run: $out" ;; esac
# the earlier form: probe + affinity applied, chcon untouched -- completed, not refused
sed 's|chcon -R -t container_file_t "{{ storage.hostPath.path }}" 2>/dev/null .*|chcon -R -t container_file_t "{{ storage.hostPath.path }}"|' "$T/03.j2" > "$T/03-earlier.j2"
out="$("$PY" "$T/fix11.py" "$T/03-earlier.j2" 2>&1 || true)"
case "$out" in *"chcon made best-effort"*) ok "fix 11: the earlier form (fatal chcon) is completed in place" ;; *) fail "fix 11 on the earlier form: $out" ;; esac
sed 's/images.benchmark.repository/images.other.repository/' "$T/03-orig.j2" > "$T/03-drift.j2"
if "$PY" "$T/fix11.py" "$T/03-drift.j2" >/dev/null 2>&1; then fail "fix 11 must fail on a missing anchor, not skip"; else ok "fix 11: a missing anchor is a hard error"; fi

if [ "$FAILED" -ne 0 ]; then
    echo "weights checks: FAIL"
    exit 1
fi
echo "weights checks OK"
