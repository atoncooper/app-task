package router

// Tests for the console CSRF hardening: session-bound synchronizer token on
// POST /console/* form flows, SameOrigin on the login/logout endpoints, and
// the header-credential bypass (scripts are CSRF-immune by construction).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"app-task/internal/auth"
)

// doForm submits an urlencoded form post (the console's own POST shape).
func doForm(h http.Handler, method, path string, vals url.Values, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// consoleCSRF extracts the session's CSRF token from a gated console page —
// what the browser's rendered forms carry. Test helper for console form
// posts; /console/password is self-service for every logged-in session and
// its form renders unconditionally, so the csrf_field partial is always
// present (list pages render forms inside {{range}} and show none when
// empty).
func consoleCSRF(t *testing.T, h http.Handler, cookie string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/console/password", nil)
	req.Header.Set("Cookie", cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /console/password for csrf token: status = %d", w.Code)
	}
	m := regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`)
	if mm := m.FindStringSubmatch(w.Body.String()); mm != nil {
		return mm[1]
	}
	t.Fatalf("csrf_token missing from console page render")
	return ""
}

// loginConsole performs the JSON login and returns "name=value" cookie pair
// plus the raw session id.
func loginConsole(t *testing.T, h http.Handler) (cookie, sid string) {
	t.Helper()
	w := doJSON(h, "POST", "/api/login", `{"username":"admin","password":"app-task-admin"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login: got %d, want 200", w.Code)
	}
	var resp struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Session == "" {
		t.Fatalf("login body: %s", w.Body.String())
	}
	return auth.SessionCookieName + "=" + resp.Session, resp.Session
}

// csrfTokenFromPage extracts the synchronizer token the console embeds into
// every form (base.tmpl csrf_field partial).
func csrfTokenFromPage(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`name="csrf_token" value="([0-9a-f]{64})"`)
	if mm := m.FindStringSubmatch(body); mm != nil {
		return mm[1]
	}
	t.Fatalf("csrf_token not rendered into console forms")
	return ""
}

// TestConsoleCSRFEnforcement: cookie-authenticated form posts must present
// the session's synchronizer token; header credentials skip the check.
func TestConsoleCSRFEnforcement(t *testing.T) {
	h := newWebUITestRouter(t, "")
	cookie, sid := loginConsole(t, h)

	// GET renders the token into the users page forms.
	g := doJSON(h, "GET", "/console/users", "", map[string]string{"Cookie": cookie})
	if g.Code != http.StatusOK {
		t.Fatalf("GET /console/users: got %d, want 200", g.Code)
	}
	tok := csrfTokenFromPage(t, g.Body.String())

	vals := url.Values{"username": {"csrfbob"}, "password": {"password123"}, "role": {"member"}}

	// Missing token -> 403.
	if w := doForm(h, "POST", "/console/users", vals, map[string]string{"Cookie": cookie}); w.Code != http.StatusForbidden {
		t.Fatalf("POST without token: got %d, want 403", w.Code)
	}
	// Wrong token -> 403.
	bad := url.Values{"username": {"csrfbob"}, "password": {"password123"}, "role": {"member"}, "csrf_token": {"deadbeef"}}
	if w := doForm(h, "POST", "/console/users", bad, map[string]string{"Cookie": cookie}); w.Code != http.StatusForbidden {
		t.Fatalf("POST with wrong token: got %d, want 403", w.Code)
	}
	// Correct token -> 303 redirect (POST-redirect-GET flow).
	good := url.Values{"username": {"csrfbob"}, "password": {"password123"}, "role": {"member"}, "csrf_token": {tok}}
	if w := doForm(h, "POST", "/console/users", good, map[string]string{"Cookie": cookie}); w.Code != http.StatusSeeOther {
		t.Fatalf("POST with session token: got %d, want 303", w.Code)
	}

	// Header-credential caller (script) skips CSRF: no cookie to auto-attach.
	if w := doForm(h, "POST", "/console/users", vals, map[string]string{"X-WebUI-Token": sid}); w.Code != http.StatusSeeOther {
		t.Fatalf("POST via header credential: got %d, want 303 (CSRF must not apply)", w.Code)
	}
}

// TestConsoleCSRFLegacySessionHeals: a session persisted before the
// CSRFToken field existed (rolling deploy, shared Redis) is minted one on
// the next page render instead of locking the operator out.
func TestConsoleCSRFLegacySessionHeals(t *testing.T) {
	h := newWebUITestRouter(t, "")
	cookie, _ := loginConsole(t, h)

	// First render heals the session (token minted and embedded).
	g := doJSON(h, "GET", "/console/users", "", map[string]string{"Cookie": cookie})
	if g.Code != http.StatusOK {
		t.Fatalf("GET: got %d, want 200", g.Code)
	}
	tok1 := csrfTokenFromPage(t, g.Body.String())

	// Second render yields the SAME token (persisted, not re-minted).
	g2 := doJSON(h, "GET", "/console/users", "", map[string]string{"Cookie": cookie})
	tok2 := csrfTokenFromPage(t, g2.Body.String())
	if tok1 != tok2 {
		t.Fatalf("csrf token changed between renders (not persisted)")
	}
}

// TestLoginSameOrigin: cross-site Origin/Referer on the login endpoint is
// rejected (login-CSRF), same-origin and header-less callers pass.
func TestLoginSameOrigin(t *testing.T) {
	h := newWebUITestRouter(t, "")

	if w := doJSON(h, "POST", "/api/login", `{"username":"admin","password":"app-task-admin"}`,
		map[string]string{"Origin": "http://evil.example"}); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login: got %d, want 403", w.Code)
	}
	if w := doJSON(h, "POST", "/api/login", `{"username":"admin","password":"app-task-admin"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("header-less login: got %d, want 200", w.Code)
	}
}
