package decision

import (
	"testing"
	"time"
)

// bigModel is the variant these tests move around; its name carries no meaning
// beyond being the one published.
const bigModel = "big"

// The store answers for the pool that was asked about, not for its namespace.
//
// A namespace may hold several pools, each with its own GPUs and its own
// resident set. Keyed by namespace alone, one pool's intent would move another
// pool's models -- and the models it moved would be ones the optimizer never
// looked at when it formed the intent.
func TestAnIntentBelongsToOnePool(t *testing.T) {
	s := NewAwakeStore()
	now := time.Now()
	s.Publish("tenant", "h100-pool", bigModel, now)

	if got, ok := s.Get("tenant", "h100-pool", time.Minute, now); !ok || got != bigModel {
		t.Errorf("Get(h100-pool) = %q, %v; want big, true", got, ok)
	}
	if got, ok := s.Get("tenant", "a100-pool", time.Minute, now); ok {
		t.Errorf("Get(a100-pool) = %q, %v; want no intent -- nothing was published for it", got, ok)
	}
	if got, ok := s.Get("other", "h100-pool", time.Minute, now); ok {
		t.Errorf("Get(other/h100-pool) = %q, %v; want no intent across namespaces", got, ok)
	}
}

// An EMPTY variant clears the intent rather than recording an empty one.
//
// "The optimizer has no opinion" and "nothing should be awake" are different
// instructions and only the first is expressible today. A cleared intent leaves
// the pool to its ordinary demand-led rules, which is what every pool did before
// this existed; a recorded empty one would read as an instruction to sleep
// whatever is serving.
func TestAnEmptyVariantClearsTheIntent(t *testing.T) {
	s := NewAwakeStore()
	now := time.Now()
	s.Publish("tenant", "pool", bigModel, now)
	s.Publish("tenant", "pool", "", now.Add(time.Second))

	if got, ok := s.Get("tenant", "pool", time.Minute, now.Add(time.Second)); ok {
		t.Errorf("Get after clearing = %q, %v; want no intent", got, ok)
	}
}

// Clearing a pool that never had an intent is not an error, and does not
// disturb the other pools in the namespace.
func TestClearingAnUnknownPoolLeavesTheRestAlone(t *testing.T) {
	s := NewAwakeStore()
	now := time.Now()
	s.Publish("tenant", "kept", bigModel, now)
	s.Publish("tenant", "never-had-one", "", now)
	s.Publish("empty-namespace", "pool", "", now)

	if got, ok := s.Get("tenant", "kept", time.Minute, now); !ok || got != bigModel {
		t.Errorf("Get(kept) = %q, %v; want big, true", got, ok)
	}
}

// A STALE intent is not acted on.
//
// An intent is re-derived every cycle, so one older than the caller's window
// means the optimizer stopped publishing. Acting on it would hold a retained
// pool's GPUs on a judgement about traffic that has since moved -- and hold it
// for as long as the controller ran, because nothing would ever replace it.
func TestAStaleIntentIsNotReturned(t *testing.T) {
	s := NewAwakeStore()
	now := time.Now()
	s.Publish("tenant", "pool", bigModel, now.Add(-10*time.Minute))

	if got, ok := s.Get("tenant", "pool", 5*time.Minute, now); ok {
		t.Errorf("Get(stale) = %q, %v; want none: the optimizer has stopped publishing", got, ok)
	}
	// The caller decides what stale means. With no window, an old intent still
	// answers -- which is what a test or a one-shot caller wants.
	if got, ok := s.Get("tenant", "pool", 0, now); !ok || got != bigModel {
		t.Errorf("Get(no window) = %q, %v; want big, true", got, ok)
	}
}

// A newer intent replaces an older one for the same pool, rather than being
// ignored or accumulating beside it.
func TestTheLatestIntentWins(t *testing.T) {
	s := NewAwakeStore()
	now := time.Now()
	s.Publish("tenant", "pool", bigModel, now)
	s.Publish("tenant", "pool", "small", now.Add(time.Second))

	if got, ok := s.Get("tenant", "pool", time.Minute, now.Add(time.Second)); !ok || got != "small" {
		t.Errorf("Get = %q, %v; want small, true", got, ok)
	}
}

// Reset drops everything. It is not only for tests: a controller that has lost
// its leadership must not go on acting on what it decided while it held it.
func TestResetDropsEveryIntent(t *testing.T) {
	s := NewAwakeStore()
	now := time.Now()
	s.Publish("a", "pool", "one", now)
	s.Publish("b", "pool", "two", now)
	s.Reset()

	for _, ns := range []string{"a", "b"} {
		if got, ok := s.Get(ns, "pool", time.Minute, now); ok {
			t.Errorf("Get(%s) after Reset = %q, %v; want none", ns, got, ok)
		}
	}
	// Still usable afterwards, rather than left with a nil map.
	s.Publish("a", "pool", "three", now)
	if got, ok := s.Get("a", "pool", time.Minute, now); !ok || got != "three" {
		t.Errorf("Get after Reset and Publish = %q, %v; want three, true", got, ok)
	}
}

// The package-level helpers reach the same store the controller wires up.
func TestThePackageHelpersUseTheDefaultStore(t *testing.T) {
	t.Cleanup(DefaultAwake.Reset)
	now := time.Now()
	PublishAwake("tenant", "pool", bigModel, now)

	if got, ok := Awake("tenant", "pool", time.Minute, now); !ok || got != bigModel {
		t.Errorf("Awake = %q, %v; want big, true", got, ok)
	}
}
