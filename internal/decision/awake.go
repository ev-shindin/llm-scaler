package decision

import (
	"sync"
	"time"
)

// AwakeIntent is which model a warm pool should have AWAKE.
//
// A retained pool holds several models on one set of GPUs, one awake, and the
// awake one changes as demand moves. Deciding which it should be is not the
// pool's job: the component that already weighs demand against capacity is the
// optimizer, and it is the only one that can compare two variants' claims on the
// same accelerators. So the optimizer publishes an intent here and the pool
// actuates it -- one component deciding, one acting.
//
// Distinct from a borrow, which is demand-led and temporary. A borrow says a
// variant is SHORT and a warm Pod can cover the gap until its ordinary replicas
// arrive. This says which model should hold a retained pool's GPUs, where no
// replicas are coming and the pool is the capacity. A bridge needs no intent and
// ignores it.
//
// Naming both sides is the point. "Wake B" alone would leave the pool to work
// out what to displace, which is the same judgement made twice, in two places,
// from different information -- and the two would disagree the moment demand
// moved between them.
type AwakeIntent struct {
	// Variant is the model that should be awake in this pool. Empty means the
	// optimizer has no opinion, which is not the same as "sleep everything":
	// absence leaves the pool to its ordinary rules.
	Variant string
	// At is when the intent was formed. An intent is re-derived every cycle, so
	// a stale one means the optimizer stopped: acting on it would hold a pool on
	// a judgement about traffic that has since moved.
	At time.Time
}

// AwakeStore holds the current intent per pool.
//
// Keyed by namespace and pool NAME, because a namespace may hold several pools
// and each has its own GPUs and its own resident set. Keying by namespace alone
// would let one pool's intent move another pool's models.
type AwakeStore struct {
	mu sync.RWMutex
	// by is namespace → pool name → intent.
	by map[string]map[string]AwakeIntent
}

// NewAwakeStore returns an empty store.
func NewAwakeStore() *AwakeStore {
	return &AwakeStore{by: map[string]map[string]AwakeIntent{}}
}

// DefaultAwake is the store the optimizer writes and the warm pool reads.
var DefaultAwake = NewAwakeStore()

// Publish records which model should be awake in one pool.
//
// An empty variant CLEARS the intent rather than recording an empty one: "no
// opinion" and "nothing should be awake" are different instructions, and only
// the first is expressible today. A pool with no intent falls back to its
// ordinary rules, which is what every pool did before this existed.
func (s *AwakeStore) Publish(namespace, pool, variant string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]map[string]AwakeIntent{}
	}
	if variant == "" {
		if pools := s.by[namespace]; pools != nil {
			delete(pools, pool)
		}
		return
	}
	if s.by[namespace] == nil {
		s.by[namespace] = map[string]AwakeIntent{}
	}
	s.by[namespace][pool] = AwakeIntent{Variant: variant, At: at}
}

// Get returns the intent for one pool, and whether there is one that is still
// fresh.
//
// maxAge is the caller's, and false is returned for an intent older than it. A
// stale intent is worse than none: it holds a pool's GPUs on a judgement about
// traffic that has since moved, and the optimizer that formed it has evidently
// stopped publishing.
func (s *AwakeStore) Get(namespace, pool string, maxAge time.Duration, now time.Time) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	intent, ok := s.by[namespace][pool]
	if !ok || intent.Variant == "" {
		return "", false
	}
	if maxAge > 0 && now.Sub(intent.At) > maxAge {
		return "", false
	}
	return intent.Variant, true
}

// Reset drops every intent. For tests, and for a controller that has lost its
// leadership and must not act on what it decided while it held it.
func (s *AwakeStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by = map[string]map[string]AwakeIntent{}
}

// PublishAwake records an intent in the default store.
func PublishAwake(namespace, pool, variant string, at time.Time) {
	DefaultAwake.Publish(namespace, pool, variant, at)
}

// Awake reads an intent from the default store.
func Awake(namespace, pool string, maxAge time.Duration, now time.Time) (string, bool) {
	return DefaultAwake.Get(namespace, pool, maxAge, now)
}
