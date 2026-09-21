package saturation_v2

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/floor"
)

// The saturation analyzer's signal code -- the per-replica capacity record,
// the capacity store and its window, the throughput floor's arithmetic --
// lives in internal/signals (engine-structure proposal, stage 1). These
// names keep this package's vocabulary, so the analyzer and its tests read
// as they did.
type (
	// ReplicaCapacity is floor.ReplicaCapacity.
	ReplicaCapacity = floor.ReplicaCapacity
	// CapacityRecord is capacity.Record.
	CapacityRecord = capacity.Record
	// CapacityKnowledgeStore is capacity.Store.
	CapacityKnowledgeStore = capacity.Store
	// EngineParams is capacity.EngineParams.
	EngineParams = capacity.EngineParams

	rollingAverage = capacity.RollingAverage
	k2Source       = capacity.K2Source
)

const (
	// RollingAverageWindowSize is capacity.RollingAverageWindowSize.
	RollingAverageWindowSize = capacity.RollingAverageWindowSize
	// HistoryEvictionTimeout is capacity.HistoryEvictionTimeout.
	HistoryEvictionTimeout = capacity.HistoryEvictionTimeout
	// BacklogDrainSeconds is floor.BacklogDrainSeconds.
	BacklogDrainSeconds = floor.BacklogDrainSeconds
	// MinThroughputSamplesToOrder is floor.MinThroughputSamplesToOrder.
	MinThroughputSamplesToOrder = floor.MinThroughputSamplesToOrder

	learnedFromLive = capacity.LearnedFromLive
	k2SrcObserved   = capacity.K2SrcObserved
	k2SrcHistorical = capacity.K2SrcHistorical
	k2SrcDerived    = capacity.K2SrcDerived
	k2SrcFallback   = capacity.K2SrcFallback
)

var k2Labels = capacity.K2Labels

// NewCapacityKnowledgeStore creates an empty capacity store.
func NewCapacityKnowledgeStore() *CapacityKnowledgeStore {
	return capacity.NewStore()
}
