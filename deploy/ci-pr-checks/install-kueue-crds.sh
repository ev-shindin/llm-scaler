#!/usr/bin/env bash
# Installs the three Kueue CRDs the quota limiter's Kueue reader lists --
# ClusterQueue, LocalQueue, ResourceFlavor -- and nothing else.
#
# The reader never talks to the Kueue controller: it lists objects through the
# API server. So the e2e that exercises it needs the KINDS to exist, not Kueue
# itself, and installing Kueue's controller (webhooks, cert rotation, a minute
# of startup) into the kind cluster would add a dependency the test does not
# use to a job that is red often enough already.
#
# The base CRDs are applied rather than Kueue's crd kustomization: that one
# patches in conversion webhooks pointing at the Kueue webhook Service, which
# would not exist here. The bases carry no conversion stanza -- both served
# versions map to one schema and the API server converts between them itself.
#
# Refuses to touch a cluster that already has the CRD. A server-side apply of a
# pinned version over a running Kueue would be a CRD downgrade on somebody
# else's install, and this script is meant for a throwaway kind cluster.
set -euo pipefail

KUEUE_VERSION="${KUEUE_VERSION:-v0.19.5}"
base="https://raw.githubusercontent.com/kubernetes-sigs/kueue/${KUEUE_VERSION}/config/components/crd/bases"
kinds=(clusterqueues localqueues resourceflavors)

if kubectl get crd clusterqueues.kueue.x-k8s.io >/dev/null 2>&1; then
    echo "Kueue CRDs already present; leaving them as they are"
    exit 0
fi

for kind in "${kinds[@]}"; do
    # Server-side: the CRD schemas are far past the client-side annotation limit.
    kubectl apply --server-side -f "${base}/kueue.x-k8s.io_${kind}.yaml"
done
for kind in "${kinds[@]}"; do
    kubectl wait --for=condition=Established "crd/${kind}.kueue.x-k8s.io" --timeout=60s
done
echo "Kueue CRDs ${KUEUE_VERSION} installed: ${kinds[*]}"
