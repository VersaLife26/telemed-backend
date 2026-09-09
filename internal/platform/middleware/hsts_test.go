package middleware

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

const hstsHeader = "Strict-Transport-Security"

// TestHSTSIsEmittedBehindATerminatingProxy pins SECURITY-REVIEW F29.
//
// SecurityHeaders gated HSTS on r.TLS != nil. Every service on this platform
// runs behind ingress or the api-gateway, both of which terminate TLS and
// speak plaintext to the pod, so r.TLS was nil on every request in production
// and the header was never emitted. The control read as present in review and
// was inert in fact -- the failure mode this test exists to prevent.
func TestHSTSIsEmittedBehindATerminatingProxy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto string
		tls   bool
		want  bool
	}{
		{name: "plaintext with no forwarded header", want: false},
		{name: "direct TLS", tls: true, want: true},
		{name: "terminated at ingress", proto: "https", want: true},
		{name: "case is not significant", proto: "HTTPS", want: true},
		{name: "proxy chain uses the left-most scheme", proto: "https, http", want: true},
		{name: "client spoke plaintext to the proxy", proto: "http", want: false},
		{name: "chain that began in plaintext", proto: "http, https", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", http.NoBody)
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.tls {
				// A non-nil ConnectionState is exactly what net/http hands a
				// handler on a TLS listener; the contents are irrelevant here.
				req.TLS = &tls.ConnectionState{HandshakeComplete: true}
			}
			rec := httptest.NewRecorder()
			SecurityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)

			got := rec.Header().Get(hstsHeader) != ""
			if got != tc.want {
				t.Fatalf("%s header present = %v, want %v (X-Forwarded-Proto=%q, r.TLS set=%v)",
					hstsHeader, got, tc.want, tc.proto, tc.tls)
			}
		})
	}
}

// TestUnconditionalSecurityHeadersDoNotDependOnScheme guards against a future
// edit hanging the rest of the baseline set off the same condition. Those
// headers are cheap and correct on any transport; only HSTS is scheme-bound.
func TestUnconditionalSecurityHeadersDoNotDependOnScheme(t *testing.T) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", http.NoBody)
	rec := httptest.NewRecorder()
	SecurityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)

	for _, h := range []string{
		"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy",
		"Permissions-Policy", "Cross-Origin-Opener-Policy", "Content-Security-Policy",
	} {
		if rec.Header().Get(h) == "" {
			t.Errorf("%s missing on a plaintext request; it should be unconditional", h)
		}
	}
}
