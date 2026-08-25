package pool

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAcceleratorComesFromTheNodesProductLabel(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
	}}}
	if got := AcceleratorOf(node); got != "NVIDIA-H100-80GB-HBM3" {
		t.Fatalf("got %q", got)
	}
	if got := AcceleratorOf(&corev1.Node{}); got != "" {
		t.Errorf("a node with no GPU label declares nothing, got %q", got)
	}
}

func TestAcceleratorIsReadFromAProviderAlias(t *testing.T) {
	// CoreWeave writes gpu.nvidia.com/model rather than the GPU Feature
	// Discovery key. Missing the aliases means a fleet that is demonstrably on
	// H200s reads as having no accelerator, and the match silently never fires.
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		"gpu.nvidia.com/model": "H200",
	}}}
	if got := AcceleratorOf(node); got != "H200" {
		t.Fatalf("got %q", got)
	}
}

func TestARequirementIsReadFromNodeAffinityNotJustNodeSelector(t *testing.T) {
	// The shape llm-d's modelservice chart actually produces, copied from a live
	// decode Deployment: nodeAffinity, and NO nodeSelector at all. A check that
	// read nodeSelector alone would conclude this workload takes any accelerator
	// and never fire on the layout it exists to protect.
	spec := &corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "nvidia.com/gpu.product",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"NVIDIA-H100-80GB-HBM3"},
				}},
			}},
		},
	}}}
	if got := AcceleratorRequiredBy(spec); got != "NVIDIA-H100-80GB-HBM3" {
		t.Fatalf("got %q", got)
	}
}

func TestANodeSelectorRequirementIsReadToo(t *testing.T) {
	spec := &corev1.PodSpec{NodeSelector: map[string]string{
		"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB",
	}}
	if got := AcceleratorRequiredBy(spec); got != "NVIDIA-A100-SXM4-80GB" {
		t.Fatalf("got %q", got)
	}
}

func TestAWorkloadPortableAcrossModelsConstrainsNothing(t *testing.T) {
	// A term listing several acceptable models says the workload is portable
	// between them. Picking the first would invent a constraint it never stated
	// and decline a pool that could serve it perfectly well.
	spec := &corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "nvidia.com/gpu.product",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"NVIDIA-H100-80GB-HBM3", "NVIDIA-A100-SXM4-80GB"},
				}},
			}},
		},
	}}}
	if got := AcceleratorRequiredBy(spec); got != "" {
		t.Fatalf("a multi-valued term is not a requirement to enforce, got %q", got)
	}
}

func TestPreferredAffinityIsNotARequirement(t *testing.T) {
	// The scheduler may ignore a preference, so a warm copy placed on the
	// strength of one could land on the wrong accelerator anyway.
	spec := &corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
			Weight: 100,
			Preference: corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "nvidia.com/gpu.product",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"NVIDIA-H100-80GB-HBM3"},
				}},
			},
		}},
	}}}
	if got := AcceleratorRequiredBy(spec); got != "" {
		t.Fatalf("a preference is not a requirement, got %q", got)
	}
}

func TestNotInSaysWhatItRefusesNotWhatItNeeds(t *testing.T) {
	spec := &corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "nvidia.com/gpu.product",
					Operator: corev1.NodeSelectorOpNotIn,
					Values:   []string{"NVIDIA-A100-SXM4-80GB"},
				}},
			}},
		},
	}}}
	if got := AcceleratorRequiredBy(spec); got != "" {
		t.Fatalf("NotIn names no requirement, got %q", got)
	}
}

func TestANonAcceleratorAffinityIsIgnored(t *testing.T) {
	// Workloads pin zones, instance types and much else. Only GPU-model keys
	// count, or every workload would appear to demand an accelerator named
	// "us-east-1a".
	spec := &corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "topology.kubernetes.io/zone",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"us-east-1a"},
				}},
			}},
		},
	}}}
	if got := AcceleratorRequiredBy(spec); got != "" {
		t.Fatalf("a zone is not an accelerator, got %q", got)
	}
}

func TestOnlyAProvenMismatchBlocks(t *testing.T) {
	if _, bad := AcceleratorMismatch("H100", "A100"); !bad {
		t.Error("two known and different accelerators is a mismatch")
	}
	if _, bad := AcceleratorMismatch("H100", "H100"); bad {
		t.Error("the same accelerator is not a mismatch")
	}
	// An unknown on either side must ALLOW. Nodes are cluster-scoped, so a
	// namespace-scoped install may have no access to them, and declining every
	// admission there would silently disable the pool -- which costs every GPU
	// it holds, where a wrong warm copy costs one load.
	if _, bad := AcceleratorMismatch("", "A100"); bad {
		t.Error("a model requiring nothing fits anywhere")
	}
	if _, bad := AcceleratorMismatch("H100", ""); bad {
		t.Error("an unreadable node must not block admission")
	}
}
