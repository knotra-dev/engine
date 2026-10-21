package store

import (
	"sync"

	"github.com/knotra-dev/engine/internal/contract"
)

const (
	maxAdmittedPlans     = 128
	maxAdmittedPlanBytes = 64 << 20
)

type admittedPlanCache struct {
	// mu guards plans and bytes; admitted plans are immutable after publication.
	mu    sync.Mutex
	plans map[string]contract.Plan
	// Serialized bytes estimate retained data; decoded maps add heap overhead.
	bytes int
}

func (c *admittedPlanCache) load(runID string) (contract.Plan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	plan, ok := c.plans[runID]
	return plan, ok
}

func (c *admittedPlanCache) store(runID string, plan contract.Plan, size int) {
	if size > maxAdmittedPlanBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.plans[runID]; ok {
		return
	}
	if len(c.plans) >= maxAdmittedPlans || c.bytes+size > maxAdmittedPlanBytes {
		// shortcut: wholesale eviction keeps bookkeeping small; use LRU if active runs churn.
		c.plans, c.bytes = nil, 0
	}
	if c.plans == nil {
		c.plans = map[string]contract.Plan{}
	}
	c.plans[runID] = plan
	c.bytes += size
}
