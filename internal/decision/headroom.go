package decision

import (
	"sync"
	"time"
)

// Headroom is how many GPUs of an accelerator a namespace may still take under
// whatever limiter is binding it.
//
// It exists for the warm pool. A pool asks KEDA for a size, and without this it
// asks for the size its reserve implies whether or not the namespace can afford
// it — the Pods are then created, sit Pending for want of a GPU or are refused
// by a quota, and the pool reports itself short forever while the cluster has
// nothing to give it. Pending replicas are not a queue here; they are a pool
// that will never fill.
//
// Contention (see ContentionStore) answers a different question. It says a model
// replica is BEING DENIED right now, which is a reason to yield ground already
// held. This says the allowance is exhausted, which is a reason not to ask in
// the first place. A namespace can be uncontended and still have no headroom.
type Headroom struct {
	// Free is namespace → accelerator → GPUs still available. A namespace absent
	// from the map is unconstrained by the binding limiter; an accelerator absent
	// from a present namespace is denied, because a namespace-scoped quota is a
	// closed allowlist.
	Free map[string]map[string]int
	// At is when this was computed. A stale reading is worse than none: the pool
	// would size itself against an allowance that has since been spent.
	At time.Time
}

// HeadroomStore holds the most recent headroom snapshot.
type HeadroomStore struct {
	mu   sync.RWMutex
	last *Headroom
}

// NewHeadroomStore returns an empty store.
func NewHeadroomStore() *HeadroomStore { return &HeadroomStore{} }

// Publish records a snapshot, replacing any previous one wholesale so an
// allowance that shrank is not remembered as larger.
func (s *HeadroomStore) Publish(free map[string]map[string]int, now time.Time) {
	copied := make(map[string]map[string]int, len(free))
	for ns, perType := range free {
		inner := make(map[string]int, len(perType))
		for k, v := range perType {
			inner[k] = v
		}
		copied[ns] = inner
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = &Headroom{Free: copied, At: now}
}

// Get returns the latest snapshot.
func (s *HeadroomStore) Get() (*Headroom, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.last == nil {
		return nil, false
	}
	return s.last, true
}

// Reset clears the store. For tests.
func (s *HeadroomStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = nil
}

// Available reports how many GPUs of this accelerator the namespace may still
// take, and whether that figure is usable at all.
//
// Answers false — meaning "do not act on this" — when there is no snapshot, when
// the snapshot is older than maxAge, or when no limiter constrains this
// namespace. A caller that cannot get an answer must not treat that as zero: a
// pool held at its current size because nobody has published a limit yet would
// never grow on a cluster that has no limiter at all.
func (s *HeadroomStore) Available(namespace, accelerator string, maxAge time.Duration, now time.Time) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.last == nil || now.Sub(s.last.At) > maxAge {
		return 0, false
	}
	perType, ok := s.last.Free[namespace]
	if !ok {
		// Unconstrained: this namespace carries no cap under the binding limiter.
		return 0, false
	}
	// PRESENT namespace, ABSENT accelerator is a real answer and it is zero: a
	// namespace-scoped quota is a closed allowlist, so an accelerator it does not
	// name is one the namespace may not use at all.
	return perType[accelerator], true
}

// DefaultHeadroom is the process-wide store. Published by the allocation layer,
// which is the only component that sees every constraint provider at once.
var DefaultHeadroom = NewHeadroomStore()

// PublishHeadroom records a snapshot in the default store.
func PublishHeadroom(free map[string]map[string]int, now time.Time) {
	DefaultHeadroom.Publish(free, now)
}

// GPUHeadroom reads the default store. See HeadroomStore.Available.
func GPUHeadroom(namespace, accelerator string, maxAge time.Duration, now time.Time) (int, bool) {
	return DefaultHeadroom.Available(namespace, accelerator, maxAge, now)
}
