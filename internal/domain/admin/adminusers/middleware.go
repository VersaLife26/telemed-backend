package adminusers

import (
	"context"
	"net/http"

	"github.com/rs/zerolog"

	"telemed/internal/platform/httpx"
	mw "telemed/internal/platform/middleware"
)

type ctxKey struct{}

// FromContext retrieves the resolved local admin account attached by
// RequireActiveAdminUser.
func FromContext(ctx context.Context) (AdminUser, bool) {
	u, ok := ctx.Value(ctxKey{}).(AdminUser)
	return u, ok
}

// RequireActiveAdminUser resolves the JWT principal to a local admin_users
// row (creating it on first login), rejects a deactivated account, and
// enforces that admin's per-user IP scope if one is configured. Mount it
// once for the whole /api/v1/admin subtree, after RequireAuth and the
// service-wide IPAllowlist and before the per-group RequireRole calls: role
// authorization still comes from the JWT, this only adds the two checks a
// JWT cannot express.
func RequireActiveAdminUser(svc *Service, log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := mw.PrincipalFrom(r.Context())
			if !ok {
				httpx.Error(w, r, httpx.ErrUnauthorized)
				return
			}

			au, err := svc.Ensure(r.Context(), p)
			if err != nil {
				log.Error().Err(err).Msg("adminusers: failed to resolve admin account")
				httpx.Error(w, r, httpx.ErrInternal.WithCause(err))
				return
			}

			if !au.Active {
				log.Warn().Str("admin_user_id", au.ID.String()).Msg("deactivated admin account attempted access")
				httpx.Error(w, r, httpx.ErrForbidden)
				return
			}

			ip := mw.ClientIP(r)
			allowed, err := ipInAllowlist(ip, au.IPAllowlist)
			if err != nil {
				log.Error().Err(err).Msg("adminusers: ip scope check failed")
				httpx.Error(w, r, httpx.ErrInternal.WithCause(err))
				return
			}
			if !allowed {
				log.Warn().Str("admin_user_id", au.ID.String()).Str("ip", ip).
					Msg("admin request blocked by per-admin IP scope")
				httpx.Error(w, r, httpx.ErrForbidden)
				return
			}

			ctx := context.WithValue(r.Context(), ctxKey{}, au)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
