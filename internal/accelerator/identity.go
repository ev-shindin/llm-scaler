package accelerator

import (
	"slices"
	"strings"
)

// SameName reports whether two accelerator names denote the same accelerator:
// identical, equal ignoring case, or one being the short name of the other
// ignoring case. It is how an operator's "H100", a node label's
// "NVIDIA-H100-80GB-HBM3" and a GKE label's "nvidia-h100-80gb" are recognised
// as one type.
//
// Case is folded because the vocabularies genuinely differ in it: NFD product
// labels are upper-case, GKE accelerator values and Kubernetes object names are
// lower-case, and operators type whichever they saw last.
//
// Two LONG names are never equated through their short forms. The short form is
// a heuristic — NormalizeAcceleratorName takes "the segment after the vendor",
// and for a name it does not know that segment can be a family word rather than
// a model — so agreeing on it is not evidence that two full product names are
// one product. A short name against a long one is different: the operator
// wrote the short name on purpose, and the question is only whether the long
// name reduces to it.
func SameName(a, b string) bool {
	if a == b || strings.EqualFold(a, b) {
		return true
	}
	if na := NormalizeAcceleratorName(a); na != a && strings.EqualFold(na, b) {
		return true
	}
	if nb := NormalizeAcceleratorName(b); nb != b && strings.EqualFold(nb, a) {
		return true
	}
	return false
}

// FindKey returns the key in known that names the same accelerator as name.
//
// Exact match first, then the short name, then SameName. The order matters:
// NormalizeAcceleratorName falls back to "the segment after the first hyphen"
// for names with no vendor prefix it knows, so an already-short "Gaudi-2"
// becomes "2" and matches nothing — trying the declared name first means such a
// name is found directly, and only names that genuinely need de-vendoring are
// normalized. When several keys match ignoring case, the lexically smallest is
// returned so the answer does not depend on map order.
func FindKey[V any](known map[string]V, name string) (string, bool) {
	if _, ok := known[name]; ok {
		return name, true
	}
	if normalized := NormalizeAcceleratorName(name); normalized != name {
		if _, ok := known[normalized]; ok {
			return normalized, true
		}
	}
	var matches []string
	for key := range known {
		if SameName(key, name) {
			matches = append(matches, key)
		}
	}
	if len(matches) == 0 {
		return "", false
	}
	return slices.Min(matches), true
}
