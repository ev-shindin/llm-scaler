package saturation_v2

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The figures below are from an H100 serving Qwen3-0.6B at 4000 input / 1000
// output tokens, which is the shape the benchmark that exposed this drives.
const (
	measuredITL     = 0.0246 // 24.6 ms, vLLM inter_token_latency_seconds
	measuredAvgOut  = 1000.0
	measuredAvgIn   = 4000.0
	measuredService = measuredAvgOut * measuredITL // 24.6s, vs 24.66s measured e2e-minus-queue
)

// avgIn is fixed at the measured shape: every case here varies ITL, output
// length or replica count, never the prompt size.
func metricsAt(itl, avgOut float64, n int) []domain.ReplicaMetrics {
	out := make([]domain.ReplicaMetrics, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, domain.ReplicaMetrics{
			AvgITL: itl, AvgInputTokens: measuredAvgIn, AvgOutputTokens: avgOut,
		})
	}
	return out
}

var _ = Describe("estimateArrivalDemand", func() {
	It("reproduces the demand the measured workload implies", func() {
		// 14 req/s x 24.6s x 5000 tokens = 1,722,000. On the run this comes
		// from, occupancy at the same moment read 152,270 and the target
		// collapsed to one replica mid-load.
		//
		// What that floor implies in replicas: 1,722,000 / 550,758 per-replica
		// capacity = 3.13, and the optimizer sizes against the scale-down
		// boundary rather than the raw capacity, so 3.13 / 0.7 = 4.5 -- against a
		// fleet that was observed to settle near 5. The divisor is easy to drop
		// and gives 3.1, which is why it is spelled out here.
		f := estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 3),
		})
		Expect(f.Reason).To(BeEmpty())
		Expect(f.W).To(BeNumerically("~", measuredService, 0.01))
		Expect(f.Tokens).To(BeNumerically("~", 14*measuredService*5000, 1))
	})

	It("does not move when replicas are added", func() {
		// The property the whole file exists for. Occupancy falls as capacity
		// rises; this must not, or it is just another signal that decays to
		// nothing once the fleet is adequate.
		one := estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 1),
		})
		ten := estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 10),
		})
		// Tolerance, not equality: averaging over one replica and over ten sums
		// a different number of identical terms, which lands a bit apart in the
		// last place. On 1.7e6 the observed gap is ~5e-10, so anything this side
		// of a cent is asserting invariance rather than float determinism.
		Expect(ten.Tokens).To(BeNumerically("~", one.Tokens, 0.01))
	})

	It("falls back to completion rate when no EPP arrival rate exists", func() {
		// At steady state completions equal arrivals. The equality fails while a
		// queue is building -- which is the case occupancy already reads well,
		// so the floor is not what carries that decision.
		rm := metricsAt(measuredITL, measuredAvgOut, 2)
		rm[0].RequestRate = 4
		rm[1].RequestRate = 3
		f := estimateArrivalDemand(domain.AnalyzerInput{ReplicaMetrics: rm})
		Expect(f.Reason).To(BeEmpty())
		Expect(f.Lambda).To(Equal(7.0))
		Expect(f.Tokens).To(BeNumerically("~", 7*measuredService*5000, 1),
			"the fallback lambda must feed the same arithmetic, not just be recorded")
	})

	It("has no opinion rather than a guess when a term is missing", func() {
		// A fabricated floor would hold up a fleet nobody is using. Each of
		// these must yield zero and say which input was absent.
		By("no arrival rate and no completions")
		f := estimateArrivalDemand(domain.AnalyzerInput{
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 2),
		})
		Expect(f.Tokens).To(BeZero())
		Expect(f.Reason).To(ContainSubstring("arrival rate"))

		By("neither service time nor inter-token latency")
		f = estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(0, measuredAvgOut, 2),
		})
		Expect(f.Tokens).To(BeZero())
		Expect(f.Reason).To(ContainSubstring("inter-token latency"))

		By("no output length")
		f = estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, 0, 2),
		})
		Expect(f.Tokens).To(BeZero())
		Expect(f.Reason).To(ContainSubstring("output length"))
	})

	It("takes the median timing unweighted across the replicas that reported one", func() {
		// These are per-request costs of the same hardware and model, so every
		// serving replica measures the same quantity and none should count for
		// more. A replica that has completed nothing reports zero and must not
		// drag the estimate down -- through it, the floor.
		//
		// The values are chosen so median and mean DISAGREE. An earlier version
		// of this case used {0.020, 0.030, 0}, whose surviving pair has a median
		// of 0.025 and a mean of 0.025 as well: it passed against the mean this
		// test was rewritten to rule out, and so asserted nothing about the name
		// it carries. Four survivors skewed high put the mean at 0.045 and the
		// (lower) median at 0.030.
		rm := []domain.ReplicaMetrics{
			{AvgITL: 0.020, AvgOutputTokens: measuredAvgOut},
			{AvgITL: 0.030, AvgOutputTokens: measuredAvgOut},
			{AvgITL: 0.040, AvgOutputTokens: measuredAvgOut},
			{AvgITL: 0.090, AvgOutputTokens: measuredAvgOut},
			{AvgITL: 0, AvgOutputTokens: measuredAvgOut},
		}
		Expect(medianOf(rm, func(m domain.ReplicaMetrics) float64 { return m.AvgITL })).
			To(BeNumerically("~", 0.030, 1e-9))
	})

	It("takes the LOWER of the two central values, so two replicas are still guarded", func() {
		// The one arrangement where "fewer than half the replicas are affected"
		// can never hold: with two replicas, one bad reading IS half. Averaging
		// the central pair would return ~260,696s here -- the mean, and the
		// incident all over again on a two-replica variant. The lower median
		// returns the sane replica's value.
		rm := []domain.ReplicaMetrics{
			{AvgServiceTime: 24.6},
			{AvgServiceTime: 521368},
		}
		Expect(medianOf(rm, func(m domain.ReplicaMetrics) float64 { return m.AvgServiceTime })).
			To(BeNumerically("~", 24.6, 1e-9))
	})

	It("ignores a single replica's wildly misreported timing entirely", func() {
		// The motivating incident: one of ten decode replicas' own
		// service-time metric implied ~145 hours per request (a vLLM-side
		// artifact), which a mean would have handed 1/10 of the weight --
		// enough to inflate the fleet estimate ~590x on its own. The median
		// must not move at all for one outlier among ten sane values.
		rm := make([]domain.ReplicaMetrics, 0, 10)
		for i := 0; i < 9; i++ {
			rm = append(rm, domain.ReplicaMetrics{AvgServiceTime: 24.6})
		}
		rm = append(rm, domain.ReplicaMetrics{AvgServiceTime: 521368})
		Expect(medianOf(rm, func(m domain.ReplicaMetrics) float64 { return m.AvgServiceTime })).
			To(BeNumerically("~", 24.6, 1e-9))
	})

	It("ignores a single replica's misreported token shape too", func() {
		// Token shape reaches the floor through the same multiplication as the
		// timing does -- lambda x W x (avgIn + avgOut) -- so a replica that can
		// misreport one can misreport the other to the same effect. Here one of
		// five replicas claims a 250x output length; against the unweighted mean
		// this used to use, that alone would raise the floor ~50x.
		//
		// Unlike the timing, this cannot be gated at the collector: the same two
		// fields price a starting pod's waiting queue per-replica, where zeroing
		// them would erase real demand. The floor aggregates across replicas, so
		// the floor is where it is handled.
		rm := metricsAt(measuredITL, measuredAvgOut, 5)
		rm[2].AvgOutputTokens = 250000

		f := estimateArrivalDemand(domain.AnalyzerInput{ArrivalRate: 14, ReplicaMetrics: rm})
		clean := estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 5),
		})
		Expect(f.Reason).To(BeEmpty())
		Expect(f.TokensPerRequest).To(BeNumerically("~", measuredAvgIn+measuredAvgOut, 1e-9))
		Expect(f.Tokens).To(BeNumerically("~", clean.Tokens, 0.01))
	})

	It("skips a replica whose scrape is stale, rather than outvoting it", func() {
		// Staleness is not an outlier, so the median is the wrong tool for it:
		// a fleet whose scrape breaks while it is busy goes stale TOGETHER, at
		// values that are internally consistent and all wrong. Here the two
		// stale replicas are the majority, so a median that counted them would
		// return their value; skipping them leaves only the fresh one.
		fresh := &domain.ReplicaMetricsMetadata{FreshnessStatus: "fresh"}
		stale := &domain.ReplicaMetricsMetadata{FreshnessStatus: "stale"}
		rm := []domain.ReplicaMetrics{
			{AvgServiceTime: 900, Metadata: stale},
			{AvgServiceTime: 900, Metadata: stale},
			{AvgServiceTime: 24.6, Metadata: fresh},
		}
		Expect(medianOf(rm, func(m domain.ReplicaMetrics) float64 { return m.AvgServiceTime })).
			To(BeNumerically("~", 24.6, 1e-9))
	})

	It("counts a replica with no metadata at all", func() {
		// Absent metadata is not a staleness verdict. Rows arrive without it
		// from paths that never set it, and reading nil as stale would silently
		// empty the sample and take the floor to zero.
		rm := []domain.ReplicaMetrics{{AvgServiceTime: 24.6}, {AvgServiceTime: 24.6}}
		Expect(medianOf(rm, func(m domain.ReplicaMetrics) float64 { return m.AvgServiceTime })).
			To(BeNumerically("~", 24.6, 1e-9))
	})

	It("prefers the engine's measured service time over the reconstruction", func() {
		// The reconstruction is decode-only: it multiplies output length by
		// inter-token latency and so cannot see prefill. Where the engine
		// publishes service time -- vLLM directly, SGLang as e2e minus queue --
		// that value already includes prefill and must win, or a prefill-heavy
		// workload is sized on decode alone and under-provisioned.
		rm := metricsAt(measuredITL, measuredAvgOut, 2)
		for i := range rm {
			rm[i].AvgServiceTime = 40 // deliberately unlike avgOut x ITL (24.6s)
		}
		f := estimateArrivalDemand(domain.AnalyzerInput{ArrivalRate: 14, ReplicaMetrics: rm})
		Expect(f.WSource).To(Equal("measured"))
		Expect(f.W).To(BeNumerically("~", 40, 1e-9))
		Expect(f.Tokens).To(BeNumerically("~", 14*40*5000, 1))
	})

	It("falls back to the reconstruction when no service time is published", func() {
		// An engine or version that reports inter-token latency but no service
		// time still gets a floor, rather than none at all.
		f := estimateArrivalDemand(domain.AnalyzerInput{
			ArrivalRate:    14,
			ReplicaMetrics: metricsAt(measuredITL, measuredAvgOut, 2),
		})
		Expect(f.WSource).To(Equal("itl"))
		Expect(f.W).To(BeNumerically("~", measuredService, 0.01))
	})
})

var _ = Describe("raiseRoleDemandTo", func() {
	It("preserves the measured split while raising the total", func() {
		// The floor is model-level: the scheduler reports arrivals with no role
		// breakdown, so there is no basis for attributing it to prefill or
		// decode. Scaling keeps whatever split was measured rather than
		// inventing a P/D model.
		rd := map[string]float64{"prefill": 100, "decode": 300}
		raiseRoleDemandTo(rd, 800)
		Expect(rd["prefill"]).To(BeNumerically("~", 200, 1e-9))
		Expect(rd["decode"]).To(BeNumerically("~", 600, 1e-9))
		Expect(rd["prefill"] + rd["decode"]).To(BeNumerically("~", 800, 1e-9))
	})

	It("leaves a split that already meets the total alone", func() {
		rd := map[string]float64{"decode": 900}
		raiseRoleDemandTo(rd, 800)
		Expect(rd["decode"]).To(Equal(900.0))
	})

	It("spreads evenly when the measured split is all zeros", func() {
		// Every role idle but the load still arriving: proportional scaling has
		// nothing to work from, and dropping the floor here would lose it in
		// exactly the state it is meant to cover.
		rd := map[string]float64{"prefill": 0, "decode": 0}
		raiseRoleDemandTo(rd, 500)
		Expect(rd["prefill"]).To(Equal(250.0))
		Expect(rd["decode"]).To(Equal(250.0))
	})

	It("gives a role measuring zero no share, deliberately", func() {
		// Scaling is proportional, and a role with no demand has no share to
		// grow. A P/D fleet whose prefill momentarily reports nothing therefore
		// gets no floor on that role for that cycle. The alternative is to invent
		// demand for a role that reports none, which is the fabrication this file
		// refuses everywhere else; the next cycle that measures prefill at all
		// gives it a share.
		rd := map[string]float64{"prefill": 0, "decode": 300}
		raiseRoleDemandTo(rd, 800)
		Expect(rd["prefill"]).To(BeZero())
		Expect(rd["decode"]).To(BeNumerically("~", 800, 1e-9))
	})

	It("is a no-op for a non-disaggregated model", func() {
		// aggregateRoleDemand returns nil there, and the model-level TotalDemand
		// carries the decision on its own.
		Expect(func() { raiseRoleDemandTo(nil, 500) }).NotTo(Panic())
	})
})

// The helpers above test the estimate in isolation. These drive the whole
// analyzer, because that is where the floor actually has to land: on
// TotalDemand, and on RoleDemand for any fleet whose variants carry a role --
// which is the only signal the optimizer reads for such a fleet. Nothing in the
// suite set ArrivalRate before this, so the branch that applies the floor was
// never taken by any test, new or old.
var _ = Describe("the floor, through Analyze", func() {
	newAnalyzer := func() *SaturationAnalyzer {
		return NewSaturationAnalyzer(NewCapacityKnowledgeStore())
	}

	// One replica, lightly loaded: occupancy is small, which is the state that
	// makes the fleet look idle while the load is anything but.
	idleish := func() ([]domain.ReplicaMetrics, []domain.VariantReplicaState) {
		rm := makeReplicaMetrics("pod-0", "v1", 10_000, 600_000, 0, measuredAvgIn, measuredAvgOut)
		rm.AvgServiceTime = 24.6
		return []domain.ReplicaMetrics{rm},
			[]domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 1}}
	}

	It("raises TotalDemand when the load implies more than occupancy shows", func() {
		rm, vs := idleish()
		in := makeAnalyzerInput(rm, vs)
		in.ArrivalRate = 14

		res, err := newAnalyzer().Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())

		// 14 req/s x 24.6s x 5000 tok. Occupancy alone would have reported the
		// 10,000 resident tokens and sized the fleet from that.
		Expect(res.TotalDemand).To(BeNumerically("~", 14*24.6*5000, 1))
	})

	It("leaves demand alone when occupancy already exceeds the floor", func() {
		// The property that makes this a floor rather than a replacement. A
		// heavily loaded replica reports far more resident KV than a trickle of
		// arrivals implies, and that larger number must survive.
		rm := makeReplicaMetrics("pod-0", "v1", 500_000, 600_000, 0, measuredAvgIn, measuredAvgOut)
		rm.AvgServiceTime = 24.6
		in := makeAnalyzerInput([]domain.ReplicaMetrics{rm},
			[]domain.VariantReplicaState{{VariantName: "v1", CurrentReplicas: 1, GPUsPerReplica: 1}})
		in.ArrivalRate = 0.01 // floor = 0.01 x 24.6 x 5000 = 1,230 tokens

		res, err := newAnalyzer().Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())
		// Pinned, not bounded: occupancy here is exactly the 500,000 resident
		// tokens, so anything else means the floor leaked into a value it was
		// only supposed to compare against. A ">" bound would pass on a partial
		// application landing anywhere above 100k.
		Expect(res.TotalDemand).To(BeNumerically("~", 500_000, 1),
			"a tiny arrival rate must not pull demand down to its own floor")
	})

	It("carries the floor into RoleDemand for a fleet whose variants have roles", func() {
		// A disaggregated fleet reads per-role spare, not the model-level signal.
		// A floor that raised only TotalDemand would change nothing for it.
		p := makeReplicaMetrics("pod-p", "vp", 5_000, 600_000, 0, measuredAvgIn, measuredAvgOut)
		p.AvgServiceTime = 24.6
		d := makeReplicaMetrics("pod-d", "vd", 5_000, 600_000, 0, measuredAvgIn, measuredAvgOut)
		d.AvgServiceTime = 24.6
		in := makeAnalyzerInput([]domain.ReplicaMetrics{p, d}, []domain.VariantReplicaState{
			{VariantName: "vp", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill},
			{VariantName: "vd", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
		})
		in.ArrivalRate = 14

		res, err := newAnalyzer().Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RoleDemand).NotTo(BeEmpty(), "a P/D fleet must report per-role demand")

		var sum float64
		for _, v := range res.RoleDemand {
			sum += v
		}
		Expect(sum).To(BeNumerically("~", res.TotalDemand, 1),
			"roles must still sum to the floored total, or the two signals disagree")
	})

	It("does not floor a model with no arrival signal at all", func() {
		// No EPP and no completions: the estimate has nothing to work from, and
		// must leave the fleet to occupancy rather than invent pressure on it.
		rm, vs := idleish()
		rm[0].RequestRate = 0
		in := makeAnalyzerInput(rm, vs)
		in.ArrivalRate = 0

		res, err := newAnalyzer().Analyze(context.Background(), in)
		Expect(err).NotTo(HaveOccurred())
		// Exactly the fixture's resident tokens. A "<" bound would pass even if a
		// stray lambda produced a floor, so long as it happened to land low.
		Expect(res.TotalDemand).To(BeNumerically("~", 10_000, 1),
			"demand should still be the measured occupancy, not a fabricated floor")
	})
})
