package policy

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// heldFor builds a pool holding one lent Pod that was borrowed `age` ago, for a
// variant that STILL needs it.
func heldFor(age time.Duration) Input {
	podRef := types.NamespacedName{Namespace: "tenant", Name: "pool-0"}
	model := pool.ModelRef{Namespace: "tenant", Variant: "big"}
	now := time.Now()
	return Input{
		Memberships: []pool.Membership{{Pod: podRef, Model: model, State: pool.Serving}},
		// Short, and staying short: a 300-second model has no ordinary replica
		// arriving to relieve the bridge.
		Variants:   []VariantDemand{{Model: model, Desired: 1, Ready: 0}},
		BorrowedAt: map[Borrow]time.Time{{Pod: podRef, Variant: "big"}: now.Add(-age)},
		Now:        now,
	}
}

// A BRIDGE is reclaimed when it has been held too long. That is the safety net:
// the ordinary replicas were supposed to arrive, and one that never did would
// otherwise keep a Pod out of the reserve forever with nothing noticing.
func TestABridgeIsReclaimedOnTheHoldTimeout(t *testing.T) {
	in := heldFor(5 * time.Minute)

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: 2 * time.Minute})

	if len(plan.Return) != 1 {
		t.Fatalf("returns = %+v, want the expired bridge reclaimed", plan.Return)
	}
}

// A RETAINED pool is not, and this is the whole difference the option makes.
//
// The model takes 300+ seconds to start, so nothing is coming to relieve this
// Pod -- the pool IS the capacity. Reclaiming on a timer would hand the Pod back
// every MaxHold and take it again on the next pass, paying a drain, a sleep and
// a wake each time, with a gap in service around each one. At a two-minute hold
// that is a switch every two minutes, forever, for no reason.
func TestARetainedPoolKeepsAPodItIsStillUsing(t *testing.T) {
	in := heldFor(5 * time.Minute)

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: 2 * time.Minute, Retained: true})

	if len(plan.Return) != 0 {
		t.Errorf("returns = %+v, want none: the variant still needs the Pod and no replica is coming",
			plan.Return)
	}
}

// What DOES return a retained Pod: the variant no longer needing it. Retained
// removes the timer, not the accounting -- otherwise a pool could never take a
// Pod back at all, and the second model would never get a turn.
func TestARetainedPoolStillReturnsWhatIsNoLongerNeeded(t *testing.T) {
	in := heldFor(30 * time.Second)
	in.Variants[0].Desired = 0
	in.Variants[0].Ready = 0

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour, Retained: true})

	if len(plan.Return) != 1 {
		t.Errorf("returns = %+v, want the Pod back once the variant stopped needing it", plan.Return)
	}
}
