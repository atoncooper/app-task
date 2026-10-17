package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newSameOriginEngine mounts the gate with one allowlisted public origin
// (gateway-fronted deployments) and a single POST/GET echo route.
func newSameOriginEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(SameOrigin([]string{"http://public.example"}))
	e.POST("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	e.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return e
}

func doSameOrigin(e *gin.Engine, method string, headers map[string]string) int {
	req := httptest.NewRequest(method, "/x", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w.Code
}

// TestSameOriginAbsentHeadersPass: CLI/scripts send neither Origin nor
// Referer and must be untouched.
func TestSameOriginAbsentHeadersPass(t *testing.T) {
	e := newSameOriginEngine()
	if got := doSameOrigin(e, http.MethodPost, nil); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (absent headers must pass)", got)
	}
}

// TestSameOriginCrossSiteRejected: a cross-site Origin on an unsafe method
// is the CSRF signature — reject.
func TestSameOriginCrossSiteRejected(t *testing.T) {
	e := newSameOriginEngine()
	if got := doSameOrigin(e, http.MethodPost, map[string]string{"Origin": "http://evil.example"}); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (cross-site origin)", got)
	}
	if got := doSameOrigin(e, http.MethodPost, map[string]string{"Referer": "http://evil.example/attack"}); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (cross-site referer)", got)
	}
}

// TestSameOriginSameSiteAndAllowlistPass: request-host and allowlisted
// origins are trusted.
func TestSameOriginSameSiteAndAllowlistPass(t *testing.T) {
	e := newSameOriginEngine()
	if got := doSameOrigin(e, http.MethodPost, map[string]string{"Origin": "http://example.com"}); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (matches request host)", got)
	}
	if got := doSameOrigin(e, http.MethodPost, map[string]string{"Origin": "http://public.example"}); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (allowlisted origin)", got)
	}
}

// TestSameOriginSafeMethodsPass: GET with a cross-site Origin is a plain
// navigation/fetch — not in scope for the gate.
func TestSameOriginSafeMethodsPass(t *testing.T) {
	e := newSameOriginEngine()
	if got := doSameOrigin(e, http.MethodGet, map[string]string{"Origin": "http://evil.example"}); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (safe method)", got)
	}
}
