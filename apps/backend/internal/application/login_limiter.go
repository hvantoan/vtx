package application

import (
	"sync"
	"time"
)

// LoginLimiter is an in-process fixed-window throttle keyed by client address
// and username. It is deliberately per-replica: the API is deployed as one
// process, and a shared limiter would add a store round-trip to every login.
type LoginLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	clock  Clock
	state  map[string]limiterEntry
}

type limiterEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

func NewLoginLimiter(max int, window time.Duration, clock Clock) *LoginLimiter {
	if clock == nil {
		clock = SystemClock{}
	}
	if max <= 0 {
		max = 5
	}
	if window <= 0 {
		window = 15 * time.Minute
	}
	return &LoginLimiter{max: max, window: window, clock: clock, state: make(map[string]limiterEntry)}
}

// Allow reports whether a login attempt may proceed.
func (l *LoginLimiter) Allow(key string) bool {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.state[key]
	if !ok {
		return true
	}
	if !entry.blockedUntil.IsZero() && now.Before(entry.blockedUntil) {
		return false
	}
	if now.Sub(entry.windowStart) >= l.window {
		delete(l.state, key)
		return true
	}
	return true
}

// Failure records a rejected credential. The attempt that reaches the limit is
// itself rejected by the next Allow call.
func (l *LoginLimiter) Failure(key string) {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.state[key]
	if !ok || now.Sub(entry.windowStart) >= l.window {
		entry = limiterEntry{windowStart: now}
	}
	entry.failures++
	if entry.failures >= l.max {
		entry.blockedUntil = now.Add(l.window)
	}
	l.state[key] = entry
}

// Reset clears the counter after a successful login.
func (l *LoginLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, key)
}
