package warmpool

import (
	"context"
	"errors"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// reviewedBy builds a client whose SelfSubjectAccessReview creates come back
// with the given verdict, and records what was asked.
func reviewedBy(t *testing.T, allowed bool, createErr error) (client.Client, *authorizationv1.ResourceAttributes) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	asked := &authorizationv1.ResourceAttributes{}
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			if createErr != nil {
				return createErr
			}
			review, ok := obj.(*authorizationv1.SelfSubjectAccessReview)
			if !ok {
				t.Fatalf("expected a SelfSubjectAccessReview, got %T", obj)
			}
			if review.Spec.ResourceAttributes != nil {
				*asked = *review.Spec.ResourceAttributes
			}
			review.Status.Allowed = allowed
			return nil
		},
	}).Build()
	return c, asked
}

func TestTheReviewAsksExactlyWhatABorrowNeeds(t *testing.T) {
	// A borrow labels a pool Pod into the model's InferencePool. Asking about
	// any other verb or resource would answer a question nobody has, and pass
	// while the real one fails.
	c, asked := reviewedBy(t, true, nil)
	allowed, err := CanBorrow(context.Background(), c, "pool-ns")
	if err != nil {
		t.Fatalf("CanBorrow: %v", err)
	}
	if !allowed {
		t.Fatal("an allowing API server must read as allowed")
	}
	if asked.Verb != "patch" || asked.Resource != "pods" || asked.Namespace != "pool-ns" {
		t.Fatalf("asked the wrong question: %+v", asked)
	}
	if asked.Group != "" {
		t.Errorf("pods are core, not %q", asked.Group)
	}
}

func TestADenialIsReportedAsADenial(t *testing.T) {
	c, _ := reviewedBy(t, false, nil)
	allowed, err := CanBorrow(context.Background(), c, "pool-ns")
	if err != nil {
		t.Fatalf("a denial is an answer, not an error: %v", err)
	}
	if allowed {
		t.Fatal("a denying API server must read as denied")
	}
}

func TestAnUnanswerableReviewIsAnErrorNotADenial(t *testing.T) {
	// The caller starts the pool when the check itself fails, so this must be
	// distinguishable from a denial. Folding a broken API call into "denied"
	// would disable the pool over an unrelated fault -- a worse failure than the
	// one the check guards against.
	c, _ := reviewedBy(t, false, errors.New("apiserver unreachable"))
	if _, err := CanBorrow(context.Background(), c, "pool-ns"); err == nil {
		t.Fatal("a failed review must return an error, not a silent denial")
	}
}

func TestTheDenialMessageSaysWhatToGrantAndWhyItMatters(t *testing.T) {
	// The whole point of asking at startup: without patch the pool admits
	// happily, holds GPUs, and fails only at the moment of a borrow.
	for _, want := range []string{"patch", "pods", "restart"} {
		if !strings.Contains(BorrowDenied, want) {
			t.Errorf("the message must mention %q: %q", want, BorrowDenied)
		}
	}
}
