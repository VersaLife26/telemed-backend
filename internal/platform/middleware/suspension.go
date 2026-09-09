package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/logger"
)

// Suspension denylist: how a suspended account stops working NOW rather than
// in fifteen minutes.
//
// The platform's access tokens are RS256 JWTs verified offline against a JWKS.
// That is the whole point of them -- no network hop, no shared session store,
// no single point of failure between a patient and their doctor -- and it is
// also why an issued token cannot be un-issued. Suspension therefore needs
// three mechanisms, not one, and each covers what the others cannot:
//
//  1. REFRESH REFUSAL, in user-service. Refresh reads the account's current
//     status inside the rotation transaction and returns ErrUserSuspended
//     instead of a new token pair. This is the durable enforcement: Postgres
//     is the truth, it survives a Redis flush, and it cannot be bypassed.
//     What it does not do is stop the access token the client is already
//     holding, which stays valid for the remainder of its TTL.
//
//  2. SESSION REVOCATION, in user-service. Every refresh token for the user is
//     revoked in the same transaction as the status change, so no device can
//     rotate its way back in even if it presents a token issued a second
//     before the suspension.
//
//  3. THIS DENYLIST, which closes the remaining window. user-service writes
//     `auth:suspended:<user_id>` when it applies the suspension and deletes it
//     on reinstatement; the gateway -- the only way a patient or doctor client
//     reaches the platform -- checks it on every authenticated request and
//     returns ACCOUNT_SUSPENDED. Enforcement is immediate.
//
// Two properties of the design are deliberate and worth defending:
//
// The key EXPIRES rather than living forever. It only has to outlive the
// longest-lived access token issued before the suspension; after that every
// such token has expired on its own and mechanisms 1 and 2 hold the line
// permanently. A never-expiring key would make Redis a second, unbounded
// source of truth for account status, and the first time it disagreed with
// Postgres a reinstated user would stay locked out with nothing to point at.
//
// The check FAILS OPEN when Redis is unreachable. That is not laziness: a
// fail-closed check would turn a Redis blip into a total platform outage,
// locking every patient out of every consultation to enforce a state a handful
// of accounts are in. Failing open degrades suspension to "takes effect within
// the access-token TTL" -- exactly where the platform stood before this
// denylist existed -- while the durable mechanisms above are untouched. The
// degradation is logged at error level precisely because it is a security
// control degrading, not a routine miss.
const (
	// suspendedKeyPrefix namespaces the denylist inside the shared Redis.
	suspendedKeyPrefix = "auth:suspended:"

	// SuspensionDenylistTTL must exceed the platform's access-token lifetime
	// (user.AccessTokenTTL, 15 minutes) by enough margin to cover clock skew
	// between the issuer and the gateway. user-service has a test asserting
	// that relationship still holds, so shortening the token TTL is safe and
	// lengthening it past this value fails the build rather than silently
	// reopening the window.
	SuspensionDenylistTTL = 20 * time.Minute
)

// SuspendedKey is the denylist key for one user. Producer (user-service) and
// consumer (the gateway) both call it, so the two can never drift apart on a
// string literal -- which is the failure mode that would make this control
// silently do nothing.
func SuspendedKey(userID uuid.UUID) string { return suspendedKeyPrefix + userID.String() }

// SuspensionChecker is the slice of cache.Cache this middleware needs.
type SuspensionChecker interface {
	Exists(ctx context.Context, key string) (bool, error)
}

// MarkSuspended adds a user to the denylist. Called by user-service after it
// has committed the status change -- never before, so the denylist can only
// ever lag the truth, not lead it.
func MarkSuspended(ctx context.Context, c cache.Cache, userID uuid.UUID) error {
	return c.Set(ctx, SuspendedKey(userID), []byte("1"), SuspensionDenylistTTL)
}

// ClearSuspended removes a user from the denylist on reinstatement. Deleting a
// key that is not there is not an error: the TTL may simply have lapsed, and
// the end state the caller wants already holds.
func ClearSuspended(ctx context.Context, c cache.Cache, userID uuid.UUID) error {
	return c.Del(ctx, SuspendedKey(userID))
}

// RejectSuspended refuses a request from a suspended account with 403
// ACCOUNT_SUSPENDED. Wrap it INSIDE RequireAuth -- it reads the verified
// principal and does nothing without one, so an unauthenticated request is
// simply passed through to whatever rejects it next.
//
// A nil checker disables the middleware entirely, which is the correct
// behaviour for a deployment with no Redis: mechanisms 1 and 2 above still
// apply, so the account is still suspended, just not instantly.
func RejectSuspended(c SuspensionChecker, log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if c == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok || p.UserID == uuid.Nil {
				next.ServeHTTP(w, r)
				return
			}

			suspended, err := c.Exists(r.Context(), SuspendedKey(p.UserID))
			if err != nil {
				// Fail open. See the block comment above for why this is the
				// right trade and what still holds when it happens.
				log.Error().Err(err).
					Str("user_id", logger.MaskID(p.UserID.String())).
					Msg("suspension denylist unreachable; allowing the request -- " +
						"suspension now takes effect within the access-token TTL instead of immediately")
				next.ServeHTTP(w, r)
				return
			}
			if suspended {
				log.Warn().
					Str("user_id", logger.MaskID(p.UserID.String())).
					Str("path", r.URL.Path).
					Msg("rejected a request from a suspended account")
				httpx.Error(w, r, httpx.ErrAccountSuspended)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
