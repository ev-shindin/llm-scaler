package e2e

import (
	"testing"
)

// These are plain Go tests, not Ginkgo specs, so they run without a cluster:
//
//	go test ./test/e2e/ -run TestSaturationConfig
//
// The builders they cover are pure string formatting, and the values they
// render are what several suites assert scaling behaviour against. A silent
// change to one of the constants would only surface in a full e2e run against
// a live cluster, hours later and far from its cause -- and this repo has been
// bitten before by fixture values drifting between near-duplicate builders.

// wantDefaultEntry is what every suite that replaces the cluster's "default"
// saturation entry has always sent. Written out in full rather than rebuilt
// from the constants, so that changing a constant fails HERE, where the change
// can be seen, rather than in an e2e run.
const wantDefaultEntry = `
model_id: ""
namespace: ""
kvCacheThreshold: 0.80
queueLengthThreshold: 1
scaleUpThreshold: 0.85
scaleDownBoundary: 0.70
analyzerName: "saturation"
`

func TestSaturationConfigDefaultEntryIsUnchanged(t *testing.T) {
	if got := buildSaturationConfigYAML(); got != wantDefaultEntry {
		t.Errorf("buildSaturationConfigYAML() =\n%q\nwant\n%q", got, wantDefaultEntry)
	}
}

func TestSaturationConfigPerModelOverrideIsUnchanged(t *testing.T) {
	// The per-model override differs from the default only by carrying the
	// identity in its BODY -- a ConfigMap data key admits only [-._a-zA-Z0-9],
	// so a namespaced model ID cannot appear in one.
	want := `
model_id: "meta/llama"
namespace: "llm-d"
kvCacheThreshold: 0.80
queueLengthThreshold: 1
scaleUpThreshold: 0.85
scaleDownBoundary: 0.70
analyzerName: "saturation"
`
	got := buildSaturationConfigYAMLWithModel(
		saturationKVCacheThreshold, saturationQueueLengthThreshold,
		saturationScaleUpThreshold, saturationScaleDownBoundary,
		"meta/llama", "llm-d",
	)
	if got != want {
		t.Errorf("buildSaturationConfigYAMLWithModel() =\n%q\nwant\n%q", got, want)
	}
}

func TestTheTierSuiteStillRendersTheSharedKVCeiling(t *testing.T) {
	// The tier suite used to pass tierKvCacheThreshold explicitly. It no longer
	// can -- the parameter is gone -- so the two constants must agree, or that
	// suite silently starts testing a different configuration than it reads as
	// testing. This is the assertion that the removal was behaviour-preserving.
	if tierKvCacheThreshold != saturationKVCacheThreshold {
		t.Fatalf("tierKvCacheThreshold = %v, saturationKVCacheThreshold = %v: "+
			"the tier suite's per-model override renders the latter, so a divergence "+
			"here changes what that suite asserts without saying so",
			tierKvCacheThreshold, saturationKVCacheThreshold)
	}
}
