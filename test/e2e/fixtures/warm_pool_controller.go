//go:build e2e

package fixtures

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// warmPoolNamespaceFlag is the argument that turns the pool on.
const warmPoolNamespaceFlag = "--warm-pool-namespace="

// ControllerDeployment is the WVA controller, wherever the suite installed it.
type ControllerDeployment struct {
	Namespace string
	Name      string
}

// EnableWarmPool restarts the controller with the warm pool watching a
// namespace, and returns a function that puts the arguments back.
//
// The suite's controller is deployed WITHOUT warm-pool flags, so every policy
// the pool implements -- what it admits, what it declines, which pool a Pod
// belongs to -- was unreachable from an e2e until this existed. That is the gap
// this closes: not a test that was never run, but a scenario that could not be
// built.
//
// Mutating a shared Deployment mid-suite is not free, which is why the restore
// is returned rather than left to the caller to remember, and why specs that use
// this should be Ordered and serial. The alternative -- a second controller
// deployment -- would need its own RBAC, service and leader-election identity,
// and would still race the first one for the same ScaledObjects.
func EnableWarmPool(ctx context.Context, clientset *kubernetes.Clientset, c ControllerDeployment, poolNamespace string) (func(context.Context) error, error) {
	before, err := clientset.AppsV1().Deployments(c.Namespace).Get(ctx, c.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read controller %s/%s: %w", c.Namespace, c.Name, err)
	}
	original := append([]string(nil), before.Spec.Template.Spec.Containers[0].Args...)

	withFlag := append([]string(nil), original...)
	replaced := false
	for i, a := range withFlag {
		if strings.HasPrefix(a, warmPoolNamespaceFlag) {
			withFlag[i] = warmPoolNamespaceFlag + poolNamespace
			replaced = true
			break
		}
	}
	if !replaced {
		withFlag = append(withFlag, warmPoolNamespaceFlag+poolNamespace)
	}

	if err := setControllerArgs(ctx, clientset, c, withFlag); err != nil {
		return nil, err
	}
	restore := func(ctx context.Context) error {
		return setControllerArgs(ctx, clientset, c, original)
	}
	if err := WaitForControllerReady(ctx, clientset, c); err != nil {
		// Put it back before failing, or every later spec runs against a
		// controller this one broke.
		_ = restore(context.WithoutCancel(ctx))
		return nil, err
	}
	return restore, nil
}

func setControllerArgs(ctx context.Context, clientset *kubernetes.Clientset, c ControllerDeployment, args []string) error {
	d, err := clientset.AppsV1().Deployments(c.Namespace).Get(ctx, c.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read controller %s/%s: %w", c.Namespace, c.Name, err)
	}
	d.Spec.Template.Spec.Containers[0].Args = args
	if _, err := clientset.AppsV1().Deployments(c.Namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("set controller args: %w", err)
	}
	return nil
}

// WaitForControllerReady waits until the controller has exactly one ready Pod
// on the current generation.
//
// The generation check is what makes this a wait for the NEW controller rather
// than the old one: immediately after an Update the previous Pod is still ready,
// and a naive readiness check returns at once and reads the old process's log.
// That is not hypothetical -- it produced a "the pool started fine" reading from
// a Pod that predated the change it was meant to prove.
func WaitForControllerReady(ctx context.Context, clientset *kubernetes.Clientset, c ControllerDeployment) error {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		d, err := clientset.AppsV1().Deployments(c.Namespace).Get(ctx, c.Name, metav1.GetOptions{})
		if err == nil &&
			d.Status.ObservedGeneration >= d.Generation &&
			d.Status.UpdatedReplicas == *d.Spec.Replicas &&
			d.Status.ReadyReplicas == *d.Spec.Replicas &&
			d.Status.UnavailableReplicas == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("controller %s/%s did not become ready", c.Namespace, c.Name)
}

// NewestControllerPod is the controller Pod that started last.
//
// Reading "the" controller Pod by label picks an arbitrary one, and during a
// rollout that is as likely to be the outgoing process as the incoming one. Its
// log then shows the behaviour of the configuration the test just replaced.
func NewestControllerPod(ctx context.Context, clientset *kubernetes.Clientset, c ControllerDeployment) (*corev1.Pod, error) {
	pods, err := clientset.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "control-plane=controller-manager",
	})
	if err != nil {
		return nil, fmt.Errorf("list controller pods: %w", err)
	}
	var newest *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	if newest == nil {
		return nil, fmt.Errorf("no running controller pod in %s", c.Namespace)
	}
	return newest, nil
}

// LabelNodeAccelerator makes a node report one GPU product, and returns a
// restore.
//
// Sets EVERY vendor's product label rather than NVIDIA's alone. The controller
// resolves a node's accelerator by walking the vendor list in reverse and taking
// the first match, so on a node that already carries another vendor's label the
// NVIDIA one never wins -- a kind worker labelled amd.com/gpu.product-name
// reported AMD-MI300X-192G no matter what this wrote, and the spec asserting on
// its own value failed for a reason that had nothing to do with the pool.
//
// Writing them all means the expected value wins whatever the precedence is,
// which keeps the test about the property it names: the accelerator the pool
// reports is the one its Pods' NODE declares.
func LabelNodeAccelerator(ctx context.Context, clientset *kubernetes.Clientset, node, product string) (func(context.Context) error, error) {
	before, err := clientset.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read node %s: %w", node, err)
	}
	original := map[string]*string{}
	for _, key := range acceleratorProductLabels() {
		if v, had := before.Labels[key]; had {
			v := v
			original[key] = &v
		} else {
			original[key] = nil
		}
		if err := patchNodeLabel(ctx, clientset, node, key, &product); err != nil {
			return nil, err
		}
	}
	return func(ctx context.Context) error {
		var firstErr error
		for key, value := range original {
			if err := patchNodeLabel(ctx, clientset, node, key, value); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}, nil
}

// acceleratorProductLabels is every vendor key that names a GPU model, read from
// the same constants the controller resolves against so the two cannot drift.
func acceleratorProductLabels() []string {
	keys := make([]string, 0, len(constants.VendorResources))
	for _, v := range constants.VendorResources {
		keys = append(keys, v.ProductLabel)
	}
	return keys
}

func patchNodeLabel(ctx context.Context, clientset *kubernetes.Clientset, node, key string, value *string) error {
	n, err := clientset.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read node %s: %w", node, err)
	}
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	if value == nil {
		delete(n.Labels, key)
	} else {
		n.Labels[key] = *value
	}
	if _, err := clientset.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("label node %s: %w", node, err)
	}
	return nil
}

// SchedulableNodes returns worker nodes, newest name order, for a test that
// needs somewhere specific to put a Pod.
func SchedulableNodes(ctx context.Context, clientset *kubernetes.Clientset) ([]corev1.Node, error) {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var out []corev1.Node
	for _, n := range nodes.Items {
		if _, isControlPlane := n.Labels["node-role.kubernetes.io/control-plane"]; isControlPlane {
			continue
		}
		if n.Spec.Unschedulable {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}
