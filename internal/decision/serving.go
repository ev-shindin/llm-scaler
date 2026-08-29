package decision

import (
	"sync"
	"time"
)

// ServingStore holds how many replicas of a variant are actually SERVING:
// reporting engine metrics, not merely passing a readiness probe.
//
// It exists for the warm pool's return rule. A bridge is handed back when the
// ordinary replicas have arrived, and "arrived" was read from
// Status.ReadyReplicas -- the kubelet's probe. A replica can pass that probe and
// not yet be doing the work: the engine answers /health once its server is up,
// while the thing that matters to the traffic being handed back is whether it is
// taking requests and reporting on them.
//
// Getting this wrong is quiet. The bridge goes back, the Pod sleeps, and the
// requests that would have gone to it arrive at a replica that is not ready to
// serve them -- which looks like a slow model rather than a pool that let go too
// early.
//
// Reporting metrics is not a perfect proxy for serving, and it is a much better
// one than a probe: a replica the collector can see is one the engine has
// registered, is scraping, and can attribute traffic to.
type ServingStore struct {
	mu sync.RWMutex
	// by is namespace → scale target → count, keyed by SCALE TARGET because that
	// is what the warm pool's demand is keyed by and what a decision names.
	by map[string]map[string]servingCount
}

type servingCount struct {
	replicas int
	at       time.Time
}

// NewServingStore returns an empty store.
func NewServingStore() *ServingStore {
	return &ServingStore{by: map[string]map[string]servingCount{}}
}

// DefaultServing is the store the engine writes and the warm pool reads.
var DefaultServing = NewServingStore()

// Publish records how many replicas of one scale target are reporting.
//
// Zero is a real answer and is recorded: a variant whose replicas have all gone
// away is exactly the case a bridge must NOT be returned for.
func (s *ServingStore) Publish(namespace, target string, replicas int, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]map[string]servingCount{}
	}
	if s.by[namespace] == nil {
		s.by[namespace] = map[string]servingCount{}
	}
	s.by[namespace][target] = servingCount{replicas: replicas, at: at}
}

// Get returns the serving count and whether there is a fresh one.
//
// False means "no answer", which callers must not read as zero. The two need
// opposite treatment: no answer is a collector that has not run for this variant
// yet, where falling back to the Ready count is right; zero is a variant whose
// replicas are demonstrably not serving, where returning its bridge would strand
// the traffic.
func (s *ServingStore) Get(namespace, target string, maxAge time.Duration, now time.Time) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.by[namespace][target]
	if !ok {
		return 0, false
	}
	if maxAge > 0 && now.Sub(c.at) > maxAge {
		return 0, false
	}
	return c.replicas, true
}

// Reset drops every count. For tests.
func (s *ServingStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by = map[string]map[string]servingCount{}
}

// PublishServing records a count in the default store.
func PublishServing(namespace, target string, replicas int, at time.Time) {
	DefaultServing.Publish(namespace, target, replicas, at)
}

// Serving reads a count from the default store.
func Serving(namespace, target string, maxAge time.Duration, now time.Time) (int, bool) {
	return DefaultServing.Get(namespace, target, maxAge, now)
}
