package config

import (
	"fmt"
	"slices"
	"time"
)

// DefaultPriority is the default model priority multiplier.
// Higher priority → preferential GPU allocation in fair-share.
const DefaultPriority = 1.0

// ScalingPolicy holds saturation-based scaling thresholds for a model variant.
// Saturation scaling is enabled by default and uses these thresholds to determine when
// replicas are saturated and when to scale up.
type ScalingPolicy struct {
	// ModelID is the model identifier (only used in override entries)
	ModelID string `yaml:"model_id,omitempty"`

	// Namespace is the namespace for this override (only used in override entries)
	Namespace string `yaml:"namespace,omitempty"`

	// KvCacheThreshold is the ceiling on usable KV cache (0.0-1.0): a replica's
	// capacity is its physical KV budget scaled by this value, so utilization
	// reaches 1.0 exactly when observed KV occupancy reaches the ceiling. At the
	// default 0.80, "100% utilized" means 80% of KV is resident — the headroom
	// notion EPP saturation uses.
	//
	// NOTE: this is a capacity multiplier, NOT the V1 boolean test it used to be
	// ("saturated if KV utilization >= threshold"). The tuning direction is
	// therefore inverted from V1: a LOWER value shrinks each replica's capacity
	// and scales out sooner, where under V1 a lower value was the aggressive
	// setting for the opposite reason. Configs carried over from V1 need review.
	//
	// It compounds with ScaleUpThreshold — the effective scale-out point is
	// KvCacheThreshold × ScaleUpThreshold of physical KV. Retiring this field in
	// favour of ScaleUpThreshold alone is proposed in
	// docs/plans/analyzers/kvcachethreshold-retirement.md.
	KvCacheThreshold float64 `yaml:"kvCacheThreshold"`

	// QueueLengthThreshold: Replica is saturated if queue length >= this threshold
	QueueLengthThreshold float64 `yaml:"queueLengthThreshold"`

	// EnableRescale: When true, the V2 GPU-constrained optimizer may run the
	// priority-weighted rescale pass — under contention it redistributes the whole
	// budget by priority x demand, reclaiming from lower-priority models so
	// higher-priority work can run. Default is false (additive fair-share only).
	//
	// This is a BUDGET-SCOPE flag, read only from the "default" entry: the value in
	// the global config governs the cluster budget, and a namespace-local config's
	// "default" governs that namespace's quota budget. It is ignored on per-model
	// override entries and has no effect unless a same-scope GPU budget exists
	// (see docs/plans/engine/rescale-alpha.md).
	EnableRescale bool `yaml:"enableRescale,omitempty"`

	// DisableShapeChangeHold turns off the fleet-shape-change hold: the
	// analyzer still tracks the workload's shape, and still keys its
	// throughput windows by it, but a change no longer withholds release or
	// stops the demand floor ordering on an existing reading.
	//
	// The hold is on by default because releasing a fleet on figures a shape
	// change has invalidated is the failure it exists to prevent. It is a
	// switch because it is the one mechanism here that can hold capacity for
	// minutes on its own judgement, and an operator who sees it misbehave
	// needs a way to stop it that does not involve a new image.
	DisableShapeChangeHold bool `yaml:"disableShapeChangeHold,omitempty"`

	// ShapeChangeHoldSeconds overrides how long the fleet is withheld from
	// release after its shape changes, when nothing settles the hold sooner.
	// Zero takes the default.
	//
	// The default is calibrated on a benchmark whose longest generation is
	// 6000 tokens: long enough for one of those plus the rate window that
	// would record it. A deployment serving materially longer generations --
	// long-context or long chain-of-thought -- has a longer "one generation
	// plus a window" than the default allows for, and should raise this
	// rather than inherit a figure measured somewhere else.
	ShapeChangeHoldSeconds int `yaml:"shapeChangeHoldSeconds,omitempty"`

	// AnalyzerName names the saturation analyzer. "saturation" is the only
	// built-in value and selects the token-based analyzer, which is also what an
	// empty value gets — the V1 percentage-based analyzer it used to select was
	// removed, so this no longer chooses between engines. It is retained because
	// it still marks an entry as V2-shaped (see IsV2) and appears on the
	// wva_config_info metric.
	AnalyzerName string `yaml:"analyzerName,omitempty"`

	// ScaleUpThreshold is the utilization threshold above which scale-up is triggered.
	// Applied by the engine post-step universally to every analyzer's result:
	//   requiredCapacity = max(0, totalDemand / ScaleUpThreshold − anticipatedSupply)
	// Default: 0.85 (85% utilization triggers scale-up)
	ScaleUpThreshold float64 `yaml:"scaleUpThreshold,omitempty"`

	// ScaleDownBoundary is the utilization boundary below which scale-down is safe.
	// Applied by the engine post-step universally to every analyzer's result:
	//   spareCapacity = max(0, totalSupply − totalDemand / ScaleDownBoundary)
	// Default: 0.70 (70% utilization allows scale-down)
	ScaleDownBoundary float64 `yaml:"scaleDownBoundary,omitempty"`

	// Priority is a multiplier for this model's scaling urgency.
	// Higher priority → preferential GPU allocation in fair-share.
	// Default: 1.0 (neutral).
	Priority float64 `yaml:"priority,omitempty"`

	// Analyzers configures the set of analyzers and their weights.
	// When empty and AnalyzerName is "saturation", defaults to
	// [{Name: "saturation", Score: 1.0, Enabled: true}].
	//
	// An entry with neither an analyzers section nor analyzerName is not
	// V2-shaped (see IsV2), which now only means the scaling band is left to be
	// resolved post-merge — it no longer routes to a different engine.
	Analyzers []AnalyzerScoreConfig `yaml:"analyzers,omitempty"`

	// ScaleToZero optionally enables/disables scaling to zero replicas for this
	// entry. When set, it overrides the separate wva-model-scale-to-zero-config
	// ConfigMap for the matching model/namespace; when nil, that ConfigMap (and
	// the WVA_SCALE_TO_ZERO env fallback) governs — see ResolveScaleToZeroEnabled.
	ScaleToZero *ScaleToZeroEnvelope `yaml:"scaleToZero,omitempty"`

	// ScaleFromZero optionally tunes how this entry is woken from zero replicas.
	// When nil, the defaults documented on ScaleFromZeroEnvelope apply.
	ScaleFromZero *ScaleFromZeroEnvelope `yaml:"scaleFromZero,omitempty"`

	// AnalyzerDefinitions declares EXTERNAL analyzers — a name, and the PromQL that
	// computes its demand and per-replica target. Honored only on a `default`
	// entry: definitions are a tier-level concern, never a policy one.
	//
	// "Policies select/weight; they don't define"
	// (docs/proposals/wva-keda-external-scaler.md §7.6). A named policy references
	// an analyzer by name and sets `enabled`, `score` and a threshold; it cannot
	// invent the query behind it. Two reasons, and both are about the query rather
	// than about trust: an expensive or high-cardinality one runs against the
	// SHARED Prometheus every cycle, and a wrongly-shaped one (count/avg where a
	// sum is required — see ExternalAnalyzerBody) mis-scales without erroring.
	// Keeping every query in one reviewable place is what makes "what does WVA run
	// against Prometheus?" an answerable question.
	//
	// Internal analyzers are compiled-in Go and are not declared here; a policy
	// references them by name exactly the same way.
	AnalyzerDefinitions ExternalAnalyzerCatalog `yaml:"analyzerDefinitions,omitempty"`

	// DefaultPolicy names the policy tier a variant scales under when its scaler's
	// trigger metadata names none. Honored only on the cluster "default" entry —
	// it is a fleet-wide fallback, so a policy or a per-model entry declaring one
	// would be choosing a default for everyone but itself.
	//
	// Empty means no fallback tier: such a variant scales under the "default" entry
	// alone.
	DefaultPolicy string `yaml:"defaultPolicy,omitempty"`

	// Limiters is the sole source that selects the GPU limiter for the scaling
	// pipeline; it is applied live (no restart) and is honored only on the cluster
	// "default" entry (a budget-scope setting, like EnableRescale). A limiters list
	// selects a single mode — a quota entry wins over any gpu-inventory entry.
	// Each entry is either
	//   - {type: gpu-inventory}  — physical GPU limiter, no quota fields; or
	//   - {type: quota, ...}     — an inline quota entry (see QuotaLimiterConfig).
	// With no limiters declared, the physical-inventory limiter is used.
	// See Config.EffectiveLimiterMode / Config.EffectiveQuotaEntries.
	Limiters []QuotaLimiterConfig `yaml:"limiters,omitempty"`
}

// ScaleToZeroEnvelope is the scale-to-zero policy for a scaling entry, and the
// only per-model surface for it.
//
// It replaced a separate wva-model-scale-to-zero-config ConfigMap that carried
// the same two settings. Having both meant scale-to-zero was configurable in
// three places — here, there, and the WVA_SCALE_TO_ZERO env — with a precedence
// rule to remember and no single answer to "is it on for this model?". Living on
// the scaling entry also gives retention the namespace tiering and per-model
// overrides it never had: the entry is resolved namespace-local → global and
// merged with any per-model override before either field is read. An override is
// identified by the modelID/namespace in its BODY, not by its key: the old
// {modelID}#{namespace} key form is not writable as a ConfigMap key at all (see
// PolicyEntryKey).
type ScaleToZeroEnvelope struct {
	// Enabled turns scale-to-zero on for the entry. A pointer so an absent field
	// (nil) means "inherit": from the merged default entry, and finally from the
	// WVA_SCALE_TO_ZERO deployment flag. Distinguishing that from an explicit
	// false is what lets an override disable scale-to-zero for one model without
	// the default entry's value winning.
	Enabled *bool `yaml:"enabled,omitempty"`

	// RetentionPeriod is how long a model must be idle before it is scaled to
	// zero, and how long a model just woken from zero is held up (the request
	// that woke it is still queued while the pod loads, so the idle signal reads
	// zero for exactly that model). A duration string, e.g. "5m". Empty inherits,
	// ending at DefaultScaleToZeroRetentionPeriod.
	RetentionPeriod string `yaml:"retentionPeriod,omitempty"`
}

// ScaleFromZeroEnvelope is the inline scale-from-zero setting on a scaling entry.
type ScaleFromZeroEnvelope struct {
	// RequirePrefill refuses to wake a P/D-disaggregated model from zero unless a
	// prefill variant can be placed alongside decode.
	//
	// The default (nil / false) wakes decode alone when prefill will not fit,
	// because that still serves: the llm-d router sends the request to a decode
	// endpoint and, with no prefill endpoint selected, the decode worker runs
	// both stages locally. Disaggregation is a TTFT/throughput optimization that
	// the router already skips on its own for short prompts and high cache hits,
	// so refusing to wake would trade a slower model for no model at all.
	//
	// Set true when degraded prefill performance is worse than unavailability
	// for this model — for example when a TTFT SLO matters more than serving at
	// all, or when decode nodes must not absorb prefill load.
	//
	// Pointer so an absent field means "inherit the default" rather than "false",
	// matching ScaleToZeroEnvelope.Enabled.
	RequirePrefill *bool `yaml:"requirePrefill,omitempty"`
}

// RequirePrefillOnScaleFromZero reports whether the entry refuses to wake from
// zero without prefill. Absent config means false — see ScaleFromZeroEnvelope.
func (c ScalingPolicy) RequirePrefillOnScaleFromZero() bool {
	if c.ScaleFromZero == nil || c.ScaleFromZero.RequirePrefill == nil {
		return false
	}
	return *c.ScaleFromZero.RequirePrefill
}

// limiterTypeGPUInventory is the inline Limiters type that selects the physical
// GPU limiter. The other accepted types reuse the canonical LimiterType* values
// ("inventory" alias, "quota"); analyzer types are not restricted to a closed set
// so that externally-registered analyzers (see Engine.RegisterAnalyzer) work.
const limiterTypeGPUInventory = "gpu-inventory"

// AnalyzerScoreConfig configures an individual analyzer's weight in the
// composite scoring function. Per-analyzer threshold overrides are optional;
// when nil, the global top-level thresholds are used.
//
// Type and Parameters implement the ScalingPolicy plugin envelope (Phase 1 of
// the proposal in llm-d/llm-d-workload-variant-autoscaler#1245):
//
//	analyzers:
//	  - type: saturation
//	    parameters: { scaleUpThreshold: 0.95 }
//
// Well-known parameter keys (scaleUpThreshold, scaleDownBoundary, score,
// enabled) are folded into the typed fields below by Normalize(), so downstream
// consumers keep reading the typed fields. Type falls back to Name when unset,
// keeping the legacy `- name: saturation` form working unchanged.
type AnalyzerScoreConfig struct {
	Type              string         `yaml:"type,omitempty"`              // plugin type; falls back to Name
	Name              string         `yaml:"name"`                        //
	Enabled           *bool          `yaml:"enabled,omitempty"`           // default true
	Score             float64        `yaml:"score,omitempty"`             // default 1.0
	ScaleUpThreshold  *float64       `yaml:"scaleUpThreshold,omitempty"`  // overrides global
	ScaleDownBoundary *float64       `yaml:"scaleDownBoundary,omitempty"` // overrides global
	Parameters        map[string]any `yaml:"parameters,omitempty"`        // plugin params; folded by Normalize
}

// EffectiveType returns the analyzer plugin type: the explicit Type when set,
// otherwise the Name. Existing configs use `name:` as both identifier and type,
// so this keeps them working while allowing the new `type:` form.
func (a *AnalyzerScoreConfig) EffectiveType() string {
	if a.Type != "" {
		return a.Type
	}
	return a.Name
}

// Normalize folds well-known keys out of Parameters into the typed fields, so
// downstream consumers keep reading Score/Enabled/ScaleUpThreshold/
// ScaleDownBoundary. A typed field already set (from an explicit top-level key)
// wins over the same key in Parameters. Unknown parameter keys are tolerated.
// Returns an error if a known parameter has the wrong type.
func (a *AnalyzerScoreConfig) Normalize() error {
	if a.Parameters == nil {
		return nil
	}
	if v, ok := a.Parameters["scaleUpThreshold"]; ok && a.ScaleUpThreshold == nil {
		f, err := paramFloat("scaleUpThreshold", v)
		if err != nil {
			return err
		}
		a.ScaleUpThreshold = &f
	}
	if v, ok := a.Parameters["scaleDownBoundary"]; ok && a.ScaleDownBoundary == nil {
		f, err := paramFloat("scaleDownBoundary", v)
		if err != nil {
			return err
		}
		a.ScaleDownBoundary = &f
	}
	if v, ok := a.Parameters["score"]; ok && a.Score == 0 {
		f, err := paramFloat("score", v)
		if err != nil {
			return err
		}
		a.Score = f
	}
	if v, ok := a.Parameters["enabled"]; ok && a.Enabled == nil {
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("analyzer %q: parameter \"enabled\" must be a boolean, got %T", a.EffectiveType(), v)
		}
		a.Enabled = &b
	}
	return nil
}

// paramFloat coerces a YAML-decoded plugin parameter to float64. yaml.v3 decodes
// integers as int and decimals as float64 when the target is interface{}, so both
// are accepted.
func paramFloat(key string, v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	default:
		return 0, fmt.Errorf("parameter %q must be a number, got %T", key, v)
	}
}

// EffectiveScaleUpThreshold returns the per-analyzer threshold if set,
// otherwise falls back to the global value.
func (a *AnalyzerScoreConfig) EffectiveScaleUpThreshold(global float64) float64 {
	if a.ScaleUpThreshold != nil {
		return *a.ScaleUpThreshold
	}
	return global
}

// EffectiveScaleDownBoundary returns the per-analyzer boundary if set,
// otherwise falls back to the global value.
func (a *AnalyzerScoreConfig) EffectiveScaleDownBoundary(global float64) float64 {
	if a.ScaleDownBoundary != nil {
		return *a.ScaleDownBoundary
	}
	return global
}

// GetAnalyzerName implements the AnalyzerConfig interface.
// Returns "saturation" if Analyzers list is populated (new-style config),
// otherwise returns the raw AnalyzerName field (backward compat).
func (c *ScalingPolicy) GetAnalyzerName() string {
	if len(c.Analyzers) > 0 {
		return "saturation"
	}
	return c.AnalyzerName
}

// IsV2 reports whether the entry is V2-shaped: it populates the Analyzers list
// (new-style) or sets AnalyzerName to "saturation" (old-style). Since the V1
// analyzer was removed this no longer selects an engine — the token-based
// analyzer runs either way. It survives only as the gate on defaulting the
// scaling band in ApplyDefaults, so a partial entry does not clobber a tuned
// global value during Merge.
func (c *ScalingPolicy) IsV2() bool {
	return len(c.Analyzers) > 0 || c.AnalyzerName == "saturation"
}

// Capacity-sizing default thresholds, applied when fields are omitted from YAML config.
const (
	DefaultKvCacheThreshold     = 0.80
	DefaultQueueLengthThreshold = 5.0
)

// V2 analyzer default thresholds, applied when fields are omitted from YAML config.
const (
	DefaultScaleUpThreshold  = 0.85
	DefaultScaleDownBoundary = 0.70
)

// Normalize folds each analyzer's Parameters into its typed fields. Call it at
// parse time, before ApplyDefaults() and Validate(), so defaulting and validation
// see the folded values.
func (c *ScalingPolicy) Normalize() error {
	for i := range c.Analyzers {
		if err := c.Analyzers[i].Normalize(); err != nil {
			return err
		}
	}
	return nil
}

// ApplyDefaults fills in zero-valued fields with their defaults.
// The capacity thresholds (KvCacheThreshold, QueueLengthThreshold) are always
// defaulted when zero — the analyzer needs them to size per-replica capacity.
// The scaling band (ScaleUpThreshold, ScaleDownBoundary) and the Analyzers list
// are defaulted only for V2-shaped configs, so that an entry which does not set
// them keeps them zero — this is deliberate: entries are ApplyDefaults()'d
// individually at parse time, and defaulting the band on a partial override
// would clobber a tuned global value during Merge(). The final resolved config
// is instead calibrated post-merge via ApplyV2ThresholdDefaults(). Must be
// called before Validate() to handle omitempty zero-values correctly.
func (c *ScalingPolicy) ApplyDefaults() {
	if c.Priority == 0 {
		c.Priority = DefaultPriority
	}
	// Capacity thresholds — always defaulted; the analyzer needs them to size capacity.
	if c.KvCacheThreshold == 0 {
		c.KvCacheThreshold = DefaultKvCacheThreshold
	}
	if c.QueueLengthThreshold == 0 {
		c.QueueLengthThreshold = DefaultQueueLengthThreshold
	}
	if c.IsV2() {
		if c.ScaleUpThreshold == 0 {
			c.ScaleUpThreshold = DefaultScaleUpThreshold
		}
		if c.ScaleDownBoundary == 0 {
			c.ScaleDownBoundary = DefaultScaleDownBoundary
		}
		// Default analyzers list when empty (backward compat for analyzerName: "saturation")
		if len(c.Analyzers) == 0 {
			enabled := true
			c.Analyzers = []AnalyzerScoreConfig{
				{Name: "saturation", Score: 1.0, Enabled: &enabled},
			}
		}
		// Apply per-entry defaults
		for i := range c.Analyzers {
			if c.Analyzers[i].Score == 0 {
				c.Analyzers[i].Score = 1.0
			}
			if c.Analyzers[i].Enabled == nil {
				enabled := true
				c.Analyzers[i].Enabled = &enabled
			}
		}
	}
}

// ApplyV2ThresholdDefaults fills zero-valued V2 thresholds (ScaleUpThreshold,
// ScaleDownBoundary) with their defaults regardless of IsV2(). Call this only on a
// FINAL resolved config (after base + override Merge), never on an individual stored
// entry: a partial per-model/namespace entry that omits the band must still be
// calibrated once merged — but defaulting these fields on the stored entry would
// clobber a tuned global threshold during Merge().
func (c *ScalingPolicy) ApplyV2ThresholdDefaults() {
	if c.ScaleUpThreshold == 0 {
		c.ScaleUpThreshold = DefaultScaleUpThreshold
	}
	if c.ScaleDownBoundary == 0 {
		c.ScaleDownBoundary = DefaultScaleDownBoundary
	}
}

// Merge overlays non-zero fields from override onto c.
// This allows per-model overrides to specify only the fields they want to change,
// inheriting all other values from the base (typically the "default" config).
func (c *ScalingPolicy) Merge(override ScalingPolicy) {
	if override.KvCacheThreshold != 0 {
		c.KvCacheThreshold = override.KvCacheThreshold
	}
	if override.QueueLengthThreshold != 0 {
		c.QueueLengthThreshold = override.QueueLengthThreshold
	}
	if override.AnalyzerName != "" {
		c.AnalyzerName = override.AnalyzerName
	}
	if override.ScaleUpThreshold != 0 {
		c.ScaleUpThreshold = override.ScaleUpThreshold
	}
	if override.ScaleDownBoundary != 0 {
		c.ScaleDownBoundary = override.ScaleDownBoundary
	}
	if override.Priority != 0 {
		c.Priority = override.Priority
	}
	if len(override.Analyzers) > 0 {
		c.Analyzers = override.Analyzers
	}
	// Merged FIELD BY FIELD, not wholesale. The envelope carries two independent
	// settings, so replacing it would make an override that sets only
	// retentionPeriod silently discard an inherited enabled (and vice versa) —
	// which is the partial-override case both fields document.
	if override.ScaleToZero != nil {
		merged := ScaleToZeroEnvelope{}
		if c.ScaleToZero != nil {
			merged = *c.ScaleToZero
		}
		if override.ScaleToZero.Enabled != nil {
			merged.Enabled = override.ScaleToZero.Enabled
		}
		if override.ScaleToZero.RetentionPeriod != "" {
			merged.RetentionPeriod = override.ScaleToZero.RetentionPeriod
		}
		c.ScaleToZero = &merged
	}
	if override.ScaleFromZero != nil {
		c.ScaleFromZero = override.ScaleFromZero
	}
	// Limiters is intentionally NOT merged: it is a cluster-"default"-scope setting
	// read only from the global default entry (see Config.EffectiveLimiterMode), so
	// a per-model override's limiters would never be consumed.
	if override.ModelID != "" {
		c.ModelID = override.ModelID
	}
	if override.Namespace != "" {
		c.Namespace = override.Namespace
	}
}

// Validate checks for invalid threshold values.
// Returns error with descriptive message if validation fails.
// Call ApplyDefaults() before Validate() to handle zero-valued omitempty fields.
func (c *ScalingPolicy) Validate() error {
	if c.KvCacheThreshold < 0 || c.KvCacheThreshold > 1 {
		return fmt.Errorf("kvCacheThreshold must be between 0 and 1, got %.2f", c.KvCacheThreshold)
	}
	if c.QueueLengthThreshold < 0 {
		return fmt.Errorf("queueLengthThreshold must be >= 0, got %.1f", c.QueueLengthThreshold)
	}
	if c.Priority < 0 {
		return fmt.Errorf("priority must be >= 0, got %.2f", c.Priority)
	}

	// V2 threshold range/consistency checks apply whenever the fields are set,
	// regardless of IsV2(): a partial per-model or namespace entry's explicit
	// scaleUpThreshold/scaleDownBoundary can still be merged onto the default and
	// consumed by the analyzer. Zero means "unset"
	// (defaulted elsewhere), so it is skipped here.
	if c.ScaleUpThreshold != 0 && (c.ScaleUpThreshold < 0 || c.ScaleUpThreshold > 1) {
		return fmt.Errorf("scaleUpThreshold must be in (0, 1], got %.2f", c.ScaleUpThreshold)
	}
	if c.ScaleDownBoundary != 0 && (c.ScaleDownBoundary < 0 || c.ScaleDownBoundary > 1) {
		return fmt.Errorf("scaleDownBoundary must be in (0, 1], got %.2f", c.ScaleDownBoundary)
	}
	if c.ScaleUpThreshold != 0 && c.ScaleDownBoundary != 0 && c.ScaleUpThreshold <= c.ScaleDownBoundary {
		return fmt.Errorf("scaleUpThreshold (%.2f) must be > scaleDownBoundary (%.2f)", c.ScaleUpThreshold, c.ScaleDownBoundary)
	}

	// V2 analyzer per-analyzer overrides (only meaningful for a V2 config).
	if c.IsV2() {
		// A V2 config must have its thresholds set — ApplyDefaults fills them, so a
		// zero here means the caller skipped ApplyDefaults, which is invalid. (The
		// upper-bound and relational checks are handled by the non-zero global checks
		// above; partial entries are only range-checked there when non-zero.)
		if c.ScaleUpThreshold <= 0 {
			return fmt.Errorf("scaleUpThreshold must be in (0, 1], got %.2f", c.ScaleUpThreshold)
		}
		if c.ScaleDownBoundary <= 0 {
			return fmt.Errorf("scaleDownBoundary must be in (0, 1], got %.2f", c.ScaleDownBoundary)
		}

		// Per-analyzer threshold overrides. Analyzer types are intentionally NOT
		// restricted to a closed set: an unrecognized type is ignored at runtime
		// (no registered analyzer matches it), and rejecting it here would drop the
		// whole entry — breaking the Engine.RegisterAnalyzer extension path and
		// forward-compat with analyzers added in a newer release.
		for _, a := range c.Analyzers {
			if a.ScaleUpThreshold != nil {
				if *a.ScaleUpThreshold <= 0 || *a.ScaleUpThreshold > 1 {
					return fmt.Errorf("analyzer %q: scaleUpThreshold must be in (0, 1], got %.2f", a.EffectiveType(), *a.ScaleUpThreshold)
				}
			}
			if a.ScaleDownBoundary != nil {
				if *a.ScaleDownBoundary <= 0 || *a.ScaleDownBoundary > 1 {
					return fmt.Errorf("analyzer %q: scaleDownBoundary must be in (0, 1], got %.2f", a.EffectiveType(), *a.ScaleDownBoundary)
				}
			}
			up := a.EffectiveScaleUpThreshold(c.ScaleUpThreshold)
			down := a.EffectiveScaleDownBoundary(c.ScaleDownBoundary)
			if up <= down {
				return fmt.Errorf("analyzer %q: scaleUpThreshold (%.2f) must be > scaleDownBoundary (%.2f)", a.EffectiveType(), up, down)
			}
		}
	}

	if err := c.validateLimiters(); err != nil {
		return err
	}

	return nil
}

// validateLimiters checks the inline Limiters list. gpu-inventory/inventory
// entries must carry no quota fields; quota entries are validated end-to-end by
// the existing QuotaLimiterEntries.Validate() (name uniqueness, scope, per-type
// ranges), so the inline and file-based quota schemas stay identical.
func (c *ScalingPolicy) validateLimiters() error {
	if len(c.Limiters) == 0 {
		return nil
	}
	var quotaOnes []QuotaLimiterConfig
	for i, l := range c.Limiters {
		switch l.Type {
		case limiterTypeGPUInventory, string(LimiterTypeInventory):
			if l.Scope != "" || len(l.ClusterQuotas) > 0 || len(l.NamespaceQuotas) > 0 || len(l.Exclude) > 0 || l.Kueue != nil {
				return fmt.Errorf("limiters[%d] (type %q): must not set quota fields (scope/quotas/namespaceQuotas/exclude/kueue)", i, l.Type)
			}
		case string(LimiterTypeQuota):
			quotaOnes = append(quotaOnes, l)
		default:
			return fmt.Errorf("limiters[%d]: unknown limiter type %q (valid: %q, %q, %q)",
				i, l.Type, limiterTypeGPUInventory, LimiterTypeInventory, LimiterTypeQuota)
		}
	}
	if len(quotaOnes) > 0 {
		entries := QuotaLimiterEntries{Limiters: quotaOnes}
		if _, err := entries.Validate(); err != nil {
			return fmt.Errorf("limiters: %w", err)
		}
	}
	return nil
}

// UnenforcedLimiterTypes returns the limiter types this policy declares that
// will NOT be built, in declaration order and de-duplicated. Empty when
// everything declared is enforced.
//
// The limiters: list reads like a set of bounds that all apply. It is not:
// Config.EffectiveLimiterMode collapses it to ONE mode — quota if any entry is
// quota — and NewLimiterFromConfig builds only that. So a policy declaring both
// quota and gpu-inventory gets the quota caps and NO physical limiter at all,
// which is the dangerous direction: the operator reads the config as "bounded by
// real GPUs as well" while nothing consults physical capacity, and a
// scale-from-zero wake can be placed onto an accelerator that is already full.
//
// Bounding by min(physical, quota) is issue #1003. Until that exists, the drop
// has to be visible — validateLimiters deliberately does not reject the
// combination, because refusing a config that used to install is a worse failure
// than one that says what it is doing.
func (c ScalingPolicy) UnenforcedLimiterTypes() []string {
	if len(c.Limiters) == 0 {
		return nil
	}
	hasQuota := false
	for _, l := range c.Limiters {
		if l.Type == string(LimiterTypeQuota) {
			hasQuota = true
			break
		}
	}
	if !hasQuota {
		// Every entry is a physical one, and the inventory limiter is built.
		return nil
	}
	var dropped []string
	for _, l := range c.Limiters {
		if l.Type == string(LimiterTypeQuota) || slices.Contains(dropped, l.Type) {
			continue
		}
		dropped = append(dropped, l.Type)
	}
	return dropped
}

// AnalyzerScore returns the AnalyzerScoreConfig.Score for the named analyzer,
// defaulting to 1.0 when the analyzer has no explicit entry in c.Analyzers.
// This value is the per-analyzer weight used by GreedyByScoreOptimizer for
// fair-share priority ordering across models.
func (c ScalingPolicy) AnalyzerScore(analyzerName string) float64 {
	for _, aw := range c.Analyzers {
		if aw.EffectiveType() == analyzerName {
			if aw.Score > 0 {
				return aw.Score
			}
			return 1.0
		}
	}
	return 1.0
}

// AnalyzerThresholds returns the scale-up/scale-down band the named analyzer
// votes against: its own per-analyzer override where it declares one, and the
// policy's band otherwise.
func (c ScalingPolicy) AnalyzerThresholds(analyzerName string) (scaleUp, scaleDown float64) {
	for _, aw := range c.Analyzers {
		if aw.EffectiveType() == analyzerName {
			return aw.EffectiveScaleUpThreshold(c.ScaleUpThreshold),
				aw.EffectiveScaleDownBoundary(c.ScaleDownBoundary)
		}
	}
	return c.ScaleUpThreshold, c.ScaleDownBoundary
}

// AnalyzerEnabled reports whether the named analyzer should participate in this
// cycle's scaling decision. An analyzer is opt-in: it participates only when it has
// an explicit entry in c.Analyzers whose Enabled is true (or nil, i.e. present but
// not yet defaulted). An analyzer registered in code but ABSENT from c.Analyzers
// does NOT participate — this prevents a registered-but-unconfigured analyzer (e.g.
// throughput) from returning SpareCapacity=0 and silently vetoing scale-down.
// Saturation is exempt: it is guarded by the SaturationAnalyzerName check upstream
// (engine_v2.go ~L136) before AnalyzerEnabled is ever called.
func (c ScalingPolicy) AnalyzerEnabled(analyzerName string) bool {
	for _, aw := range c.Analyzers {
		if aw.EffectiveType() == analyzerName {
			if aw.Enabled != nil {
				return *aw.Enabled
			}
			return true // present, not yet defaulted → participates
		}
	}
	return false // absent → opt-in: does not participate
}

// ShapeChangeHold returns how long a fleet-shape change withholds release
// before the backstop expires it, and whether the hold is enabled at all.
//
// The duration is ShapeChangeHoldSeconds when set and the caller's default
// otherwise, so the default lives beside the analyzer that justifies it rather
// than here.
func (p ScalingPolicy) ShapeChangeHold(defaultHold time.Duration) (time.Duration, bool) {
	if p.DisableShapeChangeHold {
		return 0, false
	}
	if p.ShapeChangeHoldSeconds > 0 {
		return time.Duration(p.ShapeChangeHoldSeconds) * time.Second, true
	}
	return defaultHold, true
}
