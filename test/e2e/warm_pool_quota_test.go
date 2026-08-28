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

// What a warm pool COSTS the namespace that holds it.
//
// A pool Pod is not a variant, so it is absent from the population the
// saturation engine optimizes -- and the figure a quota draws against is summed
// from exactly that population. Left alone, a namespace with a 4-GPU quota and a
// 3-GPU pool placed four more replicas and consumed seven.
//
// Two behaviours, and they are separate:
//
//   - the pool's GPUs are CHARGED, so the allowance the optimizer spends is the
//     one actually left after the pool took its share;
//   - the pool does not ASK for Pods the allowance cannot cover, because those
//     Pods are created and then sit Pending, or are refused, while the pool
//     reports itself short forever with nothing able to fill it.
//
// Asserted through the controller's own log rather than a metric, because the
// question is what the CONTROLLER concluded. A metric would show the outcome
// without saying whether the cap was the reason.
var _ = Describe("Warm pool - what it costs its namespace", Label("full"), Label("warmpool-quota"), Ordered, Serial, func() {
	const (
		quotaPool = "e2e-pool-quota"
		// One GPU per Pod, and an allowance of one. The pool's reserve then wants
		// a second Pod it cannot have, which is the condition under test.
		poolGPUs     = 1
		quotaGPUs    = 1
		heldAccel    = "E2E-ACCEL-QUOTA"
		settle       = 3 * time.Minute
		sinceRestart = int64(900)
	)

	var (
		ctx           context.Context
		controller    fixtures.ControllerDeployment
		spec          fixtures.WarmPoolSpec
		poolNode      string
		restoreConfig func(context.Context) error
	)

	controllerLog := func() (string, error) {
		_, logs, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, controller.Namespace,
			"control-plane=controller-manager", "", sinceRestart)
		return logs, err
	}

	BeforeAll(func() {
		ctx = context.Background()
		controller = fixtures.ControllerDeployment{
			Namespace: cfg.WVANamespace,
			Name:      "wva-controller-manager",
		}

		By("Pinning the pool to a node and giving that node an accelerator nothing else wants")
		nodes, err := fixtures.SchedulableNodes(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes).NotTo(BeEmpty())
		poolNode = nodes[0].Name
		restoreLabel, err := fixtures.LabelNodeAccelerator(ctx, k8sClient, poolNode, heldAccel)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restoreLabel(context.Background()) })

		By("Declaring a namespace quota smaller than the pool would like to be")
		restoreConfig, err = fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.WVANamespace,
			cfg.LLMDNamespace, heldAccel, quotaGPUs)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restoreConfig(context.Background()) })

		By("Restarting the controller with the warm pool enabled")
		restoreCtl, err := fixtures.EnableWarmPool(ctx, k8sClient, controller, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = restoreCtl(context.Background())
			_ = fixtures.WaitForControllerReady(context.Background(), k8sClient, controller)
		})

		spec = fixtures.WarmPoolSpec{
			Name:       quotaPool,
			Namespace:  cfg.LLMDNamespace,
			ProxyImage: cfg.WarmPoolProxyImage,
			PoolName:   quotaPool,
			NodeName:   poolNode,
			GPUs:       poolGPUs,
		}
		Expect(fixtures.CreateWarmPool(ctx, k8sClient, spec)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteWarmPool(context.Background(), k8sClient, spec) })

		// Reserve 1 with one Pod: the pool is short the moment it exists, so it
		// wants to grow and the quota is what stops it.
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			quotaPool, quotaPool, quotaPool, 1, 6, cfg.MonitoringNS,
			fixtures.WithWarmPoolTrigger(quotaPool, map[string]string{
				"warmPoolSleepMinSize": "2",
			}),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, quotaPool)
		})

		By("Waiting for the pool Pod, so the controller has something to charge")
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "warm-pool-fixture=" + quotaPool,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).NotTo(BeEmpty())
		}, settle, 5*time.Second).Should(Succeed())
	})

	It("stops the pool asking for Pods the namespace cannot afford", func() {
		// The pool wants two Pods (reserve 2) and the namespace may hold one. It
		// must stop at one and SAY so -- a pool that quietly asked for the second
		// would leave a Pod Pending forever, which reads to an operator exactly
		// like a cluster that is merely busy.
		Eventually(func(g Gomega) {
			logs, err := controllerLog()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).To(ContainSubstring("no GPU allowance left for it"),
				"the pool must refuse to ask beyond the namespace's quota, and say why")
		}, settle, 5*time.Second).Should(Succeed())
	})

	It("names the accelerator and the numbers, so the cap can be acted on", func() {
		// A cap with no figures is a dead end for whoever reads it: the operator
		// needs to know which allowance to raise, and by how much.
		logs, err := controllerLog()
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainSubstring(heldAccel),
			"the message must name the accelerator whose allowance is spent")
		Expect(logs).To(MatchRegexp(`"wanted": *\d+`), "and what the pool wanted")
		Expect(logs).To(MatchRegexp(`"cappedAt": *\d+`), "and what it settled for")
	})
})
