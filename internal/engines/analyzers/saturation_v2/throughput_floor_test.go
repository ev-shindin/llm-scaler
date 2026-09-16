package saturation_v2

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
)

// Figures from the shape-swap P/D run (biran-20260915-102548-571), H200,
// Qwen3-0.6B, 6 req/s throughout. In its first phase (6000 in / 1000 out) one
// decode replica saturated at ~5.4 completions/s with 1.15M tokens resident;
// at six replicas the same load occupied ~100k tokens in total and the
// controller sized the fleet to one, which then re-saturated within a minute.
const (
	runLambda     = 6.0
	runMu         = 5.4
	runK1         = int64(929_792) // 1,162,240 x 0.8
	runKvCapacity = int64(1_162_240)
)

var _ = Describe("estimateThroughputDemand", func() {
	variants := func(role string, ready, pending int) []domain.VariantCapacity {
		return []domain.VariantCapacity{{
			VariantName: "v", Role: role, ReplicaCount: ready, PendingReplicas: pending, PerReplicaCapacity: float64(runK1),
		}}
	}
	replicas := func(n int, mu float64) []ReplicaCapacity {
		out := make([]ReplicaCapacity, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, ReplicaCapacity{VariantName: "v", SaturatedThroughput: mu})
		}
		return out
	}

	It("sizes the fleet to lambda over mu, in the variant's own tokens", func() {
		// 6 / 5.4 = 1.11 replicas' worth of demand. Through the engine's
		// RC = D / 0.85 - supply that is 1.31 replicas, so a two-replica fleet
		// holds and a one-replica fleet is (correctly) short.
		f := estimateThroughputDemand(runLambda, replicas(6, runMu), variants(domain.RoleDecode, 6, 0), 0.85)
		Expect(f.ByRole).To(HaveKey(domain.RoleDecode))
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", runLambda/runMu*float64(runK1), 1e-6))
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", 1.111, 1e-3))
		Expect(f.Terms[domain.RoleDecode].Capped).To(BeFalse())
	})

	It("does not move when replicas are added or removed", func() {
		// The property the arrival floor was supposed to have and did not:
		// mu is a per-replica constant, so the floor is the same at one
		// replica as at six.
		one := estimateThroughputDemand(runLambda, replicas(1, runMu), variants(domain.RoleDecode, 1, 0), 0)
		six := estimateThroughputDemand(runLambda, replicas(6, runMu), variants(domain.RoleDecode, 6, 0), 0)
		Expect(six.ByRole[domain.RoleDecode]).To(BeNumerically("~", one.ByRole[domain.RoleDecode], 1e-6))
	})

	It("holds a fleet but never orders one: capped at scaleUp x anticipated supply", func() {
		// A mu that under-read by 10x would imply 11 replicas. The engine
		// sizes RC = D / scaleUp - anticipated, so the largest demand that
		// orders nothing is scaleUp x anticipated; above it the floor would
		// grow the fleet every cycle for as long as the bad reading lasts.
		f := estimateThroughputDemand(runLambda, replicas(2, runMu/10), variants(domain.RoleDecode, 2, 0), 0.85)
		Expect(f.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*2*float64(runK1), 1e-6))
		Expect(f.Terms[domain.RoleDecode].Capped).To(BeTrue())
		Expect(f.Terms[domain.RoleDecode].Replicas).To(BeNumerically("~", 11.1, 0.1),
			"the uncapped figure is still reported, so a bad mu is visible")

		By("counting pending replicas in the cap, since the engine does")
		g := estimateThroughputDemand(runLambda, replicas(1, runMu/10), variants(domain.RoleDecode, 1, 1), 0.85)
		Expect(g.ByRole[domain.RoleDecode]).To(BeNumerically("~", 0.85*2*float64(runK1), 1e-6))
	})

	It("has no opinion for a role that has never been seen saturated", func() {
		rcs := append(replicas(2, runMu), ReplicaCapacity{VariantName: "p", SaturatedThroughput: 0})
		vcs := append(variants(domain.RoleDecode, 2, 0),
			domain.VariantCapacity{VariantName: "p", Role: domain.RolePrefill, ReplicaCount: 1, PerReplicaCapacity: 919_449})
		f := estimateThroughputDemand(runLambda, rcs, vcs, 0.85)
		Expect(f.ByRole).To(HaveKey(domain.RoleDecode))
		Expect(f.ByRole).NotTo(HaveKey(domain.RolePrefill))
	})

	It("leaves a bridge's throughput out", func() {
		// A warm-pool bridge runs its engine on different terms (lower
		// --gpu-memory-utilization, and it is going home); its rate is not
		// this variant's.
		rcs := []ReplicaCapacity{{VariantName: "v", SaturatedThroughput: 1, FromWarmPool: true}}
		f := estimateThroughputDemand(runLambda, rcs, variants(domain.RoleBoth, 0, 0), 0.85)
		Expect(f.ByRole).To(BeEmpty())
	})

	It("says nothing without an arrival rate", func() {
		f := estimateThroughputDemand(0, replicas(2, runMu), variants(domain.RoleBoth, 2, 0), 0.85)
		Expect(f.ByRole).To(BeEmpty())
	})
})

var _ = Describe("the saturated-throughput window", func() {
	It("keeps the max, because a saturated completion rate can only under-read", func() {
		// The readings a saturated decode replica produced on the run, in
		// order: 5.4 (a full minute saturated), then 3.3 and 3.5 under KV
		// pressure with preemptions. The mean would be 4.07 and imply 1.47
		// replicas; the replica was demonstrably completing 5.4.
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		a.recordSaturatedThroughput("k", 5.4)
		a.recordSaturatedThroughput("k", 3.3)
		a.recordSaturatedThroughput("k", 3.5)
		Expect(a.saturatedThroughputFor("k")).To(Equal(5.4))
		Expect(a.saturatedThroughputFor("other")).To(BeZero())
	})

	It("ignores a non-positive reading and is evicted with the k2 history", func() {
		a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		a.recordSaturatedThroughput("k", 0)
		Expect(a.saturatedThroughput).To(BeEmpty())
		a.recordSaturatedThroughput("k", 2)
		a.EvictStaleHistory(0)
		Expect(a.saturatedThroughput).To(BeEmpty())
	})
})

// Through Analyze, replaying the run: a decode replica saturates (P1 fires,
// mu is recorded), then the fleet grows to six and occupancy collapses.
var _ = Describe("the throughput floor, through Analyze", func() {
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
	)
	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		ctx = context.Background()
	})

	decode := func(pod string, tokensInUse int64, queue int, rate float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, "decode-v", tokensInUse, runKvCapacity, queue, 6000, 1000)
		rm.RequestRate = rate
		rm.Ready = true
		return rm
	}
	prefill := func(pod string, tokensInUse int64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, "prefill-v", tokensInUse, 1_149_312, 0, 6000, 1)
		rm.RequestRate = runLambda
		rm.Ready = true
		return rm
	}
	states := func(decodeN, prefillN int) []domain.VariantReplicaState {
		return []domain.VariantReplicaState{
			{VariantName: "decode-v", Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: decodeN, GPUsPerReplica: 1},
			{VariantName: "prefill-v", Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: prefillN, GPUsPerReplica: 1},
		}
	}

	saturate := func() {
		// 07:29:08 on the run: queue 10 over threshold 5, 1.15M resident,
		// completing 5.4/s.
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_158_912, 10, runMu), prefill("prefill-0", 66_183)},
			states(1, 1))
		in.ArrivalRate = runLambda
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
	}

	It("holds the decode role at lambda / mu once the fleet has caught up", func() {
		saturate()

		// 07:33:23 on the run: six decode replicas, ~20k tokens each, no
		// queue anywhere. Occupancy said one replica; the load needed two.
		rm := make([]domain.ReplicaMetrics, 0, 9)
		for i := 0; i < 6; i++ {
			rm = append(rm, decode(string(rune('a'+i)), 20_000, 0, 1.0))
		}
		for i := 0; i < 3; i++ {
			rm = append(rm, prefill(string(rune('p'+i)), 0))
		}
		in := makeAnalyzerInput(rm, states(6, 3))
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		var decodeP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == "decode-v" {
				decodeP = vc.PerReplicaCapacity
			}
		}
		Expect(decodeP).To(BeNumerically(">", 0))
		want := runLambda / runMu * decodeP
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", want, 1),
			"decode demand must be held at lambda / mu replicas' worth")
		Expect(result.RoleDemand[domain.RoleDecode] / decodeP).To(BeNumerically("~", 1.11, 0.01))
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("<", 0.5*919_449),
			"prefill was never seen saturated and keeps its measured demand")
		Expect(result.TotalDemand).To(BeNumerically(">=", result.RoleDemand[domain.RoleDecode]))

		// Negative control, on the same fixtures: an analyzer that never saw
		// the saturation has no mu and reports occupancy, one tenth of that.
		fresh := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		bare, err := fresh.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(bare.RoleDemand[domain.RoleDecode]).To(BeNumerically("<", want/5))
	})

	It("does not order a replica the fleet does not have", func() {
		saturate()
		// One decode replica, idle-ish, mu on record: lambda / mu = 1.11
		// replicas would be 1.31 through the engine's headroom. Capped at
		// 0.85 x one replica, the demand orders nothing.
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 200_000, 0, runLambda), prefill("prefill-0", 0)},
			states(1, 1))
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		var decodeP float64
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == "decode-v" {
				decodeP = vc.PerReplicaCapacity
			}
		}
		Expect(result.RoleDemand[domain.RoleDecode]).To(BeNumerically("~", 0.85*decodeP, 1))
		Expect(aggregation.SumTotalAnticipatedSupply(result.VariantCapacities)).To(BeNumerically(">", 0))
	})

	It("does not record a throughput from a pod that is not Ready", func() {
		in := makeAnalyzerInput(
			[]domain.ReplicaMetrics{decode("decode-0", 1_158_912, 10, runMu)},
			states(1, 0)[:1])
		in.ReplicaMetrics[0].Ready = false
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.saturatedThroughput).To(BeEmpty())
	})

	It("floors a non-disaggregated fleet on its total", func() {
		both := func(pod string, tokensInUse int64, queue int, rate float64) domain.ReplicaMetrics {
			rm := makeReplicaMetrics(pod, "v", tokensInUse, runKvCapacity, queue, 6000, 1000)
			rm.RequestRate = rate
			rm.Ready = true
			return rm
		}
		st := []domain.VariantReplicaState{{VariantName: "v", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1}}
		in := makeAnalyzerInput([]domain.ReplicaMetrics{both("a", 1_158_912, 10, runMu)}, st)
		in.ArrivalRate = runLambda
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		st[0].CurrentReplicas = 4
		in = makeAnalyzerInput([]domain.ReplicaMetrics{
			both("a", 20_000, 0, 1.5), both("b", 20_000, 0, 1.5), both("c", 20_000, 0, 1.5), both("d", 20_000, 0, 1.5)}, st)
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RoleDemand).To(BeNil())
		Expect(result.TotalDemand).To(BeNumerically("~", runLambda/runMu*result.VariantCapacities[0].PerReplicaCapacity, 1))
	})
})
