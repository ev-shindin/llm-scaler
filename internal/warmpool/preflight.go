package warmpool

import (
	"context"
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CanBorrow reports whether this controller may do the one thing a borrow
// requires: label a pool Pod into a model's InferencePool.
//
// Without it the pool still ADMITS -- it loads models, holds GPUs, and reports
// itself healthy -- and then fails at the only moment that matters, once per
// spike, in a code path an operator sees only if they are reading logs at the
// time. That is the worst shape a permission error can take, and it is why this
// is asked at startup rather than discovered at the first borrow.
//
// A SelfSubjectAccessReview is the right question because it asks the API server
// what THIS identity may do, rather than reading a Role and re-implementing
// Kubernetes' own aggregation of Roles, ClusterRoles and bindings.
func CanBorrow(ctx context.Context, c client.Client, namespace string) (bool, error) {
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "patch",
				Group:     "",
				Resource:  "pods",
			},
		},
	}
	if err := c.Create(ctx, review); err != nil {
		return false, fmt.Errorf("asking whether this controller may patch pods in %s: %w", namespace, err)
	}
	return review.Status.Allowed, nil
}

// BorrowDenied is what to tell an operator when the answer is no. Written once
// here so the message and the manifest cannot drift apart.
const BorrowDenied = "the warm pool is disabled: this controller may not patch Pods, so it could " +
	"never lend one. Grant patch on pods in the pool namespace -- verbs get, list, watch, patch -- " +
	"and restart. Running without it would hold GPUs, warm models, and fail every borrow."
