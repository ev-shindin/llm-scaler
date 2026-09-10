package decision

import (
	"sync"
	"time"
)

// TrustVerdict records whether WVA had a usable view of one scale target on its
// last collection pass.
//
// It exists so WVA can decline to answer rather than answer from nothing. Every
// guard in the collector and the analyzers rejects an input it cannot trust --
// a not-Ready pod's timing, a service time that contradicts the fleet's own
// decode arithmetic, a replica whose scrape is behind. Rejecting is right, but
// rejecting ENOUGH of a variant's inputs quietly converts "the metrics pipeline
// is broken" into "this workload looks idle", and an idle-looking workload is
// scaled down. The guards make the recommendation safe from one bad replica;
// this makes the ABSENCE of a recommendation visible instead of being rounded
// off to a low one.
//
// The consumer is the KEDA external scaler, which turns an untrusted verdict
// into a gRPC error. That is deliberate reuse rather than new machinery: KEDA
// already defines what happens when a scaler cannot answer -- no metric reaches
// the HPA, so the replica count holds, and after spec.fallback.failureThreshold
// consecutive failures KEDA applies spec.fallback. WVA does not need its own
// freeze, hold-last-value or floor; it needs a way to say "no answer", which is
// the one thing the gRPC contract already has and WVA was not using.
//
// Not published as a metric label or a status: this is one component telling
// another what it just observed, which is what this package is for.
type TrustVerdict struct {
	// Trusted is false when the last pass produced rows for this target but
	// none of them were usable.
	Trusted bool
	// Reason names what was wrong, for the scaler's error message and log line.
	// Empty when Trusted.
	Reason string
	// UpdatedAt dates the verdict. A verdict no longer being refreshed must not
	// hold a workload frozen forever -- see Trust.
	UpdatedAt time.Time
}

// TrustStore holds the latest verdict per scale target, keyed the same way as
// the decision store: namespace/name of the Deployment or LWS. Safe for
// concurrent use.
type TrustStore struct {
	mu sync.RWMutex
	m  map[string]TrustVerdict
}

// NewTrustStore returns an empty store.
func NewTrustStore() *TrustStore {
	return &TrustStore{m: make(map[string]TrustVerdict)}
}

// DefaultTrust is the process-wide store.
var DefaultTrust = NewTrustStore()

// Publish records a verdict for one scale target.
//
// Callers publish on every pass that produced rows for the target, trusted or
// not, so a recovered workload clears its own verdict without anything having to
// remember to delete it.
func (s *TrustStore) Publish(namespace, name string, trusted bool, reason string, now time.Time) {
	if trusted {
		reason = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[storeKey(namespace, name)] = TrustVerdict{
		Trusted:   trusted,
		Reason:    reason,
		UpdatedAt: now,
	}
}

// There is deliberately no Forget. A target that stops producing rows -- parked
// at zero, deleted -- simply stops being republished, and its verdict expires
// through the maxAge below. A delete path would have to be called from wherever
// a workload disappears, which is several places, and forgetting to call it
// would leave a workload frozen; letting the verdict go stale fails the other
// way on its own.

// Trust reports whether the target's last verdict says its inputs were usable,
// and why not when they were not.
//
// Answers TRUSTED for a target with no verdict at all, and for a verdict older
// than maxAge. Both are the same judgement: this store exists to report a
// specific observed failure, and its own silence is not evidence of one. A
// workload nothing has collected for -- because it is at zero replicas, because
// the collector is only just starting, because whatever publishes verdicts has
// itself stopped -- must not be frozen by a store that has nothing to say about
// it. Erring the other way would let one wedged component hold an entire fleet.
//
// maxAge <= 0 disables expiry.
func (s *TrustStore) Trust(namespace, name string, now time.Time, maxAge time.Duration) (bool, string) {
	s.mu.RLock()
	v, ok := s.m[storeKey(namespace, name)]
	s.mu.RUnlock()

	if !ok || v.Trusted {
		return true, ""
	}
	if maxAge > 0 && now.Sub(v.UpdatedAt) > maxAge {
		return true, ""
	}
	return false, v.Reason
}
