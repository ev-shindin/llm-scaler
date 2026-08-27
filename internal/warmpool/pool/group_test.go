package pool

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

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
