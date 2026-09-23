package steadystate

import (
	"strings"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// blockedModelRef is the identity needed to delete a model's
// wva_model_scaling_blocked series once the model is gone, plus the reasons last
// published for it so a change can be logged without repeating on every cycle.
type blockedModelRef struct {
	namespace string
	modelID   string
	// reasons is the last published set, joined, purely for comparison.
	reasons string
}

// recordBlockedModel notes that this engine has published reasons for a model,
// so pruneBlockedModels can find the series again after the model disappears,
// and reports whether the set CHANGED since the previous cycle.
//
// The bookkeeping doubles as the transition state deliberately. A separate map
// would need its own eviction, on a process that runs for weeks and would
// otherwise retain an entry for every model it has ever seen; sharing this one
// means prune and evict already cover it.
func (e *Engine) recordBlockedModel(namespace, modelID string, reasons []string) (changed bool) {
	if e.lastBlockedModels == nil {
		e.lastBlockedModels = make(map[string]blockedModelRef)
	}
	key := utils.GetNamespacedKey(namespace, modelID)
	joined := strings.Join(reasons, ",")

	prev, seen := e.lastBlockedModels[key]
	// A model observed for the first time is a change only if something is
	// actually blocking it — otherwise every restart would log a line per healthy
	// model to say nothing is wrong.
	changed = joined != prev.reasons || (!seen && joined != "")

	e.lastBlockedModels[key] = blockedModelRef{
		namespace: namespace,
		modelID:   modelID,
		reasons:   joined,
	}
	return changed
}

// pruneBlockedModels drops every reason series for a model that is no longer
// active, and forgets its bookkeeping.
//
// An empty activeKeys is a no-op, mirroring pruneAnalyzerSeries: a cycle that
// enumerates no models is usually transient — a collector hiccup, config not
// loaded yet — and must not be read as "every model went away". The genuinely
// empty fleet is handled by evictAllBlockedModels, on the path that has already
// proved the list succeeded.
func (e *Engine) pruneBlockedModels(activeKeys map[string]bool) {
	if len(activeKeys) == 0 || e.lastBlockedModels == nil {
		return
	}
	for modelKey, ref := range e.lastBlockedModels {
		if !activeKeys[modelKey] {
			metrics.ClearModelScalingBlocked(ref.namespace, ref.modelID)
			delete(e.lastBlockedModels, modelKey)
		}
	}
}

// evictAllBlockedModels removes every reason series this engine has published.
// Used when a cycle finds no active models at all, where the per-model prune has
// nothing to compare against. Idempotent — it empties its own bookkeeping.
func (e *Engine) evictAllBlockedModels() {
	for modelKey, ref := range e.lastBlockedModels {
		metrics.ClearModelScalingBlocked(ref.namespace, ref.modelID)
		delete(e.lastBlockedModels, modelKey)
	}
}
