package warmpool

import (
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
)

// elastic is a pool of the given size, allowed to move between 1 and 4.
func elastic(replicas, reserve int) (PoolSpec, SizeBounds) {
	return PoolSpec{
			Name:     "p",
			Replicas: replicas,
			Config:   policy.Config{SleepMinSize: reserve},
		}, SizeBounds{
			Min: 1, Max: 4, Enabled: true,
		}
}

func TestResizingIsOffUnlessBothBoundsAreSet(t *testing.T) {
	// The default, and every install today: a pool holds accelerators
	// continuously, so its size is a spending decision nobody should take on an
	// operator's behalf.
	b, err := BoundsFrom(nil)
	if err != nil || b.Enabled {
		t.Fatalf("no annotations must mean no resizing: %+v %v", b, err)
	}

	// Half a range is a misconfiguration, not a partial opt-in. A max alone
	// could never grow; a min alone could shrink to the floor the first quiet
	// period and never come back.
	for _, half := range []map[string]string{
		{AnnotationMinReplicas: "1"},
		{AnnotationMaxReplicas: "4"},
	} {
		if _, err := BoundsFrom(half); err == nil {
			t.Errorf("half a range must be refused and reported: %v", half)
		}
	}

	if _, err := BoundsFrom(map[string]string{
		AnnotationMinReplicas: "4", AnnotationMaxReplicas: "2",
	}); err == nil {
		t.Error("a min above the max must be refused")
	}

	b, err = BoundsFrom(map[string]string{AnnotationMinReplicas: "1", AnnotationMaxReplicas: "4"})
	if err != nil || !b.Enabled || b.Min != 1 || b.Max != 4 {
		t.Fatalf("a whole range must be taken: %+v %v", b, err)
	}
}

func TestGrowthNeedsASustainedShortfall(t *testing.T) {
	// A single short pass is a borrow doing its job. Growing on it buys a Pod
	// for every spike, which is the opposite of what a bridge is for.
	spec, bounds := elastic(2, 1)

	if _, want := SizeFor(spec, bounds, 0, 1, 1, 0, 3, 60); want {
		t.Error("one short pass is not a shortfall")
	}
	got, want := SizeFor(spec, bounds, 0, 1, 3, 0, 3, 60)
	if !want {
		t.Fatal("a sustained shortfall must grow the pool")
	}
	if got.To != 3 {
		t.Errorf("growth is one Pod at a time, got %d", got.To)
	}
}

func TestGrowthStopsAtTheCeiling(t *testing.T) {
	spec, bounds := elastic(4, 1)
	if _, want := SizeFor(spec, bounds, 0, 1, 99, 0, 3, 60); want {
		t.Error("the max is a ceiling, not a suggestion")
	}
}

func TestShrinkingIsHarderThanGrowing(t *testing.T) {
	// Asymmetric on purpose: too small costs latency on every spike, too large
	// costs money, and paying a model load to grow back is the worst of both.
	spec, bounds := elastic(3, 1)

	// Idle, but not for long enough.
	if _, want := SizeFor(spec, bounds, 3, 0, 0, 10, 3, 60); want {
		t.Error("a brief quiet period is not grounds to shrink")
	}
	got, want := SizeFor(spec, bounds, 3, 0, 0, 60, 3, 60)
	if !want || got.To != 2 {
		t.Fatalf("a long quiet period shrinks by one: %+v %v", got, want)
	}
}

func TestAPoolLendingIsNeverShrunk(t *testing.T) {
	// Scale-down cannot choose its victim with certainty, so shrinking while a
	// bridge is open risks deleting the Pod serving live traffic at the one
	// moment the pool exists for.
	spec, bounds := elastic(3, 1)
	if _, want := SizeFor(spec, bounds, 2, 1, 0, 999, 3, 60); want {
		t.Fatal("a pool with a bridge open must not shrink")
	}
}

func TestShrinkingStopsAtTheFloorAndAtTheReserve(t *testing.T) {
	atFloor, bounds := elastic(1, 1)
	if _, want := SizeFor(atFloor, bounds, 1, 0, 0, 999, 3, 60); want {
		t.Error("the min is a floor, not a suggestion")
	}

	// Free equals the reserve: shrinking would take the pool below the floor it
	// was told to keep, which is the inert state by another route.
	atReserve, bounds2 := elastic(3, 3)
	if _, want := SizeFor(atReserve, bounds2, 3, 0, 0, 999, 3, 60); want {
		t.Error("shrinking must not eat the reserve")
	}
}

func TestAPoolThatWasNotDiscoveredFromADeploymentCannotResize(t *testing.T) {
	// Replicas is unknown, so there is no number to move.
	spec := PoolSpec{Name: "", Config: policy.Config{SleepMinSize: 1}}
	if _, want := SizeFor(spec, SizeBounds{Min: 1, Max: 4, Enabled: true}, 0, 0, 99, 0, 3, 60); want {
		t.Fatal("a pool with no Deployment has no replica count to change")
	}
}
