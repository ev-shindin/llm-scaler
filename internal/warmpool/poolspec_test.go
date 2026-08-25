package warmpool

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
)

func podNamed(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "pool", Name: name}
}

func poolDeployment(name, poolName string, replicas int32, annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "workload",
			Labels: map[string]string{
				pool.ComponentLabel: pool.ComponentValue,
				pool.PoolLabel:      poolName,
			},
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

func pools(t *testing.T, objects ...runtime.Object) []PoolSpec {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	d := &DeploymentPools{
		Client:    c,
		Namespace: "workload",
		Fallback:  policy.Config{SleepMinSize: 1, MaxHold: 2 * time.Minute, PreloadTop: 2},
	}
	got, err := d.Pools(context.Background())
	if err != nil {
		t.Fatalf("Pools: %v", err)
	}
	return got
}

func TestANamespaceWithNoPoolDeploymentStillHasOnePool(t *testing.T) {
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

func TestAnnotationsOverrideTheFlagsPerPool(t *testing.T) {
	got := pools(t, poolDeployment("a", "fast", 4, map[string]string{
		AnnotationSleepMinSize:         "2",
		AnnotationMaxHold:              "30s",
		AnnotationPreloadTop:           "5",
		AnnotationGPUMemoryUtilization: "0.75",
	}))
	if len(got) != 1 {
		t.Fatalf("want one pool, got %+v", got)
	}
	p := got[0]
	if p.Name != "fast" || p.Replicas != 4 {
		t.Errorf("name/replicas wrong: %+v", p)
	}
	if p.Config.SleepMinSize != 2 || p.Config.MaxHold != 30*time.Second || p.Config.PreloadTop != 5 {
		t.Errorf("annotations did not win: %+v", p.Config)
	}
	if p.GPUMemoryUtilization != 0.75 {
		t.Errorf("gpu utilisation not read: %v", p.GPUMemoryUtilization)
	}
}

func TestAnUnsetAnnotationKeepsTheFlagValue(t *testing.T) {
	// Layering, not replacement: a pool that tunes one knob must not silently
	// zero the other three.
	got := pools(t, poolDeployment("a", "fast", 3, map[string]string{
		AnnotationSleepMinSize: "2",
	}))
	if got[0].Config.MaxHold != 2*time.Minute || got[0].Config.PreloadTop != 2 {
		t.Fatalf("unset knobs must keep the flag value: %+v", got[0].Config)
	}
}

func TestAnUnparseableAnnotationIsIgnoredRatherThanZeroed(t *testing.T) {
	// Zero is meaningful for every one of these -- no reserve, no hold limit, no
	// preloading -- so parsing a typo as zero turns it into a policy change, and
	// a silent one. "2x" must leave the reserve alone, not remove it.
	got := pools(t, poolDeployment("a", "fast", 3, map[string]string{
		AnnotationSleepMinSize: "2x",
		AnnotationMaxHold:      "banana",
		AnnotationPreloadTop:   "-1",
	}))
	c := got[0].Config
	if c.SleepMinSize != 1 || c.MaxHold != 2*time.Minute || c.PreloadTop != 2 {
		t.Fatalf("a bad value must be ignored, not applied as zero: %+v", c)
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
		poolDeployment("a", "h100", 2, nil),
		poolDeployment("z", "a100", 3, nil),
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
	inert := PoolSpec{Name: "x", Replicas: 1, Config: policy.Config{SleepMinSize: 1}}
	why, got := inert.Inert()
	if !got {
		t.Fatal("replicas equal to the reserve can never admit anything")
	}
	if why == "" {
		t.Error("the reason must say what to change")
	}

	ok := PoolSpec{Name: "x", Replicas: 2, Config: policy.Config{SleepMinSize: 1}}
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
