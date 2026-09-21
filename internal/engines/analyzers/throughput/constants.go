package throughput

const (
	// AnalyzerName is the canonical name for the throughput analyzer.
	AnalyzerName = "throughput"

	// DefaultMinTokensPerRequest is the minimum plausible value for AvgOutputTokens
	// or AvgInputTokens. Values at or below this threshold indicate the metric
	// is unavailable or zero-padded and are flagged as a sanity issue.
	DefaultMinTokensPerRequest = 1.0

	// DefaultBaselineITLSec is the hardware baseline inter-token latency (seconds/token)
	// used in tier-2 estimation when the OLS window is not yet ready.
	// Derived from H100 SXM5 measurements at near-zero KV load; workload-independent.
	DefaultBaselineITLSec = 0.006

	// DefaultQueueDrainFactor controls how aggressively queued requests count as
	// decode demand. The assumed drain time is QueueDrainFactor × ITL(k_sat) × avgOL;
	// after avgOL cancels, queue demand = QueueSize / (QueueDrainFactor × ITL(k_sat)).
	// A factor of 2.0 bounds per-request queueing time to ≤ 2 × ITL(k_sat) × avgOL.
	DefaultQueueDrainFactor = 2.0

	// DefaultMinDecodeOLForLocalDemand is the minimum AvgOutputTokens required
	// before the k*-based local demand estimator is applied. The estimator derives
	// λ_dec = N_dec(k*) / ITL(k*) where N_dec is approximated from KV utilization
	// as k* × KV_max / KVreq. This approximation only holds in the decode-dominated
	// regime (N_pre ≈ 1, TA-supply.md §3.1), which requires sufficiently long OL.
	// When OL ≈ 0, KV usage is from prefill rather than decode; the formula then
	// produces spurious non-zero demand instead of the correct λ_dec = 0.
	DefaultMinDecodeOLForLocalDemand = 20.0

	// DefaultGPSMismatchThresholdPct is the maximum tolerable percentage error between
	// the model-predicted decode rate μ_dec(k*) and the directly observed GPS
	// (GenerationTokenRate). Errors above this threshold at k* ≥ DefaultGPSMinKForVerification
	// indicate the ITL model may be wrong and trigger SpareCapacity suppression.
	DefaultGPSMismatchThresholdPct = 15.0

	// DefaultGPSMinKForVerification is the minimum KV utilization (k*) required before
	// GPS verification is applied. Below this threshold the in-flight count N_dec is
	// small and percentage errors on GPS are unreliable.
	DefaultGPSMinKForVerification = 0.30

	// DefaultNearKSatMargin is the margin below DefaultKSat at which a replica is
	// considered "near saturation" for GPS sanity diagnostics. At k* above
	// DefaultKSat - DefaultNearKSatMargin the GPS signal is near-oracle quality:
	// any model–GPS discrepancy is a strong indicator of a model error.
	DefaultNearKSatMargin = 0.10

	// DefaultNearKSatITLResidualThreshold is the fractional ITL residual above which
	// the observed AvgITL is considered inconsistent with the ITL model near k_sat.
	// A large residual points to bad data points or model drift.
	DefaultNearKSatITLResidualThreshold = 0.20

	// DefaultNearKSatNDecResidualThreshold is the fractional N_dec cross-check threshold
	// used in near-k_sat GPS sanity. If ITL residual is small but GPS × AvgITL disagrees
	// with KV-derived N_dec, the workload shape (KVreq via IL, OL, or hit rate) may be wrong.
	DefaultNearKSatNDecResidualThreshold = 0.20

	// DefaultGPSMismatchClearThreshold is the number of consecutive reconcile cycles
	// with a GPS mismatch before the observation window is cleared for recalibration.
	// Requiring N consecutive mismatches filters transient GPS noise while still
	// breaking persistent calibration lock. The counter resets to zero whenever the
	// window is cleared (shape change or threshold reached) so it is always bound to
	// the current window's lifetime.
	DefaultGPSMismatchClearThreshold = 3

	// itlReason* are the values set on VariantCapacity.Reason by resolveITLModel.
	// They appear in the "analyzer-result" structured log line.
	itlReasonT1OLS     = "T1-ols"     // tier-1: OLS fit from live observations
	itlReasonT2Default = "T2-default" // tier-2: constrained OLS with default B baseline
	itlReasonT2Pinned  = "T2-pinned"  // tier-2: constrained OLS with previously fitted B
	itlReasonT2Failed  = "T2-failed"  // all paths exhausted; no model for this cycle
)
