package decision

import (
	"sync"
	"time"
)

// WarmPoolSupply is how much serving capacity a variant is getting from BRIDGES:
// warm pool Pods lent to it, rather than its own replicas.
//
// Deliberately separate from the supply the optimizer sizes against. A bridge is
// borrowed, not owned: the pool lends it while the variant is short and takes it
// back when the ordinary replicas arrive, so counting it as supply would tell
// the optimizer the fleet is already big enough and suppress the very scale-up
// the bridge exists to cover. The pool would then hold the Pod indefinitely,
// because the replicas that would release it are the ones it just talked the
// optimizer out of creating.
//
// So it is measured and kept to one side. Two things need it:
//
//   - RETAINED POOLS, where there are no ordinary replicas coming and the pool
//     IS the capacity. Deciding which model should hold the GPUs means comparing
//     what each is getting from the pool against what each is asking for, and
//     that comparison needs this number.
//   - Anyone asking why a variant looks short while its latency is fine: the
//     answer is usually that a bridge is carrying the load.
type WarmPoolSupply struct {
	// Replicas is how many bridges are serving this variant.
	Replicas int
	// Capacity is what they are worth, in the same units as the variant's own
	// TotalSupply -- replicas times the measured per-replica capacity.
	Capacity float64
	// At is when it was measured.
	At time.Time
}

// WarmPoolSupplyStore holds the current bridge supply per variant.
type WarmPoolSupplyStore struct {
	mu sync.RWMutex
	// by is namespace → scale target → supply.
	by map[string]map[string]WarmPoolSupply
}

// NewWarmPoolSupplyStore returns an empty store.
func NewWarmPoolSupplyStore() *WarmPoolSupplyStore {
	return &WarmPoolSupplyStore{by: map[string]map[string]WarmPoolSupply{}}
}

// DefaultWarmPoolSupply is the store the analyzer writes.
var DefaultWarmPoolSupply = NewWarmPoolSupplyStore()

// Publish records what one variant is getting from the pool.
//
// Zero is recorded rather than deleted: "this variant has no bridge" is the
// answer a switching decision most needs, and leaving the previous non-zero
// figure standing would say the opposite.
func (s *WarmPoolSupplyStore) Publish(namespace, target string, replicas int, capacity float64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]map[string]WarmPoolSupply{}
	}
	if s.by[namespace] == nil {
		s.by[namespace] = map[string]WarmPoolSupply{}
	}
	s.by[namespace][target] = WarmPoolSupply{Replicas: replicas, Capacity: capacity, At: at}
}

// Get returns a variant's bridge supply and whether there is a fresh reading.
//
// False means "not measured", which is not zero: no reading means the analyzer
// has not run for this variant, where zero means it ran and found no bridge.
func (s *WarmPoolSupplyStore) Get(namespace, target string, maxAge time.Duration, now time.Time) (WarmPoolSupply, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	got, ok := s.by[namespace][target]
	if !ok {
		return WarmPoolSupply{}, false
	}
	if maxAge > 0 && now.Sub(got.At) > maxAge {
		return WarmPoolSupply{}, false
	}
	return got, true
}

// Reset drops every reading. For tests.
func (s *WarmPoolSupplyStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by = map[string]map[string]WarmPoolSupply{}
}

// PublishWarmPoolSupply records a reading in the default store.
func PublishWarmPoolSupply(namespace, target string, replicas int, capacity float64, at time.Time) {
	DefaultWarmPoolSupply.Publish(namespace, target, replicas, capacity, at)
}

// WarmPoolSupplyFor reads a variant's bridge supply from the default store.
func WarmPoolSupplyFor(namespace, target string, maxAge time.Duration, now time.Time) (WarmPoolSupply, bool) {
	return DefaultWarmPoolSupply.Get(namespace, target, maxAge, now)
}
