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

	It("still answers when it has no model to derive from", func() {
		// No ITL readings at all: every replica reports a shape and a load but
		// no inter-token latency, so nothing can be fitted and the measured
		// window is all there is. The analyzer must still produce a result
		// rather than lose the role's floor entirely.
		a := NewSaturationAnalyzer(capacity.NewStore())
		in := fleet(3, 1000, 6000, 10, 900_000)
		for i := range in.ReplicaMetrics {
			in.ReplicaMetrics[i].AvgITL = 0
		}
		res, err := a.Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RoleDemand).To(HaveKey(domain.RoleDecode))
	})
})
