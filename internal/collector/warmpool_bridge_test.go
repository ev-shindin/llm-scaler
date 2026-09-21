package collector

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// bridgePod is the pool Pod these tests lend out, and the variant it is lent to.
const (
	bridgePod     = "wva-warm-pool-0"
	bridgeVariant = "decode-variant"
	bridgeNS      = "test-ns"
)

// collectOne drives one KV-cache sample for podName through the collector with
// the given locator answers, and returns whatever rows came out.
//
// The same shape as TestBuildInstanceKey_VANameExtraction beside it: a fake
// source, a fake owner walk, and the real attribution in between.
func collectOne(t *testing.T, podName string, located map[string]string) []metricRow {
	t.Helper()

	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {Values: []source.MetricValue{{
					Labels: map[string]string{
						seriesModelLabel: "test-model",
						"pod":            podName,
						"instance":       "10.0.0.1:8000",
					},
					Value:     0.5,
					Timestamp: time.Now(),
				}}},
			}, nil
		},
	}

	c := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil, scalerLocator(located))
	c.SetBridgeResolver(decision.DefaultBridges)
	results, err := c.CollectReplicaMetrics(
		context.Background(), "test-model", bridgeNS,
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}
	out := make([]metricRow, 0, len(results))
	for _, r := range results {
		out = append(out, metricRow{pod: r.PodName, variant: r.VariantName, fromPool: r.FromWarmPool})
	}
	return out
}

type metricRow struct {
	pod      string
	variant  string
	fromPool bool
}

// A LENT pool Pod is attributed to the variant it is serving.
//
// This is the link the rest of the chain hangs from. A pool Pod's ownerReference
// walk reaches the POOL's workload, so it resolves to nothing this model drives
// and its metrics were dropped as unattributed -- while the Pod was serving that
// model's traffic. The load a bridge carries is the load of the variant that was
// too short to serve it itself, so dropping it makes demand read lowest exactly
// when the shortfall is worst.
func TestALentPoolPodIsAttributedToTheVariantItServes(t *testing.T) {
	t.Cleanup(decision.DefaultBridges.Reset)
	decision.PublishBridges(bridgeNS, map[string]string{bridgePod: bridgeVariant}, time.Now())

	// The owner walk finds nothing: the pool's workload is not this model's.
	rows := collectOne(t, bridgePod, nil)

	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the bridge attributed rather than dropped", rows)
	}
	if rows[0].variant != bridgeVariant {
		t.Errorf("variant = %q, want %q -- the variant the Pod is lent to", rows[0].variant, bridgeVariant)
	}
	if !rows[0].fromPool {
		t.Error("FromWarmPool is false: the analyzer needs it to count the demand and NOT the supply")
	}
}

// A Pod nothing has lent is still dropped. The bridge store is an addition to the
// attribution path, not a replacement for it: a Pod with no owner and no lending
// belongs to nothing this optimizer drives, and inventing a variant for it would
// add demand out of nowhere.
func TestAPodThatIsNotLentIsStillUnattributed(t *testing.T) {
	t.Cleanup(decision.DefaultBridges.Reset)
	decision.PublishBridges(bridgeNS, map[string]string{}, time.Now())

	if rows := collectOne(t, "some-other-pod", nil); len(rows) != 0 {
		t.Errorf("rows = %+v, want none for a Pod that is neither owned nor lent", rows)
	}
}

// A STALE lending is not acted on.
//
// The pool republishes every reconcile pass, so an old map means its reconciler
// has stopped -- and attributing a Pod on a lending that may since have ended
// would add demand for load nobody is carrying, for as long as the controller
// ran.
func TestAStaleLendingDoesNotAttributeAnything(t *testing.T) {
	t.Cleanup(decision.DefaultBridges.Reset)
	decision.PublishBridges(bridgeNS,
		map[string]string{bridgePod: bridgeVariant},
		time.Now().Add(-2*warmPoolLendingMaxAge))

	if rows := collectOne(t, bridgePod, nil); len(rows) != 0 {
		t.Errorf("rows = %+v, want none: the lending is older than %s", rows, warmPoolLendingMaxAge)
	}
}

// The LENDING wins over whatever the owner walk found.
//
// A pool has a scale target of its own -- it is scaled by KEDA like everything
// else -- so the walk can resolve a pool Pod to the POOL. That is a true answer
// to a different question: while the Pod is lent, its traffic belongs to the
// variant borrowing it, and attributing it to the pool would charge the pool's
// own scaler with a model's load.
func TestTheLendingBeatsTheOwnerWalk(t *testing.T) {
	t.Cleanup(decision.DefaultBridges.Reset)
	decision.PublishBridges(bridgeNS, map[string]string{bridgePod: bridgeVariant}, time.Now())

	rows := collectOne(t, bridgePod, map[string]string{bridgePod: "the-pools-own-scaler"})

	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	if rows[0].variant != bridgeVariant {
		t.Errorf("variant = %q, want %q: the Pod is lent, so its load is the borrower's",
			rows[0].variant, bridgeVariant)
	}
	if !rows[0].fromPool {
		t.Error("FromWarmPool is false, so the analyzer would count this Pod as the variant's own supply")
	}
}

// An ORDINARY replica is untouched: attributed by the owner walk, and NOT marked
// as coming from the pool -- which is what keeps its capacity in supply.
func TestAnOrdinaryReplicaIsNotMarkedAsABridge(t *testing.T) {
	t.Cleanup(decision.DefaultBridges.Reset)
	decision.PublishBridges(bridgeNS, map[string]string{bridgePod: bridgeVariant}, time.Now())

	rows := collectOne(t, "ordinary-replica-0", map[string]string{"ordinary-replica-0": "its-own-scaler"})

	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	if rows[0].variant != "its-own-scaler" {
		t.Errorf("variant = %q, want its-own-scaler", rows[0].variant)
	}
	if rows[0].fromPool {
		t.Error("an ordinary replica was marked as a bridge, so its capacity would be left out of supply")
	}
}
