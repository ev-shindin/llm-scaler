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
)

// The warm pool's traffic gate, against a real kubelet.
//
// A pool Pod holds several engines with at most one awake, so what decides
// whether it receives traffic is its readiness — and readiness here is an
// ordinary probe against the proxy, deliberately not a Pod readiness gate the
// controller patches. That choice keeps the controller out of `pods/status`,
// and it means the behaviour cannot be established without a kubelet actually
// running the probe. Everything asserted here is something a unit test cannot
// reach: kubelet timing, NetworkPolicy enforcement, and the real proxy image
// carrying real traffic.
//
// The supervisor and engines are emulated, because a launcher needs a GPU and
// an engine needs a much larger one. The proxy is the real image. What is NOT
// covered, and needs a GPU cluster: that a real vLLM survives sleeping and
// waking, and how long that takes.
var _ = Describe("Warm pool - the pool Pod's traffic gate", Label("full"), Label("warmpool"), Ordered, func() {
	const (
		poolName      = "e2e-warm-pool"
		controlDriver = "e2e-warm-pool-controller"
		tenantDriver  = "e2e-warm-pool-tenant"
		model         = "Qwen/Qwen3-0.6B"
		instanceID    = "qwen"
		enginePort    = fixtures.WarmPoolBasePort
		// In the engine range but with nothing behind it. Outside the range
		// the proxy would refuse it for the WRONG reason -- bad port, not a
		// dead engine -- and the spec below would pass without testing the
		// vetting it names.
		deadEnginePort = fixtures.WarmPoolBasePort + 15
	)

	var (
		ctx        context.Context
		poolSpec   fixtures.WarmPoolSpec
		asControl  fixtures.DriverSpec
		asTenant   fixtures.DriverSpec
		podIP      string
		podName    string
		engineAddr string
		policyName string
	)

	// call makes a request FROM a driver Pod, which is the only vantage point a
	// NetworkPolicy is written about. The API server's pod-proxy cannot be used
	// for the guarded ports: it arrives from the control plane, matches no
	// `from:` selector, and its "cannot reach" 503 is indistinguishable from the
	// proxy's own "no model is awake" 503.
	call := func(driver, method, path string, port int, body string) fixtures.PodProxyResult {
		GinkgoHelper()
		url := fmt.Sprintf("http://%s:%d%s", podIP, port, path)
		result, err := fixtures.DriverCall(ctx, k8sClient, poolSpec.Namespace, driver, method, url, body)
		Expect(err).NotTo(HaveOccurred(), "relaying %s %s through %s", method, url, driver)
		return result
	}

	control := func(method, path string, port int, body string) fixtures.PodProxyResult {
		GinkgoHelper()
		return call(controlDriver, method, path, port, body)
	}

	// controlOK is for the calls that SET UP a scenario. It insists on a real
	// 2xx, because status 0 -- this suite's signal for "could not connect at
	// all" -- would otherwise satisfy any `< 300` bound and let a request that
	// never landed read as success. That is the exact shape of false pass this
	// suite already had once.
	controlOK := func(method, path string, port int, body string) fixtures.PodProxyResult {
		GinkgoHelper()
		result := control(method, path, port, body)
		Expect(result.Status).To(And(BeNumerically(">=", 200), BeNumerically("<", 300)),
			"%s %s on port %d: %s", method, path, port, result.Body)
		return result
	}

	// containerReady is the kubelet's view, which is the only view that decides
	// whether this Pod is in its InferencePool.
	containerReady := func(container string) bool {
		pod, err := k8sClient.CoreV1().Pods(poolSpec.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == container {
				return status.Ready
			}
		}
		return false
	}

	BeforeAll(func() {
		ctx = context.Background()
		poolSpec = fixtures.WarmPoolSpec{
			Name:       poolName,
			Namespace:  cfg.LLMDNamespace,
			ProxyImage: cfg.WarmPoolProxyImage,
		}
		asControl = fixtures.DriverSpec{
			Name: controlDriver, Namespace: cfg.LLMDNamespace, Labels: fixtures.ControllerDriverLabels(),
		}
		asTenant = fixtures.DriverSpec{
			Name: tenantDriver, Namespace: cfg.LLMDNamespace, Labels: fixtures.TenantDriverLabels(),
		}

		By("Applying the NetworkPolicy the pool actually ships with")
		// Read from config/warmpool rather than restated here, and applied by
		// the suite rather than assumed: without this the boundary spec below
		// runs against an allow-all namespace, reads the resulting 200 as "this
		// CNI does not enforce policy", and skips -- so the one spec whose
		// purpose is catching a policy regression would never run in CI, and
		// the skip would be invisible in a green summary.
		var err error
		policyName, err = fixtures.ApplyWarmPoolNetworkPolicy(ctx, k8sClient, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("Creating an emulated warm pool with the real proxy, and two drivers")
		Expect(fixtures.CreateWarmPool(ctx, k8sClient, poolSpec)).To(Succeed())
		Expect(fixtures.CreateHTTPDriver(ctx, k8sClient, asControl)).To(Succeed())
		Expect(fixtures.CreateHTTPDriver(ctx, k8sClient, asTenant)).To(Succeed())

		By("Waiting for the pool Pod to run and its supervisor to accept instructions")
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(poolSpec.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "warm-pool-fixture=" + poolName,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).NotTo(BeEmpty())
			g.Expect(pods.Items[0].Status.Phase).To(Equal(corev1.PodRunning))
			g.Expect(pods.Items[0].Status.PodIP).NotTo(BeEmpty())
			podName = pods.Items[0].Name
			podIP = pods.Items[0].Status.PodIP
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, 5*time.Second).Should(Succeed())

		Eventually(func() bool { return containerReady("inference-server") },
			time.Duration(cfg.EventuallyStandardSec)*time.Second, 2*time.Second).Should(BeTrue(),
			"the supervisor's exec probe must pass; an httpGet probe here would depend on the CNI")

		By("Waiting for both drivers")
		for _, driver := range []string{controlDriver, tenantDriver} {
			Eventually(func(g Gomega) {
				pod, err := k8sClient.CoreV1().Pods(poolSpec.Namespace).Get(ctx, driver, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
				for _, status := range pod.Status.ContainerStatuses {
					g.Expect(status.Ready).To(BeTrue())
				}
			}, time.Duration(cfg.EventuallyStandardSec)*time.Second, 3*time.Second).Should(Succeed())
		}

		engineAddr = fmt.Sprintf("127.0.0.1:%d", enginePort)
	})

	AfterAll(func() {
		if poolSpec.Name == "" {
			return
		}
		Expect(fixtures.DeleteWarmPool(ctx, k8sClient, poolSpec)).To(Succeed())
		Expect(fixtures.DeleteHTTPDriver(ctx, k8sClient, asControl)).To(Succeed())
		Expect(fixtures.DeleteHTTPDriver(ctx, k8sClient, asTenant)).To(Succeed())
		Expect(fixtures.DeleteWarmPoolNetworkPolicy(ctx, k8sClient, cfg.LLMDNamespace, policyName)).To(Succeed())
	})

	It("keeps the Pod out of service while every model is asleep", func() {
		// The invariant the whole design rests on. A pool Pod that reported
		// Ready while asleep would join its InferencePool and 503 every request
		// routed to it, which is worse than having no pool at all.
		Consistently(func() bool { return containerReady("proxy") },
			15*time.Second, 3*time.Second).Should(BeFalse(),
			"a Pod with no model awake must never be ready")

		By("and the serving port refuses rather than queues")
		refused := call(tenantDriver, "GET", "/v1/models", fixtures.WarmPoolServingPort, "")
		Expect(refused.Status).To(Equal(503))
		// The body matters as much as the code. A denial or an unreachable port
		// also produces something that is not 200, and an assertion that cannot
		// tell them apart passes for the wrong reason -- which this one did,
		// before it was driven from a Pod.
		Expect(refused.Body).To(ContainSubstring("no model is awake"),
			"the 503 must come from the proxy, not from a network failure")
	})

	It("stays alive while asleep, rather than restart-looping", func() {
		// Liveness must NOT follow readiness. A liveness probe that failed while
		// the Pod slept would restart it for doing exactly its job, and the
		// restart would take the warm set with it.
		pod, err := k8sClient.CoreV1().Pods(poolSpec.Namespace).Get(ctx, podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, status := range pod.Status.ContainerStatuses {
			Expect(status.RestartCount).To(BeZero(),
				fmt.Sprintf("container %q restarted while the pool was idle", status.Name))
		}
	})

	It("refuses an upstream that is not an engine in this Pod", func() {
		// Loopback alone is not the boundary it looks like: the supervisor and
		// the proxy's own control endpoint are loopback too. An upstream aimed
		// at either turns the serving port -- the one every Pod in the namespace
		// may dial -- into a route to them.
		for _, addr := range []string{
			fmt.Sprintf("127.0.0.1:%d", fixtures.WarmPoolSupervisorPort),
			fmt.Sprintf("127.0.0.1:%d", fixtures.WarmPoolControlPort),
			fmt.Sprintf("127.0.0.1:%d", fixtures.WarmPoolServingPort),
			"10.0.0.9:9001",
		} {
			result := control("PUT", "/upstream", fixtures.WarmPoolControlPort,
				fmt.Sprintf(`{"address":%q}`, addr))
			Expect(result.Status).To(Equal(400), "pointing at %s must be refused: %s", addr, result.Body)
		}

		By("and none of them was stored")
		Expect(control("GET", "/upstream", fixtures.WarmPoolControlPort, "").Body).
			To(ContainSubstring(`"address":""`))
	})

	It("refuses to point at an engine that does not answer", func() {
		// Otherwise the Pod goes Ready on the strength of the pointer alone and
		// admits traffic to nothing until the first request 502s. Measured on a
		// cluster before this was fixed.
		result := control("PUT", "/upstream", fixtures.WarmPoolControlPort,
			fmt.Sprintf(`{"address":"127.0.0.1:%d"}`, deadEnginePort))
		Expect(result.Status).To(Equal(502), "body: %s", result.Body)

		Consistently(func() bool { return containerReady("proxy") },
			8*time.Second, 2*time.Second).Should(BeFalse(),
			"a refused point must leave the Pod out of service")
	})

	It("admits traffic once a model is awake, and withdraws it as quickly", func() {
		By("Admitting a model to the pool")
		controlOK("PUT", "/v2/vllm/instances/"+instanceID, fixtures.WarmPoolSupervisorPort,
			fmt.Sprintf(`{"options":%q,"env_vars":{"VLLM_SERVER_DEV_MODE":"1"}}`,
				fixtures.WarmPoolInstanceOptions(model, enginePort)))

		By("Waking it and pointing the proxy, which is the borrow ordering")
		Expect(control("POST", "/wake_up", enginePort, "").Status).To(Equal(200))
		Expect(control("PUT", "/upstream", fixtures.WarmPoolControlPort,
			fmt.Sprintf(`{"address":%q}`, engineAddr)).Status).To(Equal(200))

		By("Asserting the kubelet admits the Pod within a couple of probe periods")
		// Fast on purpose: the borrow exists to beat a ~35 s cold start, and a
		// Pod that takes 20 s to join its pool has spent most of that advantage.
		start := time.Now()
		Eventually(func() bool { return containerReady("proxy") },
			30*time.Second, time.Second).Should(BeTrue())
		GinkgoWriter.Printf("kubelet admitted the Pod after %s\n", time.Since(start))

		By("Serving a request through the port the InferencePool dials")
		served := call(tenantDriver, "POST", "/v1/completions", fixtures.WarmPoolServingPort,
			fmt.Sprintf(`{"model":%q,"prompt":"hello","max_tokens":4}`, model))
		Expect(served.Status).To(Equal(200), "body: %s", served.Body)
		Expect(served.Body).To(ContainSubstring(model),
			"the request must reach the engine that is awake")

		By("Returning the bridge: clear the proxy FIRST, which is the gate")
		controlOK("DELETE", "/upstream", fixtures.WarmPoolControlPort, "")

		// The measurement that decides whether the controller's drain wait is
		// long enough. With the probe defaults (period 5, failureThreshold 3)
		// this takes 5-20 s, while the controller sleeps the engine after its
		// drain -- leaving the Pod Ready with nothing awake behind it, which is
		// a 503 for every request in the window.
		start = time.Now()
		Eventually(func() bool { return containerReady("proxy") },
			30*time.Second, time.Second).Should(BeFalse())
		withdrawn := time.Since(start)
		GinkgoWriter.Printf("kubelet withdrew the Pod after %s\n", withdrawn)
		Expect(withdrawn).To(BeNumerically("<", 10*time.Second),
			"withdrawal must fit inside the controller's drain wait, or every sleep is a 503 window")

		By("and the model is still resident, because a return sleeps rather than evicts")
		// Asserted HERE rather than in a spec of its own: the instance is
		// created a few lines above, so a separate spec would depend on this
		// one having run and would fail on its own under -ginkgo.focus, for a
		// reason that has nothing to do with the invariant.
		listed := controlOK("GET", "/v2/vllm/instances", fixtures.WarmPoolSupervisorPort, "")
		Expect(listed.Body).To(ContainSubstring(instanceID),
			"the instance must survive the return, or the pool is a cold start with extra steps")
		Expect(strings.Count(listed.Body, `"instance_id"`)).To(Equal(1))
	})

	It("lets only the controller reach the ports that can take the Pod over", func() {
		// :8001 spawns processes with caller-supplied argv in a container that
		// mounts the shared model cache read-write, and :8002 re-points where
		// this Pod sends the traffic it receives. Neither is any tenant's
		// business, and the serving port is.
		//
		// The policy is applied by this suite, so "the controller cannot reach
		// the supervisor" is a FAILURE and not a reason to skip. That
		// distinction matters: the controller being locked out of these ports
		// is precisely the regression that made the pool inert, and a guard
		// that skipped on it would swallow the defect it exists to catch.
		// EVERY guarded port, not just the supervisor. The three are in one
		// ingress rule, so trimming the rule to drop :8002 or the engine range
		// would leave the pool just as inert -- and a tenant-denial loop reads
		// "cannot connect" on a port nobody can reach, so it would stay green
		// while the controller was locked out too.
		Expect(control("GET", "/health", fixtures.WarmPoolSupervisorPort, "").Status).To(Equal(200),
			"the policy must admit the controller to the supervisor, or nothing can be listed")
		Expect(control("GET", "/upstream", fixtures.WarmPoolControlPort, "").Status).To(Equal(200),
			"and to the proxy's control endpoint, or no bridge can ever be pointed")
		Expect(control("GET", "/health", enginePort, "").Status).To(Equal(200),
			"and to the engine ports, or every /is_sleeping fails and the pool never wakes anything")

		// A CNI that ignores NetworkPolicy is a different matter: nothing is
		// wrong with the manifest, the cluster simply cannot show it. kind's
		// default CNI accepts policy objects and ignores them, and a suite that
		// called that "secure" would be worse than one that never looked.
		if call(tenantDriver, "GET", "/health", fixtures.WarmPoolSupervisorPort, "").Status != 0 {
			Skip("this cluster's CNI does not enforce NetworkPolicy, so the boundary cannot be observed")
		}

		By("A tenant workload cannot reach the supervisor, the control endpoint, or an engine")
		// The engine range is in the list because the controller reaches
		// engines DIRECTLY -- sleeping and waking are vLLM's own API -- so the
		// policy has to admit it there, and that rule is one careless
		// `podSelector: {}` away from handing every tenant a sleep switch for
		// another tenant's model.
		for _, port := range []int{
			fixtures.WarmPoolSupervisorPort,
			fixtures.WarmPoolControlPort,
			enginePort,
		} {
			denied := call(tenantDriver, "GET", "/health", port, "")
			Expect(denied.Status).To(BeZero(),
				"port %d must be unreachable for a tenant, got %d: %s", port, denied.Status, denied.Body)
		}

		By("but it can still reach the serving port, which the EPP needs")
		Expect(call(tenantDriver, "GET", "/v1/models", fixtures.WarmPoolServingPort, "").Status).
			To(Equal(503), "the serving port must stay open to the namespace")
	})
})
