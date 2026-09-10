package decision

import (
	"sync"
	"time"
)

// TrustRecord is what the collector last saw of one workload.
//
// Keyed by the SCALEDOBJECT's namespace and name, not by the scale target's.
// That is the opposite of the decision Store beside it, and both are right: the
// actuator writes decisions from a VariantAutoscaling, whose name is the
// Deployment's, while this is written from collected rows whose VariantName is
// the ScaledObject's and read by the external scaler, whose ScaledObjectRef.Name
// is also the ScaledObject's. Keying it like the decision store instead is not
// hypothetical -- it was done, and the writer and reader then never met: the
// generator names ScaledObjects "<target>-wva", so every lookup missed and the
// abstain could not fire at all.
type TrustRecord struct {
	// LastObserved is the last collection pass that produced ANY row for this
	// workload. It is WVA's own clock, deliberately: it is the one staleness
	// signal that does not depend on a timestamp Prometheus supplies.
	LastObserved time.Time
	// Untrusted marks a pass that produced rows and found every one of them
	// past the unavailable threshold.
	Untrusted bool
	// Reason names what was wrong, for the scaler's error and log line.
	Reason string
	// ModelID is the model the workload serves, so the collector can report the
	// model-level consequence without a second index.
	ModelID string
	// Namespace and Name are kept alongside the map key rather than parsed back
	// out of it. A "/"-joined key cannot be split unambiguously -- a Kubernetes
	// name cannot contain "/", but nothing in the type system says so, and a
	// splitter would be one rename away from silently mis-attributing records.
	Namespace string
	Name      string
}

// TrustStore holds the latest record per workload. Safe for concurrent use: the
// collector writes it from the optimize loop, the external scaler reads it from
// a gRPC handler.
//
// It exists so WVA can decline to answer rather than answer from nothing. Every
// guard in the collector and the analyzers rejects an input it cannot trust, and
// rejecting ENOUGH of a workload's inputs quietly converts "the metrics pipeline
// is broken" into "this workload looks idle" -- which is scaled down. The guards
// make a recommendation safe from one bad replica; this makes the ABSENCE of a
// recommendation visible instead of being rounded off to a low one.
//
// The consumer turns an untrusted record into a gRPC error, which is deliberate
// reuse rather than new machinery: KEDA already defines what happens when a
// scaler cannot answer.
type TrustStore struct {
	mu sync.RWMutex
	m  map[string]TrustRecord
}

// NewTrustStore returns an empty store.
func NewTrustStore() *TrustStore {
	return &TrustStore{m: make(map[string]TrustRecord)}
}

// DefaultTrust is the process-wide store.
var DefaultTrust = NewTrustStore()

// Observe records a collection pass that produced rows for one workload.
//
// Called for every workload a pass saw, trusted or not, so a recovered workload
// clears its own verdict and refreshes its own clock without anything having to
// remember to delete it.
func (s *TrustStore) Observe(namespace, name, modelID string, untrusted bool, reason string, at time.Time) {
	if !untrusted {
		reason = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[storeKey(namespace, name)] = TrustRecord{
		LastObserved: at,
		Untrusted:    untrusted,
		Reason:       reason,
		ModelID:      modelID,
		Namespace:    namespace,
		Name:         name,
	}
}

// Trust reports whether WVA currently has a usable view of the workload, and why
// not when it does not.
//
// Two ways to fail, and they are the two shapes a broken metrics pipeline
// actually takes:
//
//  1. The last pass saw the workload and every replica's data was past the
//     unavailable threshold. Prometheus still holds the series, so rows arrive;
//     they are just old.
//  2. No pass has seen the workload for longer than staleLimit. This is what
//     happens AFTER case 1: Prometheus drops a series once nothing has fed it,
//     the rows stop arriving entirely, and a store that only knew case 1 would
//     go quiet exactly when the problem got worse.
//
// A workload with NO record is trusted. That is a workload nothing has collected
// yet -- new, or parked at zero -- and this store's silence about it is not
// evidence against it.
//
// Note this does NOT expire an untrusted record into a trusted one. An earlier
// version did, to stop a wedged collector holding a fleet forever; that reasoning
// is inverted here on purpose, because "I have not seen this workload in five
// minutes" IS the abstain condition rather than something to time out of. A
// wedged collector holds every workload it had seen, which is the honest
// outcome: with spec.fallback configured that is a hold or a raise, never a
// drop, and the blocked-reason metric says so out loud.
func (s *TrustStore) Trust(namespace, name string, now time.Time, staleLimit time.Duration) (bool, string) {
	s.mu.RLock()
	r, ok := s.m[storeKey(namespace, name)]
	s.mu.RUnlock()

	if !ok {
		return true, ""
	}
	if r.Untrusted {
		return false, r.Reason
	}
	if staleLimit > 0 && now.Sub(r.LastObserved) > staleLimit {
		return false, "no metrics collected for this workload since " + r.LastObserved.UTC().Format(time.RFC3339)
	}
	return true, ""
}

// UnanswerableFor lists the workloads of one model that Trust would currently
// refuse to answer for, so the collector can report the model-level consequence
// on a pass that may not have produced rows for them at all.
//
// It is the same predicate as Trust, applied over the records this store holds
// for the model rather than to a single name the caller already has. Without it
// the metric could only ever report case 1 above -- the collector cannot notice
// a workload it did not see this pass by looking at the rows it did see.
func (s *TrustStore) UnanswerableFor(namespace, modelID string, now time.Time, staleLimit time.Duration) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var names []string
	for _, r := range s.m {
		if r.Namespace != namespace || r.ModelID != modelID {
			continue
		}
		if r.Untrusted || (staleLimit > 0 && now.Sub(r.LastObserved) > staleLimit) {
			names = append(names, r.Name)
		}
	}
	return names
}
