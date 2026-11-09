package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	windowStart time.Time
	count       int
}

type FixedWindow struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*bucket
	now     func() time.Time
}

func New(limit int, window time.Duration) *FixedWindow {
	return &FixedWindow{limit: limit, window: window, buckets: make(map[string]*bucket), now: time.Now}
}

func (l *FixedWindow) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil || now.Sub(b.windowStart) >= l.window {
		l.buckets[key] = &bucket{windowStart: now, count: 1}
		return true
	}
	if b.count >= l.limit {
		return false
	}
	b.count++
	return true
}
