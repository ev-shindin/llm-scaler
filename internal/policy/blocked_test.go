package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

func states(mins ...*int) []domain.VariantReplicaState {
	out := make([]domain.VariantReplicaState, 0, len(mins))
	for i, m := range mins {
		out = append(out, domain.VariantReplicaState{
			VariantName: string(rune('a' + i)),
			MinReplicas: m,
		})
	}
	return out
}

func TestScaleToZeroBlockReasons(t *testing.T) {
	zero, one := ptr.To(0), ptr.To(1)

	for _, tc := range []struct {
		name            string
		scaleToZero     bool
		engineSupported bool
		recentlyWoken   bool
		states          []domain.VariantReplicaState
		want            []string
	}{
		// The two coherent configurations. Reporting either would train operators
		// to ignore the metric, which costs more than it buys.
		{
			name:            "enabled and every variant permits zero: nothing to say",
			scaleToZero:     true,
			engineSupported: true,
			states:          states(zero, zero),
			want:            nil,
		},
		{
			name:            "disabled and a variant is floored: the two halves agree",
			scaleToZero:     false,
			engineSupported: true,
			states:          states(one, zero),
			want:            nil,
		},

		// The contradictions.
		{
			name:            "enabled but one variant floored: the model never reaches zero",
			scaleToZero:     true,
			engineSupported: true,
			states:          states(zero, one),
			want:            []string{constants.ScalingBlockedVariantFloor},
		},
		{
			name:            "disabled while every variant asks to park",
			scaleToZero:     false,
			engineSupported: true,
			states:          states(zero, zero),
			want:            []string{constants.ScalingBlockedPolicyForbidsZero},
		},
		{
			// The single-variant case is the one worth naming: minReplicas: 0 on the
			// only variant reads exactly like a deliberate request to park.
			name:            "disabled with a single variant at zero",
			scaleToZero:     false,
			engineSupported: true,
			states:          states(zero),
			want:            []string{constants.ScalingBlockedPolicyForbidsZero},
		},
		{
			name:            "enabled and unfloored, but the engine cannot be measured",
			scaleToZero:     true,
			engineSupported: false,
			states:          states(zero),
			want:            []string{constants.ScalingBlockedEngineUnsupported},
		},
		{
			name:            "both an operator floor and an unmeasurable engine",
			scaleToZero:     true,
			engineSupported: false,
			states:          states(one),
			want: []string{
				constants.ScalingBlockedVariantFloor,
				constants.ScalingBlockedEngineUnsupported,
			},
		},
		{
			// An engine WVA cannot measure is only worth reporting against a
			// configuration that would otherwise park. With scale-to-zero disabled
			// nobody asked to park, so the limitation is not blocking anything.
			name:            "unmeasurable engine is silent when scale-to-zero is off",
			scaleToZero:     false,
			engineSupported: false,
			states:          states(one),
			want:            nil,
		},

		// nil MinReplicas means unset, which is not a floor.
		{
			name:            "unset bounds count as permitting zero",
			scaleToZero:     false,
			engineSupported: true,
			states:          states(nil, nil),
			want:            []string{constants.ScalingBlockedPolicyForbidsZero},
		},
		{
			// Transient, and the reason this is reported at all: without it a held
			// model is indistinguishable from one that will never park, because every
			// skip in the enforcement path logs only at V(DEBUG).
			name:            "a model just woken from zero is held",
			scaleToZero:     true,
			engineSupported: true,
			recentlyWoken:   true,
			states:          states(zero),
			want:            []string{constants.ScalingBlockedActivationRetention},
		},
		{
			// Both at once: an operator chasing the floor must not have the hold
			// hidden, and vice versa.
			name:            "a floored model that was also just woken reports both",
			scaleToZero:     true,
			engineSupported: true,
			recentlyWoken:   true,
			states:          states(one),
			want: []string{
				constants.ScalingBlockedVariantFloor,
				constants.ScalingBlockedActivationRetention,
			},
		},
		{
			// The hold only matters against a configuration that would otherwise
			// park; with scale-to-zero off nothing was going to park anyway.
			name:            "the hold is silent when the policy forbids zero",
			scaleToZero:     false,
			engineSupported: true,
			recentlyWoken:   true,
			states:          states(one),
			want:            nil,
		},
		{
			// No variants means nothing is asking for anything. "Every variant
			// permits zero" is vacuously true here and would report a contradiction
			// for a model that has not been discovered yet.
			name:            "no variants reports nothing",
			scaleToZero:     false,
			engineSupported: true,
			states:          nil,
			want:            nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ScaleToZeroBlockReasons(tc.scaleToZero, tc.engineSupported, tc.recentlyWoken, tc.states)
			assert.Equal(t, tc.want, got)
		})
	}
}
