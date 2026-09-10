package collector

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// podServingState is what a Pod's own object says about whether it can serve.
//
// Separate from anything Prometheus reports, because a series says only that
// something answered a scrape. It does not say the Pod still exists, is not
// being deleted, or is in the rotation.
type podServingState struct {
	// live is false for a Pod that is being deleted. Its capacity is going away
	// and counting it holds the fleet up on supply that is about to vanish.
	live bool
	// ready is the kubelet's readiness verdict, which is what decides whether a
	// Service or an EPP will route to the Pod at all.
	ready bool
	// startedAt is when the kubelet started the Pod, zero when it has not yet
	// (or the field is unset). It bounds how long any request this Pod reports
	// can possibly have taken -- see podUptime, and the guard in
	// collectReplicaMetrics that spends it.
	startedAt time.Time
}

// namespacePods is one namespace's listing, plus whether it could be read.
//
// The two are not the same and conflating them is dangerous. A listing that
// SUCCEEDED and does not contain a Pod says the Pod is gone. A listing that
// FAILED says nothing at all -- and treating that as "every Pod is gone" would
// drop every series in the namespace, hand the analyzer zero supply, and scale
// the fleet to the floor on an RBAC error or a moment of API unavailability.
type namespacePods struct {
	byName map[string]podServingState
	// listed is false when the read failed. Callers fail OPEN on it: behave
	// exactly as WVA did before Pod state was consulted at all.
	listed bool
}

// podStates lists every Pod in a namespace and reduces each to the two facts
// attribution needs.
//
// A LIST rather than a Get per Pod, and an UNCACHED read rather than the
// manager's client. One call per namespace per cycle answers for every Pod at
// once, where a Get per reporting Pod would be dozens; and the manager's cache
// holds no Pods, so reading through it would start a Pod informer -- namespace
// -wide on a scoped install, CLUSTER-wide on a cluster-scoped one, which is
// exactly the memory cost the ConfigMap cache is configured to avoid.
//
// These facts are also MUTABLE, which is why they are not put in the locator's
// resolution cache beside the scale target and labels. That cache is correct
// precisely because what it holds -- ownerReferences -- cannot change. Readiness
// changes minute to minute, and a deletion is the one event that must never be
// served from a cache.
func podStates(ctx context.Context, reader client.Reader, namespace string) namespacePods {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return namespacePods{listed: false}
	}
	out := make(map[string]podServingState, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		state := podServingState{
			live:  p.DeletionTimestamp == nil,
			ready: podIsReady(p),
		}
		if p.Status.StartTime != nil {
			state.startedAt = p.Status.StartTime.Time
		}
		out[p.Name] = state
	}
	return namespacePods{byName: out, listed: true}
}

// podIsReady reports the Pod's Ready condition.
//
// The CONDITION, not the phase. A Pod stays in phase Running while its
// containers fail their probes, so phase alone would call a Pod ready that
// nothing is routing to -- which is the state a rolling replacement spends its
// first seconds in, and the one an engine spends serving /metrics before it can
// serve a request.
func podIsReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// servingState answers for one Pod, listing the namespace once per cycle.
//
// Outside a cycle there is no memo and every call lists. That is the same
// bargain BeginCycle already makes for the queries: correct either way, shared
// only when a caller has said a cycle is open.
//
// Returns the state, whether the listing CONTAINED this Pod, and whether the
// listing could be read at all. Callers must keep the last two apart: absent
// from a good listing means gone, while an unreadable listing means unknown and
// must not be acted on.
func (c *ReplicaMetricsCollector) servingState(
	ctx context.Context, namespace, podName string,
) (state podServingState, found, listed bool) {
	if podName == "" || c.apiReader == nil {
		return podServingState{}, false, false
	}

	c.cycleMu.Lock()
	if c.cyclePods != nil {
		if cached, ok := c.cyclePods[namespace]; ok {
			c.cycleMu.Unlock()
			st, in := cached.byName[podName]
			return st, in, cached.listed
		}
	}
	c.cycleMu.Unlock()

	pods := podStates(ctx, c.apiReader, namespace)

	c.cycleMu.Lock()
	if c.cyclePods != nil {
		c.cyclePods[namespace] = pods
	}
	c.cycleMu.Unlock()

	st, in := pods.byName[podName]
	return st, in, pods.listed
}

// podIsGone reports a Pod that a GOOD listing says is absent or being deleted.
//
// False whenever the listing could not be read, so a failed List behaves exactly
// as WVA did before Pod state was consulted: every series is kept. Dropping them
// instead would turn an RBAC error into an empty fleet.
func (c *ReplicaMetricsCollector) podIsGone(ctx context.Context, namespace, podName string) bool {
	state, found, listed := c.servingState(ctx, namespace, podName)
	if !listed {
		return false
	}
	return !found || !state.live
}

// podReady reports whether the Pod is in the rotation.
//
// Fails OPEN: an unreadable listing answers ready, which is the behaviour before
// this existed. The alternative -- calling every Pod unready -- would publish a
// serving count of zero, and zero is a load-bearing answer meaning "its replicas
// are demonstrably not serving", which is not what an API error establishes.
func (c *ReplicaMetricsCollector) podReady(ctx context.Context, namespace, podName string) bool {
	state, found, listed := c.servingState(ctx, namespace, podName)
	if !listed {
		return true
	}
	return found && state.ready
}

// podUptime reports how long the Pod has been running, and whether that could
// be established at all.
//
// False whenever the listing could not be read, the Pod is not in it, or it
// carries no start time — the same fail-open contract as podReady. A caller
// bounding a metric by uptime must not treat "unknown" as "zero", which would
// make every value look impossible.
func (c *ReplicaMetricsCollector) podUptime(
	ctx context.Context, namespace, podName string, now time.Time,
) (time.Duration, bool) {
	state, found, listed := c.servingState(ctx, namespace, podName)
	if !listed || !found || state.startedAt.IsZero() {
		return 0, false
	}
	uptime := now.Sub(state.startedAt)
	if uptime < 0 {
		// A start time in the future is clock skew between this process and the
		// node, not a Pod that has not started. Unknown rather than zero.
		return 0, false
	}
	return uptime, true
}

// seriesPodName pulls the Pod identity out of a series' labels.
//
// Two conventions, because the scrape decides which: the Prometheus operator's
// target relabeling produces `pod`, while raw scrape jobs and
// kube-state-metrics-style configs produce `pod_name`. Shared so that every path
// which asks "which Pod is this series from" answers it the same way.
func seriesPodName(labels map[string]string) string {
	if p := labels["pod"]; p != "" {
		return p
	}
	return labels["pod_name"]
}

// SeriesPodIsGone reports whether the Pod behind a series has been deleted or is
// terminating, so a caller holding raw series can drop it.
//
// Exported for the analyzers that do NOT go through the collector's rows. An
// external analyzer runs the operator's own PromQL and sums the result itself,
// so it never reaches buildInstanceKey -- without this it would count deleted
// Pods for as long as Prometheus keeps their series, which is the same defect
// the builder exists to prevent, reached by a path the builder cannot see.
//
// A series with no Pod label is never dropped. That is an ALREADY-AGGREGATED
// query, where Prometheus did the summing and there is no per-Pod identity left
// to judge; the honest answer is to leave it alone rather than guess.
func (c *ReplicaMetricsCollector) SeriesPodIsGone(ctx context.Context, namespace string, labels map[string]string) bool {
	podName := seriesPodName(labels)
	if podName == "" {
		return false
	}
	return c.podIsGone(ctx, namespace, podName)
}
