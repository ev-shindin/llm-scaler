# Troubleshooting


## Deployment Not Scaling Up

**Symptom**: Deployment remains at 0 replicas despite pending requests.

**Possible Causes**:

1. **InferencePool datastore is empty**:
   ```bash
   # Check if InferencePool exists and is reconciled
   kubectl get inferencepool
   ```
   
   WVA watches a single InferencePool API group (`inference.networking.k8s.io` or `inference.networking.x-k8s.io`). If the cluster's pools use the other group, the datastore stays empty and scale-from-zero never gets a recommendation.
   
   **Solution**: Ensure InferencePool is created and reconciled before creating VariantAutoscaling. When using **`make deploy-e2e-infra`**, `deploy/install-epp.sh` installs the GAIE standalone chart which creates the InferencePool after the EPP starts.

2. **Labels mismatch**:
   ```bash
   # Check deployment labels
   kubectl get deployment llama-8b-deployment -o jsonpath='{.spec.template.metadata.labels}'
   
   # Check InferencePool selector
   kubectl get inferencepool llama-pool -o jsonpath='{.spec.selector}'
   ```
   
   **Solution**: Ensure deployment labels match InferencePool selector.

3. **EPP metrics source not available**:
   ```bash
   # Check if EPP service exists
   kubectl get svc | grep epp
   ```
   
   **Solution**: Verify EndpointPicker service is running and metrics are being collected.

4. **No pending requests in queue**:

   Extract the Bearer token from the EPP metrics reader secret:
   ```bash
   TOKEN=$(kubectl -n workload-variant-autoscaler-system get secret wva-epp-metrics-token -o jsonpath='{.data.token}' | base64 --decode)
   ```

   Port-forward the EPP metrics service to localhost:9090:

   ```bash
   kubectl port-forward svc/epp 9090:9090
   ```

   In a separate terminal, query the metrics endpoint:
   ```bash
   curl -H "Authorization: Bearer $TOKEN" localhost:9090/metrics | grep inference_extension_flow_control_queue_size
   ```

   **Solution**: Verify requests are being sent to the correct model endpoint.

   The metric family was renamed: llm-d's EPP exports
   `llm_d_epp_flow_control_queue_size` and upstream gateway-api-inference-extension
   still exports `inference_extension_flow_control_queue_size`. Grep for both — WVA
   reads whichever exists.

### The EPP scrape is failing but WVA still wakes models (slowly)

**Symptom**: the log carries

```text
Scale-from-zero: EPP metrics scrape failing; reading the flow-control queue from
Prometheus instead. Wakes still work but are slower and bounded by the scrape
interval — fix the direct path.
```

and `wva_scale_from_zero_queue_fallback_active{pool="..."}` is `1`.

**What it means**: WVA reads the flow-control queue by scraping the EPP pod
**directly** — pod IP, EPP metrics port, bearer token projected at
`/var/run/secrets/epp-metrics/token`. Every other metric it consumes comes from
Prometheus, so this one path can fail on its own. When it does, WVA falls back to
reading the same metric from Prometheus so models still wake. Nothing looks broken
from the outside; wakes are just slower — bounded by the Prometheus scrape interval
rather than the engine's 100 ms loop — and a sample older than 90 s is ignored, so
a queue that has already drained cannot wake anything.

This is a degraded state, not a supported one. Check, in order:

1. **The token.** `Failed to read EPP metrics token` in the log means the projected
   volume is missing; WVA then scrapes unauthenticated and the EPP rejects it.
   ```bash
   kubectl -n workload-variant-autoscaler-system exec deploy/wva-controller-manager --      ls -l /var/run/secrets/epp-metrics/token
   ```
2. **The EPP's tokenreview RBAC**, which authorizes WVA's token. It is bound per
   release name, so a renamed or reinstalled EPP release leaves WVA's binding
   pointing at nothing.
   ```bash
   kubectl get clusterrolebinding | grep epp-tokenreview
   ```
3. **Network reachability to the EPP pod IP.** Prometheus scrapes the EPP too, so a
   NetworkPolicy admitting the monitoring namespace but not WVA's breaks exactly
   this path and no other.

The gauge returns to `0` and the log reports the scrape recovered once the direct
path works again.

### E2E and infra-only deploys

For e2e-style deploys, **`deploy/install-epp.sh`** enables EPP flow control when `ENABLE_SCALE_TO_ZERO=true` (adds the `flowControl` feature gate to the GAIE standalone chart). The **InferenceObjective** `e2e-default` is created by the scale-from-zero e2e tests (`test/e2e/fixtures`), not by the install scripts. See [deploy/install-epp.sh](../../deploy/install-epp.sh).

## Slow Scale-Up Response

**Symptom**: Deployment takes too long to scale up from zero.

**Possible Causes**:

1. **High concurrent processing load**:
   
   This can happen when there are many variants that are scaled down to zero, causing the scale-from-zero engine to process multiple scaling decisions simultaneously.
   
   **Solution**: Increase `SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY`:

   Add the environment variable to the WVA controller deployment:

   ```yaml
   apiVersion: apps/v1
   kind: Deployment
   metadata:
      name: controller-manager
      namespace: workload-variant-autoscaler-system
   spec:
      template:
         spec:
            containers:
            - name: manager
            env:
            - name: SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY
              value: "50"  # Increase for larger clusters
   ```

2. **Inference gateway not receiving requests**:
   
   **Solution**: Verify that requests are being routed through the inference gateway and not directly to model server endpoints.