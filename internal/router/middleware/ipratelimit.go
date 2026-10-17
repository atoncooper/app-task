// IPRequestLimit: generic per-IP request rate cap across every surface —
// console pages, /api/*, and the key-authenticated service surface. The
// third protection layer beyond the auth-failure throttle (which counts
// failed credentials only) and the per-key rate cap (which needs a valid
// key): this one bounds any single source regardless of credential state.
package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// IPRateLimit caps requests per minute per client IP. rpm <= 0 disables the
// gate entirely (the default — gateway-fronted deployments see every client
// behind the gateway IP, so enabling there would rate-limit the whole
// gateway as one bucket; direct-port deployments are the intended users).
// /health and /assets are exempt: monitoring probes and browser static
// bursts are not attack traffic. Counters are in-process (per instance) with
// bounded memory — multi-instance deployments scale the effective cap by
// instance count, which is acceptable for a best-effort bulkhead; the
// per-key cap remains the shared, authoritative limit.
func IPRateLimit(rpm int) gin.HandlerFunc {
	if rpm <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	l := newIPLimiter(1 << 16)
	return func(c *gin.Context) {
		// CORS preflight must neither consume the budget nor get 429'd (a
		// blocked preflight takes the whole origin down).
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		p := c.Request.URL.Path
		if p == "/health" || strings.HasPrefix(p, "/assets/") {
			c.Next()
			return
		}
		ip := c.ClientIP()
		if !l.allow(ip, rpm) {
			slog.Warn("[IPLIMIT] per-ip rate limit exceeded", "ip", ip, "rpm", rpm)
			c.Header("Retry-After", "60")
			RespondError(c, http.StatusTooManyRequests, "rate_limited", "too many requests from your address")
			c.Abort()
			return
		}
		c.Next()
	}
}

// ipWindow is one client's counter for the current fixed minute bucket.
type ipWindow struct {
	minute int64
	count  int
}

// ipLimiter is a fixed-window per-IP counter with bounded memory: when the
// map grows past cap it sweeps finished buckets, and under sustained flood
// beyond cap it resets wholesale — shedding counters beats growing without
// bound. The fixed window trades a 2× boundary burst for O(1) work per
// request; the per-key limiter (sliding authority for real clients) uses
// the same semantics.
type ipLimiter struct {
	mu      sync.Mutex
	windows map[string]*ipWindow
	cap     int
}

func newIPLimiter(cap int) *ipLimiter {
	return &ipLimiter{windows: make(map[string]*ipWindow), cap: cap}
}

// allow consumes one request for ip in the current minute bucket.
func (l *ipLimiter) allow(ip string, rpm int) bool {
	minute := time.Now().Unix() / 60
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.windows) >= l.cap {
		l.sweepLocked(minute)
	}
	w := l.windows[ip]
	if w == nil || w.minute != minute {
		l.windows[ip] = &ipWindow{minute: minute, count: 1}
		return true
	}
	w.count++
	return w.count <= rpm
}

// sweepLocked drops buckets from earlier minutes; a map still at cap after
// the sweep (single-minute flood) resets — availability of the limiter
// itself outranks counter precision.
func (l *ipLimiter) sweepLocked(curMinute int64) {
	for k, w := range l.windows {
		if w.minute < curMinute {
			delete(l.windows, k)
		}
	}
	if len(l.windows) >= l.cap {
		l.windows = make(map[string]*ipWindow)
	}
}
