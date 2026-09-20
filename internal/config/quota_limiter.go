package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
)

// QuotaLimiterReservedNamespaceKey is the reserved key in the namespace-scoped
// quotas map that matches any namespace not explicitly listed. A real namespace
// named "default" cannot be configured directly via this key — list it in
// `exclude` or use a strict allowlist instead.
const QuotaLimiterReservedNamespaceKey = "default"

// QuotaUnlimited is the per-type quota value that disables the cap for that
// accelerator type in the containing namespace. Follows the Kubernetes
// convention of -1 = no limit (e.g., `terminationGracePeriodSeconds: -1`).
const QuotaUnlimited = -1

// MaxQuotaValue is the largest finite quota a single entry may declare. It is
// far above any realistic GPU count (a million accelerators) and exists only to
// keep the aggregate sum (summed across namespaces in aggregateNamespacePools)
// well clear of int64 overflow — an overflowed sum could wrap negative and be
// misread as the QuotaUnlimited (-1) sentinel.
const MaxQuotaValue = 1 << 20 // 1,048,576

// QuotaScope identifies whether a quota limiter applies cluster-wide or
// per-namespace. Both scopes can coexist; each acts independently.
type QuotaScope string

const (
	// QuotaScopeCluster caps total GPUs of a given accelerator type across
	// all namespaces.
	QuotaScopeCluster QuotaScope = "cluster"

	// QuotaScopeNamespace caps GPUs of a given accelerator type within a
	// specific namespace, with optional fall-through via the reserved
	// `default` key.
	QuotaScopeNamespace QuotaScope = "namespace"
)

// QuotaLimiterConfig is one entry in the quota-limiter ConfigMap. Each
// entry produces an independent limiter — cluster and namespace scopes
// are declared as separate entries so operators can enable them
// independently.
//
// The active scope is chosen by the Scope field: when Scope ==
// QuotaScopeCluster, only ClusterQuotas is consulted; when Scope ==
// QuotaScopeNamespace, only NamespaceQuotas and Exclude are.
type QuotaLimiterConfig struct {
	// Name identifies the limiter in logs, metrics, and DecisionStep traces.
	// Must be non-empty and unique across all entries.
	Name string `yaml:"name" json:"name"`

	// Type must be the literal string "quota" — leaves room for future
	// limiter types in the same config schema (e.g., reservation, priority).
	Type string `yaml:"type" json:"type"`

	// Scope selects which quota map below is consulted.
	Scope QuotaScope `yaml:"scope" json:"scope"`

	// ClusterQuotas applies when Scope == QuotaScopeCluster. Keys are
	// accelerator type names (e.g., "H100"); values are the cluster-wide
	// cap in GPUs (or QuotaUnlimited for no cap).
	ClusterQuotas map[string]int `yaml:"quotas,omitempty" json:"quotas,omitempty"`

	// NamespaceQuotas applies when Scope == QuotaScopeNamespace. Top-level
	// keys are namespace names (with the reserved key `default` matching
	// any namespace not explicitly listed); inner keys are accelerator
	// type names; inner values are caps (or QuotaUnlimited).
	//
	// Looked up via QuotaForNamespace; missing or zero entries mean "no
	// allocation".
	NamespaceQuotas map[string]map[string]int `yaml:"namespaceQuotas,omitempty" json:"namespaceQuotas,omitempty"`

	// Exclude lists namespaces that bypass this limiter entirely (no
	// constraint applied). Only meaningful when Scope == QuotaScopeNamespace.
	// Useful for system namespaces or privileged tenants.
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`

	// Kueue, when enabled, reads GPU quotas from Kueue (ClusterQueue nominal
	// quotas, attributed to namespaces through their LocalQueues) and bounds
	// this entry by them: per namespace (or cluster) and accelerator type the
	// SMALLER of the Kueue cap and the static cap above wins. See BoundBy for
	// the exact rules. Nil or disabled means the static maps are the only source.
	Kueue *KueueQuotaSource `yaml:"kueue,omitempty" json:"kueue,omitempty"`
}

// KueueQuotaSource configures reading quotas from Kueue for one quota entry.
//
// Kueue is the cluster's admission-time quota authority; this makes WVA respect
// the same figures before it asks KEDA for a replica that Kueue would then hold
// pending. The mapping is: a ClusterQueue's nominalQuota for a GPU extended
// resource, per flavor, becomes a per-accelerator-type cap; a namespace is
// granted the caps of every ClusterQueue one of its LocalQueues points at; the
// cluster figure is the sum over all ClusterQueues. Borrowing limits and cohorts
// are deliberately NOT counted — the bound wants the guaranteed figure.
//
// A ClusterQueue shared by several namespaces is a per-namespace CEILING here,
// not a partition: each namespace with a LocalQueue on it is bounded at the
// queue's full nominal quota, so in sum they may still ask for more than the
// queue admits, and Kueue holds the excess pending. The bound removes the
// single-namespace over-ask; where namespaces share a queue, the shared budget
// is Kueue's to arbitrate, as it was before.
type KueueQuotaSource struct {
	// Enabled turns the reader on. A `kueue:` block with enabled false is the
	// same as no block at all.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Resources lists the extended-resource names that count as GPUs in a
	// ClusterQueue (e.g. "nvidia.com/gpu"). Empty means every vendor resource
	// WVA already knows (constants.VendorResources).
	Resources []string `yaml:"resources,omitempty" json:"resources,omitempty"`

	// RefreshInterval caps how often Kueue is re-read; a Go duration string.
	// Empty means DefaultKueueRefreshInterval. Between reads the last snapshot is
	// served, so a quota edit in Kueue takes up to this long to bind here.
	RefreshInterval string `yaml:"refreshInterval,omitempty" json:"refreshInterval,omitempty"`
}

// DefaultKueueRefreshInterval is how often Kueue quotas are re-read when the
// entry does not say. The saturation cycle is of the same order, so a shorter
// value would only add API calls the next decision cannot use.
const DefaultKueueRefreshInterval = 30 * time.Second

// KueueEnabled reports whether this entry reads quotas from Kueue.
func (q QuotaLimiterConfig) KueueEnabled() bool {
	return q.Kueue != nil && q.Kueue.Enabled
}

// KueueRefreshInterval returns the configured Kueue refresh interval, or the
// default when unset. Callers must have run Validate, which rejects a value that
// does not parse; an unparseable value here falls back to the default rather than
// panicking.
func (q QuotaLimiterConfig) KueueRefreshInterval() time.Duration {
	if q.Kueue == nil || q.Kueue.RefreshInterval == "" {
		return DefaultKueueRefreshInterval
	}
	d, err := time.ParseDuration(q.Kueue.RefreshInterval)
	if err != nil || d <= 0 {
		return DefaultKueueRefreshInterval
	}
	return d
}

// validate checks the Kueue block of one entry. Only the fields an operator can
// get wrong are checked: a resource name must be non-empty and the refresh
// interval must be a positive duration.
func (k *KueueQuotaSource) validate(location string) error {
	if k == nil {
		return nil
	}
	var errs []error
	for i, r := range k.Resources {
		if strings.TrimSpace(r) == "" {
			errs = append(errs, fmt.Errorf("%s.kueue.resources[%d]: resource name must not be empty", location, i))
		}
	}
	if k.RefreshInterval != "" {
		d, err := time.ParseDuration(k.RefreshInterval)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s.kueue.refreshInterval: %q is not a duration: %w", location, k.RefreshInterval, err))
		case d <= 0:
			errs = append(errs, fmt.Errorf("%s.kueue.refreshInterval: %q must be positive", location, k.RefreshInterval))
		}
	}
	return errors.Join(errs...)
}

// clone returns a copy of k that shares no mutable state with the original.
func (k *KueueQuotaSource) clone() *KueueQuotaSource {
	if k == nil {
		return nil
	}
	out := *k
	if k.Resources != nil {
		out.Resources = slices.Clone(k.Resources)
	}
	return &out
}

// ExternalCaps is what an external source grants one subject — a namespace, or
// the cluster. Values are finite caps; a source never emits QuotaUnlimited.
type ExternalCaps struct {
	// ByType caps per accelerator type, keyed by the short name the source could
	// attribute (a Kueue flavor whose node labels name a product).
	ByType map[string]int
	// Untyped is a grant the source could NOT attribute to an accelerator type —
	// a Kueue flavor without a product label, which Kueue's own quickstart uses.
	// It bounds every type the static entry names for this subject; on its own
	// it names no type and so cannot open one. HasUntyped tells a real grant of
	// 0 apart from no untyped grant.
	Untyped    int
	HasUntyped bool
}

// IsZero reports whether the source granted this subject nothing at all —
// neither a typed nor an untyped cap.
func (c ExternalCaps) IsZero() bool {
	return len(c.ByType) == 0 && !c.HasUntyped
}

// ExternalQuotas is a snapshot of GPU caps read from an external authority
// (today Kueue), per namespace for the namespace scope and for the whole
// cluster for the cluster scope.
//
// Absence carries meaning, and it differs between the levels:
//   - A namespace missing from Namespace is NOT governed by the source (no
//     LocalQueue of its reaches a ClusterQueue that declares a GPU resource), so
//     the static entry alone decides for it.
//   - A type missing from a PRESENT namespace's ByType is a cap of 0 — unless
//     that namespace has an untyped grant, which then applies — because the
//     source knows the namespace and grants it nothing of that type.
//   - A zero Cluster means the source declares no GPU budget at all, so it
//     contributes nothing at cluster scope.
type ExternalQuotas struct {
	Namespace map[string]ExternalCaps
	Cluster   ExternalCaps
	// ObservedAt is when the source was last read successfully; zero when it
	// never was, in which case the maps are empty and mean nothing.
	ObservedAt time.Time
}

// BoundBy returns this entry bounded by an external snapshot: the static maps and
// the external ones are combined per key so that the SMALLER cap wins, which is
// what an operator declaring both means — neither source may raise what the other
// granted. Exclude is untouched; excluded namespaces bypass both sources.
//
// Rules for the namespace scope, per namespace N present in ext.Namespace:
//   - N excluded: skipped.
//   - N is the reserved "default" key: skipped. Writing it would turn one
//     namespace's Kueue grant into the fall-through for every unlisted namespace;
//     the K8s "default" namespace is already unconfigurable here (see
//     QuotaLimiterReservedNamespaceKey).
//   - N listed in NamespaceQuotas: its map becomes min(static map, ext map).
//   - N unlisted but a static "default" exists: min(default map, ext map). The
//     namespace becomes explicitly listed, at that per-namespace budget.
//   - N unlisted and NamespaceQuotas is empty: the external map is taken as is
//     — an entry with no static map has asked for the source to be the whole
//     answer.
//   - N unlisted in a non-empty static allowlist without "default": left out.
//     The static list denies N and the source cannot open it.
//
// Namespaces the snapshot does not mention keep their static treatment.
//
// min over a static map and an external grant is taken over the UNION of their
// type keys: a static type the source does not name gets the source's untyped
// grant if there is one, else 0; a source type the static map does not name is
// 0 — both are closed allowlists, so "not listed" already means "denied" on
// each. Type keys are matched by accelerator identity (accelerator.SameName) and
// the static spelling is kept, so a Kueue flavor labelled
// "NVIDIA-H100-80GB-HBM3" bounds a static "H100". QuotaUnlimited (-1) on either
// side yields the other side's value — which is how an entry hands the numbers
// to Kueue while keeping the type names: `default: {H100: -1}` plus a Kueue
// flavor with no product label gives every namespace min(-1, untyped).
//
// An untyped grant standing alone names no type, so where the static entry
// names none either (Kueue-alone paths) it cannot be applied; UnappliedUntyped
// reports those subjects so the limiter can say so.
//
// Cluster scope: a zero ext.Cluster changes nothing; an empty static quotas map
// takes ext.Cluster's typed caps as is; otherwise min over the union, as above.
func (q QuotaLimiterConfig) BoundBy(ext ExternalQuotas) QuotaLimiterConfig {
	out := q.clone()
	switch q.Scope {
	case QuotaScopeCluster:
		switch {
		case ext.Cluster.IsZero():
		case len(q.ClusterQuotas) == 0:
			out.ClusterQuotas = maps.Clone(ext.Cluster.ByType)
		default:
			out.ClusterQuotas = minTypeQuotas(q.ClusterQuotas, ext.Cluster)
		}
	case QuotaScopeNamespace:
		if len(ext.Namespace) == 0 {
			return out
		}
		if out.NamespaceQuotas == nil {
			out.NamespaceQuotas = make(map[string]map[string]int, len(ext.Namespace))
		}
		staticDefault, hasDefault := q.NamespaceQuotas[QuotaLimiterReservedNamespaceKey]
		staticEmpty := len(q.NamespaceQuotas) == 0
		for ns, caps := range ext.Namespace {
			if ns == QuotaLimiterReservedNamespaceKey || q.IsExcluded(ns) || caps.IsZero() {
				continue
			}
			switch base, listed := q.NamespaceQuotas[ns]; {
			case listed:
				out.NamespaceQuotas[ns] = minTypeQuotas(base, caps)
			case hasDefault:
				out.NamespaceQuotas[ns] = minTypeQuotas(staticDefault, caps)
			case staticEmpty && len(caps.ByType) > 0:
				out.NamespaceQuotas[ns] = maps.Clone(caps.ByType)
			}
		}
	}
	return out
}

// UnappliedUntyped lists the subjects whose untyped external grant BoundBy could
// not apply because the static entry names no accelerator type for them: the
// namespaces (by name), and "cluster" for the cluster scope. Such a grant is
// real — Kueue would admit that many GPUs — but the limiter needs a type to
// budget under; the fix is to name the types in the static map, with -1 to let
// the source supply the figure.
func (q QuotaLimiterConfig) UnappliedUntyped(ext ExternalQuotas) []string {
	var out []string
	switch q.Scope {
	case QuotaScopeCluster:
		if ext.Cluster.HasUntyped && len(q.ClusterQuotas) == 0 {
			out = append(out, "cluster")
		}
	case QuotaScopeNamespace:
		_, hasDefault := q.NamespaceQuotas[QuotaLimiterReservedNamespaceKey]
		for ns, caps := range ext.Namespace {
			if !caps.HasUntyped || ns == QuotaLimiterReservedNamespaceKey || q.IsExcluded(ns) {
				continue
			}
			if _, listed := q.NamespaceQuotas[ns]; listed || hasDefault {
				continue
			}
			out = append(out, ns)
		}
	}
	slices.Sort(out)
	return out
}

// minTypeQuotas bounds a static per-type map by an external grant, over the
// union of type keys. A static type the grant does not name takes the untyped
// grant when there is one, else 0; a granted type the static map does not name
// is 0. Keys are matched by accelerator identity and the result keeps the static
// spelling for matched keys, the grant's for the rest.
func minTypeQuotas(base map[string]int, caps ExternalCaps) map[string]int {
	out := make(map[string]int, len(base)+len(caps.ByType))
	matched := make(map[string]bool, len(caps.ByType))
	for accType, baseCap := range base {
		// Every grant naming this accelerator, summed: a static "H100" is
		// bounded by the PCIe flavor's quota PLUS the SXM flavor's, since it
		// means either; a static "NVIDIA-H100-PCIE-80GB" meets only its own.
		otherCap, found := 0, false
		for grantType, n := range caps.ByType {
			if accelerator.SameName(grantType, accType) {
				otherCap += n
				found = true
				matched[grantType] = true
			}
		}
		if !found && caps.HasUntyped {
			otherCap = caps.Untyped
		}
		out[accType] = minQuota(baseCap, otherCap)
	}
	for accType := range caps.ByType {
		if matched[accType] {
			continue // already combined under the static spelling
		}
		out[accType] = 0
	}
	return out
}

// minQuota returns the tighter of two caps, treating QuotaUnlimited as no bound.
func minQuota(a, b int) int {
	switch {
	case a == QuotaUnlimited:
		return b
	case b == QuotaUnlimited:
		return a
	}
	return min(a, b)
}

// IsExcluded reports whether the given namespace bypasses this limiter.
// Always false for cluster-scoped entries.
func (q QuotaLimiterConfig) IsExcluded(namespace string) bool {
	if q.Scope != QuotaScopeNamespace {
		return false
	}
	return slices.Contains(q.Exclude, namespace)
}

// QuotaForNamespace returns the per-type quota map for the given namespace,
// applying the lookup rules from the design:
//
//  1. Excluded namespaces return (nil, true) — caller should skip enforcement.
//  2. Exact match in NamespaceQuotas wins.
//  3. Otherwise, fall through to the reserved `default` key. The returned map
//     represents the cap *per* unlisted namespace (matching the Kubernetes
//     LimitRange default semantic), not a shared pool — the limiter tracks
//     usage per concrete namespace name, so each unlisted namespace consumes
//     its own budget at the default level.
//  4. Otherwise, return an empty map (treated as zero quota by callers).
//
// The boolean second return value indicates whether the namespace is
// excluded (true) — the per-type map is nil in that case and callers
// must skip enforcement rather than treating it as zero.
//
// The returned per-type map is a fresh copy, so callers may read or mutate it
// without aliasing the config (and, critically, without two unlisted namespaces
// sharing one `default` map). Always returns (nil, false) for cluster-scoped
// entries.
func (q QuotaLimiterConfig) QuotaForNamespace(namespace string) (map[string]int, bool) {
	if q.Scope != QuotaScopeNamespace {
		return nil, false
	}
	if q.IsExcluded(namespace) {
		return nil, true
	}
	if quotas, ok := q.NamespaceQuotas[namespace]; ok {
		return maps.Clone(quotas), false
	}
	if quotas, ok := q.NamespaceQuotas[QuotaLimiterReservedNamespaceKey]; ok {
		return maps.Clone(quotas), false
	}
	return map[string]int{}, false
}

// clone returns a deep copy of q: the ClusterQuotas / NamespaceQuotas maps and
// the Exclude slice and the Kueue block are duplicated so the result shares no
// mutable state with the original. Used by Config.EffectiveQuotaEntries to hand out entries that callers
// cannot use to mutate the config-owned snapshot.
func (q QuotaLimiterConfig) clone() QuotaLimiterConfig {
	out := q // copies scalar fields and (to be replaced) map/slice headers
	if q.ClusterQuotas != nil {
		out.ClusterQuotas = maps.Clone(q.ClusterQuotas)
	}
	if q.NamespaceQuotas != nil {
		out.NamespaceQuotas = make(map[string]map[string]int, len(q.NamespaceQuotas))
		for ns, perType := range q.NamespaceQuotas {
			out.NamespaceQuotas[ns] = maps.Clone(perType)
		}
	}
	if q.Exclude != nil {
		out.Exclude = slices.Clone(q.Exclude)
	}
	out.Kueue = q.Kueue.clone()
	return out
}

// QuotaLimiterEntries is the top-level ConfigMap shape: one or more
// QuotaLimiterConfig entries under a single `limiters` key. Allows
// multiple limiters (e.g., cluster + namespace) to coexist in one
// ConfigMap data entry.
type QuotaLimiterEntries struct {
	Limiters []QuotaLimiterConfig `yaml:"limiters" json:"limiters"`
}

// Validate checks an entries block for structural correctness. It
// returns nil only when every entry parses cleanly. When multiple entries
// (or multiple fields within an entry) are broken, all errors are
// accumulated and joined with errors.Join — an operator fixing a malformed
// ConfigMap sees every problem in one pass instead of one-per-rebuild.
// Error messages name the failing entry by index and field so the bad row
// is easy to locate.
//
// Validation rules (all from issue #1002):
//   - Name is non-empty and unique across entries.
//   - Type == "quota" (other limiter types may be added later).
//   - Scope is one of the two QuotaScope values.
//   - Per-type quota values are >= QuotaUnlimited (only -1 is allowed as negative).
//   - Accelerator type names are non-empty.
//   - For namespace scope: namespace names are non-empty; warn (not error)
//     if a namespace appears in both Exclude and NamespaceQuotas — the
//     entry is still valid and Exclude wins.
//
// Returned warnings (non-fatal) are surfaced via a second return value
// so callers can log them without rejecting the config.
func (e *QuotaLimiterEntries) Validate() (warnings []string, err error) {
	if e == nil {
		return nil, errors.New("quota limiter entries are nil")
	}
	var errs []error
	seenNames := make(map[string]int, len(e.Limiters))
	for i, entry := range e.Limiters {
		if entry.Name == "" {
			errs = append(errs, fmt.Errorf("entry[%d]: name must not be empty", i))
			// Without a name we cannot meaningfully validate the rest of this
			// entry's contents (error messages would lack the entry identifier).
			continue
		}
		if prev, ok := seenNames[entry.Name]; ok {
			errs = append(errs, fmt.Errorf("entry[%d]: duplicate limiter name %q (first seen at entry[%d])", i, entry.Name, prev))
			continue
		}
		seenNames[entry.Name] = i

		if entry.Type != "quota" {
			errs = append(errs, fmt.Errorf("entry[%d] (%q): type must be \"quota\", got %q", i, entry.Name, entry.Type))
			// Keep going: scope/quotas validation is still useful diagnostically.
		}
		if kErr := entry.Kueue.validate(fmt.Sprintf("entry[%d] (%q)", i, entry.Name)); kErr != nil {
			errs = append(errs, kErr)
		}

		switch entry.Scope {
		case QuotaScopeCluster:
			if len(entry.NamespaceQuotas) > 0 {
				errs = append(errs, fmt.Errorf("entry[%d] (%q): namespaceQuotas is invalid for cluster scope; use the quotas map", i, entry.Name))
			}
			if len(entry.Exclude) > 0 {
				errs = append(errs, fmt.Errorf("entry[%d] (%q): exclude is invalid for cluster scope", i, entry.Name))
			}
			if vErr := validateTypeQuotaMap(entry.ClusterQuotas, fmt.Sprintf("entry[%d] (%q).quotas", i, entry.Name)); vErr != nil {
				errs = append(errs, vErr)
			}

		case QuotaScopeNamespace:
			if len(entry.ClusterQuotas) > 0 {
				errs = append(errs, fmt.Errorf("entry[%d] (%q): quotas is invalid for namespace scope; use namespaceQuotas", i, entry.Name))
			}
			for _, ns := range entry.Exclude {
				if strings.TrimSpace(ns) == "" {
					errs = append(errs, fmt.Errorf("entry[%d] (%q): exclude contains an empty namespace name", i, entry.Name))
				}
			}
			for ns, perType := range entry.NamespaceQuotas {
				if strings.TrimSpace(ns) == "" {
					errs = append(errs, fmt.Errorf("entry[%d] (%q): namespaceQuotas contains an empty namespace key", i, entry.Name))
					continue
				}
				if entry.IsExcluded(ns) {
					warnings = append(warnings,
						fmt.Sprintf("entry[%d] (%q): namespace %q is in both exclude and namespaceQuotas; the quota entry will be ignored (exclude wins)", i, entry.Name, ns))
				}
				if vErr := validateTypeQuotaMap(perType, fmt.Sprintf("entry[%d] (%q).namespaceQuotas[%q]", i, entry.Name, ns)); vErr != nil {
					errs = append(errs, vErr)
				}
			}

		default:
			errs = append(errs, fmt.Errorf("entry[%d] (%q): scope must be %q or %q, got %q",
				i, entry.Name, QuotaScopeCluster, QuotaScopeNamespace, entry.Scope))
		}
	}
	return warnings, errors.Join(errs...)
}

// validateTypeQuotaMap checks per-accelerator-type entries: type name
// non-empty, value >= QuotaUnlimited. Empty maps are allowed (the
// limiter simply has no caps for that scope). Errors within a single map
// are accumulated with errors.Join so an operator with multiple bad rows
// sees them all at once.
func validateTypeQuotaMap(quotas map[string]int, location string) error {
	var errs []error
	for accType, value := range quotas {
		if strings.TrimSpace(accType) == "" {
			errs = append(errs, fmt.Errorf("%s: accelerator type name must not be empty", location))
			continue
		}
		if value < QuotaUnlimited {
			errs = append(errs, fmt.Errorf("%s[%q]: quota value %d is invalid; must be >= %d (only -1 is allowed as negative — means unlimited)",
				location, accType, value, QuotaUnlimited))
		}
		if value > MaxQuotaValue {
			errs = append(errs, fmt.Errorf("%s[%q]: quota value %d exceeds the maximum of %d",
				location, accType, value, MaxQuotaValue))
		}
	}
	return errors.Join(errs...)
}
