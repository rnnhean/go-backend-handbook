package cache

import (
	"context"
	"sync"
	"time"
)

type item struct {
	value     any
	expiresAt time.Time
}

type MemoryCache struct {
	mu    sync.RWMutex
	items map[string]item
}

func NewMemory() *MemoryCache { return &MemoryCache{items: make(map[string]item)} }

func (c *MemoryCache) Get(_ context.Context, key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	it, ok := c.items[key]
	if !ok || time.Now().After(it.expiresAt) {
		return nil, false
	}
	return it.value, true
}

func (c *MemoryCache) Set(_ context.Context, key string, value any, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = item{value: value, expiresAt: time.Now().Add(ttl)}
	return nil
}

func (c *MemoryCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
	return nil
}
