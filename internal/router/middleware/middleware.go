// Package middleware: HTTP middleware and shared response plumbing for the
// app-task gin engine. It sits between the router's endpoints and the auth
// service: credential extraction from headers/cookies happens here, while
// verification, throttles and scope checks delegate to internal/auth.
package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"app-task/internal/auth"
	"app-task/internal/repo"

	"github.com/gin-gonic/gin"
)

// RespondError writes the service-surface error envelope: a stable
// machine-readable code for callers to branch on, plus the legacy "detail"
// field so existing gateway consumers keep parsing. NOTE: unlike c.JSON this
// does NOT abort the chain — call c.Abort() explicitly in middleware gates.
func RespondError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{
		"detail": message,
		"error":  gin.H{"code": code, "message": message},
	})
}

// CORS reflects only config-allowlisted origins (credentials enabled), so
// cross-origin browser clients pass preflight.
func CORS(allowOrigins []string) gin.HandlerFunc {
	allowed := make(map[string]bool, len(allowOrigins))
	for _, o := range allowOrigins {
		allowed[o] = true
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if allowed[origin] {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			// X-WebUI-Token/X-Operator are the console's own headers; without
			// them here a cross-origin browser client fails CORS preflight.
			c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id, X-WebUI-Token, X-Operator")
			c.Header("Access-Control-Max-Age", "600")
			// The origin is reflected, so caches must not share responses
			// across origins.
			c.Header("Vary", "Origin")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// SecurityHeaders adds baseline hardening: no framing (clickjacking), no MIME
// sniffing, no referrer leakage, and no-store so admin data and task payloads
// never land in shared caches. The no-store covers the admin API (/api/*) and
// the key-authenticated service surface (/tasks, /internal, /scripts).
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Content-Security-Policy", "frame-ancestors 'none'")
		c.Header("Referrer-Policy", "no-referrer")
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/tasks") ||
			strings.HasPrefix(p, "/internal/") || strings.HasPrefix(p, "/scripts") {
			c.Header("Cache-Control", "no-store")
		}
		c.Next()
	}
}

// extractAPIKey pulls the service credential from any of the three accepted
// header forms. Cookie-based console sessions are NOT a service credential
// here — the console talks to the service surface through /api/* instead.
func extractAPIKey(c *gin.Context) string {
	if v := c.GetHeader("X-API-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := c.GetHeader("apikey"); v != "" {
		return strings.TrimSpace(v)
	}
	if b := c.GetHeader("Authorization"); len(b) > 7 && strings.EqualFold(b[:7], "Bearer ") {
		return strings.TrimSpace(b[7:])
	}
	return ""
}

// APIKeyAuth gates the service surface. Console sessions are also accepted so
// the embedded console can never be locked out of its own service endpoints
// (belt-and-braces; the console normally uses /api/*). consoleAuth may be nil
// when webui.enabled=false — key auth then stands alone. Auth failures are
// throttled per client IP and logged.
func APIKeyAuth(keys *auth.KeyService, consoleAuth *auth.Authenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		if consoleAuth != nil {
			if _, ok := consoleAuth.Authenticate(consoleAuth.ExtractToken(c)); ok {
				c.Next()
				return
			}
		}

		ip := c.ClientIP()
		if !keys.Allow(ip) {
			c.Header("Retry-After", "60")
			RespondError(c, http.StatusTooManyRequests, "rate_limited",
				"too many failed attempts, retry after the cooldown")
			c.Abort()
			return
		}
		presented := extractAPIKey(c)
		row, valid := keys.VerifyAPIKey(presented)
		if !valid {
			if presented != "" {
				slog.Warn("[APIKEY] rejected service request", "ip", ip, "prefix", presented[:min(10, len(presented))])
			}
			keys.Fail(ip)
			RespondError(c, http.StatusUnauthorized, "unauthorized",
				"unauthorized (valid API key required; generate one in the console)")
			c.Abort()
			return
		}

		keys.Reset(ip)

		// Scope enforcement: the key must grant the group this path belongs to.
		scope := auth.RequiredScope(c.Request.URL.Path)
		if !auth.KeyHasScope(row.Scopes, scope) {
			slog.Warn("[APIKEY] scope denied", "key", row.Name, "scope", scope, "ip", ip)
			RespondError(c, http.StatusForbidden, "forbidden",
				"api key lacks required scope: "+scope)
			c.Abort()
			return
		}

		// Per-key resource control: requests/minute cap.
		if !keys.AllowRequest(row.KeyID, row.RatePerMin) {
			slog.Warn("[APIKEY] rate limit exceeded", "key", row.Name, "rate_per_min", row.RatePerMin, "ip", ip)
			c.Header("Retry-After", "60")
			RespondError(c, http.StatusTooManyRequests, "rate_limited", "api key rate limit exceeded")
			c.Abort()
			return
		}

		repo.TouchAPIKeyUsed(row.KeyID, row.LastUsedAt)
		auth.SetCredential(c, row.Name, row.UID)
		c.Next()
	}
}
