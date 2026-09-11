package e2e

import (
	"context"
	"encoding/json"
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

// A RETAINED pool does not churn the Pod it lent.
//
// `warmPoolRetained` switches off the hold timeout: `expired := !cfg.Retained &&
// now.Sub(borrowedAt) >= cfg.MaxHold`. Both halves are unit-tested, and so is
// the value's trip from trigger metadata into the policy. What was never
// exercised is the whole chain on a cluster.
//
// THE OBVIOUS ASSERTION DOES NOT WORK, and that is why this spec looks the way
// it does. Watching `lent` cannot tell a retained pool from a bridge: when the
// shortfall stays open, an expired bridge is returned and borrowed BACK IN THE
// SAME PASS. Measured directly --
//
//	retained=false -> pool calls: [deactivate qwen@a, activate qwen@a]
//	retained=true  -> pool calls: []
//
// -- so the pool summary is identical before and after (same pods, lent=1), no
// state line is emitted because nothing changed, and the return path logs
// nothing at all: it records a metric and forgets the borrow. A spec resting on
// `lent` passes with retention switched OFF, which is worth less than no spec.
//
// What the churn does leave is a SLEEP. Deactivate ends by sleeping the engine,
// so each reclaim is one more /sleep at the launcher, and the emulator counts
// them. A retained pool sleeps its lent engine zero more times; a bridge sleeps
// it once per MaxHold. That is the difference, and it is monotonic, so reading
// it is not a race.
//
// The model is pinned so its ordinary replicas can never become Ready. The
// shortfall therefore never closes and the variant never stops wanting the Pod,
// which removes the legitimate reason to hand it back and leaves the hold
// timeout as the only thing that could.
var _ = Describe("Warm pool - a retained pool does not churn its Pod", Label("full"), Label("warmpool-retained"), Ordered, Serial, func() {
	const (
		retainedPool  = "e2e-pool-retained"
		modelSvcName  = "e2e-retained-ms"
		fixturePool   = "e2e-retained-pool"
		scalerName    = "e2e-retained"
		controlDriver = "e2e-retained-control"

		retainedFakeMetrics = `{"kv-cache-usage":0.9,"running-requests":8,"waiting-requests":4}`
		rtScaleUp           = 0.30
		rtScaleDown         = 0.20
		rtKVCache           = 0.80
		rtQueue             = 50

		// SHORT, and that is the point: this is the timer the spec is about. A
		// bridge on this hold reclaims within half a minute, so the window below
		// covers several reclaims that a retained pool must not perform.
		retainedMaxHold = 20 * time.Second
		holdWindow      = 2 * time.Minute

		settle         = 5 * time.Minute
		rtSinceRestart = int64(900)
	)

	var (
		ctx        context.Context
		controller fixtures.ControllerDeployment
		poolSpec   fixtures.WarmPoolSpec
		modelNode  string
		poolPodIP  string
		enginePort = fixtures.WarmPoolBasePort
	)

	control := func(method, path string) fixtures.PodProxyResult {
		GinkgoHelper()
		res, err := fixtures.DriverCall(ctx, k8sClient, cfg.LLMDNamespace, controlDriver,
			method, fmt.Sprintf("http://%s:%d%s", poolPodIP, enginePort, path), "")
		Expect(err).NotTo(HaveOccurred(), "relaying %s %s", method, path)
		return res
	}

	// sleepCount is how many times this engine has been put to sleep, ever.
	// Monotonic, so a reading taken later can be compared with one taken
	// earlier without a race.
	sleepCount := func() int {
		GinkgoHelper()
		res := control("GET", "/sleep_count")
		Expect(res.Status).To(Equal(200), "reading /sleep_count: %+v", res)
		var got struct {
			SleepCount int `json:"sleep_count"`
		}
		Expect(json.Unmarshal([]byte(res.Body), &got)).To(Succeed(), "body: %q", res.Body)
		return got.SleepCount
	}

	poolState := func() string {
		_, logs, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, controller.Namespace,
			"control-plane=controller-manager", "", rtSinceRestart)
		if err != nil {
			return ""
		}
		last := ""
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, `"pool": "`+retainedPool+`"`) {
				last = line
			}
		}
		return last
	}

	lentNow := func() string {
		for _, field := range strings.Fields(poolState()) {
			if strings.HasPrefix(field, "lent=") {
				return strings.TrimSuffix(field, `"`)
			}
		}
		return ""
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

		By("Standing up a RETAINED pool whose hold is short enough to fire")
		poolSpec = fixtures.WarmPoolSpec{
			Name:       retainedPool,
			Namespace:  cfg.LLMDNamespace,
			ProxyImage: cfg.WarmPoolProxyImage,
			PoolName:   retainedPool,
		}
		Expect(fixtures.CreateWarmPool(ctx, k8sClient, poolSpec)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteWarmPool(context.Background(), k8sClient, poolSpec) })

		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			retainedPool, retainedPool, retainedPool, 1, 4, cfg.MonitoringNS,
			fixtures.WithWarmPoolTrigger(retainedPool, map[string]string{
				// Reserve ZERO: the admission budget is free Pods minus the
				// reserve, so a one-Pod pool with the default reserve of one
				// warms nothing and the spec would fail on configuration rather
				// than on retention.
				"warmPoolSleepMinSize": "0",
				"warmPoolMaxHold":      retainedMaxHold.String(),
				"warmPoolRetained":     "true",
			}),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, retainedPool)
		})

		By("Waiting for the pool Pod, which is what the borrow will take")
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fixtures.WarmPoolNameLabel + "=" + retainedPool,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).To(HaveLen(1))
			g.Expect(pods.Items[0].Status.PodIP).NotTo(BeEmpty())
			poolPodIP = pods.Items[0].Status.PodIP
		}, settle, 5*time.Second).Should(Succeed())

		By("Creating the control driver, which is the admitted path to the engine port")
		// ControllerDriverLabels, not tenant: the pool's NetworkPolicy admits the
		// engine range from WVA alone, and /sleep_count lives there.
		Expect(fixtures.CreateHTTPDriver(ctx, k8sClient, fixtures.DriverSpec{
			Name: controlDriver, Namespace: cfg.LLMDNamespace, Labels: fixtures.ControllerDriverLabels(),
		})).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteHTTPDriver(context.Background(), k8sClient, fixtures.DriverSpec{
				Name: controlDriver, Namespace: cfg.LLMDNamespace,
			})
		})

		By("Creating a model whose metrics say it is over its threshold")
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, fixturePool, cfg.ModelID,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", retainedFakeMetrics})).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteModelService(context.Background(), k8sClient, cfg.LLMDNamespace, modelSvcName)
		})

		By("Making those metrics reachable, without which there is no demand at all")
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelSvcName+"-decode", 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelSvcName+"-decode",
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteServiceMonitor(context.Background(), crClient, cfg.MonitoringNS, modelSvcName)
			_ = fixtures.DeleteService(context.Background(), k8sClient, cfg.LLMDNamespace, modelSvcName)
		})

		By("Pinning the model so its ordinary replicas cannot ARRIVE")
		// What leaves the hold timeout as the only possible reclaim. With
		// replicas able to land, ready would chase desired, the variant would
		// stop wanting the Pod, and it would go back for the RIGHT reason --
		// which this spec could not tell apart from the timer firing.
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
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelSvcName+"-decode", metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, settle, 5*time.Second).Should(Succeed())

		By("Registering it with WVA, and letting it borrow from the pool")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			scalerName, modelSvcName+"-decode", modelSvcName+"-variant", 1, 4, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(cfg.ModelID, "10.0"),
			fixtures.WithWarmPoolSelection(retainedPool, 1),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, scalerName)
		})

		By("Setting thresholds the fake metrics exceed")
		// Restored afterwards: the suite shares one ConfigMap and one
		// controller, so thresholds left behind are what every later spec
		// scales against.
		cmName := scalingPolicyConfigMapName()
		original, err := k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Get(ctx, cmName, metav1.GetOptions{})
		existedBefore := err == nil
		if existedBefore {
			original = original.DeepCopy()
		}
		Expect(upsertSaturationConfigEntry(ctx, cfg.WVANamespace, cmName, defaultConfigKey,
			buildSaturationConfigYAMLWithThresholds("saturation",
				rtKVCache, rtQueue, rtScaleUp, rtScaleDown),
		)).To(Succeed())
		DeferCleanup(func() {
			restoreSaturationConfigMap(context.Background(), cfg.WVANamespace, cmName, original, existedBefore)
		})
	})

	It("does not sleep the lent engine again once the hold has passed", func() {
		By("Waiting for the pool to be DECLARED, which is KEDA calling the scaler")
		Eventually(poolState, settle, 5*time.Second).ShouldNot(BeEmpty(),
			"the controller never reported this pool, so its ScaledObject was never called about")

		By("Waiting for the borrow")
		Eventually(lentNow, settle, 5*time.Second).Should(Equal("lent=1"),
			"the pool never lent a Pod, so there is nothing for retention to hold: "+poolState())

		By("Recording how many times this engine has been slept")
		before := sleepCount()

		By("Holding past several MaxHolds, which a bridge would not do")
		// THE assertion. The shortfall is pinned open, so the variant never
		// stops wanting the Pod; with retention off, each MaxHold reclaims it
		// and sleeps the engine on the way out, then borrows it straight back.
		Consistently(sleepCount, holdWindow, 10*time.Second).Should(Equal(before),
			"a retained pool slept its lent engine again, so the hold timeout reclaimed it: "+poolState())

		By("Confirming the controller still considers the pool retained")
		// Separates the two ways the assertion above can pass: retention doing
		// its job, versus the pool never having been re-evaluated at all.
		Expect(poolState()).To(ContainSubstring("retained=true"),
			"the pool held its Pod but does not report itself retained: "+poolState())
	})
})
