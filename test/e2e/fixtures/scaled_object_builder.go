package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
)

const (
	scaledObjectSuffix = "-so"

	kindLeaderWorkerSet = "LeaderWorkerSet"
	kindDeployment      = "Deployment"
	apiVersionLWS       = "leaderworkerset.x-k8s.io/v1"
	apiVersionAppsV1    = "apps/v1"
)

// ScaledObjectOption configures a KEDA ScaledObject before it is applied.
type ScaledObjectOption func(*kedav1alpha1.ScaledObject)

// WithScaledObjectScaleTargetKind sets the Kind and APIVersion on the ScaledObject's ScaleTargetRef.
func WithScaledObjectScaleTargetKind(kind string) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Spec.ScaleTargetRef == nil {
			return
		}
		so.Spec.ScaleTargetRef.Kind = kind
		switch kind {
		case kindLeaderWorkerSet:
			so.Spec.ScaleTargetRef.APIVersion = apiVersionLWS
		case kindDeployment:
			so.Spec.ScaleTargetRef.APIVersion = apiVersionAppsV1
		default:
			// Keep existing APIVersion for unknown kinds
		}
	}
}

// WithScaledObjectScaleDownStabilizationWindow sets the HPA scale-down stabilization window via
// KEDA's Advanced config. The default (300 s) is too long for e2e tests; pass a shorter value
// (e.g. 30 s) to make scale-down assertions complete within a reasonable test timeout.
func WithScaledObjectScaleDownStabilizationWindow(seconds int32) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Spec.Advanced == nil {
			so.Spec.Advanced = &kedav1alpha1.AdvancedConfig{}
		}
		if so.Spec.Advanced.HorizontalPodAutoscalerConfig == nil {
			so.Spec.Advanced.HorizontalPodAutoscalerConfig = &kedav1alpha1.HorizontalPodAutoscalerConfig{}
		}
		if so.Spec.Advanced.HorizontalPodAutoscalerConfig.Behavior == nil {
			so.Spec.Advanced.HorizontalPodAutoscalerConfig.Behavior = &autoscalingv2.HorizontalPodAutoscalerBehavior{}
		}
		so.Spec.Advanced.HorizontalPodAutoscalerConfig.Behavior.ScaleDown = &autoscalingv2.HPAScalingRules{
			StabilizationWindowSeconds: ptr.To(seconds),
		}
	}
}

// WithWVATriggerMetadata puts WVA's per-variant configuration into the trigger
// metadata of the ScaledObject. KEDA delivers that map verbatim as scalerMetadata
// on every external-scaler call, and the call is what registers the workload with
// WVA — so this is both the configuration and, indirectly, the discovery.
//
// The values are stashed on the object and moved into every trigger's metadata
// after all options have run — see applyWVATriggerMetadata. They cannot be
// written to the triggers here, because the trigger options replace
// spec.triggers wholesale and callers pass the options in either order.
func WithWVATriggerMetadata(modelID, cost string) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Annotations == nil {
			so.Annotations = make(map[string]string)
		}
		so.Annotations[pendingModelIDKey] = modelID
		so.Annotations[pendingVariantCostKey] = cost
	}
}

// WithScalingPolicy names the scaling policy TIER this workload belongs to, the
// way a real one does: as trigger metadata, resolved against a ConfigMap entry of
// that name. The tier carries no identity, so naming it here is the only thing
// that binds this workload to it.
//
// Deferred like WithWVATriggerMetadata, and for the same reason — the trigger
// options replace spec.triggers wholesale and callers pass options in any order.
func WithScalingPolicy(policy string) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Annotations == nil {
			so.Annotations = make(map[string]string)
		}
		so.Annotations[pendingScalingPolicyKey] = policy
	}
}

// externalScalerAddr is the WVA external-scaler service every fixture points KEDA
// at by default. Set once from the suite (SetExternalScalerAddress).
var externalScalerAddr string

// SetExternalScalerAddress configures the default trigger's target. Call it once
// in suite setup, before building any ScaledObject.
//
// It exists because the default HAS to be the external scaler now. A `prometheus`
// trigger never contacts WVA at all — KEDA reads the wva_desired_replicas gauge
// out of Prometheus instead — so under call-driven discovery a workload scaled
// that way is never registered, never discovered, and WVA never publishes the
// gauge KEDA is waiting for. A fixture that forgot the trigger option would not
// fail loudly; it would sit at its replica count until the spec timed out.
func SetExternalScalerAddress(addr string) { externalScalerAddr = addr }

// Private carrier keys for the deferred trigger metadata. They never reach the
// cluster: applyWVATriggerMetadata removes them.
const (
	pendingModelIDKey       = "e2e.llm-d.ai/pending-model-id"
	pendingVariantCostKey   = "e2e.llm-d.ai/pending-variant-cost"
	pendingScalingPolicyKey = "e2e.llm-d.ai/pending-scaling-policy"
	pendingWarmPoolKey      = "e2e.llm-d.ai/pending-warm-pool"
)

// WithWarmPoolTrigger makes this ScaledObject DECLARE a warm pool rather than
// scale a model.
//
// A pool is declared by its trigger, so this is what brings one into existence
// as far as WVA is concerned -- a pool Deployment with no such trigger holds
// accelerators nothing will use. tuning carries the warmPool* keys; nil takes
// the controller's defaults.
//
// Deliberately exclusive with WithWVATriggerMetadata: a trigger names a model or
// a pool, never both, and ParsePoolMeta would reject the mixture anyway.
func WithWarmPoolTrigger(poolName string, tuning map[string]string) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		if so.Annotations == nil {
			so.Annotations = make(map[string]string)
		}
		payload := map[string]string{registry.WarmPoolNameKey: poolName}
		for k, v := range tuning {
			payload[k] = v
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			// A map[string]string cannot fail to marshal; panicking here would
			// be noise in a fixture.
			return
		}
		so.Annotations[pendingWarmPoolKey] = string(encoded)
	}
}

// applyWVATriggerMetadata moves the stashed WVA configuration into every trigger.
//
// Trigger metadata is how WVA is configured now: KEDA delivers it verbatim as
// scalerMetadata on every external-scaler call, and that call is what registers
// the workload. The llm-d.ai/* annotations that used to carry this are gone.
func applyWVATriggerMetadata(so *kedav1alpha1.ScaledObject) {
	// A POOL trigger first: it carries no modelID, so the model path below
	// would return early and leave the ScaledObject declaring nothing.
	if raw, isPool := so.Annotations[pendingWarmPoolKey]; isPool {
		delete(so.Annotations, pendingWarmPoolKey)
		payload := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return
		}
		for i := range so.Spec.Triggers {
			if so.Spec.Triggers[i].Metadata == nil {
				so.Spec.Triggers[i].Metadata = make(map[string]string)
			}
			for k, v := range payload {
				so.Spec.Triggers[i].Metadata[k] = v
			}
		}
		return
	}

	modelID, ok := so.Annotations[pendingModelIDKey]
	if !ok {
		return
	}
	cost := so.Annotations[pendingVariantCostKey]
	policy := so.Annotations[pendingScalingPolicyKey]
	delete(so.Annotations, pendingModelIDKey)
	delete(so.Annotations, pendingVariantCostKey)
	delete(so.Annotations, pendingScalingPolicyKey)

	for i := range so.Spec.Triggers {
		if so.Spec.Triggers[i].Metadata == nil {
			so.Spec.Triggers[i].Metadata = make(map[string]string)
		}
		so.Spec.Triggers[i].Metadata[registry.ModelIDKey] = modelID
		if cost != "" {
			so.Spec.Triggers[i].Metadata[registry.VariantCostKey] = cost
		}
		if policy != "" {
			so.Spec.Triggers[i].Metadata[registry.ScalingPolicyKey] = policy
		}
	}
}

// defaultTriggers returns the trigger every fixture gets unless an option
// replaces it: KEDA calling WVA's external scaler over gRPC.
//
// The prometheus fallback below is only reachable when no address has been set,
// which in a configured suite means a programming error rather than a supported
// mode — it is kept so a fixture built outside the suite still produces a valid
// object rather than one the CRD rejects for having no triggers.
func defaultTriggers(prometheusURL, query string) []kedav1alpha1.ScaleTriggers {
	if externalScalerAddr != "" {
		return []kedav1alpha1.ScaleTriggers{{
			Type: "external",
			Name: "wva-external-scaler",
			Metadata: map[string]string{
				"scalerAddress": externalScalerAddr,
			},
		}}
	}
	return []kedav1alpha1.ScaleTriggers{{
		Type: "prometheus",
		Name: "wva-desired-replicas",
		Metadata: map[string]string{
			"serverAddress":       prometheusURL,
			"query":               query,
			"threshold":           "1",
			"activationThreshold": "0",
			"metricType":          "Value", // desired replicas is an absolute gauge; use value directly, not per-pod average
			"unsafeSsl":           "true",
		},
	}}
}

// WithExternalScalerTrigger replaces the ScaledObject's triggers with a single
// KEDA external trigger pointing at WVA's external-scaler gRPC service, so KEDA
// receives WVA's decision directly (over gRPC) instead of reading the
// wva_desired_replicas gauge from Prometheus. scalerAddress is host:port, e.g.
// "wva-external-scaler.<wva-namespace>.svc.cluster.local:9090".
func WithExternalScalerTrigger(scalerAddress string) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		so.Spec.Triggers = []kedav1alpha1.ScaleTriggers{
			{
				Type: "external",
				Name: "wva-external-scaler",
				Metadata: map[string]string{
					"scalerAddress": scalerAddress,
				},
			},
		}
	}
}

// WithExternalScalerPushTrigger is WithExternalScalerTrigger's push variant: a
// `external-push` trigger, which makes KEDA hold a StreamIsActive stream open so
// WVA pushes activation instead of KEDA polling for it.
//
// Scale-from-zero needs this one. A workload parked at 0 replicas is woken only
// by the activation signal, and WVA no longer patches the scale subresource
// itself — KEDA is the sole writer — so the push stream is what carries
// "requests are queued, wake up" from the scale-from-zero engine to KEDA.
func WithExternalScalerPushTrigger(scalerAddress string) ScaledObjectOption {
	return func(so *kedav1alpha1.ScaledObject) {
		so.Spec.Triggers = []kedav1alpha1.ScaleTriggers{
			{
				Type: "external-push",
				Name: "wva-external-scaler",
				Metadata: map[string]string{
					"scalerAddress": scalerAddress,
				},
			},
		}
	}
}

// CreateScaledObject creates a KEDA ScaledObject for WVA. Fails if it already exists.
func CreateScaledObject(
	ctx context.Context,
	crClient client.Client,
	namespace, name, scaleTargetName, variantName string,
	minReplicas, maxReplicas int32,
	monitoringNamespace string,
	opts ...ScaledObjectOption,
) error {
	return crClient.Create(ctx, buildScaledObject(namespace, name, scaleTargetName, variantName, minReplicas, maxReplicas, monitoringNamespace, opts...))
}

// scaledObjectRef returns a minimal typed object for ScaledObject identity (Get/Delete).
// name is the base name; the ScaledObject resource name is name + scaledObjectSuffix.
func scaledObjectRef(namespace, name string) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name + scaledObjectSuffix,
		},
	}
}

// DeleteScaledObject deletes the ScaledObject. Idempotent; ignores NotFound.
func DeleteScaledObject(ctx context.Context, crClient client.Client, namespace, name string) error {
	err := crClient.Delete(ctx, scaledObjectRef(namespace, name))
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete ScaledObject %s: %w", name+scaledObjectSuffix, err)
	}
	return nil
}

// EnsureScaledObject creates or replaces the ScaledObject (idempotent for test setup).
func EnsureScaledObject(
	ctx context.Context,
	crClient client.Client,
	namespace, name, scaleTargetName, variantName string,
	minReplicas, maxReplicas int32,
	monitoringNamespace string,
	opts ...ScaledObjectOption,
) error {
	obj := buildScaledObject(namespace, name, scaleTargetName, variantName, minReplicas, maxReplicas, monitoringNamespace, opts...)
	existing := scaledObjectRef(namespace, name)
	key := client.ObjectKey{Namespace: namespace, Name: obj.GetName()}
	err := crClient.Get(ctx, key, existing)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("check existing ScaledObject %s: %w", obj.GetName(), err)
		}
	} else {
		deleteErr := crClient.Delete(ctx, existing)
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete existing ScaledObject %s: %w", obj.GetName(), deleteErr)
		}
		waitErr := wait.PollUntilContextTimeout(ctx, 1*time.Second, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			check := scaledObjectRef(namespace, name)
			getErr := crClient.Get(ctx, key, check)
			return errors.IsNotFound(getErr), nil
		})
		if waitErr != nil {
			return fmt.Errorf("timeout waiting for ScaledObject %s deletion: %w", obj.GetName(), waitErr)
		}
	}
	return crClient.Create(ctx, obj)
}

func buildScaledObject(namespace, name, scaleTargetName, variantName string, minReplicas, maxReplicas int32, monitoringNamespace string, opts ...ScaledObjectOption) *kedav1alpha1.ScaledObject {
	objName := name + scaledObjectSuffix
	prometheusURL := "https://kube-prometheus-stack-prometheus." + monitoringNamespace + ".svc.cluster.local:9090"
	// Prometheus renames the metric's namespace label to exported_namespace when the scrape
	// target's namespace (workload-variant-autoscaler-system) differs from the label value
	// (the workload namespace, e.g. llm-d-sim). Use exported_namespace to match what
	// Prometheus actually stores.
	query := fmt.Sprintf("wva_desired_replicas{variant_name=%q,exported_namespace=%q}", variantName, namespace)

	spec := kedav1alpha1.ScaledObjectSpec{
		ScaleTargetRef: &kedav1alpha1.ScaleTarget{
			APIVersion: apiVersionAppsV1,
			Kind:       kindDeployment,
			Name:       scaleTargetName,
		},
		PollingInterval: ptr.To(int32(5)),
		CooldownPeriod:  ptr.To(int32(30)),
		MinReplicaCount: ptr.To(minReplicas),
		MaxReplicaCount: ptr.To(maxReplicas),
		Triggers:        defaultTriggers(prometheusURL, query),
	}
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objName,
			Namespace: namespace,
			Labels:    map[string]string{"test-resource": defaultTestResourceLabelValue},
		},
		Spec: spec,
	}
	for _, opt := range opts {
		opt(so)
	}
	// After the options, so it lands on whatever triggers they left behind.
	applyWVATriggerMetadata(so)
	return so
}
