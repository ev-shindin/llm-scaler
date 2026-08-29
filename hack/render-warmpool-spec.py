#!/usr/bin/env python3
"""Render the warm-pool Pod spec into deploy/warmpool.sh from config/warmpool.

The pool Pod is described ONCE, in `config/warmpool/warmpool-deployment.yaml` --
the manifest that is reviewed and deployed. `deploy/warmpool.sh` needs the same
Pod with a handful of values swapped, and it used to carry its own hand-written
copy.

That copy drifted, completely and silently: the script emitted ONE container --
the proxy image under the name `inference-server` -- with no command, no ports,
no env and no supervisor. Every pool it created held its GPUs and answered
nothing, while the controller reported the pool EMPTY. It was found on a
cluster, not in review.

Deriving it at RUN time fixes the drift but moves the cost onto whoever runs the
script: it then needs the repo checked out, plus `yq` and `kubectl kustomize`,
none of which a deploy script should assume. So it is derived HERE, at commit
time, and the script ships with the answer already in it.

Same two-file arrangement `render-guides.py` uses for guide READMEs: generated
content between markers, `--check` in CI to fail when it drifts.

Usage:
    python hack/render-warmpool-spec.py            # rewrite deploy/warmpool.sh
    python hack/render-warmpool-spec.py --check    # fail if it is out of date
"""

import argparse
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    print("ERROR: PyYAML is required. pip install pyyaml", file=sys.stderr)
    sys.exit(1)

ROOT = Path(__file__).resolve().parent.parent
MANIFEST = ROOT / "config" / "warmpool" / "warmpool-deployment.yaml"
SCRIPT = ROOT / "deploy" / "warmpool.sh"

START = "# BEGIN GENERATED POD SPEC -- regenerate with: make warmpool-render"
END = "# END GENERATED POD SPEC"

# The values warmpool.sh decides, written as the shell variables it expands.
# Everything else -- the launcher image, the env that keeps caches off a
# writable HOME, the probes, the volumes -- comes from the manifest untouched.
PLACEHOLDERS = {
    "gpus": "${GPUS_PER_POD}",
    "memory": "${WP_MEMORY}",
    "proxy_image": "${PROXY_IMAGE}",
    "cache_claim": "${CACHE_CLAIM}",
}


def pod_spec(role):
    """The manifest's Pod spec for this role, with the knobs substituted."""
    doc = None
    for candidate in yaml.safe_load_all(MANIFEST.read_text(encoding="utf-8")):
        if candidate and candidate.get("kind") == "Deployment":
            doc = candidate
            break
    if doc is None:
        raise SystemExit("%s holds no Deployment to read the Pod from" % MANIFEST)

    spec = doc["spec"]["template"]["spec"]

    containers = []
    for container in spec.get("containers", []):
        name = container.get("name")
        # A WORKER runs no proxy. The proxy is the Pod's traffic gate and is
        # Ready only while a model is awake behind it; a worker holds a rank and
        # serves nobody, so its gate never opens, the Pod is never Ready, and the
        # controller never counts the group complete. Measured on pokprod: with
        # the proxy present the pool reported pods=0 while holding two GPUs.
        if role == "worker" and name == "proxy":
            continue
        if name == "inference-server":
            for side in ("limits", "requests"):
                res = container.setdefault("resources", {}).setdefault(side, {})
                res["nvidia.com/gpu"] = PLACEHOLDERS["gpus"]
                res["memory"] = PLACEHOLDERS["memory"]
        if name == "proxy":
            container["image"] = PLACEHOLDERS["proxy_image"]
        containers.append(container)
    if not containers:
        raise SystemExit("the %s Pod came out with no containers" % role)
    spec["containers"] = containers

    for volume in spec.get("volumes", []):
        if volume.get("name") == "model-cache" and "persistentVolumeClaim" in volume:
            volume["persistentVolumeClaim"]["claimName"] = PLACEHOLDERS["cache_claim"]

    # Removed here and re-added by the shell, because it is conditional: an
    # accelerator nobody named must add no key at all, not an empty one.
    spec.pop("nodeSelector", None)
    return spec


def as_yaml(role):
    """One role's Pod spec as YAML, escaped for the shell that will carry it."""
    text = yaml.dump(pod_spec(role), default_flow_style=False, sort_keys=False)

    # EVERY backslash doubled, because the heredoc that carries this is unquoted.
    #
    # Bash reads a backslash there as an escape only before a newline, another
    # backslash, a dollar or a backtick, and passes it through untouched
    # elsewhere. Doubling is exactly cancelled in both cases -- `\\n` gives back
    # `\n`, and `\<newline>` gives back `\<newline>` -- so what YAML finally
    # parses is what was dumped here, whatever the value contains.
    #
    # This is not tidiness. PyYAML folds a long double-quoted scalar by ending
    # the line with a backslash; bash ate that newline AND the next line's
    # indentation, and the spaces landed inside a Python string literal in the
    # preStop drain hook. It read `inst.get("          options")`, got None for
    # every instance, and exited 0 having slept nothing -- so a pool Pod created
    # by this script was killed with its engine awake and requests in flight,
    # which is the one thing the hook exists to prevent. Nothing caught it: the
    # YAML still parsed, the rendered text still matched this generator, and a
    # drain that drains nothing is silent by construction.
    text = text.replace("\\", "\\\\")

    # The heredoc is UNQUOTED so the placeholders expand. Anything else the shell
    # would treat as an expansion has to be caught here rather than discovered as
    # a malformed manifest on a cluster. The manifest's own $ and ` live in
    # comments, which yaml.dump drops -- this guard is for the day one moves into
    # a value. Escaping cannot help: a doubled backslash before a dollar still
    # leaves the dollar to the shell.
    scrubbed = text
    for placeholder in PLACEHOLDERS.values():
        scrubbed = scrubbed.replace(placeholder, "")
    for char, what in (("$", "an expansion"), ("`", "a command substitution")):
        if char in scrubbed:
            line = next(l for l in scrubbed.splitlines() if char in l)
            raise SystemExit(
                "the %s Pod spec contains %r, which the shell would read as %s: %r\n"
                "Quote it in the manifest, or teach this generator to escape it."
                % (role, char, what, line.strip())
            )
    return text.rstrip("\n")


def render():
    """The generated shell function, as lines."""
    lines = [
        START,
        "#",
        "# Generated from config/warmpool/warmpool-deployment.yaml. DO NOT EDIT by",
        "# hand: run `make warmpool-render` after changing that manifest. CI fails",
        "# on drift, because a pool Pod that disagrees with the shipped one holds",
        "# GPUs and answers nothing -- which is what this file exists to prevent.",
        "pool_pod_spec() {",
        '  local memory="$1" indent="$2" role="${3:-leader}"',
        "",
        "  # Conditional: an accelerator nobody named adds no key, rather than an",
        '  # empty one, which would pin the Pod to a GPU product called "".',
        '  local WP_NODE_SELECTOR=""',
        '  if [ -n "$ACCELERATOR" ]; then',
        '    WP_NODE_SELECTOR="nodeSelector:',
        "  nvidia.com/gpu.product: ${ACCELERATOR}\"",
        "  fi",
        '  local WP_MEMORY="$memory"',
        "",
        "  local out",
        '  if [ "$role" = "worker" ]; then',
        "    out=$(cat <<YAML",
        "${WP_NODE_SELECTOR}",
        as_yaml("worker"),
        "YAML",
        "    )",
        "  else",
        "    out=$(cat <<YAML",
        "${WP_NODE_SELECTOR}",
        as_yaml("leader"),
        "YAML",
        "    )",
        "  fi",
        "",
        "  # Blank lines go: an unset nodeSelector leaves one, and a stray blank",
        "  # line inside a Pod spec is harmless but reads as a mistake.",
        '  printf \'%s\\n\' "$out" | grep -v \'^[[:space:]]*$\' | sed "s/^/${indent}/"',
        "}",
        END,
    ]
    return "\n".join(lines)


def replace_block(text, block):
    if START not in text or END not in text:
        raise SystemExit(
            "%s has no generated block. Add these two lines around the current\n"
            "pool_pod_spec definition and re-run:\n  %s\n  %s" % (SCRIPT, START, END)
        )
    head = text[: text.index(START)]
    tail = text[text.index(END) + len(END):]
    return head + block + tail


def main():
    ap = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument("--check", action="store_true",
                    help="do not write; exit non-zero if the script is stale")
    args = ap.parse_args()

    raw = SCRIPT.read_bytes()
    crlf = b"\r\n" in raw
    current = raw.decode("utf-8").replace("\r\n", "\n")
    updated = replace_block(current, render())

    if updated == current:
        print("deploy/warmpool.sh is up to date with config/warmpool")
        return 0
    if args.check:
        print(
            "deploy/warmpool.sh no longer matches config/warmpool.\n"
            "The pool Pod is described in the manifest; the script carries a\n"
            "generated copy. Run: make warmpool-render",
            file=sys.stderr,
        )
        return 1
    out = updated.replace("\n", "\r\n") if crlf else updated
    SCRIPT.write_bytes(out.encode("utf-8"))
    print("rendered deploy/warmpool.sh from config/warmpool")
    return 0


if __name__ == "__main__":
    sys.exit(main())
