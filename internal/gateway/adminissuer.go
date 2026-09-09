package gateway

import (
	"net/http"
	"slices"

	"github.com/rs/zerolog"

	"telemed/internal/platform/httpx"
	platmw "telemed/internal/platform/middleware"
)

// RequireTokenIssuer refuses a request whose verified token was minted by an
// issuer that is not on allowed.
//
// # Why this exists, given ADR-012 already landed
//
// ADR-012 bound each issuer to its own key set, which closed the forgery it
// was written about: user-service can no longer sign a token whose "iss"
// claims to be Keycloak, because the signature is checked against Keycloak's
// key set and fails.
//
// It did not close the other half. user-service can still mint a perfectly
// honest token -- "iss: telemed-user-service", signed with user-service's own
// key, verifying cleanly -- that carries realm_access.roles = ["super_admin"].
// RequireRole reads the role and waves it through, because nothing in
// Principal said which issuer produced it. The result is the same outcome
// ADR-010 exists to prevent (the patient issuer speaking for the admin
// surface), reached by a route the ADR-012 fix does not cover.
//
// That matters beyond the abstract: the admin surface is where SAML SSO and
// enforced 2FA live. A phone-OTP login that yields an admin-role token is a
// 2FA bypass, not merely a role escalation. Today the only reason it does not
// happen is that user-service always writes 'patient' into users.role -- but
// the CHECK constraint on that column accepts 'super_admin', and the token
// minter copies the column verbatim into the claim
// (telemed-user-service/internal/user/token.go:120). One row, or one future
// "promote to admin" feature, converts a latent gap into a live bypass.
//
// So: the admin surface additionally requires the token to come from the
// admin issuer. Roles say what you may do; the issuer says who is entitled to
// make that assertion at all.
//
// An empty allowed list means "not configured" and passes everything through,
// matching middleware.IPAllowlist's semantics so a developer stack with no
// Keycloak still works. Config.Validate refuses that combination in prod.
func RequireTokenIssuer(allowed []string, log zerolog.Logger) func(http.Handler) http.Handler {
	trusted := make([]string, 0, len(allowed))
	for _, iss := range allowed {
		if iss != "" && !slices.Contains(trusted, iss) {
			trusted = append(trusted, iss)
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(trusted) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			p, ok := platmw.PrincipalFrom(r.Context())
			if !ok {
				// Reached only if this middleware is composed outside
				// RequireAuth, which would be a wiring bug. Fail closed.
				httpx.Error(w, r, httpx.ErrUnauthorized)
				return
			}
			if !slices.Contains(trusted, p.Issuer) {
				log.Warn().
					Str("issuer", p.Issuer).
					Str("path", r.URL.Path).
					Msg("token from a non-admin issuer refused on the admin surface")
				httpx.Error(w, r, httpx.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
