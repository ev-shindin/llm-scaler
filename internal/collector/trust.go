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

// staleObservationLimit is how long a workload may go unobserved before WVA
// stops answering KEDA for it.
//
// This is the second of the two ways a metrics pipeline breaks, and the one that
// needs WVA's own clock rather than anything Prometheus reports: once a scrape
// has been dead long enough, Prometheus drops the series and the workload simply
// stops appearing in the rows at all. There is no stale value left to inspect --
// only an absence, which is invisible to any freshness check.
//
// Five minutes is twenty optimize intervals at the 15s default. It has to sit
// well above a single missed pass: a collection that errors, or a namespace
// whose query times out once, must not read as a workload that has vanished.
const staleObservationLimit = 5 * time.Minute

// publishTrustVerdicts records what this pass saw of each scale target and
// reports the model-level consequence on wva_model_scaling_blocked.
//
// Keyed by the SCALEDOBJECT, which is what VariantName holds -- see
// decision.TrustRecord for why that differs from the decision store's key, and
// for the bug that came of assuming they agreed.
//
// A target is marked untrusted when it produced rows and EVERY one of them is
// past the UNAVAILABLE threshold -- five minutes, not the one-minute fresh line
// the soft exclusions elsewhere use. That is a metrics pipeline that has STOPPED
// under a workload still running, which is the one state where WVA's number is a
// guess and KEDA's fallback is better than a guess. A pipeline that is merely
// late costs a replica its vote in an average; it does not cost the workload its
// ability to scale.
//
// Two conditions that look similar and are deliberately excluded:
//
//   - Not-Ready pods. A variant whose replicas are all still starting is the
//     normal shape of a scale-up, and abstaining would freeze the HPA at the
//     count it is trying to grow from -- turning a cold start into a stall.
//   - Rejected timings. A replica whose service time was dropped for exceeding
//     its own Pod's uptime has still reported its KV capacity, queue depth and
//     rates, so the analyzers retain an occupancy signal. Losing one derived
//     input is not losing the view.
//
// A pass that produced no rows for a target does not clear its record: the
// target simply stops being observed, and staleObservationLimit takes over. That
// is the whole point of tracking observation time separately from row contents.
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

	type tally struct {
		total    int
		unusable int
	}
	byTarget := make(map[string]*tally, len(rows))
	order := make([]string, 0, len(rows))

	for _, rm := range rows {
		if rm.VariantName == "" {
			continue
		}
		t, seen := byTarget[rm.VariantName]
		if !seen {
			t = &tally{}
			byTarget[rm.VariantName] = t
			order = append(order, rm.VariantName)
		}
		t.total++
		// Unavailable, NOT StaleOrOlder: this verdict stops WVA answering KEDA
		// for the workload, while the soft exclusions elsewhere only drop a
		// replica from an average. Keying it at the one-minute line would fire
		// on a healthy fleet behind a slow Prometheus, and would do so to every
		// replica at once, since scrape lag is systemic.
		if rm.Metadata.Unavailable() {
			t.unusable++
		}
	}

	logger := ctrl.LoggerFrom(ctx)
	for _, name := range order {
		t := byTarget[name]
		untrusted := t.unusable == t.total
		reason := ""
		if untrusted {
			reason = "every replica's metrics are older than the unavailable threshold"
			// At DEFAULT: this is about to stop WVA answering KEDA for this
			// workload, and an operator watching replicas not move needs the
			// reason in the controller's own log, not only in KEDA's.
			logger.V(logging.DEFAULT).Info("no trusted view of scale target; WVA will decline to answer KEDA",
				"namespace", namespace,
				"scaledObject", name,
				"replicas", t.total,
				"unusable", t.unusable)
		}
		trust.Observe(namespace, name, modelID, untrusted, reason, now)
	}

	// Model-level, because that is the granularity wva_model_scaling_blocked
	// has. Asked of the STORE rather than of this pass's rows, so it also covers
	// a workload that produced no rows at all -- the case the rows in hand
	// cannot show, and the one a dead scrape ends in.
	var active []string
	if unanswerable := trust.UnanswerableFor(namespace, modelID, now, staleObservationLimit); len(unanswerable) > 0 {
		active = constants.ScalingBlockedReasonsCollection
		logger.V(logging.DEFAULT).Info("model has workloads WVA cannot answer for",
			"namespace", namespace, "modelID", modelID, "scaledObjects", unanswerable)
	}
	metrics.SetModelScalingBlockedReasons(namespace, modelID,
		constants.ScalingBlockedReasonsCollection, active)
}
