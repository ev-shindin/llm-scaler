package warmpool

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

func podNamed(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "pool", Name: name}
}

// poolTrigger is a registered ScaledObject declaring a warm pool, as KEDA would
// have delivered it.
func poolTrigger(soName, poolName string, maxReplicas int32, tuning map[string]string) registry.Entry {
	metadata := map[string]string{registry.WarmPoolNameKey: poolName}
	for k, v := range tuning {
		metadata[k] = v
	}
	return registry.Entry{
		Namespace: "workload",
		Name:      soName,
		Metadata:  metadata,
		Target: registry.Target{
			Name:        soName,
			MaxReplicas: &maxReplicas,
		},
	}
}

func pools(t *testing.T, entries ...registry.Entry) []PoolSpec {
	t.Helper()
	r := &RegistryPools{
		Snapshot:  func() []registry.Entry { return entries },
		Namespace: "workload",
		Fallback:  policy.Config{SleepMinSize: 1, MaxHold: 2 * time.Minute, PreloadTop: 2},
	}
	got, err := r.Pools(context.Background())
	if err != nil {
		t.Fatalf("Pools: %v", err)
	}
	return got
}

func TestANamespaceDeclaringNoPoolStillHasOnePool(t *testing.T) {
	// Every install that predates named pools: Pods labelled for discovery but
	// not for a named pool, and the config in flags. Returning nothing here
	// would silently switch a working pool off on upgrade.
	got := pools(t)
	if len(got) != 1 || got[0].Name != "" {
		t.Fatalf("want one unnamed pool, got %+v", got)
	}
	if got[0].Config.SleepMinSize != 1 || got[0].Config.PreloadTop != 2 {
		t.Errorf("the unnamed pool must carry the flag config: %+v", got[0].Config)
	}
}

func TestTriggerMetadataOverridesTheFlagsPerPool(t *testing.T) {
	got := pools(t, poolTrigger("a", "fast", 4, map[string]string{
		registry.WarmPoolSleepMinSizeKey:         "2",
		registry.WarmPoolMaxHoldKey:              "30s",
		registry.WarmPoolPreloadTopKey:           "5",
		registry.WarmPoolGPUMemoryUtilizationKey: "0.75",
	}))
	if len(got) != 1 {
		t.Fatalf("want one pool, got %+v", got)
	}
	p := got[0]
	if p.Name != "fast" || p.MaxReplicas != 4 {
		t.Errorf("name/ceiling wrong: %+v", p)
	}
	if p.Config.SleepMinSize != 2 || p.Config.MaxHold != 30*time.Second || p.Config.PreloadTop != 5 {
		t.Errorf("annotations did not win: %+v", p.Config)
	}
	if p.GPUMemoryUtilization != 0.75 {
		t.Errorf("gpu utilisation not read: %v", p.GPUMemoryUtilization)
	}
}

func TestAnUnsetKnobKeepsTheFlagValue(t *testing.T) {
	// Layering, not replacement: a pool that tunes one knob must not silently
	// zero the other three.
	got := pools(t, poolTrigger("a", "fast", 3, map[string]string{
		registry.WarmPoolSleepMinSizeKey: "2",
	}))
	if got[0].Config.MaxHold != 2*time.Minute || got[0].Config.PreloadTop != 2 {
		t.Fatalf("unset knobs must keep the flag value: %+v", got[0].Config)
	}
}

func TestAnUnreadablePoolTriggerIsRefusedWhole(t *testing.T) {
	// Zero is meaningful for every one of these -- no reserve, no hold limit, no
	// preloading -- so parsing a typo as zero turns it into a policy change, and
	// a silent one. "2x" must leave the reserve alone, not remove it.
	// A pool whose trigger cannot be read is refused WHOLE rather than partly
	// applied: its knobs decide how many GPUs it holds, so carrying on with
	// some of them would leave an operator reading a number in force nowhere.
	got := pools(t, poolTrigger("a", "fast", 3, map[string]string{
		registry.WarmPoolSleepMinSizeKey: "2x",
	}))
	if len(got) != 1 || got[0].Name != "" {
		t.Fatalf("an unreadable pool must be dropped, leaving the unnamed fallback: %+v", got)
	}
}

func TestTwoPoolsAreDiscoveredInAStableOrder(t *testing.T) {
	// Two accelerator types, two pools. Order must not depend on list or map
	// order: a pool that wins a free Pod only on some passes is worse than
	// either outcome consistently.
	// The Deployment names run OPPOSITE to the pool names on purpose. With them
	// aligned the fake client's own name ordering produces the right answer
	// whether or not anything sorts, so the test agreed with a missing sort.
	got := pools(t,
		poolTrigger("a", "h100", 2, nil),
		poolTrigger("z", "a100", 3, nil),
	)
	if len(got) != 2 {
		t.Fatalf("want two pools, got %+v", got)
	}
	if got[0].Name != "a100" || got[1].Name != "h100" {
		t.Fatalf("pools must be ordered by name: %+v", got)
	}
}

func TestAPoolWhoseReserveDoesNotFitIsCalledOut(t *testing.T) {
	// Read from the Deployment, so it is reported whether or not any model has
	// asked for anything yet.
	inert := PoolSpec{Name: "x", MaxReplicas: 1, Config: policy.Config{SleepMinSize: 1}}
	why, got := inert.Inert()
	if !got {
		t.Fatal("replicas equal to the reserve can never admit anything")
	}
	if why == "" {
		t.Error("the reason must say what to change")
	}

	ok := PoolSpec{Name: "x", MaxReplicas: 2, Config: policy.Config{SleepMinSize: 1}}
	if _, bad := ok.Inert(); bad {
		t.Error("a pool with room above its reserve is fine")
	}
}

func TestMembershipsAreSplitByPool(t *testing.T) {
	all := []pool.Membership{
		{Pod: podNamed("a"), Pool: "fast"},
		{Pod: podNamed("b"), Pool: "slow"},
		{Pod: podNamed("c"), Pool: "fast"},
	}
	if got := MembershipsIn(all, "fast"); len(got) != 2 {
		t.Fatalf("want the two fast Pods, got %+v", got)
	}
	if got := MembershipsIn(all, "slow"); len(got) != 1 {
		t.Fatalf("want the one slow Pod, got %+v", got)
	}
	if got := MembershipsIn(all, "missing"); len(got) != 0 {
		t.Fatalf("an undeclared pool owns nothing, got %+v", got)
	}
}

func TestAPodBelongingToNoDeclaredPoolIsReported(t *testing.T) {
	// It holds a GPU that no pool will ever lend. Folding it into the first pool
	// would be worse -- that pool would try to admit models into a Pod sized for
	// something else -- so it is left out and named.
	all := []pool.Membership{
		{Pod: podNamed("a"), Pool: "fast"},
		{Pod: podNamed("stray"), Pool: "typo"},
	}
	got := Orphaned(all, []PoolSpec{{Name: "fast"}})
	if len(got) != 1 || got[0] != "stray" {
		t.Fatalf("want the stray Pod named, got %+v", got)
	}
	if none := Orphaned(all, []PoolSpec{{Name: "fast"}, {Name: "typo"}}); len(none) != 0 {
		t.Fatalf("nothing is orphaned once both pools are declared: %+v", none)
	}
}

// variantSelecting is one variant naming the pool it wants, or "" for none.
func variantSelecting(warmPool string) policy.VariantDemand {
	return policy.VariantDemand{
		Model:    pool.ModelRef{Namespace: "workload", Variant: "qwen"},
		WarmPool: warmPool,
	}
}

func TestOnePoolNeedsNoSelection(t *testing.T) {
	// The common case by a wide margin, and the reason the metadata key is not
	// boilerplate: with one pool there is nothing to disambiguate, so a
	// ScaledObject that says nothing still gets a warm copy.
	only := []PoolSpec{{Name: "default"}}
	got := VariantsFor([]policy.VariantDemand{variantSelecting("")}, only[0], only)
	if len(got) != 1 {
		t.Fatalf("a variant naming no pool must take the only pool: %+v", got)
	}
	if bad := Unassignable([]policy.VariantDemand{variantSelecting("")}, only); len(bad) != 0 {
		t.Errorf("nothing is unassignable when there is one pool: %+v", bad)
	}
}

func TestAVariantGoesOnlyToThePoolItNamed(t *testing.T) {
	two := []PoolSpec{{Name: "a100"}, {Name: "h100"}}
	vs := []policy.VariantDemand{variantSelecting("h100")}

	if got := VariantsFor(vs, two[1], two); len(got) != 1 {
		t.Fatalf("the named pool must take it: %+v", got)
	}
	if got := VariantsFor(vs, two[0], two); len(got) != 0 {
		t.Fatalf("the other pool must not: %+v", got)
	}
}

func TestSeveralPoolsAndNoSelectionIsReportedRatherThanGuessed(t *testing.T) {
	// Defaulting to the first pool would be wrong half the time with two
	// accelerators, and being wrong costs a load that can never serve.
	two := []PoolSpec{{Name: "a100"}, {Name: "h100"}}
	vs := []policy.VariantDemand{variantSelecting("")}

	for _, p := range two {
		if got := VariantsFor(vs, p, two); len(got) != 0 {
			t.Fatalf("no pool may claim an unselected variant: %+v", got)
		}
	}
	bad := Unassignable(vs, two)
	if bad["qwen"] == "" {
		t.Fatal("it must be reported, not silently dropped")
	}
	for _, want := range []string{"a100", "h100", "warmPool"} {
		if !strings.Contains(bad["qwen"], want) {
			t.Errorf("the reason must name %q so it can be acted on: %q", want, bad["qwen"])
		}
	}
}

func TestNamingAPoolThatDoesNotExistIsReported(t *testing.T) {
	// A typo in trigger metadata otherwise reads as a pool that is merely too
	// small, which sends an operator after entirely the wrong thing.
	one := []PoolSpec{{Name: "a100-fast"}}
	vs := []policy.VariantDemand{variantSelecting("a100-fst")}

	if got := VariantsFor(vs, one[0], one); len(got) != 0 {
		t.Fatalf("a misnamed pool must not fall back to the only one: %+v", got)
	}
	bad := Unassignable(vs, one)
	if !strings.Contains(bad["qwen"], "a100-fst") || !strings.Contains(bad["qwen"], "a100-fast") {
		t.Fatalf("the reason must show both what was asked for and what exists: %q", bad["qwen"])
	}
}

func TestTheUnnamedPoolTakesVariantsThatNameNothing(t *testing.T) {
	// An install that predates named pools: no llm-d.ai/warm-pool label anywhere
	// and no trigger metadata. Both sides are empty and must still match, or the
	// pool stops warming anything the moment this ships.
	unnamed := []PoolSpec{{Name: ""}}
	got := VariantsFor([]policy.VariantDemand{variantSelecting("")}, unnamed[0], unnamed)
	if len(got) != 1 {
		t.Fatalf("an unnamed pool must serve variants that name nothing: %+v", got)
	}
}
