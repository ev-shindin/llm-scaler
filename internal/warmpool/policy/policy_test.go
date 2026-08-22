package policy

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

var now = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

func cfg() Config {
	return Config{
		SleepMinSize:       1,
		MaxHold:            2 * time.Minute,
		AdmissionWindow:    time.Hour,
		MinMissesToAdmit:   2,
		PreloadTop:         0,
		MaxInstancesPerPod: 4,
	}
}

func pod(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "pool", Name: name}
}

func model(variant string) pool.ModelRef {
	return pool.ModelRef{Namespace: "workload", Variant: variant}
}

func resident(podName, variant string, state pool.State) pool.Membership {
	return pool.Membership{Model: model(variant), Pod: pod(podName), State: state}
}

func demand(variant string, desired, ready int) VariantDemand {
	return VariantDemand{Model: model(variant), Desired: desired, Ready: ready}
}

func variantsIn(actions []Action) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Model.Variant)
	}
	return out
}

func TestBorrowCoversTheShortfallTheOrdinaryReplicasCannot(t *testing.T) {
	// Scale-up asks for 3 where 1 is ready: the pool covers the other two while
	// the ordinary replicas start.
	in := Input{
		Memberships: []pool.Membership{
			resident("a", "qwen", pool.Asleep),
			resident("b", "qwen", pool.Asleep),
			resident("c", "qwen", pool.Asleep),
		},
		Variants: []VariantDemand{demand("qwen", 3, 1)},
		Now:      now,
	}
	got := Decide(in, cfg())
	if len(got.Borrow) != 2 {
		t.Fatalf("want 2 borrows, got %d: %+v", len(got.Borrow), got.Borrow)
	}
	if len(got.Blocked) != 0 || len(got.Missed) != 0 {
		t.Errorf("nothing should be blocked or missed: %+v", got)
	}
}

func TestAMissIsNotABlock(t *testing.T) {
	// They name different faults: a miss means the warm set is wrong, a block
	// means the reserve is too small. Conflating them hides which knob to turn.
	byModelAbsent := Input{
		Memberships: []pool.Membership{resident("a", "other", pool.Asleep)},
		Variants:    []VariantDemand{demand("qwen", 2, 0)},
		Now:         now,
	}
	got := Decide(byModelAbsent, cfg())
	if len(got.Missed) != 1 || got.Missed[0].Variant != "qwen" {
		t.Fatalf("a model resident nowhere is a MISS: %+v", got)
	}
	if len(got.Blocked) != 0 {
		t.Errorf("and not a block: %+v", got)
	}

	reserveExhausted := Input{
		// Resident in one Pod only, but two replicas are wanted.
		Memberships: []pool.Membership{resident("a", "qwen", pool.Asleep)},
		Variants:    []VariantDemand{demand("qwen", 3, 0)},
		Now:         now,
	}
	got = Decide(reserveExhausted, cfg())
	if len(got.Borrow) != 1 {
		t.Fatalf("what exists is still borrowed: %+v", got.Borrow)
	}
	if len(got.Blocked) != 1 {
		t.Fatalf("running out of resident Pods is a BLOCK: %+v", got)
	}
}

func TestHandoverReturnsTheBridgeWhenOrdinaryReplicasArrive(t *testing.T) {
	in := Input{
		Memberships: []pool.Membership{resident("a", "qwen", pool.Serving)},
		Variants:    []VariantDemand{demand("qwen", 2, 2)}, // ordinary replicas are up
		Now:         now,
	}
	got := Decide(in, cfg())
	if len(got.Return) != 1 || got.Return[0].Pod != pod("a") {
		t.Fatalf("the bridge must be given back: %+v", got.Return)
	}
	if len(got.Borrow) != 0 {
		t.Errorf("and nothing re-borrowed: %+v", got.Borrow)
	}
}

func TestScaleDownReturnsBorrowedPodsBeforeShrinkingTheSteadyState(t *testing.T) {
	// Demand falls to what the ordinary replicas already serve, so the lent Pod
	// is surplus and goes back -- reserve for every model, rather than capacity
	// for one.
	in := Input{
		Memberships: []pool.Membership{resident("a", "qwen", pool.Serving)},
		Variants:    []VariantDemand{demand("qwen", 1, 1)},
		Now:         now,
	}
	if got := Decide(in, cfg()); len(got.Return) != 1 {
		t.Fatalf("surplus lending must be returned first: %+v", got)
	}
}

func TestABridgeIsReturnedWhenItsHoldExpires(t *testing.T) {
	// The ordinary replicas never arrived -- quota, unschedulable, image pull.
	// Holding forever would convert insurance into capacity for one variant.
	in := Input{
		Memberships: []pool.Membership{resident("a", "qwen", pool.Serving)},
		Variants:    []VariantDemand{demand("qwen", 3, 0)}, // still short
		BorrowedAt:  map[Borrow]time.Time{{Pod: pod("a"), Variant: "qwen"}: now.Add(-5 * time.Minute)},
		Now:         now,
	}
	got := Decide(in, cfg())
	if len(got.Return) != 1 {
		t.Fatalf("an expired bridge must be returned even while the variant is short: %+v", got)
	}
}

func TestAFreshBridgeIsNotReturnedWhileStillNeeded(t *testing.T) {
	in := Input{
		Memberships: []pool.Membership{resident("a", "qwen", pool.Serving)},
		Variants:    []VariantDemand{demand("qwen", 3, 0)},
		BorrowedAt:  map[Borrow]time.Time{{Pod: pod("a"), Variant: "qwen"}: now.Add(-10 * time.Second)},
		Now:         now,
	}
	if got := Decide(in, cfg()); len(got.Return) != 0 {
		t.Fatalf("a bridge in use must be left alone: %+v", got.Return)
	}
}

func TestParkedVariantsAreAdmittedEagerly(t *testing.T) {
	// A parked variant has no ordinary replicas at all, so the pool is its only
	// fast path and its next wake is near-certain.
	in := Input{
		Memberships: []pool.Membership{resident("a", "other", pool.Asleep), resident("b", "other", pool.Asleep)},
		Variants: []VariantDemand{
			{Model: model("parked"), Desired: 0, Ready: 0, Parked: true},
		},
		Now: now,
	}
	got := Decide(in, cfg())
	if len(got.Admit) != 1 || got.Admit[0].Model.Variant != "parked" {
		t.Fatalf("a parked variant should be warmed without waiting for misses: %+v", got.Admit)
	}
}

func TestAOneOffMissIsNotAdmitted(t *testing.T) {
	// The frequency filter: models that spike once must not evict models that
	// spike often.
	in := Input{
		Memberships: []pool.Membership{resident("a", "other", pool.Asleep), resident("b", "other", pool.Asleep)},
		Variants:    []VariantDemand{demand("rare", 1, 0)},
		MissesAt:    map[string][]time.Time{"rare": {now.Add(-time.Minute)}},
		Now:         now,
	}
	if got := Decide(in, cfg()); len(got.Admit) != 0 {
		t.Fatalf("one miss is not a pattern: %+v", got.Admit)
	}

	// A second miss inside the window is.
	in.MissesAt["rare"] = append(in.MissesAt["rare"], now.Add(-2*time.Minute))
	got := Decide(in, cfg())
	if len(got.Admit) != 1 || got.Admit[0].Model.Variant != "rare" {
		t.Fatalf("the second miss should admit: %+v", got.Admit)
	}
}

func TestMissesOutsideTheWindowDoNotCount(t *testing.T) {
	in := Input{
		Memberships: []pool.Membership{resident("a", "other", pool.Asleep), resident("b", "other", pool.Asleep)},
		Variants:    []VariantDemand{demand("rare", 1, 0)},
		MissesAt:    map[string][]time.Time{"rare": {now.Add(-3 * time.Hour), now.Add(-2 * time.Hour)}},
		Now:         now,
	}
	if got := Decide(in, cfg()); len(got.Admit) != 0 {
		t.Fatalf("stale misses must not admit: %+v", got.Admit)
	}
}

func TestAdmissionNeverSpendsTheReserve(t *testing.T) {
	// One free Pod and a floor of one: filling the cache here would leave the
	// pool unable to serve the next spike, which is what it is for.
	c := cfg()
	c.SleepMinSize = 1
	in := Input{
		Memberships: []pool.Membership{resident("a", "other", pool.Asleep)},
		Variants:    []VariantDemand{{Model: model("parked"), Parked: true}},
		Now:         now,
	}
	if got := Decide(in, c); len(got.Admit) != 0 {
		t.Fatalf("admission must not take the reserve below its floor: %+v", got.Admit)
	}
}

func TestNothingIsAdmittedDuringABurst(t *testing.T) {
	// A blocked borrow means spikes are arriving faster than the reserve can
	// serve them. Loading a model now would take another Pod out of the reserve
	// for ~35s, exactly when it is scarcest.
	in := Input{
		Memberships: []pool.Membership{
			resident("a", "qwen", pool.Asleep),
			resident("b", "spare", pool.Asleep),
			resident("c", "spare", pool.Asleep),
		},
		Variants: []VariantDemand{
			demand("qwen", 5, 0), // wants more than the pool holds -> blocked
			{Model: model("parked"), Parked: true},
		},
		Now: now,
	}
	got := Decide(in, cfg())
	if len(got.Blocked) == 0 {
		t.Fatalf("this scenario must block: %+v", got)
	}
	if len(got.Admit) != 0 {
		t.Fatalf("no admissions during a burst: %+v", got.Admit)
	}
}

func TestGrowByReportsTheShortfallAgainstTheFloor(t *testing.T) {
	// Borrowing may take the reserve below its floor -- that is what a reserve
	// is for. What it must not do is stay there silently.
	c := cfg()
	c.SleepMinSize = 2
	in := Input{
		Memberships: []pool.Membership{
			resident("a", "qwen", pool.Asleep),
			resident("b", "qwen", pool.Asleep),
		},
		Variants: []VariantDemand{demand("qwen", 2, 0)},
		Now:      now,
	}
	got := Decide(in, c)
	if len(got.Borrow) != 2 {
		t.Fatalf("both Pods should be lent: %+v", got.Borrow)
	}
	if got.GrowBy != 2 {
		t.Fatalf("GrowBy = %d, want 2: the reserve is empty and the floor is 2", got.GrowBy)
	}
}

func TestGrowByDoesNotDoubleCountAdmissions(t *testing.T) {
	// admissions() removes what it takes from `free` in place, so subtracting
	// them again reported a shortfall that did not exist -- harmless while
	// GrowBy only logs, wrong the moment it drives growth.
	c := cfg()
	c.SleepMinSize = 2
	c.MaxInstancesPerPod = 4
	in := Input{
		Memberships: []pool.Membership{
			resident("a", "x", pool.Asleep),
			resident("b", "y", pool.Asleep),
			resident("c", "z", pool.Asleep),
		},
		Variants: []VariantDemand{{Model: model("parked"), Parked: true}},
		Now:      now,
	}
	got := Decide(in, c)
	if len(got.Admit) != 1 {
		t.Fatalf("expected one admission: %+v", got.Admit)
	}
	// Three free, one spent on the admission, floor of two: exactly met, so
	// nothing is short. Chosen so the two formulas DIFFER -- subtracting the
	// admission twice would report 1. A wider margin hides the defect.
	if got.GrowBy != 0 {
		t.Fatalf("GrowBy = %d, want 0 (2 free against a floor of 2)", got.GrowBy)
	}
}

func TestAnIdlePodCountsAsReserveAndHoldsNothing(t *testing.T) {
	// A Pod with nothing resident is represented by a placeholder membership.
	// It must count toward the reserve -- otherwise a fresh pool reports zero
	// free and can never admit its first model -- while holding no model.
	c := cfg()
	c.SleepMinSize = 0
	idle := pool.Membership{Pod: pod("empty"), State: pool.Absent}
	in := Input{
		Memberships: []pool.Membership{idle},
		Variants:    []VariantDemand{{Model: model("parked"), Parked: true}},
		Now:         now,
	}
	got := Decide(in, c)
	if len(got.Admit) != 1 || got.Admit[0].Pod != pod("empty") {
		t.Fatalf("a fresh pool must be able to admit into its idle Pod: %+v", got.Admit)
	}
	if len(got.Borrow) != 0 || len(got.Blocked) != 0 {
		t.Fatalf("and nothing borrowed or blocked: %+v", got)
	}
}

func TestAdmissionSpreadsAcrossPods(t *testing.T) {
	// Copies piling into one Pod would share a single wake slot, so the roomiest
	// Pod takes the next model.
	c := cfg()
	c.SleepMinSize = 0
	in := Input{
		Memberships: []pool.Membership{
			resident("crowded", "m1", pool.Asleep),
			resident("crowded", "m2", pool.Asleep),
			resident("roomy", "m3", pool.Asleep),
		},
		Variants: []VariantDemand{{Model: model("new"), Parked: true}},
		Now:      now,
	}
	got := Decide(in, c)
	if len(got.Admit) != 1 || got.Admit[0].Pod != pod("roomy") {
		t.Fatalf("admission should land on the roomiest Pod: %+v", got.Admit)
	}
}

func TestAFullPodIsNotOfferedForAdmission(t *testing.T) {
	c := cfg()
	c.SleepMinSize = 0
	c.MaxInstancesPerPod = 2
	in := Input{
		Memberships: []pool.Membership{
			resident("full", "m1", pool.Asleep),
			resident("full", "m2", pool.Asleep),
		},
		Variants: []VariantDemand{{Model: model("new"), Parked: true}},
		Now:      now,
	}
	if got := Decide(in, c); len(got.Admit) != 0 {
		t.Fatalf("a full Pod cannot take another model: %+v", got.Admit)
	}
}

func TestAlreadyResidentModelsAreNotAdmittedAgain(t *testing.T) {
	c := cfg()
	c.SleepMinSize = 0
	in := Input{
		Memberships: []pool.Membership{resident("a", "parked", pool.Asleep), resident("b", "x", pool.Asleep)},
		Variants:    []VariantDemand{{Model: model("parked"), Parked: true}},
		Now:         now,
	}
	if got := Decide(in, c); len(got.Admit) != 0 {
		t.Fatalf("already warm: %+v", variantsIn(got.Admit))
	}
}

func TestReturnedPodsAreAvailableToBorrowInTheSameCycle(t *testing.T) {
	// A Pod handed back by one variant can bridge another immediately: that is
	// the point of a SHARED reserve.
	in := Input{
		Memberships: []pool.Membership{
			// Pod a serves "done", which no longer needs it, and also holds a
			// sleeping copy of "next".
			resident("a", "done", pool.Serving),
			resident("a", "next", pool.Asleep),
		},
		Variants: []VariantDemand{
			demand("done", 1, 1), // handover due
			demand("next", 1, 0), // wants capacity
		},
		Now: now,
	}
	got := Decide(in, cfg())
	if len(got.Return) != 1 {
		t.Fatalf("the finished bridge must be returned: %+v", got.Return)
	}
	if len(got.Borrow) != 1 || got.Borrow[0].Model.Variant != "next" {
		t.Fatalf("the freed Pod should serve the waiting variant: %+v", got.Borrow)
	}
}

func TestDecideDoesNotMutateItsInput(t *testing.T) {
	// The loop calls this on observed state; a decision function that edits what
	// it was given makes the next observation a lie.
	memberships := []pool.Membership{resident("a", "qwen", pool.Serving)}
	in := Input{
		Memberships: memberships,
		Variants:    []VariantDemand{demand("qwen", 1, 1)},
		Now:         now,
	}
	Decide(in, cfg())
	if memberships[0].State != pool.Serving || memberships[0].Pod != pod("a") {
		t.Fatal("Decide mutated the memberships it was given")
	}
}
