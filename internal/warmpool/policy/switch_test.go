package policy

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// SWITCHING one model for another in a Pod that holds both.
//
// This is what a retained pool does for a living: several big models resident on
// one set of GPUs, one awake, and the awake one changes as demand moves. For a
// model that takes 300+ seconds to load, switching is the only way the second
// model is available at all -- a cold start is past any request timeout.
//
// The behaviour is EMERGENT rather than written: freePods counts a Pod that is
// being returned this pass as free, holding() then offers it to a variant that
// is short, and the reconciler actuates returns before borrows. Nothing asserted
// it, so nothing would have noticed it breaking -- and it breaks quietly, into
// "the switch takes two passes", which on a busy pool means a stretch with
// neither model serving.
func TestAPodSwitchesModelsInOnePass(t *testing.T) {
	podRef := types.NamespacedName{Namespace: "tenant", Name: "pool-0"}

	// One Pod, two models resident. "busy" is awake and lent; "wanted" is asleep.
	busy := pool.ModelRef{Namespace: "tenant", Variant: "busy"}
	wanted := pool.ModelRef{Namespace: "tenant", Variant: "wanted"}

	in := Input{
		Memberships: []pool.Membership{
			{Pod: podRef, Model: busy, State: pool.Serving},
			{Pod: podRef, Model: wanted, State: pool.Asleep},
		},
		Variants: []VariantDemand{
			// Its ordinary replicas have arrived, so the bridge is no longer
			// needed and the pool should hand the Pod back.
			{Model: busy, Desired: 1, Ready: 1},
			// Short, and its model is already resident in that same Pod.
			{Model: wanted, Desired: 2, Ready: 1},
		},
		BorrowedAt: map[Borrow]time.Time{},
		Now:        time.Now(),
	}

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour})

	if len(plan.Return) != 1 || plan.Return[0].Model.Variant != "busy" {
		t.Fatalf("returns = %+v, want the finished bridge handed back", plan.Return)
	}
	if len(plan.Borrow) != 1 || plan.Borrow[0].Model.Variant != "wanted" {
		t.Fatalf("borrows = %+v, want the short variant taking the freed Pod", plan.Borrow)
	}
	// The SAME Pod: that is what makes it a switch rather than two unrelated
	// moves, and it is only possible because both models are resident there.
	if plan.Borrow[0].Pod != podRef {
		t.Errorf("borrowed %s, want the Pod being returned (%s)", plan.Borrow[0].Pod, podRef)
	}
}

// A model that is still needed is NOT taken away to serve another.
//
// The counterpart, and the line the current design will not cross without a cost
// model: preempting a live bridge means deciding that one variant's traffic
// matters more than another's, and nothing in WVA can currently say that. Until
// it can, a busy model keeps its Pod and the other variant waits or misses.
func TestABusyModelIsNotPreempted(t *testing.T) {
	podRef := types.NamespacedName{Namespace: "tenant", Name: "pool-0"}
	busy := pool.ModelRef{Namespace: "tenant", Variant: "busy"}
	wanted := pool.ModelRef{Namespace: "tenant", Variant: "wanted"}

	in := Input{
		Memberships: []pool.Membership{
			{Pod: podRef, Model: busy, State: pool.Serving},
			{Pod: podRef, Model: wanted, State: pool.Asleep},
		},
		Variants: []VariantDemand{
			// STILL short: the bridge is doing its job.
			{Model: busy, Desired: 2, Ready: 1},
			{Model: wanted, Desired: 2, Ready: 1},
		},
		BorrowedAt: map[Borrow]time.Time{},
		Now:        time.Now(),
	}

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour})

	if len(plan.Return) != 0 {
		t.Errorf("returns = %+v, want none: the model is still short", plan.Return)
	}
	for _, b := range plan.Borrow {
		if b.Pod == podRef {
			t.Errorf("borrowed the Pod serving a still-needed model: %+v", b)
		}
	}
	// And the disappointment is REPORTED rather than silent, so an operator can
	// see that the pool held the model and could not lend it.
	if len(plan.Blocked) == 0 {
		t.Error("the short variant should be reported blocked: its model is resident but every holding Pod is busy")
	}
}
