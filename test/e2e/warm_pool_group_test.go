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

		// The warm pool is OFF in the e2e install, and the reconciler is simply
		// not started without it. Nothing about that is visible from outside:
		// KEDA still calls the external scaler, the ScaledObject still reports
		// Ready, the Pods still run -- and the controller reports no pool state
		// at all, which is exactly what a group being refused looks like.
		//
		// That cost several runs of chasing the group logic before noticing the
		// policy specs enable it explicitly and this one did not.
		By("Restarting the controller with the warm pool enabled")
		restoreCtl, err := fixtures.EnableWarmPool(ctx, k8sClient, controller, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = restoreCtl(context.Background())
			_ = fixtures.WaitForControllerReady(context.Background(), k8sClient, controller)
		})
		Eventually(func(g Gomega) {
			logs, lErr := controllerLog()
			g.Expect(lErr).NotTo(HaveOccurred())
			g.Expect(logs).To(ContainSubstring("warm pool enabled"),
				"the controller never reported the pool as enabled, so nothing reconciles it")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

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

		// A pool is DECLARED by a ScaledObject trigger, not by the existence of
		// its Pods: WVA learns about pools through KEDA calling it, so a pool
		// Deployment nobody declared holds accelerators the controller never
		// hears about. Without this the group formed correctly and the
		// controller reported nothing at all about it -- which reads exactly
		// like a group being refused, and would have been diagnosed as one.
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			groupPool, groupPool, groupPool, 1, 6, cfg.MonitoringNS,
			fixtures.WithWarmPoolTrigger(groupPool, nil),
			// The scale target is a LeaderWorkerSet, and the default is
			// Deployment. KEDA cannot resolve a target that is not there, so it
			// never calls WVA's external scaler -- and discovery is KEDA-driven,
			// so the pool is never learned about at all. The symptom is total
			// silence from the controller, which reads exactly like a group being
			// refused.
			fixtures.WithScaledObjectScaleTargetKind("LeaderWorkerSet"),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, groupPool)
		})

		// Wait for the group to FORM, which is not the same as every Pod being
		// Ready -- and asserting Ready here is the exact mistake this spec
		// exists to catch, made in the test instead of the controller.
		//
		// The leader carries the pool proxy, which reports Ready only when a
		// model is awake in the Pod. An idle pool leader is NotReady BY DESIGN,
		// and idle is the state the whole spec runs in. Waiting for it would
		// hang for the full timeout against a perfectly healthy pool.
		//
		// So: every Pod Running, and every WORKER Ready. That is what "the
		// ranks have joined" means from outside.
		Eventually(func(g Gomega) {
			running, workersReady, workers, created := groupFormation(ctx, groupPool)
			g.Expect(created).To(Equal(groupSize),
				"the LeaderWorkerSet created %d Pods, want %d", created, groupSize)
			g.Expect(running).To(Equal(groupSize),
				"%d of %d group Pods are Running", running, created)
			g.Expect(workersReady).To(Equal(workers),
				"%d of %d worker Pods are Ready, so a rank has not joined",
				workersReady, workers)
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

// groupFormation reports what a fixture group looks like from outside: how many
// of its Pods are Running, how many of its WORKERS are Ready, how many workers
// there are, and how many Pods exist at all.
//
// Workers and the leader are counted differently on purpose. A worker's
// readiness means its rank has joined; the leader's means a model is awake in
// the pool, which is a different question entirely and is false for every idle
// pool Pod.
func groupFormation(ctx context.Context, fixtureName string) (running, workersReady, workers, created int) {
	pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "warm-pool-fixture=" + fixtureName,
	})
	if err != nil {
		return 0, 0, 0, 0
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		created++
		if p.Status.Phase == "Running" {
			running++
		}
		if p.Labels[lwsv1.WorkerIndexLabelKey] == "0" {
			continue // the leader
		}
		workers++
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				workersReady++
			}
		}
	}
	return running, workersReady, workers, created
}
