#!/usr/bin/env python3
"""Fail when the tenant Role has drifted from the generated ClusterRole.

`config/components/tenant-installable/manager-role.yaml` is the controller's
permissions as a NAMESPACED Role, and it is maintained BY HAND as
"manager-clusterrole.yaml with the cluster-scoped resources removed". The
ClusterRole is generated from kubebuilder markers, so it moves whenever a
controller gains a permission; the Role does not move with it.

It drifted, and the drift was invisible. `pods` gained `patch` for the warm
pool -- the controller adds a borrowed Pod to a model's InferencePool and takes
it out again -- but only in the generated half. On every namespace-scoped
install the controller then started, read its own permissions, and disabled the
pool:

    the warm pool is disabled: this controller may not patch Pods, so it could
    never lend one

which is the right refusal and the wrong reason: the guide says the shipped RBAC
grants it. Nothing failed at install time, no manifest was invalid, and the pool
was created and held its accelerators.

So: every namespaced (apiGroup, resource) the ClusterRole grants must be granted
by the Role with AT LEAST the same verbs. The Role may grant more (it does not),
and it may omit the cluster-scoped resources listed below, which are the only
ones a Role cannot carry.
"""

import sys
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover
    print("ERROR: PyYAML is required. pip install pyyaml", file=sys.stderr)
    sys.exit(1)

ROOT = Path(__file__).resolve().parent.parent
CLUSTER_ROLE = ROOT / "config" / "base" / "rbac" / "manager-clusterrole.yaml"
TENANT_ROLE = ROOT / "config" / "components" / "tenant-installable" / "manager-role.yaml"

# The only resources a Role legitimately cannot carry. Each is documented in the
# tenant Role's own header, with why an install without it still works.
CLUSTER_SCOPED = {
    ("", "nodes"),
    ("", "nodes/status"),
    ("", "namespaces"),
    # Kueue ClusterQueues and ResourceFlavors: read only by a quota entry with
    # kueue.enabled, granted by components/kueue-reader (prereqs phase). The
    # namespaced half, localqueues, is in the tenant Role and is checked.
    ("kueue.x-k8s.io", "clusterqueues"),
    ("kueue.x-k8s.io", "resourceflavors"),
}


def grants(path):
    """{(apiGroup, resource): set(verbs)} from a Role or ClusterRole document."""
    doc = yaml.safe_load(path.read_text(encoding="utf-8"))
    if not doc or "rules" not in doc:
        raise SystemExit("no rules in %s -- the parser found nothing, which would "
                         "compare equal to any other empty side." % path)
    out = {}
    for rule in doc["rules"] or []:
        for group in rule.get("apiGroups") or [""]:
            for resource in rule.get("resources") or []:
                out.setdefault((group, resource), set()).update(rule.get("verbs") or [])
    if not out:
        raise SystemExit("extracted no grants from %s" % path)
    return out


def main():
    cluster = grants(CLUSTER_ROLE)
    tenant = grants(TENANT_ROLE)

    problems = []
    for key, verbs in sorted(cluster.items()):
        if key in CLUSTER_SCOPED:
            continue
        have = tenant.get(key, set())
        missing = verbs - have
        if missing:
            group, resource = key
            problems.append("  %s%s: ClusterRole grants %s, tenant Role is missing %s"
                            % (group + "/" if group else "", resource,
                               ",".join(sorted(verbs)), ",".join(sorted(missing))))

    if problems:
        print("ERROR: the tenant Role has drifted from the generated ClusterRole:",
              file=sys.stderr)
        print("\n".join(problems), file=sys.stderr)
        print("\nAdd the verbs to %s, or -- if the resource really is cluster-scoped\n"
              "and an install without it still works -- list it in CLUSTER_SCOPED here\n"
              "and say why in that file's header." % TENANT_ROLE.relative_to(ROOT),
              file=sys.stderr)
        return 1

    print("tenant Role covers the ClusterRole (%d namespaced grants)"
          % (len(cluster) - len(CLUSTER_SCOPED & set(cluster))))
    return 0


if __name__ == "__main__":
    sys.exit(main())
