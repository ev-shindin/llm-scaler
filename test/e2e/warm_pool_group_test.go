package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// What a POOL WHOSE UNIT SPANS PODS reports about itself.
//
// An engine with more ranks than one machine holds runs as a LeaderWorkerSet:
// the leader serves the API, the workers only hold ranks. The controller has to
// read that as ONE lendable unit, and until now unit tests were the only thing
// that said so -- every other warm pool spec builds a Deployment, where a Pod
// and a unit are the same thing and the distinction cannot be wrong.
//
// Two behaviours are observable from outside, and both are asserted here:
//
//   - a worker is not a member. It runs no supervisor, so a worker counted as
//     lendable is a unit whose engine does not exist -- and a worker LABELLED
//     into an InferencePool takes traffic nothing answers.
//   - a group short a Ready Pod is not a degraded engine, it is no engine. The
//     ranks cannot form, so nothing in it can be woken or lent.
//
// The third group behaviour -- that the unit's device count is the GROUP's and
// not the leader's -- is deliberately NOT asserted here. The pool's state line
// carries no GPU count, so any assertion this spec could make about it would
// pass whatever the code did. It is covered where it is actually visible, in
// TestAGroupsCapacityCountsEveryPodsDevices.
var _ = Describe("Warm pool - a unit that spans Pods", Label("full"), Label("warmpool-group"), Ordered, Serial, func() {
	const (
		groupPool  = "e2e-pool-group"
		groupSize  = 2
		gpusPerPod = 1
		settle     = 3 * time.Minute
		// Bounds every log read to this spec's controller, as the single-Pod
		// specs do: the suite reuses one controller, and an unbounded read
		// would accept a line written under a previous configuration.
		sinceControllerRestart = int64(900)
	)

	var (
		ctx        context.Context
		controller fixtures.ControllerDeployment
		spec       fixtures.WarmPoolSpec
	)

	controllerLog := func() (string, error) {
		_, logs, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, controller.Namespace,
			"control-plane=controller-manager", "", sinceControllerRestart)
		return logs, err
	}

	poolReported := func(match string) func(Gomega) {
		return func(g Gomega) {
			logs, err := controllerLog()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).To(MatchRegexp(`"pool": "`+groupPool+`", "state": "`+match),
				"pool %s never reported a state matching %q", groupPool, match)
		}
	}

	BeforeAll(func() {
		ctx = context.Background()
		controller = fixtures.ControllerDeployment{
			Namespace: cfg.WVANamespace,
			Name:      "wva-controller-manager",
		}

		// A cluster without the CRD cannot build the object under test, and a
		// spec that quietly passed there would be reporting on nothing.
		if err := crClient.List(ctx, &lwsv1.LeaderWorkerSetList{}); err != nil {
			Skip("LeaderWorkerSet is not served by this cluster, so a unit that spans Pods cannot be built: " + err.Error())
		}

		spec = fixtures.WarmPoolSpec{
			Name:       groupPool,
			Namespace:  cfg.LLMDNamespace,
			ProxyImage: cfg.WarmPoolProxyImage,
			PoolName:   groupPool,
			GroupSize:  groupSize,
			GPUs:       gpusPerPod,
		}
		Expect(fixtures.CreateWarmPoolGroup(ctx, k8sClient, crClient, spec)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteWarmPoolGroup(context.Background(), k8sClient, crClient, spec)
		})

		// Every Pod of the group Ready first. Without this the assertions below
		// cannot tell a group correctly held back for being short a rank from
		// one that simply never formed -- they report identically.
		Eventually(func(g Gomega) {
			ready, created := readyGroupPods(ctx, groupPool)
			g.Expect(ready).To(Equal(groupSize),
				"the group never became %d Ready Pods (%d Ready of %d created)",
				groupSize, ready, created)
		}, settle, 5*time.Second).Should(Succeed())
	})

	It("counts the group as one lendable unit, not one per Pod", func() {
		Eventually(poolReported(`pods=1 `), settle, 5*time.Second).Should(Succeed())

		// And never as two. A worker admitted as a member would read as a
		// second Pod, which no count-only assertion can distinguish from a
		// genuine second unit -- so it is checked directly.
		logs, err := controllerLog()
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).NotTo(MatchRegexp(`"pool": "`+groupPool+`", "state": "pods=2 `),
			"a worker Pod was counted as a lendable unit")
	})

	It("stops offering the unit when the group loses a rank", func() {
		pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "warm-pool-fixture=" + groupPool,
		})
		Expect(err).NotTo(HaveOccurred())

		// A WORKER, not the leader. Deleting the leader takes the supervisor
		// with it, and the pool would go quiet for a reason that has nothing to
		// do with group readiness -- which is the only thing this asserts.
		var worker string
		for i := range pods.Items {
			if pods.Items[i].Labels[lwsv1.WorkerIndexLabelKey] != "0" {
				worker = pods.Items[i].Name
			}
		}
		Expect(worker).NotTo(BeEmpty(), "the group has no worker Pod to remove")

		By(fmt.Sprintf("removing worker %s, which breaks the rank group", worker))
		Expect(k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Delete(
			ctx, worker, metav1.DeleteOptions{})).To(Succeed())

		// ALL OR NOTHING. pods=0 is the pool saying it holds nothing LENDABLE;
		// the leader is still running, and its engine is still there. Offering
		// it would be offering an engine whose ranks cannot form.
		Eventually(poolReported(`pods=0 `), settle, 5*time.Second).Should(Succeed())
	})
})

// readyGroupPods counts the Ready and the created Pods of a fixture group.
func readyGroupPods(ctx context.Context, fixtureName string) (ready, created int) {
	pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "warm-pool-fixture=" + fixtureName,
	})
	if err != nil {
		return 0, 0
	}
	for i := range pods.Items {
		for _, c := range pods.Items[i].Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready++
			}
		}
	}
	return ready, len(pods.Items)
}
