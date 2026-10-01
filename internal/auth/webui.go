// WebUI session authentication for the admin console.
//
// Two credential kinds are accepted, both presented via X-WebUI-Token /
// Authorization: Bearer:
//   - user sessions issued by POST /api/login (username + password), stored in
//     an HttpOnly cookie for the browser and/or a session id for API clients;
//   - the optional master token (webui.token, constant-time compare) as an
//     API-key fallback for scripts — it authenticates as admin.
//
// Failed attempts are throttled per client IP across ALL /api/* endpoints.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"app-task/internal/dto"
	"app-task/internal/repo"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

const (
	// Brute-force throttle: max failed credentials per client IP per window,
	// shared by the login endpoint and the /api/* gate so the limit cannot be
	// bypassed by hammering any other endpoint.
	webuiMaxFails     = 10
	webuiFailWindow   = time.Minute
	webuiDefaultTTL   = 12 * time.Hour
	webuiSessionBytes = 32

	// SessionCookieName is the HttpOnly cookie the browser flow rides; the
	// server gates the console PAGE itself with it (redirect to /login before
	// any app HTML is sent). Programmatic clients use the session id / master
	// token via headers instead.
	SessionCookieName = "apptask_session"

	ctxUser = "webui_user" // gin context key for the authed identity
)

// Session is the identity attached to a browser/API session: it always
// carries a username and role so handlers can authorize and audit.
type Session struct {
	UserID   string // UUID of the webui_user row ("" for master-token sessions)
	Username string
	Role     string // admin / member
	Expiry   time.Time
}

// Authenticator gates the admin console.
type Authenticator struct {
	masterToken string
	ttl         time.Duration

	// Backing stores: memory by default (single instance), Redis for
	// multi-instance console deployments (see state_stores.go / config).
	sessions sessionStore
	throttle throttler
}

// NewWebUIAuthenticator builds the console authenticator. Nil stores fall
// back to in-process memory; ttlMinutes <= 0 uses the 12h default.
func NewWebUIAuthenticator(token string, ttlMinutes int, sessions sessionStore, throttle throttler) *Authenticator {
	ttl := webuiDefaultTTL
	if ttlMinutes > 0 {
		ttl = time.Duration(ttlMinutes) * time.Minute
	}
	if sessions == nil {
		sessions = newMemorySessionStore()
	}
	if throttle == nil {
		throttle = newMemoryThrottler(webuiMaxFails, webuiFailWindow)
	}
	return &Authenticator{
		masterToken: token,
		ttl:         ttl,
		sessions:    sessions,
		throttle:    throttle,
	}
}

// Middleware gates /api/* on a valid credential and stashes the identity for
// downstream handlers. Any failed check counts toward the per-IP throttle; a
// success clears it.
func (a *Authenticator) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if !a.Allow(ip) {
			a.ThrottleAbort(c)
			return
		}
		sess, ok := a.Authenticate(a.ExtractToken(c))
		if !ok {
			a.Fail(ip)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"detail": "unauthorized (login required)"})
			return
		}
		a.Reset(ip)
		c.Set(ctxUser, sess)
		c.Next()
	}
}

// Login exchanges credentials for a fresh session. Accepts either a username +
// password (console login) or the master token (script/API fallback). The
// session is issued BOTH as an HttpOnly+SameSite=Strict cookie (browser flow)
// and in the JSON body (API flow, presented via X-WebUI-Token / Bearer).
func (a *Authenticator) Login(c *gin.Context) {
	var req dto.LoginRequest
	_ = c.ShouldBindJSON(&req)
	ip := c.ClientIP()
	if !a.Allow(ip) {
		a.ThrottleAbort(c)
		return
	}

	var sess Session
	var ok bool
	switch {
	case req.Username != "" && req.Password != "":
		sess, ok = a.VerifyUser(req.Username, req.Password)
	case req.Token != "":
		sess, ok = a.Authenticate(req.Token)
	default:
		// Empty credentials are a fresh visitor, not an attack: reject
		// without burning the throttle budget.
		c.JSON(http.StatusUnauthorized, gin.H{"detail": "username/password or token required"})
		return
	}
	if !ok {
		a.Fail(ip)
		c.JSON(http.StatusUnauthorized, gin.H{"detail": "invalid credentials"})
		return
	}
	a.Reset(ip)

	sid, err := a.NewSession(sess)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "issue session failed"})
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sid,
		Path:     "/",
		MaxAge:   int(a.ttl.Seconds()),
		HttpOnly: true,                    // JS can never read it (XSS-proof storage)
		SameSite: http.SameSiteStrictMode, // cross-site requests never carry it (CSRF)
		Secure:   c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https",
	})
	c.JSON(http.StatusOK, gin.H{
		"ok":                 true,
		"session":            sid,
		"expires_in_minutes": int(a.ttl.Minutes()),
		"user":               gin.H{"username": sess.Username, "role": sess.Role, "is_admin": sess.Role == "admin"},
	})
}

// Logout invalidates the presented session and clears the session cookie.
func (a *Authenticator) Logout(c *gin.Context) {
	if tok := a.ExtractToken(c); tok != "" {
		a.sessions.delete(tok)
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (a *Authenticator) ThrottleAbort(c *gin.Context) {
	c.Header("Retry-After", strconv.Itoa(int(webuiFailWindow.Seconds())))
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"detail": "too many failed attempts, retry after the cooldown",
	})
}

// ExtractToken finds the presented credential: X-WebUI-Token header, Bearer
// token, or the session cookie (browser navigation requests can only carry
// the cookie, which is what enables the server-side page gate).
func (a *Authenticator) ExtractToken(c *gin.Context) string {
	got := c.GetHeader("X-WebUI-Token")
	if got == "" {
		if b := c.GetHeader("Authorization"); len(b) > 7 && b[:7] == "Bearer " {
			got = b[7:]
		}
	}
	if got == "" {
		got, _ = c.Cookie(SessionCookieName)
	}
	return got
}

// Authenticate resolves a credential (session id or master token) to an
// identity. The master token is compared in constant time and maps to an admin
// identity so scripts can call /api/* without a user session.
func (a *Authenticator) Authenticate(cred string) (Session, bool) {
	if cred == "" {
		return Session{}, false
	}
	if a.masterToken != "" && subtle.ConstantTimeCompare([]byte(cred), []byte(a.masterToken)) == 1 {
		return Session{Username: "master-token", Role: "admin", Expiry: time.Now().Add(a.ttl)}, true
	}
	return a.sessions.load(cred)
}

// verifyUser checks a username + password against the webui_user store.
func (a *Authenticator) VerifyUser(username, password string) (Session, bool) {
	u, err := repo.GetUserByUsername(username)
	if err != nil || u == nil {
		return Session{}, false
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return Session{}, false
	}
	return Session{UserID: u.UserID, Username: u.Username, Role: u.Role, Expiry: time.Now().Add(a.ttl)}, true
}

func (a *Authenticator) NewSession(user Session) (string, error) {
	b := make([]byte, webuiSessionBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	sid := hex.EncodeToString(b)
	if err := a.sessions.save(sid, user, a.ttl); err != nil {
		return "", err
	}
	return sid, nil
}

func (a *Authenticator) Allow(ip string) bool { return a.throttle.allow(ip) }

func (a *Authenticator) Fail(ip string) { a.throttle.fail(ip) }

func (a *Authenticator) Reset(ip string) { a.throttle.reset(ip) }

// ── identity accessors for handlers ──────────────────────────────────

// TTL returns the configured session lifetime (console form flows reuse it
// for the cookie Max-Age).
func (a *Authenticator) TTL() time.Duration { return a.ttl }

// RevokeSession deletes one session id (form-flow logout).
func (a *Authenticator) RevokeSession(token string) { a.sessions.delete(token) }

// CurrentUser reads the authed identity set by the auth middleware.
func CurrentUser(c *gin.Context) (Session, bool) {
	v, ok := c.Get(ctxUser)
	if !ok {
		return Session{}, false
	}
	s, ok := v.(Session)
	return s, ok
}

// OperatorOf returns the authed username for audit logging (empty for master
// token / header-only callers).
func OperatorOf(c *gin.Context) string {
	if s, ok := CurrentUser(c); ok && s.Username != "" {
		return s.Username
	}
	return ""
}

// RequireAdmin gates a route group to role=admin.
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s, ok := CurrentUser(c); ok && s.Role == "admin" {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"detail": "admin role required"})
	}
}

// PageGate gates the console HTML pages: unauthenticated page requests are
// redirected to /login BEFORE any HTML is sent (the 302 twin of Middleware),
// and it stashes the identity for handlers.
func (a *Authenticator) PageGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		sess, ok := a.Authenticate(a.ExtractToken(c))
		if !ok {
			c.Redirect(http.StatusFound, "/login")
			c.Abort()
			return
		}
		c.Set(ctxUser, sess)
		c.Next()
	}
}
