package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Supply must never describe a fleet larger than the scale target is committed
// to. When it does, the optimizer is credited with spare capacity that an
// in-flight scale-down has already claimed and removes the same replicas twice,
// which is how a variant under sustained load lands on one replica. See
// steadystate.clampReplicaCountToScaleTarget.
//
// Reproducing that from a real scale-down would mean racing a termination
// window: a Deployment's status.replicas drops the moment a pod is marked for
// deletion, while the pod keeps serving and keeps being scraped until its grace
// period expires. The race is the bug's natural habitat and a bad test.
//
// This stages the same INPUT without the timing: an extra serving replica that
// no ReplicaSet owns. The collector still attributes it to the variant — it
// resolves a pod's variant by walking ownerReferences up to a Deployment, and
// this pod carries one — so it reports real capacity. But Status.Replicas is
// summed from the ReplicaSets' pod counts, and none of them owns it, so the
// scale target does not count it. Measured supply therefore exceeds the
// committed count, which is exactly the state a condemned-but-still-scraping
// replica produces, held still. See createUnownedReplica for why the owner has
// to be the Deployment and nothing else.
const concededFakeMetricsJSON = `{"kv-cache-usage":0.3,"running-requests":1,"waiting-requests":0}`

// Production-shaped thresholds, because the arithmetic this guards is only
// wrong in a particular band and the run that exposed it used these.
//
// With kv-cache-usage 0.3 and a two-replica target, a third reporting replica
// takes spare capacity from 2×P − 3×0.3×P/0.7 = 0.71×P (no replica to give up)
// to 3×P − 3×0.3×P/0.7 = 1.71×P — one whole replica of slack, on top of the one
// the target has already conceded. Unclamped that recommends 1; clamped it holds
// at 2.
const (
	concededKvCacheThreshold     = 0.80
	concededQueueLengthThreshold = 50
	concededScaleUpThreshold     = 0.85
	concededScaleDownBoundary    = 0.70
)

var _ = Describe("Scale-down with supply beyond the scale target", Label("full"), Ordered, func() {
	const (
		poolName              = "conceded-pool"
		modelSvcName          = "conceded-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		scalerBaseName        = "conceded"
		extraPodName          = modelSvcName + "-unowned-replica"
		targetReplicas        = 2
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmKey           string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		variantName     string
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"It uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		modelID = cfg.ModelID
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.WVANamespace
		cmKey = defaultConfigKey
		variantName = scalerBaseName + "-so"

		By("Snapshotting existing saturation ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		By("Creating model service + service + ServiceMonitor")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", concededFakeMetricsJSON})).To(Succeed())
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment,
		)).To(Succeed())

		By("Installing saturation config with production-shaped thresholds")
		cfgYAML := buildSaturationConfigYAMLWithThresholds(
			"saturation",
			concededKvCacheThreshold, concededQueueLengthThreshold,
			concededScaleUpThreshold, concededScaleDownBoundary,
		)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, cfgYAML)).To(Succeed())

		By("Registering the deployment with WVA via an annotated ScaledObject")
		// minReplicaCount is 1 on purpose: the assertion is that the fleet does
		// NOT fall to it. A floor of 2 would pass whether or not the bug is fixed.
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(30))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName) })
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		if cmExistedBefore && cmOriginal != nil {
			propagation := metav1.DeletePropagationBackground
			if err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete saturation configmap %s before restore: %v\n", cmName, err)
			}
			toCreate := saturationConfigMapForRecreate(cmOriginal)
			_, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				GinkgoWriter.Printf("Warning: failed to restore saturation configmap %s: %v\n", cmName, err)
			}
		}
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
	})

	It("holds the fleet at its target when an unowned replica also reports capacity", func() {
		By("Bringing the deployment to its target replica count")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		scaleDeployment(cfg.LLMDNamespace, modelDecodeDeployment, targetReplicas)
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically("==", targetReplicas))
			// TOTAL replicas as well, not just the ready ones. Status.Replicas
			// counts Pods that are still TERMINATING, and the guard below reads
			// that same field: scaling down from a larger fleet reaches
			// ReadyReplicas==2 while the third Pod is still going away, so the
			// guard sees 3 and reports adoption -- a staging failure for a
			// condition that is merely mid-scale-down.
			//
			// Invisible when this spec runs alone, because the Deployment starts
			// at one replica and there is nothing to terminate. It only appears
			// after a spec that left the fleet larger, which is why it failed in
			// the full suite and passed on its own.
			g.Expect(dep.Status.Replicas).To(BeNumerically("==", targetReplicas),
				"a Pod from an earlier scale-down is still terminating, and it counts here")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Proving WVA is actually driving this ScaledObject before relying on a negative")
		// Everything below is a Consistently on a value NOT changing. That holds
		// just as well when nothing is running: a miswired trigger, an unreachable
		// controller or a wrong modelID would leave spec.replicas parked at the 2
		// set above and the spec would pass while testing nothing. KEDA only
		// populates the HPA's external CurrentMetrics after it has read the metric
		// from WVA, so this pins the pipeline as live first.
		Eventually(func(g Gomega) {
			expectWVADesiredReplicasConsumed(g, cfg.LLMDNamespace, modelDecodeDeployment)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Adding a serving replica the Deployment does not own")
		createUnownedReplica(cfg.LLMDNamespace, modelDecodeDeployment, extraPodName)
		DeferCleanup(func() {
			propagation := metav1.DeletePropagationBackground
			_ = k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Delete(ctx, extraPodName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			})
		})
		Eventually(func(g Gomega) {
			pod, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(ctx, extraPodName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Confirming the Deployment still reports only the replicas it owns")
		// If the ReplicaSet adopted the extra pod, the premise is gone and the
		// assertion below would pass for the wrong reason.
		//
		// Bounded on ONE side only. Adoption is the failure this guard exists for
		// and it shows up as a count ABOVE the target. A count BELOW it is the
		// regression itself, and that belongs to the assertion after this one:
		// an equality check here would fire first and report a staging problem
		// for what is actually the bug under test.
		Consistently(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.Replicas).To(BeNumerically("<=", targetReplicas),
				"the ReplicaSet adopted the extra pod, so this test is not staging the condition it claims")
		}, 30*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Asserting the extra replica's capacity is never treated as removable")
		// The regression: supply over three reporting replicas yields a full
		// replica of spare on top of the one the target already conceded, and the
		// recommendation drops to minReplicaCount.
		Consistently(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(*dep.Spec.Replicas).To(BeNumerically(">=", targetReplicas),
				"WVA scaled below the target while an unowned replica was reporting: its capacity was counted as spare")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})
})

// scaleDeployment sets spec.replicas directly.
func scaleDeployment(namespace, name string, replicas int32) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		dep, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		dep.Spec.Replicas = &replicas
		_, err = k8sClient.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}, 60*time.Second, 2*time.Second).Should(Succeed())
}

// createUnownedReplica clones the Deployment's pod template into a standalone
// Pod that serves and is scraped like any other replica, but that the Deployment
// does not count.
//
// The owner reference has to satisfy three constraints at once, and getting it
// wrong makes this spec pass while proving nothing:
//
//   - The COLLECTOR must attribute the pod to the variant. It walks
//     ownerReferences for a supported ancestor and accepts only Deployment or
//     LeaderWorkerSet (collector/locator/walk.go); anything else resolves to no
//     scale target and the pod is dropped as unattributed. A reference to the
//     Service reads naturally and is exactly wrong: the pod then contributes no
//     capacity, the measured count never exceeds the target, the clamp never
//     fires, and the assertion below holds whether or not the fix is present.
//   - The REPLICASET must not adopt it. Adoption applies to matching pods with no
//     CONTROLLER owner; an adopted pod would be counted and then deleted to
//     satisfy the replica count, destroying the premise. Any controller owner
//     blocks it, including this one.
//   - The DEPLOYMENT must still not count it. Status.Replicas is summed from the
//     ReplicaSets' own pod counts, and no ReplicaSet owns this pod, so pointing
//     the reference at the Deployment does not inflate the count it is being
//     compared against.
//
// Owning it by the Deployment satisfies all three, and garbage-collects the pod
// with the fixture so a failed run cannot strand a simulator pod holding a GPU.
func createUnownedReplica(namespace, deploymentName, podName string) {
	GinkgoHelper()
	dep, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Namespace:   namespace,
			Labels:      dep.Spec.Template.Labels,
			Annotations: dep.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       dep.Name,
				UID:        dep.UID,
				Controller: &controller,
			}},
		},
		Spec: *dep.Spec.Template.Spec.DeepCopy(),
	}
	_, err = k8sClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred())
}
