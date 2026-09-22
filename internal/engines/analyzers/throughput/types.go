package throughput

// SanityIssue is a diagnostic tag describing a metric quality problem detected
// during a reconcile cycle.
type SanityIssue string

const (
	// SanityIssueNoReplicas indicates the model has no replica metrics at all.
	SanityIssueNoReplicas SanityIssue = "no_replicas"

	// SanityIssueMissingKV indicates TotalKvCapacityTokens is zero or negative,
	// meaning the KV cache configuration metric (cache_config_info) is unavailable.
	SanityIssueMissingKV SanityIssue = "missing_kv_capacity"

	// SanityIssueKVOutOfRange indicates KvUsageInstant (k*) is outside [0, 1].
	SanityIssueKVOutOfRange SanityIssue = "kv_utilization_out_of_range"

	// SanityIssueITLNonPositive indicates AvgITL is zero, negative, or NaN.
	// This prevents adding any (k, ITL) observations from affected pods.
	SanityIssueITLNonPositive SanityIssue = "itl_non_positive"

	// SanityIssueMissingShape indicates AvgOutputTokens or AvgInputTokens is
	// at or below DefaultMinTokensPerRequest, making shape tracking unreliable.
	SanityIssueMissingShape SanityIssue = "missing_shape_metrics"

	// SanityIssueStaleMetrics indicates the replica's metrics are marked stale
	// (Metadata.FreshnessStatus == "stale"). Stale data should not be used
	// for calibration.
	SanityIssueStaleMetrics SanityIssue = "stale_metrics"
)

// SanityReport summarises metric quality issues detected across a set of replica
// metrics during one reconcile cycle.
type SanityReport struct {
	// Issues lists the distinct issue types found. Empty when all metrics are healthy.
	Issues []SanityIssue
	// AffectedPods lists the pod names that had at least one issue.
	AffectedPods []string
}

// OK returns true when no issues were found.
func (r SanityReport) OK() bool {
	return len(r.Issues) == 0
}

// Has returns true when the report contains the given issue type.
func (r SanityReport) Has(issue SanityIssue) bool {
	for _, i := range r.Issues {
		if i == issue {
			return true
		}
	}
	return false
}

// ThroughputVariantState is a read-only snapshot of per-variant state.
// Returned by ThroughputAnalyzer.VariantState for tests and logging.
type ThroughputVariantState struct {
	// Shape is the current workload shape bucket for this variant.
	Shape WorkloadShape
	// ObservationReady is true when the window has enough data for OLS fitting.
	ObservationReady bool
	// KSpread is max_k - min_k over current observations (0 when window is empty).
	KSpread float64
	// SampleCount is the number of observations currently in the window.
	SampleCount int
	// LastSanityReport is the sanity report from the most recent Observe call.
	LastSanityReport SanityReport

	// ITLModel is the ITL model last resolved during Analyze for this variant
	// (tier-1 OLS or tier-2 constrained OLS). IsZero() when no model has been resolved yet.
	ITLModel ITLModel
	// PerReplicaSupply is mean(μ_dec_sat) across replicas from the last Analyze call
	// in tokens/sec. Zero when no supply could be computed.
	PerReplicaSupply float64
	// TotalSupply is Σ μ_dec_sat across replicas from the last Analyze call
	// in tokens/sec. Zero when no supply could be computed.
	TotalSupply float64
	// Demand is λ_dec (total decode token demand) from the last Analyze call
	// in tokens/sec. Zero when no demand signal was available.
	Demand float64
	// Role is the P/D disaggregation role: "prefill", "decode", "both", or ""
	// (non-disaggregated). Populated from VariantStates in Analyze(); empty until first Analyze call.
	Role string

	// LastFittedB is the B coefficient from the most recent successful Tier-1 OLS fit.
	// Zero when no Tier-1 fit has occurred yet (HasFittedB is false in that case).
	LastFittedB float64
	// HasFittedB is true when at least one Tier-1 OLS fit has succeeded for this variant.
	HasFittedB bool
}
