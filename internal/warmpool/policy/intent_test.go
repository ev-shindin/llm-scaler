package policy

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// The two models these tests move between: one serving, one resident and
// asleep. Named for their state at the start, which is what each assertion is
// about.
const (
	awakeVariant  = "awake"
	asleepVariant = "asleep"
)

// twoModelPod is one Pod holding two models, one awake and lent.
func twoModelPod() (Input, types.NamespacedName) {
	podRef := types.NamespacedName{Namespace: "tenant", Name: "pool-0"}
	awake := pool.ModelRef{Namespace: "tenant", Variant: awakeVariant}
	asleep := pool.ModelRef{Namespace: "tenant", Variant: asleepVariant}
	now := time.Now()
	in := Input{
		Memberships: []pool.Membership{
			{Pod: podRef, Model: awake, State: pool.Serving},
			{Pod: podRef, Model: asleep, State: pool.Asleep},
		},
		// BOTH still wanted. Without an intent nothing moves -- that is the
		// no-preemption rule, and it is what makes this signal necessary.
		Variants: []VariantDemand{
			{Model: awake, Desired: 2, Ready: 1},
			{Model: asleep, Desired: 2, Ready: 1},
		},
		BorrowedAt: map[Borrow]time.Time{{Pod: podRef, Variant: awakeVariant}: now},
		Now:        now,
	}
	return in, podRef
}

// The optimizer says which model should hold the pool's GPUs, and the pool moves
// them -- in ONE pass, so there is no stretch with neither model serving.
func TestAnIntentSwitchesTheAwakeModel(t *testing.T) {
	in, podRef := twoModelPod()
	in.WantAwake = asleepVariant

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour, Retained: true})

	if len(plan.Return) != 1 || plan.Return[0].Model.Variant != awakeVariant {
		t.Fatalf("returns = %+v, want the model in the way handed back", plan.Return)
	}
	if len(plan.Borrow) != 1 || plan.Borrow[0].Model.Variant != asleepVariant {
		t.Fatalf("borrows = %+v, want the chosen model taking the Pod", plan.Borrow)
	}
	if plan.Borrow[0].Pod != podRef {
		t.Errorf("borrowed %s, want the Pod that holds it (%s)", plan.Borrow[0].Pod, podRef)
	}
}

// WITHOUT the intent the same input moves nothing: both models are still wanted,
// and the pool does not preempt a live bridge on its own judgement. This is what
// the signal is for, stated as the difference it makes.
func TestWithoutAnIntentNothingIsPreempted(t *testing.T) {
	in, _ := twoModelPod()

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour, Retained: true})

	if len(plan.Return) != 0 {
		t.Errorf("returns = %+v, want none without an intent", plan.Return)
	}
}

// An intent naming the model that is ALREADY awake is a no-op, not a cycle.
// The pool reconciles every few seconds and the optimizer republishes every
// cycle, so an intent that acted every time it was seen would sleep and wake the
// same model forever.
func TestAnIntentForTheAwakeModelChangesNothing(t *testing.T) {
	in, _ := twoModelPod()
	in.WantAwake = awakeVariant

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour, Retained: true})

	if len(plan.Return) != 0 {
		t.Errorf("returns = %+v, want none: that model is already awake", plan.Return)
	}
}

// An intent for a model this pool does not hold moves nothing. Waking it would
// mean warming it first, which is an admission decision with its own budget and
// its own reporting; silently displacing a serving model for it would be the
// pool making a judgement the signal did not carry.
func TestAnIntentForAModelNotResidentIsIgnored(t *testing.T) {
	in, _ := twoModelPod()
	in.WantAwake = "somewhere-else"

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour, Retained: true})

	if len(plan.Return) != 0 {
		t.Errorf("returns = %+v, want none: the pool does not hold that model", plan.Return)
	}
}

// An intent for a model NOTHING is short of moves nothing.
//
// The intent is half a switch: it says which model should hold the pool's GPUs,
// and the borrow loop -- which acts on a shortfall -- is what actually wakes it.
// Act on the first half alone and the pool sleeps the model it is currently
// serving and wakes nothing behind it, so the signal empties the pool instead of
// switching it, and the requests the sleeping bridge was carrying have nowhere
// to go.
//
// A model with no demand entry is the same case: the pool has been told nothing
// about it, which is not a reason to preempt a live bridge for it.
func TestAnIntentForAModelNobodyIsShortOfPreemptsNothing(t *testing.T) {
	in, _ := twoModelPod()
	in.WantAwake = asleepVariant
	// Its ordinary replicas have arrived, so no borrow will be made for it.
	for i := range in.Variants {
		if in.Variants[i].Model.Variant == asleepVariant {
			in.Variants[i].Ready = in.Variants[i].Desired
		}
	}

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour, Retained: true})

	for _, r := range plan.Return {
		if r.Model.Variant == awakeVariant {
			t.Errorf("slept the serving model for an intent that wakes nothing: returns = %+v", plan.Return)
		}
	}
}
