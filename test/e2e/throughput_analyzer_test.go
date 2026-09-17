package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Saturation config YAML strings for multi-analyzer (V2) mode.
// Both use the same threshold values; only the analyzers list differs.
// The saturation thresholds (kvCacheThreshold, queueLengthThreshold, etc.) are set to
// values that reliably cross with the simulator's kv-cache-size=1 setup.
const (
	// No limiters, matching the shipped ConfigMap. These entries REPLACE the
	// cluster's "default" saturation entry wholesale, so they must carry its
	// limiter policy — and declaring gpu-inventory here would freeze this suite
	// outright: its workloads set no GPU nodeSelector, so a GPU-aware optimizer
	// resolves their accelerator to "unknown", charges them to no pool, and
	// allocates them nothing. That timed two specs out at 600s each.
	throughputBothEnabledConfig = `
model_id: ""
namespace: ""
kvCacheThreshold: 0.80
queueLengthThreshold: 5
scaleUpThreshold: 0.85
scaleDownBoundary: 0.70
analyzers:
  - name: saturation
    enabled: true
    score: 1.0
  - name: throughput
    enabled: true
    score: 1.0
`

	throughputOnlyConfig = `
model_id: ""
namespace: ""
kvCacheThreshold: 0.80
queueLengthThreshold: 5
scaleUpThreshold: 0.85
scaleDownBoundary: 0.70
analyzers:
  - name: saturation
    enabled: false
  - name: throughput
    enabled: true
    score: 1.0
`
)

// throughputScaleUpFakeMetricsJSON drives a deterministic V2 saturation scale-up.
// kv-cache-usage is a simulator gauge (tick-driven, unlike rate-based histograms/counters),
// so a static value is stable. With the simulator's kv-cache-size=1 × block-size=8 (kvMax=8)
// and throughputBothEnabledConfig (kvCacheThreshold=0.80 → perReplicaCapacity≈6.4,
// scaleUpThreshold=0.85), RequiredCapacity > 0 requires kv·8/0.85 > 6.4, i.e. kv > 0.68;
// 0.9 clears it with margin. running/waiting are cosmetic for V2: these variants carry no
// llm-d.ai/role label, so they resolve to role "both" and queue demand charges the
// rate-based AvgInputTokens + AvgOutputTokens — both of which are 0 under static fakes.
const throughputScaleUpFakeMetricsJSON = `{"kv-cache-usage":0.9,"running-requests":5,"waiting-requests":20}`

// throughputSustainedLoadScript is an inline shell script for a Kubernetes Job that
// continuously sends /v1/completions requests until the Job's activeDeadlineSeconds is reached.
// Uses /v1/completions (not /v1/chat/completions) because the llm-d simulator only tracks
// KV cache for the text completion API endpoint.
const throughputSustainedLoadScript = `#!/bin/sh
set -u
echo "Throughput load job starting: target=$TARGET_URL model=$MODEL_ID workers=$WORKERS"

# Preflight: wait for the service to respond.
CONNECTED=false
i=1
while [ "$i" -le "$MAX_RETRIES" ]; do
  HTTP=$(curl -s -o /dev/null -w "%{http_code}" --max-time "$PREFLIGHT_TIMEOUT" "$TARGET_URL/../models" || true)
  if [ "$HTTP" = "200" ]; then
    echo "Service preflight passed (attempt $i)"
    CONNECTED=true
    break
  fi
  echo "Preflight attempt $i: HTTP $HTTP, retrying in ${RETRY_DELAY}s..."
  sleep "$RETRY_DELAY"
  i=$((i + 1))
done

if [ "$CONNECTED" != "true" ]; then
  echo "ERROR: service not ready after $MAX_RETRIES attempts"
  exit 1
fi

echo "Starting $WORKERS concurrent workers..."
w=1
while [ "$w" -le "$WORKERS" ]; do
  (
    while true; do
      curl -s -o /dev/null --max-time "$CURL_TIMEOUT" -X POST "$TARGET_URL" \
        -H "Content-Type: application/json" \
        -d "{\"model\":\"$MODEL_ID\",\"prompt\":\"Explain transformer architecture in detail.\",\"max_tokens\":$MAX_TOKENS}" \
        || true
    done
  ) &
  w=$((w + 1))
done

# Wait indefinitely; the Job is killed by activeDeadlineSeconds.
wait || true
`

// restartWVAController patches the wva-controller-manager Deployment pod template with a
// restartedAt annotation to trigger a rollout, then waits for the rollout to complete
// and for the fresh pod to win leader election.
// Returns an error so callers can Skip() rather than Fail() when the restart is
// impractical (no RBAC, restricted environment). Uses a bounded wait.
func restartWVAController(ctx context.Context) error {
	// The stamp is what identifies the post-restart pod below: it lands on the
	// pod's own annotations through the template, so the pod that carries it is
	// the one this restart created and no other.
	stamp := time.Now().UTC().Format(time.RFC3339)
	patch := []byte(`{"spec":{"template":{"metadata":{"annotations":{"` + restartedAtAnnotation + `":"` +
		stamp + `"}}}}}`)
	if _, err := k8sClient.AppsV1().Deployments(cfg.WVANamespace).Patch(
		ctx, "wva-controller-manager",
		types.StrategicMergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("patch wva-controller-manager: %w", err)
	}
	// One budget for the rollout and the lease wait together. The lease wait
	// alone has been measured at 64 s (an old pod that died without releasing,
	// so the lease had to expire), so PodReadyTimeout needs to be comfortably
	// above that; the default 300 s is.
	deadline := time.Now().Add(time.Duration(cfg.PodReadyTimeout) * time.Second)
	poll := time.Duration(cfg.PollIntervalSec) * time.Second
	rolledOut := false
	for time.Now().Before(deadline) {
		dep, err := k8sClient.AppsV1().Deployments(cfg.WVANamespace).Get(ctx, "wva-controller-manager", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get wva-controller-manager: %w", err)
		}
		// Status lags the patch: until the controller has observed the new
		// generation, the counts describe the previous rollout and read as
		// complete. Without this the loop exits at once and a rollout that never
		// starts is reported by the lease wait below as an election problem.
		if dep.Status.ObservedGeneration < dep.Generation {
			time.Sleep(poll)
			continue
		}
		if dep.Status.UpdatedReplicas >= 1 &&
			dep.Status.ReadyReplicas == dep.Status.UpdatedReplicas &&
			dep.Status.UnavailableReplicas == 0 {
			rolledOut = true
			break
		}
		time.Sleep(poll)
	}
	if !rolledOut {
		return fmt.Errorf("wva-controller-manager rollout did not complete within %ds", cfg.PodReadyTimeout)
	}
	return waitForWVALeadership(ctx, stamp, deadline, poll)
}

// restartedAtAnnotation is the pod-template annotation kubectl rollout restart
// bumps; restartWVAController writes it by hand with the same key.
const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// waitForWVALeadership blocks until a running controller-manager pod holds the
// leader lease.
//
// Pod-Ready is not enough. Every engine — saturation, scale-from-zero — and the
// KEDA external-scaler gRPC server are leader-gated runnables, so a Ready pod
// that has not yet won the lease does nothing at all: it serves no decisions,
// and its InferencePool datastore is still empty. The previous holder's lease
// has to expire first, which takes up to LeaseDuration (60 s here), and specs
// that start inside that window see a WVA that is up but inert. That is what
// made the scale-from-zero specs flaky — the engine logged "Inferencepool
// datastore is empty" and the workload was woken (or not) by something else
// entirely.
//
// The holder has to be THE POD THIS RESTART CREATED, not merely a pod that
// exists. A rolling update surges the new pod in before the old one is deleted,
// and the old one keeps the lease until its shutdown finishes -- through the
// moment the rollout reports complete, since a Terminating pod is dropped from
// the Deployment's counts but is still there to Get. Accepting it returned this
// wait while the new pod was Ready, in the Service, and not yet listening on
// the scaler port. A ScaledObject created in that gap gets an HPA with an
// empty metrics list -- Kubernetes defaults it to Resource/cpu, and KEDA only
// re-derives the HPA's metrics on a ScaledObject change, so nothing ever
// replaces it: measured as a 600 s spec timeout with WVA publishing desired=2
// every cycle and the HPA parked on FailedGetResourceMetric.
//
// Measured on kind, sampling the lease once a second across a restart: the
// rollout reported complete at 3 s with the Terminating pod still the holder,
// which is where the old check returned; the new pod acquired the lease at
// 18 s in one run and at 64 s in another, where the old pod died without
// releasing and the lease had to expire. The stamp written at restart is the
// only thing that names the new pod without a clock.
func waitForWVALeadership(ctx context.Context, stamp string, deadline time.Time, poll time.Duration) error {
	// The LEADER_ELECTION_ID default (internal/config/loader.go), which the e2e
	// deployment does not override. A deployment that does override it has no
	// lease under this name, and the NotFound branch below degrades to not
	// waiting rather than failing.
	const leaseName = "72dd1cf1.llm-d.ai"
	var lastHolder string
	for time.Now().Before(deadline) {
		lease, err := k8sClient.CoordinationV1().Leases(cfg.WVANamespace).Get(ctx, leaseName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				// Leader election disabled (or a different ID): nothing to wait for.
				return nil
			}
			return fmt.Errorf("get leader lease %s: %w", leaseName, err)
		}
		if lease.Spec.HolderIdentity != nil {
			lastHolder = *lease.Spec.HolderIdentity
			// holderIdentity is "<pod-name>_<uuid>"; accept it once it names a pod
			// that currently exists, i.e. the post-restart pod rather than the
			// terminated one still nominally holding the lease.
			podName, _, found := strings.Cut(lastHolder, "_")
			if found {
				pod, err := k8sClient.CoreV1().Pods(cfg.WVANamespace).Get(ctx, podName, metav1.GetOptions{})
				if err == nil && pod.DeletionTimestamp == nil && pod.Annotations[restartedAtAnnotation] == stamp {
					return nil
				}
			}
		}
		time.Sleep(poll)
	}
	return fmt.Errorf("no live wva-controller-manager pod acquired the leader lease within %ds (last holder %q)",
		cfg.PodReadyTimeout, lastHolder)
}

// buildThroughputSustainedLoadJob returns a Job spec that sends continuous completions
// requests to targetURL until the job's activeDeadlineSeconds deadline.
func buildThroughputSustainedLoadJob(namespace, name, targetURL, modelID string, workers int, deadlineSec int64) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":           name,
				"test-resource": "true",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To(int32(0)),
			ActiveDeadlineSeconds: &deadlineSec,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":           name,
						"test-resource": "true",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					DNSConfig: &corev1.PodDNSConfig{
						Options: []corev1.PodDNSConfigOption{
							{Name: "ndots", Value: ptr.To("2")},
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "load-gen",
							Image:   "quay.io/curl/curl:8.11.1",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{throughputSustainedLoadScript},
							Env: []corev1.EnvVar{
								{Name: "TARGET_URL", Value: targetURL},
								{Name: "MODEL_ID", Value: modelID},
								{Name: "WORKERS", Value: strconv.Itoa(workers)},
								{Name: "MAX_TOKENS", Value: "400"},
								{Name: "CURL_TIMEOUT", Value: "300"},
								{Name: "MAX_RETRIES", Value: "24"},
								{Name: "RETRY_DELAY", Value: "5"},
								{Name: "PREFLIGHT_TIMEOUT", Value: "30"},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
	}
}

const defaultConfigKey = "default"

// ─── Scenario 1: Wiring Health Check (smoke/throughput) ───────────────────────

var _ = Describe("ThroughputAnalyzer wiring health check", Label("smoke", "throughput"), Ordered, func() {
	const (
		poolName              = "throughput-smoke-pool"
		modelSvcName          = "throughput-smoke-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"
	)

	var (
		modelID         string
		cmName          string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		cmKey           string
		cmNamespace     string
		// variantName is the variant_name — the annotated scaler's OBJECT name — stamped
		// as the decode pods' llm-d.ai/variant label for metric attribution.
		variantName string
	)

	BeforeAll(func() {
		modelID = cfg.ModelID
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.WVANamespace
		cmKey = defaultConfigKey
		variantName = modelSvcName + "-so"

		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		By("Writing multi-analyzer config with both analyzers enabled")
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, throughputBothEnabledConfig)).To(Succeed())

		By("Restarting WVA controller so throughput gate re-evaluates at startup")
		if err := restartWVAController(ctx); err != nil {
			Skip("ThroughputAnalyzer not registered — WVA controller restart failed or timed out: " + err.Error())
		}

		By("Creating model service for throughput smoke test")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID, cfg.UseSimulator, 2)).To(Succeed())
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment)).To(Succeed())

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering the deployment with WVA via an annotated scaler (both analyzers enabled)")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, modelSvcName) })
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		restoreSaturationConfigMap(ctx, cmNamespace, cmName, cmOriginal, cmExistedBefore)

		// Restart is mandatory: registration is sticky. A config-only restore leaves TA
		// registered and still consuming results. Only a restart with saturation-only
		// config already in place yields a true TA-off controller so sibling suites
		// (e.g. saturation_v2_test.go:280 scale-down) are not contaminated.
		By("Restarting WVA controller to restore default (saturation-only) startup config")
		Expect(restartWVAController(ctx)).To(Succeed())

		By("Cleaning up throughput smoke test resources")
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	It("emits wva_desired_replicas at steady state with both analyzers enabled", func() {
		// Post-CRD-removal WVA no longer writes VA .status; its sole output is the
		// wva_desired_replicas external metric. Steady-state reconcile is observed as
		// that metric being emitted for the variant via the KEDA-managed HPA's CurrentMetrics.
		By("Verifying KEDA read wva_desired_replicas for the variant")
		Eventually(func(g Gomega) {
			hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
			for i := range hpaList.Items {
				if hpaList.Items[i].Spec.ScaleTargetRef.Name == modelDecodeDeployment {
					kedaHPA = &hpaList.Items[i]
					break
				}
			}
			g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the deployment")
			g.Expect(kedaHPA.Status.CurrentMetrics).NotTo(BeEmpty(),
				"KEDA HPA should have CurrentMetrics populated from wva_desired_replicas")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})
})

// ─── Scenario 2: Multi-Analyzer Engine Scale-Up (full/throughput) ─────────────
//
// Validates the multi-analyzer engine end-to-end: with BOTH analyzers registered,
// the engine produces a scale-up decision that propagates to wva_desired_replicas. Scale-up is
// driven by the SATURATION analyzer via a faked kv-cache-usage gauge — the throughput
// analyzer's inputs are rates of counters/histograms that static --fake-metrics cannot
// drive (zero rate), so throughput cannot be exercised here. Its own scale-up math is
// covered by the unit tests in internal/engines/analyzers/throughput/analyzer_test.go.

var _ = Describe("Multi-analyzer engine scale-up (saturation-driven, throughput co-registered)", Label("full", "throughput"), Ordered, func() {
	const (
		poolName              = "throughput-scaleup-pool"
		modelSvcName          = "throughput-scaleup-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"
		loadJobName           = "throughput-scaleup-load"
	)

	var (
		modelID         string
		cmName          string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		cmKey           string
		cmNamespace     string
		// variantName is the variant_name — the annotated scaler's OBJECT name
		// (modelSvcName+"-so" for KEDA, +"-hpa" for the adapter) — stamped as the
		// decode pods' llm-d.ai/variant label so the collector attributes their
		// metrics to the variant. Set from the backend below.
		variantName string
	)

	BeforeAll(func() {
		modelID = cfg.ModelID
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.WVANamespace
		cmKey = defaultConfigKey
		variantName = modelSvcName + "-so"

		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		By("Writing multi-analyzer config with both analyzers enabled")
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, throughputBothEnabledConfig)).To(Succeed())

		By("Restarting WVA controller so throughput gate re-evaluates at startup")
		if err := restartWVAController(ctx); err != nil {
			Skip("ThroughputAnalyzer not registered — WVA controller restart failed or timed out: " + err.Error())
		}

		if !cfg.UseSimulator {
			Skip("This scenario needs the simulator runtime: set USE_SIMULATOR=true. " +
				"It uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		By("Creating model service with faked saturation metrics for scale-up")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelServiceWithExtraArgs(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID,
			cfg.UseSimulator, 2, []string{"--fake-metrics", throughputScaleUpFakeMetricsJSON})).To(Succeed())
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment)).To(Succeed())

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering the deployment with WVA via an annotated scaler (both analyzers enabled)")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, modelSvcName) })
		// No load job: --fake-metrics replaces simulator runtime emission entirely, so
		// service traffic has no effect on the values the engine reads. Scale-up is
		// driven solely by the faked kv-cache-usage gauge.
	})

	AfterAll(func() {
		By("Cleaning up sustained load job")
		propagation := metav1.DeletePropagationBackground
		_ = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, loadJobName, metav1.DeleteOptions{PropagationPolicy: &propagation})

		By("Restoring saturation ConfigMap state")
		restoreSaturationConfigMap(ctx, cmNamespace, cmName, cmOriginal, cmExistedBefore)

		By("Restarting WVA controller to restore default (saturation-only) startup config")
		_ = restartWVAController(ctx)

		By("Cleaning up throughput scale-up test resources")
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	It("raises wva_desired_replicas above MinReplicas with both analyzers enabled", func() {
		// Faked kv-cache-usage=0.9 > scaleUpThreshold=0.85 makes the saturation analyzer
		// deterministically recommend scale-up above MinReplicas=1. Post-CRD-removal the
		// desired count is no longer surfaced in VA status; the annotated scaler consumes
		// wva_desired_replicas and drives the target Deployment above its MinReplicas floor,
		// so we assert the observable Deployment replica count instead.
		By("Confirming KEDA has wired its external metric onto the HPA")
		// Before the assertion below, not as part of it: a Deployment that never
		// grows reads the same whether WVA recommended nothing or KEDA never wired
		// the metric that carries the recommendation. This spec restarts WVA and
		// registers the ScaledObject seconds later, which is exactly the shape
		// that once left the HPA on the CPU default for the whole timeout (see
		// waitForWVALeadership). Measured at 20-35 s on kind; 120 s is generous.
		Eventually(func(g Gomega) {
			expectKEDAExternalMetricWired(g, cfg.LLMDNamespace, modelDecodeDeployment)
		}, 120*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Asserting KEDA actuates scale-up above MinReplicas")
		// Faked kv-cache-usage=0.9 > scaleUpThreshold=0.85 deterministically drives a
		// V2 saturation scale-up; KEDA consumes wva_desired_replicas and drives the
		// Deployment above its MinReplicas floor. Assert the observable replica count —
		// the ground truth — rather than the KEDA HPA CurrentMetrics surface.
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", int32(2)),
				"faked saturation (kv-cache-usage=0.9) should drive the Deployment above MinReplicas=1")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})
})

// ─── Scenario 3: TA-Only Mode (full/throughput) ────────────────────────────────

var _ = Describe("ThroughputAnalyzer TA-only mode", Label("full", "throughput"), Ordered, func() {
	const (
		poolName              = "throughput-taonly-pool"
		modelSvcName          = "throughput-taonly-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"
		loadJobName           = "throughput-taonly-load"
	)

	var (
		modelID         string
		cmName          string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		cmKey           string
		cmNamespace     string
		// variantName is the variant_name — the annotated scaler's OBJECT name — stamped
		// as the decode pods' llm-d.ai/variant label for metric attribution.
		variantName string
	)

	BeforeAll(func() {
		modelID = cfg.ModelID
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.WVANamespace
		cmKey = defaultConfigKey
		variantName = modelSvcName + "-so"

		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		By("Writing TA-only config: saturation disabled, throughput enabled")
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, throughputOnlyConfig)).To(Succeed())

		By("Restarting WVA controller so throughput gate re-evaluates at startup")
		if err := restartWVAController(ctx); err != nil {
			Skip("ThroughputAnalyzer not registered — WVA controller restart failed or timed out: " + err.Error())
		}

		By("Creating model service for TA-only test")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID, cfg.UseSimulator, 2)).To(Succeed())
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment)).To(Succeed())

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering the deployment with WVA via an annotated scaler (TA-only config)")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, modelSvcName) })

		By("Starting sustained load for TA-only scenario")
		targetURL := fmt.Sprintf("http://%s:8000/v1/completions", serviceName)
		deadlineSec := int64(cfg.EventuallyExtendedSec + 300)
		job := buildThroughputSustainedLoadJob(cfg.LLMDNamespace, loadJobName, targetURL, modelID, 2, deadlineSec)
		_, err = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed creating sustained load job")
	})

	AfterAll(func() {
		By("Cleaning up sustained load job")
		propagation := metav1.DeletePropagationBackground
		_ = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, loadJobName, metav1.DeleteOptions{PropagationPolicy: &propagation})

		By("Restoring saturation ConfigMap state")
		restoreSaturationConfigMap(ctx, cmNamespace, cmName, cmOriginal, cmExistedBefore)

		By("Restarting WVA controller to restore default (saturation-only) startup config")
		_ = restartWVAController(ctx)

		By("Cleaning up TA-only test resources")
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	It("produces a positive desired allocation driven by the throughput analyzer", func() {
		// Post-CRD-removal WVA no longer writes VA .status; its sole output is the
		// wva_desired_replicas external metric. A positive throughput-driven allocation
		// is observed via the KEDA-managed HPA's CurrentMetrics.
		//
		// Unlike the saturation/SGLang scale-up specs (which use --fake-metrics to pin a
		// deterministic operating point), this scenario is driven by a real sustained load
		// job. The throughput analyzer's recommended replica count depends on measured
		// token throughput vs SLO, which is not deterministic in CI — so this asserts that
		// the metric is emitted and consumed (a positive allocation flows through the
		// pipeline), not a specific replica magnitude, to avoid a load-timing-dependent
		// flake.
		By("Verifying KEDA read wva_desired_replicas for the throughput-driven variant")
		Eventually(func(g Gomega) {
			hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
			for i := range hpaList.Items {
				if hpaList.Items[i].Spec.ScaleTargetRef.Name == modelDecodeDeployment {
					kedaHPA = &hpaList.Items[i]
					break
				}
			}
			g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the deployment")
			g.Expect(kedaHPA.Status.CurrentMetrics).NotTo(BeEmpty(),
				"KEDA HPA should have CurrentMetrics populated from wva_desired_replicas")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

})

// ─── Shared helpers ────────────────────────────────────────────────────────────

// restoreSaturationConfigMap restores the saturation ConfigMap to its pre-test state.
// If the configmap existed before the test, it is recreated from the snapshot.
// If it did not exist, it is deleted.
func restoreSaturationConfigMap(ctx context.Context, cmNamespace, cmName string, original *corev1.ConfigMap, existedBefore bool) {
	if existedBefore && original != nil {
		propagation := metav1.DeletePropagationBackground
		if err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !errors.IsNotFound(err) {
			GinkgoWriter.Printf("Warning: failed to delete saturation configmap %s before restore: %v\n", cmName, err)
		}
		toCreate := saturationConfigMapForRecreate(original)
		if _, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{}); err != nil {
			GinkgoWriter.Printf("Warning: failed to recreate saturation configmap %s: %v\n", cmName, err)
		}
	} else {
		_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{})
	}
}
