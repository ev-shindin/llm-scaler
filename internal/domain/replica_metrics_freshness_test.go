package domain

import "testing"

// TestStaleOrOlder pins the distinction three separate call sites got wrong by
// testing FreshnessStatus == FreshnessStale with ==.
//
// Three of the four statuses are age bands over a present timestamp and only
// FreshnessMissing means the metric was never scraped. Excluding one age band
// and not the other applies a staleness rule everywhere except to the data most
// in need of it.
func TestStaleOrOlder(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{FreshnessFresh, false},
		{FreshnessStale, true},
		{FreshnessUnavailable, true},
		{FreshnessMissing, false},
		{"", false},
	}
	for _, tc := range cases {
		m := &ReplicaMetricsMetadata{FreshnessStatus: tc.status}
		if got := m.StaleOrOlder(); got != tc.want {
			t.Errorf("StaleOrOlder() on %q is %v, want %v", tc.status, got, tc.want)
		}
	}

	// Nil is not stale: rows reach some consumers with no metadata, and reading
	// "no information" as "too old" would empty a sample rather than leave it.
	var nilMeta *ReplicaMetricsMetadata
	if nilMeta.StaleOrOlder() {
		t.Error("StaleOrOlder() on nil metadata is true, want false")
	}
}

// TestUnavailable pins the narrower of the two verdicts, and that it is strictly
// narrower.
//
// The two are spent on actions of very different weight: StaleOrOlder drops a
// replica from an average, Unavailable stops WVA answering KEDA for the whole
// workload. If Unavailable ever widened to match, a healthy fleet behind a slow
// Prometheus -- where every replica goes a minute stale together -- would be
// frozen rather than merely averaged without.
func TestUnavailable(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{FreshnessFresh, false},
		{FreshnessStale, false},
		{FreshnessUnavailable, true},
		{FreshnessMissing, false},
		{"", false},
	}
	for _, tc := range cases {
		m := &ReplicaMetricsMetadata{FreshnessStatus: tc.status}
		if got := m.Unavailable(); got != tc.want {
			t.Errorf("Unavailable() on %q is %v, want %v", tc.status, got, tc.want)
		}
		// Strictly narrower: anything Unavailable is also StaleOrOlder, never
		// the reverse.
		if got := m.Unavailable(); got && !m.StaleOrOlder() {
			t.Errorf("%q is Unavailable but not StaleOrOlder; the bands must nest", tc.status)
		}
	}

	var nilMeta *ReplicaMetricsMetadata
	if nilMeta.Unavailable() {
		t.Error("Unavailable() on nil metadata is true, want false")
	}
}
