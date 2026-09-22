package registration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

var _ = Describe("RegisterThroughputAnalyzerQueries", func() {
	var (
		ctx       context.Context
		registry  *source.SourceRegistry
		mockAPI   *mockPrometheusAPI
		queryList *source.QueryList
	)

	BeforeEach(func() {
		ctx = context.Background()
		registry = source.NewSourceRegistry()
		mockAPI = &mockPrometheusAPI{}
	})

	Context("when prometheus source is registered", func() {
		BeforeEach(func() {
			metricsSource := prometheus.NewPrometheusSource(ctx, mockAPI, prometheus.DefaultPrometheusSourceConfig())
			err := registry.Register("prometheus", metricsSource)
			Expect(err).NotTo(HaveOccurred())
			// Both, as cmd/main does when the throughput analyzer is enabled.
			// QueryRequestRate now comes from the arrival-rate set, so a fixture
			// registering only the TA queries could not render it.
			RegisterArrivalRateQueries(registry)
			RegisterThroughputAnalyzerQueries(registry)
			queryList = registry.Get("prometheus").QueryList()
		})

		It("should panic when RegisterThroughputAnalyzerQueries is called twice on the same registry", func() {
			// MustRegister panics on duplicate names; calling the function a second
			// time on the same registry (queries already registered by BeforeEach)
			// must trigger that panic.
			Expect(func() {
				RegisterThroughputAnalyzerQueries(registry)
			}).To(Panic())
		})

		It("should register exactly the TA-exclusive queries", func() {
			// QueryRequestRate and QueryModelArrivalRate left this set: they are
			// lambda's two sources and the saturation demand floor needs them
			// whether or not this analyzer runs, so they register
			// unconditionally (RegisterArrivalRateQueries).
			expectedQueries := []string{
				QueryGenerationTokenRate,
				QueryKvUsageInstant,
			}
			for _, name := range expectedQueries {
				q := queryList.Get(name)
				Expect(q).NotTo(BeNil(), "expected query %q to be registered", name)
				Expect(q.Name).To(Equal(name))
				Expect(q.Type).To(Equal(source.QueryTypePromQL))
			}
		})

		It("should build QueryGenerationTokenRate scoped to the namespace, grouped by model", func() {
			rendered, err := queryList.Build(QueryGenerationTokenRate, map[string]string{
				source.ParamNamespace: "test-ns",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(rendered).To(ContainSubstring(`namespace="test-ns"`))
			Expect(rendered).To(ContainSubstring(`sum by (model_name,`))
			Expect(rendered).NotTo(ContainSubstring(`model_name="`), "the model is partitioned in the collector, not matched in PromQL")
			Expect(rendered).To(ContainSubstring(`[1m]`))
			Expect(rendered).To(ContainSubstring(`vllm:request_generation_tokens_sum`))
		})

		It("should build QueryKvUsageInstant without max_over_time", func() {
			rendered, err := queryList.Build(QueryKvUsageInstant, map[string]string{
				source.ParamNamespace: "test-ns",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(rendered).To(ContainSubstring(`vllm:kv_cache_usage_perc`))
			Expect(rendered).NotTo(ContainSubstring(`max_over_time`))
			Expect(rendered).To(ContainSubstring(`namespace="test-ns"`))
			Expect(rendered).To(ContainSubstring(`max by (model_name,`))
			Expect(rendered).NotTo(ContainSubstring(`model_name="`), "the model is partitioned in the collector, not matched in PromQL")
		})

		It("should build QueryRequestRate with 1m window over token count, once registered", func() {
			rendered, err := queryList.Build(QueryRequestRate, map[string]string{
				source.ParamNamespace: "test-ns",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(rendered).To(ContainSubstring(`vllm:request_generation_tokens_count`))
			Expect(rendered).To(ContainSubstring(`[1m]`))
			Expect(rendered).To(ContainSubstring(`namespace="test-ns"`))
			Expect(rendered).To(ContainSubstring(`sum by (model_name,`))
			Expect(rendered).NotTo(ContainSubstring(`model_name="`), "the model is partitioned in the collector, not matched in PromQL")
		})
	})

	Context("when prometheus source is not registered", func() {
		It("should not panic", func() {
			Expect(func() {
				RegisterThroughputAnalyzerQueries(registry)
			}).NotTo(Panic())
		})
	})
})

// The arrival-rate queries must be registered whether or not the throughput
// analyzer is enabled.
//
// They used to live inside RegisterThroughputAnalyzerQueries, which cmd/main
// calls only when that analyzer is on — and it is opt-in, so by default neither
// query was registered. The saturation analyzer's demand floor also reads λ, so
// with the default configuration both of its sources were structurally zero and
// the floor could never compute anything at all. It reported "no arrival rate"
// every cycle on a fleet visibly serving 14 QPS, and no unit test noticed
// because every one of them builds AnalyzerInput in Go with the field already
// populated. Only a live run surfaced it.
var _ = Describe("arrival-rate query registration", func() {
	It("registers the floor's rate sources without the throughput analyzer", func() {
		reg := source.NewSourceRegistry()
		Expect(reg.Register("prometheus", prometheus.NewPrometheusSource(
			context.Background(), &mockPrometheusAPI{}, prometheus.DefaultPrometheusSourceConfig()))).To(Succeed())

		// Deliberately NOT calling RegisterThroughputAnalyzerQueries.
		RegisterArrivalRateQueries(reg)

		ql := reg.Get("prometheus").QueryList()
		Expect(ql.Get(QueryModelArrivalRate)).NotTo(BeNil(),
			"the EPP-sourced arrival rate must not depend on the throughput analyzer")
		Expect(ql.Get(QueryRequestRate)).NotTo(BeNil(),
			"nor the completion-rate fallback it degrades to")
		Expect(ql.Get(EngineQuery(inferenceengine.EngineSGLang, QueryRequestRate))).NotTo(BeNil(),
			"including on SGLang")

		// The saturation analyzer's floor prices mu from generated tokens over
		// the shape's output length, so this is as load-bearing for it as
		// lambda is, and as invisible when it is missing: left registered only
		// with the opt-in throughput analyzer, the field was structurally zero
		// and the floor recorded nothing for a whole benchmark run -- 21
		// saturated cycles, no window, no floor, measured 2026-09-22.
		Expect(ql.Get(QueryGenerationTokenRate)).NotTo(BeNil(),
			"mu's source must not depend on the throughput analyzer either")
		Expect(ql.Get(EngineQuery(inferenceengine.EngineSGLang, QueryGenerationTokenRate))).NotTo(BeNil(),
			"including on SGLang")
	})

	It("does not double-register when the throughput analyzer is also enabled", func() {
		// MustRegister panics on a duplicate name, so this is the failure mode of
		// splitting the registration carelessly: fine by default, dead on startup
		// for anyone who turns the throughput analyzer on.
		reg := source.NewSourceRegistry()
		Expect(reg.Register("prometheus", prometheus.NewPrometheusSource(
			context.Background(), &mockPrometheusAPI{}, prometheus.DefaultPrometheusSourceConfig()))).To(Succeed())
		Expect(func() {
			RegisterArrivalRateQueries(reg)
			RegisterThroughputAnalyzerQueries(reg)
		}).NotTo(Panic())
	})
})
