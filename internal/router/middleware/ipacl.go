// IPACL: source-IP allow/deny gate for the whole engine. Parsed from config
// CIDR lists; deny wins over allow; both empty disables the gate (default).
package middleware

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// IPACL gates every request on the client's source IP against configured
// allow/deny lists. Semantics: a deny-list match always rejects; otherwise a
// non-empty allow list rejects anything it does not contain; two empty lists
// disable the gate entirely (default, backward compatible).
//
// The gate sits before auth on purpose: it closes the direct-to-port path
// (gateway-fronted deployments restrict the port to the gateway subnet),
// which no credential check can express. Entries are single IPs or CIDRs
// ("10.0.0.0/8", "203.0.113.7", "::1/128"); they are parsed once here so the
// per-request path never parses strings. Invalid entries panic at startup —
// a half-applied ACL is worse than none.
func IPACL(allow, deny []string) gin.HandlerFunc {
	allowNets, err := parseIPEntries(allow)
	if err != nil {
		panic("security.ip_acl.allow: " + err.Error())
	}
	denyNets, err := parseIPEntries(deny)
	if err != nil {
		panic("security.ip_acl.deny: " + err.Error())
	}
	if len(allowNets) == 0 && len(denyNets) == 0 {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		raw := c.ClientIP()
		ip := net.ParseIP(raw)
		if ip == nil {
			// Unparseable source with an active list: fail closed.
			slog.Warn("[IPACL] rejected unparseable client ip", "raw", raw)
			RespondError(c, http.StatusForbidden, "forbidden", "access denied by ip policy")
			c.Abort()
			return
		}
		deny := containsNet(denyNets, ip)
		allowBlocked := len(allowNets) > 0 && !containsNet(allowNets, ip)
		if deny || allowBlocked {
			slog.Warn("[IPACL] rejected request",
				"ip", raw, "reason", map[bool]string{true: "deny_list", false: "allow_list"}[deny])
			RespondError(c, http.StatusForbidden, "forbidden", "access denied by ip policy")
			c.Abort()
			return
		}
		c.Next()
	}
}

// parseIPEntries converts allow/deny entries to nets; bare IPs become /32
// (IPv4) or /128 (IPv6) single-host nets.
func parseIPEntries(entries []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, ipnet, err := net.ParseCIDR(e); err == nil {
			nets = append(nets, ipnet)
			continue
		}
		ip := net.ParseIP(e)
		if ip == nil {
			return nil, fmt.Errorf("invalid ip or cidr entry %q", e)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return nets, nil
}

// containsNet reports whether ip falls inside any of the nets.
func containsNet(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
