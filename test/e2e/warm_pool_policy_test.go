//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// What the pool DECIDES, as opposed to how a pool Pod behaves once something has
// decided for it -- which is all the rest of the warm-pool suite covers.
//
// These need a controller with the pool switched ON, and the suite deploys one
// with no warm-pool flags at all. That is why none of this could be tested
// before: not a run nobody had done, but a scenario nobody could build. It is
// also why these specs are Ordered and Serial -- they restart the shared
// controller and put its arguments back afterwards.
var _ = Describe("Warm pool - what the pool decides", Label("full"), Label("warmpool-policy"), Ordered, Serial, func() {
	const (
		// Deliberately not real product names. A test that asserted on
		// NVIDIA-A100 would pass on a cluster that happens to have one for
		// reasons unrelated to the rule under test.
		heldAccelerator = "E2E-ACCEL-HELD"
		wantAccelerator = "E2E-ACCEL-WANTED"
		poolA           = "e2e-pool-a"
		poolB           = "e2e-pool-b"
		// sinceControllerRestart bounds every log read to this spec's
		// controller. The suite reuses one controller across specs, and an
		// unbounded read would find a line the PREVIOUS configuration wrote and
		// accept it as evidence.
		sinceControllerRestart = int64(900)
	)

	var (
		ctx        context.Context
		controller fixtures.ControllerDeployment
		poolNode   string
	)

	controllerLog := func() (string, error) {
		_, logs, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, controller.Namespace,
			"control-plane=controller-manager", "", sinceControllerRestart)
		return logs, err
	}

	controllerSaid := func(fragments ...string) func(Gomega) {
		return func(g Gomega) {
			logs, err := controllerLog()
			g.Expect(err).NotTo(HaveOccurred())
			for _, want := range fragments {
				g.Expect(logs).To(ContainSubstring(want),
					fmt.Sprintf("controller log should mention %q", want))
			}
		}
	}

	// waitForPoolPod waits for a fixture pool's single Pod to be running with
	// both containers ready. The controller cannot observe a Pod it cannot
	// reach, so a spec that raced this would read "the pool is empty" and call
	// it a decision.
	waitForPoolPod := func(fixtureName string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "warm-pool-fixture=" + fixtureName,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).To(HaveLen(1))
			pod := pods.Items[0]
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
			g.Expect(pod.Status.PodIP).NotTo(BeEmpty())
			g.Expect(pod.Status.ContainerStatuses).NotTo(BeEmpty())
			for _, cs := range pod.Status.ContainerStatuses {
				// The supervisor must be ready or the controller's read fails;
				// the proxy is NOT required to be ready, because an idle pool
				// Pod is deliberately out of service.
				if cs.Name == "inference-server" {
					g.Expect(cs.Ready).To(BeTrue())
				}
			}
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())
	}

	// declarePool stands up a pool the way an operator now does: a Deployment for
	// the Pods, and a ScaledObject whose trigger DECLARES it. Without the
	// trigger there is no pool -- the Deployment just holds accelerators.
	declarePool := func(name, poolName string, tuning map[string]string) {
		GinkgoHelper()
		spec := fixtures.WarmPoolSpec{
			Name: name, Namespace: cfg.LLMDNamespace,
			ProxyImage: cfg.WarmPoolProxyImage, PoolName: poolName, NodeName: poolNode,
		}
		Expect(fixtures.CreateWarmPool(ctx, k8sClient, spec)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteWarmPool(context.Background(), k8sClient, spec) })
		waitForPoolPod(name)

		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, name, name, name,
			1, 6, cfg.MonitoringNS,
			fixtures.WithWarmPoolTrigger(poolName, tuning),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, name)
		})
	}

	BeforeAll(func() {
		ctx = context.Background()
		controller = fixtures.ControllerDeployment{Namespace: cfg.WVANamespace, Name: "wva-controller-manager"}

		By("Finding a worker node to pin pool Pods to")
		nodes, err := fixtures.SchedulableNodes(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes).NotTo(BeEmpty(), "need at least one schedulable worker node")
		poolNode = nodes[0].Name

		By("Giving that node an accelerator no workload asks for")
		restoreLabel, err := fixtures.LabelNodeAccelerator(ctx, k8sClient, poolNode, heldAccelerator)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restoreLabel(context.Background()) })

		By("Restarting the controller with the warm pool enabled")
		restoreCtl, err := fixtures.EnableWarmPool(ctx, k8sClient, controller, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = restoreCtl(context.Background())
			_ = fixtures.WaitForControllerReady(context.Background(), k8sClient, controller)
		})

		Eventually(controllerSaid("warm pool enabled"), 3*time.Minute, 5*time.Second).
			Should(Succeed(), "the controller should report the pool as enabled")
	})

	Context("when a model needs an accelerator the pool does not have", func() {
		const (
			modelSvc = "e2e-wp-mismatch"
			variant  = "e2e-wp-mismatch-decode"
			scaler   = "e2e-wp-mismatch-so"
			poolPod  = "e2e-wp-pool-mismatch"
		)

		BeforeAll(func() {
			By("Declaring a pool on the labelled node")
			// Reserve ZERO, deliberately. A pool keeps its reserve free and
			// admits only out of what is left, so the default reserve against a
			// small ceiling is the inert pool -- it can never admit anything, and
			// a decline that never happens is not evidence of anything. The
			// controller says so out loud, which is how this was found.
			declarePool(poolPod, "mismatch", map[string]string{
				registry.WarmPoolSleepMinSizeKey: "0",
			})

			By("Creating a model pinned to a DIFFERENT accelerator")
			Expect(fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvc,
				cfg.PoolName, cfg.ModelID, true, 4,
				fixtures.WithRole("decode"),
				fixtures.WithAcceleratorNodeSelector(wantAccelerator),
			)).To(Succeed())
			DeferCleanup(func() {
				_ = fixtures.DeleteModelService(context.Background(), k8sClient, cfg.LLMDNamespace, modelSvc)
			})

			By("Confirming the fixture really pinned it to another accelerator")
			// The spec's whole premise. Without this a mismatch that never
			// existed would read as a mismatch correctly ignored, and the
			// decline would appear to work while testing nothing.
			Eventually(func(g Gomega) {
				dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).
					Get(ctx, variant, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(dep.Spec.Template.Spec.NodeSelector).
					To(HaveKeyWithValue("nvidia.com/gpu.product", wantAccelerator))
			}, 60*time.Second, 3*time.Second).Should(Succeed())

			By("Registering it, which is what makes WVA aware of it at all")
			Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scaler, variant, variant,
				1, 10, cfg.MonitoringNS,
				fixtures.WithWVATriggerMetadata(cfg.ModelID, "10.0"),
			)).To(Succeed())
			DeferCleanup(func() {
				_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, scaler)
			})
		})

		It("reads the accelerator its Pods actually sit on", func() {
			// Neither side is configured: the pool's accelerator comes from the
			// NODE and the model's from its own pod template. That is what makes
			// this worth an e2e -- a unit test supplies both, and cannot show
			// that either one arrives.
			Eventually(controllerSaid(`"pool": "mismatch"`, "accelerator="+heldAccelerator),
				3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("does not warm a model it could never serve", func() {
			// Deliberately NOT a log match on the word "declined": a line saying
			// declined and a model loaded anyway are indistinguishable in a log.
			// `resident` counts the models actually HELD, so zero is the direct
			// statement that nothing was warmed. (It used to count memberships,
			// where an empty Pod contributed a placeholder and this read
			// resident=1 -- the assertion moved with the fix.)
			//
			// The pool has admission budget here (reserve 0), so this is the
			// accelerator rule refusing it, not the reserve.
			// FIRST wait for the pool to see the model at all. Discovery is
			// call-driven -- a variant exists for WVA only once KEDA has called
			// about its ScaledObject -- so asserting immediately tests the clock,
			// not the rule. Without this the spec reads variants=0 and reports
			// "the model was not warmed", which is true and meaningless.
			By("Waiting until the pool can see the model")
			Eventually(func(g Gomega) {
				logs, err := controllerLog()
				g.Expect(err).NotTo(HaveOccurred())
				last := lastPoolState(logs)
				g.Expect(last).NotTo(BeEmpty(), "the pool should be reporting its state")
				g.Expect(last).To(ContainSubstring("variants=1"),
					"the pool must have seen the model before this proves anything: "+last)
			}, 4*time.Minute, 5*time.Second).Should(Succeed())

			By("And then it stays unwarmed")
			Consistently(func(g Gomega) {
				logs, err := controllerLog()
				g.Expect(err).NotTo(HaveOccurred())
				last := lastPoolState(logs)
				g.Expect(last).To(ContainSubstring("variants=1"),
					"the model must remain visible throughout: "+last)
				g.Expect(last).To(ContainSubstring("resident=0"),
					"a model on the wrong accelerator must not be warmed: "+last)
			}, 60*time.Second, 15*time.Second).Should(Succeed())

			By("And the pool was genuinely able to admit, so the refusal is the accelerator")
			// Without this the spec passes just as well against a pool that
			// could not admit ANYTHING -- which is the state the first version
			// of it was actually in.
			logs, err := controllerLog()
			Expect(err).NotTo(HaveOccurred())
			Expect(logs).NotTo(ContainSubstring("can never admit a model"),
				"the pool must have had budget, or this proves nothing")
		})
	})

	Context("when a namespace holds two named pools", func() {
		BeforeAll(func() {
			for _, name := range []string{poolA, poolB} {
				declarePool(name, name, nil)
			}
		})

		It("reports each pool separately, holding only its own Pods", func() {
			// Before pools were named these Pods were one flat set under one
			// reserve -- a single number spanning accelerators that cannot
			// substitute for each other. Each pool seeing exactly its own one
			// Pod is what says the split is real rather than cosmetic.
			Eventually(func(g Gomega) {
				logs, err := controllerLog()
				g.Expect(err).NotTo(HaveOccurred())
				for _, name := range []string{poolA, poolB} {
					g.Expect(logs).To(MatchRegexp(`"pool": "`+name+`", "state": "pods=1 `),
						"pool %s should see exactly its own one Pod", name)
				}
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})
	})
})

// lastPoolState is the most recent "warm pool state" line in a controller log,
// or "" if there is none.
//
// The LAST one, because that line is deduplicated: the pool logs when its state
// CHANGES, so an earlier line describes a state it has since left. Matching the
// first would assert about history.
func lastPoolState(logs string) string {
	found := ""
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "warm pool state") {
			found = line
		}
	}
	return found
}
