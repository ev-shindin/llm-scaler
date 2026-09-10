package decision

import (
	"testing"
	"time"
)

// TestTrustStore_SilenceIsTrust pins the direction this store fails in. Every
// way it can have nothing to say about a target -- never published, or published
// so long ago the verdict has expired -- must read as TRUSTED, because the
// consumer turns "untrusted" into a refusal to answer KEDA and a workload
// nothing has observed must not be frozen by a store with no opinion about it.
func TestTrustStore_SilenceIsTrust(t *testing.T) {
	s := NewTrustStore()
	now := time.Now()

	if ok, reason := s.Trust("chat", "never-seen", now, time.Minute); !ok || reason != "" {
		t.Errorf("a target with no verdict read as (%v, %q), want (true, \"\")", ok, reason)
	}

	s.Publish("chat", "wedged", false, "every replica's metrics are stale", now.Add(-10*time.Minute))
	if ok, _ := s.Trust("chat", "wedged", now, 5*time.Minute); !ok {
		t.Error("a verdict older than maxAge still blocked; an abandoned verdict must expire")
	}
	if ok, _ := s.Trust("chat", "wedged", now, 0); ok {
		t.Error("maxAge 0 should disable expiry, but the verdict was treated as expired")
	}
}

// TestTrustStore_UntrustedCarriesReason pins that a live untrusted verdict
// blocks and explains itself: the reason reaches KEDA's error message, which is
// where an operator sees it first.
func TestTrustStore_UntrustedCarriesReason(t *testing.T) {
	s := NewTrustStore()
	now := time.Now()

	s.Publish("chat", "vllm", false, "every replica's metrics are stale", now)
	ok, reason := s.Trust("chat", "vllm", now, 5*time.Minute)
	if ok {
		t.Fatal("a fresh untrusted verdict did not block")
	}
	if reason != "every replica's metrics are stale" {
		t.Errorf("reason is %q, want the published reason", reason)
	}

	// Recovery clears itself: the collector republishes every pass, so nothing
	// has to remember to delete the old verdict.
	s.Publish("chat", "vllm", true, "", now)
	if ok, reason := s.Trust("chat", "vllm", now, 5*time.Minute); !ok || reason != "" {
		t.Errorf("after recovery got (%v, %q), want (true, \"\")", ok, reason)
	}
}

// TestTrustStore_TrustedDropsReason pins that a reason handed in alongside
// trusted=true is discarded rather than stored, so a caller cannot leave a
// misleading explanation attached to a healthy target.
func TestTrustStore_TrustedDropsReason(t *testing.T) {
	s := NewTrustStore()
	now := time.Now()

	s.Publish("chat", "vllm", true, "leftover", now)
	if _, reason := s.Trust("chat", "vllm", now, time.Minute); reason != "" {
		t.Errorf("reason is %q on a trusted target, want empty", reason)
	}
}
