package saturation_v2

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The model total carries the scheduler queue once, as input + output, which
// is decode's share. estimateSchedulerQueueDemand also charges prefill its
// input tokens, and aggregateRoleDemand adds that to prefill's roleDemand --
// so the per-role demands sum to the total PLUS the input-token charge. A
// per-role adjustment that moves the total by the role's full delta therefore
// takes out tokens the total never held.
var _ = Describe("applyThroughputFloor and the model total", func() {
	const (
		k1      = 930508.0
		inTok   = 3_000_000.0 // the scheduler queue's input tokens
		outTok  = 1_000_000.0 // and its output tokens
		perRole = 100_000.0   // each role's own resident demand
	)

	// The fleet as the analyzer builds it: two roles, each with its own
	// resident demand, a scheduler queue charged to both, and a local queue
	// residency charge large enough that the floor lands below it -- which is
	// the ordinary case the take-back exists for.
	var (
		a         *SaturationAnalyzer
		cfg       *config.ScalingPolicy
		variants  []domain.VariantCapacity
		replicas  []capacity.ReplicaCapacity
		eppByRole map[string]float64
	)

	BeforeEach(func() {
		a = &SaturationAnalyzer{}
		cfg = &config.ScalingPolicy{}
		variants = []domain.VariantCapacity{
			{VariantName: "p", Role: domain.RolePrefill, PerReplicaCapacity: k1,
				ReplicaCount: 2, TotalDemand: perRole},
			{VariantName: "d", Role: domain.RoleDecode, PerReplicaCapacity: k1,
				ReplicaCount: 2, TotalDemand: perRole},
		}
		replicas = []capacity.ReplicaCapacity{
			{VariantName: "p", SaturatedThroughput: 5.4, SaturatedThroughputSamples: 2,
				QueueLength: 10, LocalQueueDemand: 2_000_000},
			{VariantName: "d", SaturatedThroughput: 5.4, SaturatedThroughputSamples: 2,
				QueueLength: 10, LocalQueueDemand: 2_000_000},
		}
		eppByRole = map[string]float64{
			domain.RolePrefill: inTok,
			domain.RoleDecode:  inTok + outTok,
		}
	})

	It("moves the total by prefill's non-queue share, not its whole demand", func() {
		// roleDemand and totalDemand exactly as analyzer.go builds them:
		// the total gets the queue once, each role gets its own share.
		roleDemand := map[string]float64{
			domain.RolePrefill: perRole + eppByRole[domain.RolePrefill],
			domain.RoleDecode:  perRole + eppByRole[domain.RoleDecode],
		}
		totalDemand := 2*perRole + (inTok + outTok)

		got := a.applyThroughputFloor(
			domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 6},
			cfg, replicas, variants, totalDemand, roleDemand, eppByRole, 20, false, GinkgoLogr)

		// The invariant, stated without reference to the implementation's
		// formula: the model total is the sum of the per-role demands the
		// cycle ends with. An expectation computed by re-running the
		// production expression on the post-call map is self-consistent with
		// whatever that expression happens to be, and cannot see a wrong one
		// -- which is how the first version of this fix passed its own spec
		// while discarding all of prefill's floor.
		Expect(got).To(BeNumerically("~",
			roleDemand[domain.RolePrefill]+roleDemand[domain.RoleDecode], 1e-6),
			"the model total is what the roles now need, no more and no less")
		Expect(got).To(BeNumerically(">=", 0))
		for role, v := range roleDemand {
			Expect(v).To(BeNumerically(">=", 0), "roleDemand[%s]", role)
		}
	})

	It("prices the total correctly when only prefill has ever saturated", func() {
		// The defect is not only that the total can go negative. With prefill
		// priced and decode not, the unfixed line returns 2,220,056 where the
		// roles between them need 5,220,112 -- low, and positive, so a sign
		// check passes it. The total is understated by prefill's queue share
		// whenever prefill is priced; a negative is the extreme where the
		// understatement exceeds what the total held.
		replicas[1].SaturatedThroughput = 0 // decode has never been seen saturated

		roleDemand := map[string]float64{
			domain.RolePrefill: perRole + eppByRole[domain.RolePrefill],
			domain.RoleDecode:  perRole + eppByRole[domain.RoleDecode],
		}
		totalDemand := 2*perRole + (inTok + outTok)

		got := a.applyThroughputFloor(
			domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 6},
			cfg, replicas, variants, totalDemand, roleDemand, eppByRole, 20, false, GinkgoLogr)

		Expect(got).To(BeNumerically("~",
			roleDemand[domain.RolePrefill]+roleDemand[domain.RoleDecode], 1e-6),
			"prefill's floor belongs in the total whole; decode is unpriced and keeps what it had")
	})

	It("subtracts the queue share without clamping when prefill's own demand exceeds it", func() {
		// The fixture above keeps prefill's own demand well below its queue
		// share, so the subtraction's pre-floor side barely clears zero. The
		// ordinary production shape is the other one: prefill holding more
		// resident KV than the scheduler queue's input charge.
		variants[0].TotalDemand = 10_000_000
		roleDemand := map[string]float64{
			domain.RolePrefill: variants[0].TotalDemand + eppByRole[domain.RolePrefill],
			domain.RoleDecode:  perRole + eppByRole[domain.RoleDecode],
		}
		totalDemand := variants[0].TotalDemand + perRole + (inTok + outTok)
		Expect(roleDemand[domain.RolePrefill]-eppByRole[domain.RolePrefill]).To(BeNumerically(">", 0),
			"the point of this spec is the unclamped branch; if this fails the fixture stopped reaching it")

		got := a.applyThroughputFloor(
			domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 6},
			cfg, replicas, variants, totalDemand, roleDemand, eppByRole, 20, false, GinkgoLogr)

		Expect(got).To(BeNumerically("~",
			roleDemand[domain.RolePrefill]+roleDemand[domain.RoleDecode], 1e-6))
	})

	It("holds prefill's contribution at zero if its demand is below its queue share", func() {
		// A contract test on the guard, not a reachable cycle. Through the
		// real call path the arrangement cannot arise: aggregateRoleDemand
		// builds prefill's demand as its own demand PLUS this very share,
		// from the same map, so taking the share back out returns its own
		// demand, which aggregation keeps non-negative. The guard exists
		// because that invariant is held by a caller two files away, and
		// without it a prefill entry below its share would hand the model
		// total a negative credit against decode -- the shape of the mistake
		// this fix already made once, in the other direction.
		//
		// Deleting the max(..., 0) passes every other spec in this file.
		roleDemand := map[string]float64{
			domain.RolePrefill: eppByRole[domain.RolePrefill] / 2, // below its share
			domain.RoleDecode:  perRole + eppByRole[domain.RoleDecode],
		}
		// and so the model total carries nothing for prefill: decode's demand
		// is the whole of it.
		totalDemand := roleDemand[domain.RoleDecode]

		got := a.applyThroughputFloor(
			domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 6},
			cfg, replicas, variants, totalDemand, roleDemand, eppByRole, 20, false, GinkgoLogr)

		Expect(got).To(BeNumerically("~",
			roleDemand[domain.RolePrefill]+roleDemand[domain.RoleDecode], 1e-6),
			"prefill held nothing for the total, so the total loses nothing for it")
		Expect(got).To(BeNumerically(">=", 0))
	})

	It("leaves the non-prefill path exactly as it was", func() {
		// The correction is prefill's alone. This spec passes both with and
		// without the fix, by design: contributionToTotal is the identity for
		// every role but prefill, so the decode path must be untouched. It is
		// the guard against broadening the clamp, not evidence the fix works
		// -- the spec above is that.
		roleDemand := map[string]float64{
			domain.RoleDecode: perRole + eppByRole[domain.RoleDecode],
		}
		onlyDecode := []domain.VariantCapacity{variants[1]}
		onlyDecodeReps := []capacity.ReplicaCapacity{replicas[1]}
		totalDemand := perRole + (inTok + outTok)

		before := roleDemand[domain.RoleDecode]
		got := a.applyThroughputFloor(
			domain.AnalyzerInput{ModelID: "m", Namespace: "n", ArrivalRate: 6},
			cfg, onlyDecodeReps, onlyDecode, totalDemand, roleDemand,
			map[string]float64{domain.RoleDecode: eppByRole[domain.RoleDecode]}, 20, false, GinkgoLogr)

		Expect(got).To(BeNumerically("~", totalDemand+(roleDemand[domain.RoleDecode]-before), 1e-6),
			"decode contributes its whole demand to the total, so its whole delta must reach it")
	})
})
