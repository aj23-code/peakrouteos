package auth

import (
	"sync"
	"time"
	"net"
)

type RateLimiter struct {
	mu          sync.Mutex
	attempts    map[string][]time.Time
	maxRequests int
	window      time.Duration
}

func NewRateLimiter(maxRequests int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		attempts:    make(map[string][]time.Time),
		maxRequests: maxRequests,
		window:      window,
	}

	// Janitor loop to purge old IPs every 5 minutes and avoid memory leaks
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			rl.mu.Lock()
			now := time.Now()
			for ip, times := range rl.attempts {
				if len(times) == 0 || now.Sub(times[len(times)-1]) > window {
					delete(rl.attempts, ip)
				}
			}
			rl.mu.Unlock()
		}
	}()

	return rl
}

func (rl *RateLimiter) Allow(ip string) bool {

	// Strip port if present
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Filter out attempts outside the current time window
	var validAttempts []time.Time
	for _, t := range rl.attempts[ip] {
		if t.After(cutoff) {
			validAttempts = append(validAttempts, t)
		}
	}

	if len(validAttempts) >= rl.maxRequests {
		return false
	}

	rl.attempts[ip] = append(validAttempts, now)
	return true
}
