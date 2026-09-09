// Command server is the entrypoint template every telemed Go service follows.
//
// The boot sequence is intentionally rigid and identical across services:
//
//	config -> logger -> metrics -> tracing -> postgres -> redis -> nats
//	  -> repositories -> services -> handlers -> routes -> background workers
//	  -> listen -> drain
//
// An operator debugging service #7 at 3am should recognise the shape of
// service #2 immediately.
//
// telemed-admin-service is a JSON API only: doctor credentialing, platform
// analytics, disputes, configuration, and the append-only audit log. There
// is no server-rendered UI here -- telemed-admin-web (Next.js) is the only
// client. See README.md for what this service owns and what it does not.
package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/credentials"

	"github.com/rs/zerolog"

	"telemed/internal/domain/admin/adminusers"
	"telemed/internal/platform/servicetoken"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-admin-service"

// runProjector wraps subscribe in a restart-free error log: a durable NATS
// consumer that returns because the broker connection dropped should not
// crash the whole service, it should log and let the outer ctx cancellation
// (SIGTERM) be the only reason it stops.
func runProjector(log zerolog.Logger, name string, subscribe func() error) {
	if err := subscribe(); err != nil && !errors.Is(err, context.Canceled) {
		log.Error().Err(err).Str("projector", name).Msg("event projector stopped unexpectedly")
	}
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// buildIdentityProvider connects the Keycloak admin client used to create and
// re-role admin accounts.
//
// Unlike user-service, an unreachable Keycloak here does NOT degrade into a
// working service with a quiet warning. There, Keycloak mirrors an account
// that already exists and works, so degrading keeps patients logging in. Here
// it IS the admin identity provider, and the operations that need it --
// creating a login, changing the realm role that authorizes every route --
// have no meaningful half-success. UnavailableProvider refuses them loudly at
// the first attempt rather than writing rows for admins who cannot sign in, or
// reporting a demotion that never reached the realm.
//
// The rest of the service is unaffected: listing admins and deactivating one
// both work from the database alone.
func buildIdentityProvider(ctx context.Context, cfg appConfig, log zerolog.Logger) adminusers.IdentityProvider {
	if cfg.KeycloakBaseURL == "" || cfg.KeycloakAdminClientID == "" {
		return adminusers.NewUnavailableProvider(log)
	}
	p, err := adminusers.NewGocloakProvider(ctx, adminusers.KeycloakConfig{
		BaseURL:      cfg.KeycloakBaseURL,
		Realm:        cfg.KeycloakRealm,
		ClientID:     cfg.KeycloakAdminClientID,
		ClientSecret: cfg.KeycloakAdminClientSecret,
	})
	if err != nil {
		log.Error().Err(err).Msg("adminusers: keycloak admin client unavailable; " +
			"creating and re-roling admins is disabled until it is reachable")
		return adminusers.NewUnavailableProvider(log)
	}
	log.Info().Str("realm", cfg.KeycloakRealm).Msg("adminusers: keycloak admin client connected")
	return p
}

// buildMeshCredentials returns the service token source used on outbound gRPC.
//
// In production a missing configuration is fatal. That is the lesson of the
// break this fixes: a client with no credential does not fail at boot, it
// fails on every call afterwards, and both ends keep building, starting and
// passing their tests while the integration is dead. Refusing to start is the
// only failure mode an operator cannot miss.
//
// Outside production it degrades to nil, so a developer running one service
// against a mesh with no Keycloak still gets a working process.
func buildMeshCredentials(cfg appConfig, log zerolog.Logger) (credentials.PerRPCCredentials, error) {
	tokenURL := cfg.MeshTokenURL
	if tokenURL == "" && cfg.KeycloakBaseURL != "" {
		tokenURL = strings.TrimSuffix(cfg.KeycloakBaseURL, "/") +
			"/realms/" + cfg.KeycloakRealm + "/protocol/openid-connect/token"
	}

	src, err := servicetoken.New(servicetoken.Config{
		TokenURL:     tokenURL,
		ClientID:     cfg.MeshClientID,
		ClientSecret: cfg.MeshClientSecret,
		// The token is a mesh-wide bearer credential, so it may only travel
		// over TLS -- except where this deployment has already made the
		// explicit decision to run the mesh in plaintext.
		RequireTLS: cfg.UserServiceGRPCTLS,
	})
	if errors.Is(err, servicetoken.ErrNotConfigured) {
		if cfg.IsProd() {
			return nil, fmt.Errorf("MESH_CLIENT_ID/MESH_CLIENT_SECRET and KEYCLOAK_BASE_URL are "+
				"required in production: user-service authenticates every gRPC method, so a "+
				"client without a service token fails on every call: %w", err)
		}
		log.Warn().Msg("mesh service token not configured; outbound gRPC will be " +
			"unauthenticated and user-service will refuse it")
		return nil, nil //nolint:nilnil // an absent credential is a valid non-prod state
	}
	if err != nil {
		return nil, err
	}
	log.Info().Str("client_id", cfg.MeshClientID).Msg("mesh service token source ready")
	return src, nil
}
