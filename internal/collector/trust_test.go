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

// TestPublishTrustVerdicts pins the exact shape of the only row-based condition
// that makes WVA decline to answer KEDA. The cases that must NOT trip it matter
// more than the one that must: each is a normal operating state, and declining
// on any of them would freeze a workload that is working.
func TestPublishTrustVerdicts(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name        string
		rows        []domain.ReplicaMetrics
		wantTrusted bool
	}{
		{
			// The HARD action keys on the five-minute band, not the one-minute
			// one. "stale" is a scrape that is LATE: the soft exclusions
			// elsewhere already drop these replicas from the averages, and
			// freezing a workload for lateness would fire on a healthy fleet
			// behind a slow Prometheus -- every replica at once, since scrape
			// lag is systemic.
			name:        "every replica merely stale keeps WVA answering",
			rows:        []domain.ReplicaMetrics{row("vllm-wva", "stale", true), row("vllm-wva", "stale", true)},
			wantTrusted: true,
		},
		{
			name: "one fresh replica is enough to keep answering",
			rows: []domain.ReplicaMetrics{
				row("vllm-wva", "unavailable", true), row("vllm-wva", "fresh", true),
			},
			wantTrusted: true,
		},
		{
			// The shape of every scale-up. Freezing here would stall a cold
			// start at the count it is trying to grow from.
			name:        "not-Ready replicas are not a reason to stop answering",
			rows:        []domain.ReplicaMetrics{row("vllm-wva", "fresh", false), row("vllm-wva", "fresh", false)},
			wantTrusted: true,
		},
		{
			name:        "every replica unavailable is the one blocking case",
			rows:        []domain.ReplicaMetrics{row("vllm-wva", "unavailable", true), row("vllm-wva", "unavailable", true)},
			wantTrusted: false,
		},
		{
			// "missing" is the one status that is not an age. It means the
			// metric was never scraped, which is also what a first collection
			// looks like, so it must not read as too old.
			name:        "missing is not too old",
			rows:        []domain.ReplicaMetrics{row("vllm-wva", "missing", true)},
			wantTrusted: true,
		},
		{
			name:        "metadata absent entirely is not too old",
			rows:        []domain.ReplicaMetrics{{Namespace: "chat", VariantName: "vllm-wva"}},
			wantTrusted: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := decision.NewTrustStore()
			publishTrustVerdicts(context.Background(), store, "chat", "test-model", tc.rows, now)

			ok, reason := store.Trust("chat", "vllm-wva", now, staleObservationLimit)
			if ok != tc.wantTrusted {
				t.Errorf("trusted is %v, want %v (reason %q)", ok, tc.wantTrusted, reason)
			}
		})
	}
}

// TestPublishTrustVerdicts_UnobservedWorkloadGoesUntrusted pins the second of
// the two ways a metrics pipeline breaks, and the one no row can show.
//
// After a scrape has been dead long enough, Prometheus drops the series and the
// workload stops appearing in the rows entirely. A verdict computed only from
// the rows in hand goes quiet exactly then — which is when the problem is worst.
func TestPublishTrustVerdicts_UnobservedWorkloadGoesUntrusted(t *testing.T) {
	store := decision.NewTrustStore()
	start := time.Now()

	// Seen healthy...
	publishTrustVerdicts(context.Background(), store, "chat", "test-model",
		[]domain.ReplicaMetrics{row("vllm-wva", "fresh", true)}, start)
	if ok, _ := store.Trust("chat", "vllm-wva", start, staleObservationLimit); !ok {
		t.Fatal("setup: a freshly observed workload should be trusted")
	}

	// ...then its rows vanish. Later passes for the same model carry nothing
	// for it, exactly as they would if Prometheus had dropped the series.
	later := start.Add(staleObservationLimit + time.Minute)
	publishTrustVerdicts(context.Background(), store, "chat", "test-model", nil, later)

	ok, reason := store.Trust("chat", "vllm-wva", later, staleObservationLimit)
	if ok {
		t.Error("a workload unobserved past the limit was still trusted")
	}
	if reason == "" {
		t.Error("the reason must say the workload has not been collected")
	}
}

// TestPublishTrustVerdicts_PerTarget pins that the record is per workload, not
// per collection pass: one wedged workload must not silence a healthy one
// collected in the same namespace on the same cycle.
func TestPublishTrustVerdicts_PerTarget(t *testing.T) {
	now := time.Now()
	store := decision.NewTrustStore()
	publishTrustVerdicts(context.Background(), store, "chat", "test-model", []domain.ReplicaMetrics{
		row("wedged-wva", "unavailable", true),
		row("healthy-wva", "fresh", true),
	}, now)

	if ok, _ := store.Trust("chat", "wedged-wva", now, staleObservationLimit); ok {
		t.Error("the all-unavailable workload was trusted")
	}
	if ok, reason := store.Trust("chat", "healthy-wva", now, staleObservationLimit); !ok {
		t.Errorf("the healthy workload was blocked by its neighbour: %q", reason)
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
		[]domain.ReplicaMetrics{row("vllm-wva", "unavailable", true)}, now)
	if got := count(); got != 1 {
		t.Errorf("%s has %d series after an untrusted pass, want 1",
			constants.WVAModelScalingBlocked, got)
	}

	// Recovery clears it. The producer owns exactly this reason and deletes it
	// when it stops holding, the same contract the other two producers follow.
	publishTrustVerdicts(context.Background(), store, "chat", "test-model",
		[]domain.ReplicaMetrics{row("vllm-wva", "fresh", true)}, now)
	if got := count(); got != 0 {
		t.Errorf("%s has %d series after recovery, want 0",
			constants.WVAModelScalingBlocked, got)
	}

	// A workload whose rows vanish still reports blocked, because the reason is
	// asked of the STORE and not of this pass's rows. An earlier version asked
	// the rows, so the metric went silent at exactly the wrong moment.
	later := now.Add(staleObservationLimit + time.Minute)
	publishTrustVerdicts(context.Background(), store, "chat", "test-model", nil, later)
	if got := count(); got != 1 {
		t.Errorf("%s has %d series for a workload that stopped being collected, want 1",
			constants.WVAModelScalingBlocked, got)
	}
}
