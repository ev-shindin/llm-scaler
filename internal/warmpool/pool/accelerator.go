package pool

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// AcceleratorOf reports the GPU model a node carries, or "" if it declares none.
//
// Vendors are walked in REVERSE order and the first match wins, matching
// gpunodes.discoverNodeGPUTypes so a multi-vendor node resolves to the same
// accelerator here as it does in capacity accounting. Two answers for one node
// would be worse than either.
func AcceleratorOf(node *corev1.Node) string {
	for i := len(constants.VendorResources) - 1; i >= 0; i-- {
		vendor := constants.VendorResources[i]
		if model, ok := node.Labels[vendor.ProductLabel]; ok && model != "" {
			return model
		}
		for _, alias := range vendor.ProductLabelAliases {
			if model, ok := node.Labels[alias]; ok && model != "" {
				return model
			}
		}
	}
	return ""
}

// AcceleratorRequiredBy reports the GPU model a workload demands, or "" if it
// demands none in particular.
//
// Both forms are read, and reading only one is the trap. llm-d's modelservice
// chart pins the accelerator with nodeAffinity:
//
//	affinity:
//	  nodeAffinity:
//	    requiredDuringSchedulingIgnoredDuringExecution:
//	      nodeSelectorTerms:
//	      - matchExpressions:
//	        - key: nvidia.com/gpu.product
//	          operator: In
//	          values: [NVIDIA-H100-80GB-HBM3]
//
// and writes NO nodeSelector at all -- verified on a live deployment. A check
// that consulted nodeSelector alone would find nothing, conclude the workload
// accepts any accelerator, and never fire on the layout it exists to protect.
//
// Only a REQUIRED term counts. Preferred affinity is a hint the scheduler may
// ignore, so a warm copy placed on the strength of one could be wrong; and only
// a single-valued In or Equals is a requirement this can act on -- a term
// listing three acceptable models says the workload is portable between them,
// which is not a constraint the pool needs to enforce.
func AcceleratorRequiredBy(spec *corev1.PodSpec) string {
	for _, key := range acceleratorLabelKeys() {
		if model, ok := spec.NodeSelector[key]; ok && model != "" {
			return model
		}
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil {
		return ""
	}
	required := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if required == nil {
		return ""
	}
	keys := acceleratorLabelKeys()
	for _, term := range required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if len(expr.Values) != 1 {
				continue
			}
			if expr.Operator != corev1.NodeSelectorOpIn && expr.Operator != corev1.NodeSelectorOpNotIn {
				continue
			}
			if expr.Operator == corev1.NodeSelectorOpNotIn {
				continue // says what it will not take, not what it needs
			}
			for _, key := range keys {
				if expr.Key == key {
					return expr.Values[0]
				}
			}
		}
	}
	return ""
}

// acceleratorLabelKeys is every node label key that names a GPU model.
func acceleratorLabelKeys() []string {
	var keys []string
	for i := len(constants.VendorResources) - 1; i >= 0; i-- {
		vendor := constants.VendorResources[i]
		keys = append(keys, vendor.ProductLabel)
		keys = append(keys, vendor.ProductLabelAliases...)
	}
	return keys
}

// AcceleratorMismatch reports whether a warm copy of a model that requires
// `wanted` would be useless in a Pod holding `held`, and why.
//
// Only a PROVEN mismatch blocks. An unknown on either side allows: the pool
// cannot read the node in an install without cluster RBAC, and refusing every
// admission there would disable the pool silently, which is the failure this
// whole configuration surface exists to stop. A warm copy on the wrong
// accelerator costs one wasted load; a pool that quietly warms nothing costs
// every GPU it holds, forever.
func AcceleratorMismatch(wanted, held string) (string, bool) {
	if wanted == "" || held == "" || wanted == held {
		return "", false
	}
	return "needs " + wanted + ", this Pod is on " + held, true
}
