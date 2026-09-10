package collector

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// publishTrustVerdicts records, per scale target, whether this pass produced a
// usable view of it -- see decision.TrustStore for what the verdict is for --
// and reports the model-level consequence on wva_model_scaling_blocked.
//
// The store is a parameter rather than decision.DefaultTrust read inline, so a
// test can supply its own instead of swapping a package-level variable out from
// under whatever else is running.
//
// The rule is narrow on purpose: a target is untrusted only when it produced
// rows and EVERY one of them is past the UNAVAILABLE threshold -- five minutes,
// not the one-minute fresh line the soft exclusions elsewhere use. That is a
// metrics pipeline that has STOPPED under a workload still running, which is the
// one state where WVA's number is a guess and KEDA's fallback is better than a
// guess. A pipeline that is merely late costs a replica its vote in an average;
// it does not cost the workload its ability to scale.
//
// Two conditions that look similar and are deliberately excluded:
//
//   - Not-Ready pods. A variant whose replicas are all still starting is the
//     normal shape of a scale-up, and abstaining would freeze the HPA at the
//     count it is trying to grow from -- turning a cold start into a stall.
//     Readiness gates a pod's TIMING, upstream of here; it says nothing about
//     whether WVA can see the workload.
//   - Rejected timings. A replica whose service time was dropped for exceeding
//     its own Pod's uptime has still reported its KV capacity, queue depth and
//     rates, so the analyzers retain an occupancy signal and the recommendation
//     stands on its own. Losing one derived input is not losing the view.
//
// A target with no rows at all gets no verdict, trusted or otherwise. It may be
// parked at zero, or new; either way this pass observed nothing about it and
// silence is the honest report. decision.TrustStore.Trust reads a missing
// verdict as trusted for the same reason.
func publishTrustVerdicts(
	ctx context.Context,
	trust *decision.TrustStore,
	namespace, modelID string,
	rows []domain.ReplicaMetrics,
	now time.Time,
) {
	if trust == nil {
		trust = decision.DefaultTrust
	}
	if len(rows) == 0 {
		// Trust verdicts are left alone -- they expire on their own -- but the
		// blocked reason is CLEARED.
		//
		// An earlier version returned here without touching the gauge, on the
		// reasoning that no rows says nothing about the model. That is true of
		// the verdict and false of the metric: the verdict expires after
		// trustTTL and WVA resumes answering, while a gauge nobody rewrites
		// stays at 1 forever, so a model whose rows vanished would report
		// blocked long after it had stopped being blocked. The metric must not
		// outlive the condition it reports. Retracting it here is the safe
		// direction of the two -- it can under-report for at most trustTTL,
		// where the alternative over-reports until the process restarts -- and
		// a model with no rows at all is usually parked, not wedged.
		metrics.SetModelScalingBlockedReasons(namespace, modelID,
			constants.ScalingBlockedReasonsCollection, nil)
		return
	}

	type tally struct {
		namespace string
		total     int
		unusable  int
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
		// Unavailable, NOT StaleOrOlder, and the gap between them is the whole
		// point. This verdict stops WVA answering KEDA for the workload, which
		// holds it where it stands; the soft exclusions elsewhere only drop a
		// replica from an average. An earlier version keyed this on
		// StaleOrOlder -- data more than a MINUTE old -- which is close enough
		// to a healthy replica's age on a 30s scrape that a slow Prometheus
		// could have frozen a working fleet, and it would have done so to every
		// replica at once, since scrape lag is systemic. Five minutes is a
		// scrape that has stopped. See ReplicaMetricsMetadata.Unavailable.
		if rm.Metadata.Unavailable() {
			t.unusable++
		}
	}

	logger := ctrl.LoggerFrom(ctx)
	anyUntrusted := false
	for _, name := range order {
		t := byTarget[name]
		trusted := t.unusable < t.total
		reason := ""
		if !trusted {
			anyUntrusted = true
			reason = "every replica's metrics are older than the unavailable threshold"
			// At DEFAULT: this is about to stop WVA answering KEDA for this
			// workload, and an operator watching replicas not move needs the
			// reason in the controller's own log, not only in KEDA's.
			logger.V(logging.DEFAULT).Info("no trusted view of scale target; WVA will decline to answer KEDA",
				"namespace", t.namespace,
				"target", name,
				"replicas", t.total,
				"unusable", t.unusable)
		}
		trust.Publish(t.namespace, name, trusted, reason, now)
	}

	// Model-level, because that is the granularity wva_model_scaling_blocked
	// has. A model with several variants reports blocked while ANY of them is
	// unanswerable: the series says a model is not being scaled the way WVA
	// would scale it, and one frozen variant makes that true.
	var active []string
	if anyUntrusted {
		active = constants.ScalingBlockedReasonsCollection
	}
	metrics.SetModelScalingBlockedReasons(namespace, modelID,
		constants.ScalingBlockedReasonsCollection, active)
}
