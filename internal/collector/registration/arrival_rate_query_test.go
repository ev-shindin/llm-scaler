package registration

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
)

// The model arrival-rate query is one string that no other test would notice
// changing, and every way of breaking it fails the same way: a PromQL error or
// an empty result, both of which CollectModelArrivalRate reports as a zero
// arrival rate, which is indistinguishable from idle traffic and permits
// scale-down.
//
// Two things about the shape of this guard, both learned by having it fail:
//
//   - it asserts on the query the REGISTRY BUILDS, not on the const. A version
//     that read the const passed green while `Template:` was replaced with the
//     pre-fix query -- the entire change reverted at the one line that decides
//     anything, with golangci-lint still at zero issues because the test's own
//     reference kept the orphaned const "used".
//   - whitespace is collapsed first, so reformatting the query for readability
//     is not reported as a semantic break.
//
// Substring counting cannot see operand order or a swapped operator, so the
// rules below are a floor and not a proof. Parsing the rendered string with
// promql/parser would be strictly better and is not done here only because it
// would promote github.com/prometheus/prometheus from an indirect dependency to
// a direct one.
//
// The measured cases are in docs/developer-guide/analyzer-evidence.md, "The
// arrival rate is a dispatch rate while the queue is building".
func TestModelArrivalRateQueryStructure(t *testing.T) {
	const ns = "test-ns"

	reg := source.NewSourceRegistry()
	if err := reg.Register("prometheus", prometheus.NewPrometheusSource(
		context.Background(), &mockPrometheusAPI{},
		prometheus.DefaultPrometheusSourceConfig())); err != nil {
		t.Fatalf("registering the prometheus source: %v", err)
	}
	RegisterArrivalRateQueries(reg)

	built, err := reg.Get("prometheus").QueryList().Build(
		QueryModelArrivalRate, map[string]string{source.ParamNamespace: ns})
	if err != nil {
		t.Fatalf("building %s: %v", QueryModelArrivalRate, err)
	}
	if strings.Contains(built, "{{") || strings.Contains(built, "}}") {
		t.Fatalf("the built query still has an unsubstituted template action: %s", built)
	}
	// Collapse runs of whitespace so the substring rules survive a reflow.
	q := strings.Join(strings.Fields(built), " ")

	rules := []struct {
		why   string // the mutation this rejects
		want  int
		found func(string) int
	}{{
		why:  "deleting group_left drops target_model_name, the collector then matches only the placement arm, and the fix is silently reverted",
		want: 1, found: count(`group_left(target_model_name)`),
	}, {
		why:  "the outer aggregation must keep target_model_name, or the collector's strict label filter discards every series",
		want: 1, found: count(`sum by (namespace, target_model_name) ((sum by (namespace, job)`),
	}, {
		why:  "the join key must stay (namespace, job); on (namespace) alone is not unique once a namespace has two pools",
		want: 1, found: count(`* on (namespace, job) group_left`),
	}, {
		// Four, not two: arrivalQueueByModel holds two selectors and is
		// referenced twice -- once for the label, once for the guard.
		why:  "both queue_size selectors must exclude the label-less sibling series an EPP also exports",
		want: 4, found: count(`target_model_name!=""`),
	}, {
		why:  "the single-model guard, so a pool serving several models falls through to placements rather than joining ambiguously",
		want: 1, found: count(`and on (namespace, job) (count by (namespace, job) (`),
	}, {
		// == 1 exactly. >= 1, >= 0 and == bool 1 all let a two-model pool
		// reach group_left and fail with "found duplicate series for the match
		// group"; == 2 drops the join on every single-model pool, restoring
		// the under-read silently.
		why:  "the guard must compare to exactly one",
		want: 1, found: count(`) == 1)`),
	}, {
		why:  "^ 0 is 1 for every finite value and for NaN; >= bool 0 is not",
		want: 1, found: count(`^ 0`),
	}, {
		why:  "nothing may scale the joined product: ^ 0 * 2 and ^ 0 + 1 both double or shift the rate",
		want: 0, found: regexpCount(`\^ 0\s*[*+\-/]`, ""),
	}, {
		why:  "the enqueue arm must be filtered, or a present-but-zero series masks a live placement rate",
		want: 1, found: count(`) > 0 or `),
	}, {
		why:  "the enqueue arm must be the primary one; swapping the top-level arms makes placements primary and reverts the fix",
		want: 1, found: count(`(rate(llm_d_epp_flow_control_request_enqueue`),
	}, {
		why:  "only successful placements count as arrivals, and nothing may negate that matcher",
		want: 2, found: count(`status="success"`),
	}, {
		why:  "no matcher may negate the success filter",
		want: 0, found: count(`status!=`),
	}, {
		why:  "every selector must be namespace-scoped, or one model's rate leaks into another's",
		want: 8, found: count(fmt.Sprintf("namespace=%q", ns)),
	}, {
		why:  "namespace must be matched exactly, never by regex",
		want: 0, found: count(`namespace=~`),
	}, {
		why:  "all four windows must be rate(...[1m]): a 5m window read 1.29 where the truth was 6.10 through a ramp",
		want: 4, found: count(`[1m]`),
	}, {
		why:  "no window other than 1m, including the units [smhdwy] omits",
		want: 0, found: regexpCount(`\[[0-9]+[a-z]\]`, `[1m]`),
	}, {
		why:  "no subquery: rate(...[1m])[5m:1m] changes what is averaged",
		want: 0, found: regexpCount(`\[[^\]]*:[^\]]*\]`, ""),
	}, {
		why:  "every rate must be rate(): increase() is 60x and irate() samples only the last two points",
		want: 4, found: count(`rate(`),
	}, {
		why:  "no increase() or irate()",
		want: 0, found: regexpCount(`\b(?:increase|irate)\(`, ""),
	}, {
		why:  "no offset and no @ modifier: both read a different instant than now",
		want: 0, found: regexpCount(`\boffset\b|@`, ""),
	}, {
		why:  "no _over_time wrapper: it changes the rate into an average of rates",
		want: 0, found: count(`_over_time(`),
	}, {
		why:  "aggregation must be sum, never avg or max, on the rate arms",
		want: 0, found: regexpCount(`(?:avg|max|min|topk|bottomk) by \(namespace, (?:job|target_model_name)\) \(rate`, ""),
	}, {
		why:  "nothing may clamp, cap or relabel the result",
		want: 0, found: regexpCount(`\b(?:clamp_max|clamp_min|clamp|label_replace|label_join|topk|bottomk)\(`, ""),
	}, {
		why:  "the current metric name must be read before the deprecated alias, for every pair",
		want: 3, found: orderedPairs,
	}, {
		why:  "the two names of a pair must be joined by `or`, never arithmetic: `+` doubles the rate and reads 12.20 where the truth is 6.10",
		want: 0, found: regexpCount(`\)\s*[+\-*/](?:\s*on\s*\([^)]*\))?\s*sum by`, ""),
	}, {
		why:  "the two names of a pair must be joined by `or`, not `and`/`unless`, which empty the arm",
		want: 0, found: regexpCount(`\)\s*(?:and|unless)\s*sum by`, ""),
	}}

	// The rules above all count substrings, so none of them notices something
	// APPENDED to the whole expression -- registering modelArrivalRateQuery+" * 2"
	// leaves every count intact and doubles the rate. Anchor the end.
	if !strings.HasSuffix(q, "[1m])))") {
		t.Errorf("arrival-rate query: must end with the placement arm's own closing parens, "+
			"or something has been appended to the whole expression; got tail %q", q[max(0, len(q)-40):])
	}

	for _, r := range rules {
		if got := r.found(q); got != r.want {
			t.Errorf("arrival-rate query: expected %d, found %d\n  rejects: %s\n  query:   %s",
				r.want, got, r.why, q)
		}
	}
	t.Logf("%d structural rules checked on the built query (%d chars)", len(rules), len(q))
}

func count(sub string) func(string) int {
	return func(q string) int { return strings.Count(q, sub) }
}

// regexpCount counts matches of pat, ignoring any match equal to exempt.
func regexpCount(pat, exempt string) func(string) int {
	re := regexp.MustCompile(pat)
	return func(q string) int {
		n := 0
		for _, m := range re.FindAllString(q, -1) {
			if m != exempt {
				n++
			}
		}
		return n
	}
}

// orderedPairs counts the metric pairs whose current llm_d_epp_ name is read
// before its deprecated inference_extension_ alias. Every pair must be.
func orderedPairs(q string) int {
	n := 0
	for _, suffix := range []string{
		"flow_control_request_enqueue_duration_seconds_count",
		"flow_control_queue_size",
		"scheduler_attempts_total",
	} {
		cur := strings.Index(q, "llm_d_epp_"+suffix)
		dep := strings.Index(q, "inference_extension_"+suffix)
		if cur >= 0 && dep >= 0 && cur < dep {
			n++
		}
	}
	return n
}
