package gateway

import (
	"net/http"
	"strings"

	chimw "github.com/go-chi/chi/v5/middleware"

	"telemed/internal/platform/middleware"
)

// Trusted internal headers. Only the gateway may set these -- a downstream
// service that trusts them is trusting that no client request ever reaches
// it carrying one directly. That trust is only sound because
// StripForgedHeaders removes any client-supplied copy before anything else
// in the request pipeline runs, including authentication.
const (
	HeaderUserID    = "X-Telemed-User-ID"
	HeaderRoles     = "X-Telemed-Roles"
	HeaderRequestID = "X-Request-ID"
)

// trustedHeaders is the exact set a client must never be able to set. Keep
// this list and the constants above in lockstep; the forgery test iterates
// this slice.
var trustedHeaders = []string{HeaderUserID, HeaderRoles}

// StripForgedHeaders deletes every trusted internal header from the inbound
// request before anything else touches it -- before auth, before routing,
// before logging. This is the single most important security control in the
// gateway: without it, a client sets X-Telemed-User-ID to any UUID it likes
// and every downstream service that trusts the header treats the request as
// that user.
//
// It must be the outermost middleware on every mount that can reach a proxy
// handler. Order matters more here than anywhere else in the chain.
func StripForgedHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range trustedHeaders {
			r.Header.Del(h)
		}
		next.ServeHTTP(w, r)
	})
}

// hopByHopHeaders are connection-scoped per RFC 7230 §6.1 and must never be
// forwarded between hops. net/http/httputil.ReverseProxy already strips
// these internally; the explicit pass here is defence in depth and, more
// importantly, is what makes the behaviour independently testable rather
// than relying on stdlib internals nobody asserts against.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func stripHopByHopHeaders(h http.Header) {
	// RFC 7230 also allows the Connection header to name additional
	// per-hop headers to strip -- read it before the loop below deletes
	// "Connection" itself, or this list is always empty.
	conn := h.Get("Connection")

	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
	if conn != "" {
		for _, name := range strings.Split(conn, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
}

// injectTrustedHeaders stamps the verified principal (if any) onto the
// outbound request as the platform's trusted internal headers, and forwards
// the request id so logs correlate across every hop. Downstream services
// still verify the JWT independently -- defence in depth -- but they may
// trust these headers precisely because StripForgedHeaders guarantees no
// client-set copy survived to this point.
func injectTrustedHeaders(out, in *http.Request) {
	if p, ok := middleware.PrincipalFrom(in.Context()); ok {
		out.Header.Set(HeaderUserID, p.UserID.String())
		if len(p.Roles) > 0 {
			roles := make([]string, len(p.Roles))
			for i, r := range p.Roles {
				roles[i] = string(r)
			}
			out.Header.Set(HeaderRoles, strings.Join(roles, ","))
		}
	}

	reqID := chimw.GetReqID(in.Context())
	if reqID != "" {
		out.Header.Set(HeaderRequestID, reqID)
	}
}
