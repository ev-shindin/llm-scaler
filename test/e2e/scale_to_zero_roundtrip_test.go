package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// Serve, park, and wake the SAME model — the handoff neither half covered.
//
// The two halves are tested separately and both pass: scale_to_zero_test.go parks
// a serving model, and the scale-from-zero suites wake one that was created at
// zero. Nothing joined them, and the join is where the interesting failures live,
// because parking and waking are different components reading different signals:
// the steady-state enforcer parks on the request counter, while the
// scale-from-zero engine wakes on the EPP's flow-control queue. A model that
// parks but cannot be woken is a permanent outage, and a suite built from either
// half alone reports success on it.
//
// It also exercises the guard that only exists on this path. A model just woken
// from zero has served nothing yet, so the very counter that parks it still reads
// idle — and the enforcer would park it again immediately, undoing the wake in a
// loop. activation-retention holds it for retentionPeriod. That hold is
// unreachable in either half: the parking suite never wakes, and the wake suites
// never had a policy that would re-park.
//
// The fixture must satisfy BOTH mechanisms at once, which is why it is not simply
// either suite's setup:
//
//   - Waking needs the EPP flow-control queue, so the workload must sit in the
//     InferencePool under test (the default llm-d.ai/guide label) and the wake
//     traffic must go through the GATEWAY, where it can queue. Traffic sent
//     straight at the model's Service never queues and never wakes anything.
//   - Parking needs the request counter to exist and then fall idle, so real
//     traffic must be served FIRST — a model Prometheus has never seen returns no
//     series, which is reported as an error rather than as zero, and is
//     deliberately refused.
var _ = Describe("Scale-To-Zero Feature - park and wake the same model", Serial, Label("full"), Ordered, func() {
	const (
		modelSvcName = "stz-rt-ms"
		decodeDeploy = modelSvcName + "-decode"
		poolName     = "stz-rt-pool"
		scalerBase   = "stz-rt"
		variantName  = "stz-rt-so"
		loadRequests = 40
		// Short enough to park inside a test run; the default is 10m. The policy
		// entry, the earliest-legal-park check and the re-park window all read this
		// one value so they cannot drift apart.
		retentionDuration = time.Minute
		// Same reasoning as the parking suite: generous, because from cold the
		// discovery, enrichment and metrics caches all start empty and the first
		// idle evaluation lands minutes later.
		parkingBudget = 12 * time.Minute
		wakeBudget    = 5 * time.Minute
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		gatewayService  string
		triggerJobName  string
		loadStarted     time.Time
	)

	BeforeAll(func() {
		if !cfg.ScaleToZeroEnabled {
			Skip("This suite parks and wakes a model: set SCALE_TO_ZERO_ENABLED=true")
		}
		if ok, why := eppFlowControlAvailable(ctx, crClient, cfg.WVANamespace, cfg.LLMDNamespace); !ok {
			Skip("EPP flow control is not available, so there is no wake signal to test: " + why)
		}

		modelID = sfzModelID("stz-rt")
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.WVANamespace

		By("Waiting for EPP pods to be ready")
		Eventually(func(g Gomega) {
			pods, err := utils.FindExistingEPPPods(ctx, k8sClient, cfg.LLMDNamespace, cfg.EPPServiceName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).ToNot(BeEmpty())
		}).Should(Succeed())

		By("Applying InferenceObjective for GIE queuing")
		poolRefName := cfg.PoolName
		if poolRefName == "" {
			poolRefName = strings.TrimSuffix(cfg.EPPServiceName, "-epp")
		}
		ioApplied, errIO := fixtures.EnsureInferenceObjective(ctx, crClient, cfg.LLMDNamespace, poolRefName)
		Expect(errIO).NotTo(HaveOccurred())
		if !ioApplied {
			Skip("InferenceObjective API not available on cluster; the wake half cannot run")
		}
		DeferCleanup(func() {
			_ = fixtures.DeleteInferenceObjective(context.Background(), crClient, cfg.LLMDNamespace)
		})

		By("Snapshotting the scaling policy ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			// A false "did not exist" here would make the restore delete the shared
			// ConfigMap without recreating it.
			Expect(err).NotTo(HaveOccurred(), "reading existing scaling policy ConfigMap")
		}

		By("Enabling scale-to-zero for this model with a short retention")
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, defaultConfigKey,
			buildSaturationConfigYAML())).To(Succeed())
		modelEntry := buildSaturationConfigYAMLWithModel(
			0.80, 1, 0.85, 0.70, modelID, cfg.LLMDNamespace,
		) + fmt.Sprintf("scaleToZero:\n  enabled: true\n  retentionPeriod: %s\n", retentionDuration)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, "stz-roundtrip-model", modelEntry)).To(Succeed())

		By("Creating the model service SERVING, inside the pool under test")
		// No WithPoolGuide: the default guide label is what places these pods in the
		// InferencePool the EPP fronts, and pool membership is what lets the wake
		// traffic queue later. Overriding it would make this model unwakeable.
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName,
			poolName, modelID, cfg.UseSimulator, cfg.MaxNumSeqs)).To(Succeed())
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, decodeDeploy, 8000)).To(Succeed())
		// Without the ServiceMonitor the request counter never reaches Prometheus and
		// the model can never park: the suite would fail on a missing precondition.
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS,
			cfg.LLMDNamespace, modelSvcName, decodeDeploy)).To(Succeed())

		By("Waiting for the model to be ready to serve")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, decodeDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", int32(1)), "model must serve before it can be parked")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering it with minReplicaCount=0 over the external-scaler push channel")
		// The push trigger is not optional here as it is in the parking suite. A
		// workload at zero is woken only by the activation signal WVA pushes to KEDA,
		// which is the sole writer on the scale subresource.
		_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBase)
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBase,
			decodeDeploy, variantName, 0, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"),
			fixtures.WithExternalScalerPushTrigger(externalScalerAddress()))).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBase)
		})

		By("Discovering the inference gateway the wake traffic must go through")
		gatewayService = discoverInferenceGateway()
	})

	AfterAll(func() {
		restoreSaturationConfigMap(ctx, cmNamespace, cmName, cmOriginal, cmExistedBefore)
		_ = fixtures.DeleteBurstLoadJob(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		if triggerJobName != "" {
			propagation := metav1.DeletePropagationBackground
			_ = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, triggerJobName,
				metav1.DeleteOptions{PropagationPolicy: &propagation})
		}
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
	})

	It("serves traffic, so the idle signal exists at all", func() {
		By("Driving requests through the model service")
		// See the guard in the parking spec: a LOWER bound on the last request is
		// what the retention window has to be measured against.
		loadStarted = time.Now()
		// The path matters and fails silently: the generator POSTs wherever pointed,
		// discards the body and exit code, and counts requests SENT, so against a bare
		// host every request 404s while it still reports success.
		target := fmt.Sprintf("http://%s-service.%s.svc.cluster.local:8000/v1/chat/completions",
			modelSvcName, cfg.LLMDNamespace)
		Expect(fixtures.EnsureBurstLoadJob(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, target,
			fixtures.LoadConfig{
				Strategy:     "synthetic",
				NumPrompts:   loadRequests,
				InputTokens:  32,
				OutputTokens: 32,
				ModelID:      modelID,
			})).To(Succeed())

		pc := roundTripProm()
		Eventually(func(g Gomega) {
			v, err := pc.QueryWithRetry(ctx, fmt.Sprintf(
				`count(vllm:request_success_total{namespace=%q,model_name=%q})`, cfg.LLMDNamespace, modelID))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).To(BeNumerically(">", 0),
				"vllm:request_success_total must exist for this model, or nothing can park")
		}, 3*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("parks it once it goes idle", func() {
		By("Stopping the load")
		Expect(fixtures.DeleteBurstLoadJob(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)).To(Succeed())

		By("Waiting for the deployment to reach zero replicas")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, decodeDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(*dep.Spec.Replicas).To(Equal(int32(0)),
				"an idle model whose policy enables scale-to-zero must park; if it does not, "+
					"check wva_model_scaling_blocked for the reason")
		}, parkingBudget, 10*time.Second).Should(Succeed())

		// A zero reached too fast is not this feature working. The variant name is
		// deterministic, so a parked decision left in Prometheus by an earlier run
		// makes KEDA deactivate the fresh deployment within seconds -- the SGLang
		// sibling of this suite once "passed" that way in 15 seconds. increase()
		// over the retention window cannot fall to zero before that window has
		// elapsed, so anything faster is stale state rather than a decision.
		//
		// Measured from when the LOAD JOB WAS CREATED, which is the subtle part.
		// The obvious anchor -- the moment the job is deleted -- is an UPPER bound
		// on the last request: the burst sends a fixed number of prompts and then
		// sits there finished, so by the time this spec deletes it the model may
		// already have been idle for the whole window. CI measured exactly that:
		// the enforcer reported an EMPTY request-count window one cycle BEFORE the
		// load was stopped, parked 14s later, and the spec called a correct park
		// stale. The window the enforcer measures runs from the LAST REQUEST, so
		// the guard needs a lower bound on it, and job creation is one -- every
		// request this suite makes comes after it. No grace is needed for the same
		// reason, and the floor is the whole window rather than a shaved-down one.
		Expect(time.Since(loadStarted)).To(BeNumerically(">=", retentionDuration),
			"parked sooner than the retention window could have elapsed since the first "+
				"request this suite made: leftover state, not an idle-driven decision")
	})

	It("wakes it again when requests queue at the gateway", func() {
		// This is the join. Everything above proves the model can be put away;
		// nothing above proves it can be got back, and a model that parks but cannot
		// wake is a permanent outage that both halves report as success.
		By("Requiring the serving pool to be empty, or nothing will ever queue")
		requireEmptyServingPool()

		By("Sending requests through the gateway, where they can queue in the EPP")
		triggerJobName = fmt.Sprintf("stz-roundtrip-wake-%d", time.Now().UnixNano())
		job := createScaleFromZeroTriggerJob(triggerJobName, cfg.LLMDNamespace, gatewayService, modelID)
		_, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to create the wake trigger job")

		By("Waiting for the deployment to come back above zero")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, decodeDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(*dep.Spec.Replicas).To(BeNumerically(">=", int32(1)),
				"a parked model with requests queueing at the EPP must be woken; if it is not, "+
					"check wva_model_scaling_blocked for no-wake-signal, which means the EPP "+
					"exports no flow-control queue for it")
		}, wakeBudget, 5*time.Second).Should(Succeed())
	})

	It("does not immediately undo the wake", func() {
		// The freshly woken model has served nothing, so the request counter that
		// parks it still reads idle — without the activation-retention hold the
		// enforcer would park it again on its next pass and the two components would
		// fight in a loop. Watching it STAY up for the retention window tests the
		// guarantee rather than racing a transient metric.
		Consistently(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, decodeDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(*dep.Spec.Replicas).To(BeNumerically(">=", int32(1)),
				"a just-woken model was re-parked while activation-retention should have held it")
		}, retentionDuration, 5*time.Second).Should(Succeed())
	})
})

// roundTripProm returns a Prometheus client, skipping when the transport is
// missing rather than failing: utils.DefaultPrometheusURL is https://localhost:9090,
// a port-forward no suite creates, so on a host without one every query errors.
func roundTripProm() *utils.PrometheusClient {
	GinkgoHelper()
	pc := promClientForCheck()
	if pc == nil {
		Skip("no Prometheus client could be built")
	}
	if _, err := pc.QueryWithRetry(ctx, "vector(1)"); err != nil {
		Skip("Prometheus is not reachable from the test host: " + err.Error() +
			" (port-forward svc/kube-prometheus-stack-prometheus 9090:9090 for the metric assertions)")
	}
	return pc
}

// discoverInferenceGateway finds the Service that fronts the EPP. Requests must
// go through it to queue; sent straight at a model's own Service they are simply
// served, or refused, and nothing ever wakes.
func discoverInferenceGateway() string {
	GinkgoHelper()
	serviceList, err := k8sClient.CoreV1().Services(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	for _, svc := range serviceList.Items {
		if strings.Contains(svc.Name, "inference-gateway") {
			return svc.Name
		}
	}
	// The GAIE standalone chart embeds Envoy in the EPP pod and exposes port 80 on
	// the EPP Service itself, so no separate gateway Service exists.
	return cfg.EPPServiceName
}
