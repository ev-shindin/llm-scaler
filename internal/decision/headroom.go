package decision

import (
	"maps"
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
	// PoolsVersion is the version of the warm-pool figure (WarmPoolGPUsVersion)
	// the usage behind this snapshot was built from. A pool compares it with
	// the version its own last publish produced: a snapshot built from an older
	// figure still credits the namespace with GPUs the pool has since taken, and
	// growing on it is how a pool overshot a one-GPU quota to three Pods on the
	// pass it first saw its Pod.
	PoolsVersion uint64
}

// HeadroomState is what a reading of the store means for a caller deciding
// whether to take more GPUs.
type HeadroomState int

const (
	// HeadroomUnknown: no usable answer -- no snapshot, one too old, or one built
	// before the caller's own consumption was charged. Do not grow on it; do not
	// read it as zero either.
	HeadroomUnknown HeadroomState = iota
	// HeadroomUnbounded: a current snapshot in which no limiter constrains this
	// namespace. Grow freely.
	HeadroomUnbounded
	// HeadroomBounded: a current snapshot names this namespace; Free is what is
	// left, and zero is a real zero.
	HeadroomBounded
)

// String names the state for logs.
func (s HeadroomState) String() string {
	switch s {
	case HeadroomUnbounded:
		return "unbounded"
	case HeadroomBounded:
		return "bounded"
	}
	return "unknown"
}

// HeadroomStore holds the most recent headroom snapshot.
type HeadroomStore struct {
	mu   sync.RWMutex
	last *Headroom
}

// NewHeadroomStore returns an empty store.
func NewHeadroomStore() *HeadroomStore { return &HeadroomStore{} }

// Publish records a snapshot, replacing any previous one wholesale so an
// allowance that shrank is not remembered as larger. poolsVersion says which
// warm-pool figure the usage behind free was built from (see
// Headroom.PoolsVersion); a publisher that read no pool figure passes 0, which
// no pool that has ever published can match, so such a snapshot never lets a
// pool grow.
func (s *HeadroomStore) Publish(free map[string]map[string]int, poolsVersion uint64, now time.Time) {
	// The outer map by hand and the inner ones with maps.Clone, which is shallow.
	copied := make(map[string]map[string]int, len(free))
	for ns, perType := range free {
		copied[ns] = maps.Clone(perType)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = &Headroom{Free: copied, At: now, PoolsVersion: poolsVersion}
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
//
// Reading is the finer answer, and the one the warm pool takes; this remains
// for callers that only need "may I, and how much".
func (s *HeadroomStore) Available(namespace, accelerator string, maxAge time.Duration, now time.Time) (int, bool) {
	free, state := s.Reading(namespace, accelerator, 0, maxAge, now)
	return free, state == HeadroomBounded
}

// Reading reports the namespace's remaining allowance of an accelerator as one
// of three states (see HeadroomState), for a snapshot no older than maxAge that
// was built from a warm-pool figure at least as new as poolsVersion.
//
// The version check is what closes the race that let a pool overshoot its
// quota: the pool charges its Pods and, on the same pass, asks whether it may
// grow. A snapshot built from the figure BEFORE that charge still credits the
// namespace with those GPUs, and answering from it grants the pool its own Pods
// twice. Such a snapshot reads as HeadroomUnknown, and the pool holds until the
// next optimize pass has seen the charge -- one cycle, not forever.
//
// A PRESENT namespace with an ABSENT accelerator is a real answer and it is
// zero: a namespace-scoped quota is a closed allowlist, so an accelerator it
// does not name is one the namespace may not use at all.
func (s *HeadroomStore) Reading(namespace, accelerator string, poolsVersion uint64, maxAge time.Duration, now time.Time) (int, HeadroomState) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.last == nil || now.Sub(s.last.At) > maxAge || s.last.PoolsVersion < poolsVersion {
		return 0, HeadroomUnknown
	}
	perType, ok := s.last.Free[namespace]
	if !ok {
		return 0, HeadroomUnbounded
	}
	return perType[accelerator], HeadroomBounded
}

// DefaultHeadroom is the process-wide store. Published by the allocation layer,
// which is the only component that sees every constraint provider at once.
var DefaultHeadroom = NewHeadroomStore()

// PublishHeadroom records a snapshot in the default store.
func PublishHeadroom(free map[string]map[string]int, poolsVersion uint64, now time.Time) {
	DefaultHeadroom.Publish(free, poolsVersion, now)
}

// GPUHeadroom reads the default store. See HeadroomStore.Available.
func GPUHeadroom(namespace, accelerator string, maxAge time.Duration, now time.Time) (int, bool) {
	return DefaultHeadroom.Available(namespace, accelerator, maxAge, now)
}

// GPUHeadroomReading reads the default store. See HeadroomStore.Reading.
func GPUHeadroomReading(namespace, accelerator string, poolsVersion uint64, maxAge time.Duration, now time.Time) (int, HeadroomState) {
	return DefaultHeadroom.Reading(namespace, accelerator, poolsVersion, maxAge, now)
}
