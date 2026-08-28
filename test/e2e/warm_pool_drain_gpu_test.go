package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// DOES A REQUEST SURVIVE THE POOL TAKING ITS POD BACK?
//
// Everything else about draining is asserted on the wire: a unit test pins that
// WVA sends /sleep?mode=wait, and the manifest checks pin that a pool Pod
// carries a preStop hook. Neither proves the thing an operator cares about,
// because both would pass just as happily against an engine that ignored the
// mode.
//
// Only a real vLLM can answer it, which is why this spec is labelled separately
// and skips without one. The emulated supervisor the other warm pool specs use
// implements /sleep as a state flag: it has no in-flight work to finish, so it
// would report success whatever was asked of it.
//
// The shape is: hold a generation open, sleep the engine underneath it, and read
// what comes back. Sleeping under a live request is not a contrived event -- it
// is exactly what returning a bridge does, and a bridge is returned whenever a
// variant stops needing it.
//
// Run on a GPU cluster:
//
//	USE_SIMULATOR=false MODEL_ID=Qwen/Qwen3-0.6B \
//	  go test ./test/e2e/ -ginkgo.label-filter=warmpool-drain -timeout 40m
var _ = Describe("Warm pool - a returned Pod finishes what it was writing", Label("gpu"), Label("warmpool-drain"), Ordered, Serial, func() {
	const (
		drainPool     = "e2e-pool-drain"
		controlDriver = "e2e-drain-control"
		// Long enough that the sleep lands while tokens are still being
		// produced. Too short and the request finishes first, and the spec
		// passes without testing anything.
		longGeneration = 512
		// How long to let the generation run before sleeping under it.
		underway = 5 * time.Second
		settle   = 10 * time.Minute
	)

	var (
		ctx        context.Context
		controller fixtures.ControllerDeployment
		poolSpec   fixtures.WarmPoolSpec
		asControl  fixtures.DriverSpec
		podIP      string
		enginePort = fixtures.WarmPoolBasePort
	)

	// control relays a call from the driver Pod, which is the only vantage point
	// the pool's NetworkPolicy admits for the supervisor and engine ports.
	control := func(method, path string, port int, body string) (fixtures.PodProxyResult, error) {
		return fixtures.DriverCall(ctx, k8sClient, cfg.LLMDNamespace, controlDriver,
			method, fmt.Sprintf("http://%s:%d%s", podIP, port, path), body)
	}

	BeforeAll(func() {
		if cfg.UseSimulator {
			Skip("this spec needs a real vLLM: the emulator's /sleep is a state flag with no work to drain. " +
				"Set USE_SIMULATOR=false on a GPU cluster")
		}
		ctx = context.Background()
		controller = fixtures.ControllerDeployment{Namespace: cfg.WVANamespace, Name: "wva-controller-manager"}

		By("Applying the NetworkPolicy the pool ships with, so the driver is the admitted path")
		_, err := fixtures.ApplyWarmPoolNetworkPolicy(ctx, k8sClient, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("Enabling the warm pool")
		restore, err := fixtures.EnableWarmPool(ctx, k8sClient, controller, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = restore(context.Background())
			_ = fixtures.WaitForControllerReady(context.Background(), k8sClient, controller)
		})

		By("Standing up a pool with a real launcher, and a driver to reach it")
		poolSpec = fixtures.WarmPoolSpec{
			Name:       drainPool,
			Namespace:  cfg.LLMDNamespace,
			ProxyImage: cfg.WarmPoolProxyImage,
			PoolName:   drainPool,
			GPUs:       1,
		}
		Expect(fixtures.CreateWarmPool(ctx, k8sClient, poolSpec)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteWarmPool(context.Background(), k8sClient, poolSpec) })

		asControl = fixtures.DriverSpec{
			Name: controlDriver, Namespace: cfg.LLMDNamespace, Labels: fixtures.ControllerDriverLabels(),
		}
		Expect(fixtures.CreateHTTPDriver(ctx, k8sClient, asControl)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteHTTPDriver(context.Background(), k8sClient, asControl) })

		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fixtures.WarmPoolNameLabel + "=" + drainPool,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).NotTo(BeEmpty())
			g.Expect(pods.Items[0].Status.PodIP).NotTo(BeEmpty())
			podIP = pods.Items[0].Status.PodIP
		}, settle, 5*time.Second).Should(Succeed())

		By("Admitting the model, which on a real launcher is a full load")
		// Driven directly rather than through a scale-up: what is under test is
		// the RETURN, and manufacturing the borrow keeps the setup from
		// depending on demand arriving at the right moment.
		Eventually(func(g Gomega) {
			res, err := control("PUT", "/v2/vllm/instances/drain-test", fixtures.WarmPoolSupervisorPort,
				fmt.Sprintf(`{"options":%q,"env_vars":{"VLLM_SERVER_DEV_MODE":"1"}}`,
					fixtures.WarmPoolInstanceOptions(cfg.ModelID, enginePort)))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.Status).To(BeNumerically("<", 300), "supervisor refused the instance: %s", res.Body)
		}, settle, 10*time.Second).Should(Succeed())

		By("Waiting for the engine to serve, which is what a borrow waits for")
		Eventually(func(g Gomega) {
			res, err := control("GET", "/health", enginePort, "")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.Status).To(Equal(200))
		}, settle, 5*time.Second).Should(Succeed())
	})

	It("finishes an in-flight generation after the engine is put to sleep", func() {
		By("Starting a long generation and leaving it running")
		type result struct {
			res fixtures.PodProxyResult
			err error
		}
		done := make(chan result, 1)
		go func() {
			defer GinkgoRecover()
			res, err := control("POST", "/v1/completions", enginePort,
				fmt.Sprintf(`{"model":%q,"prompt":"Write a long story about a warm pool.","max_tokens":%d}`,
					cfg.ModelID, longGeneration))
			done <- result{res: res, err: err}
		}()

		By("Letting it get properly under way, so the sleep lands mid-generation")
		time.Sleep(underway)

		By("Sleeping the engine underneath it, exactly as returning a bridge does")
		slept, err := control("POST", "/sleep?level=1&mode=wait", enginePort, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(slept.Status).To(Equal(200), "sleep refused: %s", slept.Body)

		By("Reading the response that was in flight")
		var got result
		Eventually(done, 6*time.Minute).Should(Receive(&got))

		// THE ASSERTION. With mode=abort -- vLLM's default, and what WVA sent
		// before this was fixed -- the generation is cancelled and the caller
		// gets nothing usable. With mode=wait the engine finishes what it was
		// writing and only then sleeps.
		Expect(got.err).NotTo(HaveOccurred(), "the generation did not survive the sleep")
		Expect(got.res.Status).To(Equal(200), "generation returned %d: %s",
			got.res.Status, truncateBody(got.res.Body))

		var parsed struct {
			Choices []struct {
				Text         string `json:"text"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		Expect(json.Unmarshal([]byte(got.res.Body), &parsed)).To(Succeed(),
			"response was not a completion: %s", truncateBody(got.res.Body))
		Expect(parsed.Choices).NotTo(BeEmpty(), "no choices in %s", truncateBody(got.res.Body))
		Expect(parsed.Choices[0].Text).NotTo(BeEmpty(), "an empty completion is an aborted one")
		Expect(parsed.Choices[0].FinishReason).To(BeElementOf("stop", "length"),
			"finish_reason %q says the generation was cut short rather than drained",
			parsed.Choices[0].FinishReason)
	})

	It("is asleep once the drain has finished, holding no GPU memory", func() {
		// The drain must END in a sleep. An engine that finished the request and
		// stayed awake would satisfy the assertion above while still holding its
		// GPU -- the pool would be paying for capacity it believes it released.
		Eventually(func(g Gomega) {
			res, err := control("GET", "/is_sleeping", enginePort, "")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(res.Body).To(ContainSubstring(`"is_sleeping":true`),
				"the engine drained but never slept: %s", res.Body)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})
})

// truncateBody keeps a failure message readable when the body is a whole
// generation.
func truncateBody(s string) string {
	const max = 300
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "..."
}
