package saturation_v2

import (
	"testing"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// perReplica is the capacity every row in these tests reports, so the medians
// are uninteresting and the counts are what the assertions are about.
const perReplica = 1000

// bridgeReplica is one live row as the collector would hand it over.
func bridgeReplica(pod string, demand int64, fromPool bool) ReplicaCapacity {
	capacity := int64(perReplica)
	return ReplicaCapacity{
		PodName:               pod,
		VariantName:           "variant-a",
		TokensInUse:           demand,
		TotalKvCapacityTokens: capacity,
		MemoryBoundCapacity:   capacity,
		ComputeBoundCapacity:  capacity,
		EffectiveCapacity:     capacity,
		ReplicaDemand:         demand,
		FromWarmPool:          fromPool,
	}
}

func oneVariantState(ready int) []domain.VariantReplicaState {
	return []domain.VariantReplicaState{{
		VariantName:     "variant-a",
		CurrentReplicas: ready,
		GPUsPerReplica:  1,
	}}
}

// A bridge's LOAD is this variant's load and counts toward demand.
//
// Leave it out and demand reads lowest exactly while a bridge is covering the
// shortfall -- the pool lends a Pod, the traffic moves onto it, and the measured
// demand falls as though the load had gone away. It then reappears from nowhere
// when the Pod is handed back, which looks like a spike arriving rather than
// capacity leaving.
func TestABridgesDemandIsCountedTowardTheVariant(t *testing.T) {
	a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

	own := bridgeReplica("variant-a-0", 100, false)
	bridge := bridgeReplica("pool-0", 400, true)

	got := a.aggregateByVariant([]ReplicaCapacity{own, bridge},
		nil, oneVariantState(1), "model", "ns", 0.9, logr.Discard())

	if len(got) != 1 {
		t.Fatalf("variants = %+v, want one", got)
	}
	if got[0].TotalDemand != 500 {
		t.Errorf("TotalDemand = %v, want 500 (the variant's own 100 plus the bridge's 400): "+
			"a bridge serves this variant's traffic", got[0].TotalDemand)
	}
}

// A bridge's CAPACITY is not this variant's supply.
//
// The Pod is borrowed and goes back when the ordinary replicas arrive. Counted
// as supply it would tell the optimizer the fleet is already big enough and
// suppress the scale-up the bridge exists to cover -- after which the pool holds
// the Pod indefinitely, because the replicas that would release it are the ones
// it prevented. The pool would have talked the optimizer out of ending the
// borrow.
func TestABridgeIsNotCountedAsSupply(t *testing.T) {
	a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

	own := bridgeReplica("variant-a-0", 100, false)
	bridge := bridgeReplica("pool-0", 400, true)

	got := a.aggregateByVariant([]ReplicaCapacity{own, bridge},
		nil, oneVariantState(1), "model", "ns", 0.9, logr.Discard())

	if got[0].ReplicaCount != 1 {
		t.Errorf("ReplicaCount = %d, want 1: the bridge is borrowed and must not be supply",
			got[0].ReplicaCount)
	}
	if got[0].WarmPoolReplicas != 1 {
		t.Errorf("WarmPoolReplicas = %d, want 1", got[0].WarmPoolReplicas)
	}
	if got[0].WarmPoolCapacity != 1000 {
		t.Errorf("WarmPoolCapacity = %v, want 1000: measured, and kept out of supply "+
			"rather than not measured at all", got[0].WarmPoolCapacity)
	}
}

// A variant served ONLY by bridges reports no supply of its own, and says so.
//
// This is the retained-pool shape: no ordinary replicas, the pool IS the
// capacity. Supply must still read zero -- the optimizer sizes the fleet the
// scale target owns -- while the bridge figures say where the serving is
// actually coming from, which is what a switching decision needs.
func TestAVariantCarriedEntirelyByThePoolHasNoSupplyOfItsOwn(t *testing.T) {
	a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

	bridge := bridgeReplica("pool-0", 400, true)

	got := a.aggregateByVariant([]ReplicaCapacity{bridge},
		nil, oneVariantState(0), "model", "ns", 0.9, logr.Discard())

	if got[0].ReplicaCount != 0 {
		t.Errorf("ReplicaCount = %d, want 0: every live row was a bridge", got[0].ReplicaCount)
	}
	if got[0].TotalDemand != 400 {
		t.Errorf("TotalDemand = %v, want 400: the load is real wherever it is being served",
			got[0].TotalDemand)
	}
	if got[0].WarmPoolReplicas != 1 || got[0].WarmPoolCapacity != 1000 {
		t.Errorf("bridge supply = %d replicas / %v capacity, want 1 / 1000",
			got[0].WarmPoolReplicas, got[0].WarmPoolCapacity)
	}
}

// With no bridge anywhere, nothing changes: the counts are the variant's own and
// the bridge figures are zero rather than absent.
func TestAVariantWithNoBridgeIsUnaffected(t *testing.T) {
	a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

	got := a.aggregateByVariant([]ReplicaCapacity{
		bridgeReplica("variant-a-0", 100, false),
		bridgeReplica("variant-a-1", 150, false),
	}, nil, oneVariantState(2), "model", "ns", 0.9, logr.Discard())

	if got[0].ReplicaCount != 2 {
		t.Errorf("ReplicaCount = %d, want 2", got[0].ReplicaCount)
	}
	if got[0].TotalDemand != 250 {
		t.Errorf("TotalDemand = %v, want 250", got[0].TotalDemand)
	}
	if got[0].WarmPoolReplicas != 0 || got[0].WarmPoolCapacity != 0 {
		t.Errorf("bridge supply = %d / %v, want zero for a variant with no bridge",
			got[0].WarmPoolReplicas, got[0].WarmPoolCapacity)
	}
}

// Utilization is demand over the variant's OWN supply.
//
// Which is the point of the split, stated as a number: with a bridge carrying
// most of the load, utilization of the fleet the optimizer controls is high --
// that fleet is too small, which is exactly why a bridge was lent. Dividing by a
// supply that included the bridge would report a comfortable figure and the
// shortfall would never be acted on.
func TestUtilizationMeasuresTheVariantsOwnFleet(t *testing.T) {
	a := NewSaturationAnalyzer(NewCapacityKnowledgeStore())

	got := a.aggregateByVariant([]ReplicaCapacity{
		bridgeReplica("variant-a-0", 500, false),
		bridgeReplica("pool-0", 400, true),
	}, nil, oneVariantState(1), "model", "ns", 0.9, logr.Discard())

	// 900 of demand over the one replica the variant actually owns.
	if got[0].Utilization != 0.9 {
		t.Errorf("Utilization = %v, want 0.9 (900 demand / 1000 own capacity)", got[0].Utilization)
	}
}
