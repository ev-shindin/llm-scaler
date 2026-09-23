package policy

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// ChangeReporter is the reporting for named policy tiers.
//
// Layered configuration is undebuggable without a "which value won" readout, and
// two of the ways it goes wrong are silent by construction:
//
//   - a policy name that resolves to nothing falls back to the default entry, so a
//     typo produces a working-looking configuration that quietly ignores the tier;
//   - variants of one model resolving to DIFFERENT tiers leaves the optimizer
//     distributing replicas across them under conflicting thresholds. WVA scales
//     the model, not the variant, so there is one right answer per model and this
//     is a configuration error rather than a merge to perform.
//
// Both are reported once per change rather than once per cycle: the optimize loop
// runs every 15s and a condition that persists for a day would otherwise produce
// 5,760 identical lines, which is how a real signal becomes noise.
//
// Safe for concurrent use: the reporter holds its own lock.
type ChangeReporter struct {
	mu   sync.Mutex
	seen map[string]string
}

// NewChangeReporter returns a reporter that has said nothing yet.
func NewChangeReporter() *ChangeReporter {
	return &ChangeReporter{seen: make(map[string]string)}
}

// changed reports whether what was last said about key differs from summary,
// recording it either way.
//
// A nil reporter reports nothing, which is what makes it safe to leave uninjected
// in tests that construct an Engine directly — the same posture UsageRefresher
// takes. Production builds one in NewEngine.
func (p *ChangeReporter) changed(key, summary string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen[key] == summary {
		return false
	}
	p.seen[key] = summary
	return true
}

// ReportUnknownPolicy warns that a variant named a policy tier that does not
// exist, so it is scaling under the default entry instead.
//
// Falling back is the right behaviour — refusing to scale a workload because its
// policy name is misspelled would turn a config typo into an outage — but doing it
// quietly is not: the ScaledObject reads as tiered and nothing about the outcome
// says otherwise.
func (p *ChangeReporter) ReportUnknownPolicy(ctx context.Context, namespace, variant, policy string, known []string) {
	if !p.changed(namespace+"/"+variant, "unknown|"+policy) {
		return
	}
	ctrl.LoggerFrom(ctx).Info(
		"Scaling policy not found; this variant is scaling under the default entry. "+
			"Check the scalingPolicy trigger metadata against the policy tiers declared "+
			"in the saturation ConfigMap.",
		"namespace", namespace, "variant", variant,
		"scalingPolicy", policy, "knownPolicies", known)
}

// ReportPolicyConflict warns that one model's variants named different policy
// tiers.
//
// WVA scales a MODEL — the optimizer distributes replicas across that model's
// variants against one set of thresholds — so a model has exactly one effective
// policy. Two tiers means the optimizer is balancing variants that disagree about
// what saturated means, which produces a stable-looking allocation that is wrong
// for at least one of them.
func (p *ChangeReporter) ReportPolicyConflict(ctx context.Context, namespace, modelID string, policies []string, chosen string) {
	if !p.changed(namespace+"|"+modelID, "conflict|"+chosen+"|"+joinSorted(policies)) {
		return
	}
	ctrl.LoggerFrom(ctx).Info(
		"A model's variants name different scaling policies; scaling the model under one of them. "+
			"A model has one effective policy — the optimizer distributes its replicas across the "+
			"variants against a single set of thresholds — so give every variant of this model the "+
			"same scalingPolicy.",
		"namespace", namespace, "modelID", modelID,
		"policies", policies, "using", chosen)
}

// ReportEffectivePolicy records which tier a model ended up scaling under, once
// per change. This is the "which value won" readout: with a default entry, a tier
// and a per-model override all contributing, the resolved thresholds are not
// derivable from any single one of them.
func (p *ChangeReporter) ReportEffectivePolicy(ctx context.Context, namespace, modelID, policy string, cfg config.ScalingPolicy) {
	name := policy
	if name == "" {
		name = "(default entry)"
	}
	summary := name + "|" + formatBand(cfg)
	if !p.changed("effective|"+namespace+"|"+modelID, summary) {
		return
	}
	ctrl.LoggerFrom(ctx).Info("Effective scaling policy",
		"namespace", namespace, "modelID", modelID, "scalingPolicy", name,
		"scaleUpThreshold", cfg.ScaleUpThreshold, "scaleDownBoundary", cfg.ScaleDownBoundary,
		"kvCacheThreshold", cfg.KvCacheThreshold, "priority", cfg.Priority)
}

// formatBand renders the fields that make two resolutions meaningfully different,
// so the readout fires on a real change rather than on a re-parse.
func formatBand(cfg config.ScalingPolicy) string {
	return fmt.Sprintf("%.3f|%.3f|%.3f|%.3f",
		cfg.ScaleUpThreshold, cfg.ScaleDownBoundary, cfg.KvCacheThreshold, cfg.Priority)
}

// joinSorted renders a deterministic list, so a set of policies reported in a
// different map order is not mistaken for a change.
func joinSorted(values []string) string {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return strings.Join(sorted, ",")
}

// ReportUnresolvedAccelerator warns that a variant's accelerator could not be
// resolved, and says what that actually costs under the current configuration.
//
// This is a LOG line as well as a Kubernetes event. It was written when the event
// did not arrive at all: variants are synthesized in memory and their kind is not
// registered in the scheme, so the recorder could not build an object reference
// for one ("no kind is registered for the type variant.VariantAutoscaling"), and a
// single e2e run attempted 31 of these warnings for which the API server received
// none. That is fixed — events hang on the ScaledObject now, see
// variant.EventTarget — so the two are complementary rather than one standing
// in for the other. They fail in opposite directions: the event is re-emitted while
// the condition holds and the API server aggregates the repeats, whereas this line
// is printed once per change (p.changed, below) and can scroll out of a long-lived
// controller's log. The event is the durable signal; a grep of the log corroborates
// it and cannot clear it.
//
// The consequence depends on whether a limiter is declared, and the difference is
// severe enough to be worth saying rather than leaving to be inferred:
//
//   - no limiter: the variant still scales. Its GPUs are charged to no accelerator
//     pool, so budgets over-state free capacity (wva_unattributed_gpus), and
//     accelerator-keyed metrics are withheld;
//   - a limiter declared: a GPU-aware optimizer allocates out of per-accelerator
//     pools, so this variant is charged to none, receives no budget, and NEVER
//     SCALES UP. Nothing errors; it simply sits at its current replica count.
//
// Reported once per change per variant, so a persistent misconfiguration does not
// print every cycle.
func (p *ChangeReporter) ReportUnresolvedAccelerator(ctx context.Context, namespace, variant, limiterMode string) {
	limited := limiterMode != "" && limiterMode != string(config.LimiterTypeNone)
	if !p.changed("accel|"+namespace+"/"+variant, strconv.FormatBool(limited)) {
		return
	}

	logger := ctrl.LoggerFrom(ctx)
	if limited {
		logger.Info("Accelerator not resolved and a GPU limiter is declared: this variant will "+
			"NOT be allocated any GPU budget and therefore will not scale up. Set a GPU product "+
			"key in the workload's nodeSelector or nodeAffinity.",
			"namespace", namespace, "variant", variant, "limiter", limiterMode)
		return
	}
	logger.Info("Accelerator not resolved: this variant's GPUs are charged to no accelerator "+
		"pool, so GPU budgets over-state free capacity by that amount (wva_unattributed_gpus) "+
		"and accelerator-keyed saturation/capacity metrics are withheld. Replica scaling metrics "+
		"still carry accelerator_type=\"unresolved\". Set a GPU product key in the workload's "+
		"nodeSelector or nodeAffinity. NOTE: declaring a GPU limiter while this is unresolved "+
		"would stop the variant scaling up entirely.",
		"namespace", namespace, "variant", variant)
}
