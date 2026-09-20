#!/usr/bin/env bash
# What deploy/enginecache.sh and hack/benchmark/engine_cache_claim.sh actually
# EMIT, and what they refuse.
#
# A hostPath volume is one character away from handing every pod in a
# namespace root write on the node, a claim without a claimRef is taken by the
# first claim that asks for its class, and a preparer that requests a GPU or
# carries a `$(` in its command parses fine and ships wrong. `bash -n` sees
# none of it; this renders the manifests and looks, runs the scenario edit
# against a stub kubectl, and reads the Makefile's argv. It needs no cluster.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
PY=${PYTHON:-python3}
FAILED=0
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; FAILED=1; }

for tool in yq jq "$PY"; do
    command -v "$tool" >/dev/null 2>&1 || { echo "  SKIP $tool is not installed; this check needs yq, jq and python3 with PyYAML" >&2; exit 0; }
done
"$PY" -c "import yaml" 2>/dev/null || { echo "  SKIP python3 has no PyYAML" >&2; exit 0; }

IMG=docker.io/vllm/vllm-openai:v0.26.0

# ---------------------------------------------------------------------------
# 1. The render.
# ---------------------------------------------------------------------------
bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" --capacity 300Gi \
    --node-selector example.com/accelerator=h200 --toleration example.com/dedicated --dry-run > "$T/render.yaml" || fail "render failed"
bash deploy/enginecache.sh apply -n check-ns --path /mnt/local/weights/check-ns --image "$IMG" --dry-run > "$T/render-default.yaml" || fail "default render failed"
bash deploy/enginecache.sh apply -n other-ns --path /mnt/local/weights/check-ns --image "$IMG" --dry-run > "$T/render-other.yaml" || fail "other-ns render failed"

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

check([d["kind"] for d in docs] == ["ServiceAccount", "PersistentVolume", "PersistentVolumeClaim", "DaemonSet"], "kinds: %s" % [d["kind"] for d in docs])
sa, pv, pvc, ds = docs
check(sa["metadata"]["name"] == "engine-cache-preparer" and sa["metadata"]["namespace"] == "check-ns" and sa.get("automountServiceAccountToken") is False,
      "a ServiceAccount of its own, with no token: %s" % sa)
check(ds["spec"]["template"]["spec"].get("serviceAccountName") == "engine-cache-preparer", "the preparer runs as its own ServiceAccount")
check(pvc["metadata"]["name"] == "engine-cache" == ds["metadata"]["name"], "the claim is named engine-cache, one per namespace, and the DaemonSet shares the name")
check(pv["spec"].get("claimRef") == {"namespace": "check-ns", "name": "engine-cache"}, "the volume is bound to this claim and no other: %s" % pv["spec"].get("claimRef"))
check(pvc["spec"]["volumeName"] == pv["metadata"]["name"], "the claim names the volume")
check(re.match(r"^engine-cache-[a-z0-9]+$", pv["metadata"]["name"]), "the volume's name carries a hash: %s" % pv["metadata"]["name"])
check(other[1]["metadata"]["name"] != pv["metadata"]["name"], "another namespace on the same directory gets its own volume")
check(pv["spec"]["storageClassName"] == pvc["spec"]["storageClassName"] == "node-local-engine-cache", "one synthetic storage class on both sides")
check(pv["spec"]["capacity"]["storage"] == pvc["spec"]["resources"]["requests"]["storage"] == "300Gi", "--capacity reaches both, equal")
check(pv["spec"]["persistentVolumeReclaimPolicy"] == "Retain", "Retain: the caches are the point")
check(pv["spec"]["hostPath"] == {"path": "/mnt/local/weights/check-ns/engine-cache", "type": "DirectoryOrCreate"}, "hostPath under DIR/engine-cache: %s" % pv["spec"]["hostPath"])
check(pv["spec"]["accessModes"] == ["ReadWriteMany"] == pvc["spec"]["accessModes"], "RWX on both: every node mounts the same claim")
for d in docs:
    check(d["metadata"]["labels"].get("app.kubernetes.io/managed-by") == "wva-enginecache", "managed-by on %s" % d["kind"])
for d in (pv, pvc, ds):
    check(d["metadata"].get("annotations", {}).get("wva.llmd.ai/engine-cache-path") == "/mnt/local/weights/check-ns/engine-cache", "annotation names the directory on %s" % d["kind"])
check(ds["metadata"]["annotations"]["wva.llmd.ai/engine-cache-node-selector"] == "example.com/accelerator=h200", "the DaemonSet records the selector")
tpl = ds["spec"]["template"]; ps = tpl["spec"]
check(all(tpl["metadata"]["labels"].get(k) == v for k, v in ds["spec"]["selector"]["matchLabels"].items()), "selector matches the template labels")
check(ps["nodeSelector"] == {"example.com/accelerator": "h200"} and "affinity" not in ps, "an explicit selector is a nodeSelector, no affinity")
check({t["key"] for t in ps["tolerations"]} == {"nvidia.com/gpu", "example.com/dedicated"}, "tolerations: %s" % ps["tolerations"])
check(ps.get("automountServiceAccountToken") is False, "no ServiceAccount token")
check((ps.get("securityContext") or {}).get("seccompProfile", {}).get("type") == "RuntimeDefault", "seccomp RuntimeDefault")
vols = ps["volumes"]
check(len(vols) == 1 and vols[0].get("persistentVolumeClaim", {}).get("claimName") == "engine-cache", "the preparer mounts the CLAIM, not a hostPath: %s" % vols)
check(all("hostPath" not in v for v in vols), "no hostPath volume in the pod (Pod Security baseline)")
c = ps["containers"][0]
check(c["image"] == "docker.io/vllm/vllm-openai:v0.26.0" and c["imagePullPolicy"] == "IfNotPresent", "prepares with the given image, IfNotPresent")
env = {e["name"]: e for e in c["env"]}
check(env["NVIDIA_VISIBLE_DEVICES"]["value"] == "void", "no GPU injected into the preparer")
mount = c["volumeMounts"][0]
check(mount["name"] == vols[0]["name"] and mount["mountPath"] == "/engine-cache" and "subPath" not in mount, "the claim is mounted at /engine-cache, whole")
script = c["args"][0]
check("$(" not in script and "$$" not in script, "the command carries no $( or $$ (Kubernetes rewrites both)")
for sub in ("vllm", "flashinfer", "triton"):
    check('mkdir -p "/engine-cache/$d"' in script, "one mkdir loop")
check("for d in vllm flashinfer triton" in script and "chmod 1777" in script, "the three cache directories, world-writable and sticky")
check("touch /engine-cache/.prepared" in script, "the marker is written after the directories")
check("trap 'exit 0' TERM" in script and "sleep 3600" in script, "then the pod stays, so numberReady counts prepared nodes")
check(c["command"] == ["/bin/sh", "-c"], "sh -c with the script as args")
probe = c["readinessProbe"]["exec"]["command"]
check(probe == ["/bin/sh", "-c", "test -f /engine-cache/.prepared && test -w /engine-cache/vllm"], "Ready is the marker AND the directory writable by this UID: %s" % probe)
res = c.get("resources", {})
for section in ("requests", "limits"):
    for k in res.get(section, {}):
        check("gpu" not in k and "/" not in k, "the preparer requests an accelerator: %s" % k)
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
    check(default[2]["spec"]["resources"]["requests"]["storage"] == "200Gi", "default capacity 200Gi")
    check(dps["tolerations"] == [{"key": "nvidia.com/gpu", "operator": "Exists"}], "default toleration")
for b in bad:
    print("  FAIL render: " + b)
sys.exit(1 if bad else 0)
PYEOF
[ "$FAILED" = 0 ] && ok "render: volume, claim and preparer have the shape the header promises"

# ---------------------------------------------------------------------------
# 2. What apply refuses, before any kubectl.
# ---------------------------------------------------------------------------
refuse() {  # refuse "<why>" args...
    local why="$1"; shift
    if bash deploy/enginecache.sh "$@" >/dev/null 2>&1; then fail "accepted: $why"; else ok "refused: $why"; fi
}
refuse "apply without a namespace" apply --path /mnt/local/w --image "$IMG" --dry-run
refuse "apply without a path" apply -n ns --image "$IMG" --dry-run
refuse "apply without an image" apply -n ns --path /mnt/local/w --dry-run
refuse "a path under /var/lib" apply -n ns --path /var/lib/kubelet --image "$IMG" --dry-run
refuse "a relative path" apply -n ns --path mnt/local --image "$IMG" --dry-run
refuse "a top-level directory on its own" apply -n ns --path /mnt --image "$IMG" --dry-run
refuse "an image with a space" apply -n ns --path /mnt/local/w --image "img one" --dry-run
refuse "a capacity that is not a quantity" apply -n ns --path /mnt/local/w --image "$IMG" --capacity "1 Ti" --dry-run
refuse "a selector with a quote" apply -n ns --path /mnt/local/w --image "$IMG" --node-selector 'a=b"' --dry-run
refuse "--dry-run on status" status -n ns --dry-run
refuse "an unknown command" prepare -n ns
refuse "an unknown option" apply -n ns --path /mnt/local/w --image "$IMG" --bogus --dry-run

# ---------------------------------------------------------------------------
# 3. engine_cache_claim.sh against a stub kubectl.
# ---------------------------------------------------------------------------
mkdir -p "$T/bin"
cat > "$T/bin/kubectl" <<EOF
#!/usr/bin/env bash
ARGV=" \$* "
case "\$ARGV" in
  *" get pvc "*)
    case "\$ARGV" in *" -n "*) ;; *) echo "stub kubectl: pvc read without -n:\$ARGV" >&2; exit 2 ;; esac
    case "\$ARGV" in
      *"jsonpath={.status.phase}"*) printf '%s' "\${STUB_PVC_PHASE-Bound}" ;;
      *"jsonpath={.spec.storageClassName}"*) printf '%s' "\${STUB_PVC_CLASS-node-local-engine-cache}" ;;
      *) echo "stub kubectl: unexpected pvc read:\$ARGV" >&2; exit 2 ;;
    esac ;;
  *) echo "stub kubectl: unexpected \$*" >&2; exit 2 ;;
esac
EOF
chmod +x "$T/bin/kubectl"
STUB_PATH="$T/bin:$PATH"
cat > "$T/scenario.yaml" <<'EOF'
scenario:
  - name: pd
    prefill:
      additionalVolumeMounts:
        - name: engine-cache
          mountPath: /engine-cache
          subPath: engine-cache
      additionalVolumes:
        - name: engine-cache
          type: persistentVolumeClaim
          persistentVolumeClaim:
            claimName: workload-pvc
    decode:
      additionalVolumeMounts:
        - name: engine-cache
          mountPath: /engine-cache
          subPath: engine-cache
        - name: other
          mountPath: /other
          subPath: keep
      additionalVolumes:
        - name: engine-cache
          type: persistentVolumeClaim
          persistentVolumeClaim:
            claimName: workload-pvc
        - name: other
          type: persistentVolumeClaim
          persistentVolumeClaim:
            claimName: other-pvc
EOF
cp "$T/scenario.yaml" "$T/s1.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s1.yaml" check-ns > "$T/s1.out" 2>&1; then
    a="$(yq -r '.scenario[0].prefill.additionalVolumes[0].persistentVolumeClaim.claimName' "$T/s1.yaml")"
    b="$(yq -r '.scenario[0].decode.additionalVolumes[0].persistentVolumeClaim.claimName' "$T/s1.yaml")"
    o="$(yq -r '.scenario[0].decode.additionalVolumes[1].persistentVolumeClaim.claimName' "$T/s1.yaml")"
    [ "$a" = engine-cache ] && [ "$b" = engine-cache ] && [ "$o" = other-pvc ] && ok "engine_cache_claim: both roles' engine-cache volumes name the claim, another volume is untouched" || fail "engine_cache_claim volumes: prefill=$a decode=$b other=$o"
    sp="$(yq -r '.scenario[0].decode.additionalVolumeMounts[0] | has("subPath")' "$T/s1.yaml")"
    sk="$(yq -r '.scenario[0].decode.additionalVolumeMounts[1].subPath' "$T/s1.yaml")"
    [ "$sp" = false ] && [ "$sk" = keep ] && ok "engine_cache_claim: the engine-cache mount loses its subPath, another mount keeps its own" || fail "engine_cache_claim mounts: subPath=$sp other=$sk"
    grep -q '2 engine-cache volume(s)' "$T/s1.out" && ok "engine_cache_claim: the message counts both" || fail "engine_cache_claim message: $(cat "$T/s1.out")"
else
    fail "engine_cache_claim failed on the fixture: $(cat "$T/s1.out")"
fi
cp "$T/scenario.yaml" "$T/s2.yaml"
if STUB_PVC_PHASE=Pending PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s2.yaml" check-ns > "$T/s2.out" 2>&1; then fail "engine_cache_claim must refuse a claim that is not Bound"; else
    grep -q "no Bound claim named engine-cache" "$T/s2.out" && cmp -s "$T/scenario.yaml" "$T/s2.yaml" && ok "engine_cache_claim: a claim that is not Bound is refused, scenario untouched" || fail "engine_cache_claim Pending: $(tail -1 "$T/s2.out")"; fi
cp "$T/scenario.yaml" "$T/s3.yaml"
if STUB_PVC_PHASE= PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s3.yaml" check-ns >/dev/null 2>&1; then fail "engine_cache_claim must refuse a missing claim"; else ok "engine_cache_claim: a missing claim is refused"; fi
cp "$T/scenario.yaml" "$T/s4.yaml"
if STUB_PVC_CLASS=shared-vast PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s4.yaml" check-ns > "$T/s4.out" 2>&1; then fail "engine_cache_claim must refuse a claim on another class"; else
    grep -q "not node-local-engine-cache" "$T/s4.out" && cmp -s "$T/scenario.yaml" "$T/s4.yaml" && ok "engine_cache_claim: a same-named claim on another class is refused, scenario untouched" || fail "engine_cache_claim class: $(tail -1 "$T/s4.out")"; fi
printf 'scenario:\n  - name: x\n    decode: {}\n' > "$T/s5.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s5.yaml" check-ns >/dev/null 2>&1; then fail "engine_cache_claim must fail on a scenario with no engine-cache volume"; else ok "engine_cache_claim: a scenario with no engine-cache volume is refused, not silently left shared"; fi
cp "$T/scenario.yaml" "$T/s6.yaml"
if PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/s6.yaml" check-ns 'Bad_Name' >/dev/null 2>&1; then fail "engine_cache_claim accepted a claim name that is not a label"; else ok "engine_cache_claim: a claim name that is not a DNS label is refused"; fi
# the shipped scenarios carry the volume the edit looks for
for sc in hack/benchmark/scenarios/guides/pd-disaggregation.yaml; do
    cp "$sc" "$T/real.yaml"
    if PATH="$STUB_PATH" bash hack/benchmark/engine_cache_claim.sh "$T/real.yaml" check-ns > "$T/real.out" 2>&1; then
        n="$(yq -r '[.. | select(type == "!!map" and has("additionalVolumes")) | .additionalVolumes[] | select(.name == "engine-cache") | .persistentVolumeClaim.claimName] | map(select(. == "engine-cache")) | length' "$T/real.yaml")"
        [ "$n" -ge 2 ] && ok "engine_cache_claim: $(basename "$sc") -- $n engine-cache volumes repointed" || fail "engine_cache_claim on $(basename "$sc"): $n repointed"
    else
        fail "engine_cache_claim on $(basename "$sc"): $(cat "$T/real.out")"
    fi
done

# ---------------------------------------------------------------------------
# 4. The Makefile: the targets' argv and the standup's step.
# ---------------------------------------------------------------------------
line="$(make -n engine-cache ENGINE_CACHE_PATH=/mnt/local/w ENGINE_CACHE_IMAGE=i:1 ENGINE_CACHE_CAPACITY=50Gi NAMESPACE=ns PREPULL_TOLERATIONS=t1 2>/dev/null | tr -d '\\\n' | grep -o 'bash deploy/enginecache.sh apply.*' || true)"
case "$line" in
    *'-n "ns"'*'--path "/mnt/local/w"'*'--image "i:1"'*'--capacity "50Gi"'*'--toleration t1'*) ok "make engine-cache: namespace, path, image, capacity and toleration reach the script" ;;
    *) fail "make engine-cache argv: $line" ;;
esac
line="$(make -n engine-cache WEIGHTS_PATH=/mnt/local/w WEIGHTS_IMAGE=i:2 NAMESPACE=ns 2>/dev/null | tr -d '\\\n' | grep -o 'bash deploy/enginecache.sh apply.*' || true)"
case "$line" in *'--path "/mnt/local/w"'*'--image "i:2"'*) ok "make engine-cache: defaults to the weights' path and image" ;; *) fail "make engine-cache defaults: $line" ;; esac
# the guard tests $(origin NAMESPACE): given on the command line it expands to a word, otherwise to nothing
if make -n engine-cache ENGINE_CACHE_PATH=/mnt/local/w ENGINE_CACHE_IMAGE=i NAMESPACE=ns 2>/dev/null | grep -q 'test -n "command line"'; then ok "make engine-cache: NAMESPACE must be given on the command line (the Makefile default is not taken)"; else fail "make engine-cache: the NAMESPACE guard is missing"; fi
if make -n engine-cache ENGINE_CACHE_PATH=/mnt/local/w ENGINE_CACHE_IMAGE=i 2>/dev/null | grep -q 'test -n ""'; then ok "make engine-cache: without NAMESPACE the guard is empty and fails at run time"; else fail "make engine-cache: the guard passes without NAMESPACE"; fi
line="$(make -n engine-cache-status NAMESPACE=ns WEIGHTS_NODE_SELECTOR=k=v 2>/dev/null | grep -o 'bash deploy/enginecache.sh status.*' || true)"
case "$line" in *'-n "ns"'*'--node-selector "k=v"'*) ok "make engine-cache-status: namespace and selector reach the script" ;; *) fail "make engine-cache-status argv: $line" ;; esac
line="$(make -n engine-cache-delete NAMESPACE=ns 2>/dev/null | grep -o 'bash deploy/enginecache.sh delete.*' || true)"
case "$line" in *'-n "ns"'*) ok "make engine-cache-delete: the namespace reaches the script" ;; *) fail "make engine-cache-delete argv: $line" ;; esac
n="$(make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_MODEL_HOSTPATH=/mnt/local/w BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null | grep -n 'model_hostpath.sh\|enginecache.sh apply\|engine_cache_claim.sh\|standup \\$' | cut -d: -f1 | tr '\n' ' ')"
set -- $n
if [ $# -ge 4 ] && [ "$1" -lt "$2" ] && [ "$2" -lt "$3" ] && [ "$3" -lt "$4" ]; then ok "benchmark-standup: BENCHMARK_MODEL_HOSTPATH alone turns the cache on too, after the weights edit and before the harness standup (apply, then the scenario edit)"; else fail "benchmark-standup ordering: $n"; fi
# make -n prints the recipe's shell text; the step is guarded by an if on the
# variable, so with neither set the condition it prints is an empty test
if make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null | grep -B1 'img="";' | grep -q 'if \[ -n "" \]'; then ok "benchmark-standup: neither variable, the cache step's condition is empty (the shared volume, as before)"; else fail "benchmark-standup: the cache step is not behind an empty test with neither variable set"; fi
if make -n benchmark-standup BENCHMARK_NAMESPACE=ns BENCHMARK_ENGINE_CACHE_HOSTPATH=/mnt/local/c BENCHMARK_SPEC=guides/pd-disaggregation 2>/dev/null | grep -q -- '--path "/mnt/local/c"'; then ok "benchmark-standup: BENCHMARK_ENGINE_CACHE_HOSTPATH on its own takes its own directory" ; else fail "benchmark-standup: the cache directory did not reach the apply"; fi

exit $FAILED
