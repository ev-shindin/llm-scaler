#!/usr/bin/env bash
# Prints the engine image the llm-d-benchmark clone will deploy -- the
# repository and tag from its config/templates/values/defaults.yaml, anchors
# resolved -- so a standup can hold that exact reference on the accelerator
# nodes before the harness deploys it. Prints nothing (exit 1) when the clone
# or the values are not where the pinned harness keeps them; a standup that
# cannot name the image must not guess one.
#
#   engine_image.sh <llm-d-benchmark clone dir>
set -euo pipefail

REPO_DIR="${1:?usage: engine_image.sh <llm-d-benchmark clone dir>}"
DEFAULTS="$REPO_DIR/config/templates/values/defaults.yaml"
[ -f "$DEFAULTS" ] || exit 1

# The harness venv has PyYAML; fall back to the system python.
PY="$REPO_DIR/.venv/bin/python"
[ -x "$PY" ] || PY="${PYTHON:-python3}"

"$PY" - "$DEFAULTS" <<'PYEOF'
import sys
try:
    import yaml
except ImportError:
    sys.exit(1)
d = yaml.safe_load(open(sys.argv[1], encoding="utf-8"))
vllm = (d.get("images") or {}).get("vllm") or {}
repo, tag = vllm.get("repository"), vllm.get("tag")
if not repo or not tag:
    sys.exit(1)
print("%s:%s" % (repo, tag))
PYEOF
