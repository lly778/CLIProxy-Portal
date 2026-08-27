package httpserver

import (
	"sync"
	"time"
)

type attempt struct {
	count        int
	reset        time.Time
	blockedUntil time.Time
}
type limiter struct {
	mu    sync.Mutex
	items map[string]attempt
}

func newLimiter() *limiter { return &limiter{items: map[string]attempt{}} }
func (l *limiter) Allow(key string, max int, window, block time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	v := l.items[key]
	if now.Before(v.blockedUntil) {
		return false
	}
	if v.reset.IsZero() || now.After(v.reset) {
		v = attempt{reset: now.Add(window)}
	}
	v.count++
	if v.count > max {
		v.blockedUntil = now.Add(block)
		l.items[key] = v
		return false
	}
	l.items[key] = v
	return true
}
func (l *limiter) Reset(key string) { l.mu.Lock(); delete(l.items, key); l.mu.Unlock() }
