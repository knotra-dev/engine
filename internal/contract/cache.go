package contract

import "sync"

const (
	compilationCacheEntries  = 256
	compilationCacheKeyBytes = 1 << 20
)

// Eviction only releases cache ownership; immutable results remain valid for callers.
type compilationCache[T any] struct {
	mu       sync.RWMutex
	entries  map[string]T
	keyBytes int
}

func (c *compilationCache[T]) load(key string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.entries[key]
	return value, ok
}

func (c *compilationCache[T]) store(key string, value T) T {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.entries[key]; ok {
		return cached
	}
	if len(key) > compilationCacheKeyBytes {
		return value
	}
	if len(c.entries) == compilationCacheEntries || c.keyBytes+len(key) > compilationCacheKeyBytes {
		// shortcut: wholesale eviction keeps bookkeeping small; use LRU if active workloads churn.
		clear(c.entries)
		c.keyBytes = 0
	}
	if c.entries == nil {
		c.entries = make(map[string]T)
	}
	c.entries[key] = value
	c.keyBytes += len(key)
	return value
}
