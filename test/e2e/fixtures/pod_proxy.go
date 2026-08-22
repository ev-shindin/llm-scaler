package fixtures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
)

// Talking to a container port from a test, through the API server.
//
// Three ways exist and two are wrong here.
//
// A port-forward does not merely fail when nothing listens on the target port:
// it poisons the whole forward, including the other ports named in the same
// command, and then returns the poll loop's own duration as though it were a
// measurement. That cost a day of an earlier investigation, and the ports this
// suite talks to are precisely the ones that are sometimes not listening.
//
// An exec into the Pod works, but `remotecommand` drags in three modules this
// repo does not otherwise depend on — a websocket library, a SPDY stream
// implementation and a flow-rate limiter — which is a lot of supply chain for a
// test helper.
//
// The pod proxy subresource needs none of that. The API server dials the
// container port and relays the response, so it supports every verb, reports a
// refusal as an ordinary error, and adds nothing to go.mod.
//
// What it cannot do, and what this suite therefore does not claim to test: it
// arrives from the control plane rather than from another Pod, so it says
// nothing about whether a NetworkPolicy admits or denies a peer.

// PodProxyResult is one request relayed to a container port.
type PodProxyResult struct {
	Status int
	Body   string
}

// JSON decodes the body, for endpoints that return some.
func (r PodProxyResult) JSON() (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		return nil, fmt.Errorf("decode %q: %w", r.Body, err)
	}
	return out, nil
}

// PodProxy makes a request to a container port through the API server.
//
// A non-2xx answer is returned as a Status, NOT as an error: several assertions
// in this suite are that a request is refused with a particular code, and an
// error would throw that away. Only a failure to reach the port at all comes
// back as an error.
func PodProxy(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	namespace, pod string,
	port int,
	method, path, body string,
) (PodProxyResult, error) {
	// The proxy subresource addresses a port as "<pod>:<port>".
	target := fmt.Sprintf("%s:%d", pod, port)

	req := clientset.CoreV1().RESTClient().Verb(method).
		Namespace(namespace).
		Resource("pods").
		Name(target).
		SubResource("proxy").
		Suffix(strings.TrimPrefix(path, "/"))

	if body != "" {
		req = req.Body([]byte(body)).SetHeader("Content-Type", "application/json")
	}

	raw, err := req.DoRaw(ctx)
	if err == nil {
		return PodProxyResult{Status: 200, Body: string(raw)}, nil
	}

	// An HTTP status the container returned reaches us as a StatusError whose
	// code is the container's, with the body still in raw. That is an answer,
	// not a transport failure.
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		if code := int(status.Status().Code); code > 0 {
			return PodProxyResult{Status: code, Body: string(raw)}, nil
		}
	}
	return PodProxyResult{}, fmt.Errorf("proxy %s %s to %s/%s: %w", method, path, namespace, target, err)
}
