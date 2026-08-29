package domain

import (
	"context"
	"time"
)

// Analyzer is the common interface for all scaling analyzers.
// Each analyzer observes workload metrics and produces capacity signals
// (required_capacity, spare_capacity) that the engine combines into
// scaling decisions. Analyzers do NOT build scaling plans — the engine does.
//
// Saturation Analyzer V2 is the first implementation of this interface.
// Future analyzers (throughput, SLO) will implement the same interface.
type Analyzer interface {
	// Name returns the analyzer's identifier (e.g., "saturation", "throughput", "slo").
	Name() string

	// Analyze computes capacity signals for a model across all its variants.
	// Returns per-variant capacity breakdown and model-level scaling signals.
	Analyze(ctx context.Context, input AnalyzerInput) (*AnalyzerResult, error)
}

// AnalyzerConfig is the interface for analyzer-specific configuration.
// Each analyzer defines its own config type that implements this interface.
type AnalyzerConfig interface {
	// GetAnalyzerName returns the name of the analyzer this config is for.
	GetAnalyzerName() string
}

// AnalyzerInput is the common input provided to all analyzers.
type AnalyzerInput struct {
	ModelID        string
	Namespace      string
	ReplicaMetrics []ReplicaMetrics
	VariantStates  []VariantReplicaState
	Config         AnalyzerConfig

	// SchedulerQueue holds model-level queue metrics from the llm-d inference
	// scheduler flow control layer. These represent requests queued upstream
	// of any pod and are not yet attributed to a specific variant or role.
	// Any analyzer with a demand model may convert this into per-analyzer
	// demand using its own unit (e.g., kv-tokens for saturation_v2,
	// tokens/sec for a future throughput analyzer). Demand attribution to
	// roles or variants is each analyzer's choice.
	// Nil when flow control is disabled or metrics are unavailable.
	SchedulerQueue *SchedulerQueueMetrics

	// ArrivalRate is the model-level request arrival rate (requests/sec) from the
	// llm-d inference scheduler, summed across the whole model with no per-pod
	// labels to reconcile. Zero when the metric is unavailable (EPP absent or no
	// traffic yet). Any analyzer with a demand model may convert this into its
	// own unit (e.g. tokens/sec for the throughput analyzer).
	ArrivalRate float64
}

// SchedulerQueueMetrics holds model-level queue metrics from the llm-d
// inference scheduler flow control layer (inference_extension_flow_control_*).
// These are model-scoped, not per-pod, since the scheduler queues requests
// before routing them to a specific backend pod.
//
// TODO(#2309): The upstream metrics lack a namespace label. If the same model
// name exists in different namespaces, these values may include cross-namespace
// data. Once the upstream adds a namespace label, queries should filter by it.
type SchedulerQueueMetrics struct {
	// QueueSize is the number of requests currently queued in the
	// scheduler's flow control layer for this model.
	// Sourced from inference_extension_flow_control_queue_size.
	QueueSize int64

	// QueueBytes is the total bytes of request bodies currently queued
	// in the scheduler's flow control layer for this model.
	// Sourced from inference_extension_flow_control_queue_bytes.
	// Approximate token count: QueueBytes / BytesPerToken.
	QueueBytes int64
}

// RoleCapacity holds per-role capacity aggregation for P/D disaggregated models.
// Analyzers cannot populate this struct — it does not appear on AnalyzerResult.
// They emit per-role demand as AnalyzerResult.RoleDemand, and the engine's
// capacity-build step pairs that with the per-role supply it derives from the
// variant capacities, writing the result to the engine-owned
// NamedAnalyzerResult.RoleCapacities. Supply is assembled by buildRoleCapacities
// and RequiredCapacity/SpareCapacity by the universal threshold post-step, so a
// zero is a literal value here, never a sentinel.
type RoleCapacity struct {
	Role                   string
	TotalSupply            float64
	TotalDemand            float64
	TotalAnticipatedSupply float64
	RequiredCapacity       float64
	SpareCapacity          float64
}

// AnalyzerResult is the common output produced by all analyzers: the pure
// (D, P) signal and nothing else. Demand is the analyzer's — it owns demand
// attribution, at the model level (TotalDemand) and per role (RoleDemand).
// Per-replica capacity P is the analyzer's too, on each VariantCapacity.
//
// Everything derived from those two — model and per-role supply, utilization,
// and the RequiredCapacity/SpareCapacity scaling signals — belongs to the
// engine's capacity-build step and lives on pipeline.NamedAnalyzerResult, not
// here. An analyzer therefore cannot write a supply or a scaling signal that
// contradicts its own (D, P): the linearity invariant
// (supply = Σ_v replicas × per-replica P) holds by construction.
type AnalyzerResult struct {
	// AnalyzerName identifies which analyzer produced this result.
	AnalyzerName string

	ModelID    string
	Namespace  string
	AnalyzedAt time.Time

	// Per-variant capacity breakdown (in analyzer-specific units).
	VariantCapacities []VariantCapacity

	// TotalDemand is the model-level demand D (in analyzer-specific units).
	// Analyzer-supplied: the analyzer owns demand attribution.
	TotalDemand float64

	// RoleDemand is the analyzer's per-role demand D for P/D disaggregated models
	// (nil when non-disaggregated). It is the demand half of the contract — the
	// analyzer owns demand attribution (e.g. saturation charges waiting per role,
	// throughput charges arrival+queue per role); the capacity builder pairs it
	// with per-role supply to assemble NamedAnalyzerResult.RoleCapacities.
	RoleDemand map[string]float64
}

// VariantCapacity is one variant's entry in an analyzer's (D, P) signal, in
// analyzer-specific units. For saturation: tokens. For throughput: tokens/sec.
// For SLO: latency-constrained capacity.
//
// It carries no variant identity beyond the name that keys it. Cost and
// accelerator are discovery's, and the optimizer reads them from
// VariantMetadata; an analyzer has no business restating them, and when it did,
// the values reached the optimizer only because the capacity builder overlaid
// the authoritative ones back on top.
//
// Role is the exception, and it stays because it is not identity here but the
// key the analyzer attributed demand by: the capacity builder pairs RoleDemand
// with the per-role supply grouped from these entries, so both halves must be
// keyed the same way. The builder still overlays it from discovery so the two
// cannot drift.
type VariantCapacity struct {
	VariantName string
	Role        string // "prefill", "decode", "both", "" (empty = non-disaggregated)

	// ReplicaCount and PendingReplicas are in SCALE-TARGET units (pods, or LWS
	// groups) — the same units as VariantMetadata.CurrentReplicas and as the
	// replica targets the optimizer produces. A pod running data parallelism
	// hosts several engine instances, but the collector merges their metrics
	// into one replica before an analyzer sees them, so there is one unit here
	// and no DP factor for a consumer to reconcile.
	//
	// ReplicaCount is analyzer-measured but not analyzer-final: like Role above,
	// the capacity-build step overlays it from discovery, capping it at
	// VariantMetadata.CurrentReplicas so supply cannot describe a larger fleet
	// than the scale target is committed to. The cap only ever lowers it. See
	// steadystate.clampReplicaCountToScaleTarget for why.
	ReplicaCount    int
	PendingReplicas int

	// PerReplicaCapacity is the representative capacity per replica, in the same
	// scale-target units as ReplicaCount — so demand / PerReplicaCapacity yields
	// a replica target directly.
	// For saturation V2: median(effectiveCapacity) in tokens across ready replicas.
	PerReplicaCapacity float64

	// Reason is a free-text string set by the analyzer to describe how the
	// variant's per-replica capacity was computed. Empty for analyzers that
	// do not set it. Saturation V2 uses "P0-store", "P1-obs", "P2-hist",
	// "P3-k2", "P4-k1", "no-data", "error". Throughput uses "T1-ols",
	// "T2-pinned", "T2-default", "T2-failed".
	Reason string

	// WarmPoolReplicas is how many BRIDGES are serving this variant: warm pool
	// Pods lent to it while it is short, counted in the same scale-target units
	// as ReplicaCount but deliberately NOT part of it.
	//
	// Their load is in TotalDemand, because it is this variant's load. Their
	// capacity is not in supply, because the Pods are borrowed: counting them
	// would tell the optimizer the fleet is already big enough and suppress the
	// scale-up the bridge exists to cover, leaving the pool holding the Pod
	// because the replicas that would release it were never created.
	WarmPoolReplicas int

	// WarmPoolCapacity is what those bridges are worth, in the same units as
	// supply. Measured and carried for the retained-pool switching decision --
	// where there are no ordinary replicas coming and the pool IS the capacity,
	// so choosing which model holds the GPUs needs to compare what each is
	// getting from the pool against what each is asking for.
	WarmPoolCapacity float64

	// TotalDemand is the aggregate demand on this variant. It INCLUDES the
	// demand carried by any bridge above.
	TotalDemand float64

	// Utilization is TotalDemand / (ReplicaCount × PerReplicaCapacity), 0.0-1.0.
	Utilization float64
}
