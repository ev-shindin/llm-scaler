#!/usr/bin/env python3
"""Render guide README command blocks from guide.yaml.

Adopts llm-d's well-lit-path convention (llm-d/guides): a guide is two files —
a machine-readable `guide.yaml` and a human-readable `README.md` — and the bash
blocks in the README are GENERATED from the YAML, bounded by marker pairs:

    <!-- guide:deploy.controller start -->
    ```bash
    make deploy-wva-namespace-on-k8s
    ```
    <!-- guide:deploy.controller end -->

Everything outside the markers is prose and is preserved byte for byte. The point
is that the commands a reader copies and the commands a tool would run are the
same strings, so a guide cannot drift from what it documents — which is the
failure this repo has already paid for twice, in a benchmark that installed a
different binary than it claimed and a doc that told people to apply a CRD that
does not exist.

Usage:
    python hack/render-guides.py            # rewrite READMEs in place
    python hack/render-guides.py --check    # fail if any README is out of date
"""

import argparse
import re
import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    print("ERROR: PyYAML is required. pip install pyyaml", file=sys.stderr)
    sys.exit(1)

GUIDES_DIR = Path(__file__).resolve().parent.parent / "docs" / "guides"

# Guides that are deliberately hand-written, with the reason. Everything else must
# carry a guide.yaml so its commands are generated rather than retyped.
HAND_WRITTEN = {
    # FMA's commands are interleaved with long explanatory prose and several are
    # illustrative rather than runnable (editing a plan file, inspecting a
    # launcher). Converting it would either lose that or fill the YAML with steps
    # nobody runs. Revisit if its runnable path is ever separated out.
    "fma",
    # The warm pool guide is mostly a decision, not a procedure: whether this
    # cluster can warm this model at all, how many fit in one Pod, which pools a
    # namespace needs. Its commands are a few deploy/warmpool.sh calls whose
    # ARGUMENTS are the answers to those questions, so a rendered linear path
    # would either invent them or drop the reasoning that produces them. The
    # runnable part is covered by test/e2e/warm_pool_policy_test.go instead.
    "warm-pool",
}

# <!-- guide:some.path start --> ... <!-- guide:some.path end -->
MARKER = re.compile(
    r"(?P<start><!--\s*guide:(?P<path>[\w.\-]+)\s+start\s*-->\n)"
    r".*?"
    r"(?P<end><!--\s*guide:(?P=path)\s+end\s*-->)",
    re.S,
)


def resolve(data, dotted):
    """Walk a dotted path into the guide.yaml, or return None."""
    node = data
    for part in dotted.split("."):
        if not isinstance(node, dict) or part not in node:
            return None
        node = node[part]
    return node


def block_for(node):
    """Render one YAML node as the bash block a reader copies.

    A step is a mapping with `run:`; a group is a mapping of steps. Comments ride
    along as `note:` so the rendered block explains itself rather than being a
    wall of commands.
    """
    steps = []
    if isinstance(node, dict) and "run" in node:
        steps = [node]
    elif isinstance(node, dict):
        steps = [v for v in node.values() if isinstance(v, dict) and "run" in v]
    elif isinstance(node, list):
        steps = [v for v in node if isinstance(v, dict) and "run" in v]

    lines = []
    for step in steps:
        note = step.get("note")
        if note:
            lines.extend(f"# {line}" for line in str(note).strip().splitlines())
        lines.append(str(step["run"]).strip())
        lines.append("")
    while lines and lines[-1] == "":
        lines.pop()
    return "```bash\n" + "\n".join(lines) + "\n```\n"


def render(readme: Path, guide: dict) -> str:
    text = readme.read_text(encoding="utf-8")

    def repl(m):
        node = resolve(guide, m.group("path"))
        if node is None:
            raise SystemExit(
                f"{readme}: marker guide:{m.group('path')} has no matching entry "
                f"in guide.yaml"
            )
        return m.group("start") + block_for(node) + m.group("end")

    return MARKER.sub(repl, text)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--check", action="store_true",
                    help="do not write; exit non-zero if any README is stale")
    args = ap.parse_args()

    if not GUIDES_DIR.is_dir():
        print(f"No guides directory at {GUIDES_DIR}", file=sys.stderr)
        return 1

    # A guide with no guide.yaml is not checked by anything: the glob below skips
    # it, so its commands drift freely and the two-file convention silently stops
    # being a convention. Fail on the reverse case the glob cannot see.
    #
    # HAND_WRITTEN is the deliberate, visible exception list. Adding to it is a
    # choice someone has to make in a diff; forgetting a guide.yaml is not.
    missing = [
        d.name
        for d in sorted(GUIDES_DIR.iterdir())
        if d.is_dir()
        and (d / "README.md").is_file()
        and not (d / "guide.yaml").is_file()
        and d.name not in HAND_WRITTEN
    ]
    if missing:
        print(
            "These guides have a README.md but no guide.yaml, so their commands are "
            "rendered by nobody and checked by nothing:",
            file=sys.stderr,
        )
        for name in missing:
            print(f"  docs/guides/{name}", file=sys.stderr)
        print(
            "Add a guide.yaml (see docs/guides/README.md), or add the name to "
            "HAND_WRITTEN in this script with a reason.",
            file=sys.stderr,
        )
        return 1

    stale, rendered = [], 0
    for guide_yaml in sorted(GUIDES_DIR.glob("*/guide.yaml")):
        readme = guide_yaml.parent / "README.md"
        if not readme.is_file():
            print(f"ERROR: {guide_yaml.parent.name} has guide.yaml but no README.md",
                  file=sys.stderr)
            return 1
        guide = yaml.safe_load(guide_yaml.read_text(encoding="utf-8")) or {}
        new = render(readme, guide)
        if new != readme.read_text(encoding="utf-8"):
            if args.check:
                stale.append(str(readme))
            else:
                readme.write_text(new, encoding="utf-8", newline="")
                print(f"rendered {readme.relative_to(GUIDES_DIR.parent.parent)}")
        rendered += 1

    if args.check and stale:
        print("These guide READMEs are out of date with their guide.yaml:",
              file=sys.stderr)
        for s in stale:
            print(f"  {s}", file=sys.stderr)
        print("Run: make guides-render", file=sys.stderr)
        return 1
    if args.check:
        print(f"{rendered} guide(s) up to date")
    return 0


if __name__ == "__main__":
    sys.exit(main())
