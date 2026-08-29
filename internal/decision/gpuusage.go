package decision

import (
	"maps"
	"sync"
	"time"
)

// GPUUsage is a snapshot of how many GPUs are currently in use.
//
// It exists so the scale-from-zero engine can ask whether a variant it is about
// to wake would actually get GPUs, without standing up a second accounting path
// of its own: ConstraintProvider.ComputeConstraints takes usage as an input, and
// a second implementation summing it independently would be free to drift from
// the one the optimizer honours, leaving the two engines disagreeing about what
// is free.
//
// The same type carries either MEASURE of usage — physical or WVA-managed — one
// per store; see DefaultGPUUsage and DefaultManagedGPUUsage for which is which
// and why both exist.
type GPUUsage struct {
	// ByType maps accelerator type to GPUs in use cluster-wide.
	ByType map[string]int
	// ByNamespace maps namespace to accelerator type to GPUs in use. Its key set
	// also defines which namespaces get namespace-scoped caps materialised.
	ByNamespace map[string]map[string]int
	// TakenAt is when the snapshot was published.
	TakenAt time.Time
}

// GPUUsageStore holds the latest GPUUsage snapshot.
type GPUUsageStore struct {
	mu   sync.RWMutex
	snap *GPUUsage
	now  func() time.Time
}

// NewGPUUsageStore returns an empty store.
func NewGPUUsageStore() *GPUUsageStore {
	return &GPUUsageStore{now: time.Now}
}

// Publish records a new snapshot. The maps are deep-copied, so the caller may
// keep mutating its own.
func (s *GPUUsageStore) Publish(byType map[string]int, byNamespace map[string]map[string]int) {
	snap := &GPUUsage{
		ByType:      make(map[string]int, len(byType)),
		ByNamespace: make(map[string]map[string]int, len(byNamespace)),
	}
	maps.Copy(snap.ByType, byType)
	for ns, perType := range byNamespace {
		snap.ByNamespace[ns] = maps.Clone(perType)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	snap.TakenAt = s.now()
	s.snap = snap
}

// Get returns the latest snapshot and whether one has been published. The
// returned value is the stored copy and must not be mutated.
//
// A false second return means this store's producer has not completed a pass
// yet. Callers must treat that as "unknown", not "nothing is in use": denying a
// wake because usage is unknown would keep a model down for the first cycle
// after every restart, which is exactly when a queued request is waiting.
func (s *GPUUsageStore) Get() (*GPUUsage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snap == nil {
		return nil, false
	}
	return s.snap, true
}

// Reset drops the stored snapshot, returning the store to its "nothing published
// yet" state.
//
// Provided so tests can isolate themselves WITHOUT reassigning DefaultGPUUsage:
// that variable is read unsynchronized by PublishGPUUsage and LatestGPUUsage, so
// swapping the pointer is a data race against any concurrent user of the store,
// which the store's own mutex cannot protect against.
func (s *GPUUsageStore) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = nil
}

// DefaultGPUUsage is the process-wide PHYSICAL usage snapshot: every GPU held on
// the cluster's GPU nodes, whoever holds it. Written by internal/gpuusage, which
// observes the cluster on its own interval and is its sole producer; read by the
// saturation optimizer and the scale-from-zero placement check.
//
// This is the figure that answers "will the scheduler find a free device?", so
// it must count consumers WVA does not manage. It is NOT the figure a quota
// draws against — see DefaultManagedGPUUsage.
//
// Treat this as immutable after init: use Reset to clear it rather than
// assigning a new store, for the reason given there.
var DefaultGPUUsage = NewGPUUsageStore()

// DefaultManagedGPUUsage is the process-wide MANAGED usage snapshot: only what
// WVA's own variants hold, summed from the population the saturation engine is
// optimizing, which is its sole producer.
//
// It exists because an operator-declared quota is an allowance granted to WVA and
// may only be spent by WVA. Charged the physical figure it binds on consumption
// it does not govern — a namespace with a 4-GPU WVA quota and an unrelated 4-GPU
// job reads as fully spent while WVA has placed nothing — so the two measures
// cannot share a store, and a provider is fed whichever one it declares (see
// pipeline.UsageBasis).
//
// Published on every completed saturation cycle INCLUDING one that finds nothing
// active, because "WVA holds no GPUs" is a true and useful statement there: it is
// the state scale-from-zero exists to serve, and a wake refused for want of a
// figure that only a running fleet could produce would never be able to start
// one. It is deliberately not published when collection FAILED, where an empty
// population means "we could not see", not "nothing is running".
var DefaultManagedGPUUsage = NewGPUUsageStore()

// warmPoolGPUs is what WVA's own warm pools hold, per namespace and accelerator.
//
// A pool Pod is not a variant, so the saturation engine's population does not
// contain it and DefaultManagedGPUUsage would not count it. But a quota is an
// allowance granted to WVA, and a pool is WVA's: it exists because WVA asked for
// it, and WVA publishes the size KEDA scales it to. Leaving it out let a
// namespace with a 4-GPU quota and a 3-GPU pool place four more replicas and
// consume seven.
//
// This is an INPUT to the managed figure, not a second publisher of it. The
// saturation engine remains the only thing that writes DefaultManagedGPUUsage --
// two producers summing "what WVA holds" differently is exactly what that store's
// comment warns against -- and it folds this in on each cycle.
var (
	warmPoolGPUsMu sync.RWMutex
	warmPoolGPUs   = map[string]map[string]int{}
)

// PublishWarmPoolGPUs records what the warm pools in one namespace hold, keyed by
// accelerator. Replaces that namespace's previous figure wholesale, so a pool
// that shrank or went away stops being charged for.
func PublishWarmPoolGPUs(namespace string, byType map[string]int) {
	warmPoolGPUsMu.Lock()
	defer warmPoolGPUsMu.Unlock()
	if len(byType) == 0 {
		delete(warmPoolGPUs, namespace)
		return
	}
	warmPoolGPUs[namespace] = maps.Clone(byType)
}

// WarmPoolGPUs returns a copy of what every namespace's pools hold.
func WarmPoolGPUs() map[string]map[string]int {
	warmPoolGPUsMu.RLock()
	defer warmPoolGPUsMu.RUnlock()
	out := make(map[string]map[string]int, len(warmPoolGPUs))
	for ns, byType := range warmPoolGPUs {
		out[ns] = maps.Clone(byType)
	}
	return out
}

// ResetWarmPoolGPUs clears the record. For tests.
func ResetWarmPoolGPUs() {
	warmPoolGPUsMu.Lock()
	defer warmPoolGPUsMu.Unlock()
	warmPoolGPUs = map[string]map[string]int{}
}

// PublishGPUUsage records a physical-usage snapshot in the default store.
func PublishGPUUsage(byType map[string]int, byNamespace map[string]map[string]int) {
	DefaultGPUUsage.Publish(byType, byNamespace)
}

// LatestGPUUsage reads the default physical-usage snapshot.
func LatestGPUUsage() (*GPUUsage, bool) {
	return DefaultGPUUsage.Get()
}

// PublishManagedGPUUsage records a WVA-managed usage snapshot in the default
// managed store.
func PublishManagedGPUUsage(byType map[string]int, byNamespace map[string]map[string]int) {
	DefaultManagedGPUUsage.Publish(byType, byNamespace)
}

// LatestManagedGPUUsage reads the default WVA-managed usage snapshot.
func LatestManagedGPUUsage() (*GPUUsage, bool) {
	return DefaultManagedGPUUsage.Get()
}
