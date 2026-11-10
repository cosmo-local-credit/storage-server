package api

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// rateLimiter is a token bucket per identity. A bucket that has refilled is
// indistinguishable from a new one, so the sweep drops those and memory follows
// active callers rather than every caller ever seen.
type rateLimiter struct {
	mu        sync.Mutex
	limit     rate.Limit
	burst     int
	buckets   map[string]*rate.Limiter
	lastSweep time.Time
	now       func() time.Time
}

const rateSweepInterval = time.Minute

func newRateLimiter(perMinute int) *rateLimiter {
	return &rateLimiter{
		limit:   rate.Limit(float64(perMinute) / 60),
		burst:   perMinute,
		buckets: make(map[string]*rate.Limiter),
		now:     time.Now,
	}
}

// allow spends one token for key, or reports how long until one is available.
// A refused call spends nothing, so retrying early does not push the wait out.
func (r *rateLimiter) allow(key string) (time.Duration, bool) {
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	if now.Sub(r.lastSweep) >= rateSweepInterval {
		for k, b := range r.buckets {
			if b.TokensAt(now) >= float64(r.burst) {
				delete(r.buckets, k)
			}
		}
		r.lastSweep = now
	}

	b, ok := r.buckets[key]
	if !ok {
		b = rate.NewLimiter(r.limit, r.burst)
		r.buckets[key] = b
	}
	res := b.ReserveN(now, 1)
	if wait := res.DelayFrom(now); wait > 0 {
		res.CancelAt(now)
		return wait, false
	}
	return 0, true
}

// retryAfterSeconds rounds up: a client that waits the advertised whole seconds
// must find a token, not be refused again by a fraction.
func retryAfterSeconds(wait time.Duration) int {
	return max(1, int(math.Ceil(wait.Seconds())))
}
