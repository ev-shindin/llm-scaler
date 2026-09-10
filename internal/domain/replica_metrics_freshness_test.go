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
