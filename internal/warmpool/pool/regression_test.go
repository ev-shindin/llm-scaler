package pool

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// These cover defects found in review. Each one failed before its fix.

func TestAnIdlePodIsStillReserve(t *testing.T) {
	// Memberships are the only view of the pool, and one exists per resident
	// instance -- so a Pod holding nothing produced no memberships at all,
	// making a healthy idle pool report a reserve of zero. Admission is budgeted
	// against the reserve, so a fresh pool could never admit its first model:
	// the shipped manifest starts exactly like this.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	h.instances = nil

	got, err := h.adapter.ListWarm(context.Background())
	if err != nil {
		t.Fatalf("ListWarm: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("an empty Pod must still appear: %+v", got)
	}
	if got[0].Model.Variant != "" || got[0].State != Absent {
		t.Fatalf("an empty Pod is the placeholder, not a model: %+v", got[0])
	}
	if FreePods(got) != 1 {
		t.Fatalf("a Pod holding nothing is free: FreePods = %d", FreePods(got))
	}
	if Resident(got) != 0 {
		t.Fatalf("and holds no models: Resident = %d", Resident(got))
	}
	// It must not be offered as holding anything.
	if pods := PodsHolding(got, ""); len(pods) != 0 {
		t.Fatalf("the placeholder is not a model: %v", pods)
	}
}

func TestActivateRefusesASelectorThatRewritesThePodsIdentity(t *testing.T) {
	// A tenant's InferencePool selector is theirs to write, and generic keys are
	// common. One naming app.kubernetes.io/component or /name would rewrite the
	// pool Pod out of its OWN Deployment's selector: the ReplicaSet releases it,
	// leaving a GPU-holding orphan with no controller, invisible to ListWarm.
	for _, guarded := range []string{ComponentLabel, NameLabel} {
		t.Run(guarded, func(t *testing.T) {
			h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
			model := qwen()
			model.PoolLabels = map[string]string{guarded: "someone-elses-value"}

			_, err := h.adapter.Activate(context.Background(), podA(), model)
			if err == nil {
				t.Fatal("want a refusal rather than an orphaned Pod")
			}
			if !strings.Contains(err.Error(), guarded) {
				t.Errorf("the error should name the label: %v", err)
			}
			h.journal.never(t, "wake")

			var got corev1.Pod
			if err := h.k8s.Get(context.Background(), podA(), &got); err != nil {
				t.Fatalf("get pod: %v", err)
			}
			if got.Labels[ComponentLabel] != ComponentValue {
				t.Fatal("the Pod must keep its own identity")
			}
		})
	}
}

func TestASelectorMayRepeatTheIdentityLabelsItAlreadyMatches(t *testing.T) {
	// Refusing only a CHANGE, not a mention: a selector that happens to name the
	// same value is harmless and must not block every wake.
	h := newHarness(t, poolPod("pod-a", "10.0.0.1", nil))
	model := qwen()
	model.PoolLabels = map[string]string{
		ComponentLabel:   ComponentValue, // same value the Pod already carries
		"llm-d.ai/model": "qwen",
	}
	if _, err := h.adapter.Activate(context.Background(), podA(), model); err != nil {
		t.Fatalf("Activate: %v", err)
	}
}
