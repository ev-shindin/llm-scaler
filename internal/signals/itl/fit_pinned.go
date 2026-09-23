package itl

// DefaultBaselineSec is the per-token latency of an unloaded replica: the
// intercept B of ITL(k) = A*k + B when there is not enough spread in k to fit
// one. It is a hardware floor -- the time a single sequence takes per token
// with the cache nearly empty -- so it varies far less between deployments
// than the contention slope A does, which is what makes pinning it and fitting
// only A a reasonable trade when the observations are poor.
//
// The throughput analyzer carries the same number as its own
// DefaultBaselineITLSec. It is stated here rather than imported because that
// package is an analyzer and this is a signal; the two collapse onto this one
// when the throughput analyzer's in-flight work lands.
const DefaultBaselineSec = 0.006

// FitPinnedB fits ITL(k) = A*k + B with B held at the value given, by least
// squares on the residual: A = sum((ITL_i - B) * k_i) / sum(k_i^2).
//
// This is the fit for a fleet whose replicas sit at similar loads. A router
// that balances well gives every replica nearly the same k, and a full
// two-parameter fit over points that share an x is meaningless -- Window.Ready
// exists to refuse exactly that, requiring DefaultMinKSpread before Fit is
// trusted. Pinning B leaves one parameter, which a cluster of points at one k
// can still determine: it answers "how much slower than an idle replica is one
// at this load", which is what the caller needs, without pretending to know
// where the line crosses zero.
//
// ok is false when there are no usable observations or the result is not a
// model any reading can be taken from. A negative slope is rejected: more
// cache in use cannot make a replica faster, and a fit that says so is
// measurement noise, not a model.
func FitPinnedB(obs []Observation, b float64) (Model, bool) {
	if len(obs) == 0 || !(b > 0) {
		return Model{}, false
	}
	var num, den float64
	n := 0
	for _, o := range obs {
		if !(o.K > 0) || !(o.ITLSec > 0) {
			continue
		}
		num += (o.ITLSec - b) * o.K
		den += o.K * o.K
		n++
	}
	if n == 0 || !(den > 0) {
		return Model{}, false
	}
	a := num / den
	if !(a >= 0) {
		return Model{}, false
	}
	m := Model{A: a, B: b}
	if !ValidModel(m.A, m.B) {
		return Model{}, false
	}
	return m, true
}
