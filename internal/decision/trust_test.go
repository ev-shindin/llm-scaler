package decision

import (
	"testing"
	"time"
)

// TestTrustStore_NoRecordIsTrusted pins the one direction this store fails in
// safely: a workload it has never seen. That is a workload nothing has collected
// yet -- new, or parked at zero -- and this store's silence about it is not
// evidence against it.
func TestTrustStore_NoRecordIsTrusted(t *testing.T) {
	s := NewTrustStore()
	if ok, reason := s.Trust("chat", "never-seen", time.Now(), time.Minute); !ok || reason != "" {
		t.Errorf("a workload with no record read as (%v, %q), want (true, \"\")", ok, reason)
	}
}

// TestTrustStore_TwoWaysToFail pins both shapes a broken metrics pipeline takes,
// because covering only the first is what made an earlier version go quiet
// exactly as the problem got worse.
func TestTrustStore_TwoWaysToFail(t *testing.T) {
	now := time.Now()

	t.Run("rows arrived and every replica was too old", func(t *testing.T) {
		s := NewTrustStore()
		s.Observe("chat", "vllm-wva", "m", true, "every replica's metrics are older than the unavailable threshold", now)
		ok, reason := s.Trust("chat", "vllm-wva", now, 5*time.Minute)
		if ok {
			t.Fatal("a fresh untrusted record did not block")
		}
		if reason == "" {
			t.Error("the reason must reach KEDA's error, which is where an operator sees it first")
		}
	})

	t.Run("no rows have arrived at all for longer than the limit", func(t *testing.T) {
		// Prometheus drops a series once nothing feeds it, so the rows stop
		// arriving entirely and there is no stale value left to inspect -- only
		// an absence, which no freshness check can see.
		s := NewTrustStore()
		s.Observe("chat", "vllm-wva", "m", false, "", now.Add(-9*time.Minute))
		ok, reason := s.Trust("chat", "vllm-wva", now, 5*time.Minute)
		if ok {
			t.Fatal("a workload unobserved for 9 minutes was still trusted")
		}
		if reason == "" {
			t.Error("the reason must say the workload has not been collected")
		}
	})

	t.Run("a recent observation is trusted", func(t *testing.T) {
		s := NewTrustStore()
		s.Observe("chat", "vllm-wva", "m", false, "", now.Add(-30*time.Second))
		if ok, reason := s.Trust("chat", "vllm-wva", now, 5*time.Minute); !ok {
			t.Errorf("a workload seen 30s ago was blocked: %q", reason)
		}
	})
}

// TestTrustStore_UntrustedDoesNotExpire pins the deliberate inversion of the
// earlier rule. An untrusted record is not timed out into a trusted one: "I have
// not collected this workload in five minutes" IS the abstain condition, not
// something to wait out. A wedged collector holds every workload it had seen,
// which spec.fallback turns into a hold or a raise rather than a drop.
func TestTrustStore_UntrustedDoesNotExpire(t *testing.T) {
	s := NewTrustStore()
	now := time.Now()
	s.Observe("chat", "vllm-wva", "m", true, "stopped", now.Add(-2*time.Hour))

	if ok, _ := s.Trust("chat", "vllm-wva", now, 5*time.Minute); ok {
		t.Error("a two-hour-old untrusted record expired into trusted")
	}
}

// TestTrustStore_RecoveryClears pins that the collector republishing every pass
// is enough to clear a verdict, so nothing has to remember to delete one.
func TestTrustStore_RecoveryClears(t *testing.T) {
	s := NewTrustStore()
	now := time.Now()

	s.Observe("chat", "vllm-wva", "m", true, "stopped", now)
	if ok, _ := s.Trust("chat", "vllm-wva", now, 5*time.Minute); ok {
		t.Fatal("setup: expected untrusted")
	}
	s.Observe("chat", "vllm-wva", "m", false, "", now)
	if ok, reason := s.Trust("chat", "vllm-wva", now, 5*time.Minute); !ok || reason != "" {
		t.Errorf("after recovery got (%v, %q), want (true, \"\")", ok, reason)
	}
}

// TestTrustStore_UnanswerableFor pins the model-level query the collector uses
// for the blocked-reason metric, including the case the rows in hand cannot
// show: a workload that produced nothing this pass.
func TestTrustStore_UnanswerableFor(t *testing.T) {
	s := NewTrustStore()
	now := time.Now()

	s.Observe("chat", "healthy-wva", "m1", false, "", now)
	s.Observe("chat", "stopped-wva", "m1", true, "stopped", now)
	s.Observe("chat", "vanished-wva", "m1", false, "", now.Add(-9*time.Minute))
	s.Observe("chat", "other-model-wva", "m2", true, "stopped", now)
	s.Observe("elsewhere", "other-ns-wva", "m1", true, "stopped", now)

	got := s.UnanswerableFor("chat", "m1", now, 5*time.Minute)
	want := map[string]bool{"stopped-wva": true, "vanished-wva": true}
	if len(got) != len(want) {
		t.Fatalf("UnanswerableFor returned %v, want exactly %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("UnanswerableFor returned %q, which belongs to another model or namespace", name)
		}
	}
}

// TestTrustStore_ReasonDroppedWhenTrusted pins that a reason handed in alongside
// trusted cannot leave a misleading explanation attached to a healthy workload.
func TestTrustStore_ReasonDroppedWhenTrusted(t *testing.T) {
	s := NewTrustStore()
	now := time.Now()
	s.Observe("chat", "vllm-wva", "m", false, "leftover", now)
	if _, reason := s.Trust("chat", "vllm-wva", now, time.Minute); reason != "" {
		t.Errorf("reason is %q on a trusted workload, want empty", reason)
	}
}
