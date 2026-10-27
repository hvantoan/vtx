package application

import (
	"testing"
	"time"
)

func TestLoginLimiterBlocksAfterMaxFailures(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	limiter := NewLoginLimiter(5, 15*time.Minute, clock)
	key := "10.0.0.1|operator"

	for i := 0; i < 4; i++ {
		if !limiter.Allow(key) {
			t.Fatalf("attempt %d blocked too early", i+1)
		}
		limiter.Failure(key)
	}
	if !limiter.Allow(key) {
		t.Fatal("fifth attempt must still be allowed")
	}
	limiter.Failure(key)
	if limiter.Allow(key) {
		t.Fatal("sixth attempt must be blocked")
	}

	clock.now = clock.now.Add(15 * time.Minute)
	if !limiter.Allow(key) {
		t.Fatal("window expiry must unblock")
	}
	if limiter.Allow("10.0.0.2|operator") != true {
		t.Fatal("keys must be independent")
	}
}

func TestLoginLimiterResetClearsFailures(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	limiter := NewLoginLimiter(2, time.Minute, clock)
	key := "10.0.0.1|operator"
	limiter.Failure(key)
	limiter.Reset(key)
	limiter.Failure(key)
	if !limiter.Allow(key) {
		t.Fatal("reset must clear the counter")
	}
}
