// Per-host circuit breaker for the HTTP executor: consecutive availability
// failures trip the circuit, dispatches fast-fail until the cooldown
// elapses, then one half-open probe decides. Pure in-memory — executor code
// never touches storage (module direction constraint).
package executor

import (
	"sync"
	"time"
)

// circuitState is the three-state lifecycle of a per-host breaker.
type circuitState int

const (
	circuitClosed   circuitState = iota // normal: traffic flows
	circuitOpen                         // tripped: fast-fail until cooldown elapses
	circuitHalfOpen                     // cooldown elapsed: one probe decides
)

// hostBreaker is a per-executor-host consecutive-failure circuit breaker.
// After threshold consecutive availability failures it fast-fails dispatches
// for the cooldown, then admits a single probe: success closes the circuit,
// failure reopens it. It exists so one dead executor cannot pin the whole
// dispatch worker pool in timeout waits (the pool is a shared bulkhead).
//
// Availability means network errors and 5xx only — a 4xx proves the target
// is alive and answering (deterministic rejection) and never trips the
// breaker. All timestamps come from the caller so tests can drive time.
type hostBreaker struct {
	threshold int
	cooldown  time.Duration

	mu       sync.Mutex
	failures int // consecutive failures while closed
	state    circuitState
	openedAt time.Time
	probing  bool // half-open probe slot taken
}

// newHostBreaker builds a breaker; threshold <= 0 must not reach here
// (callers gate on it).
func newHostBreaker(threshold int, cooldown time.Duration) *hostBreaker {
	return &hostBreaker{threshold: threshold, cooldown: cooldown}
}

// Allow reports whether a dispatch may proceed at time now. When the circuit
// is open (or the half-open probe slot is taken) it returns false plus the
// remaining wait, for surfacing in the dispatch error.
func (b *hostBreaker) Allow(now time.Time) (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case circuitClosed:
		return true, 0
	case circuitOpen:
		remaining := b.cooldown - now.Sub(b.openedAt)
		if remaining > 0 {
			return false, remaining
		}
		// Cooldown elapsed: admit one probe.
		b.state = circuitHalfOpen
		b.probing = true
		return true, 0
	default: // circuitHalfOpen
		if b.probing {
			return false, b.cooldown
		}
		b.probing = true
		return true, 0
	}
}

// RecordSuccess closes the circuit and clears the consecutive-failure count.
func (b *hostBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = circuitClosed
	b.probing = false
}

// RecordFailure counts one availability failure at time now: while closed it
// opens the circuit once the threshold is reached; a failed half-open probe
// reopens it for another full cooldown.
func (b *hostBreaker) RecordFailure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == circuitHalfOpen {
		// Probe failed: reopen for another full cooldown.
		b.state = circuitOpen
		b.openedAt = now
		b.probing = false
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.state = circuitOpen
		b.openedAt = now
	}
}
