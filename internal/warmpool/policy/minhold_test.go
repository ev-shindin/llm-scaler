package policy

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// MinHold floors a borrow: the bridge stays until the replica that replaced it
// has had time to warm.
//
// Ready is not the same as useful. A replica that has just passed its readiness
// probe has an empty KV cache and an empty prefix cache, and llm-d's shipped
// scheduling profile weights the prefix-cache scorer highest -- so it serves
// slower AND is chosen less. Measured on CoreWeave, bridges went back 7s, 7s and
// 19s after the replica they covered reported Ready, and the one rise where no
// replica ever arrived (so the pool carried the burst) was the best of four by
// 5679ms at p95.

const (
	lender = "variant-lender"
	other  = "variant-other"
)

func podName(n string) types.NamespacedName {
	return types.NamespacedName{Namespace: "ns", Name: n}
}

// held builds the one input shape these tests need: one Pod lent to `lender`,
// borrowed `age` ago, with `lender` no longer short.
func held(age time.Duration, others []VariantDemand) (VariantDemand, []pool.Membership, Input) {
	now := time.Now()
	v := VariantDemand{
		Model:   pool.ModelRef{Variant: lender},
		Desired: 2,
		Ready:   2, // the ordinary replica has arrived: the bridge is excess
	}
	lent := []pool.Membership{{
		Pod:   podName("pool-0"),
		Model: pool.ModelRef{Variant: lender},
	}}
	in := Input{
		Variants:   append([]VariantDemand{v}, others...),
		BorrowedAt: map[Borrow]time.Time{{Pod: podName("pool-0"), Variant: lender}: now.Add(-age)},
		Now:        now,
	}
	return v, lent, in
}

func TestABridgeIsHeldPastReadyForMinHold(t *testing.T) {
	v, lent, in := held(30*time.Second, nil)
	cfg := Config{MaxHold: 5 * time.Minute, MinHold: 90 * time.Second}

	if got := returnsFor(v, lent, in, cfg); len(got) != 0 {
		t.Fatalf("returned a bridge held 30s against a 90s floor: %v.\n"+
			"The replica that replaced it reported Ready seconds ago and is still cold; "+
			"handing the Pod back now swaps a warm engine for an empty one at the crossover.", got)
	}
}

func TestABridgeGoesBackOnceMinHoldHasPassed(t *testing.T) {
	v, lent, in := held(2*time.Minute, nil)
	cfg := Config{MaxHold: 5 * time.Minute, MinHold: 90 * time.Second}

	if got := returnsFor(v, lent, in, cfg); len(got) != 1 {
		t.Fatalf("a bridge past its floor was not returned: %v. The floor delays the "+
			"handover, it does not cancel it -- a Pod never handed back is capacity for "+
			"one model, not insurance for several.", got)
	}
}

// THE PROPERTY THAT KEEPS THE FLOOR HONEST.
//
// With a reserve of one exactly one Pod is lendable, so a bridge lingering for
// model A is a bridge model B cannot borrow -- and in an anti-phase fleet, B
// wanting it is the entire reason the pool is shared. A floor that outranked
// another model's need would invert the pool's purpose while looking like it was
// doing more work.
func TestAnotherVariantBeingShortWaivesTheFloor(t *testing.T) {
	v, lent, in := held(10*time.Second, []VariantDemand{{
		Model:   pool.ModelRef{Variant: other},
		Desired: 2,
		Ready:   1, // short, and the only free Pod is the one being lingered on
	}})
	cfg := Config{MaxHold: 5 * time.Minute, MinHold: 90 * time.Second}

	if got := returnsFor(v, lent, in, cfg); len(got) != 1 {
		t.Fatalf("the floor held a Pod while another variant was short: %v.\n"+
			"With one lendable Pod that delays the other model's lend by the whole floor, "+
			"which is the opposite of what a shared pool is for.", got)
	}
}

func TestAParkedVariantAlsoWaivesTheFloor(t *testing.T) {
	// Parked is the case with no alternative: no replicas at all to fall back on,
	// so its wake is more urgent than another model's warm-up, not less.
	v, lent, in := held(10*time.Second, []VariantDemand{{
		Model:   pool.ModelRef{Variant: other},
		Desired: 0,
		Ready:   0,
		Parked:  true,
	}})
	cfg := Config{MaxHold: 5 * time.Minute, MinHold: 90 * time.Second}

	if got := returnsFor(v, lent, in, cfg); len(got) != 1 {
		t.Fatalf("the floor held a Pod while a parked variant wanted one: %v", got)
	}
}

// The ceiling always wins. Otherwise a MinHold set above MaxHold would pin a Pod
// for ever -- the safety net removed by a knob that never mentions it.
func TestMaxHoldStillWinsOverMinHold(t *testing.T) {
	v, lent, in := held(3*time.Minute, nil)
	cfg := Config{MaxHold: 2 * time.Minute, MinHold: 10 * time.Minute}

	if got := returnsFor(v, lent, in, cfg); len(got) != 1 {
		t.Fatalf("a bridge past MaxHold was kept by MinHold: %v. The floor must never "+
			"remove the ceiling's safety net.", got)
	}
}

// Default off: this changes when accelerators are given back, and a pool that
// holds longer is a pool that costs more. Nobody gets that by accident.
func TestZeroMinHoldIsTheOldBehaviour(t *testing.T) {
	v, lent, in := held(time.Second, nil)
	cfg := Config{MaxHold: 5 * time.Minute}

	if got := returnsFor(v, lent, in, cfg); len(got) != 1 {
		t.Fatalf("with no floor configured a bridge was still held: %v", got)
	}
}

// A bridge still covering a real shortfall is not excess, floor or no floor --
// the floor must not be what keeps it, or turning it off would hand back a Pod
// that is carrying traffic.
func TestAStillNeededBridgeIsNeverReturned(t *testing.T) {
	now := time.Now()
	v := VariantDemand{Model: pool.ModelRef{Variant: lender}, Desired: 3, Ready: 1}
	lent := []pool.Membership{{Pod: podName("pool-0"), Model: pool.ModelRef{Variant: lender}}}
	in := Input{
		Variants:   []VariantDemand{v},
		BorrowedAt: map[Borrow]time.Time{{Pod: podName("pool-0"), Variant: lender}: now.Add(-time.Minute)},
		Now:        now,
	}
	cfg := Config{MaxHold: 5 * time.Minute}

	if got := returnsFor(v, lent, in, cfg); len(got) != 0 {
		t.Fatalf("a bridge covering a live shortfall was returned: %v", got)
	}
}
