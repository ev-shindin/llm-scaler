package scalingpolicy

import (
	"context"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// HasMinReplicasAboveZero returns true if any variant in the states has MinReplicas > 0.
func HasMinReplicasAboveZero(states []domain.VariantReplicaState) bool {
	for _, state := range states {
		if state.MinReplicas != nil && *state.MinReplicas > 0 {
			return true
		}
	}
	return false
}

// ScaleToZeroBlockReasons reports every reason a model cannot reach zero despite
// a configuration that reads as though it should.
//
// It reports CONTRADICTIONS, not settings. Scale-to-zero disabled alongside a
// variant floor is a coherent choice and yields nothing; so does scale-to-zero
// enabled with every variant permitting it. What is worth an operator's
// attention is one half permitting zero while the other forbids it, because the
// half they set is the half they will believe:
//
//   - scale-to-zero enabled, some variant floored — the model never reaches zero
//     and the setting is inert. This one has no other symptom at all: the model
//     is up and serving, exactly as every other metric says it should be, while
//     the accelerators are billed indefinitely.
//   - every variant floored at zero, policy disabling scale-to-zero — the bounds
//     are inert. Most misleading with a single variant, where minReplicas: 0
//     reads exactly like a deliberate request to park.
//
// recentlyWoken is the activation-retention hold — transient, and the only reason
// here that clears itself. It is included because the alternative is a model
// sitting at one replica for minutes with the explanation available only at
// V(DEBUG).
//
// engineSupported folds in the enforcer's own limitation rather than the
// operator's: a non-vLLM engine cannot be measured for idleness, so WVA declines
// to park it. It is reported only when the configuration would otherwise park,
// since otherwise it is a limitation on something nobody asked for.
//
// Pure, so it is worth testing directly: this is the whole of the judgement, and
// the caller does nothing but emit the result.
func ScaleToZeroBlockReasons(scaleToZeroEnabled, engineSupported, recentlyWoken bool, states []domain.VariantReplicaState) []string {
	var reasons []string

	if !scaleToZeroEnabled {
		// Only a contradiction if every variant is asking to park. A floor here
		// agrees with the policy, and agreement is not worth reporting.
		if !HasMinReplicasAboveZero(states) && len(states) > 0 {
			reasons = append(reasons, constants.ScalingBlockedPolicyForbidsZero)
		}
		return reasons
	}

	if HasMinReplicasAboveZero(states) {
		reasons = append(reasons, constants.ScalingBlockedVariantFloor)
	}
	if !engineSupported {
		reasons = append(reasons, constants.ScalingBlockedEngineUnsupported)
	}
	// Reported alongside the others rather than instead of them: a model can be
	// both freshly woken and floored, and an operator chasing one should not have
	// the other hidden.
	if recentlyWoken {
		reasons = append(reasons, constants.ScalingBlockedActivationRetention)
	}
	return reasons
}

// LogBlockedTransition reports a change in why a model cannot reach zero.
//
// Info at V(0), not a warning: logr has Info and Error and no Warn, and none of
// these reasons is an error — a model that will never park is serving perfectly
// well, which is exactly why nothing else reports it. On transition only,
// because the alternative on a per-interval loop is the same line forever.
func LogBlockedTransition(ctx context.Context, namespace, modelID string, reasons []string) {
	logger := ctrl.LoggerFrom(ctx)
	if len(reasons) == 0 {
		logger.Info("Model can now reach zero: nothing is blocking it",
			"modelID", modelID, "namespace", namespace)
		return
	}
	logger.Info("Model will not scale to zero",
		"modelID", modelID,
		"namespace", namespace,
		"reasons", strings.Join(reasons, ","),
		"detail", blockedDetail(reasons))
}

// blockedDetail spells out what each reason means for this model, since the
// reason slugs are chosen for a metric label rather than for a reader.
func blockedDetail(reasons []string) string {
	details := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		switch reason {
		case constants.ScalingBlockedVariantFloor:
			details = append(details, "scale-to-zero is enabled but a variant declares minReplicas > 0, "+
				"so the model never reaches zero and the setting is inert")
		case constants.ScalingBlockedPolicyForbidsZero:
			details = append(details, "every variant permits zero but this model's scaling policy disables "+
				"scale-to-zero, so the replica bounds are inert")
		case constants.ScalingBlockedActivationRetention:
			details = append(details, "the model was recently woken from zero and is held for its "+
				"retention period, so the request that woke it is not undone before it is served")
		case constants.ScalingBlockedEngineUnsupported:
			details = append(details, "the model runs more than one inference engine, so no single "+
				"request counter measures its idleness; vLLM and SGLang are each supported alone")
		default:
			details = append(details, reason)
		}
	}
	return strings.Join(details, "; ")
}
