// Package scaler implements KEDA's ExternalScaler gRPC service for WVA.
//
// WVA computes the desired replica count with its capacity model; this scaler
// delivers that decision to KEDA/HPA over the external-scaler contract, so KEDA
// actuates (no Prometheus round-trip, no prometheus-adapter). It resolves the
// scale target from the KEDA ScaledObject (namespace/name -> scaleTargetRef) and
// reads WVA's latest decision from the in-memory store the actuator feeds
// (internal/decision).
package scaler

import (
	"context"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
)

// MetricName is the external metric WVA exposes to KEDA/HPA. It is advertised
// with a target of 1, so HPA computes ceil(metricValue / 1) = metricValue and
// scales the target to exactly WVA's desired replica count.
const MetricName = "wva-desired-replicas"

// activationTTL bounds how long a "keep this target awake" decision is honoured
// without being refreshed. See Handler.honoursDecision.
const activationTTL = 60 * time.Second

// trustStaleLimit is how long a workload may go uncollected before WVA stops
// answering KEDA for it.
//
// It is NOT an expiry on the verdict -- the opposite. An earlier version expired
// an untrusted verdict back into a trusted one, so a wedged collector could not
// freeze a fleet forever. That was covering for a signal that could not fire:
// "I have not collected this workload in five minutes" IS the condition worth
// abstaining on, not something to time out of. A wedged collector now holds
// every workload it had seen, which is the honest outcome, and spec.fallback
// makes that a hold or a raise rather than a drop.
//
// Twenty optimize intervals at the 15s default. It has to sit well above a
// single missed pass: one collection that errors, or one namespace query that
// times out, must not read as a workload that has vanished.
//
// Kept equal to the collector's own staleObservationLimit; they answer the same
// question from the two ends of the store.
const trustStaleLimit = 5 * time.Minute

// Handler implements pb.ExternalScalerServer.
type Handler struct {
	pb.UnimplementedExternalScalerServer
	// client MUST be an uncached reader (manager.GetAPIReader). A cached Get of a
	// ScaledObject lazily starts a cluster-wide LIST+WATCH informer for the kind
	// on first use — the watch this design exists to remove — and it would appear
	// the moment KEDA made its first call, with nothing in the code saying "watch".
	client client.Reader
	store  *decision.Store
	// trust carries the collector's verdict on whether WVA can see a workload at
	// all. Read by GetMetrics only — see trusted.
	trust *decision.TrustStore
	// registry is the set of workloads WVA manages. Every RPC registers its ref
	// there — see observe.
	registry *registry.Registry
	// now is the clock used for decision freshness; overridden in tests.
	now func() time.Time
}

// NewHandler builds a Handler. A nil store falls back to decision.Default, a nil
// trust store to decision.DefaultTrust, and a nil reg to registry.Default.
//
// c must be uncached — see the client field.
func NewHandler(c client.Reader, store *decision.Store, reg *registry.Registry) *Handler {
	if store == nil {
		store = decision.Default
	}
	if reg == nil {
		reg = registry.Default
	}
	return &Handler{client: c, store: store, trust: decision.DefaultTrust, registry: reg, now: time.Now}
}

// WithTrustStore returns h using ts for trust verdicts instead of the
// process-wide store. For tests, which must not race on a shared store; nil is
// ignored so a caller cannot accidentally disarm the check.
func (h *Handler) WithTrustStore(ts *decision.TrustStore) *Handler {
	if ts != nil {
		h.trust = ts
	}
	return h
}

// observe records that KEDA has asked about this ref. It is the discovery event:
// WVA does not look for the workloads it manages, it manages the ones it is
// called about (docs/plans/engine/keda-driven-discovery.md).
//
// Called at the top of every RPC, including ones whose answer does not depend on
// the ref, because registration is the point and the answer is incidental. It
// never fails and never rejects: metadata that does not parse is still a
// registration — the engine reports the bad trigger once per cycle with the
// object's name attached, which is a far better diagnostic than a workload that
// silently never appears.
func (h *Handler) observe(ref *pb.ScaledObjectRef) {
	if ref == nil || h.registry == nil {
		return
	}
	h.registry.Observe(ref.GetNamespace(), ref.GetName(), ref.GetScalerMetadata())
}

// honoursDecision reports whether d still speaks for its target, or has gone
// stale and should be treated as "no opinion".
//
// The store is a latch: Set overwrites and nothing is ever removed, so without
// this a decision written once outlives whatever produced it. That is not
// hypothetical — a variant briefly running long enough for the saturation engine
// to publish "2" keeps isActive true forever after the target is scaled to zero,
// so the target is woken the instant KEDA asks, is never observed inactive, and
// NO engine can ever detect pending demand on it. Scale-from-zero is dead for
// that target until the process restarts.
//
// The rule is asymmetric, and the asymmetry is what makes it safe:
//
//   - "Keep this awake" (> 0) is a claim that must be kept current. Its
//     publishers normally re-publish well inside activationTTL — the
//     scale-from-zero engine every 100ms while a queue is pending, the saturation
//     engine every optimize interval (GLOBAL_OPT_INTERVAL, default 15s) — so an
//     unrefreshed positive decision means nobody is making the claim any more,
//     and the honest answer is to look at the target instead (see
//     currentlyRunning). GLOBAL_OPT_INTERVAL has only a lower bound, so a
//     deployment configuring it at or above activationTTL expires decisions
//     between publishes as a matter of course — and this repo ships one:
//     config/components/openshift/configmap-patch.yaml sets 60s, exactly the TTL.
//     The effect is benign, because the fallback then answers from the target's
//     replica count, which for a running target is still "active"; but it is the
//     normal case there, not an edge case.
//
//   - "This should be asleep" (0) never expires. It is the resting state, and
//     expiring it would flip a target that is still draining back to active,
//     flapping against the scale-down that is already in flight.
//
// Note that expiry cannot strand a cold start: as soon as KEDA acts on an
// activation the target has replicas, so the currentlyRunning fallback answers
// "active" on its own. The activation decision only has to outlive KEDA's
// reaction, not the pod's startup.
//
// Scope: this governs the 0<->1 gate only. GetMetrics applies no freshness check
// to the DECISION (a value collapsing to 0 between publishes would be worse than
// a slightly old one), so a stale decision on a RUNNING target still reports its
// last replica count there. GetMetrics does check whether WVA can see the
// workload at all, but that is a different question answered from a different
// store — see trusted — and it declines to answer rather than answering with a
// stale number. Either way, expiring the activation bit does not, on its own,
// let a target with an abandoned decision scale back down.
func (h *Handler) honoursDecision(d decision.Decision) bool {
	if d.DesiredReplicas <= 0 {
		return true
	}
	now := h.now
	if now == nil { // zero-value Handler (tests); NewHandler always sets it.
		now = time.Now
	}
	return now().Sub(d.UpdatedAt) <= activationTTL
}

// targetName resolves the scale-target name for a ScaledObjectRef, cheapest
// source first.
//
//  1. the registry, if the enricher has already resolved this entry — the steady
//     state, and the reason this path costs no API traffic at all; and only then
//  2. an uncached read of the ScaledObject, for the window between a workload's
//     first call and its first enrichment pass.
//
// Step 1 matters because step 2 is uncached by necessity: making it cached would
// serve it from a cluster-wide informer, so without the registry hop every KEDA
// poll of every workload would be a real API request.
//
// The scale target is never taken from trigger metadata. ScaledObject.spec
// .scaleTargetRef is the authority on what a ScaledObject scales, and a second
// hand-written copy of that name can disagree with it -- silently attributing a
// variant's metrics to another workload. That is not hypothetical: cloning a
// ScaledObject to add a variant used to carry the field across and point the new
// entry at the original's Deployment.
func (h *Handler) targetName(ctx context.Context, ref *pb.ScaledObjectRef) (string, error) {
	if ref == nil {
		return "", status.Error(codes.InvalidArgument, "scaledObjectRef is required")
	}
	if h.registry != nil {
		if e, ok := h.registry.Get(ref.GetNamespace(), ref.GetName()); ok && e.Target.Name != "" {
			return e.Target.Name, nil
		}
	}
	var so kedav1alpha1.ScaledObject
	nn := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	if err := h.client.Get(ctx, nn, &so); err != nil {
		return "", status.Errorf(codes.NotFound, "getting ScaledObject %s: %v", nn, err)
	}
	if so.Spec.ScaleTargetRef == nil || so.Spec.ScaleTargetRef.Name == "" {
		return "", status.Errorf(codes.FailedPrecondition, "ScaledObject %s has no scaleTargetRef.name", nn)
	}
	return so.Spec.ScaleTargetRef.Name, nil
}

// decisionFor returns WVA's latest decision for the ref and whether one exists
// yet.
func (h *Handler) decisionFor(ctx context.Context, ref *pb.ScaledObjectRef) (decision.Decision, bool, error) {
	name, err := h.targetName(ctx, ref)
	if err != nil {
		return decision.Decision{}, false, err
	}
	d, ok := h.store.Get(ref.Namespace, name)
	return d, ok, nil
}

// desired is gone: it existed only to hide decisionFor's third return from
// GetMetrics, and GetMetrics now resolves the target name itself so it can pass
// the same name to the trust check and the store lookup without resolving twice.
// isActive still calls decisionFor directly, as it always did.

// GetMetricSpec advertises the WVA metric with a target of 1 so HPA scales the
// target to exactly the value GetMetrics returns.
//
// The response does not depend on the ref, but the call still registers it: KEDA
// asks for the spec when it starts managing a ScaledObject, which makes this the
// earliest notice WVA gets that a workload exists.
func (h *Handler) GetMetricSpec(_ context.Context, ref *pb.ScaledObjectRef) (*pb.GetMetricSpecResponse, error) {
	h.observe(ref)
	return &pb.GetMetricSpecResponse{
		MetricSpecs: []*pb.MetricSpec{{MetricName: MetricName, TargetSize: 1}},
	}, nil
}

// GetMetrics returns WVA's desired replica count as the metric value. Before the
// first optimization decision exists it returns 0, so HPA holds the target at
// minReplicaCount rather than acting on a guess.
//
// It can also decline to answer, when the collector has no usable view of the
// workload. WVA's number would then be computed from inputs its own guards
// rejected, and the honest response is an error rather than a figure. KEDA
// already defines what an erroring scaler means, which is why this is an error
// and not a new WVA mechanism: no metric reaches the HPA, and after
// spec.fallback.failureThreshold consecutive failures KEDA applies
// spec.fallback.
//
// What that costs, stated exactly, because an earlier version of this comment
// got it wrong: an error here ALSO makes KEDA treat the scaler as INACTIVE.
// pkg/scalers/external_scaler.go's GetMetricsAndActivity calls GetMetrics first
// and returns `(nil, false, err)` on error -- it never reaches its IsActive
// call. So WVA's own IsActive answering correctly does not protect the 0<->1
// gate on this path, because KEDA does not ask. The protection is entirely in
// spec.fallback: KEDA's executor takes its "log only" branch for an erroring
// scaler when Fallback is set AND Fallback.Replicas != 0, which is precisely
// what stops a ScaledObject with minReplicaCount: 0 falling through to
// scaleToZeroOrIdle. deploy/lib/scaledobject.sh therefore floors
// fallback.replicas at 1 and never emits 0 -- see the comment there, and do not
// "simplify" it back to minReplicaCount.
//
// Scope is still GetMetrics only: IsActive and StreamIsActive keep answering,
// which matters for the push path (StreamIsActive drives activation and is not
// part of GetMetricsAndActivity).
func (h *Handler) GetMetrics(ctx context.Context, req *pb.GetMetricsRequest) (*pb.GetMetricsResponse, error) {
	ref := req.GetScaledObjectRef()
	h.observe(ref)
	// The trust check is asked of the ref ITSELF -- the ScaledObject's namespace
	// and name -- not of the resolved scale target. That is how the collector
	// keys it (rows carry the ScaledObject name as VariantName), and keying it
	// like the decision store instead is the bug this replaced: the generator
	// names ScaledObjects "<target>-wva", so a lookup by target name missed every
	// verdict ever published and the abstain could not fire.
	//
	// It runs BEFORE targetName on purpose. targetName falls back to an UNCACHED
	// Get whenever the registry has not yet enriched this ref, and there is no
	// reason to pay for it to answer a question that has already been decided.
	if err := h.trusted(ctx, ref.GetNamespace(), ref.GetName()); err != nil {
		return nil, err
	}
	// Resolved once, and only for the decision lookup, which IS keyed by the
	// scale target -- see decision.TrustRecord for why the two stores differ.
	name, err := h.targetName(ctx, ref)
	if err != nil {
		return nil, err
	}
	var value int64
	if d, ok := h.store.Get(ref.Namespace, name); ok {
		value = int64(d.DesiredReplicas)
	}
	return &pb.GetMetricsResponse{
		MetricValues: []*pb.MetricValue{{MetricName: MetricName, MetricValue: value}},
	}, nil
}

// trusted returns a gRPC error when the collector has no usable view of the
// named ScaledObject, and nil otherwise.
//
// Takes the SCALEDOBJECT's namespace and name -- the ref's own fields -- because
// that is the key the collector publishes under. See decision.TrustRecord.
//
// codes.Unavailable, because that is what it is -- the input is temporarily
// missing and the call is worth retrying, which is exactly how KEDA treats it:
// it counts the failure toward fallback.failureThreshold and asks again next
// poll. Not FailedPrecondition, which invites a reader to treat it as a
// permanent misconfiguration.
func (h *Handler) trusted(ctx context.Context, namespace, name string) error {
	now := h.now
	if now == nil { // zero-value Handler (tests); NewHandler always sets it.
		now = time.Now
	}
	trust := h.trust
	if trust == nil { // zero-value Handler (tests); NewHandler always sets it.
		trust = decision.DefaultTrust
	}
	if ok, reason := trust.Trust(namespace, name, now(), trustStaleLimit); !ok {
		log.FromContext(ctx).V(logging.DEFAULT).Info("declining to answer KEDA: no trusted view of the workload",
			"namespace", namespace, "scaledObject", name, "reason", reason)
		return status.Errorf(codes.Unavailable,
			"no trusted metrics for %s/%s: %s", namespace, name, reason)
	}
	return nil
}

// IsActive reports the target active unless WVA has decided it needs zero
// replicas; before the first decision it mirrors the target's current replica
// count (see isActive).
//
// This is the poll path, used by a `type: external` trigger. A `type:
// external-push` trigger uses StreamIsActive instead, which reports the same
// predicate without waiting for a poll interval.
func (h *Handler) IsActive(ctx context.Context, ref *pb.ScaledObjectRef) (*pb.IsActiveResponse, error) {
	h.observe(ref)
	active, err := h.isActive(ctx, ref)
	if err != nil {
		return nil, err
	}
	log.FromContext(ctx).V(1).Info("external scaler IsActive",
		"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "active", active)
	return &pb.IsActiveResponse{Result: active}, nil
}

// isActive is the shared activation predicate behind IsActive and
// StreamIsActive: active unless WVA has decided the target needs zero replicas.
//
// Before the first decision exists the predicate falls back to the target's
// CURRENT replica count, which is the only honest answer: reporting active
// unconditionally would wake every workload parked at zero the moment KEDA
// first asks — defeating scale-to-zero and pre-empting the scale-from-zero
// engine, which never sees the workload as inactive. Reporting inactive
// unconditionally would be worse, scaling a running workload to zero before WVA
// has looked at it. So: a target already at zero stays asleep until a decision
// (or the scale-from-zero engine) wakes it; a running target stays up.
//
// A decision that has gone stale takes the same fallback as no decision at all,
// so a target cannot be held awake by a value nobody is publishing any more —
// see honoursDecision.
func (h *Handler) isActive(ctx context.Context, ref *pb.ScaledObjectRef) (bool, error) {
	d, ok, err := h.decisionFor(ctx, ref)
	if err != nil {
		return false, err
	}
	if ok && h.honoursDecision(d) {
		return d.DesiredReplicas > 0, nil
	}
	return h.currentlyRunning(ctx, ref), nil
}

// currentlyRunning reports whether the scale target has replicas right now. It
// is only consulted before WVA's first decision. Any failure to read the target
// answers true — the safe direction, since a false negative would scale a
// running workload to zero on the strength of a failed lookup.
func (h *Handler) currentlyRunning(ctx context.Context, ref *pb.ScaledObjectRef) bool {
	logger := log.FromContext(ctx)

	var so kedav1alpha1.ScaledObject
	nn := types.NamespacedName{Namespace: ref.GetNamespace(), Name: ref.GetName()}
	if err := h.client.Get(ctx, nn, &so); err != nil || so.Spec.ScaleTargetRef == nil {
		logger.V(1).Info("no decision yet and ScaledObject unreadable; reporting active",
			"scaledObject", nn.String())
		return true
	}

	target := &unstructured.Unstructured{}
	apiVersion := so.Spec.ScaleTargetRef.APIVersion
	if apiVersion == "" {
		apiVersion = "apps/v1" // KEDA's own default for scaleTargetRef
	}
	kind := so.Spec.ScaleTargetRef.Kind
	if kind == "" {
		kind = "Deployment" // KEDA's own default for scaleTargetRef
	}
	target.SetAPIVersion(apiVersion)
	target.SetKind(kind)
	targetKey := types.NamespacedName{Namespace: ref.GetNamespace(), Name: so.Spec.ScaleTargetRef.Name}
	if err := h.client.Get(ctx, targetKey, target); err != nil {
		logger.V(1).Info("no decision yet and scale target unreadable; reporting active",
			"target", targetKey.String(), "kind", kind)
		return true
	}

	// An absent spec.replicas means the workload defaults to 1 (Deployment and
	// LeaderWorkerSet both do), so absent reads as running.
	replicas, found, err := unstructured.NestedInt64(target.Object, "spec", "replicas")
	if err != nil || !found {
		return true
	}
	return replicas > 0
}

// streamKeepalive bounds how long StreamIsActive stays silent. Activation is
// driven by decision-store wake-ups, so this only re-asserts the current state
// periodically — cheap insurance against a wake-up lost to a dropped stream or
// a decision written before KEDA opened it.
const streamKeepalive = 30 * time.Second

// StreamIsActive pushes activation to KEDA for a `type: external-push` trigger.
// It sends the current state immediately, then again whenever the target's
// decision changes it — so a workload sitting at zero is activated as soon as
// the decision lands, rather than up to one poll interval later. This is the
// path scale-from-zero rides: the engine that spots pending requests writes a
// non-zero decision, and that write wakes this stream.
//
// The stream lives until KEDA closes it (ctx cancellation), which returns nil —
// a closed stream is normal, not a failure. Errors resolving the scale target
// are returned, so KEDA surfaces a misconfigured ScaledObject instead of
// silently never activating.
func (h *Handler) StreamIsActive(ref *pb.ScaledObjectRef, stream pb.ExternalScaler_StreamIsActiveServer) error {
	ctx := stream.Context()
	logger := log.FromContext(ctx).WithValues(
		"namespace", ref.GetNamespace(), "scaledObject", ref.GetName())

	// Registered for the life of the stream rather than by timestamp. On a push
	// trigger this is the ONLY call a workload parked at zero ever receives —
	// KEDA does not poll IsActive, and the HPA does not query metrics for a
	// workload it is not scaling — so a TTL keyed on the last call would evict
	// exactly the entries whose purpose is to be woken from zero.
	if h.registry != nil && ref != nil {
		release := h.registry.Hold(ref.GetNamespace(), ref.GetName(), ref.GetScalerMetadata())
		defer release()
	}

	// Resolve the target once, up front: it is fixed for the life of the
	// ScaledObject, and subscribing needs the resolved name.
	name, err := h.targetName(ctx, ref)
	if err != nil {
		return err
	}
	updates, unsubscribe := h.store.Subscribe(ref.GetNamespace(), name)
	defer unsubscribe()

	// Send the opening state before waiting on anything, so KEDA is never left
	// holding a stream that has said nothing.
	active, err := h.isActive(ctx, ref)
	if err != nil {
		return err
	}
	if err := stream.Send(&pb.IsActiveResponse{Result: active}); err != nil {
		return err
	}
	logger.V(1).Info("external scaler StreamIsActive opened", "active", active)

	ticker := time.NewTicker(streamKeepalive)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.V(1).Info("external scaler StreamIsActive closed")
			return nil
		case <-updates:
			next, err := h.isActive(ctx, ref)
			if err != nil {
				return err
			}
			if next == active {
				continue // nothing for KEDA to act on
			}
			active = next
			if err := stream.Send(&pb.IsActiveResponse{Result: active}); err != nil {
				return err
			}
			logger.V(1).Info("external scaler pushed activation change", "active", active)
		case <-ticker.C:
			next, err := h.isActive(ctx, ref)
			if err != nil {
				return err
			}
			active = next
			if err := stream.Send(&pb.IsActiveResponse{Result: active}); err != nil {
				return err
			}
		}
	}
}
