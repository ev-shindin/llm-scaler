package estimator

import (
	lru "github.com/hashicorp/golang-lru/v2"
)

// warmSet is the models a pool holds resident, evicting the least recently
// wanted when full.
//
// LRU because this is a cache and inference popularity is skewed, so recency
// predicts the next spike better than anything else available without a
// forecast. It is deliberately the simplest defensible policy: a better one can
// only raise the hit rate, so a pool sized on these numbers is not oversold.
//
// The eviction itself comes from hashicorp/golang-lru, which this repo already
// depends on and already wraps for the same reason in
// internal/collector/locator/cache.go. The value type is empty because only
// membership matters here.
type warmSet struct {
	cache *lru.Cache[string, struct{}]
}

func newWarmSet(capacity int) *warmSet {
	if capacity < 1 {
		capacity = 1
	}
	// The only error is a non-positive size, which the guard above removes.
	cache, _ := lru.New[string, struct{}](capacity)
	return &warmSet{cache: cache}
}

// has reports whether a model is resident, without counting as a use. Read-only
// on purpose: asking whether a spike would hit must not change what the cache
// would have evicted.
func (w *warmSet) has(model string) bool {
	return w.cache.Contains(model)
}

// touch admits a model, or refreshes one already resident, evicting the least
// recently wanted if that makes the set too large.
func (w *warmSet) touch(model string) {
	w.cache.Add(model, struct{}{})
}
