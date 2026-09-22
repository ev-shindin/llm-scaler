package saturation_v2

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// ObservedReplicas is the count of rows the analyzer attributed to a variant
// this cycle, and nothing else: not scale-target status, and not capped at
// what the target owns. Both halves are load-bearing for the metric built on
// it -- a status-derived value would read "saw the whole fleet" for a variant
// nothing was scraped from, and a capped one could never show an unowned
// replica serving.
var _ = Describe("ObservedReplicas", func() {
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
	)

	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(capacity.NewStore())
		ctx = context.Background()
	})

	It("counts the rows that reported, beyond what the scale target owns", func() {
		metrics := []domain.ReplicaMetrics{
			makeReplicaMetrics("pod-1", "decode", 1000, 16000, 0, 100, 50),
			makeReplicaMetrics("pod-2", "decode", 1000, 16000, 0, 100, 50),
			makeReplicaMetrics("pod-3", "decode", 1000, 16000, 0, 100, 50),
		}
		input := makeAnalyzerInput(metrics, []domain.VariantReplicaState{
			{VariantName: "decode", CurrentReplicas: 2, PendingReplicas: 0, GPUsPerReplica: 1},
		})

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))
		// Three reported while the target owns two: the third is the unowned
		// replica the conceded-supply spec stages, and this is the only number
		// that can show it.
		Expect(result.VariantCapacities[0].ObservedReplicas).To(Equal(3))
	})

	It("is zero when nothing reported, even though status says replicas are ready", func() {
		input := makeAnalyzerInput(nil, []domain.VariantReplicaState{
			{VariantName: "decode", CurrentReplicas: 2, PendingReplicas: 0, GPUsPerReplica: 1},
		})

		result, err := analyzer.Analyze(ctx, input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.VariantCapacities).To(HaveLen(1))
		vc := result.VariantCapacities[0]
		// ReplicaCount falls back to the target's ready count so supply is not
		// zero; ObservedReplicas must not follow it, or a broken PodMonitor
		// reads as a fully scraped fleet.
		Expect(vc.ReplicaCount).To(Equal(2), "ReplicaCount is the status fallback")
		Expect(vc.ObservedReplicas).To(BeZero(), "nothing reported, so nothing was observed")
	})
})
