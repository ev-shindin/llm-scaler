package pool

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ComponentLabel marks a Pod as pool capacity. It is what keeps a lent Pod
	// out of the variant's replica accounting: a lent Pod carries the variant's
	// own labels to join its InferencePool, and would otherwise be scraped as
	// one of its replicas -- which is precisely the mistake that would let the
	// pool suppress the scale-up it exists to cover.
	ComponentLabel = "app.kubernetes.io/component"
	// ComponentValue is the value ComponentLabel carries on pool Pods.
	ComponentValue = "warm-pool"
	// NameLabel is the other half of the pool Deployment's own selector.
	NameLabel = "app.kubernetes.io/name"

	// BasePort is the first port an instance may listen on inside a Pod.
	// Instance ports are ours to choose, unlike FMA's, which come from the
	// InferenceServerConfig and therefore collide between two instances of one
	// model.
	BasePort = 9001
	// MaxInstancesPerPod bounds the port range, and with it the warm set.
	MaxInstancesPerPod = 16
)

// portFlag finds the port an instance was told to listen on. The options string
// is the source of truth because it is what the process was actually started
// with.
var portFlag = regexp.MustCompile(`--port[= ](\d+)`)

// Adapter implements Pool against the real data plane: the supervisor for
// instance lifecycle, the engine for sleep and wake, the proxy for the serving
// port, and Kubernetes for InferencePool membership.
//
// It carries no policy. Which model to warm, wake or evict, and in which Pod, is
// decided above and passed in.
type Adapter struct {
	client    client.Client
	namespace string
	tier      Tier

	// DrainWait is how long to let in-flight work finish after the Pod leaves
	// service and before the engine sleeps. Measured drain is ~630 ms; the
	// default leaves margin, because sleeping early is a 503 and sleeping late
	// costs only milliseconds.
	DrainWait time.Duration

	// These exist so the protocol halves can be substituted in tests. Each
	// takes a Pod IP because every one of them is addressed per Pod.
	newSupervisor func(podIP string) *Supervisor
	newEngine     func(ep Endpoint) *Engine
	newProxy      func(podIP string) *Proxy
}

// NewAdapter builds an Adapter over the pool Pods in a namespace.
func NewAdapter(c client.Client, namespace string, tier Tier) *Adapter {
	return &Adapter{
		client:        c,
		namespace:     namespace,
		tier:          tier,
		DrainWait:     2 * time.Second,
		newSupervisor: func(podIP string) *Supervisor { return NewSupervisor(podIP, 0) },
		newEngine:     func(ep Endpoint) *Engine { return NewEngine(ep, 0) },
		newProxy:      func(podIP string) *Proxy { return NewProxy(podIP, 0) },
	}
}

// InstanceID is the supervisor's key for a model in a Pod.
//
// Keyed on the VARIANT, not on a hash over GPU UUIDs. FMA hashes the GPUs
// because its sleepers are not portable between them -- CUDA_VISIBLE_DEVICES is
// fixed at process start -- but a pool Pod owns its GPUs for as long as it owns
// the model, so the question here is only "is this model resident in this Pod".
func InstanceID(model ModelRef) string { return model.Variant }

// ListWarm discovers what is resident, by asking rather than remembering.
func (a *Adapter) ListWarm(ctx context.Context) ([]Membership, error) {
	var pods corev1.PodList
	if err := a.client.List(ctx, &pods,
		client.InNamespace(a.namespace),
		client.MatchingLabels{ComponentLabel: ComponentValue},
	); err != nil {
		return nil, fmt.Errorf("list pool pods: %w", err)
	}

	var out []Membership
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.PodIP == "" || p.DeletionTimestamp != nil {
			continue // not addressable, or on its way out
		}
		found, err := a.membershipsIn(ctx, p)
		if err != nil {
			// One unreachable Pod must not blind the controller to the rest:
			// the reserve is still countable without it, and treating a
			// transient supervisor error as "the pool is empty" would trigger
			// pointless admissions.
			continue
		}
		out = append(out, found...)
	}
	return out, nil
}

func (a *Adapter) membershipsIn(ctx context.Context, p *corev1.Pod) ([]Membership, error) {
	instances, err := a.newSupervisor(p.Status.PodIP).List(ctx)
	if err != nil {
		return nil, err
	}
	// Where the Pod is currently sending traffic tells us which instance is
	// serving, as opposed to merely awake.
	upstream, err := a.newProxy(p.Status.PodIP).Upstream(ctx)
	if err != nil {
		upstream = ""
	}

	podRef := types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
	if len(instances) == 0 {
		// An idle Pod is reserve, and must be visible as such. See Membership.
		return []Membership{{Pod: podRef, State: Absent, Tier: a.tier}}, nil
	}
	out := make([]Membership, 0, len(instances))
	for _, inst := range instances {
		port := portOf(inst.Options)
		ep := Endpoint{PodIP: p.Status.PodIP, Port: port}
		out = append(out, Membership{
			Model:    ModelRef{Namespace: a.namespace, Variant: inst.ID, EngineOptions: inst.Options},
			Pod:      podRef,
			State:    a.stateOf(ctx, ep, upstream),
			Tier:     a.tier,
			Endpoint: ep,
		})
	}
	return out, nil
}

// stateOf asks the engine what it is. A Pod label would be cheaper and wrong: it
// is per-Pod, and flips for reasons unrelated to a given model.
func (a *Adapter) stateOf(ctx context.Context, ep Endpoint, upstream string) State {
	asleep, err := a.newEngine(ep).IsSleeping(ctx)
	switch {
	case err != nil:
		// The instance exists but will not answer: still loading, most likely,
		// and either way it cannot serve a wake, which is what the caller needs
		// to know.
		return Loading
	case asleep:
		return Asleep
	case upstream == localAddr(ep.Port):
		return Serving
	default:
		return Waking
	}
}

// Warm admits a model: it creates the instance and leaves it asleep.
func (a *Adapter) Warm(ctx context.Context, pod types.NamespacedName, model ModelRef, tier Tier) error {
	p, err := a.pod(ctx, pod)
	if err != nil {
		return err
	}
	sup := a.newSupervisor(p.Status.PodIP)

	existing, err := sup.List(ctx)
	if err != nil {
		return fmt.Errorf("list instances in %s: %w", pod, err)
	}
	for _, inst := range existing {
		if inst.ID == InstanceID(model) {
			return nil // already resident; admission is idempotent
		}
	}
	port, err := freePort(existing)
	if err != nil {
		return fmt.Errorf("%s: %w", pod, err)
	}

	spec := InstanceSpec{
		Options: fmt.Sprintf("%s --port %d", model.EngineOptions, port),
		EnvVars: map[string]string{
			// Without this vLLM does not expose /sleep or /wake_up at all, and
			// the pool has no mechanism. It belongs with the instance, not with
			// the Deployment, because it is what makes THIS instance warmable.
			"VLLM_SERVER_DEV_MODE": "1",
		},
	}
	if _, err := sup.Create(ctx, InstanceID(model), spec); err != nil {
		return fmt.Errorf("create instance for %s in %s: %w", model.Variant, pod, err)
	}

	ep := Endpoint{PodIP: p.Status.PodIP, Port: port}
	engine := a.newEngine(ep)
	if err := engine.WaitServing(ctx); err != nil {
		return fmt.Errorf("instance for %s in %s never served: %w", model.Variant, pod, err)
	}
	// Admission ends asleep. A model admitted and left awake would hold the
	// GPU it was supposed to share.
	if err := engine.Sleep(ctx, tier); err != nil {
		return fmt.Errorf("sleep newly admitted %s in %s: %w", model.Variant, pod, err)
	}
	return nil
}

// Activate puts a resident model into service.
func (a *Adapter) Activate(ctx context.Context, pod types.NamespacedName, model ModelRef) (Endpoint, error) {
	p, err := a.pod(ctx, pod)
	if err != nil {
		return Endpoint{}, err
	}
	ep, err := a.endpointOf(ctx, p, model)
	if err != nil {
		return Endpoint{}, err
	}

	// Label FIRST. Readiness is what admits traffic, so this costs nothing now
	// and takes the EPP's ~460 ms admit latency off the critical path.
	if err := a.setLabels(ctx, p, model.PoolLabels); err != nil {
		return Endpoint{}, fmt.Errorf("join the InferencePool for %s: %w", model.Variant, err)
	}
	if err := a.newEngine(ep).Wake(ctx); err != nil {
		// Leave the labels: the Pod is NotReady while the proxy has no
		// upstream, so it takes no traffic, and the caller is about to fall
		// through to the cold path. Unlabelling here would race the retry.
		return Endpoint{}, fmt.Errorf("wake %s in %s: %w", model.Variant, pod, err)
	}
	if err := a.newProxy(p.Status.PodIP).Point(ctx, ep); err != nil {
		return Endpoint{}, fmt.Errorf("point %s at %s: %w", pod, model.Variant, err)
	}
	return ep, nil
}

// Deactivate returns a serving model to the reserve.
func (a *Adapter) Deactivate(ctx context.Context, pod types.NamespacedName, model ModelRef) error {
	p, err := a.pod(ctx, pod)
	if err != nil {
		return err
	}
	ep, err := a.endpointOf(ctx, p, model)
	if err != nil {
		return err
	}

	// Clear the proxy FIRST: it is the gate. /readyz then fails, the kubelet
	// marks the Pod NotReady, and the EPP drains within the measured ~630 ms.
	if err := a.newProxy(p.Status.PodIP).Clear(ctx); err != nil {
		return fmt.Errorf("take %s out of service: %w", pod, err)
	}
	if wait := a.drainFor(ctx); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := a.removeLabels(ctx, p, model.PoolLabels); err != nil {
		return fmt.Errorf("leave the InferencePool for %s: %w", model.Variant, err)
	}
	if err := a.newEngine(ep).Sleep(ctx, a.tier); err != nil {
		return fmt.Errorf("sleep %s in %s: %w", model.Variant, pod, err)
	}
	return nil
}

// drainFor is how long to let in-flight work finish, bounded by the time the
// context actually has left.
//
// The drain must never be allowed to consume the whole budget, because the two
// calls AFTER it are the ones that matter. Deactivate clears the proxy first, so
// a timeout inside the drain leaves the Pod NotReady, still carrying its
// InferencePool labels, and with its engine still awake holding the GPU. The
// next pass reads that as Waking, which still counts as lent, and schedules the
// same Deactivate -- which fails at the same point again. The Pod never returns
// to the reserve.
//
// That is a livelock reachable purely by configuration: DrainWait lives on this
// Adapter and the deadline comes from the reconciler's ActTimeout, two knobs in
// two packages with no invariant tying them together. Rather than assert one,
// this makes the drain yield. Half the remaining budget, so the rule scales with
// whatever budget it is given and always leaves room for the unlabel and the
// sleep.
//
// Cutting the drain short costs the requests still in flight, which is a real
// cost -- but a strictly smaller one than losing a GPU-holding Pod permanently.
func (a *Adapter) drainFor(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return a.DrainWait
	}
	if half := time.Until(deadline) / 2; half < a.DrainWait {
		return max(half, 0)
	}
	return a.DrainWait
}

// Evict removes a model from a Pod, freeing its host memory.
func (a *Adapter) Evict(ctx context.Context, pod types.NamespacedName, model ModelRef) error {
	p, err := a.pod(ctx, pod)
	if err != nil {
		return err
	}
	if err := a.newSupervisor(p.Status.PodIP).Delete(ctx, InstanceID(model)); err != nil {
		return fmt.Errorf("evict %s from %s: %w", model.Variant, pod, err)
	}
	return nil
}

func (a *Adapter) pod(ctx context.Context, ref types.NamespacedName) (*corev1.Pod, error) {
	var p corev1.Pod
	if err := a.client.Get(ctx, ref, &p); err != nil {
		return nil, fmt.Errorf("get pool pod %s: %w", ref, err)
	}
	if p.Status.PodIP == "" {
		return nil, fmt.Errorf("pool pod %s has no address yet", ref)
	}
	return &p, nil
}

// endpointOf finds where a model listens in a Pod, from the options the instance
// was created with.
func (a *Adapter) endpointOf(ctx context.Context, p *corev1.Pod, model ModelRef) (Endpoint, error) {
	instances, err := a.newSupervisor(p.Status.PodIP).List(ctx)
	if err != nil {
		return Endpoint{}, fmt.Errorf("list instances in %s: %w", p.Name, err)
	}
	for _, inst := range instances {
		if inst.ID != InstanceID(model) {
			continue
		}
		port := portOf(inst.Options)
		if port == 0 {
			return Endpoint{}, fmt.Errorf("instance %s in %s has no --port", inst.ID, p.Name)
		}
		return Endpoint{PodIP: p.Status.PodIP, Port: port}, nil
	}
	return Endpoint{}, fmt.Errorf("%s is not resident in %s", model.Variant, p.Name)
}

func (a *Adapter) setLabels(ctx context.Context, p *corev1.Pod, labels map[string]string) error {
	if len(labels) == 0 {
		return errors.New("no pool labels given: membership must come from the InferencePool's own selector")
	}
	// A tenant's selector is theirs to write, and generic keys are common. One
	// naming `app.kubernetes.io/component` or `app.kubernetes.io/name` would
	// otherwise rewrite this Pod out of its OWN Deployment's selector: the
	// ReplicaSet then releases it, creating a GPU-holding orphan with no
	// controller, invisible to ListWarm and recoverable only by hand.
	for _, guarded := range identityLabels {
		if want, taken := labels[guarded]; taken && want != p.Labels[guarded] {
			return fmt.Errorf("refusing to join a pool whose selector rewrites %s (%q -> %q): "+
				"that label is what makes this Pod part of its own Deployment", guarded, p.Labels[guarded], want)
		}
	}
	patch := client.MergeFrom(p.DeepCopy())
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	for k, v := range labels {
		p.Labels[k] = v
	}
	return a.client.Patch(ctx, p, patch)
}

// identityLabels are the ones that make a pool Pod part of its own Deployment.
// They are never written by a join and never removed by a leave.
var identityLabels = []string{ComponentLabel, NameLabel}

func (a *Adapter) removeLabels(ctx context.Context, p *corev1.Pod, labels map[string]string) error {
	patch := client.MergeFrom(p.DeepCopy())
	for k := range labels {
		// Never remove what marks this Pod as pool capacity, even if a tenant's
		// selector happens to name it: without those labels the Pod stops being
		// excluded from replica accounting, and stops matching its own
		// Deployment.
		if isIdentityLabel(k) {
			continue
		}
		delete(p.Labels, k)
	}
	return a.client.Patch(ctx, p, patch)
}

func isIdentityLabel(key string) bool {
	return slices.Contains(identityLabels, key)
}

func portOf(options string) int {
	m := portFlag.FindStringSubmatch(options)
	if m == nil {
		return 0
	}
	port, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return port
}

func localAddr(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// freePort picks the lowest port in the Pod's range that no instance holds.
func freePort(existing []Instance) (int, error) {
	taken := make(map[int]bool, len(existing))
	for _, inst := range existing {
		taken[portOf(inst.Options)] = true
	}
	for port := BasePort; port < BasePort+MaxInstancesPerPod; port++ {
		if !taken[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free port: the Pod already holds %d instances", MaxInstancesPerPod)
}
