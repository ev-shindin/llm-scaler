package collector

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// freshnessStale is the collector's own verdict string for a scrape that is
// behind, as written into ReplicaMetricsMetadata.FreshnessStatus.
const freshnessStale = "stale"

// publishTrustVerdicts records, per scale target, whether this pass produced a
// usable view of it -- see decision.TrustStore for what the verdict is for.
//
// The rule is narrow on purpose: a target is untrusted only when it produced
// rows and EVERY one of them is stale. That is the metrics pipeline breaking
// under a workload that is still there, which is the one state where WVA's
// number is a guess and KEDA's fallback is a better answer than a guess.
//
// Two conditions that look similar and are deliberately excluded:
//
//   - Not-Ready pods. A variant whose replicas are all still starting is the
//     normal shape of a scale-up, and abstaining would freeze the HPA at the
//     count it is trying to grow from -- turning a cold start into a stall.
//     Readiness gates a pod's TIMING, upstream of here; it says nothing about
//     whether WVA can see the workload.
//   - Rejected timings. A replica whose service time failed the plausibility
//     cross-check has still reported its KV capacity, queue depth and rates, so
//     the analyzers retain an occupancy signal and the recommendation stands on
//     its own. Losing one derived input is not losing the view.
//
// A target with no rows at all gets no verdict, trusted or otherwise. It may be
// parked at zero, or new; either way this pass observed nothing about it and
// silence is the honest report. decision.TrustStore.Trust reads a missing
// verdict as trusted for the same reason.
func publishTrustVerdicts(ctx context.Context, rows []domain.ReplicaMetrics, now time.Time) {
	if len(rows) == 0 {
		return
	}

	type tally struct {
		namespace string
		total     int
		stale     int
	}
	byTarget := make(map[string]*tally, len(rows))
	order := make([]string, 0, len(rows))

	for _, rm := range rows {
		if rm.VariantName == "" {
			continue
		}
		t, seen := byTarget[rm.VariantName]
		if !seen {
			t = &tally{namespace: rm.Namespace}
			byTarget[rm.VariantName] = t
			order = append(order, rm.VariantName)
		}
		t.total++
		if rm.Metadata != nil && rm.Metadata.FreshnessStatus == freshnessStale {
			t.stale++
		}
	}

	logger := ctrl.LoggerFrom(ctx)
	for _, name := range order {
		t := byTarget[name]
		trusted := t.stale < t.total
		reason := ""
		if !trusted {
			reason = "every replica's metrics are stale"
			// At DEFAULT: this is about to stop WVA answering KEDA for this
			// workload, and an operator watching replicas not move needs the
			// reason in the controller's own log, not only in KEDA's.
			logger.V(logging.DEFAULT).Info("no trusted view of scale target; WVA will decline to answer KEDA",
				"namespace", t.namespace,
				"target", name,
				"replicas", t.total,
				"stale", t.stale)
		}
		decision.DefaultTrust.Publish(t.namespace, name, trusted, reason, now)
	}
}
