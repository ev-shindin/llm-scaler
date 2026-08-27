package policy

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

var now = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

// qwenVariant is the variant most of these cases are about.
const qwenVariant = "qwen"

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

func withGPUs(m pool.Membership, gpus int) pool.Membership {
	m.Capacity.GPUs = gpus
	return m
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
	if len(got.Missed) != 1 || got.Missed[0].Variant != qwenVariant {
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

func TestPreloadingAdmitsThePopularModelsWithoutWaitingForAMiss(t *testing.T) {
	// Prefetch beats demand-fill when the distribution is known, and skew is
	// what makes a small warm set work at all: with PreloadTop=2 the two
	// busiest variants are warmed before either has missed once.
	//
	// Share is real now -- a variant's portion of the fleet's desired replicas
	// -- so this branch spends reserve slots in production. The ordering it
	// spends them in is the whole point, which is what these cases pin.
	c := cfg()
	c.PreloadTop = 2
	c.SleepMinSize = 0

	in := Input{
		Memberships: []pool.Membership{
			resident("a", "", pool.Absent),
			resident("b", "", pool.Absent),
			resident("c", "", pool.Absent),
		},
		Variants: []VariantDemand{
			{Model: model("rare"), Desired: 1, Ready: 1, Share: 0.05},
			{Model: model("busiest"), Desired: 1, Ready: 1, Share: 0.60},
			{Model: model("second"), Desired: 1, Ready: 1, Share: 0.30},
		},
		Now: now,
	}

	got := variantsIn(Decide(in, c).Admit)
	if len(got) != 2 {
		t.Fatalf("PreloadTop=2 should admit exactly the top two, got %v", got)
	}
	if got[0] != "busiest" || got[1] != "second" {
		t.Errorf("admitted %v, want the busiest first: a small budget is spent where it buys most", got)
	}
	for _, v := range got {
		if v == "rare" {
			t.Error("a variant outside the top C must wait for the frequency filter")
		}
	}
}

func TestPreloadingStillRespectsTheReserve(t *testing.T) {
	// The reserve exists for the next spike. Filling the cache is worth less
	// than being able to answer, so popularity must not be a way around
	// sleepMinSize -- which is exactly what a preload that ignored the budget
	// would be.
	c := cfg()
	c.PreloadTop = 3
	c.SleepMinSize = 2

	in := Input{
		Memberships: []pool.Membership{
			resident("a", "", pool.Absent),
			resident("b", "", pool.Absent),
			resident("c", "", pool.Absent),
		},
		Variants: []VariantDemand{
			{Model: model("busiest"), Desired: 1, Ready: 1, Share: 0.60},
			{Model: model("second"), Desired: 1, Ready: 1, Share: 0.30},
			{Model: model("third"), Desired: 1, Ready: 1, Share: 0.10},
		},
		Now: now,
	}

	got := variantsIn(Decide(in, c).Admit)
	if len(got) != 1 || got[0] != "busiest" {
		t.Fatalf("three free Pods and a floor of two leaves room for one: %v", got)
	}
}

func TestPreloadIsOffWhenNothingReportsPopularity(t *testing.T) {
	// The production configuration: PreloadTop unset. Every Share is zero, and
	// admission must fall through to parking and the frequency filter rather
	// than warming whichever variant happened to sort first.
	in := Input{
		Memberships: []pool.Membership{
			resident("a", "", pool.Absent),
			resident("b", "", pool.Absent),
		},
		Variants: []VariantDemand{
			{Model: model("one"), Desired: 1, Ready: 1},
			{Model: model("two"), Desired: 1, Ready: 1},
		},
		Now: now,
	}
	if got := Decide(in, cfg()); len(got.Admit) != 0 {
		t.Fatalf("with no popularity signal and no misses, nothing is admitted: %+v", got.Admit)
	}
}

func TestAResidentVariantWithNoFreeHolderIsBlockedNotMissed(t *testing.T) {
	// The two faults tell an operator to turn different knobs: a miss says the
	// warm set is too small (raise C), a block says the reserve is (raise K).
	// `holding` searches FREE Pods only, so a variant resident in Pods that are
	// all lent reads as no candidates -- and calling that a miss sends the
	// operator after the wrong one.
	in := Input{
		Memberships: []pool.Membership{
			// Both copies of "qwen" are already serving; nothing is free.
			resident("a", "qwen", pool.Serving),
			resident("b", "qwen", pool.Serving),
		},
		Variants: []VariantDemand{demand("qwen", 5, 0)},
		Now:      now,
	}
	got := Decide(in, cfg())
	if len(got.Missed) != 0 {
		t.Errorf("a resident variant did not miss: %+v", got.Missed)
	}
	if len(got.Blocked) != 1 || got.Blocked[0].Variant != qwenVariant {
		t.Fatalf("want qwen blocked, got %+v", got.Blocked)
	}
}

func TestAVariantResidentNowhereStillMisses(t *testing.T) {
	// The other half of the same distinction: nothing holds this model, so the
	// warm set really is the thing that was wrong.
	in := Input{
		Memberships: []pool.Membership{resident("a", "other", pool.Asleep)},
		Variants:    []VariantDemand{demand("stranger", 2, 0)},
		Now:         now,
	}
	got := Decide(in, cfg())
	if len(got.Blocked) != 0 {
		t.Errorf("a variant resident nowhere is not blocked: %+v", got.Blocked)
	}
	if len(got.Missed) != 1 || got.Missed[0].Variant != "stranger" {
		t.Fatalf("want stranger missed, got %+v", got.Missed)
	}
}

func TestAVariantResidentOnlyInALoadingPodIsBlocked(t *testing.T) {
	// A Pod mid-admission cannot serve a wake -- that is why Loading is not
	// reserve -- but the model IS resident, so the shortfall is one of free
	// Pods and not of coverage.
	in := Input{
		Memberships: []pool.Membership{resident("a", "qwen", pool.Loading)},
		Variants:    []VariantDemand{demand("qwen", 2, 0)},
		Now:         now,
	}
	got := Decide(in, cfg())
	if len(got.Blocked) != 1 || len(got.Missed) != 0 {
		t.Fatalf("blocked=%+v missed=%+v, want blocked", got.Blocked, got.Missed)
	}
}

func TestAnAwakeEngineNobodyIsUsingIsHandedBack(t *testing.T) {
	// The state a controller restart leaves behind. Warm creates an instance,
	// waits for it to serve, and only then sleeps it; a restart between those
	// last two steps leaves an engine awake, in no InferencePool, with nothing
	// pointed at it -- burning a GPU for no one.
	in := Input{
		Memberships: []pool.Membership{resident("a", "qwen", pool.Waking)},
		Variants:    []VariantDemand{demand("qwen", 1, 1)},
		Now:         now,
	}
	got := Decide(in, cfg())
	if len(got.Return) != 1 {
		t.Fatalf("an awake engine nobody is using must be handed back: %+v", got.Return)
	}
	if got.Return[0].Pod != pod("a") || got.Return[0].Model.Variant != qwenVariant {
		t.Errorf("returned %+v, want qwen from pod a", got.Return[0])
	}
}

func TestAnOrphanDoesNotCountAsCoverForItsVariant(t *testing.T) {
	// The damage the old accounting did. An orphan charged to a variant made it
	// read as already covered, so the borrow it actually needed was suppressed
	// -- and because a restart also loses the borrow times, the hold timeout
	// that would eventually free the Pod restarted from zero.
	in := Input{
		Memberships: []pool.Membership{
			resident("a", "qwen", pool.Waking), // awake, serving nothing
			resident("b", "qwen", pool.Asleep),
		},
		Variants: []VariantDemand{demand("qwen", 2, 1)},
		Now:      now,
	}
	got := Decide(in, cfg())
	if len(got.Borrow) != 1 {
		t.Fatalf("the shortfall must be covered rather than read as already met: %+v", got.Borrow)
	}
}

func TestAnOrphanIsFinishedRatherThanSleptAndWokenAgain(t *testing.T) {
	// It is already awake, for the right model. Sleeping it and waking it again
	// inside one plan would cost a drain, a sleep and a wake to reach the state
	// it was in to begin with -- so the return is dropped and the borrow
	// finishes the activation the interrupted sequence began.
	in := Input{
		Memberships: []pool.Membership{resident("a", "qwen", pool.Waking)},
		Variants:    []VariantDemand{demand("qwen", 1, 0)},
		Now:         now,
	}
	got := Decide(in, cfg())
	if len(got.Borrow) != 1 || got.Borrow[0].Pod != pod("a") {
		t.Fatalf("the orphan must be put to work: %+v", got.Borrow)
	}
	if len(got.Return) != 0 {
		t.Errorf("and not slept on the way: %+v", got.Return)
	}
}

func TestAnOrphanNobodyNeedsIsStillPutBackToSleep(t *testing.T) {
	// The other half. With no shortfall to finish it into, an awake engine
	// serving nothing is simply burning a GPU, and belongs in the reserve.
	in := Input{
		Memberships: []pool.Membership{resident("a", "qwen", pool.Waking)},
		Variants:    []VariantDemand{demand("qwen", 1, 1)},
		Now:         now,
	}
	got := Decide(in, cfg())
	if len(got.Return) != 1 || got.Return[0].Pod != pod("a") {
		t.Fatalf("an unwanted orphan must be handed back: %+v", got.Return)
	}
	if len(got.Borrow) != 0 {
		t.Errorf("and not borrowed: %+v", got.Borrow)
	}
}

func TestAServingBridgeIsNotMistakenForAnOrphan(t *testing.T) {
	// Serving means traffic is pointed at it. Returning that would take a live
	// bridge out from under the requests it is carrying.
	in := Input{
		Memberships: []pool.Membership{resident("a", "qwen", pool.Serving)},
		Variants:    []VariantDemand{demand("qwen", 2, 0)},
		Now:         now,
	}
	got := Decide(in, cfg())
	if len(got.Return) != 0 {
		t.Fatalf("a serving bridge that is still needed must not be returned: %+v", got.Return)
	}
}

func TestAModelNeedingMoreGPUsThanAPodHasIsDeclined(t *testing.T) {
	// A warm copy inherits the ordinary replicas' parallelism flags, so a
	// tensor-parallel workload asks for more devices than a single-GPU pool Pod
	// has. Without this the admission is accepted, the engine cannot start, and
	// the ~35 s load is spent and re-spent every cycle.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0 // the reserve floor is not what these cases are about

	big := VariantDemand{
		Model: pool.ModelRef{
			Namespace:     "workload",
			Variant:       "tp2",
			EngineOptions: "--model meta-llama/Llama-3.1-8B --tensor-parallel-size 2",
		},
		Parked: true,
	}
	in := Input{
		Memberships: []pool.Membership{withGPUs(resident("a", "", pool.Absent), 1)},
		Variants:    []VariantDemand{big},
		Now:         now,
	}
	got := Decide(in, c)
	if len(got.Admit) != 0 {
		t.Fatalf("a two-GPU engine must not be admitted into a one-GPU Pod: %+v", got.Admit)
	}
	if len(got.Declined) != 1 || !strings.Contains(got.Declined[0].Reason, "GPU") {
		t.Fatalf("and the reason must say so: %+v", got.Declined)
	}
}

func TestThePodsOwnGPUCountDecidesWhatFits(t *testing.T) {
	// The positive half, and the reason --warm-pool-gpus-per-pod could go: the
	// SAME tensor-parallel model that a one-GPU Pod declines is admitted by a
	// two-GPU one, with no configuration telling the controller either number.
	// Only the Pod's declared capacity differs between the two cases.
	tp2 := VariantDemand{
		Model: pool.ModelRef{
			Namespace:     "workload",
			Variant:       "tp2",
			EngineOptions: "--model Qwen/Qwen3-0.6B --tensor-parallel-size 2",
		},
		Parked: true,
	}
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	small := Decide(Input{
		Memberships: []pool.Membership{withGPUs(resident("a", "", pool.Absent), 1)},
		Variants:    []VariantDemand{tp2},
		Now:         now,
	}, c)
	if len(small.Admit) != 0 {
		t.Fatalf("a one-GPU Pod cannot run a TP=2 engine: %+v", small.Admit)
	}

	big := Decide(Input{
		Memberships: []pool.Membership{withGPUs(resident("a", "", pool.Absent), 2)},
		Variants:    []VariantDemand{tp2},
		Now:         now,
	}, c)
	if len(big.Admit) != 1 {
		t.Fatalf("a two-GPU Pod must take it: admit=%+v declined=%+v", big.Admit, big.Declined)
	}
}

func TestAModelPinnedToAnotherAcceleratorIsDeclined(t *testing.T) {
	// The right NUMBER of the wrong GPU is still the wrong GPU. A warm copy is
	// only reusable on the accelerator it was loaded on, and the workload's own
	// affinity would refuse the node this Pod is on, so a bridge from here could
	// not serve it at all -- the load is spent for nothing.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	onA100 := withGPUs(resident("a", "", pool.Absent), 1)
	onA100.Capacity.Accelerator = "NVIDIA-A100-SXM4-80GB"

	wantsH100 := VariantDemand{
		Model: pool.ModelRef{
			Namespace:     "workload",
			Variant:       "pinned",
			EngineOptions: smallModel,
			Accelerator:   "NVIDIA-H100-80GB-HBM3",
		},
		Parked: true,
	}
	got := Decide(Input{
		Memberships: []pool.Membership{onA100},
		Variants:    []VariantDemand{wantsH100},
		Now:         now,
	}, c)
	if len(got.Admit) != 0 {
		t.Fatalf("an H100 model must not be warmed on an A100 Pod: %+v", got.Admit)
	}
	if len(got.Declined) != 1 || !strings.Contains(got.Declined[0].Reason, "A100") {
		t.Fatalf("and the reason must name what the Pod actually holds: %+v", got.Declined)
	}

	// Same Pod, same model, matching accelerator: admitted.
	onH100 := withGPUs(resident("a", "", pool.Absent), 1)
	onH100.Capacity.Accelerator = "NVIDIA-H100-80GB-HBM3"
	ok := Decide(Input{
		Memberships: []pool.Membership{onH100},
		Variants:    []VariantDemand{wantsH100},
		Now:         now,
	}, c)
	if len(ok.Admit) != 1 {
		t.Fatalf("a matching accelerator must be admitted: admit=%+v declined=%+v", ok.Admit, ok.Declined)
	}
}

func TestAModelTooLargeForTheBudgetIsDeclined(t *testing.T) {
	// The expensive one to get wrong. A level-1 sleeper keeps its weights in
	// HOST memory against a hard container limit, so one model too many does not
	// fail its own admission -- it OOM-kills the launcher and takes every model
	// already resident in that Pod with it.
	c := cfg()
	c.PodMemoryBytes = 20 << 30 // room for one 8B, not two
	c.SleepMinSize = 0

	in := Input{
		Memberships: []pool.Membership{{
			Pod:      pod("a"),
			Model:    pool.ModelRef{Variant: "first", EngineOptions: "--model meta-llama/Llama-3.1-8B"},
			State:    pool.Asleep,
			Capacity: pool.PodCapacity{GPUs: 1},
		}},
		Variants: []VariantDemand{{
			Model: pool.ModelRef{
				Variant:       "second",
				EngineOptions: "--model meta-llama/Llama-3.1-8B",
			},
			Parked: true,
		}},
		Now: now,
	}
	got := Decide(in, c)
	if len(got.Admit) != 0 {
		t.Fatalf("a second 8B must not be admitted into a 20GiB budget: %+v", got.Admit)
	}
	if len(got.Declined) != 1 || !strings.Contains(got.Declined[0].Reason, "budget") {
		t.Fatalf("and the reason must say so: %+v", got.Declined)
	}
}

func TestAModelThatFitsIsStillAdmitted(t *testing.T) {
	// The check must not become a blanket refusal.
	c := cfg()
	c.PodMemoryBytes = 20 << 30
	c.SleepMinSize = 0

	in := Input{
		Memberships: []pool.Membership{withGPUs(resident("a", "", pool.Absent), 1)},
		Variants: []VariantDemand{{
			Model: pool.ModelRef{
				Variant:       "small",
				EngineOptions: smallModel,
			},
			Parked: true,
		}},
		Now: now,
	}
	got := Decide(in, c)
	if len(got.Admit) != 1 {
		t.Fatalf("a 0.6B fits a 20GiB budget: admit=%+v declined=%+v", got.Admit, got.Declined)
	}
}

func TestAModelWhoseSizeCannotBeWorkedOutIsDeclined(t *testing.T) {
	// Consistent with the engine-flag allowlist: the pool refuses to guess. A
	// warm copy it declines to make costs a cold start; one that does not fit
	// costs every model in the Pod.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	in := Input{
		Memberships: []pool.Membership{withGPUs(resident("a", "", pool.Absent), 1)},
		Variants: []VariantDemand{{
			Model:  pool.ModelRef{Variant: "mystery", EngineOptions: "--model BAAI/bge-m3"},
			Parked: true,
		}},
		Now: now,
	}
	got := Decide(in, c)
	if len(got.Admit) != 0 {
		t.Fatalf("an unsizeable model must not be admitted: %+v", got.Admit)
	}
	if len(got.Declined) != 1 {
		t.Fatalf("and must be reported as declined: %+v", got.Declined)
	}
}

func TestTheFitCheckIsOffWhenNoBudgetIsSet(t *testing.T) {
	// An unset field must not be silently restrictive.
	c := cfg()
	c.PodMemoryBytes = 0
	c.SleepMinSize = 0

	in := Input{
		Memberships: []pool.Membership{withGPUs(resident("a", "", pool.Absent), 1)},
		Variants: []VariantDemand{{
			Model:  pool.ModelRef{Variant: "mystery", EngineOptions: "--model BAAI/bge-m3"},
			Parked: true,
		}},
		Now: now,
	}
	if got := Decide(in, c); len(got.Admit) != 1 {
		t.Fatalf("with no budget the check is off: admit=%+v declined=%+v", got.Admit, got.Declined)
	}
}

func TestAReturnCarriesTheLabelsItHasToRemove(t *testing.T) {
	// An action built from an OBSERVATION carries no PoolLabels -- ListWarm
	// learns a variant's name and options from the supervisor, and the selector
	// comes from the tenant's InferencePool, which only demand has read. Since
	// Deactivate removes exactly the labels it is handed, a return built that
	// way removed nothing at all: the Pod kept every InferencePool selector it
	// had ever served, and the next model woken in it went Ready as an endpoint
	// of BOTH pools.
	withLabels := pool.ModelRef{
		Namespace:  "workload",
		Variant:    qwenVariant,
		PoolLabels: map[string]string{"llm-d.ai/model": "qwen", "llm-d.ai/inferenceServing": "true"},
	}
	in := Input{
		// The membership is what an observation produces: no labels on it.
		Memberships: []pool.Membership{{
			Pod:   pod("a"),
			Model: pool.ModelRef{Namespace: "workload", Variant: qwenVariant},
			State: pool.Serving,
		}},
		Variants: []VariantDemand{{Model: withLabels, Desired: 1, Ready: 1}},
		Now:      now,
	}
	got := Decide(in, cfg())
	if len(got.Return) != 1 {
		t.Fatalf("the bridge is no longer needed and must be returned: %+v", got.Return)
	}
	if len(got.Return[0].Model.PoolLabels) != 2 {
		t.Fatalf("the return carries %v; without the selector it removes nothing",
			got.Return[0].Model.PoolLabels)
	}
}

func TestAnOrphanReturnAlsoCarriesTheLabels(t *testing.T) {
	// Same requirement on the other return path. An orphan is an engine left
	// awake by a sequence that did not finish -- it may well have been labelled
	// before it was abandoned.
	withLabels := pool.ModelRef{
		Namespace:  "workload",
		Variant:    qwenVariant,
		PoolLabels: map[string]string{"llm-d.ai/model": "qwen"},
	}
	in := Input{
		Memberships: []pool.Membership{{
			Pod:   pod("a"),
			Model: pool.ModelRef{Namespace: "workload", Variant: qwenVariant},
			State: pool.Waking,
		}},
		Variants: []VariantDemand{{Model: withLabels, Desired: 1, Ready: 1}},
		Now:      now,
	}
	got := Decide(in, cfg())
	if len(got.Return) != 1 || len(got.Return[0].Model.PoolLabels) != 1 {
		t.Fatalf("an orphan return must carry the selector too: %+v", got.Return)
	}
}

func TestAVariantDemandNoLongerKnowsAboutKeepsTheObservation(t *testing.T) {
	// Deregistered while lent. Nothing better is available than what the
	// observation saw, and the plan must still be produced -- leaving the Pod
	// lent forever would be worse than leaving a label behind.
	in := Input{
		Memberships: []pool.Membership{{
			Pod:   pod("a"),
			Model: pool.ModelRef{Namespace: "workload", Variant: "forgotten"},
			State: pool.Waking,
		}},
		Now: now,
	}
	got := Decide(in, cfg())
	if len(got.Return) != 1 || got.Return[0].Model.Variant != "forgotten" {
		t.Fatalf("want the orphan still returned: %+v", got.Return)
	}
}

func TestTheHoldExpiresExactlyAtMaxHold(t *testing.T) {
	// The comparison is >=, and the tests around it use five minutes against a
	// two-minute hold and ten seconds against the same -- both so far from the
	// edge that flipping >= to > survives them. A bridge exactly at its limit
	// is the case the operator asked for when they set the number.
	at := func(age time.Duration) Input {
		return Input{
			Memberships: []pool.Membership{resident("a", qwenVariant, pool.Serving)},
			Variants:    []VariantDemand{demand(qwenVariant, 3, 0)}, // still short
			BorrowedAt:  map[Borrow]time.Time{{Pod: pod("a"), Variant: qwenVariant}: now.Add(-age)},
			Now:         now,
		}
	}
	c := cfg()
	if got := Decide(at(c.MaxHold), c); len(got.Return) != 1 {
		t.Errorf("a bridge exactly at the hold must be returned: %+v", got.Return)
	}
	if got := Decide(at(c.MaxHold-time.Nanosecond), c); len(got.Return) != 0 {
		t.Errorf("and one a nanosecond short must not: %+v", got.Return)
	}
}

func TestAMissExactlyAtTheWindowEdgeStillCounts(t *testing.T) {
	// Same shape as the hold: the comparison is <=, and the cases around it use
	// misses two and three hours old against a one-hour window, which an
	// off-by-one survives untouched.
	c := cfg()
	at := func(age time.Duration) Input {
		return Input{
			Memberships: []pool.Membership{resident("a", "other", pool.Asleep), resident("b", "other", pool.Asleep)},
			Variants:    []VariantDemand{demand("rare", 1, 0)},
			MissesAt:    map[string][]time.Time{"rare": {now.Add(-age), now.Add(-age)}},
			Now:         now,
		}
	}
	if got := Decide(at(c.AdmissionWindow), c); len(got.Admit) != 1 {
		t.Errorf("a miss exactly at the window edge still counts: %+v", got.Admit)
	}
	if got := Decide(at(c.AdmissionWindow+time.Nanosecond), c); len(got.Admit) != 0 {
		t.Errorf("and one a nanosecond past it does not: %+v", got.Admit)
	}
}

func TestReturnsHandBackTheOldestBridgeFirst(t *testing.T) {
	// With more lent Pods than excess, WHICH one comes back decides whether the
	// pool notices a scale-up that is failing. Returning the freshest would
	// leave a stuck bridge lent indefinitely while its hold clock is repeatedly
	// reset -- and nothing else in the suite sets up partial excess at all.
	in := Input{
		Memberships: []pool.Membership{
			resident("new", qwenVariant, pool.Serving),
			resident("old", qwenVariant, pool.Serving),
		},
		Variants: []VariantDemand{demand(qwenVariant, 3, 2)}, // room to give one back
		BorrowedAt: map[Borrow]time.Time{
			{Pod: pod("new"), Variant: qwenVariant}: now.Add(-10 * time.Second),
			{Pod: pod("old"), Variant: qwenVariant}: now.Add(-90 * time.Second),
		},
		Now: now,
	}
	got := Decide(in, cfg())
	if len(got.Return) != 1 {
		t.Fatalf("exactly one bridge is spare: %+v", got.Return)
	}
	if got.Return[0].Pod != pod("old") {
		t.Errorf("returned %s, want the oldest bridge", got.Return[0].Pod)
	}
}

// smallModel is an engine command line whose size is known and tiny, so these
// cases turn on the rule under test rather than on the memory budget.
const smallModel = "--model Qwen/Qwen3-0.6B"

func copies(n int) *int { return &n }

func TestAVariantCanAskForSeveralWarmCopies(t *testing.T) {
	// The case automatic mode cannot express at all. One warm copy bridges one
	// scale-up, so a variant that scales twice in quick succession takes a cold
	// start for the second with free Pods sitting beside it.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	in := Input{
		Memberships: []pool.Membership{
			withGPUs(resident("a", "", pool.Absent), 1),
			withGPUs(resident("b", "", pool.Absent), 1),
		},
		Variants: []VariantDemand{{
			Model:      pool.ModelRef{Variant: "busy", EngineOptions: smallModel},
			WarmCopies: copies(2),
		}},
		Now: now,
	}
	got := Decide(in, c)
	if len(got.Admit) != 2 {
		t.Fatalf("two copies were asked for: admit=%+v declined=%+v", got.Admit, got.Declined)
	}
	// And they must land in DIFFERENT Pods, or the second shares the first's
	// GPUs and could never serve a second bridge.
	if got.Admit[0].Pod == got.Admit[1].Pod {
		t.Fatalf("both copies went to the same Pod: %+v", got.Admit)
	}
}

func TestASecondCopyIsNotAdmittedWhenOnlyOneWasAsked(t *testing.T) {
	// Automatic mode is one copy, which is the behaviour every existing install
	// has. A second free Pod must not quietly become a second copy.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	in := Input{
		Memberships: []pool.Membership{
			withGPUs(resident("a", "", pool.Absent), 1),
			withGPUs(resident("b", "", pool.Absent), 1),
		},
		Variants: []VariantDemand{{
			Model:  pool.ModelRef{Variant: "qwen", EngineOptions: smallModel},
			Parked: true,
		}},
		Now: now,
	}
	if got := Decide(in, c); len(got.Admit) != 1 {
		t.Fatalf("automatic mode holds one copy: %+v", got.Admit)
	}
}

func TestZeroCopiesOptsOutEvenWhenParked(t *testing.T) {
	// The point of writing "0" is to free the slot for models that gain more.
	// Parked is the strongest automatic reason to warm something, so if it can
	// override the opt-out the setting is useless.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	in := Input{
		Memberships: []pool.Membership{withGPUs(resident("a", "", pool.Absent), 1)},
		Variants: []VariantDemand{{
			Model:      pool.ModelRef{Variant: "optout", EngineOptions: smallModel},
			Parked:     true,
			WarmCopies: copies(0),
		}},
		Now: now,
	}
	if got := Decide(in, c); len(got.Admit) != 0 {
		t.Fatalf("an explicit opt-out must win over parking: %+v", got.Admit)
	}
}

func TestAPinnedVariantOutranksAPopularOne(t *testing.T) {
	// A low-traffic but latency-critical model is exactly what popularity
	// ranking can never warm. With one slot free, the pinned variant takes it.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0
	c.PreloadTop = 5

	in := Input{
		Memberships: []pool.Membership{withGPUs(resident("a", "", pool.Absent), 1)},
		Variants: []VariantDemand{
			{
				Model: pool.ModelRef{Variant: "popular", EngineOptions: smallModel},
				Share: 0.99,
			},
			{
				Model:      pool.ModelRef{Variant: "pinned", EngineOptions: smallModel},
				Share:      0.01,
				WarmCopies: copies(1),
			},
		},
		Now: now,
	}
	got := Decide(in, c)
	if len(got.Admit) != 1 {
		t.Fatalf("one slot, one admission: %+v", got.Admit)
	}
	if got.Admit[0].Model.Variant != "pinned" {
		t.Fatalf("the pinned variant must take the slot, got %q", got.Admit[0].Model.Variant)
	}
}

func TestMoreCopiesThanPodsTakesWhatThereIs(t *testing.T) {
	// Asking for more than the pool can hold must not stall the loop or starve
	// everything else -- it takes what is free and stops.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	in := Input{
		Memberships: []pool.Membership{withGPUs(resident("a", "", pool.Absent), 1)},
		Variants: []VariantDemand{{
			Model:      pool.ModelRef{Variant: "greedy", EngineOptions: smallModel},
			WarmCopies: copies(5),
		}},
		Now: now,
	}
	if got := Decide(in, c); len(got.Admit) != 1 {
		t.Fatalf("one Pod, one copy: %+v", got.Admit)
	}
}

func TestASecondCopyAvoidsThePodThatAlreadyHoldsOne(t *testing.T) {
	// The case that distinguishes a real exclusion from an accident. A Pod
	// holding a SLEEPING copy is still free, so the roomiest-free-Pod search can
	// return the very Pod that already has this model. A second copy there would
	// share the first's GPUs and could never serve a second bridge -- and the
	// supervisor keys instances by variant, so the create would be refused.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	// Pod "a" is the ROOMIEST -- one resident model against "b"'s two -- so the
	// ordinary search returns it, and only the exclusion sends the copy
	// elsewhere. With "b" roomier the test would pass either way and prove
	// nothing, which is exactly what an earlier version of it did.
	holding := withGPUs(resident("a", "busy", pool.Asleep), 1)
	holding.Model.EngineOptions = smallModel
	otherA := withGPUs(resident("b", "other-1", pool.Asleep), 1)
	otherA.Model.EngineOptions = smallModel
	otherB := withGPUs(resident("b", "other-2", pool.Asleep), 1)
	otherB.Model.EngineOptions = smallModel

	in := Input{
		Memberships: []pool.Membership{holding, otherA, otherB},
		Variants: []VariantDemand{{
			Model:      pool.ModelRef{Variant: "busy", EngineOptions: smallModel},
			WarmCopies: copies(2),
		}},
		Now: now,
	}
	got := Decide(in, c)
	if len(got.Admit) != 1 {
		t.Fatalf("one copy exists, so exactly one more is needed: %+v", got.Admit)
	}
	if got.Admit[0].Pod.Name != "b" {
		t.Fatalf("the second copy must go to the Pod without one, got %q", got.Admit[0].Pod.Name)
	}
}

func TestZeroCopiesReleasesAModelAlreadyWarm(t *testing.T) {
	// Found on a cluster: setting "0" on a resident model left it warm forever,
	// because eviction was never implemented. The setting then silently meant
	// "at least this many", which is not what it says and not what an operator
	// asking to free a slot needs.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	held := withGPUs(resident("a", "leaving", pool.Asleep), 1)
	held.Model.EngineOptions = smallModel

	got := Decide(Input{
		Memberships: []pool.Membership{held},
		Variants: []VariantDemand{{
			Model:      pool.ModelRef{Variant: "leaving", EngineOptions: smallModel},
			WarmCopies: copies(0),
		}},
		Now: now,
	}, c)
	if len(got.Evict) != 1 || got.Evict[0].Model.Variant != "leaving" {
		t.Fatalf("an opted-out model must be released: %+v", got.Evict)
	}
}

func TestLoweringTheCountReleasesOnlyTheExcess(t *testing.T) {
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	first := withGPUs(resident("a", "busy", pool.Asleep), 1)
	first.Model.EngineOptions = smallModel
	first.LastUsed = now.Add(-time.Hour) // the older copy
	second := withGPUs(resident("b", "busy", pool.Asleep), 1)
	second.Model.EngineOptions = smallModel
	second.LastUsed = now

	got := Decide(Input{
		Memberships: []pool.Membership{first, second},
		Variants: []VariantDemand{{
			Model:      pool.ModelRef{Variant: "busy", EngineOptions: smallModel},
			WarmCopies: copies(1),
		}},
		Now: now,
	}, c)
	if len(got.Evict) != 1 {
		t.Fatalf("two copies down to one releases exactly one: %+v", got.Evict)
	}
	if got.Evict[0].Pod.Name != "a" {
		t.Errorf("the LEAST recently used copy goes, got %q", got.Evict[0].Pod.Name)
	}
}

func TestALentCopyIsNeverEvicted(t *testing.T) {
	// It is serving live traffic. The hold timeout already returns it; taking it
	// out from under a request to satisfy a configuration change trades a real
	// response for a bookkeeping one.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	serving := withGPUs(resident("a", "busy", pool.Serving), 1)
	serving.Model.EngineOptions = smallModel

	got := Decide(Input{
		Memberships: []pool.Membership{serving},
		Variants: []VariantDemand{{
			Model:      pool.ModelRef{Variant: "busy", EngineOptions: smallModel},
			WarmCopies: copies(0),
			Desired:    2,
			Ready:      1,
		}},
		Now: now,
	}, c)
	for _, e := range got.Evict {
		if e.Pod.Name == "a" {
			t.Fatalf("a serving copy must not be evicted: %+v", got.Evict)
		}
	}
}

func TestAutomaticModeNeverEvicts(t *testing.T) {
	// A model automatic mode warmed is one it judged worth warming, and it holds
	// no more than one copy of each, so there is nothing for eviction to fix.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	held := withGPUs(resident("a", "auto", pool.Asleep), 1)
	held.Model.EngineOptions = smallModel

	got := Decide(Input{
		Memberships: []pool.Membership{held},
		Variants:    []VariantDemand{{Model: pool.ModelRef{Variant: "auto", EngineOptions: smallModel}}},
		Now:         now,
	}, c)
	if len(got.Evict) != 0 {
		t.Fatalf("automatic mode must not evict: %+v", got.Evict)
	}
}

func TestAutomaticModeLeavesExtraCopiesAlone(t *testing.T) {
	// The case that distinguishes "automatic never evicts" from "automatic has
	// nothing to evict". A variant can hold two copies and then lose its
	// explicit count -- someone removes warmPoolCopies from the trigger -- and
	// automatic mode must not start tearing down what it did not put there.
	// With only ONE copy held, want defaults to 1 and no eviction follows
	// either way, so that fixture proves nothing.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	first := withGPUs(resident("a", "auto", pool.Asleep), 1)
	first.Model.EngineOptions = smallModel
	second := withGPUs(resident("b", "auto", pool.Asleep), 1)
	second.Model.EngineOptions = smallModel

	got := Decide(Input{
		Memberships: []pool.Membership{first, second},
		Variants:    []VariantDemand{{Model: pool.ModelRef{Variant: "auto", EngineOptions: smallModel}}},
		Now:         now,
	}, c)
	if len(got.Evict) != 0 {
		t.Fatalf("automatic mode must leave copies it did not choose: %+v", got.Evict)
	}
}

func TestAGrownPodIsWarmedWhileABridgeIsStillOpen(t *testing.T) {
	// The deadlock the empty-Pod rule exists to break, and the exact case a pool
	// scale-up is FOR. A block means the model is warm but every Pod holding it
	// is serving; a borrow can only take a Pod that already holds the model, so
	// a fresh empty Pod cannot relieve it. The remedy is another warm copy --
	// which the old code suppressed precisely because something was blocked. The
	// pool grew a Pod to fix the block and then refused to use it.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	serving := withGPUs(resident("a", "busy", pool.Serving), 1)
	serving.Model.EngineOptions = smallModel
	fresh := withGPUs(resident("b", "", pool.Absent), 1)

	got := Decide(Input{
		Memberships: []pool.Membership{serving, fresh},
		Variants: []VariantDemand{{
			Model:      pool.ModelRef{Variant: "busy", EngineOptions: smallModel},
			Desired:    3,
			Ready:      1,
			WarmCopies: copies(2),
		}},
		Now: now,
	}, c)
	if len(got.Blocked) != 1 {
		t.Fatalf("the fixture must actually be blocked, or this proves nothing: %+v", got.Blocked)
	}
	if len(got.Admit) != 1 {
		t.Fatalf("the empty Pod must be warmed despite the block: %+v", got.Admit)
	}
	if got.Admit[0].Pod.Name != "b" {
		t.Errorf("into the EMPTY Pod, got %q", got.Admit[0].Pod.Name)
	}
}

func TestMidBurstAPodHoldingWarmModelsIsLeftBorrowable(t *testing.T) {
	// The half of the old guard that was worth keeping. Admitting into a Pod
	// takes it out of the free set for the whole load, so a Pod holding other
	// warm models would stop being borrowable for them mid-burst. Only Pods
	// holding nothing may be spent.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	serving := withGPUs(resident("a", "busy", pool.Serving), 1)
	serving.Model.EngineOptions = smallModel
	// Free, but holding a warm model someone could borrow.
	occupied := withGPUs(resident("b", "other", pool.Asleep), 1)
	occupied.Model.EngineOptions = smallModel

	got := Decide(Input{
		Memberships: []pool.Membership{serving, occupied},
		Variants: []VariantDemand{
			{Model: pool.ModelRef{Variant: "busy", EngineOptions: smallModel}, Desired: 3, Ready: 1, WarmCopies: copies(2)},
			{Model: pool.ModelRef{Variant: "other", EngineOptions: smallModel}},
		},
		Now: now,
	}, c)
	if len(got.Blocked) != 1 {
		t.Fatalf("the fixture must be blocked: %+v", got.Blocked)
	}
	for _, a := range got.Admit {
		if a.Pod.Name == "b" {
			t.Fatalf("a Pod holding borrowable models must not be spent mid-burst: %+v", got.Admit)
		}
	}
}

func TestAPodWithNoDeclaredGPUStillReportsItsAccelerator(t *testing.T) {
	// Found by an e2e that could not fail. capacityOf required a GPU count or a
	// memory limit before it would return anything, so a Pod declaring neither
	// discarded the accelerator read from its node -- and the fit check, seeing
	// none, treated every model as portable and warmed one onto hardware its own
	// affinity refuses.
	//
	// A real pool Pod always requests a GPU, which is what hid it. An emulated
	// one does not, so the suite written to prove the accelerator rule was the
	// one place it silently did not hold.
	c := cfg()
	c.PodMemoryBytes = 100 << 30
	c.SleepMinSize = 0

	emulated := resident("a", "", pool.Absent) // no GPUs, no memory limit
	emulated.Capacity.Accelerator = "HELD"

	got := Decide(Input{
		Memberships: []pool.Membership{emulated},
		Variants: []VariantDemand{{
			Model:  pool.ModelRef{Variant: "pinned", EngineOptions: smallModel, Accelerator: "WANTED"},
			Parked: true,
		}},
		Now: now,
	}, c)
	if len(got.Admit) != 0 {
		t.Fatalf("a Pod with no GPU request still knows what it sits on: %+v", got.Admit)
	}
	if len(got.Declined) != 1 || !strings.Contains(got.Declined[0].Reason, "HELD") {
		t.Fatalf("and the decline must name it: %+v", got.Declined)
	}
}

func TestAPermanentDeclineIsToldApartFromAFullPod(t *testing.T) {
	// Which fault it is decides which knob an operator turns -- the same
	// distinction this policy already draws between a MISS and a BLOCK. A model
	// on the wrong accelerator needs its configuration fixed; a Pod with no room
	// left needs capacity, and resolves on its own when something is evicted.
	// Reporting both as "declined" leaves an operator unable to tell which.
	c := cfg()
	c.SleepMinSize = 0
	c.PodMemoryBytes = 100 << 30

	wrongAccel := withGPUs(resident("a", "", pool.Absent), 1)
	wrongAccel.Capacity.Accelerator = "HELD"
	got := Decide(Input{
		Memberships: []pool.Membership{wrongAccel},
		Variants: []VariantDemand{{
			Model:  pool.ModelRef{Variant: "pinned", EngineOptions: smallModel, Accelerator: "WANTED"},
			Parked: true,
		}},
		Now: now,
	}, c)
	if len(got.Declined) != 1 || !got.Declined[0].Permanent {
		t.Fatalf("an accelerator mismatch can never resolve: %+v", got.Declined)
	}

	// A full Pod is the other kind: the budget is spent on what is resident, so
	// an eviction or a bigger pool fixes it.
	tight := cfg()
	tight.SleepMinSize = 0
	tight.PodMemoryBytes = 20 << 30 // room for one 8B, not two
	full := pool.Membership{
		Pod:      pod("a"),
		Model:    pool.ModelRef{Variant: "first", EngineOptions: "--model meta-llama/Llama-3.1-8B"},
		State:    pool.Asleep,
		Capacity: pool.PodCapacity{GPUs: 1},
	}
	got = Decide(Input{
		Memberships: []pool.Membership{full},
		Variants: []VariantDemand{{
			Model:  pool.ModelRef{Variant: "second", EngineOptions: "--model meta-llama/Llama-3.1-8B"},
			Parked: true,
		}},
		Now: now,
	}, tight)
	if len(got.Declined) != 1 {
		t.Fatalf("the second 8B does not fit: %+v", got.Declined)
	}
	if got.Declined[0].Permanent {
		t.Fatalf("a full Pod resolves when something is evicted; it is not permanent: %+v", got.Declined[0])
	}
}

// asGroup makes a membership describe a multi-Pod warm unit.
func asGroup(m pool.Membership, pods, gpusPerPod int) pool.Membership {
	m.Capacity.PodsPerGroup = pods
	m.Capacity.GPUs = pods * gpusPerPod
	return m
}

// A group is created with a fixed size and cannot be resized -- `size` is the
// engine's shape, not a scaling knob. So an engine whose ranks are laid out
// across a different number of Pods cannot start here even when the device
// totals agree exactly.
func TestAModelSpanningADifferentNumberOfPodsIsDeclined(t *testing.T) {
	c := cfg()
	c.PodMemoryBytes = 400 << 30
	c.SleepMinSize = 0

	// 16 devices over FOUR Pods, offered a group of TWO holding 8 each.
	// Same sixteen GPUs, different engine.
	across4 := VariantDemand{
		Model: pool.ModelRef{
			Namespace:     "workload",
			Variant:       "wide",
			EngineOptions: "--model Qwen/Qwen3-0.6B --tensor-parallel-size 16 --nnodes 4",
		},
		Parked: true,
	}

	got := Decide(Input{
		Memberships: []pool.Membership{asGroup(resident("leader", "", pool.Absent), 2, 8)},
		Variants:    []VariantDemand{across4},
		Now:         now,
	}, c)

	if len(got.Admit) != 0 {
		t.Fatalf("a 4-Pod engine must not be admitted into a 2-Pod group: %+v", got.Admit)
	}
	if len(got.Declined) != 1 || !strings.Contains(got.Declined[0].Reason, "Pod") {
		t.Fatalf("and the reason must name the layout: %+v", got.Declined)
	}
	if !got.Declined[0].Permanent {
		t.Error("a group cannot be resized, so the decline is permanent")
	}
}

// The positive half: the same engine fits the group built for its shape.
func TestAModelIsAdmittedIntoAGroupOfItsOwnShape(t *testing.T) {
	c := cfg()
	c.PodMemoryBytes = 400 << 30
	c.SleepMinSize = 0

	across2 := VariantDemand{
		Model: pool.ModelRef{
			Namespace:     "workload",
			Variant:       "wide",
			EngineOptions: "--model Qwen/Qwen3-0.6B --tensor-parallel-size 16 --nnodes 2",
		},
		Parked: true,
	}

	got := Decide(Input{
		Memberships: []pool.Membership{asGroup(resident("leader", "", pool.Absent), 2, 8)},
		Variants:    []VariantDemand{across2},
		Now:         now,
	}, c)

	if len(got.Admit) != 1 {
		t.Fatalf("a 2-Pod engine belongs in a 2-Pod group: admit=%+v declined=%+v",
			got.Admit, got.Declined)
	}
}

// A single-Pod engine has no business in a group either: it would occupy a warm
// unit whose other Pods it cannot use, and the group was built for an engine
// that spans them.
func TestASinglePodModelIsDeclinedByAGroup(t *testing.T) {
	c := cfg()
	c.PodMemoryBytes = 400 << 30
	c.SleepMinSize = 0

	single := VariantDemand{
		Model: pool.ModelRef{
			Namespace:     "workload",
			Variant:       "small",
			EngineOptions: "--model Qwen/Qwen3-0.6B",
		},
		Parked: true,
	}

	got := Decide(Input{
		Memberships: []pool.Membership{asGroup(resident("leader", "", pool.Absent), 2, 8)},
		Variants:    []VariantDemand{single},
		Now:         now,
	}, c)

	if len(got.Admit) != 0 {
		t.Fatalf("a single-Pod engine must not take a group: %+v", got.Admit)
	}
}
