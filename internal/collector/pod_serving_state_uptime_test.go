package collector

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestPodUptime covers podUptime directly, and in particular the branches the
// service-time guard's own table cannot reach.
//
// Every one of them is a fail-open path, and they are the reason the guard is
// safe to have at all: a bound that cannot be established must not delete a
// value it cannot judge. A test that only drove the guard end-to-end left the
// clock-skew branch removable with the whole suite still green.
func TestPodUptime(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	now := time.Now()

	pod := func(name string, start *time.Time) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test-ns"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		if start != nil {
			ts := metav1.NewTime(*start)
			p.Status.StartTime = &ts
		}
		return p
	}

	oneHourAgo := now.Add(-time.Hour)
	future := now.Add(10 * time.Minute)

	cases := []struct {
		name      string
		podName   string
		wantKnown bool
		wantMin   time.Duration
	}{
		{
			name: "a running pod reports its age", podName: "started",
			wantKnown: true, wantMin: 59 * time.Minute,
		},
		{
			// Unknown rather than zero: a bound of zero would make every value
			// look impossible.
			name: "no start time is unknown", podName: "no-start", wantKnown: false,
		},
		{
			// A start time in the future is clock skew between this process and
			// the node, not a pod that has not started.
			name: "a future start time is unknown", podName: "skewed", wantKnown: false,
		},
		{
			// Absent from a good listing. The guard must not judge a pod the
			// listing does not contain.
			name: "a pod absent from the listing is unknown", podName: "ghost", wantKnown: false,
		},
	}

	c := NewReplicaMetricsCollector(nil, nil,
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			pod("started", &oneHourAgo),
			pod("no-start", nil),
			pod("skewed", &future),
		).Build(),
		nil, nil)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uptime, known := c.podUptime(context.Background(), "test-ns", tc.podName, now)
			if known != tc.wantKnown {
				t.Fatalf("known is %v, want %v (uptime %v)", known, tc.wantKnown, uptime)
			}
			if known && uptime < tc.wantMin {
				t.Errorf("uptime is %v, want at least %v", uptime, tc.wantMin)
			}
			if !known && uptime != 0 {
				t.Errorf("uptime is %v on an unknown answer, want 0", uptime)
			}
		})
	}
}

// TestPodUptime_UnreadableListingIsUnknown pins the last fail-open path: with no
// API reader there is no listing, and an unreadable listing says nothing at all.
// Treating it as "every pod is brand new" would delete every service time in the
// namespace on an RBAC error.
func TestPodUptime_UnreadableListingIsUnknown(t *testing.T) {
	c := NewReplicaMetricsCollector(nil, nil, nil, nil, nil)
	if uptime, known := c.podUptime(context.Background(), "test-ns", "any", time.Now()); known {
		t.Errorf("an unreadable listing reported a known uptime of %v", uptime)
	}
}
