package saturation_v2

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The cycle this replays is 15:01:33Z on the shape-swap P/D benchmark's cold
// pass (2026-09-18): two decode replicas over the queue threshold at about
// their k1, one prefill replica showing queue 30 and 357 800 resident tokens
// because decode could not admit what it had prefilled. Before the gate the
// analyzer recorded that as prefill's k2 (against a k1 of 919 449 here) and
// a prefill mu of 4.77-5.57 req/s, ordered a second prefill replica on the
// spot, and held both for the rest of the run on lambda / mu = 1.08 while
// their resident KV read zero.
var _ = Describe("a prefill saturation under a saturated decode", func() {
	var (
		analyzer *SaturationAnalyzer
		ctx      context.Context
	)
	BeforeEach(func() {
		analyzer = NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		ctx = context.Background()
	})

	const (
		prefillVariant = "prefill-v"
		prefillKv      = int64(1_149_312)
		prefillK1      = 919_449.0 // prefillKv x 0.8, truncated
		prefillKey     = "test-model|H200|1|prefill|short|q5"
		decodeKey      = "test-model|H200|1|decode|long|q5"
	)
	decode := func(pod string, tokensInUse int64, queue int) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, decodeVariant, tokensInUse, runKvCapacity, queue, 6000, 1000)
		rm.RequestRate = 4.73
		rm.Ready = true
		return rm
	}
	// The prefill row as the run showed it: queue 30, 357 800 resident.
	prefill := func() domain.ReplicaMetrics {
		rm := makeReplicaMetrics("prefill-0", prefillVariant, 357_800, prefillKv, 30, 6000, 1)
		rm.RequestRate = 4.77
		rm.Ready = true
		return rm
	}
	states := []domain.VariantReplicaState{
		{VariantName: decodeVariant, Role: domain.RoleDecode, AcceleratorName: "H200", CurrentReplicas: 2, GPUsPerReplica: 1},
		{VariantName: prefillVariant, Role: domain.RolePrefill, AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1},
	}
	perReplica := func(result *domain.AnalyzerResult, variant string) float64 {
		for _, vc := range result.VariantCapacities {
			if vc.VariantName == variant {
				return vc.PerReplicaCapacity
			}
		}
		Fail("no capacity for " + variant)
		return 0
	}

	It("records neither prefill's k2 nor its throughput, and prices prefill at k1", func() {
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 970_475, 36),
			decode("decode-1", 1_039_474, 81),
			prefill(),
		}, states)
		in.ArrivalRate = runLambda
		var result *domain.AnalyzerResult
		for i := 0; i < 4; i++ { // the four cycles the run showed it for
			var err error
			result, err = analyzer.Analyze(ctx, in)
			Expect(err).NotTo(HaveOccurred())
		}

		Expect(perReplica(result, prefillVariant)).To(Equal(prefillK1),
			"prefill stays memory-bound: 357 800 resident tokens while decode is full is decode's backlog, not prefill's capacity")
		Expect(analyzer.computeCapacityHistory).NotTo(HaveKey(prefillKey), "no k2 history for prefill")
		Expect(analyzer.saturatedThroughput).NotTo(HaveKey(prefillKey), "no mu for prefill")
		Expect(result.RoleDemand[domain.RolePrefill]).To(BeNumerically("~", 357_800+30*6000, 1),
			"prefill's demand this cycle is its resident KV plus its own queue at input-only footprint, as for any role without a mu")

		By("recording decode's saturation as decode's, in the same cycles")
		Expect(analyzer.computeCapacityHistory).To(HaveKey(decodeKey))
		Expect(analyzer.saturatedThroughput).To(HaveKey(decodeKey))
		Expect(analyzer.saturatedThroughput[decodeKey].Max()).To(Equal(4.73))
	})

	It("records prefill's saturation once decode is not saturated", func() {
		// The same prefill row on a cycle decode is keeping up in: prefill
		// is the bottleneck now, and its reading is its own.
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 300_000, 0),
			decode("decode-1", 280_000, 2),
			prefill(),
		}, states)
		in.ArrivalRate = runLambda
		result, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())

		Expect(perReplica(result, prefillVariant)).To(Equal(357_800.0))
		Expect(analyzer.computeCapacityHistory).To(HaveKey(prefillKey))
		Expect(analyzer.saturatedThroughput).To(HaveKey(prefillKey))
		Expect(analyzer.saturatedThroughput[prefillKey].Max()).To(Equal(4.77))
	})

	It("does not gate decode on prefill, nor a non-disaggregated fleet on anything", func() {
		By("a saturated decode records while prefill is saturated too")
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			decode("decode-0", 970_475, 36),
			prefill(),
		}, states)
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).To(HaveKey(decodeKey))
		Expect(analyzer.computeCapacityHistory).NotTo(HaveKey(prefillKey))

		By("a 'both' replica has no downstream, and records as before")
		both := NewSaturationAnalyzer(NewCapacityKnowledgeStore())
		rm := makeReplicaMetrics("both-0", "both-v", 900_000, runKvCapacity, 40, 6000, 1000)
		rm.Ready = true
		_, err = both.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{rm},
			[]domain.VariantReplicaState{{VariantName: "both-v", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1}}))
		Expect(err).NotTo(HaveOccurred())
		Expect(both.computeCapacityHistory).To(HaveKey("test-model|H200|1|both|long|q5"))
	})

	It("decides decode's saturation over every decode row, whatever order the rows come in", func() {
		// The prefill row first, then the saturated decode: the gate must not
		// depend on having seen decode before pricing prefill.
		in := makeAnalyzerInput([]domain.ReplicaMetrics{
			prefill(),
			decode("decode-0", 300_000, 0),
			decode("decode-1", 1_039_474, 81),
		}, states)
		_, err := analyzer.Analyze(ctx, in)
		Expect(err).NotTo(HaveOccurred())
		Expect(analyzer.computeCapacityHistory).NotTo(HaveKey(prefillKey))
	})
})

var _ = Describe("roleSaturated", func() {
	roles := map[string]string{"d": domain.RoleDecode, "p": domain.RolePrefill, "": ""}
	It("is the P1-obs admission test applied across a role", func() {
		Expect(roleSaturated([]domain.ReplicaMetrics{
			{VariantName: "d", QueueLength: 5, TokensInUse: 1},
		}, roles, domain.RoleDecode, 5)).To(BeTrue(), "at the threshold, with resident tokens")
		Expect(roleSaturated([]domain.ReplicaMetrics{
			{VariantName: "d", QueueLength: 4, TokensInUse: 1_000_000},
		}, roles, domain.RoleDecode, 5)).To(BeFalse(), "under the threshold")
		Expect(roleSaturated([]domain.ReplicaMetrics{
			{VariantName: "d", QueueLength: 50, TokensInUse: 0},
		}, roles, domain.RoleDecode, 5)).To(BeFalse(), "a queue on a replica holding nothing is not a saturation, as computeK2 would not admit it either")
		Expect(roleSaturated([]domain.ReplicaMetrics{
			{VariantName: "p", QueueLength: 50, TokensInUse: 1},
		}, roles, domain.RoleDecode, 5)).To(BeFalse(), "another role's saturation is not this one's")
		Expect(roleSaturated([]domain.ReplicaMetrics{
			{VariantName: "", QueueLength: 50, TokensInUse: 1},
		}, roles, domain.RoleBoth, 5)).To(BeTrue(), "an empty role is 'both'")
		Expect(roleSaturated([]domain.ReplicaMetrics{
			{VariantName: "d", QueueLength: 50, TokensInUse: 1, FromWarmPool: true},
		}, roles, domain.RoleDecode, 5)).To(BeTrue(), "a saturated bridge lent to decode is decode saturated")
		Expect(roleSaturated(nil, roles, domain.RoleDecode, 5)).To(BeFalse())
	})
})

// The gate's log line joins the k2-decision contract: dump_k2_decisions.py
// reads the priority field, and a reader of the run has to be able to find
// why a saturated prefill replica stayed at P4-k1.
func TestLogContract_PrefillSaturationUnderDecodeIsLabelled(t *testing.T) {
	ctx, logs := observedCtx(t)
	analyzer := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

	prefill := makeReplicaMetrics("pod-p", "variant-p", 357_800, 1_149_312, 30, 6000, 1)
	decode := makeReplicaMetrics("pod-d", "variant-d", 1_039_474, 1_162_240, 81, 6000, 1000)
	prefill.Ready, decode.Ready = true, true
	_, err := analyzer.Analyze(ctx, makeAnalyzerInput([]domain.ReplicaMetrics{prefill, decode},
		[]domain.VariantReplicaState{
			{VariantName: "variant-p", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RolePrefill},
			{VariantName: "variant-d", AcceleratorName: "H200", CurrentReplicas: 1, GPUsPerReplica: 1, Role: domain.RoleDecode},
		}))
	require.NoError(t, err)

	var seen []string
	for _, e := range logs.FilterMessage("k2-decision").All() {
		f := e.ContextMap()
		if f["variant"] != "variant-p" {
			continue
		}
		seen = append(seen, stringField(t, f, "priority"))
		if f["priority"] == k2ReasonObsDownstream {
			for _, key := range logContract["k2-decision"] {
				assert.Contains(t, f, key)
			}
			assert.Contains(t, stringField(t, f, "reason"), "decode")
			assert.EqualValues(t, 30, f["queueLength"])
		}
	}
	assert.Equal(t, []string{k2ReasonObsDownstream, "P4-k1"}, seen,
		"the gate line, then the fallback the analyzer fell through to")
	assert.Equal(t, "P1-obs", func() string {
		for _, e := range logs.FilterMessage("k2-decision").All() {
			if f := e.ContextMap(); f["variant"] == "variant-d" {
				return stringField(t, f, "priority")
			}
		}
		return ""
	}(), "decode's own saturation is recorded in the same cycle")
}
