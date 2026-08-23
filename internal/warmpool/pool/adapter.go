package pool

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
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
	// gpuResource is what a pool Pod asks for, and what identifies the
	// container running engines among the Pod's containers.
	gpuResource = "nvidia.com/gpu"
	// ControlPlaneLabel is how the pool's NetworkPolicy recognises the
	// controller, and therefore who may reach the supervisor and the engines.
	// It is guarded for that reason rather than for ownership -- see
	// identityLabels.
	ControlPlaneLabel = "control-plane"

	// abandonTimeout bounds the cleanup of a failed admission. Short, because it
	// is one call and it runs after something has already gone wrong.
	abandonTimeout = 30 * time.Second

	// BasePort is the first port an instance may listen on inside a Pod.
	// Instance ports are ours to choose, unlike FMA's, which come from the
	// InferenceServerConfig and therefore collide between two instances of one
	// model.
	BasePort = 9001
	// MaxInstancesPerPod bounds the port range, and with it the warm set.
	MaxInstancesPerPod = 16
)

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
	// service and before the engine sleeps.
	//
	// It has to cover the whole chain, not just the drain: the kubelet must
	// notice /readyz failing (up to one probe period), the Pod must leave its
	// EndpointSlice, and the EPP must stop dispatching to it (~630 ms measured).
	// With the manifest's periodSeconds: 1 and failureThreshold: 1 that is
	// roughly 1.7 s, so the default leaves margin on top. Sleeping early is a
	// 503 for every request still arriving; sleeping late costs only the
	// milliseconds a returned Pod spends out of the reserve.
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
		DrainWait:     4 * time.Second,
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
			//
			// Logged, and at a level that ships. A Pod dropping silently out of
			// every observation looks exactly like a pool that is too small,
			// which sends an operator after the wrong thing entirely -- and the
			// most likely cause is not transient at all but a NetworkPolicy
			// naming the wrong namespace, which denies every port at once.
			log.FromContext(ctx).V(logging.DEFAULT).Info(
				"warm pool Pod could not be read; it is absent from this observation",
				"pod", p.Name, "err", err.Error())
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
	//
	// A FAILED read is not an empty one, and folding the two together was a
	// real defect rather than a tidy-up: an empty upstream means "no instance
	// is in service here", so a Pod that is awake and correctly pointed would
	// read as Waking the moment one GET to :8002 failed. Since a Waking engine
	// is treated as an orphan and slept, a single flaky read was enough to
	// drain and sleep a bridge that was serving live traffic.
	//
	// Unknown is its own answer. The Pod drops out of this observation, exactly
	// as it does when the supervisor will not answer, and the next pass looks
	// again.
	upstream, err := a.newProxy(p.Status.PodIP).Upstream(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the proxy's upstream in %s: %w", p.Name, err)
	}

	podRef := types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
	capacity := capacityOf(p)
	if len(instances) == 0 {
		// An idle Pod is reserve, and must be visible as such. See Membership.
		return []Membership{{Pod: podRef, State: Absent, Tier: a.tier, Capacity: capacity}}, nil
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
			Capacity: capacity,
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
		if inst.ID != InstanceID(model) {
			continue
		}
		// Already resident, and admission is idempotent -- but only if it is
		// the SAME engine. An instance carrying different options is a
		// different model, a different shape, or a different compile cache key,
		// and quietly serving traffic from it would be answering one variant's
		// requests with another variant's engine.
		if optionsWithoutPort(inst.Options) != optionsWithoutPort(model.EngineOptions) {
			return fmt.Errorf(
				"%s is resident in %s with different options (%q, wanted %q); refusing to reuse it",
				model.Variant, pod, inst.Options, model.EngineOptions)
		}
		return nil
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
		return a.abandon(ctx, sup, pod, model,
			fmt.Errorf("instance for %s in %s never served: %w", model.Variant, pod, err))
	}
	// Admission ends asleep. A model admitted and left awake would hold the
	// GPU it was supposed to share.
	if err := engine.Sleep(ctx, tier); err != nil {
		return a.abandon(ctx, sup, pod, model,
			fmt.Errorf("sleep newly admitted %s in %s: %w", model.Variant, pod, err))
	}
	return nil
}

// abandon removes an instance whose admission did not finish, and returns the
// error that caused it to be abandoned.
//
// Without this a failed admission is permanent. The instance exists in the
// supervisor but will not answer, so stateOf reports it as Loading -- which is
// deliberately NOT part of the reserve, because a Pod mid-load cannot serve a
// wake. Nothing ever revisits it: admission is idempotent on the instance
// already being there, so the pool simply loses a Pod, and with it a GPU.
//
// The delete gets a context of its own. The one that failed is very likely the
// reason it failed, and a cleanup that inherits an expired deadline cleans
// nothing up.
func (a *Adapter) abandon(ctx context.Context, sup *Supervisor, pod types.NamespacedName, model ModelRef, cause error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonTimeout)
	defer cancel()

	if err := sup.Delete(cleanup, InstanceID(model)); err != nil {
		// Reported together: an instance that can be neither loaded nor removed
		// has taken the Pod out of the pool, and that is worth more than the
		// original failure alone.
		return fmt.Errorf("%w (and it could not be removed, so %s is out of the reserve: %v)",
			cause, pod, err)
	}
	return cause
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

// capacityOf reads what a Pod actually has from its own spec.
//
// The ENGINE container's resources, not the Pod's total: the proxy sidecar has
// a small limit of its own that has nothing to do with how many models can
// sleep here, and summing the two would overstate the budget by exactly that
// much.
func capacityOf(p *corev1.Pod) PodCapacity {
	var capacity PodCapacity
	for i := range p.Spec.Containers {
		c := &p.Spec.Containers[i]
		gpus, hasGPUs := c.Resources.Limits[gpuResource]
		if !hasGPUs {
			gpus, hasGPUs = c.Resources.Requests[gpuResource]
		}
		if !hasGPUs {
			continue // not the container running engines
		}
		capacity.GPUs = int(gpus.Value())
		if limit, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			capacity.MemoryLimitBytes = limit.Value()
		}
		break
	}
	return capacity
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

// identityLabels are the ones a join must never write and a leave must never
// remove.
//
// The first two make a pool Pod part of its own Deployment. The third is
// different in kind and just as important: `control-plane` is what the pool's
// NetworkPolicy matches to decide who may reach :8001, :8002 and the engine
// ports. A pool Pod already carries `app.kubernetes.io/name:
// workload-variant-autoscaler`, so a tenant whose InferencePool selector also
// named `control-plane: controller-manager` would have WVA ITSELF label that Pod
// into the trusted set -- after which the tenant's own model code, running
// inside it, could reach the supervisor of every other pool Pod in the
// namespace and spawn processes there with argv of its choosing.
var identityLabels = []string{ComponentLabel, NameLabel, ControlPlaneLabel}

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

// optionsWithoutPort is an instance's options with the pool-assigned port
// removed, which is what makes two instances comparable: the port is the pool's
// choice and differs between Pods, everything else is the engine's identity.
func optionsWithoutPort(options string) string {
	fields := strings.Fields(options)
	out := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		name, _, hasInline := strings.Cut(fields[i], "=")
		if name != "--port" {
			out = append(out, fields[i])
			continue
		}
		if !hasInline && i+1 < len(fields) {
			i++
		}
	}
	return strings.Join(out, " ")
}

// portOf finds the port an instance was told to listen on, by tokenising rather
// than by searching.
//
// A regular expression over the whole options string was wrong in a way that
// mattered: it matched the FIRST `--port=NNNN` anywhere, including inside
// another flag's VALUE. Since the pool appends the real `--port N` last, a
// workload naming a model at, say, `/model-cache/w--port=9002/` would have the
// controller believe the engine listens on 9002 -- so freePort would reserve
// the wrong port, endpointOf would resolve to a DIFFERENT resident instance,
// and Activate would wake that one and point the serving port at it while
// applying the requesting variant's InferencePool labels.
//
// Tokenised, and the LAST occurrence wins, which is the one the pool appended.
func portOf(options string) int {
	port := 0
	fields := strings.Fields(options)
	for i := 0; i < len(fields); i++ {
		var value string
		name, inline, hasInline := strings.Cut(fields[i], "=")
		switch {
		case name != "--port":
			continue
		case hasInline:
			value = inline
		case i+1 < len(fields):
			value = fields[i+1]
			i++
		default:
			continue
		}
		if parsed, err := strconv.Atoi(value); err == nil {
			port = parsed
		}
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
