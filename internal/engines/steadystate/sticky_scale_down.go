package steadystate

import (
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/policy"

	"k8s.io/apimachinery/pkg/types"
)

// decidedMark is when a scale target was last decided for, for which
// incarnation of it (its UID at the time), and how many cycles since have
// passed without a decision. See Engine.lastDecided.
type decidedMark struct {
	at     time.Time
	uid    types.UID
	missed int
}

// pruneLastDecided drops marks too old to be trusted by the hold or the
// carry, so the map follows the fleet rather than growing with every scale
// target the controller has ever decided for.
func (e *Engine) pruneLastDecided(maxAge time.Duration, now time.Time) {
	for key, mark := range e.lastDecided {
		if now.Sub(mark.at) > maxAge {
			delete(e.lastDecided, key)
		}
	}
}

// stickyAgeCycles is how many optimize cycles the bound covers at least: a
// cycle's own length must fit under it several times over, or a deployment
// with a long GLOBAL_OPT_INTERVAL would find every published value already
// stale by the next cycle and the switch would silently do nothing.
const stickyAgeCycles = 4

// stickyAge is the age bound in force: policy.DefaultMaxAge, or stickyAgeCycles
// optimize intervals when those are longer.
func (e *Engine) stickyAge() time.Duration {
	age := policy.DefaultMaxAge
	if e.Config != nil {
		if byInterval := stickyAgeCycles * e.Config.OptimizationInterval(); byInterval > age {
			age = byInterval
		}
	}
	return age
}

// noteScaleTargetUID records which incarnation of a scale target this cycle
// read, so a published value decided for a different one is not trusted.
func (e *Engine) noteScaleTargetUID(key string, uid types.UID) {
	if e.scaleTargetUIDs == nil {
		e.scaleTargetUIDs = make(map[string]types.UID)
	}
	e.scaleTargetUIDs[key] = uid
}
