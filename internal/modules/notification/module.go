// Package notification assembles the notification domain into a modular.Module.
//
// This is the body of what used to be cmd/notification-service/main.go, minus
// everything the composer now owns: the logger, metrics, tracing, Redis, NATS,
// the authenticator and the HTTP server. What is left is the part that is
// genuinely this domain's -- its pool, its providers, its routes and its four
// background loops.
package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/credentials"

	"telemed/internal/domain/notification/notification"
	userv1 "telemed/internal/pb/user/v1"
	"telemed/internal/platform/config"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/server"
	"telemed/internal/platform/servicetoken"
)

// Domain is the name this module registers under: the schema suffix, the
// TELEMED_DOMAINS selector, and the metrics label.
const Domain = "notification"

const serviceName = "telemed-notification-service"

// New assembles the notification domain.
func New(ctx context.Context, deps modular.Deps) (*modular.Module, error) {
	// --- configuration ---------------------------------------------------
	// Its own viper instance. Every domain reads the same process environment,
	// which is right for the shared keys (REDIS_URL, NATS_URL) and harmless
	// for the domain-specific ones, whose names do not collide. The one key
	// that must NOT be shared is the database URL, which comes from the
	// composer instead -- see deps.DSN.
	v := config.New(serviceName)
	notification.RegisterDefaults(v)
	var cfg notification.Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("notification: load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("notification: %w", err)
	}
	log := deps.Log.With().Str("domain", Domain).Logger()

	m := &modular.Module{Name: Domain}
	fail := func(err error) (*modular.Module, error) {
		closeAll(m)
		return nil, err
	}

	// --- this domain's own pool, as this domain's own role ----------------
	dsn, err := deps.DSN(Domain)
	if err != nil {
		return nil, fmt.Errorf("notification: %w", err)
	}
	pool, err := database.Connect(ctx, database.Config{
		URL:         dsn,
		MaxConns:    deps.MaxConns,
		MinConns:    deps.MinConns,
		MaxConnLife: deps.MaxConnLife,
		AppName:     serviceName,
		SearchPath:  database.SearchPathFor(Domain),
	}, log)
	if err != nil {
		return nil, fmt.Errorf("notification: connect postgres: %w", err)
	}
	m.Pool = pool

	// --- user directory ---------------------------------------------------
	// Where a recipient's phone and email come from. The canonical events do
	// not carry them (PHI must not fan out), so they are fetched per person at
	// enqueue time.
	directory, err := userDirectory(cfg, deps, log)
	if err != nil {
		return fail(fmt.Errorf("notification: init user directory: %w", err))
	}
	m.Closers = append(m.Closers, func() { _ = directory.Close() })

	// --- domain wiring ----------------------------------------------------
	repo := notification.NewRepository(pool)

	providerRegistry, err := buildProviderRegistry(ctx, cfg, log)
	if err != nil {
		return fail(fmt.Errorf("notification: build provider registry: %w", err))
	}

	svc := notification.NewService(repo, providerRegistry, log, notification.ServiceOptions{
		MaxAttempts: cfg.MaxSendAttempts,
		BackoffBase: time.Duration(cfg.SendBackoffBaseSeconds) * time.Second,
		BackoffMax:  time.Duration(cfg.SendBackoffMaxMinutes) * time.Minute,
	})
	handler := notification.NewHandler(svc, repo, log)

	// --- background workers -----------------------------------------------
	// The outbox relay runs in every replica. Row-level SKIP LOCKED makes that
	// safe, and it means no single "worker" pod to lose. This domain rarely
	// enqueues outbox rows of its own -- it is mostly a leaf consumer, the
	// platform's mouth -- but the relay is kept running so the boot sequence
	// stays identical across every domain.
	relay := events.NewRelay(pool, deps.Broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)

	// Turns booking/payment/doctor/prescription/waitlist/consultation events
	// into rendered, queued notifications. Idempotent on envelope.ID via
	// dedupe_key -- see consumer.go.
	consumer := notification.NewConsumer(svc, repo, directory, notification.NewLinks(cfg.PublicAppBaseURL), log)
	consumer.SetApplicationNotify(cfg.DoctorApplicationsNotifyEmail, cfg.DoctorPortalBaseURL)

	// The retention/maintenance job: prunes PHI-adjacent notification bodies
	// past 90 days and hard-deletes long-invalidated device tokens.
	maintenance := notification.NewMaintenanceJob(repo, log,
		time.Duration(cfg.NotificationBodyRetentionDays)*24*time.Hour,
		time.Duration(cfg.DeviceTokenRetentionDays)*24*time.Hour)

	reminderCron := notification.NewReminderCron(svc, repo, log)

	m.Workers = []modular.Worker{
		modular.SimpleWorker("notification-outbox-relay", relay.Run),
		{
			Name: "notification-events",
			Run: func(ctx context.Context) error {
				return deps.Broker.Subscribe(ctx, serviceName+"-events", consumer.Subjects(), consumer.Handle)
			},
		},
		modular.SimpleWorker("notification-dispatcher", func(ctx context.Context) {
			svc.RunDispatcher(ctx, time.Duration(cfg.DispatchIntervalSeconds)*time.Second, cfg.DispatchBatchSize)
		}),
		modular.SimpleWorker("notification-reminder-cron", func(ctx context.Context) {
			reminderCron.Run(ctx, time.Duration(cfg.ReminderIntervalSeconds)*time.Second)
		}),
		modular.SimpleWorker("notification-maintenance", func(ctx context.Context) {
			maintenance.Run(ctx, time.Duration(cfg.MaintenanceIntervalHours)*time.Hour)
		}),
	}

	// --- routes ------------------------------------------------------------
	m.API = func(r chi.Router) { handler.Routes(r, deps.Auth) }

	// Delivery callbacks carry no platform JWT, so they are gated on a shared
	// secret in the callback URL instead. Config.Validate refuses to boot
	// without one, so this is never the empty string.
	m.Root = func(r chi.Router) {
		r.Route("/webhooks", func(r chi.Router) {
			handler.WebhookRoutes(r, cfg.WebhookSecret, middleware.RateLimit(deps.Redis, middleware.RateLimitConfig{
				Name: "notification-webhooks",
				// Generous enough for a burst of provider retries after an
				// outage, tight enough that a flood is cheap to shed.
				Requests: 600,
				Window:   time.Minute,
			}, log))
		})
	}

	m.Health = []server.HealthCheck{{Name: "postgres", Critical: true, Check: pool.Ping}}
	return m, nil
}

func closeAll(m *modular.Module) {
	for i := len(m.Closers) - 1; i >= 0; i-- {
		m.Closers[i]()
	}
	if m.Pool != nil {
		m.Pool.Close()
	}
}

// userDirectory resolves recipients, in-process when the user domain is loaded
// here and over gRPC when it is not.
//
// The in-process path is not a shortcut past an authorisation check: the mesh
// token the gRPC path presents proves "I am inside the mesh", which a call
// from another package in this binary already is. See user.InProcessClient.
func userDirectory(cfg notification.Config, deps modular.Deps, log zerolog.Logger) (*notification.UserServiceDirectory, error) {
	timeout := time.Duration(cfg.UserLookupTimeoutSeconds) * time.Second

	if v, ok := deps.Registry.Lookup(modular.KeyUserDirectory); ok {
		if client, ok := v.(userv1.UserServiceClient); ok {
			log.Info().Msg("user directory resolved in-process; no gRPC dial")
			return notification.NewInProcessDirectory(client, timeout), nil
		}
	}

	// The credential this process presents to user-service. Without it every
	// recipient lookup returns Unauthenticated, and because the consumer
	// propagates that error rather than swallowing it, the messages retry
	// instead of vanishing -- a growing backlog rather than silent loss, but
	// nothing gets delivered either way.
	meshCreds, err := buildMeshCredentials(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("init mesh credentials: %w", err)
	}
	return notification.NewUserServiceDirectory(cfg.UserServiceGRPCAddr, timeout,
		notification.GRPCTLSConfig{
			Enabled:              cfg.UserServiceGRPCTLS,
			CAFile:               cfg.UserServiceGRPCCAFile,
			ServerName:           cfg.UserServiceGRPCServerName,
			AllowPlaintextInProd: cfg.UserServiceGRPCAllowPlaintext,
			IsProd:               cfg.IsProd(),
		}, meshCreds)
}

func buildMeshCredentials(cfg notification.Config, log zerolog.Logger) (credentials.PerRPCCredentials, error) {
	tokenURL := cfg.MeshTokenURL
	if tokenURL == "" && cfg.KeycloakBaseURL != "" {
		tokenURL = strings.TrimSuffix(cfg.KeycloakBaseURL, "/") +
			"/realms/" + cfg.KeycloakRealmName + "/protocol/openid-connect/token"
	}

	src, err := servicetoken.New(servicetoken.Config{
		TokenURL:     tokenURL,
		ClientID:     cfg.MeshClientID,
		ClientSecret: cfg.MeshClientSecret,
		RequireTLS:   cfg.UserServiceGRPCTLS,
	})
	if errors.Is(err, servicetoken.ErrNotConfigured) {
		if cfg.IsProd() {
			return nil, fmt.Errorf("MESH_CLIENT_ID/MESH_CLIENT_SECRET and KEYCLOAK_BASE_URL are "+
				"required in production: user-service authenticates every gRPC method, so "+
				"without a service token no recipient can be resolved: %w", err)
		}
		log.Warn().Msg("mesh service token not configured; recipient lookups will be " +
			"refused by user-service")
		return nil, nil //nolint:nilnil // an absent credential is a valid non-prod state
	}
	if err != nil {
		return nil, err
	}
	log.Info().Str("client_id", cfg.MeshClientID).Msg("mesh service token source ready")
	return src, nil
}
