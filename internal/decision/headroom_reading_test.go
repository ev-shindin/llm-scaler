package decision

import (
	"testing"
	"time"
)

// Reading is the three-valued answer the warm pool sizes itself by, and the
// version gate is what stops it growing on a snapshot that predates its own
// charge. Available remains the two-valued view for callers that only ask
// "may I, and how much".

func TestReadingTellsUnboundedFromUnknown(t *testing.T) {
	s := NewHeadroomStore()
	now := time.Now()

	if _, state := s.Reading("ns", "H100", 0, time.Minute, now); state != HeadroomUnknown {
		t.Errorf("an empty store reads %v, want unknown", state)
	}

	s.Publish(map[string]map[string]int{"other": {"H100": 4}}, 0, now)
	if _, state := s.Reading("ns", "H100", 0, time.Minute, now); state != HeadroomUnbounded {
		t.Errorf("a namespace no limiter names reads %v, want unbounded", state)
	}
	if free, state := s.Reading("other", "H100", 0, time.Minute, now); state != HeadroomBounded || free != 4 {
		t.Errorf("a named namespace reads (%d, %v), want (4, bounded)", free, state)
	}
	if free, state := s.Reading("other", "A100", 0, time.Minute, now); state != HeadroomBounded || free != 0 {
		t.Errorf("an unlisted accelerator in a named namespace reads (%d, %v), want (0, bounded)", free, state)
	}
}

// A snapshot built from a warm-pool figure older than the caller's own charge
// still credits the caller with what it has since taken. It is no answer.
func TestReadingRefusesASnapshotOlderThanTheCallersCharge(t *testing.T) {
	s := NewHeadroomStore()
	now := time.Now()
	s.Publish(map[string]map[string]int{"ns": {"H100": 1}}, 7, now)

	if _, state := s.Reading("ns", "H100", 8, time.Minute, now); state != HeadroomUnknown {
		t.Errorf("a snapshot at pools version 7 read for version 8 gives %v, want unknown", state)
	}
	if free, state := s.Reading("ns", "H100", 7, time.Minute, now); state != HeadroomBounded || free != 1 {
		t.Errorf("a snapshot at version 7 read for version 7 gives (%d, %v), want (1, bounded)", free, state)
	}
	if _, state := s.Reading("ns", "H100", 3, time.Minute, now); state != HeadroomBounded {
		t.Errorf("a newer snapshot serves an older ask: got %v, want bounded", state)
	}
	// The gate applies to the unbounded answer too: "nothing bounds you" from
	// before the charge is still a statement about the world before it.
	if _, state := s.Reading("elsewhere", "H100", 8, time.Minute, now); state != HeadroomUnknown {
		t.Errorf("an old unbounded snapshot read for a newer version gives %v, want unknown", state)
	}
}

func TestReadingHonoursMaxAge(t *testing.T) {
	s := NewHeadroomStore()
	now := time.Now()
	s.Publish(map[string]map[string]int{"ns": {"H100": 1}}, 0, now.Add(-10*time.Minute))

	if _, state := s.Reading("ns", "H100", 0, time.Minute, now); state != HeadroomUnknown {
		t.Errorf("a stale snapshot reads %v, want unknown", state)
	}
}

// The warm-pool figure's version moves only when the figure does, so a pool that
// republishes the same figure every pass does not keep raising the bar a
// snapshot has to clear.
func TestWarmPoolGPUsVersionMovesOnlyOnChange(t *testing.T) {
	ResetWarmPoolGPUs()
	t.Cleanup(ResetWarmPoolGPUs)

	v1 := PublishWarmPoolGPUs("ns", map[string]int{"H100": 1})
	v2 := PublishWarmPoolGPUs("ns", map[string]int{"H100": 1})
	if v2 != v1 {
		t.Errorf("republishing an unchanged figure moved the version %d -> %d", v1, v2)
	}
	v3 := PublishWarmPoolGPUs("ns", map[string]int{"H100": 2})
	if v3 <= v2 {
		t.Errorf("a changed figure must move the version, got %d after %d", v3, v2)
	}
	v4 := PublishWarmPoolGPUs("ns", nil)
	if v4 <= v3 {
		t.Errorf("a pool going away is a change, got %d after %d", v4, v3)
	}
	v5 := PublishWarmPoolGPUs("ns", nil)
	if v5 != v4 {
		t.Errorf("an absent pool staying absent is not a change, got %d after %d", v5, v4)
	}
	if _, got := WarmPoolGPUsWithVersion(); got != v5 {
		t.Errorf("the read-side version is %d, want %d", got, v5)
	}
}

// The bar a pool sets is its OWN namespace's last change. Another namespace's
// pool flapping moves the global counter, and must not make this pool wait for
// a snapshot newer than the one that already charged it.
func TestAPoolsBarIsItsOwnNamespacesLastChange(t *testing.T) {
	ResetWarmPoolGPUs()
	t.Cleanup(ResetWarmPoolGPUs)

	mine := PublishWarmPoolGPUs("mine", map[string]int{"H100": 1})
	_, global := WarmPoolGPUsWithVersion()
	if mine != global {
		t.Fatalf("the first change sets the bar at the counter: %d vs %d", mine, global)
	}
	PublishWarmPoolGPUs("other", map[string]int{"H100": 1})
	PublishWarmPoolGPUs("other", map[string]int{"H100": 2})
	again := PublishWarmPoolGPUs("mine", map[string]int{"H100": 1}) // unchanged
	if again != mine {
		t.Errorf("another namespace's changes raised this pool's bar from %d to %d", mine, again)
	}
	_, global = WarmPoolGPUsWithVersion()
	if global <= mine {
		t.Errorf("the counter must have moved for the other namespace: %d after %d", global, mine)
	}

	// A snapshot built from the counter as it stood after this pool's change
	// but before the other namespace's serves this pool.
	s := NewHeadroomStore()
	s.Publish(map[string]map[string]int{"mine": {"H100": 1}}, mine, time.Now())
	if _, state := s.Reading("mine", "H100", again, time.Minute, time.Now()); state != HeadroomBounded {
		t.Errorf("the snapshot that charged this pool reads %v, want bounded", state)
	}
}
