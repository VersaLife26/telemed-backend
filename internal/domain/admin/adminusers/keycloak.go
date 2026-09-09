package adminusers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/rs/zerolog"
)

// ErrIdentityProviderUnavailable is returned when an operation needs Keycloak
// and Keycloak cannot be reached.
//
// admin-service does NOT degrade here, and that is a deliberate difference
// from user-service. There, Keycloak is a mirror of an account that already
// exists and works, so degrading is right: a patient must be able to log in by
// OTP during a Keycloak outage. Here Keycloak IS the admin identity provider
// and the source of the roles that authorize every admin route. Half-creating
// an admin -- a database row with no login, or a login with no role -- leaves
// an account whose privileges nobody can read off either system. Refusing is
// the safe answer.
var ErrIdentityProviderUnavailable = errors.New("adminusers: identity provider unavailable")

// IdentityProvider is the slice of Keycloak this package needs. It is an
// interface so the service layer can be tested without a live realm, and so
// the "no credentials configured" case is a type rather than a nil check
// scattered through the callers.
type IdentityProvider interface {
	// CreateAdmin provisions a login for email and returns its subject.
	// It never sets a password: see the required actions in the implementation.
	CreateAdmin(ctx context.Context, email, displayName, role string) (string, error)
	// SetRole makes role the admin's ONLY realm role, removing the previous
	// one. Roles are what RequireRole reads, so this is the call that makes a
	// role change real rather than cosmetic.
	SetRole(ctx context.Context, subject, oldRole, newRole string) error
	// SetEnabled disables the login itself, so a deactivated admin cannot
	// obtain a fresh token once their current one expires.
	SetEnabled(ctx context.Context, subject string, enabled bool) error
	// RevokeSessions ends every active session, so a demotion or a
	// deactivation takes effect in seconds rather than at token expiry.
	RevokeSessions(ctx context.Context, subject string) error
}

// GocloakProvider implements IdentityProvider against a live realm.
type GocloakProvider struct {
	client *gocloak.GoCloak
	cfg    KeycloakConfig
}

// KeycloakConfig is the admin connection used to manage admin logins.
type KeycloakConfig struct {
	BaseURL      string
	Realm        string
	ClientID     string
	ClientSecret string
}

var _ IdentityProvider = (*GocloakProvider)(nil)

// NewGocloakProvider builds the client and proves it can obtain an admin
// token, so a misconfiguration surfaces at boot rather than the first time a
// super_admin tries to create a colleague.
func NewGocloakProvider(ctx context.Context, cfg KeycloakConfig) (*GocloakProvider, error) {
	if cfg.BaseURL == "" || cfg.Realm == "" || cfg.ClientID == "" {
		return nil, fmt.Errorf("adminusers: incomplete keycloak configuration")
	}
	client := gocloak.NewClient(cfg.BaseURL)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := client.LoginClient(pingCtx, cfg.ClientID, cfg.ClientSecret, cfg.Realm); err != nil {
		return nil, fmt.Errorf("adminusers: keycloak admin login: %w", err)
	}
	return &GocloakProvider{client: client, cfg: cfg}, nil
}

func (g *GocloakProvider) token(ctx context.Context) (string, error) {
	jwt, err := g.client.LoginClient(ctx, g.cfg.ClientID, g.cfg.ClientSecret, g.cfg.Realm)
	if err != nil {
		// Both verbs are %w: callers match on the sentinel with errors.Is, and
		// the underlying Keycloak error stays in the chain for the operator.
		return "", fmt.Errorf("%w: %w", ErrIdentityProviderUnavailable, err)
	}
	return jwt.AccessToken, nil
}

// CreateAdmin provisions the login. It sets NO password.
//
// The three required actions are the whole security design of this function:
//
//   - UPDATE_PASSWORD: the new admin chooses their own credential on first
//     login. This platform therefore never generates, stores, transmits or
//     logs an admin password, and there is no temporary password sitting in an
//     inbox waiting to be found.
//   - CONFIGURE_TOTP: admin-web's 2FA gate refuses any session whose token
//     does not evidence a second factor, so without this the account would be
//     created unable to log in at all. Enrolment is forced at first login
//     rather than left to a policy someone has to remember to apply.
//   - VERIFY_EMAIL: the address is how the account is recovered, so it has to
//     be proven to belong to the person before it can be used to reset a
//     credential that reads patient data.
//
// Enabled is true because the required actions gate the login by themselves --
// a disabled account with pending actions cannot complete them.
func (g *GocloakProvider) CreateAdmin(ctx context.Context, email, displayName, role string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	token, err := g.token(callCtx)
	if err != nil {
		return "", err
	}

	user := gocloak.User{
		Username:      gocloak.StringP(email),
		Email:         gocloak.StringP(email),
		FirstName:     gocloak.StringP(displayName),
		Enabled:       gocloak.BoolP(true),
		EmailVerified: gocloak.BoolP(false),
		RequiredActions: &[]string{
			"UPDATE_PASSWORD",
			"CONFIGURE_TOTP",
			"VERIFY_EMAIL",
		},
	}

	subject, err := g.client.CreateUser(callCtx, token, g.cfg.Realm, user)
	if err != nil {
		return "", fmt.Errorf("adminusers: keycloak create user: %w", err)
	}

	if err := g.assign(callCtx, token, subject, role); err != nil {
		// Roll the login back. An account that can authenticate but carries no
		// role is not harmless: it is an authenticated principal on the admin
		// issuer, and the next person to look at the realm sees a login that
		// appears provisioned.
		if delErr := g.client.DeleteUser(callCtx, token, g.cfg.Realm, subject); delErr != nil {
			return "", fmt.Errorf("adminusers: assign role: %w (and rollback failed: %v; "+
				"keycloak subject %s must be removed by hand)", err, delErr, subject)
		}
		return "", err
	}
	return subject, nil
}

func (g *GocloakProvider) assign(ctx context.Context, token, subject, role string) error {
	r, err := g.client.GetRealmRole(ctx, token, g.cfg.Realm, role)
	if err != nil {
		return fmt.Errorf("adminusers: realm role %q: %w", role, err)
	}
	if err := g.client.AddRealmRoleToUser(ctx, token, g.cfg.Realm, subject, []gocloak.Role{*r}); err != nil {
		return fmt.Errorf("adminusers: add realm role %q: %w", role, err)
	}
	return nil
}

// SetRole adds the new role before removing the old one. The order matters:
// if the process dies between the two calls, the admin briefly holds both
// roles rather than neither, and an admin who can still work is a better
// failure than one locked out of an incident they were called in to handle.
// The reverse order would also make a demotion look successful while leaving
// the account with no access at all.
func (g *GocloakProvider) SetRole(ctx context.Context, subject, oldRole, newRole string) error {
	if oldRole == newRole {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	token, err := g.token(callCtx)
	if err != nil {
		return err
	}
	if err := g.assign(callCtx, token, subject, newRole); err != nil {
		return err
	}
	if oldRole == "" {
		return nil
	}
	old, err := g.client.GetRealmRole(callCtx, token, g.cfg.Realm, oldRole)
	if err != nil {
		return fmt.Errorf("adminusers: realm role %q: %w", oldRole, err)
	}
	if err := g.client.DeleteRealmRoleFromUser(callCtx, token, g.cfg.Realm, subject, []gocloak.Role{*old}); err != nil {
		return fmt.Errorf("adminusers: remove realm role %q: %w", oldRole, err)
	}
	return nil
}

func (g *GocloakProvider) SetEnabled(ctx context.Context, subject string, enabled bool) error {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	token, err := g.token(callCtx)
	if err != nil {
		return err
	}
	if err := g.client.UpdateUser(callCtx, token, g.cfg.Realm, gocloak.User{
		ID: gocloak.StringP(subject), Enabled: gocloak.BoolP(enabled),
	}); err != nil {
		return fmt.Errorf("adminusers: keycloak set enabled=%v: %w", enabled, err)
	}
	return nil
}

// RevokeSessions is what makes a demotion or deactivation immediate. Without
// it the change lands only when the current access token expires, and the
// window between "the console says this person is now support" and "they can
// still move money" is the access-token TTL.
func (g *GocloakProvider) RevokeSessions(ctx context.Context, subject string) error {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	token, err := g.token(callCtx)
	if err != nil {
		return err
	}
	if err := g.client.LogoutAllSessions(callCtx, token, g.cfg.Realm, subject); err != nil {
		return fmt.Errorf("adminusers: keycloak logout sessions: %w", err)
	}
	return nil
}

// UnavailableProvider stands in when no Keycloak credentials are configured.
// Every call fails with ErrIdentityProviderUnavailable, so a deployment that
// forgot to configure the admin client cannot silently create database rows
// for admins who have no login -- it refuses, loudly, at the first attempt.
type UnavailableProvider struct{ log zerolog.Logger }

func NewUnavailableProvider(log zerolog.Logger) *UnavailableProvider {
	log.Warn().Msg("adminusers: no keycloak admin credentials configured; " +
		"creating and re-roling admin accounts is disabled")
	return &UnavailableProvider{log: log}
}

var _ IdentityProvider = (*UnavailableProvider)(nil)

func (u *UnavailableProvider) CreateAdmin(context.Context, string, string, string) (string, error) {
	return "", ErrIdentityProviderUnavailable
}
func (u *UnavailableProvider) SetRole(context.Context, string, string, string) error {
	return ErrIdentityProviderUnavailable
}
func (u *UnavailableProvider) SetEnabled(context.Context, string, bool) error {
	return ErrIdentityProviderUnavailable
}
func (u *UnavailableProvider) RevokeSessions(context.Context, string) error {
	return ErrIdentityProviderUnavailable
}
