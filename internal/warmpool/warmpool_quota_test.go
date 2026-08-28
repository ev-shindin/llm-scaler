package warmpool

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// What a namespace's warm pools cost the quota.
//
// A pool Pod is not a variant, so the saturation engine's population does not
// contain it and the managed figure a quota draws against would not count it.
// It is WVA's consumption all the same: the pool exists because WVA asked for
// it, and WVA publishes the size KEDA scales it to.

// PER POD, once. Memberships are per model per Pod, so a Pod holding three warm
// models appears three times -- and summing memberships would charge its GPUs
// three times over. The pool would then read as larger the more useful it was,
// and the quota would bind because the pool was successfully sharing a GPU.
func TestAPodsGPUsAreChargedOnceHoweverManyModelsItHolds(t *testing.T) {
	pod := types.NamespacedName{Namespace: "ns", Name: "pool-0"}
	cap := pool.PodCapacity{GPUs: 2, Accelerator: "H100"}
	memberships := []pool.Membership{
		{Pod: pod, Capacity: cap, Model: pool.ModelRef{Variant: "a"}},
		{Pod: pod, Capacity: cap, Model: pool.ModelRef{Variant: "b"}},
		{Pod: pod, Capacity: cap, Model: pool.ModelRef{Variant: "c"}},
	}

	if got := warmPoolGPUsByAccelerator(memberships)["H100"]; got != 2 {
		t.Errorf("one Pod holding three models charged %d GPUs, want 2", got)
	}
}

// Two Pods are two charges, so the count still tracks the pool's real size.
func TestEachPodIsChargedSeparately(t *testing.T) {
	cap := pool.PodCapacity{GPUs: 1, Accelerator: "H100"}
	memberships := []pool.Membership{
		{Pod: types.NamespacedName{Namespace: "ns", Name: "pool-0"}, Capacity: cap},
		{Pod: types.NamespacedName{Namespace: "ns", Name: "pool-1"}, Capacity: cap},
	}

	if got := warmPoolGPUsByAccelerator(memberships)["H100"]; got != 2 {
		t.Errorf("two one-GPU Pods charged %d, want 2", got)
	}
}

// A pool whose accelerator cannot be resolved still holds its GPUs. Dropping
// them is how a quota over-grants, so they are charged under a name that says
// what happened.
func TestUnresolvedAcceleratorIsStillCharged(t *testing.T) {
	memberships := []pool.Membership{{
		Pod:      types.NamespacedName{Namespace: "ns", Name: "pool-0"},
		Capacity: pool.PodCapacity{GPUs: 4},
	}}

	got := warmPoolGPUsByAccelerator(memberships)
	if got["unknown"] != 4 {
		t.Errorf("unresolved accelerator charged %v, want 4 under \"unknown\"", got)
	}
}

// An empty pool costs nothing, and says so -- the figure has to be publishable
// so that a pool which shrank stops being charged for what it used to hold.
func TestAnEmptyPoolChargesNothing(t *testing.T) {
	if got := warmPoolGPUsByAccelerator(nil); len(got) != 0 {
		t.Errorf("no memberships charged %v, want nothing", got)
	}
}
