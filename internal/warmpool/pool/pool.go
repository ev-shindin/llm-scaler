// Package pool is the boundary between what WVA decides about warm capacity and
// how that decision is carried out.
//
// One logical operation is several calls against three different systems in an
// order that must not be got wrong. Activating a model means waking the engine
// (vLLM's own API), confirming it answers, pointing the Pod's serving port at it
// (the warm-pool proxy) and joining the model's InferencePool (Kubernetes). Sleep
// is the reverse with a drain in the middle, and sleeping before the traffic is
// gone is measurably a 503.
//
// Stating those as five intents keeps that ordering in one place, keeps HTTP out
// of the policy that decides which model should be warm, and leaves room for the
// mechanism to change: today a warm copy is a sleeping vLLM process, and a
// CRIU-style restore would implement the same five calls.
package pool

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Tier is what kind of warm copy is wanted, as INTENT rather than mechanism.
//
// The distinction matters because the mechanism is expected to change: `Ram`
// means "fast, and pay host memory for it", not "call /sleep with level=1".
type Tier string

const (
	// Ram keeps the weights in host memory. Measured wake: ~355 ms.
	Ram Tier = "ram"
	// Disk keeps only the allocator's buffers and re-reads the weights on wake.
	// Cheap in memory, bounded by the engine's effective read bandwidth, which
	// on shared storage was measured at about a quarter of the device's
	// sequential rate.
	Disk Tier = "disk"
)

// State is where a model stands in one Pod.
type State string

const (
	// Absent means the Pod has no instance for this model.
	Absent State = "absent"
	// Loading means an instance is being created: a full model load, and the
	// Pod is NOT part of the reserve while it lasts.
	Loading State = "loading"
	// Asleep means resident and wakeable.
	Asleep State = "asleep"
	// Waking means /wake_up has been called and the engine has not yet answered.
	Waking State = "waking"
	// Serving means awake and in its InferencePool.
	Serving State = "serving"
	// Draining means it has left its InferencePool and is finishing in-flight
	// work before sleeping.
	Draining State = "draining"
)

// ModelRef names a model as WVA knows it: the workload's namespace and the
// variant whose InferencePool the woken copy must join, plus the engine
// arguments that make a copy of it.
type ModelRef struct {
	// Namespace is the workload's namespace, not the pool's.
	Namespace string
	// Variant is the name WVA scales, and the key the warm set is indexed by.
	Variant string
	// EngineOptions is the vLLM command line, which must match the ordinary
	// replicas' -- a different --gpu-memory-utilization is a different
	// torch.compile cache key, and a cache miss costs ~9 s of extra compile.
	EngineOptions string
}

// Membership is one model's residency in one Pod.
type Membership struct {
	Model ModelRef
	Pod   types.NamespacedName
	State State
	Tier  Tier
	// Endpoint is where this instance listens inside its Pod. Empty unless the
	// instance exists.
	Endpoint Endpoint
	// LastUsed is when this model last served from this Pod, and is what
	// eviction ranks on.
	LastUsed time.Time
}

// Endpoint is an instance's address inside its Pod.
type Endpoint struct {
	PodIP string
	Port  int
}

// Pool is what WVA's policy talks to. Implementations carry no policy: they are
// told which model to warm, wake, sleep or evict, never which model deserves it.
type Pool interface {
	// ListWarm reports every model resident in every Pod of the pool.
	//
	// This is also how WVA rebuilds its world after a restart: warm state is
	// discovered from the Pods and their supervisors, never remembered, because
	// a controller that trusts its memory about a data plane it does not own
	// will eventually be wrong about it.
	ListWarm(ctx context.Context) ([]Membership, error)

	// Warm makes a model resident and asleep -- admission. It costs a full
	// model load (~33-37 s measured) and transient GPU memory of roughly the
	// weights, so the Pod it lands on is out of the reserve until it finishes.
	Warm(ctx context.Context, model ModelRef, tier Tier) error

	// Activate wakes a resident model and puts it into service: label, wake,
	// confirm the engine answers, point the Pod's serving port at it.
	//
	// The label goes first deliberately. Readiness is what admits traffic, so
	// labelling early costs nothing and takes the EPP's ~460 ms admit latency
	// off the critical path.
	Activate(ctx context.Context, model ModelRef) (Endpoint, error)

	// Deactivate returns a serving model to the reserve: clear the serving
	// port, drain, unlabel, sleep.
	//
	// The proxy is cleared FIRST because it is the gate. Sleeping while traffic
	// can still arrive is the Ready-but-asleep window, which is a 503.
	Deactivate(ctx context.Context, model ModelRef) error

	// Evict removes a model from a Pod entirely, freeing its host memory.
	Evict(ctx context.Context, model ModelRef) error
}

// Free reports whether a Pod can serve a wake: every instance asleep, and none
// loading.
//
// Loading counts against it because a Pod admitting a model cannot wake another,
// and counting such a Pod as free is how a pool comes to report a reserve it
// cannot honour.
func Free(inPod []Membership) bool {
	for _, m := range inPod {
		switch m.State {
		case Loading, Waking, Serving, Draining:
			return false
		case Absent, Asleep:
			// still free
		}
	}
	return true
}

// ByPod groups memberships by the Pod holding them, which is the shape every
// policy decision needs: the reserve is counted in Pods, not in instances.
func ByPod(all []Membership) map[types.NamespacedName][]Membership {
	out := make(map[types.NamespacedName][]Membership, len(all))
	for _, m := range all {
		out[m.Pod] = append(out[m.Pod], m)
	}
	return out
}

// FreePods counts the Pods that can serve a wake -- the reserve that
// sleepMinSize is a floor on.
func FreePods(all []Membership) int {
	free := 0
	for _, inPod := range ByPod(all) {
		if Free(inPod) {
			free++
		}
	}
	return free
}

// PodsHolding returns the Pods where a variant is resident and wakeable, which
// is what decides whether a spike hits or misses.
func PodsHolding(all []Membership, variant string) []types.NamespacedName {
	var out []types.NamespacedName
	for pod, inPod := range ByPod(all) {
		if !Free(inPod) {
			continue
		}
		for _, m := range inPod {
			if m.Model.Variant == variant && m.State == Asleep {
				out = append(out, pod)
				break
			}
		}
	}
	return out
}
