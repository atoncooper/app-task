package executor

// Tests for the per-host circuit breaker: the state machine (closed → open →
// half-open probe) driven by injected timestamps, plus end-to-end dispatch
// behaviour against httptest servers (fast-fail stops hitting the target,
// recovery closes the circuit, 4xx never trips it).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestBreakerStateMachine drives the transitions with injected timestamps:
// threshold consecutive failures open the circuit, cooldown elapses into a
// single probe slot, probe success closes, probe failure reopens.
func TestBreakerStateMachine(t *testing.T) {
	b := newHostBreaker(3, time.Minute)
	t0 := time.Now()

	for i := 0; i < 3; i++ {
		if ok, _ := b.Allow(t0); !ok {
			t.Fatalf("Allow %d: circuit closed, dispatch must pass", i)
		}
		b.RecordFailure(t0)
	}
	if ok, wait := b.Allow(t0); ok {
		t.Fatalf("Allow after threshold: must fast-fail")
	} else if wait <= 0 || wait > time.Minute {
		t.Fatalf("retryAfter = %s, want in (0, cooldown]", wait)
	}

	// Half-way through the cooldown still denied.
	if ok, _ := b.Allow(t0.Add(30 * time.Second)); ok {
		t.Fatalf("Allow mid-cooldown: must fast-fail")
	}

	// Cooldown elapsed: exactly one probe admitted.
	if ok, _ := b.Allow(t0.Add(2 * time.Minute)); !ok {
		t.Fatalf("Allow after cooldown: probe must be admitted")
	}
	if ok, _ := b.Allow(t0.Add(2 * time.Minute)); ok {
		t.Fatalf("second Allow in half-open: concurrent probes must be denied")
	}

	// Probe success closes the circuit and resets the failure count.
	b.RecordSuccess()
	if ok, _ := b.Allow(t0.Add(2 * time.Minute)); !ok {
		t.Fatalf("Allow after probe success: circuit must be closed")
	}

	// Two fresh failures do not trip (consecutive semantics), the third does.
	b.RecordFailure(t0.Add(3 * time.Minute))
	b.RecordFailure(t0.Add(3 * time.Minute))
	if ok, _ := b.Allow(t0.Add(3 * time.Minute)); !ok {
		t.Fatalf("two failures below threshold: must stay closed")
	}
	b.RecordFailure(t0.Add(3 * time.Minute))
	if ok, _ := b.Allow(t0.Add(3 * time.Minute)); ok {
		t.Fatalf("third consecutive failure: must open")
	}

	// Failed probe reopens for a full cooldown.
	if ok, _ := b.Allow(t0.Add(4 * time.Minute)); !ok {
		t.Fatalf("probe must be admitted after cooldown")
	}
	b.RecordFailure(t0.Add(4 * time.Minute))
	if ok, wait := b.Allow(t0.Add(4 * time.Minute).Add(30 * time.Second)); ok {
		t.Fatalf("failed probe must reopen the circuit")
	} else if wait <= 0 {
		t.Fatalf("reopened circuit: retryAfter must be positive, got %s", wait)
	}
}

// dispatchTask runs one dispatch against the executor under test.
func dispatchTask(t *testing.T, e *HTTPExecutor, rawURL string) error {
	t.Helper()
	return e.Handler()(context.Background(), Task{ID: "j1", Meta: map[string]any{"executor_url": rawURL, "async": false}})
}

// TestHTTPExecutorBreakerFastFailsDeadHost: after threshold 5xx responses the
// breaker fast-fails WITHOUT reaching the server (request count freezes).
func TestHTTPExecutorBreakerFastFailsDeadHost(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := mustHTTP(t, HTTPOptions{Timeout: 5 * time.Second, BreakerThreshold: 2, BreakerCooldownSeconds: 60})
	for i := 0; i < 2; i++ {
		if err := dispatchTask(t, e, srv.URL); err == nil {
			t.Fatalf("dispatch %d: want 500 error", i)
		}
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("server hits = %d, want 2", got)
	}
	err := dispatchTask(t, e, srv.URL)
	if err == nil || !errors.Is(err, ErrRetry) || !strings.Contains(err.Error(), "circuit open") {
		t.Fatalf("fast-fail err = %v, want retryable circuit-open error", err)
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Fatalf("open circuit must not reach the server: hits = %d, want 2", got)
	}
}

// TestHTTPExecutorBreakerRecoversAfterCooldown: cooldown elapsed + healthy
// target → probe closes the circuit and normal dispatch resumes.
func TestHTTPExecutorBreakerRecoversAfterCooldown(t *testing.T) {
	var hits int64
	healthy := int32(0) // 0 = always 500, 1 = 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if atomic.LoadInt32(&healthy) == 1 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := mustHTTP(t, HTTPOptions{Timeout: 5 * time.Second, BreakerThreshold: 2, BreakerCooldownSeconds: 1})
	_ = dispatchTask(t, e, srv.URL)
	_ = dispatchTask(t, e, srv.URL)
	if err := dispatchTask(t, e, srv.URL); !strings.Contains(err.Error(), "circuit open") {
		t.Fatalf("expected open circuit, got %v", err)
	}

	// Target recovers; the breaker must too — wait out the 1s cooldown.
	atomic.StoreInt32(&healthy, 1)
	time.Sleep(1100 * time.Millisecond)
	if err := dispatchTask(t, e, srv.URL); err != nil {
		t.Fatalf("probe dispatch after recovery: %v", err)
	}
	before := atomic.LoadInt64(&hits)
	if err := dispatchTask(t, e, srv.URL); err != nil {
		t.Fatalf("dispatch after close: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != before+1 {
		t.Fatalf("closed circuit must pass through: hits %d -> %d", before, got)
	}
}

// TestHTTPExecutorBreakerIgnores4xx: 4xx is a live server rejecting
// deterministically — dispatches keep reaching it no matter how many.
func TestHTTPExecutorBreakerIgnores4xx(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	e := mustHTTP(t, HTTPOptions{Timeout: 5 * time.Second, BreakerThreshold: 2, BreakerCooldownSeconds: 60})
	for i := 0; i < 5; i++ {
		if err := dispatchTask(t, e, srv.URL); err == nil {
			t.Fatalf("dispatch %d: want 400 error", i)
		}
	}
	if got := atomic.LoadInt64(&hits); got != 5 {
		t.Fatalf("4xx must never trip the breaker: hits = %d, want 5", got)
	}
}

// TestHTTPExecutorBreakerProbeSlotNotLeakedOnEarlyReturn: failures BEFORE
// the dispatch (TLS setup) must not consume or leak a half-open probe slot —
// the breaker only records what flows through Do. Regression: Allow used to
// run before clientForTLS, so a broken CA config leaked the probe slot and
// permanently denied the host.
func TestHTTPExecutorBreakerProbeSlotNotLeakedOnEarlyReturn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := mustHTTP(t, HTTPOptions{Timeout: 5 * time.Second, CAFile: "definitely-missing.pem", BreakerThreshold: 1, BreakerCooldownSeconds: 3600})
	for i := 0; i < 3; i++ {
		err := dispatchTask(t, e, srv.URL)
		if err == nil || !strings.Contains(err.Error(), "TLS setup") {
			t.Fatalf("dispatch %d: err = %v, want TLS setup error (not circuit open)", i, err)
		}
	}
}

// TestHTTPExecutorBreakerDisabled: threshold 0 (default) keeps legacy
// behaviour — every dispatch reaches the target.
func TestHTTPExecutorBreakerDisabled(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := mustHTTP(t, HTTPOptions{Timeout: 5 * time.Second})
	for i := 0; i < 5; i++ {
		_ = dispatchTask(t, e, srv.URL)
	}
	if got := atomic.LoadInt64(&hits); got != 5 {
		t.Fatalf("breaker disabled: hits = %d, want 5", got)
	}
	if e.breakerFor("any-host") != nil {
		t.Fatalf("disabled breaker must not create per-host breakers")
	}
}
