package accelerator

import (
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// NormalizeAcceleratorName converts a full GPU model name to a short name.
// This enables matching between declared names (e.g., "A100") and discovery
// results (e.g., "NVIDIA-A100-PCIE-80GB").
//
// Examples:
//   - "NVIDIA-A100-PCIE-80GB" -> "A100"
//   - "NVIDIA-H100-SXM5-80GB" -> "H100"
//   - "AMD-MI300X-192G" -> "MI300X"
//   - "Intel-Gaudi-2-96GB" -> "Gaudi-2"
//   - "A100" -> "A100" (already short)
func NormalizeAcceleratorName(fullName string) string {
	// If already a short name (no hyphens or known pattern), return as-is
	if !strings.Contains(fullName, "-") {
		return fullName
	}

	// Common patterns for GPU model names:
	// NVIDIA-{model}-{variant} -> extract {model}
	// AMD-{model}-{memory} -> extract {model}
	// Intel-{model}-{memory} -> extract {model}

	parts := strings.Split(fullName, "-")
	if len(parts) < 2 {
		return fullName
	}

	// Check for known vendor prefixes
	vendor := strings.ToUpper(parts[0])
	switch vendor {
	case "NVIDIA":
		// NVIDIA-A100-PCIE-80GB -> A100
		// NVIDIA-H100-SXM5-80GB -> H100
		if len(parts) >= 2 {
			return parts[1]
		}
	case "AMD":
		// AMD-MI300X-192G -> MI300X
		if len(parts) >= 2 {
			return parts[1]
		}
	case "INTEL":
		// Intel-Gaudi-2-96GB -> Gaudi-2
		if len(parts) >= 3 {
			return parts[1] + "-" + parts[2]
		}
		if len(parts) >= 2 {
			return parts[1]
		}
	}

	// Fallback: return the second part (after vendor)
	return parts[1]
}

// GetProductKeys returns unique vendor product (node label) keys, in stable (sorted) order
func GetProductKeys() []string {
	labels := make(map[string]bool, len(constants.VendorResources))
	for _, res := range constants.VendorResources {
		labels[res.ProductLabel] = true
		for _, label := range res.ProductLabelAliases {
			labels[label] = true
		}
	}
	return slices.Sorted(maps.Keys(labels))
}

// GetAcceleratorNameFromScaleTarget extracts GPU product information from a scale
// target's nodeSelector or nodeAffinity, checked against the keys listed in
// constants.VendorResources. Returns the first matching value, or
// constants.DefaultAcceleratorName ("unknown") when the workload constrains
// nothing.
//
// Placement constraints are the ONLY source, deliberately. An
// inference.optimization/acceleratorName label used to serve as a fallback, and it
// was unsound: a workload whose pod spec does not constrain placement can be
// scheduled onto any GPU node, so the label asserted a type nothing enforced. On a
// heterogeneous cluster that is not a corner case but the expected outcome, and it
// split the accounting in two — the physical usage view attributes by the node a
// pod actually runs on, while the population view believed the label, so a
// mislabelled workload was billed to one accelerator's quota while occupying
// another. Where the label was RIGHT it merely repeated the nodeSelector.
//
// "unknown" is therefore the honest answer for an unconstrained workload, and it
// is read as "any accelerator can serve this": a single-type cluster deduces the
// type outright (TypeInventory.SetUsed), and the placement check asks whether ANY
// pool has room rather than guessing one (FitsGPUBudget). Deriving the type from
// the node a running variant's pods landed on would resolve it exactly, and is
// not implemented — see docs/concepts/gpu-capacity-accounting.md.
func GetAcceleratorNameFromScaleTarget(_ *llmdVariantAutoscalingV1alpha1.VariantAutoscaling, scaleTarget scaletarget.ScaleTargetAccessor) string {
	// Check scaleTarget for accelerator name if it's not nil
	if scaleTarget != nil {
		podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
		if podTemplateSpec != nil {
			prodKeys := GetProductKeys()

			// Check nodeSelector first
			if podTemplateSpec.Spec.NodeSelector != nil {
				for _, key := range prodKeys {
					if val, ok := podTemplateSpec.Spec.NodeSelector[key]; ok {
						return val
					}
				}
			}

			// Check nodeAffinity
			if podTemplateSpec.Spec.Affinity != nil && podTemplateSpec.Spec.Affinity.NodeAffinity != nil {
				if val := extractGPUFromNodeAffinity(podTemplateSpec.Spec.Affinity.NodeAffinity, prodKeys); val != "" {
					return val
				}
			}
		}
	}

	return constants.DefaultAcceleratorName
}

// extractGPUFromNodeAffinity extracts GPU product information from NodeAffinity.
// It checks both required and preferred node affinity terms for the given GPU keys.
func extractGPUFromNodeAffinity(nodeAffinity *corev1.NodeAffinity, gpuKeys []string) string {
	// Check required node affinity
	if nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		for _, term := range nodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if val := extractGPUFromNodeSelectorTerm(term, gpuKeys); val != "" {
				return val
			}
		}
	}

	// Check preferred node affinity
	if nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
		for _, preferred := range nodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			if val := extractGPUFromNodeSelectorTerm(preferred.Preference, gpuKeys); val != "" {
				return val
			}
		}
	}

	return ""
}

// extractGPUFromNodeSelectorTerm extracts GPU product from a NodeSelectorTerm.
// It checks MatchExpressions for the given GPU keys with "In" or "Exists" operators.
func extractGPUFromNodeSelectorTerm(term corev1.NodeSelectorTerm, gpuKeys []string) string {
	for _, expr := range term.MatchExpressions {
		for _, key := range gpuKeys {
			if expr.Key == key {
				// For "In" operator, return the first value
				if expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 {
					return expr.Values[0]
				}
				// For "Exists" operator, we found the key but no specific value
				// Continue searching for other keys that might have values
			}
		}
	}
	return ""
}
