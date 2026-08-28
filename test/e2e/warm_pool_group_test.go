package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
		// A driver Pod is the only way to read a supervisor a NetworkPolicy may
		// be guarding: the API server's pod proxy arrives from the control plane
		// and matches no `from:` selector. Another spec in this suite applies the
		// shipped policy, and spec order is not fixed.
		groupDriver   = "e2e-pool-group-control"
		groupModelSvc = "e2e-group-ms"
		groupScaler   = "e2e-group"
		settle        = 3 * time.Minute
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

	// supervisorInstances reads what one Pod's supervisor was actually asked to
	// create. It is the only place the fan-out's output is visible: the
	// controller logs that it warmed a group, not the argv it sent each rank.
	supervisorInstances := func(podIP string) string {
		GinkgoHelper()
		result, err := fixtures.DriverCall(ctx, k8sClient, cfg.LLMDNamespace, groupDriver,
			"GET", fmt.Sprintf("http://%s:%d/v2/vllm/instances", podIP, fixtures.WarmPoolSupervisorPort), "")
		Expect(err).NotTo(HaveOccurred(), "reading instances from %s", podIP)
		Expect(result.Status).To(Equal(200), "supervisor at %s: %s", podIP, result.Body)
		return result.Body
	}

	// podIPsByGroup maps each Pod to its GROUP and its rank within that group.
	//
	// BOTH indices are needed, and leaving out the group was a real defect in this
	// spec: a pool of groups is not one group. Lending a unit takes it out of the
	// reserve, so the pool grows another -- and with two groups present, rank 0
	// names two different Pods. Keyed by worker index alone, the spec read the
	// leader of whichever group came back last in the listing, found an empty
	// supervisor, and reported that the leader was not rank 0.
	//
	// Within a group, the worker index IS the rank: the adapter orders members by
	// it and hands rank N to the Pod carrying N.
	podIPsByGroup := func() map[string]map[int]string {
		GinkgoHelper()
		pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "warm-pool-fixture=" + groupPool,
		})
		Expect(err).NotTo(HaveOccurred())
		out := map[string]map[int]string{}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Status.PodIP == "" || p.DeletionTimestamp != nil {
				continue
			}
			rank, convErr := strconv.Atoi(p.Labels[lwsv1.WorkerIndexLabelKey])
			Expect(convErr).NotTo(HaveOccurred(), "%s carries no readable worker index", p.Name)
			group := p.Labels[lwsv1.GroupIndexLabelKey]
			Expect(group).NotTo(BeEmpty(), "%s carries no group index", p.Name)
			if out[group] == nil {
				out[group] = map[int]string{}
			}
			out[group][rank] = p.Status.PodIP
		}
		return out
	}

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

	// THE FAN-OUT, actuated. Everything above is accounting -- how the controller
	// COUNTS a group. This is the only spec that makes it warm one, and the only
	// place the argv each rank receives is observable.
	//
	// Those flags are not cosmetic. A rank not told --headless starts its own API
	// server; one given the wrong --master-addr waits at a rendezvous nobody
	// attends; two ranks on different ports never meet. Each failure is a group
	// that holds its GPUs and never serves, and none is visible in the
	// controller's own logs, which say only that a group was warmed.
	//
	// The supervisor is emulated, so this does NOT prove a real vLLM accepts the
	// flags. It proves the orchestration around them -- which rank gets what,
	// from where -- and that is where the code is.
	It("gives every rank its own place in the engine", func() {
		By("Creating a driver to read the supervisors through")
		driver := fixtures.DriverSpec{
			Name: groupDriver, Namespace: cfg.LLMDNamespace, Labels: fixtures.ControllerDriverLabels(),
		}
		Expect(fixtures.CreateHTTPDriver(ctx, k8sClient, driver)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteHTTPDriver(context.Background(), k8sClient, driver) })

		By("Creating a model that declares it spans two Pods")
		// --nnodes is what makes this model a candidate for a group at all: the
		// fit check compares it against the group's actual size, and a model
		// without it is warmed into a single Pod like any other.
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, groupModelSvc, groupPool+"-pool", cfg.ModelID,
			cfg.UseSimulator, cfg.MaxNumSeqs, []string{"--nnodes", "2"})).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteModelService(context.Background(), k8sClient, cfg.LLMDNamespace, groupModelSvc)
		})
		// Deliberately NOT waiting for it to be Ready. The pool derives a warm
		// copy from the Deployment's POD SPEC, not from a running process, so a
		// model that has not started yet is still warmable -- and --nnodes is a
		// vLLM flag the simulator has no reason to accept, so requiring the pod
		// to run would make this spec depend on the emulator tolerating an
		// argument that is only here to be READ.

		By("Asking the pool to hold one copy of it")
		// By NAME, not by earning it: admission through popularity or repeated
		// misses depends on what else the suite is running.
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			groupScaler, groupModelSvc+"-decode", groupModelSvc+"-variant", 1, 2, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(cfg.ModelID, "10.0"),
			fixtures.WithWarmPoolSelection(groupPool, 1),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, groupScaler)
		})

		By("Waiting for the group to hold the model")
		// resident ONLY. The pool's size is not fixed while this runs and must not
		// be asserted: a variant whose own replicas cannot start is short, so the
		// group gets LENT, and a lent unit is out of the reserve -- which the pool
		// answers by growing a second one. Pinning pods=1 made this spec fail on
		// the pool doing exactly what it exists to do.
		Eventually(poolReported(`[^"]*resident=[1-9]`), settle, 5*time.Second).Should(Succeed())

		// The group that actually HOLDS the model. With the pool free to grow,
		// which group was warmed is not knowable in advance -- so it is found by
		// asking, rather than assumed to be the first.
		var byRank map[int]string
		var leaderIP string
		for _, members := range podIPsByGroup() {
			if members[0] == "" {
				continue
			}
			if strings.Contains(supervisorInstances(members[0]), "--node-rank") {
				byRank, leaderIP = members, members[0]
				break
			}
		}
		Expect(leaderIP).NotTo(BeEmpty(),
			"no group's leader holds a warmed instance, so the fan-out reached nothing")
		Expect(byRank).To(HaveLen(groupSize), "the warmed group is short a rank")

		By("Checking the leader was made rank 0, and serves")
		leader := supervisorInstances(leaderIP)
		Expect(leader).To(ContainSubstring("--node-rank 0"), "the leader is not rank 0: %s", leader)
		Expect(leader).To(ContainSubstring("--master-addr "+leaderIP),
			"the leader must point at itself for the rendezvous: %s", leader)
		Expect(leader).NotTo(ContainSubstring("--headless"),
			"a headless leader serves no API, so the group can never take traffic")

		By("Checking every worker was made a HEADLESS rank pointing at the leader")
		// Each Pod is checked against the rank ITS OWN index says it should hold,
		// so a fan-out that handed two Pods the same rank -- an engine that can
		// never form -- fails here rather than passing on a count.
		for rank := 1; rank < groupSize; rank++ {
			ip := byRank[rank]
			Expect(ip).NotTo(BeEmpty(), "the group has no rank %d", rank)
			worker := supervisorInstances(ip)
			Expect(worker).To(ContainSubstring(fmt.Sprintf("--node-rank %d", rank)),
				"the Pod carrying worker index %d was not made rank %d: %s", rank, rank, worker)
			Expect(worker).To(ContainSubstring("--master-addr "+leaderIP),
				"rank %d waits at a rendezvous the leader does not hold: %s", rank, worker)
			Expect(worker).To(ContainSubstring("--headless"),
				"rank %d started its own API server instead of joining: %s", rank, worker)
		}

		By("Checking every rank agrees on the port, or they never meet")
		// Read from the LEADER's own line rather than assumed, so the check still
		// holds if the port the adapter picks ever changes.
		port := portFlagIn(leader)
		Expect(port).NotTo(BeEmpty(), "no --port in the leader's options: %s", leader)
		for rank := 1; rank < groupSize; rank++ {
			Expect(supervisorInstances(byRank[rank])).To(ContainSubstring("--port "+port),
				"rank %d is on a different port, so it never joins the group", rank)
		}
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

// portFlagIn returns the value of --port in a supervisor's recorded options, or
// "" when there is none. Read rather than assumed: the port is chosen by the
// adapter from what the Pod already holds, so hard-coding one would make the
// agreement check pass on a coincidence.
func portFlagIn(body string) string {
	fields := strings.Fields(body)
	for i, f := range fields {
		if f == "--port" && i+1 < len(fields) {
			return strings.Trim(fields[i+1], "\",")
		}
	}
	return ""
}
