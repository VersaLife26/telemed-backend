package middleware

import (
	"fmt"
	"net"
	"strings"
)

// CIDRContainsAny parses each of cidrs (accepting a bare IP as a /32 or /128,
// exactly like IPAllowlist) and reports whether ip matches at least one. It
// exists so packages that need a narrower, per-principal IP scope on top of
// the service-wide IPAllowlist (see internal/adminusers) reuse the same CIDR
// parsing rules rather than a second, possibly-inconsistent implementation.
//
// LOCAL PLATFORM ADDITION (telemed-admin-service). It lives in its own file so
// a future `cp -a _shared/template/internal/platform/.` sync cannot silently
// drop it the way it would if it sat in middleware.go. Promote it to the
// template when the next platform pass runs.
func CIDRContainsAny(ip string, cidrs []string) (bool, error) {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false, fmt.Errorf("middleware: %q is not a valid IP", ip)
	}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			if strings.Contains(c, ":") {
				c += "/128"
			} else {
				c += "/32"
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue // a malformed stored CIDR is skipped, never treated as a wildcard match
		}
		if n.Contains(parsed) {
			return true, nil
		}
	}
	return false, nil
}
