package scaler

import (
	"context"
	"testing"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
)

// These specs cover the decision-freshness rule in honoursDecision. They live in
// package scaler (not scaler_test) because they drive the handler's clock
// directly rather than sleeping.

const (
	freshnessNS     = "chat"
	freshnessSO     = "chat-decode"
	freshnessTarget = "chat-decode-deploy"
)

// freshnessHandler builds a Handler over a ScaledObject pointing at a Deployment
// with the given spec.replicas, plus a controllable clock starting at t0.
func freshnessHandler(t *testing.T, store *decision.Store, targetReplicas int32, nowPtr *time.Time) *Handler {
	t.Helper()

	s := runtime.NewScheme()
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add keda scheme: %v", err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}

	objs := []client.Object{
		&kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{Namespace: freshnessNS, Name: freshnessSO},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &kedav1alpha1.ScaleTarget{
					APIVersion: "apps/v1", Kind: "Deployment", Name: freshnessTarget,
				},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: freshnessNS, Name: freshnessTarget},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(targetReplicas)},
		},
	}

	h := NewHandler(fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(), store, registry.New(0))
	h.now = func() time.Time { return *nowPtr }
	return h
}

func freshnessRef() *pb.ScaledObjectRef {
	return &pb.ScaledObjectRef{Namespace: freshnessNS, Name: freshnessSO}
}

// TestStaleActivationReleasesZeroParkedTarget is the regression this rule exists
// for. A variant that ran briefly leaves a positive decision behind; once the
// target is scaled to zero, that latched value used to hold isActive true
// forever, so the target was woken the moment KEDA asked, was never observed
// inactive, and no engine could detect demand on it.
func TestStaleActivationReleasesZeroParkedTarget(t *testing.T) {
	// The injected clock is read AFTER the store has been written, not before.
	// Set stamps the decision with the real time.Now(), so starting the clock
	// first leaves the test only the one second of slack that the Add below
	// adds on top of the TTL -- and any pause longer than that between the two
	// lines (a loaded machine, -race, packages building in parallel) leaves the
	// decision looking fresh and fails the assertion. Reproduced with a 1.5 s
	// sleep in that gap; it is why this test flaked in a full -race run.
	var now time.Time
	store := decision.NewStore()
	// The target is parked at zero, but a stale "2" is still in the store.
	h := freshnessHandler(t, store, 0, &now)
	store.Set(freshnessNS, freshnessTarget, 2)
	now = time.Now()

	active, err := h.isActive(context.Background(), freshnessRef())
	if err != nil {
		t.Fatalf("isActive: %v", err)
	}
	if !active {
		t.Fatal("a fresh positive decision must still hold the target active")
	}

	now = now.Add(activationTTL + time.Second)
	active, err = h.isActive(context.Background(), freshnessRef())
	if err != nil {
		t.Fatalf("isActive after TTL: %v", err)
	}
	if active {
		t.Fatal("a stale positive decision must not hold a zero-parked target active; " +
			"it should fall back to the target's own replica count")
	}
}

// TestStaleActivationOnRunningTargetIsHarmless: expiry falls back to reality, and
// for a target that is up, reality is "active". Publishers going quiet must never
// scale a running workload to zero.
func TestStaleActivationOnRunningTargetIsHarmless(t *testing.T) {
	now := time.Now()
	store := decision.NewStore()
	h := freshnessHandler(t, store, 3, &now)
	store.Set(freshnessNS, freshnessTarget, 3)

	now = now.Add(activationTTL + time.Second)
	active, err := h.isActive(context.Background(), freshnessRef())
	if err != nil {
		t.Fatalf("isActive: %v", err)
	}
	if !active {
		t.Fatal("a running target must stay active when its decision goes stale")
	}
}

// TestScaleToZeroDecisionNeverExpires pins the asymmetry. "This should be asleep"
// is the resting state; if it expired, a target still draining would flip back to
// active and flap against the scale-down already in flight.
func TestScaleToZeroDecisionNeverExpires(t *testing.T) {
	now := time.Now()
	store := decision.NewStore()
	// Still running (drain in progress) while the decision says zero.
	h := freshnessHandler(t, store, 2, &now)
	store.Set(freshnessNS, freshnessTarget, 0)

	for _, age := range []time.Duration{0, activationTTL + time.Second, 24 * time.Hour} {
		now = now.Add(age)
		active, err := h.isActive(context.Background(), freshnessRef())
		if err != nil {
			t.Fatalf("isActive at age %s: %v", age, err)
		}
		if active {
			t.Fatalf("a zero decision must not expire (age %s); the target would flap back up", age)
		}
	}
}

// TestFreshActivationWakesZeroParkedTarget is the scale-from-zero path itself:
// the engine publishes 1 for a target sitting at zero and KEDA must see active.
func TestFreshActivationWakesZeroParkedTarget(t *testing.T) {
	now := time.Now()
	store := decision.NewStore()
	h := freshnessHandler(t, store, 0, &now)

	active, err := h.isActive(context.Background(), freshnessRef())
	if err != nil {
		t.Fatalf("isActive before any decision: %v", err)
	}
	if active {
		t.Fatal("a zero-parked target with no decision must report inactive")
	}

	store.Set(freshnessNS, freshnessTarget, 1)
	active, err = h.isActive(context.Background(), freshnessRef())
	if err != nil {
		t.Fatalf("isActive after activation: %v", err)
	}
	if !active {
		t.Fatal("a fresh activation must wake a zero-parked target")
	}
}

// TestGetMetricsIgnoresFreshness keeps the change scoped to the 0<->1 gate.
// GetMetrics drives HPA's scaling at >=1 replicas; expiring values there would
// make a running target's metric collapse to 0 between publishes.
func TestGetMetricsIgnoresFreshness(t *testing.T) {
	now := time.Now()
	store := decision.NewStore()
	h := freshnessHandler(t, store, 3, &now)
	store.Set(freshnessNS, freshnessTarget, 4)

	now = now.Add(activationTTL + time.Minute)
	resp, err := h.GetMetrics(context.Background(), &pb.GetMetricsRequest{ScaledObjectRef: freshnessRef()})
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if got := resp.MetricValues[0].MetricValue; got != 4 {
		t.Fatalf("GetMetrics = %d, want 4 (freshness must not apply here)", got)
	}
}
