package collector

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
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
			// The HARD action keys on the five-minute band, not the one-minute
			// one. "stale" is a scrape that is LATE: the soft exclusions
			// elsewhere already drop these replicas from the averages, and
			// freezing a workload for lateness would fire on a healthy fleet
			// behind a slow Prometheus -- systemically, every replica at once,
			// which is exactly the all-replicas condition this looks for.
			name:        "every replica merely stale keeps WVA answering",
			rows:        []domain.ReplicaMetrics{row("vllm", "stale", true), row("vllm", "stale", true)},
			wantTrusted: true,
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
			// The one blocking case: every replica past five minutes, which is
			// a scrape that has stopped rather than one running behind.
			name:        "every replica unavailable is the one blocking case",
			rows:        []domain.ReplicaMetrics{row("vllm", "unavailable", true), row("vllm", "unavailable", true)},
			wantTrusted: false,
			wantVerdict: true,
		},
		{
			// A replica that is merely late still counts as a view of the
			// workload, so it rescues the target from the abstain even though
			// the soft exclusions will drop it from the averages.
			name:        "a mix of stale and unavailable keeps WVA answering",
			rows:        []domain.ReplicaMetrics{row("vllm", "stale", true), row("vllm", "unavailable", true)},
			wantTrusted: true,
			wantVerdict: true,
		},
		{
			// "missing" is the one status that is not an age. It means the
			// metric was never scraped, which is also what a first collection
			// looks like, so it must not read as too old.
			name:        "missing is not stale",
			rows:        []domain.ReplicaMetrics{row("vllm", "missing", true)},
			wantTrusted: true,
			wantVerdict: true,
		},
		{
			name:        "metadata absent entirely is not stale",
			rows:        []domain.ReplicaMetrics{{Namespace: "chat", VariantName: "vllm"}},
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
			publishTrustVerdicts(context.Background(), store, "chat", "test-model", tc.rows, now)

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
	publishTrustVerdicts(context.Background(), store, "chat", "test-model", []domain.ReplicaMetrics{
		row("wedged", "unavailable", true),
		row("healthy", "fresh", true),
	}, now)

	if ok, _ := store.Trust("chat", "wedged", now, time.Minute); ok {
		t.Error("the all-stale target was trusted")
	}
	if ok, reason := store.Trust("chat", "healthy", now, time.Minute); !ok {
		t.Errorf("the healthy target was blocked by its neighbour: %q", reason)
	}
}

// TestPublishTrustVerdicts_BlockedReason pins that the abstain is visible on
// wva_model_scaling_blocked and not only in a log line.
//
// WVA erroring at KEDA shows up on the HPA as ScalingActive=False, which says a
// scaler failed but not which guard fired. Without the reason an operator sees a
// fleet that has stopped moving and nothing on the dashboard explaining it.
func TestPublishTrustVerdicts_BlockedReason(t *testing.T) {
	now := time.Now()
	reg := prometheus.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}
	store := decision.NewTrustStore()

	count := func() int {
		n, err := testutil.GatherAndCount(reg, constants.WVAModelScalingBlocked)
		if err != nil {
			t.Fatalf("GatherAndCount: %v", err)
		}
		return n
	}

	publishTrustVerdicts(context.Background(), store, "chat", "test-model",
		[]domain.ReplicaMetrics{row("vllm", "unavailable", true)}, now)
	if got := count(); got != 1 {
		t.Errorf("%s has %d series after an untrusted pass, want 1",
			constants.WVAModelScalingBlocked, got)
	}

	// Recovery clears it. The producer owns exactly this reason and deletes it
	// when it stops holding, the same contract the other two producers follow.
	publishTrustVerdicts(context.Background(), store, "chat", "test-model",
		[]domain.ReplicaMetrics{row("vllm", "fresh", true)}, now)
	if got := count(); got != 0 {
		t.Errorf("%s has %d series after recovery, want 0",
			constants.WVAModelScalingBlocked, got)
	}

	// And a model whose rows vanish entirely clears it as well. The trust
	// verdict expires on its own after trustTTL, so a gauge left at 1 here
	// would outlive the condition it reports and sit blocked forever.
	publishTrustVerdicts(context.Background(), store, "chat", "test-model",
		[]domain.ReplicaMetrics{row("vllm", "unavailable", true)}, now)
	if got := count(); got != 1 {
		t.Fatalf("%s has %d series, want 1 before the vanish", constants.WVAModelScalingBlocked, got)
	}
	publishTrustVerdicts(context.Background(), store, "chat", "test-model", nil, now)
	if got := count(); got != 0 {
		t.Errorf("%s has %d series after the model's rows vanished, want 0 — "+
			"the metric must not outlive the verdict, which expires on its own",
			constants.WVAModelScalingBlocked, got)
	}
}
