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
bash deploy/weights.sh apply -n check-ns --model gpt2 --path /mnt/local/models --image "$IMG" --hf-token-secret myonlysecret --dry-run > "$T/render-secret.yaml"
# the same model in another namespace: a different volume, the same claim name
bash deploy/weights.sh apply -n other-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" --dry-run > "$T/render-other.yaml"

"$PY" - "$T/render.yaml" "$T/render-default.yaml" "$T/render-other.yaml" deploy/lib/accelerator_labels.py "$T/render-secret.yaml" <<'PYEOF' || FAILED=1
import re, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d]
default = [d for d in yaml.safe_load_all(open(sys.argv[2], encoding="utf-8")) if d]
other = [d for d in yaml.safe_load_all(open(sys.argv[3], encoding="utf-8")) if d]
secret = [d for d in yaml.safe_load_all(open(sys.argv[5], encoding="utf-8")) if d]
ns = {}
exec(open(sys.argv[4], encoding="utf-8").read(), ns)
product_keys = ns["PRODUCT_KEYS"]
bad = []
def check(cond, msg):
    if not cond:
        bad.append(msg)

check([d["kind"] for d in docs] == ["ServiceAccount", "PersistentVolume", "PersistentVolumeClaim", "DaemonSet"], "kinds: %s" % [d["kind"] for d in docs])
sa, pv, pvc, ds = docs
check(sa["metadata"]["name"] == "weights-downloader" and sa["metadata"]["namespace"] == "check-ns" and sa.get("automountServiceAccountToken") is False,
      "a ServiceAccount of its own, with no token: %s" % sa)
check(ds["spec"]["template"]["spec"].get("serviceAccountName") == "weights-downloader", "the downloader runs as its own ServiceAccount, so an SCC granted for it is granted to it alone")
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
    check(d["metadata"]["labels"].get("app.kubernetes.io/managed-by") == "wva-weights", "managed-by on %s" % d["kind"])
for d in (pv, pvc, ds):
    check(d["metadata"].get("annotations", {}).get("wva.llmd.ai/weights-model") == "Qwen/Qwen3-32B", "annotation names the model on %s" % d["kind"])
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

check(len(default) == 4, "defaults render four documents")
if len(default) == 4:
    dps = default[3]["spec"]["template"]["spec"]
    check("nodeSelector" not in dps, "no --node-selector: no nodeSelector")
    terms = dps.get("affinity", {}).get("nodeAffinity", {}).get("requiredDuringSchedulingIgnoredDuringExecution", {}).get("nodeSelectorTerms", [])
    keys = [t["matchExpressions"][0]["key"] for t in terms if len(t.get("matchExpressions", [])) == 2]
    check(keys == product_keys, "the default affinity is the product-key list: %s" % keys)
    check(default[2]["spec"]["resources"]["requests"]["storage"] == "1Ti", "default capacity 1Ti")
    check("HF_TOKEN" not in {e["name"] for e in dps["containers"][0]["env"]}, "no --hf-token-secret: no HF_TOKEN env")
    check(dps["tolerations"] == [{"key": "nvidia.com/gpu", "operator": "Exists"}], "default toleration")
if len(secret) == 4:
    senv = {e["name"]: e for e in secret[3]["spec"]["template"]["spec"]["containers"][0]["env"]}
    check(senv["HF_TOKEN"]["valueFrom"]["secretKeyRef"] == {"name": "myonlysecret", "key": "HF_TOKEN"}, "a Secret named without /KEY uses the key HF_TOKEN: %s" % senv["HF_TOKEN"])
    check(senv["TARGET_DIR"]["value"] == "/weights/models/gpt2", "a model id with no org lands under models/<id>: %s" % senv["TARGET_DIR"]["value"])
if len(other) == 4:
    check(other[1]["metadata"]["name"] != default[1]["metadata"]["name"], "the same model in another namespace gets its own (cluster-scoped) volume")
    check(other[2]["metadata"]["name"] == default[2]["metadata"]["name"], "and the same claim name (namespace-scoped)")
    check(other[1]["spec"]["claimRef"]["namespace"] == "other-ns", "bound into its own namespace")

if bad:
    for b in bad:
        print("  FAIL " + b)
    sys.exit(1)
print("  ok   volume, claim and downloader rendered: bound pair, claim-mounted (no hostPath in the pod), marker probe, layout, placement, no GPU, no token")
PYEOF

refused=0
for badmodel in 'a/b/c' '/x' 'x/' 'a b' 'a"b' '' '..' 'a/..' '../x' 'Qwen/..' '.hidden' 'Qwen--x' 'a-/b' 'a/b.'; do
    if bash deploy/weights.sh apply -n check-ns --model "$badmodel" --path /mnt/local/models --image "$IMG" --dry-run >/dev/null 2>&1; then fail "a --model of '$badmodel' must be refused"; refused=1; fi
done
[ "$refused" -eq 0 ] && ok "a model id that is not [org/]name of Hugging Face characters, or carries .. / -- / a segment edge of . or -, is refused"
refused=0
for badpath in relative / 'a b' '/p"' '' /mnt /etc /etc/models /var/lib/kubelet /var/lib/containerd/x /tmp/models /home/me/models /proc/1 /mnt/local/models/ /mnt/local/../x /var/home/core/w /var/roothome/w /var/usrlocal/w /var/opt/cni/w /sysroot/ostree/x /ostree/x; do
    if bash deploy/weights.sh apply -n check-ns --model m --path "$badpath" --image "$IMG" --dry-run >/dev/null 2>&1; then fail "a --path of '$badpath' must be refused"; refused=1; fi
done
[ "$refused" -eq 0 ] && ok "a path that is not absolute, has one component, ends in /, holds .., or sits under a system prefix (/etc, /var/lib, /tmp, /home, ...) is refused"
for goodpath in /mnt/local/models /data/models /opt/models /srv/weights /var/mnt/weights /var/srv/weights /var/data/weights; do
    bash deploy/weights.sh apply -n check-ns --model m --path "$goodpath" --image "$IMG" --dry-run >/dev/null 2>&1 || fail "a --path of '$goodpath' must be accepted"
done
ok "a data directory of two or more components outside the system prefixes is accepted, RHCOS's /var/mnt and /var/srv included"
# the refusal loops prove nothing if the good form is refused too
bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --dry-run >/dev/null 2>&1 && ok "the form the refusal tests vary is itself accepted" || fail "the refusal tests' base invocation is refused, so they prove nothing"
if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --hf-token-secret 'a b' --dry-run >/dev/null 2>&1; then fail "a Secret reference with a space must be refused"; else ok "a Secret reference that is not NAME[/KEY] is refused"; fi
if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --capacity '1Ti; rm' --dry-run >/dev/null 2>&1; then fail "a capacity that is not a quantity must be refused"; else ok "a capacity that is not a quantity is refused"; fi
if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --dry-run >/dev/null 2>&1; then fail "apply without --image must be refused"; else ok "apply without --image is refused"; fi
if bash deploy/weights.sh apply -n check-ns --model m --path /mnt/local/models --image "$IMG" --all --dry-run >/dev/null 2>&1; then fail "apply must refuse --all"; else ok "apply: --all is refused"; fi

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
 {"metadata":{"name":"node-cpu",     "labels":{"gpu.nvidia.com/model":""}}},
 {"metadata":{"name":"node-amd",     "labels":{"amd.com/gpu.product-name":"MI300X"}}},
 {"metadata":{"name":"node-evicted", "labels":{"nvidia.com/gpu.product":"H200"}}}
]}
EOF
cat > "$T/pods.json" <<EOF
{"items":[
 {"metadata":{"labels":{"wva.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-ready"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-loading"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"running":{}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-failing"},"status":{"phase":"Running","containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/weights":"other"}},"spec":{"nodeName":"node-nopod"},"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}},
 {"metadata":{"labels":{"wva.llmd.ai/weights":"${NAME}"}},"spec":{"nodeName":"node-evicted"},"status":{"phase":"Failed","reason":"Evicted","message":"The node had condition: [DiskPressure]."}}
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
  "get nodes")   if [ -n "\${STUB_SELECTOR:-}" ]; then want -l "\$STUB_SELECTOR"; else refuse -l; fi
                 [ -n "\${STUB_NODES_ERROR:-}" ] && { echo "\$STUB_NODES_ERROR" >&2; exit 1; }
                 cat "$T/nodes.json" ;;
  "get pods")    want -n check-ns; want -l "$LBL"; cat "$T/pods.json" ;;
  "get daemonset")
      want -n check-ns
      case "\$ARGV" in
        *" -l "*) want -l "$LBL"; printf '%s\n' "\${STUB_DAEMONSETS-Qwen/Qwen3-32B}" ;;
        *) want -o "jsonpath={.spec.template.spec.nodeSelector}"
           case "\$ARGV" in *" ${NAME} "*) ;; *) echo "stub kubectl: get daemonset for a name that is not the DaemonSet:\$ARGV" >&2; exit 2 ;; esac
           if [ -n "\${STUB_DS_SELECTOR:-}" ]; then jq -cn --arg k "\${STUB_DS_SELECTOR%%=*}" --arg v "\${STUB_DS_SELECTOR#*=}" '{(\$k): \$v}'; else printf '{}'; fi ;;
      esac ;;
  "get pvc")
      want -n check-ns
      case "\$ARGV" in
        *"jsonpath={.status.phase}"*) case "\$ARGV" in *" ${NAME} "*) ;; *) echo "stub kubectl: the claim read names no claim:\$ARGV" >&2; exit 2 ;; esac; printf '%s' "\${STUB_PVC_PHASE-Bound}" ;;
        *"jsonpath={range"*)
            # real kubectl: a get by NAME answers one object, whose .items is
            # empty for a range; only a get by -l answers a List
            case "\$ARGV" in *" -l "*) printf '%s\n' "\${STUB_PVC_VOLUMES-${PVNAME}}" ;; *) : ;; esac ;;
        *"jsonpath={.spec.volumeName}"*)
            case "\$ARGV" in *" -l "*) echo "stub kubectl: a single-object jsonpath on a list get:\$ARGV" >&2; exit 2 ;; esac
            case "\$ARGV" in *" ${NAME} "*) printf '%s\n' "\${STUB_PVC_VOLUMES-${PVNAME}}" ;; *) : ;; esac ;;
        *"model-pvc"*) printf '%s' "\${STUB_MODEL_PVC_CLASS:-}" ;;
        *) echo "stub kubectl: unexpected pvc read:\$ARGV" >&2; exit 2 ;;
      esac ;;
  "get events")  want -n check-ns; printf '%s' "\${STUB_EVENT:-}" ;;
  "logs -n")     want -n check-ns; printf '%s\n' "\${STUB_LOG:-}" ;;
  "api-resources --api-group=security.openshift.io") [ -n "\${STUB_OPENSHIFT:-}" ] && echo "securitycontextconstraints scc security.openshift.io/v1 false SecurityContextConstraints"; : ;;
  "delete serviceaccount") want -n check-ns; echo deleted ;;
  "apply -f")    cat >/dev/null; echo applied ;;
  "delete daemonset"|"delete pvc") want -n check-ns; echo deleted ;;
  "get pv")      want -l "$LBL"; want -o json; refuse -n; cat "$T/pvs.json" ;;
  "delete pv")   refuse -n; echo deleted ;;
  *) echo "stub kubectl: unexpected \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/bin/kubectl"
STUB_PATH="$T/bin:$PATH"
# the volumes carrying our labels: ours, the same model in another namespace,
# and one whose claimRef is not a weights claim at all
cat > "$T/pvs.json" <<EOF
{"items":[
 {"metadata":{"name":"${PVNAME}"},              "spec":{"claimRef":{"namespace":"check-ns","name":"${NAME}"}}},
 {"metadata":{"name":"${NAME}-othernamespace"}, "spec":{"claimRef":{"namespace":"other-ns","name":"${NAME}"}}},
 {"metadata":{"name":"weights-second-model-11111111-22222222"}, "spec":{"claimRef":{"namespace":"check-ns","name":"weights-second-model-11111111"}}}
]}
EOF

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
expect_line node-evicted absent
grep -q 'node-evicted .*Evicted' "$T/status.out" && ok "status: an evicted downloader shows its reason" || fail "status: Evicted reason missing"
grep -q '^  node-cpu ' "$T/status.out" && fail "status: a node with an empty product label was counted" || ok "status: the default placement leaves the empty-label node out"
grep -q 'node-failing .*CrashLoopBackOff' "$T/status.out" && ok "status: a failing downloader shows its reason" || fail "status: reason missing"
grep -q 'Permission denied' "$T/status.out" && fail "status: a permission diagnosis with no permission error in the log" || ok "status: no permission diagnosis when the log shows none"
STUB_LOG='PermissionError: [Errno 13] Permission denied: /weights/models/Qwen' PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/eacces.out" 2>&1 || true
grep -q 'cannot write .*Permission denied in its log' "$T/eacces.out" && grep -q 'chgrp 0 DIR && chmod 2775 DIR && chcon -t container_file_t DIR' "$T/eacces.out" && ok "status: a downloader dying on Permission denied is named, with the node-directory steps (OpenShift: project UID, no relabel)" || fail "status EACCES diagnosis: $(grep -c 'Permission' "$T/eacces.out") line(s)"
grep -q '1/6 nodes hold it; 5 do not' "$T/status.out" && ok "status: the tally counts only Ready (download complete) as holding" || fail "status: tally: $(grep 'nodes hold' "$T/status.out")"
grep -q '1 node(s) carry an AMD, Intel or Gaudi accelerator label' "$T/status.out" && ok "status: a non-NVIDIA node under the default placement is named" || fail "status: vendor warning missing"
grep -q "claim ${NAME}: Bound" "$T/status.out" && ok "status: reports the claim Bound" || fail "status: claim line: $(head -1 "$T/status.out")"
[ "$rc" -ne 0 ] && ok "status: exits non-zero while a node lacks the weights" || fail "status: exit 0 with nodes lacking the weights"
grep -q 'stub kubectl:' "$T/status.out" && fail "status: a kubectl call lacked its scope: $(grep 'stub kubectl:' "$T/status.out" | head -1)" || ok "status: every kubectl call carried -n / -l"
grep -q "^Qwen/Qwen3-32B  (claim" "$T/status.out" && ok "status: with no --model the models are discovered from the DaemonSets" || fail "status: discovery: $(head -1 "$T/status.out")"
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns --model Qwen/Qwen3-32B > "$T/explicit.out" 2>&1 || true
grep -q '^  node-ready ' "$T/explicit.out" && ! grep -q "^get daemonset -n check-ns -l" "$T/calls" && ok "status --model: reports without discovering the DaemonSets" || fail "status --model: $(head -2 "$T/explicit.out"); calls: $(grep '^get daemonset' "$T/calls" | head -2 | tr '\n' ';')"
if PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns --model 'a/b/c' >/dev/null 2>&1; then fail "status --model must refuse a bad model id"; else ok "status --model: a bad model id is refused"; fi
# no holder anywhere: the DaemonSet's FailedCreate event
cp "$T/pods.json" "$T/pods-all.json"; printf '{"items":[]}' > "$T/pods.json"
if STUB_EVENT='pods "weights-x-" is forbidden: violates PodSecurity "restricted:latest"' PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/nopods.out" 2>&1; then fail "status with no downloader must fail"; else
    grep -q 'cannot create its pods: pods "weights-x-" is forbidden' "$T/nopods.out" && ok "status: with no downloader anywhere the DaemonSet's FailedCreate event is printed" || fail "status: FailedCreate event missing: $(tail -2 "$T/nopods.out")"; fi
cp "$T/pods-all.json" "$T/pods.json"
# no jq: apply still succeeds with the report skipped; status refuses
mkdir -p "$T/nojq"
for tool in bash sed awk tr printf cat grep head cut mktemp rm sort uniq shasum sha256sum env dirname basename readlink date id uname wc true false; do
    tp="$(command -v "$tool" 2>/dev/null || true)"; [ -n "$tp" ] && ln -sf "$tp" "$T/nojq/$tool"
done
if out="$(PATH="$T/bin:$T/nojq" bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" 2>&1)"; then
    case "$out" in *"jq is not installed"*) ok "apply: without jq the apply succeeds and the report is skipped with a warning" ;; *) fail "apply without jq: $out" ;; esac
else
    fail "apply must not fail for a missing jq after the manifests were applied: $out"
fi
if PATH="$T/bin:$T/nojq" bash deploy/weights.sh status -n check-ns >/dev/null 2>&1; then fail "status without jq must fail"; else ok "status: without jq refuses with a reason"; fi
STUB_OPENSHIFT=1 PATH="$STUB_PATH" bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /var/mnt/weights --image "$IMG" > "$T/ocp.out" 2>&1 || true
grep -q 'OpenShift: the downloader runs as the project UID' "$T/ocp.out" && grep -q 'chcon -t container_file_t /var/mnt/weights' "$T/ocp.out" && ok "apply: on a cluster with SecurityContextConstraints the OpenShift node-directory steps are printed once, with the path" || fail "apply on OpenShift: $(grep -c OpenShift "$T/ocp.out") notice(s)"
PATH="$STUB_PATH" bash deploy/weights.sh apply -n check-ns --model Qwen/Qwen3-32B --path /mnt/local/models --image "$IMG" > "$T/k8s.out" 2>&1 || true
grep -q 'OpenShift:' "$T/k8s.out" && fail "apply: the OpenShift notice printed on a cluster without SCCs" || ok "apply: no OpenShift notice on a cluster without SecurityContextConstraints"
if STUB_PVC_PHASE=Pending PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/pending.out" 2>&1; then fail "status must fail while the claim is not Bound"; else grep -q 'Pending' "$T/pending.out" && ok "status: an unbound claim is reported and fails" || fail "status: unbound claim: $(head -1 "$T/pending.out")"; fi
if STUB_DAEMONSETS="" PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/none.out" 2>&1; then fail "status with nothing to check must fail"; else grep -q 'no weights DaemonSets' "$T/none.out" && ok "status: no DaemonSets and no --model is refused with a reason" || fail "status: $(tail -1 "$T/none.out")"; fi
: > "$T/calls"
STUB_DS_SELECTOR=example.com/accelerator=h200 STUB_SELECTOR=example.com/accelerator=h200 PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns >/dev/null 2>&1 || true
grep -q 'get nodes -l example.com/accelerator=h200' "$T/calls" && ok "status: lists the nodes the DaemonSet was applied for (its live nodeSelector)" || fail "status: live selector not used: $(grep 'get nodes' "$T/calls")"
if STUB_DS_SELECTOR='a=b"c' PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/edited.out" 2>&1; then fail "status must refuse a DaemonSet nodeSelector that is not label characters"; else
    grep -q 'this script did not write' "$T/edited.out" && ok "status: a DaemonSet nodeSelector this script did not write is refused before kubectl" || fail "status on an edited DaemonSet: $(tail -1 "$T/edited.out")"; fi
if STUB_NODES_ERROR='error: unable to parse requirement' PATH="$STUB_PATH" bash deploy/weights.sh status -n check-ns > "$T/nf.out" 2>&1; then fail "status must fail when nodes cannot be listed"; else
    grep -q 'unable to parse requirement' "$T/nf.out" && ! grep -q cluster-reader "$T/nf.out" && ok "status: a node-list failure that is not a Forbidden reports kubectl's reason" || fail "status non-Forbidden: $(tail -1 "$T/nf.out")"; fi

# delete: DaemonSet and claim in the namespace; the volume found by OUR
# labels and its claimRef into this namespace -- never by a name read off a
# claim, which a tenant writes
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --model Qwen/Qwen3-32B >/dev/null 2>&1
grep -q "^delete daemonset -n check-ns ${NAME} --ignore-not-found" "$T/calls" && grep -q "^delete pvc -n check-ns ${NAME} --ignore-not-found" "$T/calls" \
    && grep -q "^delete pv ${PVNAME} --ignore-not-found" "$T/calls" && ok "delete --model: DaemonSet, claim, and the volume whose claimRef is this claim" || fail "delete --model issued: $(grep delete "$T/calls" | tr '\n' ';')"
grep -q '^get pvc' "$T/calls" && fail "delete: a claim was read for its volume name (a tenant writes that field)" || ok "delete: no claim is read for a volume name"
grep -q "^delete pv .*othernamespace" "$T/calls" && fail "delete --model: another namespace's volume for the same model was deleted" || ok "delete --model: another namespace's volume for the same model is left alone"
grep -q "^delete pv .*second-model" "$T/calls" && fail "delete --model: another model's volume was deleted" || ok "delete --model: another model's volume in this namespace is left alone"
grep -q "^delete serviceaccount" "$T/calls" && fail "delete --model: the ServiceAccount other models' downloaders share was deleted" || ok "delete --model: the shared ServiceAccount stays"
: > "$T/calls"
PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --all >/dev/null 2>&1
grep -q "^delete daemonset -n check-ns -l ${LBL} --ignore-not-found" "$T/calls" && grep -q "^delete pvc -n check-ns -l ${LBL} --ignore-not-found" "$T/calls" && ok "delete --all: by both labels" || fail "delete --all issued: $(grep delete "$T/calls" | tr '\n' ';')"
line="$(grep '^delete pv ' "$T/calls" || true)"
case "$line" in
    "delete pv ${PVNAME} weights-second-model-11111111-22222222 --ignore-not-found"|"delete pv weights-second-model-11111111-22222222 ${PVNAME} --ignore-not-found") ok "delete --all: every volume with our labels whose claimRef is in this namespace, and no other" ;;
    *) fail "delete --all pv delete: $line" ;;
esac
grep -q "^get pv -l ${LBL} -o json" "$T/calls" && ok "delete: the volumes are listed by our labels, cluster-scoped" || fail "delete: pv listing: $(grep '^get pv' "$T/calls")"
grep -q "^delete serviceaccount -n check-ns weights-downloader --ignore-not-found" "$T/calls" && ok "delete --all: the downloader ServiceAccount goes too" || fail "delete --all: ServiceAccount kept: $(grep serviceaccount "$T/calls")"
: > "$T/calls"
printf '{"items":[]}' > "$T/pvs-none.json"; cp "$T/pvs.json" "$T/pvs-all.json"; cp "$T/pvs-none.json" "$T/pvs.json"
PATH="$STUB_PATH" bash deploy/weights.sh delete -n check-ns --all >/dev/null 2>&1
grep -q '^delete pv ' "$T/calls" && fail "delete: with no volume of ours there is nothing to delete" || ok "delete --all with no volume of ours issues no pv delete"
cp "$T/pvs-all.json" "$T/pvs.json"
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
PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/s1.yaml" /mnt/local/wva check-ns example.com/accelerator=h200 dedicated,example.com/pool > "$T/hp2.out" 2>&1 || fail "model_hostpath on the shared layout failed: $(cat "$T/hp2.out")"
[ "$(yq -r '.shared.storage.hostPath.tolerations | map(.key) | join(",")' "$T/s1.yaml")" = "nvidia.com/gpu,dedicated,example.com/pool" ] && ok "model_hostpath: the taint keys reach the harness DaemonSet's tolerations, after nvidia.com/gpu" || fail "model_hostpath tolerations: $(yq '.shared.storage.hostPath.tolerations' "$T/s1.yaml")"
cp "$T/nested.yaml" "$T/n7.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/n7.yaml" /mnt/local/wva check-ns "" 'bad key' >/dev/null 2>&1; then fail "model_hostpath accepted a taint key with a space"; else ok "model_hostpath: a taint key that is not one is refused"; fi
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
cat > "$T/two.yaml" <<'EOF'
scenario:
  - name: a
    common:
      storage:
        modelPvc:
          size: 40Gi
  - name: b
    common:
      storage:
        modelPvc:
          size: 80Gi
EOF
if PATH="$STUB_PATH" bash hack/benchmark/model_hostpath.sh "$T/two.yaml" /mnt/local/wva check-ns > "$T/two.out" 2>&1; then
    a="$(yq -r '.scenario[0].common.storage.hostPath.capacity' "$T/two.yaml")"; b="$(yq -r '.scenario[1].common.storage.hostPath.capacity' "$T/two.yaml")"
    [ "$a" = 40Gi ] && [ "$b" = 40Gi ] && grep -q '(2 storage block(s))' "$T/two.out" && ok "model_hostpath: two stacks both get the block, the first claim's size (one volume, one capacity), and the message counts two" || fail "model_hostpath two stacks: a=$a b=$b; $(tail -1 "$T/two.out")"
else
    fail "model_hostpath on two stacks failed: $(cat "$T/two.out")"
fi
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
line="$(make -n weights WEIGHTS_MODEL=m WEIGHTS_PATH=/p WEIGHTS_IMAGE=i NAMESPACE=ns PREPULL_NODE_SELECTOR=k=v 2>/dev/null | tr -d '\\\n' || true)"
case "$line" in *'--node-selector "k=v"'*) ok "make weights: PREPULL_NODE_SELECTOR is inherited" ;; *) fail "make weights inheritance: $line" ;; esac
line="$(make -n weights WEIGHTS_MODEL=m WEIGHTS_PATH=/p WEIGHTS_IMAGE=i NAMESPACE=ns PREPULL_NODE_SELECTOR=k=v WEIGHTS_NODE_SELECTOR=w=x 2>/dev/null | tr -d '\\\n' || true)"
case "$line" in *'--node-selector "w=x"'*) ok "make weights: WEIGHTS_NODE_SELECTOR overrides PREPULL_NODE_SELECTOR" ;; *) fail "make weights override: $line" ;; esac
line="$(make -n weights-delete NAMESPACE=ns 2>/dev/null | grep 'weights.sh delete' || true)"
case "$line" in *"--all"*) ok "make weights-delete with no WEIGHTS_MODEL deletes every model's holder" ;; *) fail "weights-delete: $line" ;; esac
: > "$T/calls"
if PATH="$STUB_PATH" make weights-status > "$T/make-nons.out" 2>&1; then fail "make weights-status without NAMESPACE ran against the Makefile default"; else
    [ ! -s "$T/calls" ] && ok "make weights-status without NAMESPACE is refused before any kubectl call" || fail "make weights-status without NAMESPACE called kubectl: $(cat "$T/calls")"; fi
: > "$T/calls"
if PATH="$STUB_PATH" make weights WEIGHTS_PATH=/p WEIGHTS_IMAGE=i NAMESPACE=ns >/dev/null 2>&1; then fail "make weights without WEIGHTS_MODEL must be refused"; else
    [ ! -s "$T/calls" ] && ok "make weights without WEIGHTS_MODEL is refused before any kubectl call" || fail "make weights without WEIGHTS_MODEL called kubectl"; fi
line="$(make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_MODEL_HOSTPATH=/mnt/local/w BENCHMARK_SPEC=guides/pd-disaggregation PREPULL_TOLERATIONS=t1 2>/dev/null | grep -A2 'model_hostpath.sh' || true)"
case "$line" in
    *'model_hostpath.sh'*'guides/pd-disaggregation.yaml'*'"/mnt/local/w" "ns" "" "t1"'*) ok "benchmark-standup: BENCHMARK_MODEL_HOSTPATH runs model_hostpath.sh on the scenario copy with the namespace, selector and tolerations" ;;
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
out="$("$PY" "$T/fix11.py" "$T/03.j2" 2>&1 || true)"
case "$out" in *"already applied"*) ok "fix 11: idempotent" ;; *) fail "fix 11 second run: $out" ;; esac
# the earlier form: probe + affinity applied, chcon untouched -- completed, not refused
grep -q 'chcon -R -t container_file_t -l s0 "{{ storage.hostPath.path }}" 2>/tmp/chcon.err' "$T/03.j2" && grep -q 'grep -q "unlabeled file' "$T/03.j2" && grep -q 'cat /tmp/chcon.err; exit 1' "$T/03.j2" && ok "fix 11: chcon at level s0, skipped only on the no-SELinux text, fatal otherwise (a wrong SCC must not mark an unreadable directory)" || fail "fix 11: chcon form"
"$PY" - "$T/03.j2" <<'PYEOF' || fail "fix 11: could not rebuild the earlier forms"
import sys, re
src = open(sys.argv[1]).read()
a = src.index("              # wva-patch: skipped only where there is no SELinux")
b = src.index("\n              fi\n", src.index("cat /tmp/chcon.err; exit 1")) + len("\n              fi\n")
open(sys.argv[1].replace("03.j2", "03-earlier.j2"), "w").write(src[:a] + '              chcon -R -t container_file_t "{{ storage.hostPath.path }}"\n' + src[b:])
open(sys.argv[1].replace("03.j2", "03-prev.j2"), "w").write(src[:a] + '              chcon -R -t container_file_t "{{ storage.hostPath.path }}" 2>/dev/null || echo "Relabelling skipped: no SELinux on this node (wva-patch)."\n' + src[b:])
PYEOF
out="$("$PY" "$T/fix11.py" "$T/03-earlier.j2" 2>&1 || true)"
case "$out" in *"chcon made best-effort"*) ok "fix 11: the earliest form (fatal chcon) is completed in place" ;; *) fail "fix 11 on the earliest form: $out" ;; esac
out="$("$PY" "$T/fix11.py" "$T/03-prev.j2" 2>&1 || true)"
case "$out" in *"gated on the no-SELinux text"*) ok "fix 11: the previous form (chcon swallowed on any error) is completed in place" ;; *) fail "fix 11 on the previous form: $out" ;; esac
cmp -s "$T/03-earlier.j2" "$T/03.j2" && cmp -s "$T/03-prev.j2" "$T/03.j2" && ok "fix 11: both migrations end at the current form" || fail "fix 11: migrated templates differ from a fresh apply"
sed 's/images.benchmark.repository/images.other.repository/' "$T/03-orig.j2" > "$T/03-drift.j2"
if "$PY" "$T/fix11.py" "$T/03-drift.j2" >/dev/null 2>&1; then fail "fix 11 must fail on a missing anchor, not skip"; else ok "fix 11: a missing anchor is a hard error"; fi

# ---------------------------------------------------------------------------
# 6. patch_harness.sh fix 12: the harness volume named for its namespace,
#    with a claimRef; the claim's volumeName follows; the teardown scoped.
# ---------------------------------------------------------------------------
sed -n '/fix 12 (hostPath PV owned by its namespace) failed"/,/^PYEOF$/p' hack/benchmark/patch_harness.sh | sed '1d;$d' > "$T/fix12.py"
[ -s "$T/fix12.py" ] || fail "could not extract fix 12 from patch_harness.sh"
mkdir -p "$T/h12"
cat > "$T/h12/pv.j2" <<'EOF2'
metadata:
  name: {{ storage.modelPvc.name }}-hostpath-pv
  labels:
    app: {{ labels.app }}
    usage: model-cache
spec:
  capacity:
EOF2
cat > "$T/h12/pvc.j2" <<'EOF2'
  storageClassName: {{ storage.hostPath.storageClassName }}
  volumeName: {{ storage.modelPvc.name }}-hostpath-pv
EOF2
cat > "$T/h12/td.py" <<'EOF2'
class S:
    def a(self, cmd, context):
        pv_result = cmd.kube(
            "delete",
            "pv",
            "-l",
            "usage=model-cache",
            "--ignore-not-found",
        )
    def b(self, cmd, context):
        pv_result = cmd.kube(
            "delete",
            "pv",
            "-l",
            "usage=model-cache",
            "--ignore-not-found",
        )
EOF2
out="$("$PY" "$T/fix12.py" "$T/h12/pv.j2" "$T/h12/pvc.j2" "$T/h12/td.py" 2>&1 || true)"
case "$out" in *"applied"*) ok "fix 12: applies to the upstream anchors" ;; *) fail "fix 12 on the fixtures: $out" ;; esac
grep -q 'name: {{ storage.modelPvc.name }}-{{ namespace.name }}-hostpath-pv' "$T/h12/pv.j2" && grep -q 'volumeName: {{ storage.modelPvc.name }}-{{ namespace.name }}-hostpath-pv' "$T/h12/pvc.j2" && ok "fix 12: the volume is named for its namespace and the claim follows" || fail "fix 12: names"
grep -A2 'claimRef:' "$T/h12/pv.j2" | grep -q 'namespace: {{ namespace.name }}' && ok "fix 12: the volume carries a claimRef into its namespace" || fail "fix 12: claimRef missing"
grep -q 'wva.llmd.ai/model-namespace: {{ namespace.name }}' "$T/h12/pv.j2" && [ "$(grep -c 'usage=model-cache,wva.llmd.ai/model-namespace={context.require_namespace()}' "$T/h12/td.py")" -eq 2 ] && ok "fix 12: the teardown deletes only the volume labelled for the namespace being torn down (both sites)" || fail "fix 12: teardown scoping"
"$PY" -c "import ast,sys; ast.parse(open(sys.argv[1]).read())" "$T/h12/td.py" && ok "fix 12: the patched teardown still parses" || fail "fix 12: teardown does not parse"
out="$("$PY" "$T/fix12.py" "$T/h12/pv.j2" "$T/h12/pvc.j2" "$T/h12/td.py" 2>&1 || true)"
case "$out" in *"already applied"*) ok "fix 12: idempotent" ;; *) fail "fix 12 second run: $out" ;; esac
sed -i 's/usage: model-cache$/usage: cache/' "$T/h12/pv.j2"; sed -i 's/{{ namespace.name }}-hostpath-pv/hostpath-pv/' "$T/h12/pv.j2"
if "$PY" "$T/fix12.py" "$T/h12/pv.j2" "$T/h12/pvc.j2" "$T/h12/td.py" >/dev/null 2>&1; then fail "fix 12 must fail on a missing anchor"; else ok "fix 12: a missing anchor is a hard error"; fi

if [ "$FAILED" -ne 0 ]; then
    echo "weights checks: FAIL"
    exit 1
fi
echo "weights checks OK"
