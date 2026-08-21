package estimator

import "container/list"

// lru is the warm set: the models a pool holds resident, evicting the least
// recently wanted when full.
//
// LRU because this is a cache and inference popularity is skewed, so recency
// predicts the next spike better than anything else available without a
// forecast. It is deliberately the simplest defensible policy: a better one can
// only raise the hit rate, so a pool sized on these numbers is not oversold.
type lru struct {
	capacity int
	order    *list.List               // front = most recently wanted
	index    map[string]*list.Element // model -> its element
}

func newLRU(capacity int) *lru {
	return &lru{
		capacity: capacity,
		order:    list.New(),
		index:    make(map[string]*list.Element, capacity),
	}
}

func (c *lru) has(model string) bool {
	_, ok := c.index[model]
	return ok
}

// touch admits a model, or refreshes one already resident, evicting the least
// recently wanted when that makes the warm set too large.
func (c *lru) touch(model string) {
	if el, ok := c.index[model]; ok {
		c.order.MoveToFront(el)
		return
	}
	if c.order.Len() >= c.capacity {
		if last := c.order.Back(); last != nil {
			c.order.Remove(last)
			delete(c.index, last.Value.(string))
		}
	}
	c.index[model] = c.order.PushFront(model)
}
