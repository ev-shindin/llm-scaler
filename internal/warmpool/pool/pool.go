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
// variant whose InferencePool the woken copy must join, plus what it takes to
// make and place a copy of it.
type ModelRef struct {
	// Namespace is the workload's namespace, not the pool's.
	Namespace string
	// Variant is the name WVA scales, and the key the warm set is indexed by.
	Variant string
	// EngineOptions is the vLLM command line, which must match the ordinary
	// replicas' -- a different --gpu-memory-utilization is a different
	// torch.compile cache key, and a cache miss costs ~9 s of extra compile.
	EngineOptions string
	// Accelerator is the GPU model the workload requires, from its own pod
	// template, or "" if it requires none in particular. A warm copy is only
	// reusable on the accelerator it was loaded on, so this is what keeps a pool
	// of one GPU type from warming a model pinned to another.
	Accelerator string
	// PoolLabels are the labels that make a Pod a member of this model's
	// InferencePool: the target pool's own `spec.selector.matchLabels`, resolved
	// by the caller from the InferencePool rather than assumed here. Selectors
	// belong to the tenant, and one that requires something other than
	// llm-d.ai/model is not a hypothetical.
	PoolLabels map[string]string
}

// Membership is one model's residency in one Pod.
//
// A Pod holding NOTHING still produces one Membership, with an empty Variant and
// State Absent. Without it an idle Pod would be invisible -- memberships are the
// only view of the pool -- so a fresh pool would report a reserve of zero, and
// could never admit its first model because admission is budgeted against the
// reserve. An empty pool that cannot bootstrap is not a hypothetical: it is the
// shipped manifest, which starts one Pod with nothing resident.
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

	// Pool is the name of the pool this Pod belongs to, from its
	// llm-d.ai/warm-pool label. Empty when the install has one unnamed pool,
	// which is the shape every existing deployment has.
	Pool string

	// Capacity is what the Pod holding this instance actually has, read from
	// its own spec rather than configured.
	//
	// Two numbers used to have to agree by hand: how many GPUs the Deployment
	// asks for and what the controller was told to believe, and likewise the
	// container's memory limit against the admission budget. Nothing checked
	// either pair, and both mismatches are silent in the direction that hurts
	// -- a flag saying four GPUs against a one-GPU Pod admits engines that
	// cannot start, and a budget above the container limit is not a wall at
	// all. Observing the Pod removes the question.
	Capacity PodCapacity
}

// PodCapacity is what a pool Pod has, as its own spec declares it.
type PodCapacity struct {
	// GPUs the Pod requests. Zero when it declares none, which is how an
	// emulated or misconfigured Pod reads.
	GPUs int
	// MemoryLimitBytes is the container's hard limit. Zero when unset, which is
	// a Pod the kubelet may evict at any size and therefore one whose warm set
	// cannot be bounded by observation.
	MemoryLimitBytes int64
	// Accelerator is the GPU model of the node this Pod runs on, or "" when it
	// could not be read -- which is the ordinary case for an install without
	// cluster-scoped node access, not an error.
	Accelerator string
	// PodsPerGroup is how many Pods one warm unit spans: 1 for an ordinary pool
	// Pod, and the LeaderWorkerSet's size for a pool of groups.
	//
	// Needed separately from GPUs because the two do not determine each other. A
	// model wanting sixteen devices across two Pods and one wanting them across
	// four are the same GPU count and different engines, and a group can only
	// host the shape it was created with -- `size` is fixed when the group is.
	PodsPerGroup int
}

// Group reports whether this capacity describes a multi-Pod warm unit.
func (c PodCapacity) Group() bool { return c.PodsPerGroup > 1 }

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

	// Warm makes a model resident and asleep in a given Pod -- admission. It
	// costs a full model load (~33-37 s measured) and transient GPU memory of
	// roughly the weights, so that Pod is out of the reserve until it finishes.
	//
	// The Pod is the CALLER's choice. Placement is policy -- spreading copies so
	// that expected concurrent wakes per Pod stay within what a Pod can serve --
	// and policy does not belong behind this boundary.
	Warm(ctx context.Context, pod types.NamespacedName, model ModelRef, tier Tier) error

	// Activate wakes a resident model in a given Pod and puts it into service:
	// label, wake, confirm the engine answers, point the Pod's serving port at
	// it. A model may be resident in several Pods, so which one serves is the
	// caller's decision.
	//
	// The label goes first deliberately. Readiness is what admits traffic, so
	// labelling early costs nothing and takes the EPP's ~460 ms admit latency
	// off the critical path.
	Activate(ctx context.Context, pod types.NamespacedName, model ModelRef) (Endpoint, error)

	// Deactivate returns a serving model to the reserve: clear the serving
	// port, drain, unlabel, sleep.
	//
	// The proxy is cleared FIRST because it is the gate. Sleeping while traffic
	// can still arrive is the Ready-but-asleep window, which is a 503.
	Deactivate(ctx context.Context, pod types.NamespacedName, model ModelRef) error

	// Evict removes a model from a Pod entirely, freeing its host memory.
	Evict(ctx context.Context, pod types.NamespacedName, model ModelRef) error
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

// Resident counts the models actually held in a Pod, ignoring the placeholder
// that marks an empty one.
func Resident(inPod []Membership) int {
	n := 0
	for _, m := range inPod {
		if m.Model.Variant != "" {
			n++
		}
	}
	return n
}
