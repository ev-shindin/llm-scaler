package pool

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// groupPodPtr is groupPod as an addressable Pod with an address, which is what
// the client and the fan-out both need.
func groupPodPtr(name string, worker, size int) *corev1.Pod {
	p := groupPod(name, worker, size, 1, true)
	p.Status.PodIP = "10.0.0." + itoa(worker+1)
	return &p
}

// fakeClientWith builds a client holding exactly these Pods.
func fakeClientWith(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// groupPod builds a Pod as LeaderWorkerSet stamps them: every member carries the
// set name, its group index, its worker index and the group's size.
func groupPod(name string, worker int, size int, gpus int64, ready bool) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels: map[string]string{
				lwsv1.SetNameLabelKey:     "warm",
				lwsv1.GroupIndexLabelKey:  "0",
				lwsv1.WorkerIndexLabelKey: itoa(worker),
			},
			Annotations: map[string]string{lwsv1.SizeAnnotationKey: itoa(size)},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "engine",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				gpuResource:           *resource.NewQuantity(gpus, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(64<<30, resource.BinarySI),
			}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0." + itoa(worker+1)},
	}
	if ready {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return p
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

// A group's warm unit is every Pod in it, so its device count is the group's,
// not the leader's. Reading the leader's own spec would size a two-Pod group at
// eight GPUs and refuse every engine that needs the sixteen it actually has.
func TestAGroupsCapacityCountsEveryPodsDevices(t *testing.T) {
	// Two shapes, so the arithmetic is the multiplication and not a coincidence
	// with one Pod count.
	for _, tc := range []struct {
		name       string
		size       int
		gpusPerPod int64
		wantGPUs   int
	}{
		{"two Pods of eight", 2, 8, 16},
		{"three Pods of four", 3, 4, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leader := groupPod("warm-0", 0, tc.size, tc.gpusPerPod, true)

			got := capacityOf(&leader)

			if got.GPUs != tc.wantGPUs {
				t.Errorf("group of %d Pods holding %d GPUs each: got %d GPUs, want %d",
					tc.size, tc.gpusPerPod, got.GPUs, tc.wantGPUs)
			}
			if got.PodsPerGroup != tc.size {
				t.Errorf("PodsPerGroup = %d, want %d", got.PodsPerGroup, tc.size)
			}
			if !got.Group() {
				t.Error("a multi-Pod unit should report Group()")
			}
		})
	}
}

// Memory is charged per container, so the budget that bounds a warm set is ONE
// Pod's limit. Multiplying it by the group size would admit a model that then
// OOM-kills every member at once -- the same failure the single-Pod case guards
// against, multiplied by the group.
func TestAGroupsMemoryBudgetStaysPerPod(t *testing.T) {
	leader := groupPod("warm-0", 0, 4, 8, true)

	if got := capacityOf(&leader).MemoryLimitBytes; got != 64<<30 {
		t.Errorf("memory limit = %d, want one Pod's %d", got, int64(64)<<30)
	}
}

// An ordinary pool Pod is in no group and must keep reading as a single unit.
func TestAPlainPodIsAGroupOfOne(t *testing.T) {
	plain := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-abc", Namespace: "ns"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				gpuResource: *resource.NewQuantity(1, resource.DecimalSI),
			}},
		}}},
	}

	got := capacityOf(&plain)

	if got.PodsPerGroup != 1 || got.GPUs != 1 {
		t.Errorf("plain Pod: got %d GPUs across %d Pods, want 1 and 1", got.GPUs, got.PodsPerGroup)
	}
	if got.Group() {
		t.Error("a plain Pod must not report Group()")
	}
}

// A size that cannot be read is not a group we can size a model against.
// Answering 1 makes the fit check refuse anything larger, which costs a decline
// rather than a wasted load into a unit that cannot hold the model.
func TestAnUnreadableGroupSizeReadsAsOne(t *testing.T) {
	p := groupPod("warm-0", 0, 2, 8, true)
	p.Annotations[lwsv1.SizeAnnotationKey] = "not-a-number"

	if got := groupSizeOf(&p); got != 1 {
		t.Errorf("unreadable size: got %d, want 1", got)
	}
}

// Only the leader runs the supervisor and serves the API. A worker counted as a
// member is a unit the pool believes it can lend whose engine does not exist --
// and a worker LABELLED into an InferencePool takes traffic nothing answers.
func TestOnlyTheLeaderIsAMember(t *testing.T) {
	for _, tc := range []struct {
		name   string
		worker int
		want   bool
	}{
		{"leader", 0, false},
		{"first worker", 1, true},
		{"third worker", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := groupPod("warm-0", tc.worker, 4, 8, true)
			if got := isLWSWorker(&p); got != tc.want {
				t.Errorf("isLWSWorker = %v, want %v", got, tc.want)
			}
		})
	}
}

// A Pod outside any group is never a worker, however its labels read.
func TestAPodInNoGroupIsNotAWorker(t *testing.T) {
	plain := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pool-abc"}}
	if isLWSWorker(&plain) {
		t.Error("a Pod with no worker-index label must not read as a worker")
	}
}

// ALL OR NOTHING. A group missing a Pod is not a degraded engine, it is no
// engine: the ranks cannot form, so nothing in it can be woken or lent.
func TestAGroupIsCountedOnlyWhenEveryPodIsReady(t *testing.T) {
	full := []corev1.Pod{
		groupPod("warm-0", 0, 3, 8, true),
		groupPod("warm-0-1", 1, 3, 8, true),
		groupPod("warm-0-2", 2, 3, 8, true),
	}
	if got := readyGroupMembers(full)["warm/0"]; got != 3 {
		t.Errorf("all three Ready: counted %d, want 3", got)
	}

	// One worker Running but not Ready: its rank has not joined.
	partial := []corev1.Pod{
		groupPod("warm-0", 0, 3, 8, true),
		groupPod("warm-0-1", 1, 3, 8, true),
		groupPod("warm-0-2", 2, 3, 8, false),
	}
	if got := readyGroupMembers(partial)["warm/0"]; got != 2 {
		t.Errorf("one not Ready: counted %d, want 2 (so the group is short)", got)
	}
}

// A Pod on its way out has already stopped being part of the engine, even while
// the API still lists it.
func TestATerminatingPodDoesNotCountTowardItsGroup(t *testing.T) {
	pods := []corev1.Pod{
		groupPod("warm-0", 0, 2, 8, true),
		groupPod("warm-0-1", 1, 2, 8, true),
	}
	now := metav1.Now()
	pods[1].DeletionTimestamp = &now

	if got := readyGroupMembers(pods)["warm/0"]; got != 1 {
		t.Errorf("terminating worker counted: got %d, want 1", got)
	}
}

// Two groups of the same set must be counted apart, or a complete group and a
// broken one average into two half-usable ones.
func TestGroupsAreCountedSeparately(t *testing.T) {
	pods := []corev1.Pod{
		groupPod("warm-0", 0, 2, 8, true),
		groupPod("warm-0-1", 1, 2, 8, true),
		groupPod("warm-1", 0, 2, 8, true),
		groupPod("warm-1-1", 1, 2, 8, false),
	}
	pods[2].Labels[lwsv1.GroupIndexLabelKey] = "1"
	pods[3].Labels[lwsv1.GroupIndexLabelKey] = "1"

	counts := readyGroupMembers(pods)

	if counts["warm/0"] != 2 {
		t.Errorf("group 0: got %d, want 2", counts["warm/0"])
	}
	if counts["warm/1"] != 1 {
		t.Errorf("group 1: got %d, want 1", counts["warm/1"])
	}
}

// The LEADER of an idle pool group is deliberately NotReady, and it must still
// count toward its group.
//
// The pool proxy runs on the leader and reports Ready exactly when a model is
// awake in the Pod -- that is how an idle Pod stays out of its InferencePool.
// So an EMPTY pool leader, which is the only kind the controller has anything to
// warm into, is NotReady by design.
//
// Counting readiness there deadlocked the group completely: the unit was never
// offered, so nothing was ever warmed into it, so its leader never became Ready,
// so it was never offered. A group warm pool could not be used at all. The
// existing tests could not see it because they build every Pod Ready.
func TestAnIdleLeaderStillCountsTowardItsGroup(t *testing.T) {
	pods := []corev1.Pod{
		groupPod("warm-0", 0, 2, 8, false), // leader: Running, NotReady -- no model awake
		groupPod("warm-0-1", 1, 2, 8, true),
	}

	if got := readyGroupMembers(pods)["warm/0"]; got != 2 {
		t.Errorf("idle leader counted %d of the group, want 2: a NotReady leader is an "+
			"empty pool Pod, not a broken rank", got)
	}
}

// A worker, though, is a rank and nothing else. Its readiness carries no traffic
// meaning, so it remains the signal that the rank has joined.
func TestANotReadyWorkerStillBreaksItsGroup(t *testing.T) {
	pods := []corev1.Pod{
		groupPod("warm-0", 0, 2, 8, false),
		groupPod("warm-0-1", 1, 2, 8, false), // worker not Ready: rank has not joined
	}

	if got := readyGroupMembers(pods)["warm/0"]; got != 1 {
		t.Errorf("group counted %d, want 1: a worker that is not Ready has no rank", got)
	}
}

// ---------------------------------------------------------------------------
// Fanning an engine out across the Pods of a group.
// ---------------------------------------------------------------------------

// Rank 0 serves the API and every other rank does not. Getting --headless the
// wrong way round is not a degraded engine: a headless rank 0 serves nothing at
// all, and a serving worker answers requests with an engine holding one shard.
func TestRankOptionsMarkOnlyWorkersHeadless(t *testing.T) {
	const base = "--model Qwen/Qwen3-8B --tensor-parallel-size 16 --nnodes 2"

	leader := rankOptions(base, 0, "10.0.0.1", 9001)
	if strings.Contains(leader, "--headless") {
		t.Errorf("rank 0 serves the API and must not be headless: %s", leader)
	}
	if !strings.Contains(leader, "--node-rank 0") {
		t.Errorf("rank 0 must say so: %s", leader)
	}

	worker := rankOptions(base, 1, "10.0.0.1", 9001)
	if !strings.Contains(worker, "--headless") {
		t.Errorf("a worker serves nothing and must be headless: %s", worker)
	}
	if !strings.Contains(worker, "--node-rank 1") {
		t.Errorf("a worker must carry its own rank: %s", worker)
	}
}

// Every rank rendezvouses at the SAME address, and it is the leader's. Ranks
// pointed at different masters form two process groups of one, each waiting for
// peers that are talking to somebody else.
func TestEveryRankRendezvousesAtTheLeader(t *testing.T) {
	const base = "--model Qwen/Qwen3-8B --nnodes 3"
	for rank := 0; rank < 3; rank++ {
		got := rankOptions(base, rank, "10.0.0.7", 9001)
		if !strings.Contains(got, "--master-addr 10.0.0.7") {
			t.Errorf("rank %d does not point at the leader: %s", rank, got)
		}
		// The engine's own shape has to survive: --nnodes is what the pool
		// matched the group against in the first place.
		if !strings.Contains(got, "--nnodes 3") {
			t.Errorf("rank %d lost the engine's layout: %s", rank, got)
		}
	}
}

// The group is returned in RANK order, which is the order its --node-rank flags
// are taken from. Sorting by Pod name would work only while the names happen to
// sort the same way, and LWS names rank 10 "-0-10", which sorts before "-0-2".
func TestGroupMembersComeBackInRankOrder(t *testing.T) {
	pods := []client.Object{
		groupPodPtr("warm-0", 0, 3),
		groupPodPtr("warm-0-2", 2, 3),
		groupPodPtr("warm-0-1", 1, 3),
	}
	a := &Adapter{client: fakeClientWith(pods...), namespace: "ns"}

	got, err := a.groupMembers(context.Background(), groupPodPtr("warm-0", 0, 3))
	if err != nil {
		t.Fatalf("enumerating a complete group: %v", err)
	}
	want := []string{"warm-0", "warm-0-1", "warm-0-2"}
	if len(got) != len(want) {
		t.Fatalf("got %d members, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("rank %d is %s, want %s", i, got[i].Name, w)
		}
	}
}

// A group short a rank is refused OUTRIGHT rather than warmed partially. Warming
// what is there leaves an engine waiting on a rank that does not exist, holding
// every GPU in the group and answering nothing.
func TestAnIncompleteGroupIsRefusedRatherThanPartlyWarmed(t *testing.T) {
	pods := []client.Object{
		groupPodPtr("warm-0", 0, 3),
		groupPodPtr("warm-0-1", 1, 3),
		// rank 2 never came up
	}
	a := &Adapter{client: fakeClientWith(pods...), namespace: "ns"}

	_, err := a.groupMembers(context.Background(), groupPodPtr("warm-0", 0, 3))
	if err == nil {
		t.Fatal("a group missing a rank must not be enumerated as usable")
	}
	if !strings.Contains(err.Error(), "missing rank 2") {
		t.Errorf("the error must name the rank that is absent, got: %v", err)
	}
}

// A Pod in no group is a group of one, so every caller can ask without first
// asking whether it should.
func TestAPlainPodEnumeratesAsItself(t *testing.T) {
	plain := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-abc", Namespace: "ns"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	a := &Adapter{client: fakeClientWith(plain), namespace: "ns"}

	got, err := a.groupMembers(context.Background(), plain)
	if err != nil {
		t.Fatalf("a plain Pod: %v", err)
	}
	if len(got) != 1 || got[0].Name != "pool-abc" {
		t.Errorf("got %+v, want just the Pod itself", got)
	}
}

// A rank with no address cannot be reached, and a group enumerated with one is a
// warm that fails halfway with instances already created.
func TestARankWithoutAnAddressStopsTheGroup(t *testing.T) {
	noIP := groupPodPtr("warm-0-1", 1, 2)
	noIP.Status.PodIP = ""
	a := &Adapter{client: fakeClientWith(groupPodPtr("warm-0", 0, 2), noIP), namespace: "ns"}

	_, err := a.groupMembers(context.Background(), groupPodPtr("warm-0", 0, 2))
	if err == nil || !strings.Contains(err.Error(), "no address") {
		t.Errorf("a rank without an address must stop the group, got: %v", err)
	}
}
