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

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// THE WHOLE POINT OF THE POOL, decided by the controller rather than by hand.
//
// The other warm-pool specs MANUFACTURE the borrow: they PUT an instance, POST
// /wake_up and PUT /upstream themselves, then check what the controller made of
// the result. That proves the mechanism and not the decision -- the path from
// "this variant wants a replica it does not have" to "a warm Pod is serving it"
// has never run end to end.
//
// So this one creates DEMAND and touches nothing else. Fake metrics put a
// variant over its scale-up threshold, WVA decides it wants more replicas than
// are ready, and a pool allowed to hold that model is the fast way to cover the
// gap. Everything after that -- warming the model, waking it, pointing the
// proxy, labelling the Pod into the InferencePool -- is WVA's to do.
//
// The last assertion is a real completion, because every cheaper check has a way
// of passing while the thing an operator cares about is false: lent=1 is the
// controller SAYING it lent a Pod, and a Pod can be believed-lent, labelled and
// Ready while the engine behind the proxy is still asleep.
var _ = Describe("Warm pool - a borrow WVA decides on its own", Label("full"), Label("warmpool-borrow"), Ordered, Serial, func() {
	const (
		borrowPool   = "e2e-pool-borrow"
		modelSvcName = "e2e-borrow-ms"
		fixturePool  = "e2e-borrow-pool"
		scalerName   = "e2e-borrow"
		tenantDriver = "e2e-borrow-tenant"

		// Over the scale-up threshold below, so WVA wants a replica the variant
		// does not have. The same lever the external-scaler spec uses, for the
		// same reason: real load makes the demand depend on timing.
		borrowFakeMetrics = `{"kv-cache-usage":0.9,"running-requests":8,"waiting-requests":4}`
		scaleUpThreshold  = 0.30
		scaleDownBoundary = 0.20
		kvCacheThreshold  = 0.80
		queueThreshold    = 50

		// Generous because a borrow is several passes deep: a miss, then a model
		// admission, then the borrow itself.
		settle       = 5 * time.Minute
		sinceRestart = int64(900)
	)

	var (
		ctx        context.Context
		controller fixtures.ControllerDeployment
		poolSpec   fixtures.WarmPoolSpec
		asTenant   fixtures.DriverSpec
		servingSet string
		poolPod    string
		poolPodIP  string
		modelNode  string
	)

	controllerLog := func() (string, error) {
		_, logs, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, controller.Namespace,
			"control-plane=controller-manager", "", sinceRestart)
		return logs, err
	}

	// poolState returns the LAST "warm pool state" line for this pool -- the
	// controller's own summary: pods, free, resident, variants, lent.
	//
	// A helper rather than a regex per assertion, because the failures it
	// separates are otherwise indistinguishable, and "no borrow happened" names
	// none of them. A pool that never appears at all was never DECLARED: KEDA has
	// not called the scaler for its ScaledObject. variants=0 means it was
	// declared but sees no model to warm. resident=0 with variants=1 means the
	// model was seen and not admitted. Three different fixes.
	poolState := func() string {
		logs, err := controllerLog()
		if err != nil {
			return ""
		}
		last := ""
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, `"pool": "`+borrowPool+`"`) {
				last = line
			}
		}
		return last
	}

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("--fake-metrics is simulator-only, and this spec needs demand that does not depend on timing")
		}
		ctx = context.Background()
		controller = fixtures.ControllerDeployment{
			Namespace: cfg.WVANamespace,
			Name:      "wva-controller-manager",
		}
		servingSet = fixtures.ServingPoolNameForEPP(cfg.EPPServiceName)

		nodes, err := fixtures.SchedulableNodes(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes).NotTo(BeEmpty())
		modelNode = nodes[0].Name

		By("Restarting the controller with the warm pool enabled")
		restoreCtl, err := fixtures.EnableWarmPool(ctx, k8sClient, controller, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = restoreCtl(context.Background())
			_ = fixtures.WaitForControllerReady(context.Background(), k8sClient, controller)
		})

		By("Standing up the pool the model will be warmed into")
		poolSpec = fixtures.WarmPoolSpec{
			Name:       borrowPool,
			Namespace:  cfg.LLMDNamespace,
			ProxyImage: cfg.WarmPoolProxyImage,
			PoolName:   borrowPool,
		}
		Expect(fixtures.CreateWarmPool(ctx, k8sClient, poolSpec)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteWarmPool(context.Background(), k8sClient, poolSpec) })

		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			borrowPool, borrowPool, borrowPool, 1, 4, cfg.MonitoringNS,
			fixtures.WithWarmPoolTrigger(borrowPool, map[string]string{
				// Reserve ZERO, and this is not a detail. The admission budget is
				// free Pods minus the reserve, so a one-Pod pool with a reserve of
				// one has a budget of zero: the model is never warmed, the borrow
				// never happens, and the spec fails on a configuration choice
				// rather than on the behaviour it is named for.
				"warmPoolSleepMinSize": "0",
				// The default hold is two minutes, after which a bridge goes back
				// whether or not the scale-up behind it has landed. That timeout
				// has its own unit tests; here it would only be a clock racing the
				// assertions, so it is moved out of the way.
				"warmPoolMaxHold": "15m",
			}),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, borrowPool)
		})

		By("Waiting for the pool Pod, which is what the borrow will take")
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fixtures.WarmPoolNameLabel + "=" + borrowPool,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).To(HaveLen(1))
			g.Expect(pods.Items[0].Status.PodIP).NotTo(BeEmpty())
			poolPod = pods.Items[0].Name
			poolPodIP = pods.Items[0].Status.PodIP
		}, settle, 5*time.Second).Should(Succeed())

		By("Creating a driver Pod to send the tenant's request from")
		// The API server's pod proxy cannot stand in for a tenant here: it
		// arrives from the control plane, matches no `from:` selector in the
		// pool's NetworkPolicy, and its "cannot reach" 503 is indistinguishable
		// from the proxy's own "no model is awake" 503 -- which is precisely the
		// distinction the final assertion rests on.
		asTenant = fixtures.DriverSpec{
			Name: tenantDriver, Namespace: cfg.LLMDNamespace, Labels: fixtures.TenantDriverLabels(),
		}
		Expect(fixtures.CreateHTTPDriver(ctx, k8sClient, asTenant)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteHTTPDriver(context.Background(), k8sClient, asTenant) })

		By("Creating a model whose metrics say it is over its threshold")
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, fixturePool, cfg.ModelID,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", borrowFakeMetrics})).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteModelService(context.Background(), k8sClient, cfg.LLMDNamespace, modelSvcName)
		})

		By("Making those metrics reachable, without which there is no demand at all")
		// A Service and a ServiceMonitor, and they are load-bearing rather than
		// hygiene. WVA decides from PROMETHEUS, so a model nothing scrapes reports
		// nothing, WVA wants exactly the replicas it already has, and no shortfall
		// ever opens. The pool then sits warm and idle -- pods=1 resident=1 lent=0
		// -- which reads as a borrow that failed, when in fact no borrow was ever
		// called for. That is how this spec failed the first time it ran.
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelSvcName+"-decode", 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelSvcName+"-decode",
		)).To(Succeed())
		DeferCleanup(func() {
			// Both, or the next run inherits a monitor scraping a Service that
			// selects nothing -- which is quiet, and exactly the kind of leftover
			// that makes a later spec's metrics hard to trust.
			_ = fixtures.DeleteServiceMonitor(context.Background(), crClient, cfg.MonitoringNS, modelSvcName)
			_ = fixtures.DeleteService(context.Background(), k8sClient, cfg.LLMDNamespace, modelSvcName)
		})

		By("Pinning the model so its ordinary replicas cannot ARRIVE")
		// This is what makes the spec deterministic, and it is also the case the
		// pool matters most in. Every replica reports the same saturated fake
		// metrics, so WVA keeps wanting one more; if those replicas could start,
		// ready would chase desired and the bridge would be returned -- possibly
		// between the lent gate below and the request that proves it serves.
		//
		// Pinned to ONE node with one replica per node, the first replica runs and
		// no second one ever becomes Ready. The shortfall then stays open, the
		// bridge is held, and a scale-up that cannot land is exactly the situation
		// a warm pool exists for.
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelSvcName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			app := dep.Spec.Template.Labels["app"]
			g.Expect(app).NotTo(BeEmpty(), "the model pod template must carry an app label to spread on")
			dep.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": modelNode}
			dep.Spec.Template.Spec.Affinity = &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
						TopologyKey:   "kubernetes.io/hostname",
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
					}},
				},
			}
			_, err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Update(ctx, dep, metav1.UpdateOptions{})
			g.Expect(err).NotTo(HaveOccurred())
		}, time.Minute, 5*time.Second).Should(Succeed())

		By("Waiting for the model to be Ready before registering it")
		// The pool reads its scale target to derive engine options, and skips any
		// variant it cannot read -- so a spec that registers the workload before
		// the Deployment exists spends its first passes being ignored, and any
		// failure afterwards points at the borrow rather than at the race.
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelSvcName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, settle, 5*time.Second).Should(Succeed())

		By("Registering it with WVA, and letting it borrow from the pool")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			// Capped at four replicas. The fake metrics are CONSTANT, so every
			// replica KEDA starts reports the same saturation and WVA keeps
			// wanting one more -- which is what holds the shortfall open long
			// enough to observe the bridge, and also what would fill the cluster
			// if it were left unbounded.
			scalerName, modelSvcName+"-decode", modelSvcName+"-variant", 1, 4, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(cfg.ModelID, "10.0"),
			// One copy, asked for BY NAME. An unnamed variant has to earn its
			// place in the warm set through parking, popularity or repeated
			// misses -- all of which depend on what else the suite is running, so
			// a spec resting on them passes or fails by its neighbours.
			fixtures.WithWarmPoolSelection(borrowPool, 1),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, scalerName)
		})

		By("Setting thresholds the fake metrics exceed")
		// The ConfigMap the controller is READING. There is also a pre-rename one
		// it ignores whenever this exists, and thresholds written there change
		// nothing at all.
		Expect(upsertSaturationConfigEntry(ctx, cfg.WVANamespace,
			scalingPolicyConfigMapName(), defaultConfigKey,
			buildSaturationConfigYAMLWithThresholds("saturation",
				kvCacheThreshold, queueThreshold, scaleUpThreshold, scaleDownBoundary),
		)).To(Succeed())
	})

	It("warms the model, lends the Pod, and the woken engine answers the request", func() {
		By("Waiting for the pool to be DECLARED, which is KEDA calling the scaler")
		// A pool the controller never mentions does not exist as far as WVA is
		// concerned, however many Pods are running: pools are discovered from the
		// trigger metadata of a ScaledObject KEDA has actually called about.
		Eventually(poolState, settle, 5*time.Second).ShouldNot(BeEmpty(),
			"the controller never reported this pool, so its ScaledObject was never called about")

		By("Waiting for the pool to SEE the variant")
		// Separate from the admission below because the causes are unrelated: a
		// variant is dropped here when its scale target cannot be read or its
		// InferencePool cannot be resolved, neither of which has anything to do
		// with whether the pool had room.
		Eventually(poolState, settle, 5*time.Second).Should(MatchRegexp(`variants=[1-9]`),
			"the pool saw no variant to warm: check for 'skipping variant' in the controller log")

		By("Waiting for the model to be warmed into the pool")
		// A borrow can only take a Pod that ALREADY holds the model: the policy
		// searches resident Pods, never empty ones. So the first pass misses, the
		// model is admitted, and a later pass lends.
		Eventually(poolState, settle, 5*time.Second).Should(MatchRegexp(`resident=[1-9]`),
			"the pool never warmed the model, so there was never anything to lend")

		By("Waiting for WVA to decide the borrow by itself")
		// The decision under test. Everything above is setup; this is the first
		// line that depends on WVA having judged the variant short.
		Eventually(poolState, settle, 5*time.Second).Should(MatchRegexp(`lent=[1-9]`),
			"WVA never decided to borrow, so nothing below is being tested")

		By("Confirming the Pod became a READY member of the InferencePool")
		// Membership, not a label check: joining the pool is what routes traffic,
		// and a Pod carrying the selector while failing its probe is in exactly
		// the state that looks lent and serves nothing.
		Eventually(func(g Gomega) {
			members, err := fixtures.ReadyPoolMembers(ctx, crClient, cfg.LLMDNamespace, servingSet)
			g.Expect(err).NotTo(HaveOccurred())
			names := make([]string, 0, len(members))
			for _, m := range members {
				names = append(names, m.Name)
			}
			g.Expect(names).To(ContainElement(poolPod),
				"a lent Pod that never joined the serving pool takes no traffic, whatever the controller believes")
		}, settle, 5*time.Second).Should(Succeed())

		By("Sending a tenant request to the port the InferencePool dials")
		// THE ASSERTION EVERYTHING ELSE IS SCAFFOLDING FOR. The pool's promise is
		// a scale-up served by a model that was already loaded, and this is the
		// only line that checks the request was answered at all.
		//
		// Two minutes rather than the usual five: the bridge is already in place
		// by now, so this waits for nothing. A longer window would only let the
		// spec sit through a return and then fail for a reason that has nothing
		// to do with serving.
		Eventually(func(g Gomega) {
			served, err := fixtures.DriverCall(ctx, k8sClient, cfg.LLMDNamespace, tenantDriver,
				"POST", fmt.Sprintf("http://%s:%d/v1/completions", poolPodIP, fixtures.WarmPoolServingPort),
				fmt.Sprintf(`{"model":%q,"prompt":"hello","max_tokens":4}`, cfg.ModelID))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(served.Status).To(Equal(200),
				"the borrowed Pod must SERVE, not merely be lent: %s", served.Body)
			g.Expect(served.Body).To(ContainSubstring(cfg.ModelID),
				"and the answer must come from the model that was warm inside it")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})
})
