package pool

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func pod(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "pool-ns", Name: name}
}

func member(p, variant string, state State) Membership {
	return Membership{
		Model: ModelRef{Namespace: "workload-ns", Variant: variant},
		Pod:   pod(p),
		State: state,
		Tier:  Ram,
	}
}

func TestFreeMeansEveryInstanceAsleep(t *testing.T) {
	if !Free([]Membership{member("a", "x", Asleep), member("a", "y", Asleep)}) {
		t.Error("a Pod with two sleepers can serve a wake")
	}
	if Free([]Membership{member("a", "x", Serving), member("a", "y", Asleep)}) {
		t.Error("a Pod already serving cannot serve another wake")
	}
	if !Free(nil) {
		t.Error("a Pod holding nothing is free, if useless")
	}
}

func TestLoadingPodIsNotPartOfTheReserve(t *testing.T) {
	// The distinction the reserve arithmetic depends on: a Pod admitting a
	// model cannot wake another for ~33-37s, so counting it as free reports a
	// reserve the pool cannot honour.
	if Free([]Membership{member("a", "x", Asleep), member("a", "y", Loading)}) {
		t.Fatal("a Pod loading a model must not count as free")
	}
	for _, busy := range []State{Loading, Waking, Serving, Draining} {
		if Free([]Membership{member("a", "x", busy)}) {
			t.Errorf("state %q must not count as free", busy)
		}
	}
}

func TestFreePodsCountsPodsNotInstances(t *testing.T) {
	// sleepMinSize is a floor on PODS. A Pod with eight sleepers is one unit of
	// reserve, not eight.
	all := []Membership{
		member("a", "x", Asleep), member("a", "y", Asleep), member("a", "z", Asleep),
		member("b", "x", Serving), member("b", "y", Asleep),
		member("c", "x", Asleep),
	}
	if got := FreePods(all); got != 2 {
		t.Fatalf("FreePods = %d, want 2 (a and c; b is lent)", got)
	}
}

func TestByPodGroupsWhatPolicyNeeds(t *testing.T) {
	all := []Membership{
		member("a", "x", Asleep), member("a", "y", Asleep),
		member("b", "x", Asleep),
	}
	grouped := ByPod(all)
	if len(grouped) != 2 {
		t.Fatalf("want 2 Pods, got %d", len(grouped))
	}
	if len(grouped[pod("a")]) != 2 {
		t.Errorf("Pod a holds two models, got %d", len(grouped[pod("a")]))
	}
}
