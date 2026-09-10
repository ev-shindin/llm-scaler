package collector

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

func row(variant, status string, ready bool) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{
		Namespace:   "chat",
		VariantName: variant,
		Ready:       ready,
		Metadata:    &domain.ReplicaMetricsMetadata{FreshnessStatus: status},
	}
}

// TestPublishTrustVerdicts pins the exact shape of the only condition that makes
// WVA decline to answer KEDA. The cases that must NOT trip it matter more than
// the one that must: each of them is a normal operating state, and declining on
// any of them would freeze a workload that is working.
func TestPublishTrustVerdicts(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name        string
		rows        []domain.ReplicaMetrics
		wantTrusted bool
		wantVerdict bool // whether a verdict is published at all
	}{
		{
			name:        "every replica stale is the one blocking case",
			rows:        []domain.ReplicaMetrics{row("vllm", "stale", true), row("vllm", "stale", true)},
			wantTrusted: false,
			wantVerdict: true,
		},
		{
			name: "one fresh replica is enough to keep answering",
			rows: []domain.ReplicaMetrics{
				row("vllm", "stale", true), row("vllm", "stale", true), row("vllm", "fresh", true),
			},
			wantTrusted: true,
			wantVerdict: true,
		},
		{
			// The shape of every scale-up. Freezing here would stall a cold
			// start at the count it is trying to grow from.
			name:        "not-Ready replicas are not a reason to stop answering",
			rows:        []domain.ReplicaMetrics{row("vllm", "fresh", false), row("vllm", "fresh", false)},
			wantTrusted: true,
			wantVerdict: true,
		},
		{
			// "missing"/"unavailable" mean the metric was never there, which is
			// a cold scrape, not a broken one.
			name:        "missing is not stale",
			rows:        []domain.ReplicaMetrics{row("vllm", "missing", true)},
			wantTrusted: true,
			wantVerdict: true,
		},
		{
			name:        "no rows publishes no verdict at all",
			rows:        nil,
			wantVerdict: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := decision.NewTrustStore()
			prev := decision.DefaultTrust
			decision.DefaultTrust = store
			t.Cleanup(func() { decision.DefaultTrust = prev })

			publishTrustVerdicts(context.Background(), tc.rows, now)

			ok, reason := store.Trust("chat", "vllm", now, time.Minute)
			if !tc.wantVerdict {
				if !ok {
					t.Fatalf("expected no verdict, but the target read as untrusted (%q)", reason)
				}
				return
			}
			if ok != tc.wantTrusted {
				t.Errorf("trusted is %v, want %v (reason %q)", ok, tc.wantTrusted, reason)
			}
		})
	}
}

// TestPublishTrustVerdicts_PerTarget pins that the verdict is per scale target,
// not per collection pass: one wedged workload must not silence a healthy one
// scraped in the same namespace on the same cycle.
func TestPublishTrustVerdicts_PerTarget(t *testing.T) {
	now := time.Now()
	store := decision.NewTrustStore()
	prev := decision.DefaultTrust
	decision.DefaultTrust = store
	t.Cleanup(func() { decision.DefaultTrust = prev })

	publishTrustVerdicts(context.Background(), []domain.ReplicaMetrics{
		row("wedged", "stale", true),
		row("healthy", "fresh", true),
	}, now)

	if ok, _ := store.Trust("chat", "wedged", now, time.Minute); ok {
		t.Error("the all-stale target was trusted")
	}
	if ok, reason := store.Trust("chat", "healthy", now, time.Minute); !ok {
		t.Errorf("the healthy target was blocked by its neighbour: %q", reason)
	}
}
