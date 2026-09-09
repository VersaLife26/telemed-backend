package user

import (
	"context"
	"fmt"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/rs/zerolog"
)

// GocloakConfig configures the admin connection used to mirror a user into
// Keycloak on registration.
type GocloakConfig struct {
	BaseURL      string
	Realm        string
	ClientID     string
	ClientSecret string
}

// GocloakClient implements KeycloakClient against a live Keycloak realm.
type GocloakClient struct {
	client *gocloak.GoCloak
	cfg    GocloakConfig
}

var _ KeycloakClient = (*GocloakClient)(nil)

// NewGocloakClient builds the client and verifies it can obtain an admin
// token. It is called once at boot; the caller (cmd/server/main.go) treats a
// non-nil error as "Keycloak is unreachable right now" and falls back to
// DegradedKeycloakClient rather than failing startup -- an OTP-login health
// app must not crash-loop because an identity mirror is down.
func NewGocloakClient(ctx context.Context, cfg GocloakConfig) (*GocloakClient, error) {
	client := gocloak.NewClient(cfg.BaseURL)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := client.LoginClient(pingCtx, cfg.ClientID, cfg.ClientSecret, cfg.Realm); err != nil {
		return nil, fmt.Errorf("user: keycloak admin login: %w", err)
	}
	return &GocloakClient{client: client, cfg: cfg}, nil
}

// CreateUser provisions a mirrored Keycloak identity carrying the
// telemed_user_id attribute, so a future SSO or WebAuthn passkey login (SDD
// section 12) resolves back to this user's row without a second lookup
// table.
func (g *GocloakClient) CreateUser(ctx context.Context, u User) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	token, err := g.client.LoginClient(callCtx, g.cfg.ClientID, g.cfg.ClientSecret, g.cfg.Realm)
	if err != nil {
		return "", fmt.Errorf("user: keycloak admin token: %w", err)
	}

	enabled := u.Status == StatusActive
	kcUser := gocloak.User{
		Username: gocloak.StringP(preferredUsername(u)),
		Enabled:  gocloak.BoolP(enabled),
		Attributes: &map[string][]string{
			"telemed_user_id": {u.ID.String()},
			"phone":           {u.Phone},
		},
	}
	if u.Email != nil {
		kcUser.Email = u.Email
	}
	if u.Name != "" {
		kcUser.FirstName = gocloak.StringP(u.Name)
	}

	id, err := g.client.CreateUser(callCtx, token.AccessToken, g.cfg.Realm, kcUser)
	if err != nil {
		return "", fmt.Errorf("user: keycloak create user: %w", err)
	}
	return id, nil
}

// DegradedKeycloakClient is used when Keycloak was unreachable at boot, or
// when no admin credentials are configured (e.g. local development). Every
// call fails fast with a clear error that the service layer logs and
// swallows -- registration and login proceed on this service's own JWTs
// regardless, per AGENT-BRIEF: "the service must also work when Keycloak is
// unreachable at boot -- degrade, do not crash-loop."
type DegradedKeycloakClient struct {
	log zerolog.Logger
}

var _ KeycloakClient = (*DegradedKeycloakClient)(nil)

// NewDegradedKeycloakClient builds the no-op fallback.
func NewDegradedKeycloakClient(log zerolog.Logger) *DegradedKeycloakClient {
	return &DegradedKeycloakClient{log: log}
}

func (d *DegradedKeycloakClient) CreateUser(_ context.Context, _ User) (string, error) {
	return "", fmt.Errorf("user: keycloak integration is degraded (unreachable at boot or unconfigured)")
}
