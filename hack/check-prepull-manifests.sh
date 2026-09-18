#!/usr/bin/env bash
# Renders deploy/prepull.sh's DaemonSets and asserts their shape.
#
# Every defect this covers parses fine as YAML: a holder that requests an
# accelerator (it would then HOLD one, which is the opposite of the point), a
# nodeSelector that lands on every node, two images colliding on one DaemonSet
# name, a pull policy that re-pulls a pinned tag on every restart, or a tag
# whose annotation no longer names the image `status` compares against.
set -euo pipefail
cd "$(dirname "$0")/.."

PY=${PYTHON:-python3}
OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

bash deploy/prepull.sh apply -n check-ns \
    --image docker.io/vllm/vllm-openai:v0.26.0 \
    --image ghcr.io/other/vllm-openai:v0.26.0 \
    --image 'docker.io/vllm/vllm-openai@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef' \
    --node-selector example.com/accelerator=h200 \
    --toleration example.com/dedicated \
    --dry-run > "$OUT"

"$PY" - "$OUT" <<'PYEOF'
import sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1], encoding="utf-8")) if d]
fail = []
def check(cond, msg):
    if not cond:
        fail.append(msg)

check(len(docs) == 3, "expected 3 DaemonSets, got %d" % len(docs))
names = [d["metadata"]["name"] for d in docs]
check(len(set(names)) == 3, "names collide: %s" % names)
for n in names:
    check(len(n) <= 63 and n == n.lower() and all(c.isalnum() or c == "-" for c in n),
          "name is not a valid DNS label: %r" % n)
images = ["docker.io/vllm/vllm-openai:v0.26.0", "ghcr.io/other/vllm-openai:v0.26.0",
          "docker.io/vllm/vllm-openai@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"]
for d, image in zip(docs, images):
    check(d["kind"] == "DaemonSet", "kind %s" % d["kind"])
    check(d["metadata"]["namespace"] == "check-ns", "namespace")
    check(d["metadata"]["annotations"]["wva.llmd.ai/prepull-image"] == image, "annotation names the image: %s" % d["metadata"]["annotations"])
    tpl = d["spec"]["template"]
    check(tpl["spec"]["nodeSelector"] == {"example.com/accelerator": "h200"}, "nodeSelector: %s" % tpl["spec"]["nodeSelector"])
    keys = {t["key"] for t in tpl["spec"]["tolerations"]}
    check({"nvidia.com/gpu", "example.com/dedicated"} <= keys, "tolerations: %s" % keys)
    sel = d["spec"]["selector"]["matchLabels"]
    check(all(tpl["metadata"]["labels"].get(k) == v for k, v in sel.items()), "selector does not match the template labels")
    cs = tpl["spec"]["containers"]
    check(len(cs) == 1, "one container")
    c = cs[0]
    check(c["image"] == image, "container image %s" % c["image"])
    check(c["imagePullPolicy"] == "IfNotPresent", "pull policy %s" % c["imagePullPolicy"])
    res = c.get("resources", {})
    for section in ("requests", "limits"):
        for k in res.get(section, {}):
            check("gpu" not in k and "/" not in k, "the holder requests an accelerator: %s" % k)
    check(res.get("limits", {}).get("memory"), "a memory limit, so a holder can never grow")
    check(c["command"][0] == "/bin/sh", "runs the image itself, asleep")
    check("terminationGracePeriodSeconds" in tpl["spec"], "short termination so a holder yields its node quickly")

if fail:
    print("prepull manifests: FAIL")
    for f in fail:
        print("  - " + f)
    sys.exit(1)
print("prepull manifests OK (%d DaemonSets rendered, shape asserted)" % len(docs))
PYEOF
