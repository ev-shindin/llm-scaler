package steadystate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/scalingpolicy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// The hold and the carry are wired into applySaturationDecisions, and the
// order there is load-bearing: the hold reads the published value BEFORE the
// deciding mark is written for this cycle, and the carry runs AFTER the
// no-decision fallback has resolved the running count. Pure-function specs
// cannot see either. This drives the real function, with a fake API server
// holding the scale target, through the cycles the benchmark showed, and reads
// what reached the decision store -- which is what KEDA reads.

const (
	wiringNS     = "wiring"
	wiringTarget = "qwen-decode"
)

// stickyConfig is a Config loaded the way the controller loads one, with the
// switch at its default (on) or turned off.
func stickyConfig(t *testing.T, on bool) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "PROMETHEUS_BASE_URL: \"https://prometheus:9090\"\n"
	if !on {
		body += "WVA_STICKY_SCALE_DOWN: \"false\"\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	cfg, err := config.Load(nil, path)
	require.NoError(t, err)
	require.Equal(t, on, cfg.StickyScaleDownEnabled())
	return cfg
}

// wiringDeployment is the scale target as the first incarnation, "fleet-1",
// running two replicas; a later incarnation is what a cycle READS (its UID),
// not a second object.
func wiringDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: wiringTarget, Namespace: wiringNS, UID: "fleet-1"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(2))},
	}
}

func wiringEngine(t *testing.T, objs ...client.Object) *Engine {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(s))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	// The apply pass records the saturation gauges; they have to exist.
	require.NoError(t, metrics.InitMetrics(prometheus.NewRegistry()))
	return &Engine{
		client:         c,
		Config:         stickyConfig(t, true),
		metricsEmitter: metrics.NewMetricsEmitter(),
		policies:       scalingpolicy.NewChangeReporter(),
	}
}

func wiringVA() *variant.VariantAutoscaling {
	return &variant.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: wiringTarget, Namespace: wiringNS},
		Spec: variant.VariantAutoscalingSpec{
			ModelID:        "Qwen/Qwen3-8B",
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: wiringTarget},
		},
	}
}

func wiringDecision(current, target int, demand float64) domain.VariantDecision {
	d := decisionFor(current, target, demand)
	d.VariantName = wiringTarget
	d.Namespace = wiringNS
	d.ModelID = "Qwen/Qwen3-8B"
	d.AcceleratorName = "NVIDIA-H100-80GB-HBM3"
	d.SetDecisionReason(d.Action, domain.DecisionReasonV2, "test")
	return d
}

// cycle runs one apply pass the way optimizeV2 would: the scale target's
// identity read for this cycle, then the decisions (or none).
func cycle(t *testing.T, e *Engine, uid types.UID, decisions ...domain.VariantDecision) int32 {
	t.Helper()
	e.scaleTargetUIDs = nil // as optimizeV2 resets it every cycle
	if uid != "" {
		e.noteScaleTargetUID(utils.GetNamespacedKey(wiringNS, wiringTarget), uid)
	}
	va := wiringVA()
	vaMap := map[string]*variant.VariantAutoscaling{utils.GetNamespacedKey(wiringNS, wiringTarget): va}
	e.applySaturationDecisions(context.Background(), decisions, vaMap, map[string]*domain.Allocation{})
	d, ok := decision.Get(wiringNS, wiringTarget)
	require.True(t, ok, "the apply pass publishes something every cycle")
	return d.DesiredReplicas
}

func TestStickyWiring_HoldThenCarryThenExternalPark(t *testing.T) {
	dep := wiringDeployment()
	e := wiringEngine(t, dep)

	// Cycle 1: the optimizer says 1 (demand under the boundary at one replica).
	require.EqualValues(t, 1, cycle(t, e, "fleet-1", wiringDecision(2, 1, 18770)))

	// Cycle 2: the demand noise says 2 again while the fleet is still at 2.
	// The hold keeps the published 1 -- this is the chatter the feature stops.
	require.EqualValues(t, 1, cycle(t, e, "fleet-1", wiringDecision(2, 2, 20676)))

	// Cycle 3: no decision (a scrape gap). The fallback reads the running
	// count (2) from the API server; the carry republishes the held 1 -- so the
	// carry ran AFTER the fallback resolved the count, or it would have had 0
	// to carry against and the running count would have been published.
	require.EqualValues(t, 1, cycle(t, e, "fleet-1"))

	// Cycle 4: the operator scales the Deployment to zero by hand; no metrics,
	// no decision. A READ zero is published as zero: the carry must not wake
	// what was just parked.
	dep.Spec.Replicas = ptr.To(int32(0))
	require.NoError(t, e.client.Update(context.Background(), dep))
	require.EqualValues(t, 0, cycle(t, e, "fleet-1"))
}

func TestStickyWiring_ANewIncarnationIsNotHeldToTheOldDescent(t *testing.T) {
	e := wiringEngine(t, wiringDeployment())
	require.EqualValues(t, 1, cycle(t, e, "fleet-1", wiringDecision(2, 1, 18770)))

	// The Deployment is deleted and re-created under the same name at 3
	// replicas, and the first decision for it says 2. Read this cycle under a
	// different UID, the old fleet's published 1 is not trusted: 2 stands.
	require.EqualValues(t, 2, cycle(t, e, "fleet-2", wiringDecision(3, 2, 30000)))
}

func TestStickyWiring_ABurstReleasesTheHold(t *testing.T) {
	e := wiringEngine(t, wiringDeployment())
	require.EqualValues(t, 1, cycle(t, e, "fleet-1", wiringDecision(2, 1, 18770)))
	// Demand at one replica reaches the scale-up threshold: the fresh target
	// stands, and the fleet is not held down.
	require.EqualValues(t, 2, cycle(t, e, "fleet-1", wiringDecision(2, 2, 24300)))
}

func TestStickyWiring_OffIsTheOldBehaviour(t *testing.T) {
	e := wiringEngine(t, wiringDeployment())
	e.Config = stickyConfig(t, false)
	require.EqualValues(t, 1, cycle(t, e, "fleet-1", wiringDecision(2, 1, 18770)))
	// The crept-up target is published as is: the chatter, unchanged.
	require.EqualValues(t, 2, cycle(t, e, "fleet-1", wiringDecision(2, 2, 20676)))
}
