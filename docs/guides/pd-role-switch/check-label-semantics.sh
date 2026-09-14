#!/bin/bash
# Prove why switchable.yaml keeps llm-d.ai/role OUT of the Deployment
# selector, by doing it both ways and relabelling a pod of each.
#
# No GPUs and no vLLM: this is Kubernetes ownership semantics. Runs in about a
# minute and is the negative control for the whole deployment shape.
#
#   ./check-label-semantics.sh [namespace]
set -uo pipefail

NS=${1:-${NAMESPACE:-default}}
IMAGE=${IMAGE:-quay.io/prometheus/busybox:latest}
K="kubectl -n $NS"

cleanup() {
  $K delete deploy labeltest-role-in-selector labeltest-role-out-of-selector \
     --ignore-not-found --wait=false >/dev/null 2>&1
  $K delete pod -l app=labeltest-a --ignore-not-found --wait=false >/dev/null 2>&1
  $K delete pod -l app=labeltest-b --ignore-not-found --wait=false >/dev/null 2>&1
}
trap cleanup EXIT

mk () {  # $1=name $2=app $3=selector-includes-role(yes/no)
  local sel="      app: $2"
  [ "$3" = "yes" ] && sel="$sel
      llm-d.ai/role: prefill"
  cat <<YAML | $K apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: { name: $1 }
spec:
  replicas: 2
  selector:
    matchLabels:
$sel
  template:
    metadata:
      labels:
        app: $2
        llm-d.ai/role: prefill
    spec:
      containers:
        - name: c
          image: $IMAGE
          command: ["/bin/sh","-c","sleep 3600"]
          resources: { limits: { cpu: 100m, memory: 64Mi } }
YAML
}

echo "### namespace=$NS"
cleanup; sleep 3
mk labeltest-role-in-selector     labeltest-a yes
mk labeltest-role-out-of-selector labeltest-b no

for i in $(seq 1 40); do
  n=$($K get pods -l 'app in (labeltest-a,labeltest-b)' --no-headers 2>/dev/null | grep -c Running)
  [ "${n:-0}" -ge 4 ] && break
  sleep 3
done
echo "### pods running: ${n:-0} (want 4)"
[ "${n:-0}" -ge 4 ] || { echo "### could not start the fixtures"; exit 2; }

A=$($K get pods -l app=labeltest-a --no-headers -o custom-columns=':metadata.name' | head -1)
B=$($K get pods -l app=labeltest-b --no-headers -o custom-columns=':metadata.name' | head -1)
$K label pod "$A" llm-d.ai/role=decode --overwrite >/dev/null
$K label pod "$B" llm-d.ai/role=decode --overwrite >/dev/null
sleep 5

CA=$($K get pods -l app=labeltest-a --no-headers 2>/dev/null | wc -l)
CB=$($K get pods -l app=labeltest-b --no-headers 2>/dev/null | wc -l)
OA=$($K get pod "$A" -o jsonpath='{.metadata.ownerReferences[0].name}' 2>/dev/null)
OB=$($K get pod "$B" -o jsonpath='{.metadata.ownerReferences[0].name}' 2>/dev/null)

echo
echo "role IN  selector: pods=$CA owner_of_relabelled=${OA:-<none, ORPHANED>}"
echo "role OUT selector: pods=$CB owner_of_relabelled=${OB:-<none, ORPHANED>}"
echo

RC=0
if [ "$CA" -gt 2 ] && [ -z "$OA" ]; then
  echo "PASS  role in the selector: relabelling orphaned the pod and forced a replacement"
else
  echo "FAIL  expected an orphan and a replacement with role in the selector"; RC=1
fi
if [ "$CB" -eq 2 ] && [ -n "$OB" ]; then
  echo "PASS  role out of the selector: pod kept its owner, no replacement"
else
  echo "FAIL  expected the pod to keep its owner with role out of the selector"; RC=1
fi
exit $RC
