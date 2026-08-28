package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// A named scaling policy is a reusable TIER — "interactive", "batch" — carrying
// thresholds but no model and no namespace. A workload joins one by naming it in
// its trigger metadata, which is the only thing binding the two: the tier does not
// know its members, and that is the whole point of it.
//
// This suite proves the name travels the entire path it has to travel to matter:
//
//	ScaledObject trigger metadata → KEDA → gRPC scalerMetadata → registry →
//	policy resolution (default entry ← tier) → optimizer → decision → KEDA → pods
//
// It is built as a matched pair of arcs at ONE operating point. The simulator
// reports a fixed kv-cache-usage of 0.3 for the whole run, so the replica count is
// a pure function of the thresholds in play — and the two threshold pairs used
// here are the ones the KEDA external-scaler suite and the V2 suite already pin to
// 2 replicas and 1 replica respectively at exactly this occupancy.
//
// The default entry alone therefore decides 1, and the tier decides 2.
//
// The suite builds the policy up one layer at a time, so every assertion is either
// "stays at 1" or "reaches 2" — never "comes back down". Scale-down is not a
// reliable signal here: at the ceiling, demand tracks supply, so utilization sits
// at 1.0 and spare capacity at 0, and the workload stays where it is however the
// policy resolves.
//
//	arc 1  tier absent            → holds at 1   (falls back to the default entry)
//	arc 2  tier + model override  → holds at 1   (override outranks the tier)
//	arc 3  override removed       → reaches 2    (the tier now decides)
//
// Arc 3 is what makes arcs 1 and 2 mean anything: it shows the flat line was the
// policy holding the workload down, not a workload that was never going to scale.
const tierFakeMetricsJSON = `{"kv-cache-usage":0.3,"running-requests":1,"waiting-requests":0}`

const (
	// Shared by both entries so the arcs differ ONLY by the threshold pair.
	tierKvCacheThreshold     = 0.80
	tierQueueLengthThreshold = 50

	// Default entry: the canonical ordering the V2 suite settles at 1 replica.
	tierDefaultScaleUpThreshold  = 0.95
	tierDefaultScaleDownBoundary = 0.85

	// The tier: the pair the external-scaler suite drives to 2 replicas.
	tierPolicyScaleUpThreshold  = 0.30
	tierPolicyScaleDownBoundary = 0.20

	// The tier's name, which is also its ConfigMap key. A tier carries no model
	// identity in its body — that is what tells it apart from an override.
	tierPolicyName = "interactive"

	// The override entry's key. Arbitrary by design — what binds it to the model
	// is the model_id/namespace in its body.
	tierOverrideKey = "policy-tier-model-override"

	// How long the override must hold the workload down before we accept that the
	// tier is not winning. The first arc drives this same workload from 1 to 2 in
	// well under a minute, so a window several times that is enough to distinguish
	// "suppressed" from "not yet scaled". It is a fixed window rather than
	// cfg.ScaleUpTimeout because that is 10 minutes — a Consistently of that length
	// would add ten idle minutes to every full run.
	tierOverrideHoldSeconds = 150
)

var _ = Describe("Named scaling policy tier", Label("full"), Ordered, func() {
	const (
		poolName              = "policy-tier-pool"
		modelSvcName          = "policy-tier-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"

		scalerBaseName = "policy-tier"
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		variantName     string
		scalerAddress   string
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"It uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		// A model of its own -- same remedy as sfzModelID, a different coupling.
		//
		// Every arc here asserts on a REPLICA COUNT that only follows from the
		// policy while capacity stays memory-bound: k1 is 6, demand is 2, so
		// utilization is 0.33 and the default entry (0.95) holds at 1 while the
		// tier (0.30) takes it to 2. A COMPUTE bound learned for the model
		// replaces that 6 -- seen at k2=2 in a full run -- and utilization becomes
		// 1.0, which is above every threshold in the file. The workload then
		// scales under the default entry, arc 1 fails, and the suite can no longer
		// tell apart the two policies it exists to distinguish.
		//
		// It cannot seed that history itself: computeK2 records only under its
		// Priority 1, which needs queueLen >= queueThreshold, and this suite pins
		// the threshold at 50 (tierQueueLengthThreshold) while its --fake-metrics
		// report no waiting requests at all. So every entry that can appear under
		// the shared key belongs to some other suite, and a key nothing else
		// writes to is the whole fix.
		modelID = suiteModelID("tier")
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.WVANamespace
		variantName = scalerBaseName + "-so"
		scalerAddress = "wva-external-scaler." + cfg.WVANamespace + ".svc.cluster.local:9090"

		By("Snapshotting the scaling policy ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing scaling policy configmap")
		}

		By("Installing ONLY the default entry, which holds at 1 replica")
		// Written before the workload registers, so the first decision the engine
		// makes for it already has the tier to resolve against.
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, defaultConfigKey,
			buildSaturationConfigYAMLWithThresholds(
				"saturation", tierKvCacheThreshold, tierQueueLengthThreshold,
				tierDefaultScaleUpThreshold, tierDefaultScaleDownBoundary,
			))).To(Succeed())
		// The tier is deliberately NOT created here. The workload registers naming
		// it anyway, so the first arc observes the unresolvable-name fallback from a
		// standing start rather than by waiting for a scale-down.

		By("Creating the model service with --fake-metrics so the operating point is fixed")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", tierFakeMetricsJSON})).To(Succeed())
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment,
		)).To(Succeed())

		By("Waiting for the policy-tier model deployment to be ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Registering the workload with WVA, naming the " + tierPolicyName + " tier in its trigger metadata")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"),
			fixtures.WithScalingPolicy(tierPolicyName),
			fixtures.WithExternalScalerTrigger(scalerAddress),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(30))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName) })
	})

	AfterAll(func() {
		By("Restoring the scaling policy ConfigMap")
		if cmExistedBefore && cmOriginal != nil {
			propagation := metav1.DeletePropagationBackground
			if err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete scaling policy configmap %s before restore: %v\n", cmName, err)
			}
			toCreate := saturationConfigMapForRecreate(cmOriginal)
			if _, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: failed to recreate scaling policy configmap %s: %v\n", cmName, err)
			}
		} else {
			_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{})
		}

		By("Cleaning up policy-tier resources")
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	// The three arcs below assert only SUPPRESSION or SCALE-UP, never a
	// scale-down, and that is the whole design of this suite.
	//
	// An earlier ordering drove the workload up with the tier and then waited for
	// it to come back down. It failed intermittently while the feature worked
	// perfectly: once a workload is at its ceiling, demand tracks supply, so
	// utilization sits at 1.0, spare capacity at 0, and scale-down stays blocked
	// for as long as that holds. The assertions were timing out on a property of
	// the capacity model and reporting it as a policy-resolution failure.
	//
	// Read in order, the arcs still prove the full precedence chain:
	// default entry < named tier < per-model override.

	// Arc 1. The workload names a tier that does not exist. An unresolvable name
	// must fall back to the default entry rather than fail — refusing to scale a
	// workload because its policy name went missing would turn a config edit into
	// an outage.
	It("falls back to the default entry when the named tier does not exist", func() {
		By("Holding at 1 replica: the default entry's 0.95 decides 1 at this occupancy")
		Consistently(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically("<=", 1),
				"the workload names a tier with no entry in the ConfigMap; that must resolve to "+
					"the default entry, not fail and not scale")
		}, tierOverrideHoldSeconds*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// Arc 2. The tier now exists AND a per-model override exists. The tier alone
	// would decide 2; the override decides 1. Staying at 1 can therefore only mean
	// the override outranks the tier the workload names.
	//
	// The override stays innermost so a fleet can adopt tiers model by model: a
	// model that already has one keeps it when its workload joins a tier.
	It("lets a per-model override win over the tier the workload names", func() {
		By("Adding the " + tierPolicyName + " tier, which alone would decide 2 replicas")
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, tierPolicyName,
			buildSaturationConfigYAMLWithThresholds(
				"saturation", tierKvCacheThreshold, tierQueueLengthThreshold,
				tierPolicyScaleUpThreshold, tierPolicyScaleDownBoundary,
			))).To(Succeed())

		By("Adding a per-model override carrying the no-scale threshold pair")
		// Same numbers as the default entry, so the ONLY question this asks is which
		// layer wins. The entry names the model in its BODY; its key is arbitrary and
		// has to be, because a ConfigMap data key admits only [-._a-zA-Z0-9] and the
		// model ID here contains a slash.
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, tierOverrideKey,
			buildSaturationConfigYAMLWithModel(
				tierKvCacheThreshold, tierQueueLengthThreshold,
				tierDefaultScaleUpThreshold, tierDefaultScaleDownBoundary,
				modelID, cfg.LLMDNamespace,
			))).To(Succeed())

		By("Asserting the override holds the workload at 1 while the tier alone would take it to 2")
		Consistently(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically("<=", 1),
				"a per-model override is the innermost layer; the workload still names the "+
					"tier, and the tier must not win over settings bound to this model")
		}, tierOverrideHoldSeconds*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// Arc 3. Remove the override and the tier takes effect. This closes the loop:
	// it proves the previous arc's flat line was the override holding the workload
	// down, and not simply a workload that was never going to scale.
	It("scales on the tier's thresholds once the override is removed", func() {
		By("Deleting the per-model override, leaving the tier the workload names")
		Expect(deleteSaturationConfigEntry(ctx, cmNamespace, cmName, tierOverrideKey)).To(Succeed())

		By("Asserting KEDA actuates scale-up to >= 2 replicas on the tier's scaleUpThreshold")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
				"the "+tierPolicyName+" tier sets scaleUpThreshold=0.30, which decides 2 replicas at "+
					"this occupancy; staying at 1 means the tier never reached the optimizer")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})
})
