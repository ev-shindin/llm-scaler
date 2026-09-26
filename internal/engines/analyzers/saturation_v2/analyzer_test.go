package saturation_v2

import (
	"time"

	"context"
	"math"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

var _ = Describe("SaturationAnalyzer", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *capacity.Store
		ctx      context.Context
	)

	BeforeEach(func() {
		store = capacity.NewStore()
		analyzer = NewSaturationAnalyzer(store)
		ctx = context.Background()
	})

	Describe("Name", func() {
		It("should return 'saturation-token-based'", func() {
			Expect(analyzer.Name()).To(Equal("saturation-token-based"))
		})
	})

	Describe("k1/k2 interaction", func() {
		It("should use k1 (memory-bound) when k2 is unknown", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						5000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			// k1 = 16000 * 0.8 = 12800, k2 = k1 (fallback, queue < threshold)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})

		It("should use k2 (compute-bound) when queue is saturated", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					// Queue >= threshold (5), tokensInUse = 8000
					makeReplicaMetrics("pod-1", "variant-a",
						8000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// k1 = 12800, k2 = 8000 (observed: tokensInUse when queue saturated)
			// effective = min(12800, 8000) = 8000
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(8000)))
		})

		It("should detect compute-bound when k2 < k1 with high queue and low KV", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					// Low KV usage but saturated queue
					makeReplicaMetrics("pod-1", "variant-a",
						4000, 16000, 10, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// k1 = 12800, k2 = 4000 (observed), effective = 4000
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(4000)))
		})

		It("should discard an observed k2 that exceeds k1 as implausible", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					// Queue saturated, but tokensInUse (20000) exceeds k1
					// (12800) -- physically impossible for real KV
					// occupancy, so P1-obs must not win.
					makeReplicaMetrics("pod-1", "variant-a",
						20000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// No history and no derivable engine params, so it falls all the
			// way through to Priority 4 (k1 fallback) rather than using the
			// implausible 20000 observation.
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})
	})

	Describe("k2 history", func() {
		It("should store k2 observation in rolling average", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						8000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			_, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Verify k2 was stored in history
			histKey := "test-model|H100|1|both|short|q5"
			ra, ok := analyzer.computeCapacityHistory[histKey]
			Expect(ok).To(BeTrue())
			Expect(ra.Average()).To(Equal(float64(8000)))
		})

		It("should dilute a single outlier observation to 1/N of the rolling average", func() {
			// Nine steady observations at 8000, then one outlier at 3000 (e.g. a
			// pod's admitted batch shrinking for a cycle while its wait queue
			// stays saturated -- the exact churn that motivated returning the
			// blended average instead of the raw sample). The window size is 10,
			// so this fills it exactly with no eviction.
			for i := 0; i < 9; i++ {
				input := makeAnalyzerInput(
					[]domain.ReplicaMetrics{
						makeReplicaMetrics("pod-1", "variant-a",
							8000, 16000, 6, 100, 50),
					},
					[]domain.VariantReplicaState{
						{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
					},
				)
				_, err := analyzer.Analyze(ctx, input)
				Expect(err).NotTo(HaveOccurred())
			}

			outlierInput := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						3000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			result, err := analyzer.Analyze(ctx, outlierInput)
			Expect(err).NotTo(HaveOccurred())
			// (9*8000 + 3000) / 10 = 7500, not the raw 3000 observation.
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(7500)))
			// The number alone cannot show this came from P1: Add() has already
			// run by the time Priority 2 would read the window, so a silent
			// fall-through returns the same 7500. Only the reason distinguishes
			// them, and the reason is the sole observable difference between the
			// two paths now that both return the same average.
			Expect(result.VariantCapacities[0].Reason).To(Equal(capacity.K2SrcObserved.String()))
		})

		It("should converge on a sustained shift once the window turns over", func() {
			// The comment on Priority 1 justifies smoothing by claiming a real,
			// sustained shift still dominates the average. That claim is what
			// makes the 1/N outlier weight acceptable, so it is pinned here.
			//
			// One replica, so exactly one sample enters the window per cycle and
			// the arithmetic is the window itself rather than a function of
			// replica count.
			steady := func(tokens int64) {
				input := makeAnalyzerInput(
					[]domain.ReplicaMetrics{
						makeReplicaMetrics("pod-1", "variant-a",
							tokens, 16000, 6, 100, 50),
					},
					[]domain.VariantReplicaState{
						{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
					},
				)
				_, err := analyzer.Analyze(ctx, input)
				Expect(err).NotTo(HaveOccurred())
			}

			// Fill the window at 8000.
			for i := 0; i < capacity.RollingAverageWindowSize; i++ {
				steady(8000)
			}

			// Step down to 3000 and hold. Each cycle replaces one 8000 with a
			// 3000, so the average walks down 500 per cycle: 7500, 7000, ...
			for i := 1; i < capacity.RollingAverageWindowSize; i++ {
				steady(3000)
			}

			// After N-1 post-step samples the window holds one 8000 and nine
			// 3000s: (8000 + 9*3000) / 10 = 3500.
			final := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						3000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			result, err := analyzer.Analyze(ctx, final)
			Expect(err).NotTo(HaveOccurred())
			// The window is now entirely 3000: the shift has fully propagated,
			// and it took a full window to do it.
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(3000)))
			Expect(result.VariantCapacities[0].Reason).To(Equal(capacity.K2SrcObserved.String()))
		})

		It("should not dilute a fresh observation against a stale window", func() {
			// A variant that goes quiet keeps its window: samples are evicted by
			// count, never by age, and EvictStaleHistory has no caller on the
			// reconcile path. Blending a first observation back into that window
			// would size current capacity from behaviour before the gap, at 9/10
			// weight -- an exposure the raw return did not have.
			steady := func(tokens int64) *domain.AnalyzerResult {
				input := makeAnalyzerInput(
					[]domain.ReplicaMetrics{
						makeReplicaMetrics("pod-1", "variant-a",
							tokens, 16000, 6, 100, 50),
					},
					[]domain.VariantReplicaState{
						{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
					},
				)
				res, err := analyzer.Analyze(ctx, input)
				Expect(err).NotTo(HaveOccurred())
				return res
			}

			for i := 0; i < capacity.RollingAverageWindowSize; i++ {
				steady(8000)
			}

			// Age every window past the eviction timeout, as a quiet period would.
			analyzer.mu.Lock()
			Expect(analyzer.computeCapacityHistory).NotTo(BeEmpty())
			for _, ra := range analyzer.computeCapacityHistory {
				ra.TouchAt(time.Now().Add(-2 * capacity.HistoryEvictionTimeout))
			}
			analyzer.mu.Unlock()

			// The window is discarded, so this observation stands alone rather
			// than being blended to (9*8000 + 3000)/10 = 7500.
			result := steady(3000)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(3000)))
			Expect(result.VariantCapacities[0].Reason).To(Equal(capacity.K2SrcObserved.String()))
		})

		It("should keep an observation between k1 and the physical KV ceiling", func() {
			// k1 is 0.80 x 16000 = 12800, so 14000 tokens in use is a replica at
			// 87.5% occupancy -- legitimate, and with the queue saturated it is the
			// most informative reading available. Only a value above the physical
			// ceiling (16000) is a scrape artifact.
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						14000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			ra, ok := analyzer.computeCapacityHistory["test-model|H100|1|both|short|q5"]
			Expect(ok).To(BeTrue(), "a legitimate high-occupancy reading must seed history")
			Expect(ra.Average()).To(Equal(float64(14000)))
			// k1 still bounds what the analyzer reports this cycle.
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})

		It("should not store an observation above the physical KV ceiling in history", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						20000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			_, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			histKey := "test-model|H100|1|both|short|q5"
			_, ok := analyzer.computeCapacityHistory[histKey]
			Expect(ok).To(BeFalse())
		})

		It("should use historical k2 when queue drops below threshold", func() {
			// First call: queue saturated → observes k2
			input1 := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						8000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			_, err := analyzer.Analyze(ctx, input1)
			Expect(err).NotTo(HaveOccurred())

			// Second call: queue below threshold → uses historical k2
			input2 := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						6000, 16000, 2, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			result, err := analyzer.Analyze(ctx, input2)
			Expect(err).NotTo(HaveOccurred())
			// Should use historical k2=8000, not fallback to k1=12800
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(8000)))
		})
	})

	Describe("Output-length bucketing", func() {
		It("should use different k2 history buckets for different output lengths", func() {
			// Short output workload: k2 observation
			input1 := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						8000, 16000, 6, 100, 50), // avgOutput=50 → "short"
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			_, _ = analyzer.Analyze(ctx, input1)

			// Long output workload: no history yet
			input2 := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						6000, 16000, 2, 100, 600), // avgOutput=600 → "long"
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			result, err := analyzer.Analyze(ctx, input2)
			Expect(err).NotTo(HaveOccurred())
			// No "long" history → falls back to k1=12800 (not the "short" k2=8000)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})
	})

	Describe("History key stability", func() {
		It("keeps using a variant's history when the accelerator stops resolving", func() {
			// Accelerator discovery is intermittent for a workload that pins no GPU
			// product: the type is deduced on a single-accelerator cluster and the
			// deduction does not hold every cycle. The accelerator is part of the
			// history key, so a blip must not move the variant to a fresh bucket --
			// that loses k2, falls back to k1, and (measured on kind) flips capacity
			// 2 <-> 8 on alternating cycles while the fleet oscillates 1 <-> 2.
			resolved := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", 8000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			_, err := analyzer.Analyze(ctx, resolved)
			Expect(err).NotTo(HaveOccurred())
			ra, ok := analyzer.computeCapacityHistory["test-model|H100|1|both|short|q5"]
			Expect(ok).To(BeTrue())
			Expect(ra.Average()).To(Equal(float64(8000)))

			// Same variant, same everything -- except discovery could not name the
			// accelerator this cycle.
			unresolved := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", 9000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "unknown", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			_, err = analyzer.Analyze(ctx, unresolved)
			Expect(err).NotTo(HaveOccurred())

			_, spurious := analyzer.computeCapacityHistory["test-model|unknown|1|both|short|q5"]
			Expect(spurious).To(BeFalse(),
				"an unresolved accelerator opened a second history bucket; this is the oscillation")
			Expect(ra.Average()).To(Equal(float64(8500)),
				"the unresolved cycle must land in the variant's existing bucket")
		})

		It("ages the accelerator memo out with the history it keys", func() {
			// The memo is per-variant state with no other owner, so without this it
			// outlives every variant that ever existed -- the unbounded-map failure
			// EvictStaleHistory exists to prevent for the history beside it.
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", 8000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			_, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(analyzer.lastAccelerator).To(HaveLen(1))

			// A live variant keeps its memo.
			analyzer.EvictStaleHistory(time.Hour)
			Expect(analyzer.lastAccelerator).To(HaveLen(1),
				"a memo younger than the timeout was evicted")

			// One that has gone quiet loses it, on the same sweep as its history.
			analyzer.EvictStaleHistory(0)
			Expect(analyzer.lastAccelerator).To(BeEmpty(),
				"the memo outlived the history it keys, so nothing ever frees it")
			Expect(analyzer.computeCapacityHistory).To(BeEmpty())
		})

		It("does not reuse k2 learned under a different queue threshold", func() {
			// P1 records k2 as the occupancy seen WHEN THE QUEUE WAS SATURATED, so
			// the reading only means anything relative to the threshold that
			// defined saturation. Nothing else invalidates history: EvictStaleHistory
			// is age-based. Measured: a k2 of 2 learned under a low threshold held a
			// variant at utilization 1.0 under a threshold of 100, where P1 could
			// not fire at all, and the variant scaled up every other cycle.
			seed := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", 2000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			_, err := analyzer.Analyze(ctx, seed)
			Expect(err).NotTo(HaveOccurred())
			_, ok := analyzer.computeCapacityHistory["test-model|H100|1|both|short|q5"]
			Expect(ok).To(BeTrue())

			// The operator retunes the threshold. The old observation is not a
			// capacity estimate under the new one.
			retuned := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", 2000, 16000, 6, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			retuned.Config.(*config.ScalingPolicy).QueueLengthThreshold = 100

			result, err := analyzer.Analyze(ctx, retuned)
			Expect(err).NotTo(HaveOccurred())
			// k1 = 0.8 x 16000 = 12800. Reusing the old k2 would report 2000.
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)),
				"k2 recorded under threshold 5 was reused under threshold 100")
		})
	})

	Describe("Role-scoped history", func() {
		It("should not let one role's P1-obs seed another role's history bucket", func() {
			// Both variants land in the same "short" output bucket (avgOutput=50
			// for both), which is exactly the collision this fix targets: a
			// prefill variant (permanently ~0 avgOutput in real traffic) and a
			// decode variant that happens to be at "short" too (e.g. a cold
			// replica before it's served real traffic).
			//
			// Saturated on separate cycles: a prefill saturation is recorded
			// only while decode is not saturated (computeK2), so one cycle with
			// both over the threshold would seed decode's history alone.
			states := []domain.VariantReplicaState{
				{VariantName: "variant-p", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill},
				{VariantName: "variant-d", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
			}
			_, err := analyzer.Analyze(ctx, makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-p", "variant-p", 8000, 16000, 6, 100, 50),
					makeReplicaMetrics("pod-d", "variant-d", 3000, 16000, 0, 100, 50),
				}, states))
			Expect(err).NotTo(HaveOccurred())
			_, err = analyzer.Analyze(ctx, makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-p", "variant-p", 8000, 16000, 0, 100, 50),
					makeReplicaMetrics("pod-d", "variant-d", 3000, 16000, 6, 100, 50),
				}, states))
			Expect(err).NotTo(HaveOccurred())

			prefillHist, ok := analyzer.computeCapacityHistory["test-model|H100|1|prefill|short|q5"]
			Expect(ok).To(BeTrue())
			Expect(prefillHist.Average()).To(Equal(float64(8000)))

			decodeHist, ok := analyzer.computeCapacityHistory["test-model|H100|1|decode|short|q5"]
			Expect(ok).To(BeTrue())
			Expect(decodeHist.Average()).To(Equal(float64(3000)))

			// Pre-fix, both observations landed in the single shared
			// "test-model|H100|1|short|q5" key and averaged together.
			_, ok = analyzer.computeCapacityHistory["test-model|H100|1|short|q5"]
			Expect(ok).To(BeFalse())
		})
	})

	Describe("k2 derivation from deployment params", func() {
		It("should derive k2 from chunked prefill params", func() {
			// Pre-populate store with deployment params for this variant
			store.Update("test-ns", "test-model", "variant-a", capacity.Record{
				GpuCount: 1,
				EngineParams: &capacity.EngineParams{
					EffectiveMaxBatchedTokens: 2048,
					MaxNumSeqs:                256,
					ChunkedPrefillEnabled:     true,
				},
				LearnedFrom: "deployment",
			})

			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					// Queue below threshold, no history → uses derived k2
					makeReplicaMetrics("pod-1", "variant-a",
						5000, 16000, 2, 500, 100), // I=500, O=100
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// B=2048, S=256, I=500, O=100
			// N_steady = min(2048*100/(500+100), 256) = min(341.3, 256) = 256
			// k2 = 256 * (500 + 100/2) = 256 * 550 = 140800
			// k1 = 12800, effective = min(12800, 140800) = 12800
			// k2 > k1, so memory-bound
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})

		It("should fall back to k1 when no batch/queue/history data exists", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						5000, 16000, 0, 0, 0), // no avg tokens
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// k2 derivation needs avgOutput > 0, falls back to k1
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})

		It("should use the derived k2 for a decode-role variant when it is below k1", func() {
			store.Update("test-ns", "test-model", "variant-d", capacity.Record{
				GpuCount: 1,
				EngineParams: &capacity.EngineParams{
					EffectiveMaxBatchedTokens: 2048,
					MaxNumSeqs:                10,
					ChunkedPrefillEnabled:     true,
				},
				LearnedFrom: "deployment",
			})

			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					// Queue below threshold, no history → uses derived k2.
					makeReplicaMetrics("pod-1", "variant-d",
						100, 16000, 0, 100, 1000), // I=100, O=1000
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-d", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// B=2048, S=10, I=100, O=1000
			// N_steady = min(2048*1000/1100, 10) = 10
			// k2 = 10 * (100 + 1000/2) = 10 * 600 = 6000
			// k1 = 12800, effective = min(12800, 6000) = 6000 (compute-bound)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(6000)))
		})

		It("should skip the derived formula for a prefill-role variant and fall back to k1", func() {
			// Identical EngineParams/inputs to the decode case above -- the
			// only difference is Role. A prefill vLLM instance reports
			// avgOutput~0-1 in real traffic (it hands off to decode before
			// generating anything); avgOutput=1000 here stands in for
			// whatever this replica happens to report, to isolate that the
			// skip is driven by role, not by the input shape.
			//
			// Priority 3 is skipped for prefill entirely rather than given
			// its own derivation: the batch-token budget alone (tried in an
			// earlier revision, reported directly as its own priority tier)
			// is not comparable to demand -- every other capacity figure in
			// this chain is a product scaled to demand's order of magnitude
			// (k1 is capacity x threshold, decode's own P3 is concurrency x
			// footprint), while the raw per-step budget has no such scaling.
			// It disagreed with this replica's own directly observed
			// behavior on a real benchmark: at EffectiveMaxBatchedTokens=
			// 65536, vllm:num_requests_waiting read 0 across an entire run,
			// yet the budget-only figure read as 5-10x under water against
			// demand snapshots in the hundreds of thousands. k1 over-states
			// capacity in the other direction, but at least it bounds
			// something real (the KV cache this replica actually has).
			store.Update("test-ns", "test-model", "variant-p", capacity.Record{
				GpuCount: 1,
				EngineParams: &capacity.EngineParams{
					EffectiveMaxBatchedTokens: 2048,
					MaxNumSeqs:                10,
					ChunkedPrefillEnabled:     true,
				},
				LearnedFrom: "deployment",
			})

			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-p",
						100, 16000, 0, 100, 1000),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-p", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// Priority 3 skipped for prefill -> Priority 4 (k1 fallback),
			// not the 6000 a decode-role variant would get from the same
			// EngineParams/inputs.
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12800)))
		})
	})

	Describe("Pending replicas", func() {
		It("should include pending replicas in anticipated supply for scale-up", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						10000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 2, PendingReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// readyCount = 2 - 1 = 1; anticipated = (1 + 1) × PRC
			// RC/SC are not on the analyzer contract at all; the engine's
			// capacity-build step derives them from this (D, P).
			Expect(aggregation.SumTotalAnticipatedSupply(result.VariantCapacities)).To(BeNumerically(">", aggregation.SumTotalSupply(result.VariantCapacities)))
		})

		It("counts a Ready replica that has not been scraped yet as arriving, not as absent", func() {
			// From the shape-swap P/D run at 07:30:23: the decode target reported
			// 4 replicas with 2 Ready, and one metrics row -- the second Ready
			// pod passed its probe one second before the cycle. The target's own
			// pending count (4 - 2 = 2) excludes it and the live rows exclude it,
			// so anticipated supply counted 3 replicas and the role was sized to
			// 7 instead of 6.
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("decode-0", "decode-v", 1_149_823, 1_162_240, 137, 6000, 1000),
				},
				[]domain.VariantReplicaState{
					{VariantName: "decode-v", Role: domain.RoleDecode, AcceleratorName: "H200",
						CurrentReplicas: 4, PendingReplicas: 2, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			vc := result.VariantCapacities[0]
			Expect(vc.ReplicaCount).To(Equal(1), "supply is what reported")
			Expect(vc.PendingReplicas).To(Equal(3), "everything else the target owns is arriving")
			Expect(aggregation.SumTotalAnticipatedSupply(result.VariantCapacities)).
				To(BeNumerically("~", 4*vc.PerReplicaCapacity, 1e-6),
					"anticipated supply must be the whole fleet the target already has")
		})

		It("reports no pending replicas while condemned replicas outnumber the target", func() {
			// An in-flight scale-down: the target is at 1 with 3 rows still
			// reporting. The engine's clamp caps supply at the target's count;
			// pending must be zero here, not the target's stale not-ready figure
			// -- which is set to a nonzero value so that the old code (pending =
			// vs.PendingReplicas) and the new one give different answers.
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a", 1000, 16000, 0, 100, 50),
					makeReplicaMetrics("pod-2", "variant-a", 1000, 16000, 0, 100, 50),
					makeReplicaMetrics("pod-3", "variant-a", 1000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, PendingReplicas: 5, GPUsPerReplica: 1},
				},
			)
			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities[0].PendingReplicas).To(BeZero())
		})

		It("should NOT include pending replicas in scale-down calculation", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						1000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 3, PendingReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// TotalSupply uses ready replicas only; TotalAnticipatedSupply includes pending.
			// RC/SC are not on the analyzer contract at all; the engine's
			// capacity-build step derives them from this (D, P).
			Expect(aggregation.SumTotalAnticipatedSupply(result.VariantCapacities)).To(BeNumerically(">", aggregation.SumTotalSupply(result.VariantCapacities)))
		})
	})

	Describe("Zero-replica variants", func() {
		It("should use stored live capacity directly when variant has zero replicas", func() {
			store.Update("test-ns", "test-model", "variant-a", capacity.Record{
				GpuCount:          1,
				EffectiveCapacity: 12000,
				LearnedFrom:       "live",
			})

			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{}, // no pods
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			// Live record → uses stored EffectiveCapacity directly
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(12000)))
		})

		It("should derive capacity from params + workload for deployment-derived records", func() {
			// Zero-replica variant with deployment-derived params only
			store.Update("test-ns", "test-model", "variant-b", capacity.Record{
				GpuCount:          1,
				EffectiveCapacity: 8192, // conservative fallback from LoadFromDeployment
				EngineParams: &capacity.EngineParams{
					EffectiveMaxBatchedTokens: 8192,
					MaxNumSeqs:                256,
				},
				LearnedFrom: "deployment",
			})

			// Another variant has live pods providing workload data
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						5000, 16000, 0, 500, 100), // I=500, O=100
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
					{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(2))

			// variant-b: deployment-derived with workload data available
			// B=8192, S=256, I=500, O=100
			// N_steady = min(8192*100/(500+100), 256) = min(1365.3, 256) = 256
			// k2_derived = 256 * (500 + 100/2) = 256 * 550 = 140800
			// Much better than the 8192 fallback!
			varB := result.VariantCapacities[1]
			Expect(varB.VariantName).To(Equal("variant-b"))
			Expect(varB.PerReplicaCapacity).To(Equal(float64(140800)))
		})

		It("should bound k2 estimate by compatible variant's live EffectiveCapacity", func() {
			// variant-b is a new deployment on the same H100 hardware as variant-a
			defaultParams := &capacity.EngineParams{
				GpuMemoryUtilization:      0.9,
				BlockSize:                 16,
				KvCacheDtype:              "auto",
				TensorParallelSize:        1,
				MaxNumSeqs:                256,
				EffectiveMaxBatchedTokens: 8192,
			}
			store.Update("test-ns", "test-model", "variant-b", capacity.Record{
				GpuCount:          1,
				EffectiveCapacity: 8192,
				EngineParams:      defaultParams,
				LearnedFrom:       "deployment",
			})

			// variant-a has a compatible live record (same accel, GPU count, params)
			store.Update("test-ns", "test-model", "variant-a", capacity.Record{
				GpuCount:              1,
				TotalKvCapacityTokens: 50000,
				EffectiveCapacity:     40000, // observed min(k1, k2) = 40000
				EngineParams:          defaultParams,
				LearnedFrom:           "live",
			})

			// variant-a provides workload data
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						5000, 50000, 0, 500, 100), // I=500, O=100
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
					{VariantName: "variant-b", AcceleratorName: "H100", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// k2_derived = 256 * 550 = 140800, but bounded by variant-a's live
			// EffectiveCapacity = 40000 → result = min(140800, 40000) = 40000
			varB := result.VariantCapacities[1]
			Expect(varB.VariantName).To(Equal("variant-b"))
			Expect(varB.PerReplicaCapacity).To(Equal(float64(40000)))
		})

		It("should bound k2 estimate by own k1 when TotalKvCapacityTokens is known", func() {
			store.Update("test-ns", "test-model", "variant-a", capacity.Record{
				GpuCount:              1,
				EffectiveCapacity:     8192,
				TotalKvCapacityTokens: 30000, // from num_gpu_blocks_override
				EngineParams: &capacity.EngineParams{
					EffectiveMaxBatchedTokens: 8192,
					MaxNumSeqs:                256,
					NumGpuBlocksOverride:      1875,
					BlockSize:                 16,
				},
				LearnedFrom: "deployment",
			})

			// Another variant provides workload data. Its accelerator differs from
			// variant-a's, so FindCompatible must not match it — the accelerator is
			// stated on the variant state because that is where discovery puts it.
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-x",
						5000, 16000, 0, 500, 100),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-x", AcceleratorName: "L40S", CurrentReplicas: 1, GPUsPerReplica: 1},
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// k2_derived = 256 * 550 = 140800
			// k1 = 30000 * 0.8 (KvCacheThreshold) = 24000
			// No compatible live record (A100 vs L40S) → bounded by k1 only
			// result = min(140800, 24000) = 24000
			varA := result.VariantCapacities[1]
			Expect(varA.VariantName).To(Equal("variant-a"))
			Expect(varA.PerReplicaCapacity).To(Equal(float64(24000)))
		})

		It("should use EffectiveMaxBatchedTokens fallback when no workload data exists", func() {
			// Zero-replica variant with deployment-derived params, no other live pods
			store.Update("test-ns", "test-model", "variant-a", capacity.Record{
				GpuCount:          1,
				EffectiveCapacity: 8192,
				EngineParams: &capacity.EngineParams{
					EffectiveMaxBatchedTokens: 8192,
					MaxNumSeqs:                256,
				},
				LearnedFrom: "deployment",
			})

			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{}, // no live pods at all
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 0, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			// No workload data → falls back to stored EffectiveCapacity (8192)
			Expect(result.VariantCapacities[0].PerReplicaCapacity).To(Equal(float64(8192)))
		})
	})

	Describe("estimateCapacityFromParams", func() {
		It("should compute k2 from B, S, I, O", func() {
			params := &capacity.EngineParams{
				EffectiveMaxBatchedTokens: 4096,
				MaxNumSeqs:                256,
			}
			// B=4096, S=256, I=500, O=100
			// N_steady = min(4096*100/600, 256) = min(682.6, 256) = 256
			// k2 = 256 * (500 + 50) = 256 * 550 = 140800
			Expect(estimateCapacityFromParams(params, 500, 100)).To(Equal(int64(140800)))
		})

		It("should cap N_steady at MaxNumSeqs", func() {
			params := &capacity.EngineParams{
				EffectiveMaxBatchedTokens: 8192,
				MaxNumSeqs:                64,
			}
			// B=8192, S=64, I=100, O=200
			// N_steady = min(8192*200/300, 64) = min(5461, 64) = 64
			// k2 = 64 * (100 + 100) = 64 * 200 = 12800
			Expect(estimateCapacityFromParams(params, 100, 200)).To(Equal(int64(12800)))
		})

		It("should return 0 when avgOutput is 0", func() {
			params := &capacity.EngineParams{
				EffectiveMaxBatchedTokens: 8192,
				MaxNumSeqs:                256,
			}
			Expect(estimateCapacityFromParams(params, 500, 0)).To(Equal(int64(0)))
		})

		It("should return 0 when params is nil", func() {
			Expect(estimateCapacityFromParams(nil, 500, 100)).To(Equal(int64(0)))
		})
	})

	Describe("Scaling signals", func() {
		It("should signal scale-up when demand exceeds threshold", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						11000, 16000, 3, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// demand is high relative to supply — TotalDemand > TotalAnticipatedSupply.
			// The analyzer emits only (D, P), so compare against the supply the
			// engine's capacity-build step derives from the same variant capacities;
			// that comparison is exactly what makes the engine's RC positive.
			Expect(result.TotalDemand).To(BeNumerically(">",
				aggregation.SumTotalAnticipatedSupply(result.VariantCapacities)*0.85))
		})

		It("should signal scale-down when utilization is below boundary", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						1000, 16000, 0, 100, 50),
					makeReplicaMetrics("pod-2", "variant-a",
						1000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 2, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// Very low utilization — TotalSupply well above TotalDemand/scaleDown,
			// which is what makes the engine's SC positive.
			Expect(aggregation.SumTotalSupply(result.VariantCapacities)).To(BeNumerically(">", result.TotalDemand/0.70))
		})

		It("should signal steady state when utilization is between thresholds", func() {
			// Supply ~ demand / 0.77 (between 0.70 boundary and 0.85 threshold)
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						10000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// utilization = 10000/12800 = 0.78 — between the 0.70 boundary and the
			// 0.85 threshold, so the engine derives RC = SC = 0 from this (D, P).
			// Asserted on the derived utilization because the analyzer emits neither.
			supply := aggregation.SumTotalSupply(result.VariantCapacities)
			Expect(supply).To(BeNumerically(">", 0))
			Expect(result.TotalDemand / supply).To(BeNumerically("~", 0.78, 0.01))
		})
	})

	Describe("Scheduler queue demand", func() {
		It("should add scheduler queue demand to total demand", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						5000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			input.SchedulerQueue = &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 8000,
			}

			resultWithQueue, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Without scheduler queue
			input.SchedulerQueue = nil
			// Reset analyzer for clean comparison
			analyzer2 := NewSaturationAnalyzer(store)
			resultWithout, err := analyzer2.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			Expect(resultWithQueue.TotalDemand).To(BeNumerically(">", resultWithout.TotalDemand))
		})

		It("should not add demand when scheduler queue is nil", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						5000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			// Total demand = just the replica demand (tokensInUse)
			Expect(result.TotalDemand).To(Equal(float64(5000)))
		})

		It("should reduce input tokens by prefix cache hit rate", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					{
						PodName: "pod-1", VariantName: "variant-a",
						TokensInUse: 5000, TotalKvCapacityTokens: 16000,
						NumGpuBlocks: 1000, BlockSize: 16,
						AvgInputTokens: 100, AvgOutputTokens: 50,
						PrefixCacheHitRate: 0.5, // 50% cache hit rate
					},
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			input.SchedulerQueue = &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 4000, // 4000/4 = 1000 tokens from bytes
			}

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Input from count: 10 * 100 = 1000 tokens
			// Input from bytes: 4000 / 4 = 1000 tokens
			// max(1000, 1000) = 1000
			// After cache hit: 1000 * (1 - 0.5) = 500
			// Output: 10 * 50 = 500
			// Scheduler demand = 500 + 500 = 1000
			// Replica demand = 5000
			// Total = 5000 + 1000 = 6000
			Expect(result.TotalDemand).To(Equal(float64(6000)))
		})

		It("should use max of bytes and count estimates for input tokens", func() {
			input := makeAnalyzerInput(
				[]domain.ReplicaMetrics{
					makeReplicaMetrics("pod-1", "variant-a",
						5000, 16000, 0, 100, 50),
				},
				[]domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
				},
			)
			input.SchedulerQueue = &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 20000, // 20000/4 = 5000 >> 10*100=1000
			}

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())

			// Bytes estimate (5000) > count estimate (1000), so bytes wins
			// Input = 5000 * (1-0) = 5000 (no cache hit)
			// Output = 10 * 50 = 500
			// Scheduler demand = 5500
			// Total = 5000 + 5500 = 10500
			Expect(result.TotalDemand).To(Equal(float64(10500)))
		})
	})

	Describe("Scheduler queue demand role attribution", func() {
		It("should attribute inputTokens to prefill and inputTokens+outputTokens to decode", func() {
			metrics := []domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a",
					5000, 16000, 0, 100, 50),
			}
			activeRoles := map[string]bool{"prefill": true, "decode": true}
			sq := &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0, // use count-based estimation only
			}

			result := estimateSchedulerQueueDemand(sq, metrics, nil, activeRoles)

			// Input: max(0/4=0, 10*100=1000) = 1000 (no cache hit)
			// Output: 10 * 50 = 500
			// Total: 1000 + 500 = 1500
			Expect(result.total).To(Equal(1500.0))

			// Prefill gets inputTokens only
			Expect(result.byRole["prefill"]).To(Equal(1000.0))
			// Decode gets inputTokens + outputTokens
			Expect(result.byRole["decode"]).To(Equal(1500.0))
		})

		It("should attribute full demand to 'both' role", func() {
			metrics := []domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a",
					5000, 16000, 0, 100, 50),
			}
			activeRoles := map[string]bool{"both": true}
			sq := &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0,
			}

			result := estimateSchedulerQueueDemand(sq, metrics, nil, activeRoles)

			Expect(result.total).To(Equal(1500.0))
			Expect(result.byRole["both"]).To(Equal(1500.0))
		})

		It("should return empty byRole when nil activeRoles", func() {
			metrics := []domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a",
					5000, 16000, 0, 100, 50),
			}
			sq := &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0,
			}

			result := estimateSchedulerQueueDemand(sq, metrics, nil, nil)

			Expect(result.total).To(Equal(1500.0))
			Expect(result.byRole).To(BeEmpty())
		})

		It("should return zero for nil scheduler queue", func() {
			metrics := []domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a",
					5000, 16000, 0, 100, 50),
			}
			activeRoles := map[string]bool{"prefill": true, "decode": true}

			result := estimateSchedulerQueueDemand(nil, metrics, nil, activeRoles)

			Expect(result.total).To(Equal(0.0))
			Expect(result.byRole).To(BeNil())
		})

		It("should apply prefix cache hit rate to per-role input tokens", func() {
			metrics := []domain.ReplicaMetrics{
				{
					PodName: "pod-1", VariantName: "variant-a",
					TokensInUse: 5000, TotalKvCapacityTokens: 16000,
					NumGpuBlocks: 1000, BlockSize: 16,
					AvgInputTokens: 100, AvgOutputTokens: 50,
					PrefixCacheHitRate: 0.5, // 50% cache hit rate
				},
			}
			activeRoles := map[string]bool{"prefill": true, "decode": true}
			sq := &domain.SchedulerQueueMetrics{
				QueueSize:  10,
				QueueBytes: 0,
			}

			result := estimateSchedulerQueueDemand(sq, metrics, nil, activeRoles)

			// Input: 10*100=1000, after cache: 1000*(1-0.5)=500
			// Output: 10*50=500
			// Total: 500 + 500 = 1000
			Expect(result.total).To(Equal(1000.0))

			// Prefill: inputTokens = 500
			Expect(result.byRole["prefill"]).To(Equal(500.0))
			// Decode: inputTokens + outputTokens = 500 + 500 = 1000
			Expect(result.byRole["decode"]).To(Equal(1000.0))
		})

		It("prices a queued request's output from the decode side, not diluted by prefill", func() {
			// From a P/D run (shape 6000 in / 1000 out, one replica per role):
			// the prefill replica's own generation average is ~1 because it
			// hands off after one token. Averaged unweighted with decode's
			// 1000, the model-level output length read 500 and 213 queued
			// requests were charged half their real output.
			metrics := []domain.ReplicaMetrics{
				makeReplicaMetrics("prefill-0", "prefill-v", 0, 1149312, 0, 6000, 1),
				makeReplicaMetrics("decode-0", "decode-v", 1158912, 1162240, 0, 6000, 1000),
			}
			roles := map[string]string{"prefill-v": domain.RolePrefill, "decode-v": domain.RoleDecode}
			activeRoles := map[string]bool{domain.RolePrefill: true, domain.RoleDecode: true}
			sq := &domain.SchedulerQueueMetrics{QueueSize: 213}

			result := estimateSchedulerQueueDemand(sq, metrics, roles, activeRoles)

			// Input: 213 × 6000 from either side; output: 213 × 1000, decode's
			// figure alone. The old mean gave 213 × 500 = 106,500 here.
			Expect(result.byRole[domain.RolePrefill]).To(Equal(213.0 * 6000))
			Expect(result.byRole[domain.RoleDecode]).To(Equal(213.0*6000 + 213.0*1000))
			Expect(result.total).To(Equal(213.0*6000 + 213.0*1000))

			// Negative control: with no roles recorded the mean is the old
			// unweighted one, and reproduces the diluted charge from the run.
			_, diluted, _ := computeModelWorkloadAverages(metrics, nil)
			Expect(diluted).To(Equal(500.5))
		})

		It("averages over every replica when no role is recorded", func() {
			// A non-disaggregated fleet has no prefill to exclude, so the
			// role-aware mean must be the plain mean it always was.
			metrics := []domain.ReplicaMetrics{
				makeReplicaMetrics("pod-1", "variant-a", 5000, 16000, 0, 100, 50),
				makeReplicaMetrics("pod-2", "variant-a", 5000, 16000, 0, 100, 150),
			}
			avgIn, avgOut, _ := computeModelWorkloadAverages(metrics, nil)
			Expect(avgIn).To(Equal(100.0))
			Expect(avgOut).To(Equal(100.0))
		})

		It("keeps the input-token and hit-rate means over both roles", func() {
			// Only the output length is a property of the role. Prompts are the
			// same on both sides, and the prefix cache a queued prompt can hit
			// is the prefill side's, so those two must not lose it.
			metrics := []domain.ReplicaMetrics{
				{PodName: "prefill-0", VariantName: "prefill-v", AvgInputTokens: 6000, AvgOutputTokens: 1, PrefixCacheHitRate: 0.8},
				{PodName: "decode-0", VariantName: "decode-v", AvgInputTokens: 6000, AvgOutputTokens: 1000, PrefixCacheHitRate: 0},
			}
			roles := map[string]string{"prefill-v": domain.RolePrefill, "decode-v": domain.RoleDecode}
			avgIn, avgOut, hit := computeModelWorkloadAverages(metrics, roles)
			Expect(avgIn).To(Equal(6000.0))
			Expect(avgOut).To(Equal(1000.0))
			Expect(hit).To(Equal(0.4))
		})

		It("should add queue demand to role capacities in end-to-end analysis", func() {
			input := domain.AnalyzerInput{
				ModelID:   "test-model",
				Namespace: "test-ns",
				ReplicaMetrics: []domain.ReplicaMetrics{
					{
						PodName: "prefill-pod", VariantName: "prefill-v",
						TokensInUse: 3000, TotalKvCapacityTokens: 16000,
						NumGpuBlocks: 1000, BlockSize: 16,
						AvgInputTokens: 100, AvgOutputTokens: 50,
						ModelID: "test-model", Namespace: "test-ns",
					},
					{
						PodName: "decode-pod", VariantName: "decode-v",
						TokensInUse: 2000, TotalKvCapacityTokens: 16000,
						NumGpuBlocks: 1000, BlockSize: 16,
						AvgInputTokens: 100, AvgOutputTokens: 50,
						ModelID: "test-model", Namespace: "test-ns",
					},
				},
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "prefill-v", CurrentReplicas: 1, GPUsPerReplica: 1, Role: "prefill"},
					{VariantName: "decode-v", CurrentReplicas: 1, GPUsPerReplica: 1, Role: "decode"},
				},
				Config: &config.ScalingPolicy{
					KvCacheThreshold:     0.8,
					QueueLengthThreshold: 5,
					AnalyzerName:         "saturation",
					ScaleUpThreshold:     0.85,
					ScaleDownBoundary:    0.70,
				},
				SchedulerQueue: &domain.SchedulerQueueMetrics{
					QueueSize:  10,
					QueueBytes: 0,
				},
			}

			store := capacity.NewStore()
			a := NewSaturationAnalyzer(store)
			result, err := a.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RoleDemand).NotTo(BeNil())

			// Scheduler queue: input=max(0, 10*100)=1000, output=10*50=500
			// Decode role demand: replica(2000) + queue(1500) = 3500
			// Prefill role demand: replica(3000) only. Its 1000-token share of
			// the scheduler queue is attributed (see the log line) and then
			// dropped by the throughput model while prefill has no saturated
			// throughput: a prefill replica holds a prompt for its prefill
			// time plus the hand-off, and a queue at the scheduler is decode's
			// to drain (throughput_floor.go).
			Expect(result.RoleDemand["prefill"]).To(Equal(3000.0))
			Expect(result.RoleDemand["decode"]).To(Equal(3500.0))

			// Model-level total still uses inputTokens+outputTokens (1500)
			// Replica demand = 3000 + 2000 = 5000
			// Total = 5000 + 1500 = 6500
			Expect(result.TotalDemand).To(Equal(6500.0))
		})
	})

	Describe("median helper", func() {
		It("should return 0 for empty slice", func() {
			Expect(median([]int64{})).To(Equal(int64(0)))
		})

		It("should return the value for single element", func() {
			Expect(median([]int64{42})).To(Equal(int64(42)))
		})

		It("should return middle value for odd count", func() {
			Expect(median([]int64{1, 3, 5})).To(Equal(int64(3)))
		})

		It("should return average of middle two for even count", func() {
			Expect(median([]int64{1, 3, 5, 7})).To(Equal(int64(4)))
		})

		It("should handle unsorted input", func() {
			Expect(median([]int64{5, 1, 3})).To(Equal(int64(3)))
		})
	})
})

// makeAnalyzerInput creates a standard AnalyzerInput with default config.
var _ = Describe("fleetOutputLength", func() {
	roles := map[string]string{"d": domain.RoleDecode, "p": domain.RolePrefill}
	It("weights each replica's average output by its request rate, so a fresh replica barely moves it", func() {
		// Three replicas serving ~3400-token outputs at 1.4 req/s and two
		// fresh ones whose handful of completions average 900 at 0.2 req/s:
		// the plain mean, 2400, is `xlong`; the rate-weighted one, ~3180,
		// is `xxlong` -- the shape the fleet is actually serving.
		rms := []domain.ReplicaMetrics{
			{VariantName: "d", AvgOutputTokens: 3400, RequestRate: 1.4},
			{VariantName: "d", AvgOutputTokens: 3400, RequestRate: 1.4},
			{VariantName: "d", AvgOutputTokens: 3400, RequestRate: 1.4},
			{VariantName: "d", AvgOutputTokens: 900, RequestRate: 0.2},
			{VariantName: "d", AvgOutputTokens: 900, RequestRate: 0.2},
			{VariantName: "p", AvgOutputTokens: 1, RequestRate: 6}, // prefill: not a generating replica
		}
		got := fleetOutputLength(rms, roles)
		Expect(got).To(BeNumerically("~", (3*1.4*3400+2*0.2*900)/(3*1.4+2*0.2), 1e-9))
		Expect(classifyOutputLength(got)).To(Equal("xxlong"))
		Expect(classifyOutputLength(2400)).To(Equal("xlong"), "the plain mean would have landed one bucket short")
	})
	It("falls back to the plain mean without rates, and to zero without completions", func() {
		Expect(fleetOutputLength([]domain.ReplicaMetrics{
			{VariantName: "d", AvgOutputTokens: 1000}, {VariantName: "d", AvgOutputTokens: 3000},
		}, roles)).To(Equal(2000.0))
		Expect(fleetOutputLength([]domain.ReplicaMetrics{{VariantName: "d", RequestRate: 2}}, roles)).To(Equal(0.0))
	})
})

func makeAnalyzerInput(
	metrics []domain.ReplicaMetrics,
	states []domain.VariantReplicaState,
) domain.AnalyzerInput {
	config := &config.ScalingPolicy{
		KvCacheThreshold:     0.8,
		QueueLengthThreshold: 5,
		AnalyzerName:         "saturation",
		ScaleUpThreshold:     0.85,
		ScaleDownBoundary:    0.70,
	}
	return domain.AnalyzerInput{
		ModelID:        "test-model",
		Namespace:      "test-ns",
		ReplicaMetrics: metrics,
		VariantStates:  states,
		Config:         config,
	}
}

// makeReplicaMetrics creates a ReplicaMetrics with the given parameters.
func makeReplicaMetrics(
	podName, variantName string,
	tokensInUse, totalCapacity int64,
	queueLen int,
	avgInput, avgOutput float64,
) domain.ReplicaMetrics {
	var kvUsage float64
	if totalCapacity > 0 {
		kvUsage = float64(tokensInUse) / float64(totalCapacity)
	}
	blockSize := int64(16)
	numBlocks := totalCapacity / blockSize

	return domain.ReplicaMetrics{
		PodName:               podName,
		VariantName:           variantName,
		KvCacheUsage:          kvUsage,
		QueueLength:           queueLen,
		NumGpuBlocks:          numBlocks,
		BlockSize:             blockSize,
		TotalKvCapacityTokens: totalCapacity,
		TokensInUse:           tokensInUse,
		AvgInputTokens:        avgInput,
		AvgOutputTokens:       avgOutput,
		ModelID:               "test-model",
		Namespace:             "test-ns",
	}
}

var _ = Describe("aggregateRoleDemand", func() {
	var analyzer *SaturationAnalyzer

	BeforeEach(func() {
		store := capacity.NewStore()
		analyzer = NewSaturationAnalyzer(store)
	})

	It("should return nil when all variants are role 'both'", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "v1", Role: "both", TotalDemand: 5000, ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "v2", Role: "", TotalDemand: 10000, ReplicaCount: 1, PerReplicaCapacity: 20000},
		}
		result := analyzer.aggregateRoleDemand(vcs, nil)
		Expect(result).To(BeNil())
	})

	It("should return nil when every variant has an empty role", func() {
		// Empty role canonicalizes to "both", so this is still non-disaggregated
		// and must not produce a per-role breakdown.
		vcs := []domain.VariantCapacity{
			{VariantName: "v1", Role: "", TotalDemand: 5000, ReplicaCount: 1, PerReplicaCapacity: 10000},
		}
		Expect(analyzer.aggregateRoleDemand(vcs, nil)).To(BeNil())
	})

	It("should not attribute queue demand when non-disaggregated", func() {
		// Even with queue demand present, an all-"both" fleet stays model-level:
		// the queue term is already folded into the model-level TotalDemand.
		vcs := []domain.VariantCapacity{
			{VariantName: "v1", Role: "both", TotalDemand: 5000, ReplicaCount: 1, PerReplicaCapacity: 10000},
		}
		queueByRole := map[string]float64{"prefill": 1000, "decode": 1500}
		Expect(analyzer.aggregateRoleDemand(vcs, queueByRole)).To(BeNil())
	})

	It("should compute per-role demand for a P/D disaggregated model", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalDemand: 9000, ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 10000},
			{VariantName: "decode-v1", Role: "decode", TotalDemand: 5000, ReplicaCount: 2, PendingReplicas: 0, PerReplicaCapacity: 10000},
		}
		result := analyzer.aggregateRoleDemand(vcs, nil)
		Expect(result).NotTo(BeNil())
		Expect(result).To(HaveLen(2))
		Expect(result["prefill"]).To(Equal(9000.0))
		Expect(result["decode"]).To(Equal(5000.0))
	})

	It("should sum demand across multiple variants sharing a role", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "decode-a", Role: "decode", TotalDemand: 5000, ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "decode-b", Role: "decode", TotalDemand: 2500, ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "prefill-a", Role: "prefill", TotalDemand: 1000, ReplicaCount: 1, PerReplicaCapacity: 10000},
		}
		result := analyzer.aggregateRoleDemand(vcs, nil)
		Expect(result["decode"]).To(Equal(7500.0))
		Expect(result["prefill"]).To(Equal(1000.0))
	})

	It("should handle mixed roles including 'both'", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalDemand: 9000, ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "both-v1", Role: "both", TotalDemand: 5000, ReplicaCount: 1, PerReplicaCapacity: 10000},
		}
		result := analyzer.aggregateRoleDemand(vcs, nil)
		// Has disaggregation because prefill-v1 has role != "both"; the "both"
		// bucket keeps its own demand so no variant's demand is dropped.
		Expect(result).NotTo(BeNil())
		Expect(result).To(HaveKey("prefill"))
		Expect(result).To(HaveKey("both"))
		Expect(result["prefill"]).To(Equal(9000.0))
		Expect(result["both"]).To(Equal(5000.0))
	})

	It("should fold an empty-role variant into the 'both' bucket when disaggregated", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "decode-v1", Role: "decode", TotalDemand: 3000, ReplicaCount: 1, PerReplicaCapacity: 10000},
			{VariantName: "legacy", Role: "", TotalDemand: 700, ReplicaCount: 1, PerReplicaCapacity: 10000},
		}
		result := analyzer.aggregateRoleDemand(vcs, nil)
		Expect(result).To(HaveKey(domain.RoleBoth))
		Expect(result[domain.RoleBoth]).To(Equal(700.0))
		Expect(result["decode"]).To(Equal(3000.0))
	})

	It("should add scheduler queue demand to per-role demand", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalDemand: 2000, ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 10000},
			{VariantName: "decode-v1", Role: "decode", TotalDemand: 3000, ReplicaCount: 2, PendingReplicas: 0, PerReplicaCapacity: 10000},
		}
		queueByRole := map[string]float64{
			"prefill": 1000, // inputTokens only
			"decode":  1500, // inputTokens + outputTokens
		}
		result := analyzer.aggregateRoleDemand(vcs, queueByRole)
		Expect(result).NotTo(BeNil())
		// 2000 (replica) + 1000 (queue) = 3000
		Expect(result["prefill"]).To(Equal(3000.0))
		// 3000 (replica) + 1500 (queue) = 4500
		Expect(result["decode"]).To(Equal(4500.0))
	})

	It("should skip queue demand for roles with no variants", func() {
		vcs := []domain.VariantCapacity{
			{VariantName: "prefill-v1", Role: "prefill", TotalDemand: 5000, ReplicaCount: 1, PendingReplicas: 0, PerReplicaCapacity: 10000},
		}
		// Queue demand for decode, but no decode variants exist
		queueByRole := map[string]float64{
			"prefill": 1000,
			"decode":  1500,
		}
		result := analyzer.aggregateRoleDemand(vcs, queueByRole)
		Expect(result).NotTo(BeNil())
		Expect(result).To(HaveLen(1))
		Expect(result).To(HaveKey("prefill"))
		Expect(result["prefill"]).To(Equal(6000.0)) // 5000 + 1000
	})
})

var _ = Describe("computeReplicaCapacityFallback", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *capacity.Store
		cfg      *config.ScalingPolicy
	)

	BeforeEach(func() {
		store = capacity.NewStore()
		analyzer = NewSaturationAnalyzer(store)
		cfg = &config.ScalingPolicy{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.85,
			ScaleDownBoundary:    0.70,
		}
	})

	It("keeps KvCacheThreshold out of both sides of the ratio", func() {
		// Regression: demand used to be charged against the *thresholded* capacity,
		// so the threshold cancelled and utilization equalled KvCacheUsage no matter
		// how it was configured — the knob had no effect on the scaling decision.
		// Tightening the ceiling must raise utilization for identical KV occupancy.
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})
		rm := domain.ReplicaMetrics{
			PodName:      "pod-1",
			VariantName:  "variant-a",
			KvCacheUsage: 0.5,
		}

		utilizationAt := func(threshold float64) float64 {
			cfg.KvCacheThreshold = threshold
			r := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())
			Expect(r).NotTo(BeNil())
			return float64(r.ReplicaDemand) / float64(r.EffectiveCapacity)
		}

		// utilization == KvCacheUsage / KvCacheThreshold, as on the main path.
		Expect(utilizationAt(1.0)).To(BeNumerically("~", 0.5, 1e-9))
		Expect(utilizationAt(0.8)).To(BeNumerically("~", 0.625, 1e-9))
		// At a ceiling equal to the occupancy, the replica is exactly saturated.
		Expect(utilizationAt(0.5)).To(BeNumerically("~", 1.0, 1e-9))
	})

	It("floors a tiny usable capacity at one token instead of dropping the replica", func() {
		// int64 truncation used to turn a small (capacity x threshold) product into
		// 0, and the zero-guard then discarded the replica entirely — the variant
		// reported no capacity at all, so the engine could see a shortfall it had no
		// per-replica capacity to divide by and never acted on it.
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 10, // 10 * 0.05 = 0.5 -> truncates to 0
			LearnedFrom:       "deployment",
		})
		cfg.KvCacheThreshold = 0.05

		rm := domain.ReplicaMetrics{
			PodName:      "pod-1",
			VariantName:  "variant-a",
			KvCacheUsage: 0.5,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())
		Expect(result).NotTo(BeNil(), "a positive stored capacity must stay sizable")
		Expect(result.EffectiveCapacity).To(Equal(int64(1)))
	})

	It("still returns nil when the stored capacity is genuinely zero", func() {
		// The floor must not resurrect a record that carries no capacity at all.
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 0,
			LearnedFrom:       "deployment",
		})
		rm := domain.ReplicaMetrics{
			PodName:      "pod-1",
			VariantName:  "variant-a",
			KvCacheUsage: 0.5,
		}
		Expect(analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())).To(BeNil())
	})

	It("should return nil when capacity store has no record", func() {
		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			KvCacheUsage:          0.5,
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())
		Expect(result).To(BeNil())
	})

	It("should return nil when capacity store record has zero effective capacity", func() {
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 0,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:      "pod-1",
			VariantName:  "variant-a",
			KvCacheUsage: 0.5,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())
		Expect(result).To(BeNil())
	})

	It("should apply KvCacheThreshold to stored capacity (consistent with main path)", func() {
		// Store raw capacity of 10000
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			KvCacheUsage:          0.6,
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())
		Expect(result).NotTo(BeNil())
		// Capacity is the usable portion: 10000 * 0.8 (KvCacheThreshold) = 8000.
		Expect(result.EffectiveCapacity).To(Equal(int64(8000)))
		// Demand is occupancy against the RAW capacity: 0.6 * 10000 = 6000. Charging
		// it against the thresholded capacity instead would put the threshold on both
		// sides and cancel, making utilization equal KvCacheUsage regardless of it.
		Expect(result.ReplicaDemand).To(Equal(int64(6000)))
		// utilization = 6000/8000 = 0.75 = KvCacheUsage/KvCacheThreshold, as the main path.
	})

	It("should detect saturation at KvCacheUsage >= KvCacheThreshold", func() {
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			KvCacheUsage:          1.0, // 100% KV usage
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 = 8000; demand = 1.0 * 10000 = 10000 >= 8000
		Expect(result.ReplicaDemand).To(Equal(int64(10000)))
	})

	It("should detect saturation when KvCacheUsage exceeds threshold (matching main path behavior)", func() {
		// This verifies the fix from the review: at 90% KV usage with 0.8 threshold,
		// the fallback should report saturation, matching the main path behavior.
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			KvCacheUsage:          0.9, // 90% usage, threshold is 80%
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 = 8000; demand = 0.9 * 10000 = 9000 >= 8000.
		// The spec title is now actually satisfied: occupancy past the configured
		// ceiling reads as saturated, exactly as the main path does. Under the older
		// arithmetic demand was charged against the thresholded capacity (0.9 * 8000 =
		// 7200 < 8000), so utilization was pinned to KvCacheUsage and no KV occupancy
		// short of 100% could ever cross the ceiling.
		Expect(result.EffectiveCapacity).To(Equal(int64(8000)))
		Expect(result.ReplicaDemand).To(Equal(int64(9000)))
	})

	It("should add queue-based demand when avg input tokens available", func() {
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			KvCacheUsage:          0.5,
			TotalKvCapacityTokens: 0,
			QueueLength:           3,
			AvgInputTokens:        500,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 = 8000
		// demand = 0.5 * 10000 + 3 * 500 = 5000 + 1500 = 6500
		Expect(result.ReplicaDemand).To(Equal(int64(6500)))
	})

	It("should not add queue demand when token metrics are unavailable", func() {
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			KvCacheUsage:          0.3,
			TotalKvCapacityTokens: 0,
			QueueLength:           10,
			AvgInputTokens:        0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 = 8000
		// demand = 0.3 * 10000 = 3000 (no queue contribution)
		Expect(result.ReplicaDemand).To(Equal(int64(3000)))
	})

	It("should charge queue demand by role", func() {
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			KvCacheUsage:          0.5,
			TotalKvCapacityTokens: 0,
			QueueLength:           3,
			AvgInputTokens:        500,
			AvgOutputTokens:       250,
		}

		// effectiveCapacity = 10000 * 0.8 = 8000; resident = 0.5 * 10000 = 5000.
		prefill := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RolePrefill, "H100", logr.Discard())
		Expect(prefill).NotTo(BeNil())
		// 5000 + 3 * 500 = 6500 — output tokens excluded.
		Expect(prefill.ReplicaDemand).To(Equal(int64(6500)))

		decode := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleDecode, "H100", logr.Discard())
		Expect(decode).NotTo(BeNil())
		// 5000 + 3 * (500 + 250) = 7250 — output tokens included.
		Expect(decode.ReplicaDemand).To(Equal(int64(7250)))
	})

	It("should not report saturation for an idle replica with a shallow queue", func() {
		// The fallback denominator is a per-step batched-token budget, while the
		// queue charge is in absolute KV tokens — a dimensional mismatch this
		// path inherits. Raising the per-request charge moves the point where
		// that mismatch invents saturation, so pin the safe end: a mostly-idle
		// replica with a few queued requests must not read as saturated.
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 8192,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			KvCacheUsage:          0.10,
			TotalKvCapacityTokens: 0,
			QueueLength:           2,
			AvgInputTokens:        200,
			AvgOutputTokens:       100,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleDecode, "H100", logr.Discard())
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 8192 * 0.8 = 6553
		// demand = 0.10 * 8192 + 2 * (200 + 100) = 819 + 600 = 1419
		Expect(result.EffectiveCapacity).To(Equal(int64(6553)))
		Expect(result.ReplicaDemand).To(Equal(int64(1419)))
	})

	It("should populate all ReplicaCapacity fields correctly", func() {
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			EffectiveCapacity: 10000,
			LearnedFrom:       "deployment",
		})

		rm := domain.ReplicaMetrics{
			PodName:               "pod-1",
			VariantName:           "variant-a",
			KvCacheUsage:          0.4,
			TotalKvCapacityTokens: 0,
		}

		result := analyzer.computeReplicaCapacityFallback(rm, cfg, "test-model", "test-ns", domain.RoleBoth, "H100", logr.Discard())
		Expect(result).NotTo(BeNil())
		// effectiveCapacity = 10000 * 0.8 = 8000
		Expect(result.PodName).To(Equal("pod-1"))
		Expect(result.VariantName).To(Equal("variant-a"))
		Expect(result.AcceleratorName).To(Equal("H100"))
		Expect(result.EffectiveCapacity).To(Equal(int64(8000)))
		Expect(result.MemoryBoundCapacity).To(Equal(int64(8000)))
		Expect(result.ComputeBoundCapacity).To(Equal(int64(8000)))
		Expect(result.TotalKvCapacityTokens).To(Equal(int64(8000)))
		// demand = 0.4 * 10000 (raw) = 4000
		Expect(result.TokensInUse).To(Equal(int64(4000)))
		Expect(result.ReplicaDemand).To(Equal(int64(4000)))
	})
})

var _ = Describe("waitingQueueDemand", func() {
	// Average request shape shared by the role cases: a waiting request costs
	// 100 input tokens and, once it starts generating, 50 more output tokens.
	rm := domain.ReplicaMetrics{
		QueueLength:     4,
		AvgInputTokens:  100,
		AvgOutputTokens: 50,
	}

	It("charges prefill replicas for input tokens only", func() {
		// Prompt KV is all a prefill replica materializes: 4 * 100.
		Expect(waitingQueueDemand(rm, domain.RolePrefill)).To(Equal(int64(400)))
	})

	It("charges decode replicas for input plus output tokens", func() {
		// Decode holds the transferred prompt KV and grows it per generated
		// token: 4 * (100 + 50).
		Expect(waitingQueueDemand(rm, domain.RoleDecode)).To(Equal(int64(600)))
	})

	It("charges 'both' replicas for input plus output tokens", func() {
		Expect(waitingQueueDemand(rm, domain.RoleBoth)).To(Equal(int64(600)))
	})

	It("treats an empty role as 'both'", func() {
		Expect(waitingQueueDemand(rm, "")).To(Equal(int64(600)))
	})

	It("treats an unknown role as 'both'", func() {
		Expect(waitingQueueDemand(rm, "some-future-role")).To(Equal(int64(600)))
	})

	It("returns zero for an empty queue regardless of role", func() {
		empty := rm
		empty.QueueLength = 0
		for _, role := range []string{domain.RolePrefill, domain.RoleDecode, domain.RoleBoth, ""} {
			Expect(waitingQueueDemand(empty, role)).To(BeZero())
		}
	})

	It("returns zero for a negative queue length", func() {
		negative := rm
		negative.QueueLength = -1
		Expect(waitingQueueDemand(negative, domain.RoleDecode)).To(BeZero())
	})

	It("returns zero when no token metrics are available", func() {
		noTokens := domain.ReplicaMetrics{QueueLength: 10}
		Expect(waitingQueueDemand(noTokens, domain.RolePrefill)).To(BeZero())
		Expect(waitingQueueDemand(noTokens, domain.RoleDecode)).To(BeZero())
	})

	It("returns zero for non-finite token metrics", func() {
		// Converting a non-finite float64 to int64 is implementation-defined in
		// Go, so an unguarded NaN yields garbage demand — hugely negative on
		// amd64. A `<= 0` check does not catch NaN, hence the explicit guard.
		for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			Expect(waitingQueueDemand(
				domain.ReplicaMetrics{QueueLength: 3, AvgInputTokens: bad}, domain.RolePrefill)).To(BeZero())
			Expect(waitingQueueDemand(
				domain.ReplicaMetrics{QueueLength: 3, AvgOutputTokens: bad}, domain.RoleDecode)).To(BeZero())
			Expect(waitingQueueDemand(
				domain.ReplicaMetrics{QueueLength: 3, AvgInputTokens: 100, AvgOutputTokens: bad}, domain.RoleDecode)).To(BeZero())
		}
	})

	It("returns zero for finite token metrics too large for an int64", func() {
		// Finite but out of int64 range hits the same implementation-defined
		// conversion as NaN, so the guard must cover it too.
		Expect(waitingQueueDemand(
			domain.ReplicaMetrics{QueueLength: 3, AvgInputTokens: 1e300}, domain.RolePrefill)).To(BeZero())
		Expect(waitingQueueDemand(
			domain.ReplicaMetrics{QueueLength: 1e9, AvgInputTokens: 1e12}, domain.RoleDecode)).To(BeZero())

		// Exactly 2^63: float64(math.MaxInt64) rounds up to this value, so a
		// `>` bound would admit it and overflow to MinInt64.
		Expect(waitingQueueDemand(
			domain.ReplicaMetrics{QueueLength: 2, AvgInputTokens: math.Pow(2, 62)}, domain.RolePrefill)).To(BeZero())
	})

	It("charges decode replicas for output even when input tokens are unreported", func() {
		// A decode replica whose prompt-token metric is missing still pays for
		// generation; the old input-only formula returned 0 here.
		outputOnly := domain.ReplicaMetrics{QueueLength: 4, AvgOutputTokens: 50}
		Expect(waitingQueueDemand(outputOnly, domain.RoleDecode)).To(Equal(int64(200)))
		Expect(waitingQueueDemand(outputOnly, domain.RolePrefill)).To(BeZero())
	})
})

var _ = Describe("Analyze per-replica waiting-queue demand by role", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *capacity.Store
		ctx      context.Context
		satCfg   *config.ScalingPolicy
	)

	BeforeEach(func() {
		store = capacity.NewStore()
		analyzer = NewSaturationAnalyzer(store)
		ctx = context.Background()
		satCfg = &config.ScalingPolicy{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.85,
			ScaleDownBoundary:    0.70,
		}
	})

	// replicaFor builds a replica with 1000 tokens resident and 4 requests
	// waiting, each averaging 100 input and 50 output tokens.
	replicaFor := func(variant string) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{
			PodName:               variant + "-pod-1",
			VariantName:           variant,
			ModelID:               "test-model",
			Namespace:             "test-ns",
			TotalKvCapacityTokens: 10000,
			TokensInUse:           1000,
			QueueLength:           4,
			AvgInputTokens:        100,
			AvgOutputTokens:       50,
		}
	}

	demandFor := func(role string) float64 {
		input := domain.AnalyzerInput{
			ModelID:        "test-model",
			Namespace:      "test-ns",
			ReplicaMetrics: []domain.ReplicaMetrics{replicaFor("variant-a")},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1, Role: role},
			},
			Config: satCfg,
		}

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))
		return result.VariantCapacities[0].TotalDemand
	}

	It("excludes output tokens for a prefill variant", func() {
		// 1000 resident + 4 * 100 input = 1400
		Expect(demandFor(domain.RolePrefill)).To(BeNumerically("==", 1400))
	})

	It("includes output tokens for a decode variant", func() {
		// 1000 resident + 4 * (100 + 50) = 1600
		Expect(demandFor(domain.RoleDecode)).To(BeNumerically("==", 1600))
	})

	It("includes output tokens for a 'both' variant", func() {
		Expect(demandFor(domain.RoleBoth)).To(BeNumerically("==", 1600))
	})

	// The fallback path (no vllm:cache_config_info) must honour the role too.
	// These go through Analyze so they pin the role forwarding from
	// computeReplicaCapacity into computeReplicaCapacityFallback, not just the
	// helper — hardcoding a role at that hand-off would otherwise go unnoticed.
	Context("on the fallback path (no cache_config_info)", func() {
		fallbackDemandFor := func(role string) float64 {
			store.Update("test-ns", "test-model", "variant-a", capacity.Record{
				EffectiveCapacity: 10000,
				LearnedFrom:       "deployment",
			})

			rm := replicaFor("variant-a")
			rm.TotalKvCapacityTokens = 0 // forces the fallback
			rm.TokensInUse = 0           // fallback derives demand from KvCacheUsage
			rm.KvCacheUsage = 0.5

			input := domain.AnalyzerInput{
				ModelID:        "test-model",
				Namespace:      "test-ns",
				ReplicaMetrics: []domain.ReplicaMetrics{rm},
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1, Role: role},
				},
				Config: satCfg,
			}

			result, err := analyzer.Analyze(ctx, input)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.VariantCapacities).To(HaveLen(1))
			return result.VariantCapacities[0].TotalDemand
		}

		It("excludes output tokens for a prefill variant", func() {
			// effectiveCapacity = 10000 * 0.8 = 8000
			// demand = 0.5 * 10000 (raw) + 4 * 100 input = 5000 + 400 = 5400
			Expect(fallbackDemandFor(domain.RolePrefill)).To(BeNumerically("==", 5400))
		})

		It("includes output tokens for a decode variant", func() {
			// demand = 0.5 * 10000 (raw) + 4 * (100 + 50) = 5000 + 600 = 5600
			Expect(fallbackDemandFor(domain.RoleDecode)).To(BeNumerically("==", 5600))
		})
	})

	It("includes output tokens when the role is unset", func() {
		// An unset role canonicalizes to "both", so non-disaggregated
		// deployments get the full-lifecycle charge.
		Expect(demandFor("")).To(BeNumerically("==", 1600))
	})

	It("charges each role correctly in a P/D disaggregated model", func() {
		input := domain.AnalyzerInput{
			ModelID:   "test-model",
			Namespace: "test-ns",
			ReplicaMetrics: []domain.ReplicaMetrics{
				replicaFor("prefill-variant"),
				replicaFor("decode-variant"),
			},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "prefill-variant", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill},
				{VariantName: "decode-variant", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
			},
			Config: satCfg,
		}

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(2))

		byVariant := map[string]float64{}
		for _, vc := range result.VariantCapacities {
			byVariant[vc.VariantName] = vc.TotalDemand
		}
		Expect(byVariant["prefill-variant"]).To(BeNumerically("==", 1400))
		Expect(byVariant["decode-variant"]).To(BeNumerically("==", 1600))

		// Model-level demand is the sum of both roles.
		Expect(result.TotalDemand).To(BeNumerically("==", 3000))
	})
})

var _ = Describe("Analyze with fallback (no cache_config_info)", func() {
	var (
		analyzer *SaturationAnalyzer
		store    *capacity.Store
		ctx      context.Context
	)

	BeforeEach(func() {
		store = capacity.NewStore()
		analyzer = NewSaturationAnalyzer(store)
		ctx = context.Background()
	})

	It("should produce valid result using fallback when cache_config_info is absent", func() {
		store.Update("test-ns", "test-model", "variant-a", capacity.Record{
			GpuCount:          1,
			EffectiveCapacity: 8192,
			LearnedFrom:       "deployment",
		})

		input := domain.AnalyzerInput{
			ModelID:   "test-model",
			Namespace: "test-ns",
			ReplicaMetrics: []domain.ReplicaMetrics{
				{
					PodName:               "pod-1",
					VariantName:           "variant-a",
					ModelID:               "test-model",
					Namespace:             "test-ns",
					KvCacheUsage:          0.9,
					TotalKvCapacityTokens: 0,
					TokensInUse:           0,
					QueueLength:           3,
					AvgInputTokens:        100,
					AvgOutputTokens:       50,
				},
			},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
			Config: &config.ScalingPolicy{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.85,
				ScaleDownBoundary:    0.70,
			},
		}

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())
		Expect(result.VariantCapacities).To(HaveLen(1))

		vc := result.VariantCapacities[0]
		Expect(vc.VariantName).To(Equal("variant-a"))
		// effectiveCapacity = int64(8192 * 0.8) = 6553
		storeCapacity := float64(8192)
		expectedCapacity := float64(int64(storeCapacity * 0.8))
		Expect(vc.PerReplicaCapacity).To(Equal(expectedCapacity))
		Expect(vc.TotalDemand).To(BeNumerically(">", 0))
		Expect(result.TotalDemand).To(BeNumerically(">", 0))
	})

	It("should skip replicas with no store data and no cache_config_info", func() {
		input := domain.AnalyzerInput{
			ModelID:   "test-model",
			Namespace: "test-ns",
			ReplicaMetrics: []domain.ReplicaMetrics{
				{
					PodName:               "pod-1",
					VariantName:           "variant-a",
					ModelID:               "test-model",
					Namespace:             "test-ns",
					KvCacheUsage:          0.5,
					TotalKvCapacityTokens: 0,
				},
			},
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "variant-a", AcceleratorName: "H100", CurrentReplicas: 1, GPUsPerReplica: 1},
			},
			Config: &config.ScalingPolicy{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				AnalyzerName:         "saturation",
				ScaleUpThreshold:     0.85,
				ScaleDownBoundary:    0.70,
			},
		}

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())
	})
})

var _ = Describe("ScalingPolicy ApplyDefaults before Validate", func() {
	It("should pass validation after ApplyDefaults for V2 config with omitted thresholds", func() {
		cfg := config.ScalingPolicy{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
		}

		cfg.ApplyDefaults()
		err := cfg.Validate()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.ScaleUpThreshold).To(BeNumerically(">", 0))
		Expect(cfg.ScaleDownBoundary).To(BeNumerically(">", 0))
	})

	It("should fail validation without ApplyDefaults for V2 config with omitted thresholds", func() {
		cfg := config.ScalingPolicy{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
		}

		err := cfg.Validate()
		Expect(err).To(HaveOccurred())
	})

	It("should preserve explicitly set values after ApplyDefaults", func() {
		cfg := config.ScalingPolicy{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
			ScaleUpThreshold:     0.9,
			ScaleDownBoundary:    0.6,
			Priority:             2.0,
		}

		cfg.ApplyDefaults()
		err := cfg.Validate()
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.ScaleUpThreshold).To(Equal(0.9))
		Expect(cfg.ScaleDownBoundary).To(Equal(0.6))
		Expect(cfg.Priority).To(Equal(2.0))
	})

	It("should apply default priority when omitted", func() {
		cfg := config.ScalingPolicy{
			KvCacheThreshold:     0.8,
			QueueLengthThreshold: 5,
			AnalyzerName:         "saturation",
		}

		cfg.ApplyDefaults()
		Expect(cfg.Priority).To(BeNumerically(">", 0))
	})
})

var _ = Describe("aggregateByVariant capacity Reason", func() {
	It("sets Reason to P0-store when no live replicas but a capacity store record exists", func() {
		store := capacity.NewStore()
		store.Update("ns", "m", "v1", capacity.Record{
			EffectiveCapacity: 50000,
			LearnedFrom:       capacity.LearnedFromLive,
		})
		a := NewSaturationAnalyzer(store)

		input := domain.AnalyzerInput{
			ModelID:   "m",
			Namespace: "ns",
			// No ReplicaMetrics — store path will fire.
			ReplicaMetrics: nil,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "v1", CurrentReplicas: 0, PendingReplicas: 0},
			},
			Config: &config.ScalingPolicy{KvCacheThreshold: 0.9},
		}
		result, err := a.Analyze(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))
		Expect(result.VariantCapacities[0].Reason).To(Equal(satReasonP0Store))
	})

	It("sets Reason to no-data when variant has zero replicas and no store or compatible record", func() {
		store := capacity.NewStore()
		a := NewSaturationAnalyzer(store)

		input := domain.AnalyzerInput{
			ModelID:        "m",
			Namespace:      "ns",
			ReplicaMetrics: nil,
			VariantStates: []domain.VariantReplicaState{
				{VariantName: "v1", CurrentReplicas: 0, PendingReplicas: 0},
			},
			Config: &config.ScalingPolicy{KvCacheThreshold: 0.9},
		}
		result, err := a.Analyze(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))
		Expect(result.VariantCapacities[0].Reason).To(Equal(satReasonNoData))
	})
})

var _ = Describe("k2SourceLabel", func() {
	It("returns error when K2Priority is not in the known set", func() {
		replicas := []capacity.ReplicaCapacity{{K2Priority: 0, EffectiveCapacity: 100}}
		Expect(k2SourceLabel(replicas)).To(Equal("error"))
	})

	It("returns empty string when replicas slice is empty", func() {
		Expect(k2SourceLabel(nil)).To(Equal(""))
	})
})

var _ = Describe("aggregateByVariant DP>1 with pending replicas", func() {
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
	)

	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(capacity.NewStore())
		ctx = context.Background()
	})

	It("keeps ready and pending counts in scale-target units on a DP>1 variant", func() {
		// The collector merges a pod's DP ranks into one ReplicaMetrics, so a
		// DP=8 pod arrives as a single record carrying the whole pod's capacity
		// (8 × 16000). Ready and pending are then both pod counts and add
		// directly — no instances-per-pod factor to reconcile.
		metrics := []domain.ReplicaMetrics{
			makeReplicaMetrics("pod-1", "decode-v1", 64000, 128000, 0, 100, 50),
		}

		input := makeAnalyzerInput(
			metrics,
			[]domain.VariantReplicaState{
				{VariantName: "decode-v1", CurrentReplicas: 2, PendingReplicas: 1, GPUsPerReplica: 8},
			},
		)

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))

		vc := result.VariantCapacities[0]
		Expect(vc.ReplicaCount).To(Equal(1))    // one pod reported capacity
		Expect(vc.PendingReplicas).To(Equal(1)) // one pod still coming up

		// The per-replica capacity is the whole pod's, so dividing a demand by it
		// yields a pod target — the unit the optimizer spends it in.
		Expect(vc.PerReplicaCapacity).To(BeNumerically(">", 64000))

		// TotalAnticipatedSupply = (ReplicaCount + PendingReplicas) × PerReplicaCapacity
		want := float64(vc.ReplicaCount+vc.PendingReplicas) * vc.PerReplicaCapacity
		Expect(aggregation.SumTotalAnticipatedSupply(result.VariantCapacities)).To(Equal(want))
	})
})

var _ = Describe("the fleet-shape memo's lifetime", func() {
	It("ages out with the rest of the analyzer's per-model state", func() {
		// Every other memo on the analyzer is swept by EvictStaleHistory; this
		// one was added without a sweep and grew for the life of the process.
		// The real clock: EvictStaleHistory measures age with time.Since, as
		// it does for every other memo on this struct, while lastSeen is
		// stamped from a.now(). The two agree in production and a fake clock
		// set in the past would read every entry as stale.
		analyzer := NewSaturationAnalyzer(capacity.NewStore())

		rm := makeReplicaMetrics("d0", "decode-v", 200_000, 1_162_240, 0, 1000, 6000)
		rm.RequestRate = 0.6
		rm.GenerationTokenRate = rm.RequestRate * 6000
		rm.Ready = true
		in := makeAnalyzerInput([]domain.ReplicaMetrics{rm}, []domain.VariantReplicaState{
			{VariantName: "decode-v", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
		})
		in.ArrivalRate = 6
		_, err := analyzer.Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.fleetShape).To(HaveLen(1), "one cycle records one model's shape")

		analyzer.EvictStaleHistory(time.Hour)
		Expect(analyzer.fleetShape).To(HaveLen(1), "a memo in use is kept")

		analyzer.EvictStaleHistory(0)
		Expect(analyzer.fleetShape).To(BeEmpty(), "and a stale one goes")
	})
})
