package saturation_v2

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The fit the 2026-09-23 shape-swap runs were taken on, and the card they ran
// on: ITL(k) = 30.7 ms*k + 1.6 ms over 70 intervals, 1,163,136 KV tokens,
// max_num_seqs 256.
var (
	tracedModel  = itl.Model{A: 0.0307, B: 0.0016}
	tracedParams = &capacity.EngineParams{MaxNumSeqs: 256}
	tracedKv     = int64(1_163_136)
)

var _ = Describe("deriveMu", func() {
	It("reproduces the mu the fleet measured for itself under the first shape", func() {
		// 1000-token prompts, 6000-token generations. Saturated, the fleet
		// recorded 1.4292 and 1.5429 requests a second across the two runs.
		got := deriveMu(tracedModel, tracedParams, tracedKv, 1000, 6000)
		Expect(got.ok).To(BeTrue())
		Expect(got.rate).To(BeNumerically("~", 1.49, 0.12),
			"the model has to land where the hardware did, or it is not describing it")
	})

	It("prices the second shape, which the fleet never measured at all", func() {
		// 8000-token prompts, 1000-token generations. The fleet was
		// over-provisioned for this and never saturated under it, so no reading
		// was ever taken -- the floor went on using the first shape's figure
		// for the remaining nineteen minutes of the run.
		got := deriveMu(tracedModel, tracedParams, tracedKv, 8000, 1000)
		Expect(got.ok).To(BeTrue())
		Expect(got.rate).To(BeNumerically(">", 3.0))
		Expect(6.0/got.rate).To(BeNumerically("<", 2.5),
			"the arrival rate over a shape-correct mu is a small fleet, not the seven that ran")
	})

	It("is capped by max_num_seqs, which the trace's second phase sat on", func() {
		// The cache alone would imply over 6,500 resident sequences for a
		// short shape; the engine admits 256, and phase 2 ran at exactly that.
		got := deriveMu(tracedModel, tracedParams, tracedKv, 100, 100)
		Expect(got.ok).To(BeTrue())
		Expect(got.seqs).To(Equal(float64(tracedParams.MaxNumSeqs)))

		uncapped := deriveMu(tracedModel, &capacity.EngineParams{}, tracedKv, 100, 100)
		Expect(uncapped.seqs).To(BeNumerically(">", 6000),
			"and without the cap it is the number the engine will not admit")
	})

	It("declines rather than guessing", func() {
		Expect(deriveMu(itl.Model{}, tracedParams, tracedKv, 1000, 6000).ok).To(BeFalse(),
			"no fitted model")
		Expect(deriveMu(tracedModel, tracedParams, 0, 1000, 6000).ok).To(BeFalse(),
			"no KV capacity")
		Expect(deriveMu(tracedModel, tracedParams, tracedKv, 1000, 0).ok).To(BeFalse(),
			"no generation length is not a decode shape")
		Expect(deriveMu(tracedModel, tracedParams, tracedKv, 0, 6000).ok).To(BeFalse(),
			"no prompt length is not a shape at all")
		Expect(deriveMu(itl.Model{A: -1, B: 0.5}, tracedParams, tracedKv, 1000, 6000).ok).To(BeFalse(),
			"a model whose reading at k_sat is not positive")
	})
})

var _ = Describe("the floor's mu, through Analyze", func() {
	const variant = "decode-v"

	// A fleet whose replicas sit at a spread of loads, each reporting the ITL
	// the traced model predicts for its own k, so the fit recovers that model.
	// Ten of them clears the window's sample count and its k-spread in one
	// cycle.
	fleet := func(n int, avgIn, avgOut float64, queue int, tokens int64) domain.AnalyzerInput {
		rms := make([]domain.ReplicaMetrics, 0, n)
		for i := 0; i < n; i++ {
			k := 0.20 + 0.06*float64(i)
			rm := makeReplicaMetrics(fmt.Sprintf("d%d", i), variant, tokens, tracedKv, queue, avgIn, avgOut)
			rm.Ready = true
			rm.KvUsageInstant = k
			rm.AvgITL = tracedModel.ITLAt(k)
			rm.RequestRate = 0.6
			rm.GenerationTokenRate = rm.RequestRate * avgOut
			rms = append(rms, rm)
		}
		in := makeAnalyzerInput(rms, []domain.VariantReplicaState{
			{VariantName: variant, Role: domain.RoleDecode, AcceleratorName: "H200",
				CurrentReplicas: n, GPUsPerReplica: 1},
		})
		in.ArrivalRate = 6
		return in
	}

	// What the floor asked for, in replicas: the role's demand over what one
	// replica of it supplies.
	impliedReplicas := func(res *domain.AnalyzerResult) float64 {
		var perReplica float64
		for _, vc := range res.VariantCapacities {
			if vc.VariantName == variant {
				perReplica = vc.PerReplicaCapacity
			}
		}
		Expect(perReplica).To(BeNumerically(">", 0))
		return res.RoleDemand[domain.RoleDecode] / perReplica
	}

	It("prices the shape arriving now, on the first cycle it is seen", func() {
		a := NewSaturationAnalyzer(capacity.NewStore())
		ctx := context.Background()

		// One cycle of the first shape, saturated, is enough to fit ITL(k) --
		// ten replicas at a spread of loads clears the window in one go.
		_, err := a.Analyze(ctx, fleet(10, 1000, 6000, 10, 900_000))
		Expect(err).NotTo(HaveOccurred())

		// The shape swaps to short generations and long prompts, and the fleet
		// is now far too big for it: nothing is queued and occupancy has
		// collapsed. On the measured path nothing would saturate again, so the
		// floor would go on pricing 6 req/s against the 6000-token figure for
		// the rest of the run -- which is what the fleet did on 2026-09-23,
		// holding seven replicas against a utilization of 0.028.
		res, err := a.Analyze(ctx, fleet(10, 8000, 1000, 0, 17_282))
		Expect(err).NotTo(HaveOccurred())

		Expect(impliedReplicas(res)).To(BeNumerically("<", 2.5),
			"a mu derived for the shape now arriving asks for one or two replicas")
	})

	It("carries the fit across a shape change instead of refitting", func() {
		// The window is not cleared when the shape moves, and this is the spec
		// that holds it to that: cycle 2 brings only three replicas, which on
		// their own are short of DefaultMinSamples and of the k-spread, so a
		// model can only exist here if cycle 1's readings are still in the
		// window. ITL(k) describes the hardware, not the shape, so they are.
		a := NewSaturationAnalyzer(capacity.NewStore())
		ctx := context.Background()

		_, err := a.Analyze(ctx, fleet(10, 1000, 6000, 10, 900_000))
		Expect(err).NotTo(HaveOccurred())

		res, err := a.Analyze(ctx, fleet(3, 8000, 1000, 0, 17_282))
		Expect(err).NotTo(HaveOccurred())
		Expect(impliedReplicas(res)).To(BeNumerically("<", 2.5),
			"three fresh readings cannot fit a model; the carried-over ones can")
	})

	It("falls back to the measured window when no model can be fitted", func() {
		// Ten replicas, so the sample count is not what stops it -- every one
		// of them reports no inter-token latency, which is the exclusion under
		// test. The floor must still bind, on the measured reading, rather than
		// the role quietly losing its floor.
		a := NewSaturationAnalyzer(capacity.NewStore())
		in := fleet(10, 1000, 6000, 10, 900_000)
		for i := range in.ReplicaMetrics {
			in.ReplicaMetrics[i].AvgITL = 0
		}
		res, err := a.Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())

		// The measured mu here is the fleet's own completion rate, 0.6 req/s
		// against 6 arriving, so the floor asks for far more than the derived
		// path would -- which is the point: this is the old answer, and it is
		// still available when the new one is not.
		Expect(impliedReplicas(res)).To(BeNumerically(">", 2.5),
			"with no model the floor is back on the measured reading")
	})
})

var _ = Describe("noteITL", func() {
	const variant = "decode-v"

	reading := func(pod string, k, avgITL float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, variant, 900_000, tracedKv, 0, 1000, 6000)
		rm.Ready = true
		rm.KvUsageInstant = k
		rm.AvgITL = avgITL
		return rm
	}
	// Ten readings on the traced line, enough to fit it.
	line := func() []domain.ReplicaMetrics {
		out := make([]domain.ReplicaMetrics, 0, 10)
		for i := 0; i < 10; i++ {
			k := 0.20 + 0.06*float64(i)
			out = append(out, reading(fmt.Sprintf("d%d", i), k, tracedModel.ITLAt(k)))
		}
		return out
	}
	fit := func(rms []domain.ReplicaMetrics) itl.Model {
		a := NewSaturationAnalyzer(capacity.NewStore())
		return a.noteITL("ns|model|"+variant, rms, variant, a.now())
	}

	It("fits the line its replicas are reporting", func() {
		got := fit(line())
		Expect(got.IsZero()).To(BeFalse())
		Expect(got.A).To(BeNumerically("~", tracedModel.A, 1e-6))
		Expect(got.B).To(BeNumerically("~", tracedModel.B, 1e-6))
	})

	DescribeTable("leaves out what is not a reading of this variant's own replicas",
		func(spoil func(*domain.ReplicaMetrics)) {
			rms := line()
			for i := range rms {
				spoil(&rms[i])
			}
			Expect(fit(rms).IsZero()).To(BeTrue())
		},
		Entry("another variant's", func(rm *domain.ReplicaMetrics) { rm.VariantName = "other-v" }),
		Entry("a pod still failing readiness", func(rm *domain.ReplicaMetrics) { rm.Ready = false }),
		Entry("a warm-pool bridge on the pool's own settings", func(rm *domain.ReplicaMetrics) { rm.FromWarmPool = true }),
		Entry("no inter-token latency", func(rm *domain.ReplicaMetrics) { rm.AvgITL = 0 }),
		Entry("no utilization to place it at", func(rm *domain.ReplicaMetrics) { rm.KvUsageInstant = 0 }),
		Entry("a load above the band the line was fitted in", func(rm *domain.ReplicaMetrics) {
			rm.KvUsageInstant = itl.DefaultMaxObservableK + 0.05
		}),
	)

	It("keeps the good readings when only some are excluded", func() {
		rms := line()
		rms = append(rms, reading("warm", 0.5, 0.02))
		rms[len(rms)-1].FromWarmPool = true
		rms = append(rms, reading("other", 0.5, 0.02))
		rms[len(rms)-1].VariantName = "other-v"

		got := fit(rms)
		Expect(got.IsZero()).To(BeFalse())
		Expect(got.A).To(BeNumerically("~", tracedModel.A, 1e-6),
			"the excluded readings are off the line and would drag the fit")
	})
})
