package decision

import (
	"maps"
	"sync"
	"time"
)

// BridgeStore records which warm pool Pods are currently LENT, and to which
// variant.
//
// It exists so the collector can attribute a bridge's engine metrics. A pool Pod
// is owned by the POOL's workload, so the ownerReference walk that attributes
// every other Pod reaches the pool's scale target and not the model's -- the Pod
// is serving a variant's traffic while belonging, structurally, to something
// else entirely. Without this the metrics are dropped as unattributed, and the
// load the bridge is carrying is invisible to the analyzer that decides how much
// capacity the model needs.
//
// Which is the wrong way round. A bridge exists precisely because a variant is
// short, so its traffic is the traffic that most needs counting; dropping it
// makes demand look lowest exactly when it is highest, and makes the demand
// appear from nowhere the moment the bridge is handed back.
//
// The pool publishes this because the pool is the only component that knows it.
// Lending is a decision it makes and records in Pod labels, and reading those
// labels back out would mean the collector re-deriving a fact the controller
// already has -- against an InferencePool selector, which is a join the
// collector has no reason to carry.
type BridgeStore struct {
	mu sync.RWMutex
	// by is namespace → the whole lending map for that namespace.
	by map[string]bridgeSnapshot
}

// bridgeSnapshot is one namespace's lending, as of one pass.
type bridgeSnapshot struct {
	// lentTo is pod name → the SCALE TARGET the Pod is lent to, which is the
	// name the analyzer and the collector both key a variant by.
	lentTo map[string]string
	at     time.Time
}

// NewBridgeStore returns an empty store.
func NewBridgeStore() *BridgeStore {
	return &BridgeStore{by: map[string]bridgeSnapshot{}}
}

// DefaultBridges is the store the warm pool writes and the collector reads.
var DefaultBridges = NewBridgeStore()

// Publish replaces a namespace's lending wholesale.
//
// Wholesale, and an EMPTY map is a real answer: a Pod handed back has stopped
// being a bridge, and a store that only ever learned of new lendings would go on
// attributing a returned Pod's metrics to a variant it no longer serves. The
// pool publishes on every pass in which it could observe itself, and publishes
// nothing on a pass where it could not -- "could not see" must not read as
// "nothing is lent".
func (s *BridgeStore) Publish(namespace string, lentTo map[string]string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.by == nil {
		s.by = map[string]bridgeSnapshot{}
	}
	s.by[namespace] = bridgeSnapshot{lentTo: maps.Clone(lentTo), at: at}
}

// VariantFor returns the scale target a Pod is lent to, and whether it is a
// bridge at all.
//
// A reading older than maxAge answers false. The pool republishes every pass, so
// a stale map means the pool's reconciler has stopped -- and attributing a Pod
// to a variant on the strength of a lending that may since have ended would add
// demand for load nobody is carrying.
func (s *BridgeStore) VariantFor(namespace, pod string, maxAge time.Duration, now time.Time) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.by[namespace]
	if !ok {
		return "", false
	}
	if maxAge > 0 && now.Sub(snapshot.at) > maxAge {
		return "", false
	}
	variant, lent := snapshot.lentTo[pod]
	if !lent || variant == "" {
		return "", false
	}
	return variant, true
}

// Reset drops every lending. For tests, and for a controller that has lost its
// leadership.
func (s *BridgeStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.by = map[string]bridgeSnapshot{}
}

// PublishBridges records a namespace's lending in the default store.
func PublishBridges(namespace string, lentTo map[string]string, at time.Time) {
	DefaultBridges.Publish(namespace, lentTo, at)
}

// BridgeVariant reads a lending from the default store.
func BridgeVariant(namespace, pod string, maxAge time.Duration, now time.Time) (string, bool) {
	return DefaultBridges.VariantFor(namespace, pod, maxAge, now)
}
