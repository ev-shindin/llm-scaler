package registry

import (
	"strings"
	"testing"
)

// TestParseMetaAcceptsAFullTrigger — the shape a working ScaledObject carries.
func TestParseMetaAcceptsAFullTrigger(t *testing.T) {
	m, err := ParseMeta(map[string]string{
		ScalerAddressKey: "wva-external-scaler.wva-system.svc:9090",
		ModelIDKey:       "default/default",
		VariantCostKey:   "12.5",
		ScalingPolicyKey: "interactive",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ModelID != "default/default" {
		t.Errorf("modelID: have %q", m.ModelID)
	}
	if m.VariantCost != "12.5" {
		t.Errorf("variantCost: have %q", m.VariantCost)
	}
	if m.ScalingPolicy != "interactive" {
		t.Errorf("scalingPolicy: have %q", m.ScalingPolicy)
	}
}

// TestUnknownKeysAreIgnored: the scale target is never taken from metadata, so a
// stale "variantName" left on a cloned trigger must not resolve to anything.
// Cloning a ScaledObject used to carry that key across and point the new entry
// at the original's Deployment.
func TestUnknownKeysAreIgnored(t *testing.T) {
	m, err := ParseMeta(map[string]string{
		ModelIDKey:    "default/default",
		"variantName": "some-other-deployment",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ModelID != "default/default" {
		t.Errorf("modelID: have %q", m.ModelID)
	}
}

// TestModelIDIsRequired: it is the grouping key for every multi-variant
// decision, so a variant without one cannot be optimized against a model at all.
func TestModelIDIsRequired(t *testing.T) {
	for _, meta := range []map[string]string{
		nil,
		{},
		{ModelIDKey: ""},
		{VariantCostKey: "1.0"},
	} {
		if _, err := ParseMeta(meta); err == nil {
			t.Errorf("expected an error for metadata %v", meta)
		}
	}
}

// TestVariantCostDefaults so a trigger that does not care about cost ranks where
// an unset cost has always ranked, rather than at zero.
func TestVariantCostDefaults(t *testing.T) {
	m, err := ParseMeta(map[string]string{ModelIDKey: "m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.VariantCost != DefaultVariantCost {
		t.Errorf("want the default %q, have %q", DefaultVariantCost, m.VariantCost)
	}
}

// TestVariantCostIsValidated at the boundary, so no consumer downstream has to
// wonder whether the string parses.
func TestVariantCostIsValidated(t *testing.T) {
	for _, cost := range []string{"free", "-1", "1.0.0", "10 "} {
		if _, err := ParseMeta(map[string]string{ModelIDKey: "m", VariantCostKey: cost}); err == nil {
			t.Errorf("expected an error for variantCost %q", cost)
		}
	}
}

// TestScalingPolicyIsOptional — absent, the cluster default applies.
func TestScalingPolicyIsOptional(t *testing.T) {
	m, err := ParseMeta(map[string]string{ModelIDKey: "m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ScalingPolicy != "" {
		t.Errorf("expected no policy, have %q", m.ScalingPolicy)
	}
}

func TestAPoolTriggerNeedsNoModelID(t *testing.T) {
	// A warm pool serves no model in particular. Requiring modelID would force
	// an operator to invent one, and an invented model id becomes a variant
	// every engine then has to special-case.
	meta, err := ParseMeta(map[string]string{WarmPoolNameKey: "default"})
	if err != nil {
		t.Fatalf("a pool trigger must be accepted without a model: %v", err)
	}
	if meta.WarmPoolName != "default" {
		t.Errorf("the pool name must survive: %+v", meta)
	}
}

func TestAModelTriggerStillRequiresAModelID(t *testing.T) {
	// The pool exemption must not become a way to register a model with no
	// identity.
	_, err := ParseMeta(map[string]string{VariantCostKey: "10.0"})
	if err == nil {
		t.Fatal("a trigger that names neither a model nor a pool must be refused")
	}
	if !strings.Contains(err.Error(), WarmPoolNameKey) {
		t.Errorf("the error should point at the pool key as the alternative: %v", err)
	}
}

func TestScalesAWarmPoolIsHowConsumersTell(t *testing.T) {
	if !ScalesAWarmPool(map[string]string{WarmPoolNameKey: "default"}) {
		t.Error("a pool trigger must be recognisable")
	}
	if ScalesAWarmPool(map[string]string{ModelIDKey: "m"}) {
		t.Error("a model trigger is not a pool")
	}
	if ScalesAWarmPool(map[string]string{WarmPoolNameKey: ""}) {
		t.Error("an empty pool name names no pool")
	}
}

func TestWarmPoolCopiesIsParsedAndValidated(t *testing.T) {
	meta, err := ParseMeta(map[string]string{ModelIDKey: "m", WarmPoolCopiesKey: "2"})
	if err != nil || meta.WarmPoolCopies == nil || *meta.WarmPoolCopies != 2 {
		t.Fatalf("copies must be read: %+v %v", meta, err)
	}

	// Zero is a real setting -- never warm this -- and must survive as zero
	// rather than collapsing into "unset".
	meta, err = ParseMeta(map[string]string{ModelIDKey: "m", WarmPoolCopiesKey: "0"})
	if err != nil || meta.WarmPoolCopies == nil || *meta.WarmPoolCopies != 0 {
		t.Fatalf("zero must be distinguishable from absent: %+v %v", meta, err)
	}

	if meta, _ := ParseMeta(map[string]string{ModelIDKey: "m"}); meta.WarmPoolCopies != nil {
		t.Error("absent must stay nil, which is what selects automatic mode")
	}

	for _, bad := range []string{"-1", "two", "1.5"} {
		if _, err := ParseMeta(map[string]string{ModelIDKey: "m", WarmPoolCopiesKey: bad}); err == nil {
			t.Errorf("%q must be refused with its trigger, not read as zero", bad)
		}
	}
}
