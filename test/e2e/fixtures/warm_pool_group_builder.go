package fixtures

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// A warm pool whose unit spans Pods, built as a LeaderWorkerSet.
//
// This exists because the controller treats a group as ONE lendable unit and
// nothing outside unit tests proved it: capacityOf multiplies the leader's
// device count by the group size, ListWarm drops workers, and a group missing a
// Ready Pod is not offered at all. All three are invisible to a Deployment-based
// pool, which is what every other warm pool spec builds.
//
// The workers run a container that does nothing but stay Ready. That is enough,
// and it is the point: the controller must never talk to a worker. Only the
// leader runs the supervisor, so a worker that answered anything would be
// hiding the very mistake these specs are for.

// CreateWarmPoolGroup creates a LeaderWorkerSet warm pool of spec.GroupSize Pods
// per group. The leader carries the supervisor and proxy, exactly as a
// single-Pod pool's only Pod does.
func CreateWarmPoolGroup(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	crClient client.Client,
	spec WarmPoolSpec,
) error {
	if spec.GroupSize < 2 {
		return fmt.Errorf("warm pool group %s: GroupSize must be at least 2, got %d",
			spec.Name, spec.GroupSize)
	}
	// The supervisor script is mounted the same way as for a Deployment pool.
	if err := createWarmPoolScript(ctx, clientset, spec); err != nil {
		return err
	}

	labels := map[string]string{
		"app.kubernetes.io/name": "workload-variant-autoscaler",
		WarmPoolComponentLabel:   WarmPoolComponentValue,
		"warm-pool-fixture":      spec.Name,
	}
	if spec.PoolName != "" {
		labels[WarmPoolNameLabel] = spec.PoolName
	}

	replicas := spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	supervisor := warmPoolSupervisorContainer()
	if spec.GPUs > 0 {
		// PER POD. The controller multiplies by the group size to get the unit's
		// devices, so writing the group total here would count them twice.
		supervisor.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				gpuResourceName: *resource.NewQuantity(int64(spec.GPUs), resource.DecimalSI),
			},
		}
	}

	leader := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false),
			NodeName:                     spec.NodeName,
			Volumes: []corev1.Volume{{
				Name: "script",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: spec.Name + warmPoolScriptSuffix,
						},
					},
				},
			}},
			Containers: []corev1.Container{
				supervisor,
				warmPoolProxyContainer(spec.ProxyImage),
			},
		},
	}

	// The worker carries the pool labels too, because the controller finds pool
	// Pods by label and must then decline the workers ITSELF. Labelling only the
	// leader would make the spec pass without the worker-index check existing.
	workerContainer := corev1.Container{
		Name:    "rank",
		Image:   warmPoolEmulatorImage,
		Command: []string{"sh", "-c", "sleep infinity"},
	}
	if spec.GPUs > 0 {
		workerContainer.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				gpuResourceName: *resource.NewQuantity(int64(spec.GPUs), resource.DecimalSI),
			},
		}
	}
	worker := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr.To(false),
			NodeName:                     spec.NodeName,
			Containers:                   []corev1.Container{workerContainer},
		},
	}

	size := int32(spec.GroupSize)
	set := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.Name,
			Namespace:   spec.Namespace,
			Labels:      labels,
			Annotations: spec.Annotations,
		},
		Spec: lwsv1.LeaderWorkerSetSpec{
			Replicas: ptr.To(replicas),
			// Required by the API server: the field has no server-side default,
			// so an unset value is rejected outright rather than filled in.
			// LeaderCreated starts the workers as soon as the leader Pod exists,
			// which is what a pool wants -- the leader's readiness depends on a
			// supervisor that takes a moment, and the ranks need not wait for it.
			StartupPolicy: lwsv1.LeaderCreatedStartupPolicy,
			LeaderWorkerTemplate: lwsv1.LeaderWorkerTemplate{
				Size:           ptr.To(size),
				LeaderTemplate: &leader,
				WorkerTemplate: worker,
				RestartPolicy:  lwsv1.RecreateGroupOnPodRestart,
			},
		},
	}
	if err := crClient.Create(ctx, set); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create warm pool group %s: %w", spec.Name, err)
	}
	return nil
}

// DeleteWarmPoolGroup removes the LeaderWorkerSet and the script it mounted.
func DeleteWarmPoolGroup(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	crClient client.Client,
	spec WarmPoolSpec,
) error {
	set := &lwsv1.LeaderWorkerSet{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace},
	}
	if err := crClient.Delete(ctx, set); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete warm pool group %s: %w", spec.Name, err)
	}
	if err := clientset.CoreV1().ConfigMaps(spec.Namespace).Delete(
		ctx, spec.Name+warmPoolScriptSuffix, metav1.DeleteOptions{},
	); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete warm pool group script %s: %w", spec.Name, err)
	}
	return nil
}
