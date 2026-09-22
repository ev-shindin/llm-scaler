package saturation_v2

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// The two names the engine wires the analyzer with, kept until stage 3 of the
// engine-structure proposal touches steadystate/engine.go; everything else the
// analyzer reads from internal/signals it reads under the packages' own names.

// CapacityKnowledgeStore is capacity.Store.
type CapacityKnowledgeStore = capacity.Store

// NewCapacityKnowledgeStore creates an empty capacity store.
func NewCapacityKnowledgeStore() *CapacityKnowledgeStore {
	return capacity.NewStore()
}
