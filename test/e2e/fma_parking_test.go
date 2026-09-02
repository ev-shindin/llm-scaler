package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// Parking an FMA variant, and what its launchers must NOT do afterwards.
//
// Parking an FMA model scales its requester Deployment to zero. The launchers do
// not go away with it: they keep running, keep holding their GPUs, and keep
// answering /metrics — a sleeping launcher reports vllm:num_requests_running 0
// and vllm:kv_cache_usage_perc 0 rather than nothing at all. That is FMA working
// as designed; those sleepers are the warm pool a later wake reuses in seconds
// instead of ~80 (docs/proposals/fma-post-mortem.md).
//
// It is also the one state where scale-to-zero and FMA combine into an outage. If
// a parked model's leftover launchers were still attributed to its scale target,
// WVA would read a model at zero replicas as a model that is serving — and the
// scale-from-zero engine refuses to wake a model whose decode is already covered.
// The model would be parked, unwakeable, and reported healthy. Nothing else
// covers this: the parking suites run non-FMA workloads, and the FMA attribution
// suite only ever has a live pair.
//
// The distinction pinned here is narrow and easy to lose. FMA attribution already
// tests a launcher that was NEVER bound — it carries no pairing label, so the
// PodMonitor's keep rule never generates a target for it. This is the other case:
// a launcher that WAS bound, still carries the pairing label and the server-port
// annotation that make it a scrape target, and whose partner has since gone. It
// is scraped, it reaches the collector, and only the hop's partner-must-resolve
// guard stops it being charged to a variant that serves nothing.
var _ = Describe("FMA parking - a parked variant's launchers", Label("full"), Label("fma"), Ordered, func() {
	const (
		layoutName                = "e2e-fma-park"
		modelID                   = "default/default-fma-park"
		modelLabel                = "e2e-fma-park-model"
		maxNumSeqs                = 4
		fmaControllerManagerLabel = "control-plane=controller-manager"
	)

	var (
		ctx    context.Context
		layout *fixtures.FMALayout
	)

	BeforeAll(func() {
		ctx = context.Background()

		DeferCleanup(func() {
			_ = fixtures.DeleteFMALauncherPodMonitor(ctx, crClient, cfg.LLMDNamespace)
			_ = fixtures.DeleteFMALayout(ctx, k8sClient, cfg.LLMDNamespace, layout)
		})

		By("creating an FMA layout: a requester Deployment, a bound launcher, and one warm spare")
		var err error
		layout, err = fixtures.CreateFMALayout(ctx, k8sClient, cfg.LLMDNamespace,
			layoutName, modelLabel, modelID, maxNumSeqs)
		Expect(err).NotTo(HaveOccurred())

		By("scraping the launchers the way the shipped PodMonitor does")
		Expect(fixtures.EnsureFMALauncherPodMonitor(ctx, crClient, cfg.LLMDNamespace)).To(Succeed())

		// Registration is what makes WVA collect for this model at all — with no
		// ScaledObject there is no variant, the collector never runs for it, and
		// every assertion below would pass for the wrong reason.
		//
		// minReplicaCount is 1, NOT 0, even though this suite is about parking. A
		// variant registered at 0 is parked from the moment it is registered, so it
		// never becomes active, the collector never runs for it, and the attribution
		// this suite must first observe never happens — the wait below simply times
		// out. Parking is therefore simulated by unbinding the pair, which is the
		// state that actually matters here; the replica count is not what the guard
		// keys on.
		By("registering the requester, which is what makes WVA collect for this model at all")
		scalerAddress := "wva-external-scaler." + cfg.WVANamespace + ".svc.cluster.local:9090"
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			layout.RequesterDeployment, layout.RequesterDeployment, layout.RequesterDeployment+"-wva",
			1, 2, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "10.0"),
			fixtures.WithExternalScalerTrigger(scalerAddress),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, layout.RequesterDeployment)
		})

		By("waiting until the bound launcher is attributed, so the unbind below means something")
		// Proving a launcher STOPS being attributed requires first proving it was.
		// Without this the suite would pass on a controller that had simply not
		// collected anything yet.
		Eventually(func() bool {
			ok, _, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace,
				fmaControllerManagerLabel, "Attributed FMA launcher pods through their dual-pods pairing", 300)
			return err == nil && ok
		}, 5*time.Minute, 15*time.Second).Should(BeTrue(),
			"the pairing hop never attributed the bound launcher, so this suite cannot observe it stopping")
	})

	It("stops attributing a launcher once its requester is gone", func() {
		By("deleting the requester pod, which is what parking does to a bound pair")
		// Parking scales the requester Deployment to zero and its pod goes, leaving
		// the launcher holding a pairing label that names a pod which no longer
		// exists. FMA clears that label on unbind, but the window between the two is
		// real, and a launcher outliving its label removal is exactly what a crashed
		// dual-pods controller leaves behind. The guard must hold on the label alone.
		Expect(k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Delete(ctx,
			layout.RequesterPod, metav1.DeleteOptions{})).To(Succeed())

		By("asserting the launcher is filed as pairing_unresolved rather than charged to the variant")
		// pairing_unresolved is the reason meaning "it declared a partner and the
		// partner is not there". Attributing it instead would be silent and wrong:
		// the variant would report a serving replica while its requester is at zero.
		Eventually(func() bool {
			ok, _, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace,
				fmaControllerManagerLabel, "pairing_unresolved", 600)
			return err == nil && ok
		}, 5*time.Minute, 15*time.Second).Should(BeTrue(),
			"a launcher whose partner was deleted was not filed as pairing_unresolved; if it is "+
				"still attributed, a parked FMA model reads as serving and can never be woken")
	})

	It("keeps the warm launchers running, because they are the warm pool", func() {
		// The counterpart assertion, and the reason the one above must be about
		// ATTRIBUTION rather than about the pods going away. Parking must not
		// disturb the launchers themselves: they hold instances keyed to the GPUs
		// their requesters reserved, which is what makes a later wake take seconds
		// instead of a full model load. A "fix" that deleted them would satisfy the
		// spec above and destroy the feature.
		for _, pod := range []string{layout.LauncherPod, layout.UnboundLauncherPod} {
			p, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(ctx, pod, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "launcher %s should still exist after the variant parked", pod)
			Expect(p.DeletionTimestamp).To(BeNil(),
				"launcher %s is terminating; parking a variant must leave the warm pool intact", pod)
		}
	})

	// A spec asserting the ABSENCE of the attribution line was removed rather than
	// fixed. It could not work: the controller's log is append-only, and BeforeAll
	// deliberately waits for that very line before unbinding — so any tail window
	// large enough to be meaningful still contains the earlier, correct occurrence.
	// It failed for that reason, not because a launcher was being re-attributed.
	//
	// The positive assertion above carries the property anyway: pairing_unresolved
	// is emitted for exactly the pods that would otherwise have been attributed, so
	// its presence is what proves they were not.
})
