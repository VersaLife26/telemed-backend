package adminusers

import (
	"context"
	"errors"
	"strings"

	"github.com/rs/zerolog"
)

// ErrIdentityProviderUnavailable is returned by an IdentityProvider that
// could not reach whatever is on the other side of it.
//
// AccessProvider never returns it -- it makes no network call -- but it stays
// part of the contract: the handler turns it into a 503 rather than a 500,
// which is the right answer for any future provider that does call out, and
// removing it would mean rediscovering that mapping when one does.
var ErrIdentityProviderUnavailable = errors.New("adminusers: identity provider unavailable")

// IdentityProvider is the administrator identity provider's side of an admin
// account change: the parts that do not live in this service's database.
//
// It exists as an interface because what is on the other side of it has
// changed once already -- Keycloak, then Cloudflare Access -- and the service
// logic in service.go should not have to know which.
type IdentityProvider interface {
	// CreateAdmin provisions a login for email and returns its subject, the
	// stable identifier stored on the admin_users row.
	CreateAdmin(ctx context.Context, email, displayName, role string) (string, error)
	// SetRole records a role change with the provider.
	SetRole(ctx context.Context, subject, oldRole, newRole string) error
	// SetEnabled enables or disables the login itself.
	SetEnabled(ctx context.Context, subject string, enabled bool) error
	// RevokeSessions ends every active session the provider holds.
	RevokeSessions(ctx context.Context, subject string) error
}

// AccessProvider is the IdentityProvider for a deployment whose administrator
// identity provider is Cloudflare Access.
//
// Access is not a user directory this service can write to, and it does not
// need to be. Under Keycloak the realm held both halves of an admin account --
// the login and the realm_access role that authorised every route -- so every
// change here had to reach the realm to be real. Under Access the two halves
// are split:
//
//	authentication  Cloudflare Access, admitted by the Access policy's email
//	                list, which is deployment configuration (Ansible, from the
//	                vault) rather than something an API call from inside the
//	                product should be editing.
//	authorisation   the admin_users table in this service, resolved per request
//	                by Directory.RolesForSubject against the email in the Access
//	                token, with a cache TTL measured in seconds.
//
// The database is therefore authoritative for everything RequireRole reads,
// and the operations below that used to be load-bearing calls into a realm are
// records of a change this service has already made durably. They are no-ops
// rather than errors: the service layer's write is the change.
//
// The one thing Access genuinely cannot do from here is admit a new email --
// see CreateAdmin.
type AccessProvider struct{ log zerolog.Logger }

// NewAccessProvider builds the provider.
func NewAccessProvider(log zerolog.Logger) *AccessProvider {
	return &AccessProvider{log: log}
}

var _ IdentityProvider = (*AccessProvider)(nil)

// CreateAdmin returns the subject to store on the new admin_users row.
//
// The email is the subject. Directory.RolesForSubject matches the Access token
// on its email claim, not on a provider-assigned id, so the email is both
// stable and the thing actually used -- and admin_users.keycloak_subject is
// NOT NULL with a unique index, which a per-email value satisfies. (The column
// keeps its old name: renaming it is a migration on a live table for no
// behavioural gain.)
//
// IMPORTANT, and the one genuine gap left by removing Keycloak: this does NOT
// admit the person through Cloudflare Access. Creating the row makes them an
// administrator as far as this service is concerned, and they still cannot
// reach the console until their address is in the Access policy for the admin
// hostname -- vault_admin_access_emails, applied by playbook 04. It is logged
// at WARN for that reason, because a row for someone who cannot sign in is
// exactly the half-success the Keycloak implementation refused to create.
func (p *AccessProvider) CreateAdmin(_ context.Context, email, _, role string) (string, error) {
	subject := strings.TrimSpace(strings.ToLower(email))
	p.log.Warn().Str("email", subject).Str("role", role).
		Msg("admin row created; Cloudflare Access will NOT admit this address until it is added to the " +
			"admin application's policy (vault_admin_access_emails)")
	return subject, nil
}

// SetRole is a no-op: the role on the admin_users row is what RequireRole
// reads, and the service has already written it. Under Keycloak this call was
// what made a role change real rather than cosmetic; here it is the other way
// round.
func (p *AccessProvider) SetRole(_ context.Context, subject, oldRole, newRole string) error {
	p.log.Info().Str("subject", subject).Str("from", oldRole).Str("to", newRole).
		Msg("admin role changed")
	return nil
}

// SetEnabled is a no-op for the same reason. A deactivated admin resolves to
// no roles at the next Directory lookup, so they lose the console within the
// directory's cache TTL -- seconds -- without their Access session being
// touched. Access still recognises the person; this service no longer grants
// them anything.
func (p *AccessProvider) SetEnabled(_ context.Context, subject string, enabled bool) error {
	p.log.Info().Str("subject", subject).Bool("enabled", enabled).
		Msg("admin login enabled state changed")
	return nil
}

// RevokeSessions is a no-op. There is no session held here to end, and ending
// the Access session would not be what bounds the revocation anyway: the
// Directory's TTL is, because authorisation is re-read from the database
// rather than carried in the token.
func (p *AccessProvider) RevokeSessions(_ context.Context, subject string) error {
	p.log.Info().Str("subject", subject).
		Msg("admin deauthorised; the console is refused at the next role lookup")
	return nil
}
