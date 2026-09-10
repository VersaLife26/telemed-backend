package adminusers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	mw "telemed/internal/platform/middleware"
)

// Directory answers "what may this verified person do on this platform".
//
// It exists because the platform's administrator identity now comes from an
// external provider -- Cloudflare Access -- whose token proves WHO someone is
// and says nothing about what they may do. Access authenticates; admin_users
// authorizes. Keeping the two apart is what lets an operator revoke an
// administrator here, immediately, without waiting on a change in an identity
// provider they may not control.
//
// It satisfies mw.RoleResolver.
type Directory struct {
	repo *Repository
	log  zerolog.Logger

	// issuer is the ONE issuer this directory will answer for.
	//
	// Without it, any token that happened to arrive with no roles would have
	// roles attached from this table by email -- including a user-service
	// token for a patient whose email matches an administrator's. Binding the
	// lookup to the admin issuer keeps the property ADR-012 is about: which
	// issuer minted a token decides what it can become.
	issuer string

	mu    sync.Mutex
	cache map[string]cachedRoles
	ttl   time.Duration
	now   func() time.Time
}

type cachedRoles struct {
	roles []mw.Role
	until time.Time
}

// NewDirectory builds the resolver. issuer must be the admin token issuer;
// an empty issuer disables the directory entirely, because a resolver that
// answers for every issuer is worse than none.
func NewDirectory(repo *Repository, issuer string, ttl time.Duration, log zerolog.Logger) *Directory {
	if ttl <= 0 {
		// Short on purpose. This is the window during which a revoked
		// administrator still has their role, so it is measured in seconds
		// rather than minutes.
		ttl = 30 * time.Second
	}
	return &Directory{
		repo: repo, log: log, issuer: strings.TrimSpace(issuer),
		cache: map[string]cachedRoles{}, ttl: ttl, now: time.Now,
	}
}

var _ mw.RoleResolver = (*Directory)(nil)

// RolesForSubject returns the roles held by a verified identity.
//
// An unknown or deactivated account resolves to no roles, not an error: that
// person authenticated successfully and simply holds nothing here, which is a
// 403 from RequireRole rather than a 500 from this lookup. A database failure
// IS an error -- failing open on a role lookup for the admin surface would
// hand an authenticated stranger whatever the caller's default is.
func (d *Directory) RolesForSubject(ctx context.Context, issuer, _, email string) ([]mw.Role, error) {
	if d.issuer == "" || issuer != d.issuer {
		return nil, nil
	}
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		// Access always sends one. A token without it is not something to
		// guess about.
		return nil, nil
	}

	if roles, ok := d.cached(email); ok {
		return roles, nil
	}

	u, err := d.repo.GetByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		d.store(email, nil)
		return nil, nil
	case err != nil:
		return nil, err
	}

	if !u.Active {
		d.log.Warn().Str("admin_id", u.ID.String()).
			Msg("adminusers: deactivated account authenticated; resolving to no roles")
		d.store(email, nil)
		return nil, nil
	}

	roles := []mw.Role{mw.Role(u.Role)}
	d.store(email, roles)
	return roles, nil
}

func (d *Directory) cached(email string) ([]mw.Role, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.cache[email]
	if !ok || d.now().After(e.until) {
		return nil, false
	}
	return e.roles, true
}

func (d *Directory) store(email string, roles []mw.Role) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// The admin population is small and bounded by the admin_users table, so
	// this map cannot grow without an operator creating accounts. No eviction
	// beyond expiry is needed, but a cap keeps a pathological case bounded.
	if len(d.cache) > 1024 {
		d.cache = map[string]cachedRoles{}
	}
	d.cache[email] = cachedRoles{roles: roles, until: d.now().Add(d.ttl)}
}
