package middleware

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// Client IP resolution, done safely.
//
// chi's middleware.RealIP is deprecated precisely because it rewrites
// r.RemoteAddr from X-Forwarded-For unconditionally (GHSA-3fxj-6jh8-hvhx and
// friends). Any client can send that header. On this platform the consequence
// is not theoretical: IPAllowlist guards the admin surface, so a spoofable
// client IP turns a documented security control into decoration.
//
// The rule implemented here: proxy headers are believed only when the
// connection actually arrives from a proxy we trust. Otherwise the TCP peer
// address is the answer, and no header can change it.

// TrustedProxies holds the networks whose forwarding headers we honour.
type TrustedProxies struct {
	nets []*net.IPNet
}

// NewTrustedProxies parses a CIDR list. Entries without a prefix length are
// treated as single hosts. Malformed entries are skipped rather than fatal, so
// one typo in a config map cannot prevent a pod from starting -- but the caller
// is told which ones were dropped.
func NewTrustedProxies(cidrs []string) (proxies *TrustedProxies, malformed []string) {
	tp := &TrustedProxies{}
	var bad []string

	for _, raw := range cidrs {
		c := strings.TrimSpace(raw)
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
			bad = append(bad, raw)
			continue
		}
		tp.nets = append(tp.nets, n)
	}
	return tp, bad
}

// DefaultTrustedProxies covers the private ranges a Kubernetes ingress or a
// docker-compose network occupies. It deliberately does NOT include any public
// range: if you terminate at Cloudflare, configure Cloudflare's published
// ranges explicitly rather than trusting the internet.
func DefaultTrustedProxies() *TrustedProxies {
	tp, _ := NewTrustedProxies([]string{
		"127.0.0.0/8", "::1/128",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"fc00::/7", // unique local addresses
	})
	return tp
}

// trusts reports whether addr is a proxy whose headers we accept.
func (t *TrustedProxies) trusts(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range t.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP resolves the caller's address.
//
// When the peer is a trusted proxy, X-Forwarded-For is walked from the RIGHT,
// skipping addresses that are themselves trusted proxies, and the first
// untrusted address is the client. Reading from the left -- which is what the
// deprecated RealIP does -- takes whatever the client typed, because the client
// controls the start of that list.
//
// When the peer is not trusted, forwarding headers are ignored entirely.
func (t *TrustedProxies) ClientIP(r *http.Request) string {
	peer := peerIP(r)
	if peer == nil {
		return ""
	}
	if !t.trusts(peer) {
		return peer.String()
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip == nil {
				continue
			}
			if !t.trusts(ip) {
				return ip.String()
			}
		}
	}

	// X-Real-IP carries a single value and is only meaningful from a proxy we
	// already decided to trust.
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if ip := net.ParseIP(xr); ip != nil {
			return ip.String()
		}
	}
	return peer.String()
}

// peerIP returns the TCP peer address, which no header can influence.
func peerIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(strings.TrimSpace(host))
}

type clientIPKey struct{}

func contextWithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// RealIP resolves the client address once per request and stashes it, replacing
// chi's deprecated middleware of the same name. Downstream code reads it via
// ClientIPFrom and never re-parses headers.
func RealIP(tp *TrustedProxies) func(http.Handler) http.Handler {
	if tp == nil {
		tp = DefaultTrustedProxies()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := tp.ClientIP(r)
			ctx := contextWithClientIP(r.Context(), ip)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ClientIPFrom returns the resolved client address, falling back to the raw
// peer address when RealIP was not installed.
func ClientIPFrom(r *http.Request) string {
	if v, ok := r.Context().Value(clientIPKey{}).(string); ok && v != "" {
		return v
	}
	if ip := peerIP(r); ip != nil {
		return ip.String()
	}
	return ""
}
