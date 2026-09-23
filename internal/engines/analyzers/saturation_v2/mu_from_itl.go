package saturation_v2

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The demand floor's mu is a request rate, and a request rate is not a property
// of a replica -- it is a property of a replica AND a shape. One replica
// generating G tokens a second completes G/O requests a second, so halving the
// generation length doubles mu with nothing about the hardware having changed.
// Measuring that per output-length bucket is a workaround for the units, and it
// cannot be made to work: the reading is only taken while a replica is
// saturated, and a fleet over-provisioned for the shape it has just moved to
// never saturates, so it never prices that shape.
//
// Measured on the 1k/6000 -> 8k/1000 swap of 2026-09-23. Nineteen minutes after
// the switch not one reading had been taken in the bucket the fleet was
// serving, and the floor was still pricing 6 req/s against the 6000-token
// phase's figure. Which figure it inherited was luck: 1.2754 on that run
// against 1.5429 on the one before, implying 4.70 and 3.89 replicas from an
// identical workload. The fleet held 7 while occupancy read a utilization
// of 0.028.
//
// Derived instead, over the model the throughput analyzer already fits and
// signals/itl now carries the arithmetic for:
//
//	muTok = itl.TokenRate(model, kSat, C, KVreq)   tokens/s at saturation
//	muReq = muTok / O                              what the floor prices with
//
// Nothing in it is keyed by a bucket and nothing waits for a saturated cycle:
// (A, B) is fitted from (k, ITL) pairs a replica reports at ANY load, and I
// and O are this cycle's own shape. A shape change reprices the fleet on the
// cycle it is observed rather than whenever the fleet next happens to saturate.
//
// It reproduces a measurement it was never given, which is the reason to
// believe it: on the trace's first shape it derives 1.487 req/s where the fleet
// measured 1.4292 and 1.5429 for itself, and on the second -- which the fleet
// never measured at all -- 4.200 req/s, asking for 1.43 replicas at the 6 req/s
// arriving. Occupancy independently said one.
//
// docs/proposals/shape-shift-treatment.md sets this out in full.

// derivedMu is one variant's mu, priced from its fitted ITL model rather than
// from a saturated reading. ok is false when the variant has no model yet, in
// which case the caller keeps the measured window.
type derivedMu struct {
	rate     float64
	seqs     float64
	tokenSec float64
	ok       bool
}

// deriveMu prices one replica of this variant at saturation under the shape it
// is serving now.
//
// kvPerRequest is the time-averaged footprint IL + OL/2 (shape.Shape.KVreq):
// sequence ages are spread over [0, OL] in steady state, so the average
// resident sequence carries half its generation.
//
// The cap is the engine's max_num_seqs, and it is not optional: short requests
// imply more resident sequences than the engine will admit, and the trace's
// second phase ran at exactly 256, the configured ceiling. itl.TokenRate does
// not apply it -- it prices a cache, not an engine -- so the sequence count is
// capped here before the division.
//
// C is the engine's whole KV capacity, not k1. k1 is already C times the
// analyzer's KV threshold, and itl.Sequences applies k itself, so passing k1
// would apply a threshold twice.
func deriveMu(model itl.Model, params *capacity.EngineParams,
	totalKvTokens int64, avgInput, avgOutput float64) derivedMu {
	if model.IsZero() || !(avgOutput > 0) || !(avgInput > 0) {
		return derivedMu{}
	}
	capacityTokens := float64(totalKvTokens)
	if params != nil && params.TotalKvTokensOverride > 0 {
		capacityTokens = float64(params.TotalKvTokensOverride)
	}
	kvPerRequest := avgInput + avgOutput/2
	seqs := itl.Sequences(itl.DefaultKSat, capacityTokens, kvPerRequest)
	if seqs <= 0 {
		return derivedMu{}
	}
	if params != nil && params.MaxNumSeqs > 0 {
		seqs = min(seqs, float64(params.MaxNumSeqs))
	}
	itlSec := model.ITLAt(itl.DefaultKSat)
	if !(itlSec > 0) {
		return derivedMu{}
	}
	tokenSec := seqs / itlSec
	rate := tokenSec / avgOutput
	if !(rate > 0) {
		return derivedMu{}
	}
	return derivedMu{rate: rate, seqs: seqs, tokenSec: tokenSec, ok: true}
}

// noteITL adds this cycle's (k, ITL) readings for one variant to its rolling
// window and returns the model fitted over what the window holds.
//
// Ready replicas of the variant itself only, and only where both halves of the
// pair are present: a pod still failing its readiness probe, or one lent by the
// warm pool running on the pool's own engine settings, is not a reading of what
// one of this variant's replicas does. Above itl.DefaultMaxObservableK the
// engine preempts rather than slowing down and the line stops describing it, so
// those readings are left out too.
//
// The window is NOT cleared on a shape change, which is the difference from the
// throughput analyzer's use of it. ITL(k) is a property of the hardware and the
// engine, not of the shape -- that is the whole reason mu can be derived across
// a shape change -- so a fit made under one shape is still the right fit under
// the next, and clearing it would reintroduce exactly the wait this replaces.
func (a *SaturationAnalyzer) noteITL(key string, replicas []domain.ReplicaMetrics,
	variantName string, now time.Time) itl.Model {
	a.mu.Lock()
	defer a.mu.Unlock()
	w, ok := a.itlWindows[key]
	if !ok {
		w = itl.NewWindow(
			itl.DefaultWindowMaxSize,
			itl.DefaultObservationMaxAge,
			itl.DefaultMinSamples,
			itl.DefaultMinKSpread,
			itl.DefaultMinObservableK,
			itl.DefaultMaxObservableK,
		)
		a.itlWindows[key] = w
	}
	for _, rm := range replicas {
		if rm.VariantName != variantName || !rm.Ready || rm.FromWarmPool {
			continue
		}
		if !(rm.AvgITL > 0) || !(rm.KvUsageInstant > 0) {
			continue
		}
		if rm.KvUsageInstant > itl.DefaultMaxObservableK {
			continue
		}
		w.Add(rm.KvUsageInstant, rm.AvgITL, now)
	}
	w.Prune(now)
	// The window's own confidence gate, not itl.Fit's. Fit will draw a line
	// through any two points that are not on top of each other; Ready is
	// what says the points are enough of them and far enough apart in k to
	// mean something (DefaultMinSamples, DefaultMinKSpread). The throughput
	// analyzer gates on it for the same reason, and a derived mu overrides
	// the measured one, so it has to clear a higher bar than two readings.
	if !w.Ready() {
		return itl.Model{}
	}
	model, ok := itl.Fit(w.Observations())
	if !ok {
		return itl.Model{}
	}
	return model
}
