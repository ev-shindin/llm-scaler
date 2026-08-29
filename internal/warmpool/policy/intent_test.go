package policy

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

// twoModelPod is one Pod holding two models, one awake and lent.
func twoModelPod() (Input, types.NamespacedName) {
	podRef := types.NamespacedName{Namespace: "tenant", Name: "pool-0"}
	awake := pool.ModelRef{Namespace: "tenant", Variant: "awake"}
	asleep := pool.ModelRef{Namespace: "tenant", Variant: "asleep"}
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
		BorrowedAt: map[Borrow]time.Time{{Pod: podRef, Variant: "awake"}: now},
		Now:        now,
	}
	return in, podRef
}

// The optimizer says which model should hold the pool's GPUs, and the pool moves
// them -- in ONE pass, so there is no stretch with neither model serving.
func TestAnIntentSwitchesTheAwakeModel(t *testing.T) {
	in, podRef := twoModelPod()
	in.WantAwake = "asleep"

	plan := Decide(in, Config{SleepMinSize: 0, MaxHold: time.Hour, Retained: true})

	if len(plan.Return) != 1 || plan.Return[0].Model.Variant != "awake" {
		t.Fatalf("returns = %+v, want the model in the way handed back", plan.Return)
	}
	if len(plan.Borrow) != 1 || plan.Borrow[0].Model.Variant != "asleep" {
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
	in.WantAwake = "awake"

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
