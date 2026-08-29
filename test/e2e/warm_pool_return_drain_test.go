package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// WHAT THE CONTROLLER ASKS FOR WHEN IT HANDS A POD BACK.
//
// vLLM's /sleep takes a mode and defaults to "abort", which cancels every
// in-flight request the moment it lands. Only mode=wait drains them. Returning a
// bridge is routine -- it happens whenever a variant stops needing one -- so a
// return that aborts makes the pool a source of failed requests in exactly the
// situation it exists to smooth.
//
// A unit test pins the query WVA builds, and a GPU spec proves a real generation
// survives it. Neither covers the path between them: the CONTROLLER deciding to
// return a Pod and issuing the call itself. That is what this asserts, by
// reading what the supervisor was actually asked for.
//
// It cannot prove the request survives -- the emulated engine has no work to
// drain, and saying otherwise would be the kind of false comfort this suite
// keeps finding. What it proves is that the controller-driven return asks to
// drain, which is the half the GPU spec cannot reach and the half most likely to
// regress silently: dropping the parameter leaves every other assertion here
// passing.
var _ = Describe("Warm pool - the controller drains when it returns a Pod", Label("full"), Label("warmpool-return"), Ordered, Serial, func() {
	const (
		returnPool    = "e2e-pool-return"
		controlDriver = "e2e-return-control"
		model         = "Qwen/Qwen3-0.6B"
		instanceID    = "returner"
		enginePort    = fixtures.WarmPoolBasePort
		settle        = 3 * time.Minute
	)

	var (
		ctx       context.Context
		poolSpec  fixtures.WarmPoolSpec
		asControl fixtures.DriverSpec
		podIP     string
		podName   string
	)

	control := func(method, path string, port int, body string) fixtures.PodProxyResult {
		GinkgoHelper()
		res, err := fixtures.DriverCall(ctx, k8sClient, cfg.LLMDNamespace, controlDriver,
			method, fmt.Sprintf("http://%s:%d%s", podIP, port, path), body)
		Expect(err).NotTo(HaveOccurred(), "relaying %s %s", method, path)
		return res
	}

	BeforeAll(func() {
		ctx = context.Background()
		controller := fixtures.ControllerDeployment{Namespace: cfg.WVANamespace, Name: "wva-controller-manager"}

		By("Applying the NetworkPolicy, so the driver is the admitted path")
		_, err := fixtures.ApplyWarmPoolNetworkPolicy(ctx, k8sClient, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("Enabling the warm pool")
		restore, err := fixtures.EnableWarmPool(ctx, k8sClient, controller, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = restore(context.Background())
			_ = fixtures.WaitForControllerReady(context.Background(), k8sClient, controller)
		})

		poolSpec = fixtures.WarmPoolSpec{
			Name: returnPool, Namespace: cfg.LLMDNamespace,
			ProxyImage: cfg.WarmPoolProxyImage, PoolName: returnPool,
		}
		Expect(fixtures.CreateWarmPool(ctx, k8sClient, poolSpec)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteWarmPool(context.Background(), k8sClient, poolSpec) })

		// A pool is DECLARED by a ScaledObject trigger, not by the existence of
		// its Pods: without this the Deployment runs, the Pods are healthy, and
		// the controller says nothing about any of it -- which reads exactly
		// like a controller that has decided not to act.
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			returnPool, returnPool, returnPool, 1, 4, cfg.MonitoringNS,
			fixtures.WithWarmPoolTrigger(returnPool, map[string]string{
				"warmPoolSleepMinSize": "0",
			}),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, returnPool)
		})

		asControl = fixtures.DriverSpec{
			Name: controlDriver, Namespace: cfg.LLMDNamespace, Labels: fixtures.ControllerDriverLabels(),
		}
		Expect(fixtures.CreateHTTPDriver(ctx, k8sClient, asControl)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteHTTPDriver(context.Background(), k8sClient, asControl) })

		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fixtures.WarmPoolNameLabel + "=" + returnPool,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).NotTo(BeEmpty())
			g.Expect(pods.Items[0].Status.PodIP).NotTo(BeEmpty())
			podIP = pods.Items[0].Status.PodIP
			podName = pods.Items[0].Name
		}, settle, 5*time.Second).Should(Succeed())
	})

	It("asks the engine to drain, not to abort", func() {
		By("Putting a model in the Pod and lending it, which is what a borrow does")
		// Driven by hand: what is under test is the RETURN, and manufacturing the
		// borrow keeps the setup independent of demand arriving on cue.
		created := control("PUT", "/v2/vllm/instances/"+instanceID, fixtures.WarmPoolSupervisorPort,
			fmt.Sprintf(`{"options":%q,"env_vars":{"VLLM_SERVER_DEV_MODE":"1"}}`,
				fixtures.WarmPoolInstanceOptions(model, enginePort)))
		Expect(created.Status).To(And(BeNumerically(">=", 200), BeNumerically("<", 300)),
			"supervisor refused the instance: %s", created.Body)

		Expect(control("POST", "/wake_up", enginePort, "").Status).To(Equal(200))
		// The proxy is deliberately NOT pointed at it. An awake instance the
		// proxy does not serve is a bridge the controller started and never
		// finished -- WAKING rather than Serving -- and reclaiming those is a
		// path the controller drives on its own, without needing a variant to
		// stop being short. That is what makes this a controller-driven return
		// rather than another hand-driven one.

		By("Labelling the Pod as lent, so the controller sees a bridge to return")
		// The controller reclaims a Pod that carries a variant's pool labels but
		// belongs to no variant it can find short -- an orphan return. That is a
		// real path, and the shortest way to make the controller issue a return
		// of its own accord.
		pod, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(ctx, podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels["llm-d.ai/inferenceServing"] = "true"
		pod.Labels["llm-d.ai/model"] = instanceID
		_, err = k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Update(ctx, pod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the controller to hand the Pod back on its own")
		// The engine going back to sleep is the observable end of a return: the
		// controller clears the proxy, waits out the routing chain, and sleeps
		// the engine.
		Eventually(func(g Gomega) {
			res := control("GET", "/is_sleeping", enginePort, "")
			// PARSED, not string-matched. The engine answers through json.dumps,
			// which puts a space after the colon, so a literal "is_sleeping":true
			// never matches and the spec reports the controller as having done
			// nothing while the body in the same message says it slept.
			body, err := res.JSON()
			g.Expect(err).NotTo(HaveOccurred(), "is_sleeping: %s", res.Body)
			g.Expect(body["is_sleeping"]).To(BeTrue(),
				"the controller has not returned the Pod yet: %s", res.Body)
		}, settle, 5*time.Second).Should(Succeed())

		By("Reading what the controller ASKED the engine for")
		asked := control("GET", "/last_sleep", enginePort, "")
		Expect(asked.Status).To(Equal(200))
		Expect(asked.Body).To(ContainSubstring("mode=wait"),
			"the controller slept the engine as %s; without mode=wait vLLM ABORTS every "+
				"in-flight request, and returning a bridge is routine", asked.Body)
		Expect(asked.Body).To(ContainSubstring("level=1"),
			"the pool sleeps at level 1: %s", asked.Body)
	})
})
