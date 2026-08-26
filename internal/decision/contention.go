package decision

import (
	"sync"
	"time"
)

// GPUContention records which (namespace, accelerator) pairs currently have a
// variant that wanted GPUs and was denied them.
//
// It exists so the warm pool can get out of the way. The pool takes accelerators
// by growing its own Deployment, which no limiter bounds -- the optimizer's
// budgets govern what MODEL replicas may have, and the pool is not a model. On a
// contended cluster that puts the pool in competition with the very scale-up it
// exists to bridge: a burst blocks borrows, the pool asks for another Pod to
// relieve the block, and that Pod may be the GPU a starved replica needed.
//
// The rule this enables is one-directional and deliberately modest: while a
// variant of the pool's accelerator type is being denied GPUs, the pool does not
// GROW. It keeps what it holds -- a warm copy already loaded is worth more than
// the GPU it sits on is worth to a replica that would take ~35 s to use it -- and
// it does not compete for more.
//
// Not published as a metric label or a status: this is one component telling
// another what it just observed, which is what this package is for.
type GPUContention struct {
	// Limited is the set of accelerator types, per namespace, that denied a
	// variant its GPUs on the last optimizer pass.
	Limited map[string]map[string]bool
	// UpdatedAt dates the observation. A stale reading must not hold a pool
	// down forever -- see Contended.
	UpdatedAt time.Time
}

// ContentionStore holds the latest contention snapshot.
type ContentionStore struct {
	mu   sync.RWMutex
	last *GPUContention
}

// NewContentionStore returns an empty store.
func NewContentionStore() *ContentionStore { return &ContentionStore{} }

// DefaultContention is the process-wide store.
var DefaultContention = NewContentionStore()

// Publish replaces the snapshot. An EMPTY map is a real answer -- it says the
// last pass denied nobody -- and must overwrite a previous non-empty one, or a
// single contended pass would pin every pool forever.
func (s *ContentionStore) Publish(limited map[string]map[string]bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = &GPUContention{Limited: limited, UpdatedAt: now}
}

// Get returns the snapshot and whether one has ever been published.
func (s *ContentionStore) Get() (*GPUContention, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.last == nil {
		return nil, false
	}
	return s.last, true
}

// Contended reports whether a variant in this namespace was denied GPUs of this
// accelerator type recently enough to still be believed.
//
// Three ways to answer no, and each is deliberate:
//
//   - nothing published yet. The optimizer has not run, so there is no evidence
//     of contention and none of its absence either. A pool that refused to grow
//     on no evidence would never grow on a cluster where the optimizer is not
//     running at all.
//   - the snapshot is older than maxAge. A pool held down by a reading nobody
//     has refreshed is held down forever, and the failure would be invisible.
//   - an accelerator this pool does not hold, or a namespace it is not in.
//     Contention for A100s says nothing about an H100 pool.
//
// An UNKNOWN accelerator -- the pool could not read its nodes -- matches
// nothing, so the pool grows as it did before. That is the same
// unknown-does-not-block rule the fit check uses, and for the same reason:
// a check that cannot see should not be the thing that stops the feature.
func (s *ContentionStore) Contended(namespace, accelerator string, maxAge time.Duration, now time.Time) bool {
	if accelerator == "" {
		return false
	}
	snapshot, ok := s.Get()
	if !ok {
		return false
	}
	if maxAge > 0 && now.Sub(snapshot.UpdatedAt) > maxAge {
		return false
	}
	return snapshot.Limited[namespace][accelerator]
}

// PublishGPUContention records a contention snapshot in the default store.
func PublishGPUContention(limited map[string]map[string]bool, now time.Time) {
	DefaultContention.Publish(limited, now)
}

// GPUContended reports contention from the default store.
func GPUContended(namespace, accelerator string, maxAge time.Duration, now time.Time) bool {
	return DefaultContention.Contended(namespace, accelerator, maxAge, now)
}
