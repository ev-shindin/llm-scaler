// Package locator resolves a pod to the managed scaler (HPA or KEDA
// ScaledObject) that controls its replica count, via ownerReferences walking
// for Deployment / LWS layouts and via the variant name for shadow-pod
// layouts.

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch

package locator

import (
	"context"
	"fmt"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagedScaler identifies the scaler in front of a workload.
//
// It carries a name rather than the object because the object is no longer read:
// attribution resolves against the in-memory registry, which is the set of
// workloads KEDA has called WVA about. Name is the ScaledObject's name, which is
// what every downstream consumer keys a variant by.
type ManagedScaler struct {
	Namespace string
	Name      string
}

// The locator no longer knows or cares whether KEDA is installed. It used to:
// the ScaledObject field index was only registered when the CRD was present, so
// every lookup had to be gated on that. Attribution now resolves against the
// in-memory registry, which is empty when nothing has called WVA — the same
// answer, without the flag or the informer.

// PodLocator resolves pods to managed scalers. Implementations are safe
// for concurrent use.
type PodLocator interface {
	// Locate finds the managed scaler whose scale-target chain contains the
	// given pod. Returns (nil, nil) when the pod is unmanaged or when its
	// ownerReferences chain does not reach a Deployment / LeaderWorkerSet
	// (e.g. shadow pod). Errors only on infrastructure failures or invariant
	// violations (cycle, depth exceeded, both an HPA and a ScaledObject
	// managing the same scale target).
	//
	// This is the collector's only source of variant identity, so a pod it
	// cannot attribute contributes no replica metrics.
	Locate(ctx context.Context, namespace, podName string) (*ManagedScaler, error)

	// ResolveScaleTarget returns the top-level Deployment / LWS scale target
	// in the pod's ownerReferences chain, independent of whether a managed
	// scaler controls it. ok is false when the pod has no scaler-eligible
	// ancestor (shadow pod, unknown chain) or does not exist. Reuses the
	// pod→target cache, so a call following Locate for the same pod issues
	// no extra API reads. Use this to attribute metrics for scale targets
	// fronted by an unmanaged scaler (e.g. a KServe-created HPA without
	// llm-d.ai/managed=true), where Locate returns (nil, nil).
	//
	// TODO(va-removal): this method exists only for the CRD-based dual-mode
	// fallback in the collector (buildInstanceKey). Remove it when the
	// VariantAutoscaling CRD is removed.
	ResolveScaleTarget(ctx context.Context, namespace, podName string) (ref autoscalingv2.CrossVersionObjectReference, ok bool, err error)

	// GetPodLabels returns the labels for the specified pod. This reuses the
	// same pod fetch that Locate performs. Returns nil if the pod does not exist or on error.
	GetPodLabels(ctx context.Context, namespace, podName string) map[string]string
}

// New constructs a PodLocator.
//
//   - apiReader — uncached reader (mgr.GetAPIReader()), used for every
//     Pod / ReplicaSet / Deployment / LWS read in the owner-chain walk.
//   - variants  — supplies the registry of workloads KEDA has called WVA about,
//     which is what turns a resolved scale target into a variant identity.
//
// variants is a func rather than a value because the locator is built inside the
// engine's constructor while the registry is wired onto the engine afterwards.
// Resolving it per call keeps ONE source of truth: whatever the engine is given
// is what attribution uses. A value captured here could silently differ, and the
// symptom would be metrics attributed against a different fleet than the
// optimizer is balancing. Nil, or a nil result, attributes nothing — the right
// answer when WVA has been called about nothing.
//
// It takes no cached client. It used to, for the field-indexed scaler lookup and
// a direct Get by name — and both of those were served by a cluster-wide
// ScaledObject informer. The registry replaces them with an in-memory lookup.
func New(apiReader client.Reader, variants func() *registry.Registry) (PodLocator, error) {
	cache, err := newResolutionCache(defaultCacheSize)
	if err != nil {
		return nil, err
	}
	return &podLocator{
		apiReader: apiReader,
		variants:  variants,
		maxDepth:  defaultMaxDepth,
		cache:     cache,
	}, nil
}

type podLocator struct {
	apiReader client.Reader
	variants  func() *registry.Registry
	maxDepth  int
	cache     *resolutionCache
}

// registry returns the current variant registry, or nil when none is wired.
func (l *podLocator) registry() *registry.Registry {
	if l.variants == nil {
		return nil
	}
	return l.variants()
}

func (l *podLocator) Locate(ctx context.Context, namespace, podName string) (*ManagedScaler, error) {
	// Step 1: pod → top-level scale target. Immutable per Kubernetes'
	// ownerReference rules, so the result is cacheable indefinitely.
	target, err := l.resolveTarget(ctx, namespace, podName)
	if err != nil {
		return nil, err
	}
	if target == (chainNode{}) {
		// The walk reached no scale target. On every topology but one that
		// settles it, and this returns nil without another call.
		return l.locateViaPairing(ctx, namespace, podName)
	}

	// Step 2: scale target → managed scaler. NOT cached; field-index reads
	// are cheap and reflect the current annotation / scaleTargetRef state.
	return l.resolveScaler(ctx, target)
}

// locateViaPairing resolves a pod whose ownerReferences reach no scale target by
// following a declared pairing to the pod that does.
//
// Fast Model Actuation splits a model server across two pods: a REQUESTER
// Deployment that carries the llm-d identity and is what a scaler moves, and
// LAUNCHER pods — owned by a LauncherConfig — that hold the GPU and run the
// engine. The ownerReferences gap is deliberate and permanent: FMA patches the
// provider's labels specifically so the ReplicaSet does not adopt it, which is
// exactly why the walk above ends with nothing. But the engine metrics come from
// the launcher, so without this hop an FMA variant has no workload WVA can both
// scale and measure, and its load is attributed to nothing.
//
// FMA maintains DualPodsPairLabelKey on both halves of a bound pair, each naming
// the other, so a launcher names its requester directly and one lookup suffices.
// Going the other way would mean listing every pod in the namespace on every
// collection cycle, including namespaces that have no FMA at all.
//
// Detection is a label read, never a CRD read: no client for fma.llm-d.ai, no
// informer, no RBAC on that group, and no check that the API even exists. FMA can
// therefore be installed long after WVA and be picked up on the next cycle, and a
// cluster that never installs it never executes past the first lookup below.
func (l *podLocator) locateViaPairing(ctx context.Context, namespace, podName string) (*ManagedScaler, error) {
	// resolveTarget has already cached this pod's labels, so the common case —
	// an ordinary unmanaged pod with no pairing — costs one map lookup and no
	// API call. A miss means the pod does not exist, the one path that does not
	// populate the cache.
	labels, cached := l.cache.getLabels(podKey{Namespace: namespace, Name: podName})
	if !cached {
		return nil, nil
	}
	partner := labels[constants.DualPodsPairLabelKey]
	if partner == "" || partner == podName {
		return nil, nil
	}

	// One hop, and one only: resolveTarget rather than Locate, so a pair that
	// names each other resolves or fails rather than recursing. The partner is
	// looked up in this pod's own namespace — the label carries a bare name, and
	// a pairing that crossed namespaces would not be one FMA created.
	//
	// The partner's cached entry is dropped first, so this always re-reads it.
	// The resolution cache is correct for the lifetime of the pod it describes,
	// and here we are resolving somebody ELSE's pod on the strength of a label:
	// the partner can be deleted while the launcher still names it. Trusting the
	// cache then keeps a launcher attributed to a variant whose requester is
	// gone — and for a PARKED model that means it reads as serving, so
	// scale-from-zero declines to wake a model whose decode it believes is
	// already covered. Parked, unwakeable, and reported healthy.
	//
	// This costs one GET per bound launcher per cycle, which is exactly what this
	// hop's cost was specified to be when this traversal was designed: one
	// extra GET. Pods that are not FMA launchers never
	// reach here, so the common path is untouched.
	l.cache.remove(podKey{Namespace: namespace, Name: partner})
	partnerTarget, err := l.resolveTarget(ctx, namespace, partner)
	if err != nil {
		return nil, err
	}
	if partnerTarget == (chainNode{}) {
		return nil, nil
	}

	// Both halves carry ModelLabelKey in the same sanitized form, so this
	// compares like with like. It rejects what FMA permits but no observed
	// deployment does: one launcher hosting instances for several models, whose
	// singular pairing label can name only one requester. Absence on either side
	// is not a mismatch — the label reaches a launcher from the
	// InferenceServerConfig's label map, which FMA treats as arbitrary, so its
	// absence proves nothing either way.
	if partnerLabels, ok := l.cache.getLabels(podKey{Namespace: namespace, Name: partner}); ok {
		podModel, partnerModel := labels[constants.ModelLabelKey], partnerLabels[constants.ModelLabelKey]
		if podModel != "" && partnerModel != "" && podModel != partnerModel {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info(
				"pairing not followed: the paired pod serves a different model",
				"namespace", namespace, "pod", podName, "partner", partner,
				"model", podModel, "partnerModel", partnerModel)
			return nil, nil
		}
	}

	return l.resolveScaler(ctx, partnerTarget)
}

// resolveTarget runs Step 1 of resolution: pod → top-level scale-target
// chainNode, memoized in the pod→target cache. Returns the zero chainNode
// (with nil error) when the pod has no scaler-eligible ancestor or does not
// exist. Shared by Locate and ResolveScaleTarget.
func (l *podLocator) resolveTarget(ctx context.Context, namespace, podName string) (chainNode, error) {
	if target, hit := l.cache.getTarget(podKey{Namespace: namespace, Name: podName}); hit {
		return target, nil
	}
	pod := &corev1.Pod{}
	if err := l.apiReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return chainNode{}, nil
		}
		return chainNode{}, fmt.Errorf("get pod %s/%s: %w", namespace, podName, err)
	}
	target, err := l.resolveScaleTarget(ctx, pod, namespace)
	if err != nil {
		return chainNode{}, err
	}
	l.cache.add(podKey{Namespace: namespace, Name: podName}, target, pod.Labels)
	return target, nil
}

func (l *podLocator) GetPodLabels(ctx context.Context, namespace, podName string) map[string]string {
	if podName == "" {
		return nil
	}

	// Check cache first
	key := podKey{Namespace: namespace, Name: podName}
	if labels, hit := l.cache.getLabels(key); hit {
		return labels
	}

	// Not in cache, fetch the pod
	pod := &corev1.Pod{}
	if err := l.apiReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			ctrl.LoggerFrom(ctx).Error(err, "GetPodLabels: failed to get pod", "namespace", namespace, "pod", podName)
		}
		return nil
	}

	// Resolve the target to populate the cache (so subsequent Locate calls don't refetch).
	// Skip caching on error to avoid a permanent negative entry from a transient failure.
	target, err := l.resolveScaleTarget(ctx, pod, namespace)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "GetPodLabels: resolveScaleTarget failed, skipping cache", "namespace", namespace, "pod", podName)
		return pod.Labels
	}
	l.cache.add(key, target, pod.Labels)

	return pod.Labels
}

// ResolveScaleTarget returns the top-level scale target for the given pod.
func (l *podLocator) ResolveScaleTarget(ctx context.Context, namespace, podName string) (autoscalingv2.CrossVersionObjectReference, bool, error) {
	target, err := l.resolveTarget(ctx, namespace, podName)
	if err != nil {
		return autoscalingv2.CrossVersionObjectReference{}, false, err
	}
	if target == (chainNode{}) {
		return autoscalingv2.CrossVersionObjectReference{}, false, nil
	}
	return autoscalingv2.CrossVersionObjectReference{
		APIVersion: target.APIVersion,
		Kind:       target.Kind,
		Name:       target.Name,
	}, true, nil
}

// resolveScaleTarget walks the pod's ownerReferences and returns the first
// ancestor that is a Deployment or LWS. Returns the zero chainNode if no
// such ancestor exists.
func (l *podLocator) resolveScaleTarget(ctx context.Context, pod *corev1.Pod, namespace string) (chainNode, error) {
	chain, err := walkOwnersUp(ctx, l.apiReader, pod, namespace, l.maxDepth)
	if err != nil {
		return chainNode{}, err
	}
	for _, n := range chain {
		if scaleTargetKindSupported(n.Kind) {
			return n, nil
		}
	}
	return chainNode{}, nil
}

// resolveScaler finds the scaler in front of a top-level scale target.
//
// It reads the in-memory registry rather than a field index. An index means an
// informer and therefore a cluster-wide LIST+WATCH; the registry holds the same
// mapping for every workload WVA manages, so this costs no API traffic.
func (l *podLocator) resolveScaler(_ context.Context, target chainNode) (*ManagedScaler, error) {
	reg := l.registry()
	if reg == nil {
		return nil, nil
	}
	entry, ok := reg.FindByScaleTarget(target.Namespace, target.Kind, target.Name)
	if !ok {
		// The workload has no scaler WVA has been called about. Unmanaged as far as
		// WVA is concerned, which is the same answer the field index used to give
		// for an object carrying no annotation — without the cluster-wide watch
		// that index implied.
		return nil, nil
	}
	return &ManagedScaler{Namespace: entry.Namespace, Name: entry.Name}, nil
}
