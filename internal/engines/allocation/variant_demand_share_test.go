package allocation

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// The demand carried on a decision is the model's (or role's), which the
// scheduler-queue estimate is part of, taken at the variant's share -- so a
// stage re-pricing one variant's replicas does not price the other variants'
// traffic against them.
func TestVariantDemandShare(t *testing.T) {
	vcs := []domain.VariantCapacity{
		{VariantName: "a", Role: "decode", TotalDemand: 30},
		{VariantName: "b", Role: "decode", TotalDemand: 10},
		{VariantName: "p", Role: "prefill", TotalDemand: 100},
	}
	assert.InDelta(t, 0.75, variantDemandShare(vcs, "a", "decode"), 1e-9)
	assert.InDelta(t, 0.25, variantDemandShare(vcs, "b", "decode"), 1e-9)
	// The only variant of its role has the whole of it.
	assert.InDelta(t, 1, variantDemandShare(vcs, "p", "prefill"), 1e-9)
	// A role's variants that reported nothing share evenly rather than at zero.
	none := []domain.VariantCapacity{{VariantName: "a"}, {VariantName: "b"}}
	assert.InDelta(t, 0.5, variantDemandShare(none, "a", domain.RoleBoth), 1e-9)
	// One variant, no rows: still the whole of it.
	assert.InDelta(t, 1, variantDemandShare(none[:1], "a", domain.RoleBoth), 1e-9)
}
