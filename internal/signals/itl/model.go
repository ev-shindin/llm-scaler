// Package itl is the linear inter-token-latency model ITL(k) = A·k + B over
// KV utilization k: the observations it is fitted from, the window that
// holds them, the fit and its validity rule.
package itl

import (
	"math"
)

// DefaultKSat is the KV utilization fraction at which per-replica capacity is
// evaluated. Mirrors DefaultScaleUpThreshold in saturation config so that the
// throughput analyzer and saturation analyzer agree on the definition of "full".
// TODO: unify with the system-wide k_sat used by the EPP and saturation analyzer.
const DefaultKSat = 0.85

// slopeEpsilon is the smallest slope A that counts as meaningfully positive.
// A perfectly flat fit is mathematically A == 0 but OLS rounding leaves noise of
// order 1e-17; real slopes in this domain are order 1e-2. This threshold sits far
// above the noise floor and far below any genuine slope.
const slopeEpsilon = 1e-12

// Model is the linear inter-token latency model: ITL(k) = A·k + B.
// A is the slope (marginal latency cost per unit of KV utilization) and B is the
// hardware baseline latency observed at near-zero KV load.
type Model struct {
	A float64
	B float64
}

// IsZero returns true when the model has not been fitted (both coefficients are zero).
func (m Model) IsZero() bool {
	return m.A == 0 && m.B == 0
}

// ITLAt returns the predicted ITL (seconds/token) at KV utilization k.
func (m Model) ITLAt(k float64) float64 {
	return m.A*k + m.B
}

// ValidModel reports whether an (A, B) pair is a usable ITL model: finite,
// meaningfully-positive slope, finite intercept, and positive ITL at saturation.
// Shared by the Tier-1 OLS fit (Fit) and the throughput analyzer's Tier-2
// constrained fit so the two validation paths cannot drift apart.
func ValidModel(a, b float64) bool {
	// Defensive guard: NaN/+Inf a both slip past the a <= 0 check below, and a non-zero
	// sumK can leave b finite so the b guard would not catch them. Symmetric with the b guard.
	if math.IsNaN(a) || math.IsInf(a, 0) {
		return false
	}
	// Reject flat/inverted fits. A perfectly flat line (constant ITL) is
	// mathematically a == 0, but OLS rounding leaves tiny noise whose sign is
	// platform-dependent (e.g. +2.6e-17 on arm64, ≤0 on amd64). Compare against a
	// small epsilon so any slope that isn't meaningfully positive counts as flat.
	// Real slopes in this domain are order 1e-2; noise is order 1e-17.
	if a <= slopeEpsilon {
		return false
	}
	// Defensive guard: NaN/Inf b is mathematically possible with degenerate input.
	if math.IsNaN(b) || math.IsInf(b, 0) {
		return false
	}
	// Guard: ensure ITL at saturation is positive. A noisy fit can yield negative
	// b (valid a>0), making ITLAt(DefaultKSat) near-zero and inflating supply.
	if a*DefaultKSat+b <= 0 {
		return false
	}
	return true
}

// Fit fits the linear model ITL(k) = A·k + B to the observations using
// ordinary least squares (OLS). Returns (model, true) on success.
//
// Returns (zero, false) when:
//   - fewer than 2 observations are provided
//   - k-spread across observations is zero (degenerate — no discriminating signal)
//   - the fitted (A, B) fails ValidModel (inverted/flat/non-finite/non-positive-at-saturation)
func Fit(obs []Observation) (Model, bool) {
	n := float64(len(obs))
	if n < 2 {
		return Model{}, false
	}

	var sumK, sumITL, sumK2, sumKITL float64
	for _, o := range obs {
		sumK += o.K
		sumITL += o.ITLSec
		sumK2 += o.K * o.K
		sumKITL += o.K * o.ITLSec
	}

	denom := n*sumK2 - sumK*sumK
	if math.Abs(denom) < 1e-12 {
		return Model{}, false
	}

	A := (n*sumKITL - sumK*sumITL) / denom
	B := (sumITL - A*sumK) / n

	if !ValidModel(A, B) {
		return Model{}, false
	}

	return Model{A: A, B: B}, true
}
