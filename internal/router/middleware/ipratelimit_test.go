package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

// newIPLimitRouter mounts the gate with the given rpm and a single echo route.
func newIPLimitRouter(t *testing.T, rpm int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	if err := e.SetTrustedProxies(nil); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	e.Use(IPRateLimit(rpm))
	e.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	e.GET("/health", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	e.GET("/assets/app.js", func(c *gin.Context) { c.String(http.StatusOK, "js") })
	return e
}

func doIPLimit(e *gin.Engine, remote, path string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w.Code
}

// TestIPRateLimitDisabled: rpm <= 0 keeps legacy behaviour (no cap).
func TestIPRateLimitDisabled(t *testing.T) {
	e := newIPLimitRouter(t, 0)
	for i := 0; i < 10; i++ {
		if got := doIPLimit(e, "203.0.113.9:1000", "/ping"); got != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (disabled)", i, got)
		}
	}
}

// TestIPRateLimitEnforced: rpm caps a single source; the exceeded request
// gets 429 with Retry-After.
func TestIPRateLimitEnforced(t *testing.T) {
	e := newIPLimitRouter(t, 3)
	for i := 0; i < 3; i++ {
		if got := doIPLimit(e, "203.0.113.9:1000", "/ping"); got != http.StatusOK {
			t.Fatalf("request %d within cap: status = %d, want 200", i, got)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.RemoteAddr = "203.0.113.9:1000"
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over cap: status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatalf("429 must carry Retry-After")
	}
}

// TestIPRateLimitPerIPIsolation: exhausting one source never affects another.
func TestIPRateLimitPerIPIsolation(t *testing.T) {
	e := newIPLimitRouter(t, 2)
	for i := 0; i < 2; i++ {
		doIPLimit(e, "203.0.113.9:1000", "/ping")
	}
	if got := doIPLimit(e, "198.51.100.7:2000", "/ping"); got != http.StatusOK {
		t.Fatalf("other IP: status = %d, want 200 (limits are per-IP)", got)
	}
	if got := doIPLimit(e, "203.0.113.9:1000", "/ping"); got != http.StatusTooManyRequests {
		t.Fatalf("exhausted IP: status = %d, want 429", got)
	}
}

// TestIPRateLimitExemptPaths: /health and /assets are not counted.
func TestIPRateLimitExemptPaths(t *testing.T) {
	e := newIPLimitRouter(t, 1)
	doIPLimit(e, "203.0.113.9:1000", "/ping") // burn the single slot
	if got := doIPLimit(e, "203.0.113.9:1000", "/health"); got != http.StatusOK {
		t.Fatalf("/health: status = %d, want 200 (exempt)", got)
	}
	if got := doIPLimit(e, "203.0.113.9:1000", "/assets/app.js"); got != http.StatusOK {
		t.Fatalf("/assets: status = %d, want 200 (exempt)", got)
	}
}

// TestIPRateLimitConcurrentCap: concurrent requests from one source never
// exceed the cap (counter race would let the burst through).
func TestIPRateLimitConcurrentCap(t *testing.T) {
	e := newIPLimitRouter(t, 50)
	const total = 200
	var wg sync.WaitGroup
	ok := make(chan struct{}, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if doIPLimit(e, "203.0.113.9:1000", "/ping") == http.StatusOK {
				ok <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(ok)
	if n := len(ok); n != 50 {
		t.Fatalf("accepted = %d, want exactly 50 (fixed-window cap)", n)
	}
}
