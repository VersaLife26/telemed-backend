// Package middleware holds the cross-cutting HTTP concerns every telemed
// service applies: authentication, authorization, rate limiting, PHI-safe
// request logging, and security headers.
package middleware

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"telemed/internal/platform/httpx"
)

// Role is a platform authorization role. Roles come from the Keycloak token;
// the service never trusts a role supplied in a request body or header.
type Role string

const (
	RolePatient    Role = "patient"
	RoleDoctor     Role = "doctor"
	RoleAdmin      Role = "admin"
	RoleSuperAdmin Role = "super_admin"
	RoleOps        Role = "ops"
	RoleFinance    Role = "finance"
	RoleSupport    Role = "support"
	RoleService    Role = "service" // machine-to-machine, internal mesh only
)

// AdminRoles is every role permitted on the admin surface.
var AdminRoles = []Role{RoleAdmin, RoleSuperAdmin, RoleOps, RoleFinance, RoleSupport}

type principalKey struct{}

// Principal is the authenticated caller. It is derived only from a verified
// token signature.
type Principal struct {
	UserID   uuid.UUID
	Subject  string
	Roles    []Role
	Phone    string
	Email    string
	Language string
	// DoctorID is populated for doctor tokens so a handler can authorize
	// "this doctor may only touch their own slots" without an extra lookup.
	DoctorID  uuid.UUID
	ExpiresAt time.Time
	// Issuer is the verified "iss" claim: which of the platform's two token
	// issuers actually minted this token. It is only ever set by Verify,
	// after keyForToken has required the signature to check out against the
	// key set BOUND to that exact issuer -- so by the time it is readable it
	// is a fact, not a claim.
	//
	// It exists because binding an issuer to its keys (ADR-012) stops
	// user-service forging "iss: keycloak"; it does NOT stop user-service
	// asserting an admin ROLE under its own honest issuer. Nothing downstream
	// could tell those apart without this field. See
	// gateway.RequireTokenIssuer.
	Issuer string
}

// HasRole reports whether the principal carries the given role.
func (p Principal) HasRole(r Role) bool { return slices.Contains(p.Roles, r) }

// HasAnyRole reports whether the principal carries at least one of the roles.
func (p Principal) HasAnyRole(roles ...Role) bool {
	for _, r := range roles {
		if p.HasRole(r) {
			return true
		}
	}
	return false
}

// WithPrincipal returns a context carrying an authenticated principal.
//
// Production code does not call this -- RequireAuth does, after verifying a
// signature. It is exported for two legitimate callers: tests that need an
// authenticated context, and the API gateway, which verifies once at the edge
// and needs to attach the result before proxying.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom extracts the authenticated caller from the context.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// MustPrincipal returns the caller, panicking when the route was not wrapped in
// RequireAuth. The panic is a programming error caught by the Recoverer, not a
// runtime condition a client can trigger.
func MustPrincipal(ctx context.Context) Principal {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		panic("middleware: route requires RequireAuth but no principal in context")
	}
	return p
}

// Authenticator verifies bearer tokens against one or more JWKS endpoints.
// Keys are fetched and refreshed in the background, so a key rotation does not
// require a service restart.
//
// The platform has TWO legitimate token issuers, and every service must accept
// both:
//
//   - telemed-user-service issues patient and doctor tokens. Phone-OTP login
//     cannot depend on Keycloak being reachable -- a patient trying to reach a
//     doctor must not be blocked by an identity-provider outage -- so
//     user-service signs its own RS256 tokens and publishes them at
//     /.well-known/jwks.json.
//   - Keycloak issues admin tokens, where SAML SSO and enforced 2FA are worth
//     the hard dependency, and where an outage means ten staff wait rather than
//     ten thousand patients.
//
// Accepting a token means its signature verifies against the key set BOUND TO
// the issuer it claims, AND that issuer is one we expect.
//
// The binding is the load-bearing part, and it is easy to get wrong. Merging
// every JWKS into one key set and then checking "iss" against an allowlist
// looks equivalent and is not: the merged set matches purely on "kid", so
// user-service's key would happily sign a token whose "iss" says Keycloak,
// and the allowlist -- reading a claim the signer controls -- would wave it
// through. That gives the patient issuer the power to mint admin tokens,
// which is the exact outcome ADR-010 says two issuers must never allow. Pass
// IssuerKeys and each issuer can only ever speak for itself.
type Authenticator struct {
	// byIssuer maps a trusted issuer to the one key set permitted to sign for
	// it. Populated from AuthConfig.IssuerKeys.
	byIssuer map[string]keyfunc.Keyfunc
	issuers  []string
	audience string

	// roles resolves the roles of a principal whose token carries none.
	//
	// An external identity provider authenticates a person; it does not know
	// what this platform lets them do. Cloudflare Access, in particular, mints
	// a token with a verified "sub" and "email" and no roles at all -- so
	// without this the admin surface would authenticate an administrator
	// perfectly and then 403 every request.
	//
	// Nil disables the lookup, which is correct for every issuer that does
	// carry roles. See RequireAuth for why it runs there and not in Verify.
	roles RoleResolver
}

// RoleResolver maps a verified external identity onto this platform's roles.
//
// The token is proof of WHO; this is the answer to WHAT THEY MAY DO, and the
// two deliberately come from different places. Roles live in the admin_users
// table so that removing an administrator is a change an operator makes here,
// not one that has to be made in an identity provider and then waited for.
type RoleResolver interface {
	// RolesForSubject returns the roles held by the given verified identity.
	// An unknown subject is not an error: it is a person who authenticated
	// successfully and holds no roles on this platform, and must receive a
	// 403 rather than a 500.
	RolesForSubject(ctx context.Context, issuer, subject, email string) ([]Role, error)
}

// WithRoleResolver attaches a resolver. It is set by the composer rather than
// passed to New because the domain that owns the roles is built after the
// authenticator every other domain shares.
func (a *Authenticator) WithRoleResolver(r RoleResolver) *Authenticator {
	a.roles = r
	return a
}

// ResolvesRoles reports whether a resolver is attached.
func (a *Authenticator) ResolvesRoles() bool { return a.roles != nil }

// AuthConfig configures the authenticator.
//
// There is deliberately no "just give me a list of JWKS URLs" option. Merging
// key sets and checking the "iss" claim afterwards does NOT bind an issuer to
// its keys: JWKS matching is by "kid" alone and is issuer-agnostic, while "iss"
// is chosen by whoever signs. On this platform that would let user-service --
// which signs patient tokens -- mint a token claiming Keycloak's issuer and
// have it accepted as an admin. Binding is the only safe shape, so it is the
// only shape offered.
type AuthConfig struct {
	// IssuerKeys maps each trusted issuer to the ONE key set permitted to sign
	// for it. Required.
	IssuerKeys map[string]string
	// Issuers is the allowlist of acceptable "iss" claims. Empty means every
	// issuer in IssuerKeys is accepted, which is the usual case.
	Issuers []string
	// Audience, when set, must appear in the token's "aud" claim.
	Audience string
}

// NewAuthenticatorFrom builds an authenticator from an explicit config.
func NewAuthenticatorFrom(ctx context.Context, cfg AuthConfig) (*Authenticator, error) {
	issuers := make([]string, 0, len(cfg.Issuers)+len(cfg.IssuerKeys))
	for _, i := range cfg.Issuers {
		if i = strings.TrimSpace(i); i != "" && !slices.Contains(issuers, i) {
			issuers = append(issuers, i)
		}
	}

	// One key set per issuer, fetched separately. Separately is the point: a
	// key that only ever appears under user-service's JWKS can then only ever
	// verify a token that says it came from user-service.
	byIssuer := make(map[string]keyfunc.Keyfunc, len(cfg.IssuerKeys))
	bound := make(map[string]bool, len(cfg.IssuerKeys))
	for iss, u := range cfg.IssuerKeys {
		iss, u = strings.TrimSpace(iss), strings.TrimSpace(u)
		if iss == "" || u == "" {
			continue
		}
		kf, err := keyfunc.NewDefaultCtx(ctx, []string{u})
		if err != nil {
			return nil, fmt.Errorf("middleware: load jwks for issuer %q from %s: %w", iss, u, err)
		}
		byIssuer[iss] = kf
		bound[u] = true
		if !slices.Contains(issuers, iss) {
			issuers = append(issuers, iss)
		}
	}

	if len(byIssuer) == 0 {
		return nil, fmt.Errorf("middleware: AuthConfig.IssuerKeys is required; " +
			"set USER_ISSUER+USER_JWKS_URL and/or KEYCLOAK_ISSUER+KEYCLOAK_JWKS_URL")
	}

	return &Authenticator{byIssuer: byIssuer, issuers: issuers, audience: cfg.Audience}, nil
}

// keyForToken picks the key set allowed to verify tok, based on the issuer the
// token claims. The claim is untrusted at this point -- that is precisely why
// it selects the key set rather than being compared to a list afterwards. A
// token claiming an issuer it cannot produce a signature for simply fails.
func (a *Authenticator) keyForToken(tok *jwt.Token) (any, error) {
	iss, err := tok.Claims.GetIssuer()
	if err != nil {
		return nil, fmt.Errorf("middleware: token has no readable issuer: %w", err)
	}
	kf, ok := a.byIssuer[iss]
	if !ok {
		return nil, fmt.Errorf("middleware: no key set configured for issuer %q", iss)
	}
	return kf.Keyfunc(tok)
}

// NewSingleIssuerAuthenticator binds one issuer to one key set. It exists for
// tests and for a genuinely single-issuer deployment. Note that it still BINDS:
// there is no path in this package that verifies a token against a key set not
// tied to the issuer the token claims.
func NewSingleIssuerAuthenticator(ctx context.Context, jwksURL, issuer, audience string) (*Authenticator, error) {
	return NewAuthenticatorFrom(ctx, AuthConfig{
		IssuerKeys: map[string]string{issuer: jwksURL},
		Issuers:    []string{issuer},
		Audience:   audience,
	})
}

// claims covers every role-claim shape the platform's two issuers can produce.
//
// Keycloak's built-in realm-roles mapper nests roles under "realm_access", but
// a hand-written mapper commonly emits a flat "roles" array instead -- and our
// own realm export does exactly that. Reading only realm_access meant a genuine
// super_admin arrived with zero roles and every admin endpoint returned 403,
// with nothing in the logs to say why.
//
// Accepting both shapes is the robust choice: it costs one extra field and
// removes an entire class of silent lockout when a realm is re-exported or a
// mapper is reconfigured.
type claims struct {
	jwt.RegisteredClaims
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
	FlatRoles         []string `json:"roles"`
	PreferredUsername string   `json:"preferred_username"`
	PhoneNumber       string   `json:"phone_number"`
	Email             string   `json:"email"`
	Locale            string   `json:"locale"`
	UserID            string   `json:"telemed_user_id"`
	DoctorID          string   `json:"telemed_doctor_id"`
}

// Verify parses and validates a raw bearer token.
func (a *Authenticator) Verify(raw string) (Principal, error) {
	var c claims
	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "RS512", "ES256"}),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30 * time.Second), // tolerate modest clock skew between nodes
	}
	if a.audience != "" {
		parserOpts = append(parserOpts, jwt.WithAudience(a.audience))
	}

	tok, err := jwt.ParseWithClaims(raw, &c, a.keyForToken, parserOpts...)
	if err != nil {
		return Principal{}, fmt.Errorf("middleware: verify token: %w", err)
	}
	if !tok.Valid {
		return Principal{}, fmt.Errorf("middleware: token rejected")
	}

	// The issuer allowlist is checked here rather than via jwt.WithIssuer,
	// which accepts exactly one value. This is the second of two checks, not
	// the only one: keyForToken has already required the signature to verify
	// against the key set bound to this exact issuer. The allowlist alone
	// would not be enough, because "iss" is a claim the signer chooses.
	if len(a.issuers) > 0 && !slices.Contains(a.issuers, c.Issuer) {
		return Principal{}, fmt.Errorf("middleware: untrusted token issuer %q", c.Issuer)
	}

	p := Principal{
		Subject:  c.Subject,
		Phone:    c.PhoneNumber,
		Email:    c.Email,
		Language: c.Locale,
		Issuer:   c.Issuer,
	}
	if c.ExpiresAt != nil {
		p.ExpiresAt = c.ExpiresAt.Time
	}
	// telemed_user_id is a Keycloak protocol-mapper claim carrying our own
	// users.id. We fall back to sub so a realm without the mapper still works.
	if id, err := uuid.Parse(c.UserID); err == nil {
		p.UserID = id
	} else if id, err := uuid.Parse(c.Subject); err == nil {
		p.UserID = id
	}
	if id, err := uuid.Parse(c.DoctorID); err == nil {
		p.DoctorID = id
	}
	p.Roles = collectRoles(c)
	return p, nil
}

// RequireAuth rejects requests without a valid bearer token.
func RequireAuth(a *Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := bearerToken(r)
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			p, err := a.Verify(raw)
			if err != nil {
				httpx.Error(w, r, httpx.ErrUnauthorized.WithCause(err))
				return
			}

			// Role resolution happens HERE and not inside Verify, for two
			// reasons: Verify is a pure function over a string that many
			// tests call directly, and a lookup needs the request context so
			// a slow directory cannot outlive the request that triggered it.
			//
			// Only tokens that arrive with no roles are resolved. A token
			// that carries its own roles is authoritative for them, and
			// re-resolving would let a directory silently widen what a
			// user-service token already asserted.
			if a.roles != nil && len(p.Roles) == 0 {
				resolved, err := a.roles.RolesForSubject(r.Context(), p.Issuer, p.Subject, p.Email)
				if err != nil {
					httpx.Error(w, r, httpx.ErrInternal.WithCause(err))
					return
				}
				p.Roles = resolved
			}

			ctx := WithPrincipal(r.Context(), p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// OptionalAuth attaches a principal when a valid token is present but never
// rejects. Used on endpoints that personalize for signed-in users and still
// serve anonymous ones, such as doctor search.
func OptionalAuth(a *Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if raw, err := bearerToken(r); err == nil {
				if p, err := a.Verify(raw); err == nil {
					r = r.WithContext(WithPrincipal(r.Context(), p))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRole rejects callers lacking every listed role.
func RequireRole(roles ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				httpx.Error(w, r, httpx.ErrUnauthorized)
				return
			}
			if !p.HasAnyRole(roles...) {
				httpx.Error(w, r, httpx.ErrForbidden)
				return
			}
			// An admin role is only honoured from the admin issuer.
			//
			// ADR-012 bound each issuer to its own key set, which stops
			// user-service signing a token that CLAIMS to be Keycloak. It does
			// not stop user-service minting an honest token, under its own
			// issuer, carrying realm_access.roles = ["super_admin"] -- and
			// without this check RequireRole would wave that straight through.
			//
			// The admin surface is where SAML SSO and enforced 2FA live, so a
			// phone-OTP login yielding an admin token is a 2FA bypass, not just
			// a role escalation. The gateway enforces this at the edge; it is
			// repeated here because a service reachable inside the mesh -- via
			// a compromised pod or a NetworkPolicy gap -- must not be the soft
			// underbelly. Defence in depth means the check lives where the
			// decision is made, not only where traffic enters.
			if err := p.checkAdminIssuer(); err != nil {
				httpx.Error(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// adminIssuer is the one issuer permitted to assert an administrative role.
// Set once at boot by SetAdminIssuer; empty disables the check, which is only
// correct in tests and in a single-issuer deployment.
var adminIssuer string

// SetAdminIssuer records which issuer may speak for the admin surface. Call it
// during boot, from the same config that builds the Authenticator.
func SetAdminIssuer(iss string) { adminIssuer = iss }

// IsAdmin reports whether the caller holds an administrative role that this
// service is willing to honour.
//
// Every business-logic check that reads an admin role must go through this
// rather than through HasAnyRole(AdminRoles...) directly. RequireRole applies
// the issuer binding, but a service that authorises inside its service layer
// -- "an admin may also read this consultation", "an admin may also delete
// this document" -- never passes through RequireRole at all, and a bare
// HasAnyRole there re-opens exactly the hole checkAdminIssuer exists to close:
// user-service can mint an honest token, under its own issuer, asserting
// super_admin.
//
// When SetAdminIssuer has not been called this is identical to
// HasAnyRole(AdminRoles...), so it is safe to adopt everywhere.
func (p Principal) IsAdmin() bool { return p.HasAdminRole(AdminRoles...) }

// HasAdminRole is IsAdmin narrowed to a subset of the admin roles, for the
// call sites that authorise "finance or super_admin" rather than "any admin".
// It applies the same issuer binding.
func (p Principal) HasAdminRole(roles ...Role) bool {
	if !p.HasAnyRole(roles...) {
		return false
	}
	return p.checkAdminIssuer() == nil
}

// checkAdminIssuer refuses an administrative role that arrived from anywhere
// but the admin issuer.
func (p Principal) checkAdminIssuer() error {
	if adminIssuer == "" {
		return nil
	}
	if !p.HasAnyRole(AdminRoles...) {
		return nil
	}
	if p.Issuer == adminIssuer {
		return nil
	}
	return httpx.NewError(http.StatusForbidden, httpx.CodeForbidden,
		"administrative roles are not accepted from this token issuer")
}

func bearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", httpx.ErrUnauthorized
	}
	scheme, token, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", httpx.NewError(http.StatusUnauthorized, httpx.CodeUnauthorized,
			"Authorization header must be in 'Bearer <token>' form")
	}
	return token, nil
}

// collectRoles merges every role-claim shape, de-duplicating and dropping
// Keycloak's internal roles, which are noise to this platform's RBAC.
func collectRoles(c claims) []Role {
	seen := make(map[string]struct{}, 8)
	var out []Role

	add := func(names []string) {
		for _, n := range names {
			switch n {
			case "", "offline_access", "uma_authorization":
				continue
			}
			if strings.HasPrefix(n, "default-roles-") {
				continue
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, Role(n))
		}
	}

	add(c.RealmAccess.Roles)
	add(c.FlatRoles)
	return out
}
