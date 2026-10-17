package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newIPACLRouter builds a minimal engine with the IPACL gate mounted and
// trusted proxies disabled (mirrors production wiring in router.New).
func newIPACLRouter(t *testing.T, allow, deny []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	if err := e.SetTrustedProxies(nil); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	e.Use(IPACL(allow, deny))
	e.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	return e
}

// doIPACL sends a GET /ping pretending to come from the given remote address
// and returns the response status code.
func doIPACL(t *testing.T, e *gin.Engine, remote string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.RemoteAddr = remote
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w.Code
}

// TestIPACLDisabled: both lists empty -> gate off, any source passes.
func TestIPACLDisabled(t *testing.T) {
	e := newIPACLRouter(t, nil, nil)
	if got := doIPACL(t, e, "203.0.113.9:1000"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (gate must be a no-op when lists are empty)", got)
	}
}

// TestIPACLDenyList: CIDR deny match rejects, non-matching source passes.
func TestIPACLDenyList(t *testing.T) {
	e := newIPACLRouter(t, nil, []string{"203.0.113.0/24"})
	if got := doIPACL(t, e, "203.0.113.9:1000"); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CIDR deny match)", got)
	}
	if got := doIPACL(t, e, "198.51.100.7:1000"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (outside deny CIDR)", got)
	}
}

// TestIPACLAllowList: non-empty allow list rejects everything it does not contain.
func TestIPACLAllowList(t *testing.T) {
	e := newIPACLRouter(t, []string{"10.0.0.0/8"}, nil)
	if got := doIPACL(t, e, "10.1.2.3:5555"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (inside allow CIDR)", got)
	}
	if got := doIPACL(t, e, "203.0.113.9:1000"); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (outside allow CIDR)", got)
	}
}

// TestIPACLDenyOverridesAllow: an IP listed in both is rejected (deny wins).
func TestIPACLDenyOverridesAllow(t *testing.T) {
	e := newIPACLRouter(t, []string{"10.0.0.0/8"}, []string{"10.1.2.3"})
	if got := doIPACL(t, e, "10.1.2.3:5555"); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (deny must override allow)", got)
	}
	if got := doIPACL(t, e, "10.9.9.9:5555"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (allowed subnet, not denied)", got)
	}
}

// TestIPACLBareIPEntry: a bare IP entry matches exactly one host.
func TestIPACLBareIPEntry(t *testing.T) {
	e := newIPACLRouter(t, []string{"192.168.1.7"}, nil)
	if got := doIPACL(t, e, "192.168.1.7:9999"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (exact bare-IP allow)", got)
	}
	if got := doIPACL(t, e, "192.168.1.8:9999"); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (adjacent IP must not match)", got)
	}
}

// TestIPACLIPv6: IPv6 loopback via /128 CIDR entry.
func TestIPACLIPv6(t *testing.T) {
	e := newIPACLRouter(t, []string{"::1/128"}, nil)
	if got := doIPACL(t, e, "[::1]:443"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (IPv6 loopback allow)", got)
	}
	if got := doIPACL(t, e, "[2001:db8::1]:443"); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (other IPv6 host)", got)
	}
}

// TestIPACLInvalidEntryPanics: bad entries must fail loud at startup.
func TestIPACLInvalidEntryPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic on invalid ip/cidr entry")
		}
	}()
	IPACL([]string{"not-an-ip"}, nil)
}
