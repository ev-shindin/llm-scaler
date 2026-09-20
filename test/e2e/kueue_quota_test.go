package e2e

import (
	"context"
	"fmt"
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// A quota entry bounded by Kueue, against a real API server: the reader lists
// the Kueue kinds with the controller's own RBAC and whichever API version the
// cluster serves, the SMALLER of the static cap and the Kueue nominal quota
// becomes the entry the limiter enforces, and a Kueue change is picked up live
// within refreshInterval.
//
// Asserted through the limiter's own report of its effective entry -- the one
// `quota entry bounded by external caps` line it logs whenever the Kueue
// snapshot changes -- because that is the seam this feature adds. What the
// optimizer and the warm pool then do with an effective entry is the same code
// whichever source the figures came from, and is covered where those live
// (warm_pool_quota_test.go for the pool's cap). The static entry allows three;
// Kueue grants one; the entry must read one. Then the LocalQueue goes away, the
// namespace is no longer Kueue-governed, and the entry must read three again.
//
// The Kueue ResourceFlavor names the accelerator by its full product label
// ("NVIDIA-KUEUEACCEL-80GB") and the static entry by the short name
// ("KUEUEACCEL"); the two meet only by accelerator identity -- which is the
// production case: NFD labels on one side, an operator's short names on the
// other.
//
// Only the three Kueue CRDs are needed (deploy/ci-pr-checks/install-kueue-crds.sh
// installs them on kind); the reader lists objects and never talks to Kueue's
// controller. On kind their absence is a setup defect and the spec FAILS — a
// skip there would read as coverage that does not exist. On any other cluster
// (the OpenShift CI runs the same `full` filter and installs nothing) the spec
// runs when the kinds are served and skips, saying so, when they are not:
// whether a shared cluster carries Kueue is not this suite's to decide.
var _ = Describe("Quota limiter - bounded by Kueue", Label("full"), Label("kueue-quota"), Ordered, Serial, func() {
	const (
		grantName  = "e2e-kueue-quota"
		heldAccel  = "KUEUEACCEL"
		product    = "NVIDIA-" + heldAccel + "-80GB"
		staticGPUs = 3
		kueueGPUs  = 1
		refresh    = "5s"
		settle     = 3 * time.Minute
		// The suite restarts the controller before the first spec; nothing
		// older than this window is ours.
		sinceRestart = int64(900)
	)

	var (
		ctx        context.Context
		controller fixtures.ControllerDeployment
		grant      fixtures.KueueQuota
	)

	controllerLog := func() (string, error) {
		_, logs, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, controller.Namespace,
			"control-plane=controller-manager", "", sinceRestart)
		return logs, err
	}

	// effectiveAt matches the limiter's report of the namespace's effective cap
	// for heldAccel, as the console encoder prints the map.
	effectiveAt := func(gpus int) string {
		return regexp.QuoteMeta(fmt.Sprintf(`"effectiveNamespaceQuotas": {"%s":{"%s":%d}}`,
			cfg.LLMDNamespace, heldAccel, gpus))
	}

	BeforeAll(func() {
		ctx = context.Background()
		controller = fixtures.ControllerDeployment{
			Namespace: cfg.WVANamespace,
			Name:      "wva-controller-manager",
		}

		if !fixtures.KueueInstalled(crClient) {
			if cfg.Environment == envKindEmulator {
				Fail("the Kueue CRDs (ClusterQueue, LocalQueue, ResourceFlavor) are not installed on this kind cluster; " +
					"test-e2e-full-with-setup installs them via deploy/ci-pr-checks/install-kueue-crds.sh")
			}
			Skip("Kueue CRDs (ClusterQueue, LocalQueue, ResourceFlavor) are not served on this cluster; " +
				"the Kueue-bounded quota spec needs them and does not install them outside kind")
		}

		By("Granting the namespace one GPU through Kueue")
		grant = fixtures.KueueQuota{
			Name:         grantName,
			Namespace:    cfg.LLMDNamespace,
			ProductLabel: "nvidia.com/gpu.product",
			Product:      product,
			Resource:     "nvidia.com/gpu",
			GPUs:         kueueGPUs,
		}
		Expect(fixtures.CreateKueueQuota(ctx, crClient, grant)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteKueueQuota(context.Background(), crClient, grant) })

		By("Declaring a static quota of three, bounded by Kueue")
		restoreConfig, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.WVANamespace,
			scalingPolicyConfigMapName(), cfg.LLMDNamespace, heldAccel, staticGPUs,
			fixtures.WithKueue(refresh))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restoreConfig(context.Background()) })
	})

	It("reads the Kueue grant and enforces the smaller cap", func() {
		Eventually(func(g Gomega) {
			logs, err := controllerLog()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).NotTo(ContainSubstring("external quota source unreadable"),
				"the reader must be able to list the Kueue objects -- a Forbidden here is an RBAC defect")
			g.Expect(logs).To(MatchRegexp(effectiveAt(kueueGPUs)),
				"the effective entry must carry Kueue's %d, not the static %d", kueueGPUs, staticGPUs)
		}, settle, 5*time.Second).Should(Succeed())
	})

	It("returns to the static cap once the namespace leaves Kueue", func() {
		By("Deleting the LocalQueue, so Kueue no longer governs the namespace")
		Expect(fixtures.DeleteKueueLocalQueue(ctx, crClient, grant)).To(Succeed())

		// The reader re-reads within refreshInterval and the limiter logs the
		// change on its next pass: the static three is all that remains.
		Eventually(func(g Gomega) {
			logs, err := controllerLog()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).To(MatchRegexp(effectiveAt(staticGPUs)),
				"with Kueue's grant gone the entry must read the static %d", staticGPUs)
		}, settle, 5*time.Second).Should(Succeed())
	})
})
