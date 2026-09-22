package collector

import (
	"context"
	"testing"
	"time"

	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// An FMA launcher that resolved through its pairing is reported to the caller
// as paired whether or not its row survives: the loop this came from counted
// the pairing before the LWS-worker skip, and the once-per-cycle "attributed
// through their dual-pods pairing" line is how an operator sees the hop
// working. A launcher that is also a worker is dropped as a row and still
// counted.
func TestAttributeInstance_PairedLauncherIsReportedEvenWhenDroppedAsWorker(t *testing.T) {
	ctx := context.Background()
	newCollector := func(labels map[string]string) *ReplicaMetricsCollector {
		loc := &mockLocator{
			locateFunc: func(context.Context, string, string) (*locator.ManagedScaler, error) {
				return &locator.ManagedScaler{Namespace: "ns", Name: "decode"}, nil
			},
			getPodLabelsFunc: func(context.Context, string, string) map[string]string { return labels },
		}
		// No API reader: the readiness and uptime reads fail open, as they do
		// on a listing that cannot be made.
		return NewReplicaMetricsCollector(nil, nil, nil, nil, loc)
	}
	data := func() *podMetricData {
		return &podMetricData{podName: "launcher-0", vaName: "decode", hasKv: true, kvUsage: 0.5}
	}
	freshness := map[string]map[string]int{}

	t.Run("a paired launcher that is an LWS leader is a row and is reported paired", func(t *testing.T) {
		c := newCollector(map[string]string{
			constants.ComponentLabelKey: constants.LauncherComponent,
			lwsv1.WorkerIndexLabelKey:   "0",
		})
		metric, pairedAs, ok := c.attributeInstance(ctx, "m", "ns", "launcher-0:8000", data(), time.Now(), nil, freshness)
		if !ok {
			t.Fatalf("a leader launcher must produce a row")
		}
		if metric.VariantName != "decode" {
			t.Errorf("VariantName: got %q, want decode", metric.VariantName)
		}
		if pairedAs != "launcher-0 -> decode" {
			t.Errorf("pairedAs: got %q, want %q", pairedAs, "launcher-0 -> decode")
		}
	})

	t.Run("a paired launcher that is an LWS worker is no row and is still reported paired", func(t *testing.T) {
		c := newCollector(map[string]string{
			constants.ComponentLabelKey: constants.LauncherComponent,
			lwsv1.WorkerIndexLabelKey:   "1",
		})
		_, pairedAs, ok := c.attributeInstance(ctx, "m", "ns", "launcher-0:8000", data(), time.Now(), nil, freshness)
		if ok {
			t.Fatalf("a worker must not produce a row")
		}
		if pairedAs != "launcher-0 -> decode" {
			t.Errorf("pairedAs: got %q, want %q (the pairing was counted before the worker skip)", pairedAs, "launcher-0 -> decode")
		}
	})

	t.Run("an ordinary pod is a row and is not paired", func(t *testing.T) {
		c := newCollector(map[string]string{})
		_, pairedAs, ok := c.attributeInstance(ctx, "m", "ns", "launcher-0:8000", data(), time.Now(), nil, freshness)
		if !ok || pairedAs != "" {
			t.Errorf("got ok=%v pairedAs=%q, want ok=true pairedAs=\"\"", ok, pairedAs)
		}
	})
}
