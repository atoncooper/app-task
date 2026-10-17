// SameOrigin: rejects state-changing requests that present a cross-site
// Origin/Referer. Defense-in-depth for endpoints a synchronizer token cannot
// cover (login/logout have no session to bind a token to).
package middleware

import (
	"log/slog"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
)

// SameOrigin blocks unsafe requests whose Origin (preferred) or Referer
// host differs from the request host. Headers carry no security decision
// when absent — non-browser clients (CLI, scripts, curl) never send them
// and pass untouched. allowOrigins extends the trusted hosts with the
// configured CORS origins so gateway-fronted deployments (public origin !=
// upstream Host) keep working. This closes login-CSRF (logging the victim
// into an attacker-chosen account), which a session token cannot express —
// the login page has no session yet.
func SameOrigin(allowOrigins []string) gin.HandlerFunc {
	allowed := make(map[string]bool, len(allowOrigins))
	for _, o := range allowOrigins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			allowed[u.Host] = true
		}
	}
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		for _, h := range []string{"Origin", "Referer"} {
			v := c.GetHeader(h)
			if v == "" {
				continue
			}
			u, err := url.Parse(v)
			if err != nil || u.Host == "" {
				continue // opaque/invalid header: nothing to compare
			}
			if u.Host == c.Request.Host || allowed[u.Host] {
				continue
			}
			slog.Warn("[CSRF] cross-site request rejected", "header", h, "origin", v, "ip", c.ClientIP(), "path", c.Request.URL.Path)
			RespondError(c, http.StatusForbidden, "forbidden", "cross-site request rejected")
			c.Abort()
			return
		}
		c.Next()
	}
}
