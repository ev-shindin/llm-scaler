package controller

// This file centralizes the +kubebuilder:rbac markers for cluster resources the
// WVA controller and optimization engine read or scale. They previously lived on
// the VariantAutoscaling reconciler, which has been removed; the permissions are
// still required by the collector and the saturation / scale-from-zero engines.
// `make manifests` aggregates these into config/base/rbac/manager-clusterrole.yaml.

// Scale targets and their pods.
//
// READ ONLY, and that is the whole architecture rather than an oversight: KEDA
// owns the HPA and performs every write. WVA computes a replica count and hands
// it over gRPC (internal/actuator publishes to an in-process decision.Set); it
// never calls Update, Patch or the scale subresource on anything.
//
// The update/patch verbs on deployments, deployments/scale and the LeaderWorkerSet
// equivalents were granted and never exercised — a cluster-wide licence to resize
// any workload, which is the single permission a cluster admin is most right to
// refuse. Do not re-add them without a caller: if one ever appears, the design
// question ("why is WVA writing when KEDA actuates?") comes first.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale,verbs=get
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="apps",resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets,verbs=get;list;watch
// +kubebuilder:rbac:groups=leaderworkerset.x-k8s.io,resources=leaderworkersets/scale,verbs=get

// Node inventory for accelerator/capacity discovery.
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;list;watch

// Core resources read during optimization and metrics collection.
//
// Pods also take `patch`, and only for the warm pool: joining a woken pool Pod
// to a model's InferencePool means writing that pool's selector labels onto it,
// and leaving means removing them. Without this every borrow fails at the label
// write -- and worse, every RETURN fails after the proxy has already been
// cleared, leaving a Pod out of service with an awake model still holding its
// GPU and no timeout that recovers it.
//
// `pods/status` is deliberately NOT taken: readiness is reported by the pool
// Pod's own probe, so nothing here writes Pod status.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// Note: Namespace watch permission is required for label-based namespace opt-in for namespace-local ConfigMaps.

// ScaledObjects, read one at a time to resolve a registered workload's scale
// target and min/max envelope (registry.Enricher, and the external scaler's own
// lookups).
//
// GET ONLY, deliberately. The markers that used to grant this rule belonged to
// the ScaledObject and HPA reconcilers and went with them, leaving the checked-in
// ClusterRole correct only because nothing had regenerated it — the next
// `make manifests` silently drops the permission, and enrichment then fails for
// every workload, which is the whole of discovery.
//
// list and watch are NOT granted and must not be: a cached/listing read is served
// by a cluster-wide informer, the very LIST+WATCH this design exists to remove.
// Every read goes through mgr.GetAPIReader(). Nothing writes them — KEDA owns
// these objects.
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get

// InferencePool discovery and controller-owned ServiceMonitor observation.
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch
// +kubebuilder:rbac:groups=inference.networking.x-k8s.io;inference.networking.k8s.io,resources=inferencepools,verbs=get;list;watch
