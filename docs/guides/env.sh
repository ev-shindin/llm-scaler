#!/usr/bin/env bash
#
# Shared environment for the WVA guides. Source it before running guide commands:
#
#     source docs/guides/env.sh
#
# Same shape as llm-d's guides/env.sh: versions and image references live here,
# because they change together and no guide should carry its own copy. The
# NAMESPACE deliberately does not — each guide exports the one it installs into,
# exactly as llm-d's guides do.

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
export REPO_ROOT

# The controller image the guides install.
#
# The default is a build of THIS branch, not a release: these manifests pass
# --external-scaler-bind-address, and no released image accepts it. Pointing the
# guides at ghcr.io/llm-d/...:latest gave a CrashLoopBackOff on "unknown flag",
# which reads like a broken image rather than version skew.
#
# Set this to a build of your own tree when you change controller code:
#     make docker-build docker-push IMG=ghcr.io/<you>/<repo>:<tag>
#
# The variable is IMG, because that is what the make targets read. It was
# exported here as WVA_IMAGE, which nothing anywhere reads: a reader testing an
# unmerged branch set it, saw no effect, and got the CrashLoopBackOff described
# above from the published image they thought they had replaced.
#
# TODO(IMG, release hardening): :main is a floating tag from a personal namespace,
# not a pinned digest -- deliberately, while this is still pre-release: the branch
# moves daily and a pinned digest would need updating by hand on every guide edit.
# Before a real release this needs a pin (a digest, or a versioned tag cut from
# CI) so two readers running the same guide a week apart get the same build.
# Left as a comment, not a behavior change: not yet, while the image is still a
# moving target on purpose.
export IMG="${IMG:-ghcr.io/ev-shindin/llm-scaling-manager:main}"

# The namespace WVA installs into.
#
# Left unset on purpose. The install uses WVA_NS, then NAMESPACE — llm-d's own
# variable — and if neither is set it FINDS the namespace running llm-d model
# servers. So a reader who has followed an llm-d guide already has this right,
# and everyone else can leave it alone and let the preflight report what it found.
#
#   export NAMESPACE=llm-d-optimized-baseline

# KEDA and Prometheus are prerequisites rather than variables: the install adds
# KEDA if the cluster has none, and detects Prometheus (on OpenShift, the
# platform's Thanos Querier at a fixed address). PROMETHEUS_URL overrides that.
