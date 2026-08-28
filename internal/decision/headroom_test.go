package decision

import (
	"testing"
	"time"
)

// Headroom exists so a warm pool stops asking for Pods the namespace cannot
// afford. The distinction that matters is between "nothing left" and "no answer":
// the first caps growth, the second must not, or a pool on a cluster with no
// limiter would never grow at all.

func TestNoSnapshotIsNoAnswerRatherThanZero(t *testing.T) {
	s := NewHeadroomStore()

	if _, known := s.Available("ns", "H100", time.Minute, time.Now()); known {
		t.Error("an empty store must answer 'unknown', not zero: a pool would never grow")
	}
}

func TestAnUnconstrainedNamespaceIsNoAnswer(t *testing.T) {
	s := NewHeadroomStore()
	s.Publish(map[string]map[string]int{"other": {"H100": 4}}, time.Now())

	if _, known := s.Available("ns", "H100", time.Minute, time.Now()); known {
		t.Error("a namespace no limiter bounds must answer 'unknown', not zero")
	}
}

// A namespace-scoped quota is a closed allowlist, so an accelerator it does not
// name is denied outright. That IS an answer, and it is zero.
func TestAnUnlistedAcceleratorInACappedNamespaceIsZero(t *testing.T) {
	s := NewHeadroomStore()
	s.Publish(map[string]map[string]int{"ns": {"H100": 4}}, time.Now())

	free, known := s.Available("ns", "A100", time.Minute, time.Now())
	if !known {
		t.Fatal("a capped namespace must answer for every accelerator")
	}
	if free != 0 {
		t.Errorf("an accelerator the allowlist omits has %d free, want 0", free)
	}
}

func TestFreeGPUsAreReported(t *testing.T) {
	s := NewHeadroomStore()
	s.Publish(map[string]map[string]int{"ns": {"H100": 3}}, time.Now())

	free, known := s.Available("ns", "H100", time.Minute, time.Now())
	if !known || free != 3 {
		t.Errorf("got (%d, %v), want (3, true)", free, known)
	}
}

// A stale reading is worse than none: the pool would size itself against an
// allowance that has since been spent.
func TestAStaleSnapshotIsNoAnswer(t *testing.T) {
	s := NewHeadroomStore()
	now := time.Now()
	s.Publish(map[string]map[string]int{"ns": {"H100": 3}}, now.Add(-10*time.Minute))

	if _, known := s.Available("ns", "H100", time.Minute, now); known {
		t.Error("a snapshot older than maxAge must answer 'unknown'")
	}
}

// Publishing replaces wholesale, so an allowance that shrank is not remembered
// as larger -- and a namespace that lost its cap stops being capped.
func TestPublishingReplacesTheWholeSnapshot(t *testing.T) {
	s := NewHeadroomStore()
	now := time.Now()
	s.Publish(map[string]map[string]int{"ns": {"H100": 8}}, now)
	s.Publish(map[string]map[string]int{"ns": {"H100": 1}}, now)

	if free, _ := s.Available("ns", "H100", time.Minute, now); free != 1 {
		t.Errorf("after shrinking, got %d free, want 1", free)
	}

	s.Publish(map[string]map[string]int{}, now)
	if _, known := s.Available("ns", "H100", time.Minute, now); known {
		t.Error("a namespace no longer capped must answer 'unknown'")
	}
}

// The snapshot handed out must not alias the caller's map, or a later mutation
// upstream would silently change what every reader sees.
func TestTheSnapshotDoesNotAliasTheCaller(t *testing.T) {
	s := NewHeadroomStore()
	live := map[string]map[string]int{"ns": {"H100": 4}}
	s.Publish(live, time.Now())

	live["ns"]["H100"] = 0

	if free, _ := s.Available("ns", "H100", time.Minute, time.Now()); free != 4 {
		t.Errorf("mutating the caller's map changed the store: got %d, want 4", free)
	}
}
