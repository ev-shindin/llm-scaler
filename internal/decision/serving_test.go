package decision

import (
	"testing"
	"time"
)

// NOT KNOWN and ZERO are different answers, and the warm pool treats them
// oppositely: no answer falls back to the Ready count, zero keeps the bridge.
// Collapsing them would hand a Pod back to a variant whose replicas are
// demonstrably not serving.
func TestServingSeparatesUnknownFromZero(t *testing.T) {
	s := NewServingStore()
	now := time.Now()

	if _, known := s.Get("tenant", "never-seen", time.Minute, now); known {
		t.Error("a target nobody has reported on must read as unknown")
	}

	s.Publish("tenant", "gone", 0, now)
	count, known := s.Get("tenant", "gone", time.Minute, now)
	if !known {
		t.Fatal("a published zero must read as known")
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

// A stale count is refused rather than returned. It means the collector stopped,
// and a bridge returned on a count from ten minutes ago is returned on evidence
// about a fleet that has since changed.
func TestServingRefusesAStaleCount(t *testing.T) {
	s := NewServingStore()
	now := time.Now()
	s.Publish("tenant", "old", 3, now.Add(-10*time.Minute))

	if _, known := s.Get("tenant", "old", time.Minute, now); known {
		t.Error("a count older than maxAge must read as unknown")
	}
	if count, known := s.Get("tenant", "old", time.Hour, now); !known || count != 3 {
		t.Errorf("within maxAge: (%d, %v), want (3, true)", count, known)
	}
}

// Counts are per namespace AND target: two namespaces may run variants of the
// same name, and one must not answer for the other.
func TestServingIsScopedToItsNamespace(t *testing.T) {
	s := NewServingStore()
	now := time.Now()
	s.Publish("a", "decode", 2, now)
	s.Publish("b", "decode", 5, now)

	if c, _ := s.Get("a", "decode", time.Minute, now); c != 2 {
		t.Errorf("namespace a = %d, want 2", c)
	}
	if c, _ := s.Get("b", "decode", time.Minute, now); c != 5 {
		t.Errorf("namespace b = %d, want 5", c)
	}
}
