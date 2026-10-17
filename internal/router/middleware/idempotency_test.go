package middleware

// Tests for the Idempotency-Key middleware: replay within TTL, scope
// isolation per credential, no-key passthrough, non-2xx never cached, and
// the disabled (ttl<=0) passthrough.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"app-task/internal/auth"

	"github.com/gin-gonic/gin"
)

// newIdempotencyEngine mounts the gate with a counting echo handler that
// answers "n=<executions>" so tests can assert how often it really ran.
func newIdempotencyEngine(t *testing.T, ttl time.Duration) (*gin.Engine, *int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	executions := 0
	var mu sync.Mutex
	e.Use(Idempotency(ttl))
	e.POST("/internal/email/send", func(c *gin.Context) {
		mu.Lock()
		executions++
		n := executions
		mu.Unlock()
		if c.GetHeader("X-Fail") == "1" {
			c.JSON(http.StatusInternalServerError, gin.H{"detail": "boom"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"n": n})
	})
	return e, &executions
}

func doIdem(e *gin.Engine, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/internal/email/send", nil)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// TestIdempotencyReplay: the second request with the same key replays the
// first response without executing the handler again.
func TestIdempotencyReplay(t *testing.T) {
	e, exec := newIdempotencyEngine(t, time.Minute)

	w1 := doIdem(e, "abc")
	w2 := doIdem(e, "abc")
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("codes = %d/%d, want 200/200", w1.Code, w2.Code)
	}
	if w1.Body.String() != w2.Body.String() {
		t.Fatalf("replayed body differs: %s vs %s", w1.Body.String(), w2.Body.String())
	}
	if w2.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay must carry Idempotency-Replayed: true")
	}
	if *exec != 1 {
		t.Fatalf("handler executions = %d, want 1", *exec)
	}
}

// TestIdempotencyScopeIsolation: the same key under a different API
// credential executes separately; a different key too.
func TestIdempotencyScopeIsolation(t *testing.T) {
	e, exec := newIdempotencyEngine(t, time.Minute)

	if w := doIdem(e, "k1"); w.Code != http.StatusOK {
		t.Fatalf("first call: %d", w.Code)
	}
	if w := doIdem(e, "k2"); w.Code != http.StatusOK || w.Header().Get("Idempotency-Replayed") == "true" {
		t.Fatalf("different key must execute fresh, got %d", w.Code)
	}
	// Credential scoping: CurrentAPIKeyName is empty on this engine, so both
	// calls share the empty scope — same key replays.
	if w := doIdem(e, "k1"); w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("same key same scope must replay")
	}
	if *exec != 2 {
		t.Fatalf("handler executions = %d, want 2", *exec)
	}
}

// TestIdempotencyNoKeyPasses: callers without the header execute every time.
func TestIdempotencyNoKeyPasses(t *testing.T) {
	e, exec := newIdempotencyEngine(t, time.Minute)
	doIdem(e, "")
	doIdem(e, "")
	if *exec != 2 {
		t.Fatalf("handler executions = %d, want 2 (no key = no dedup)", *exec)
	}
}

// TestIdempotency5xxNotCached: a failing execution releases the slot so the
// retry executes for real.
func TestIdempotency5xxNotCached(t *testing.T) {
	e, exec := newIdempotencyEngine(t, time.Minute)

	w1 := doIdemWith(e, "abc", "1")
	if w1.Code != http.StatusInternalServerError {
		t.Fatalf("first: %d, want 500", w1.Code)
	}
	w2 := doIdemWith(e, "abc", "0")
	if w2.Code != http.StatusOK || w2.Header().Get("Idempotency-Replayed") == "true" {
		t.Fatalf("retry after 5xx must execute fresh, got %d", w2.Code)
	}
	if *exec != 2 {
		t.Fatalf("handler executions = %d, want 2", *exec)
	}
}

func doIdemWith(e *gin.Engine, key, fail string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/internal/email/send", nil)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-Fail", fail)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// TestIdempotencyDisabled: ttl <= 0 is a passthrough — no dedup at all.
func TestIdempotencyDisabled(t *testing.T) {
	e, exec := newIdempotencyEngine(t, 0)
	doIdem(e, "abc")
	doIdem(e, "abc")
	if *exec != 2 {
		t.Fatalf("handler executions = %d, want 2 (disabled)", *exec)
	}
}

// TestIdempotencyPanicReleasesSlot: a panicking handler must not leave the
// in-flight placeholder stuck — the key is usable again on the next request
// (re-panic preserved for the upstream GinRecovery).
func TestIdempotencyPanicReleasesSlot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(Idempotency(time.Minute))
	boom := true
	e.POST("/x", func(c *gin.Context) {
		if boom {
			panic("handler exploded")
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	do := func() (code int, panicked bool) {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.Header.Set("Idempotency-Key", "p1")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		return w.Code, false
	}

	if _, panicked := do(); !panicked {
		t.Fatalf("first request: handler panic must propagate (no recovery mounted here)")
	}
	boom = false
	code, panicked := do()
	if panicked {
		t.Fatalf("second request must execute normally (slot must be released)")
	}
	if code != http.StatusOK {
		t.Fatalf("second request: status = %d, want 200", code)
	}
}

// TestIdempotencyCredentialScope: the key is namespaced by the presented
// API credential, so two credentials with the same key string don't collide.
func TestIdempotencyCredentialScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	exec := 0
	e.Use(func(c *gin.Context) {
		// Simulate two different credentials alternating by header.
		auth.SetCredential(c, c.GetHeader("X-Test-Cred"), 0)
		c.Next()
	})
	e.Use(Idempotency(time.Minute))
	e.POST("/x", func(c *gin.Context) {
		exec++
		c.JSON(http.StatusOK, gin.H{"exec": exec})
	})

	do := func(cred, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.Header.Set("X-Test-Cred", cred)
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		return w
	}
	if w := do("credA", "same"); !strings.Contains(w.Body.String(), "1") {
		t.Fatalf("credA first: %s", w.Body.String())
	}
	if w := do("credB", "same"); !strings.Contains(w.Body.String(), "2") {
		t.Fatalf("credB same key must execute separately: %s", w.Body.String())
	}
	if w := do("credA", "same"); w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("credA same key must replay")
	}
}
