package policy

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// A Pod this plan just chose to LEND must not also be evicted by the same plan.
//
// Eviction releases a copy a variant no longer wants, and it reads its
// candidates from the state as it was BEFORE the pass. A Pod picked for a borrow
// moments earlier is still "asleep and not lent" in that view, so it can be
// chosen as the spare -- and because the reconciler actuates borrows before
// evictions, the sequence is: wake the engine, label the Pod into its
// InferencePool, point the proxy at it, then delete the instance underneath all
// three.
//
// What survives is a Pod carrying serving labels whose proxy points at an engine
// that no longer exists: the wake is wasted, the traffic just routed to it has
// nowhere to land, and nothing in the plan says anything went wrong.
func TestAPodBorrowedThisPassIsNotEvictedByIt(t *testing.T) {
	podA := types.NamespacedName{Namespace: "tenant", Name: "pool-a"}
	podB := types.NamespacedName{Namespace: "tenant", Name: "pool-b"}
	model := pool.ModelRef{Namespace: "tenant", Variant: "big"}
	now := time.Now()

	one := 1
	in := Input{
		// Two copies resident and neither lent. The variant wants ONE copy, so
		// eviction has a genuine spare to release -- and a borrow has a genuine
		// Pod to take.
		Memberships: []pool.Membership{
			{Pod: podA, Model: model, State: pool.Asleep, LastUsed: now.Add(-time.Hour)},
			{Pod: podB, Model: model, State: pool.Asleep, LastUsed: now},
		},
		Variants: []VariantDemand{
			{Model: model, Desired: 1, Ready: 0, WarmCopies: &one},
		},
		BorrowedAt: map[Borrow]time.Time{},
		Now:        now,
	}

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour})

	if len(plan.Borrow) == 0 {
		t.Fatal("precondition: the short variant should borrow one of its resident copies")
	}
	borrowed := map[types.NamespacedName]bool{}
	for _, b := range plan.Borrow {
		borrowed[b.Pod] = true
	}
	for _, e := range plan.Evict {
		if borrowed[e.Pod] {
			t.Errorf("Pod %s is borrowed AND evicted by the same plan: the borrow wakes it, "+
				"labels it into the pool and points the proxy at it, and the eviction then "+
				"deletes the instance underneath", e.Pod)
		}
	}
}

// The spare is still released.
//
// Excluding the borrowed Pod must not turn eviction off. Lowering the copy count
// has to free the slot it was asked to free, or warmPoolCopies silently means
// "at least this many" -- the exact failure evictions() exists to prevent.
//
// Three copies, one wanted, one lent. A lent copy has never counted toward the
// wanted number (evictions() has always skipped it), so the arithmetic is over
// the two that remain: one is kept, one is released.
func TestTheSpareIsStillEvictedAroundTheBorrow(t *testing.T) {
	podA := types.NamespacedName{Namespace: "tenant", Name: "pool-a"}
	podB := types.NamespacedName{Namespace: "tenant", Name: "pool-b"}
	podC := types.NamespacedName{Namespace: "tenant", Name: "pool-c"}
	model := pool.ModelRef{Namespace: "tenant", Variant: "big"}
	now := time.Now()

	one := 1
	in := Input{
		Memberships: []pool.Membership{
			{Pod: podA, Model: model, State: pool.Asleep, LastUsed: now.Add(-2 * time.Hour)},
			{Pod: podB, Model: model, State: pool.Asleep, LastUsed: now.Add(-time.Hour)},
			{Pod: podC, Model: model, State: pool.Asleep, LastUsed: now},
		},
		Variants: []VariantDemand{
			{Model: model, Desired: 1, Ready: 0, WarmCopies: &one},
		},
		BorrowedAt: map[Borrow]time.Time{},
		Now:        now,
	}

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour})

	if len(plan.Borrow) != 1 {
		t.Fatalf("borrows = %+v, want one", plan.Borrow)
	}
	if len(plan.Evict) != 1 {
		t.Fatalf("evictions = %+v, want the one spare copy released", plan.Evict)
	}
	if plan.Evict[0].Pod == plan.Borrow[0].Pod {
		t.Errorf("evicted the Pod that was borrowed (%s)", plan.Evict[0].Pod)
	}
}
