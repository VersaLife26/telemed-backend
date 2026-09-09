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
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"telemed/internal/domain/notification/notification"
	"telemed/internal/platform/cache"
	"telemed/internal/platform/config"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
	"telemed/internal/platform/server"
	"telemed/internal/platform/servicetoken"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-notification-service"

func main() {
	if err := run(); err != nil {
		// Write to stderr rather than the structured logger: a failure here
		// may well be the logger itself, or config that never loaded.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// The root context is cancelled on SIGTERM so background workers stop in
	// step with the HTTP server instead of being killed mid-transaction.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- configuration -------------------------------------------------
	v := config.New(serviceName)
	notification.RegisterDefaults(v)
	var cfg notification.Config
	if err := v.Unmarshal(&cfg); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// --- logging -------------------------------------------------------
	log := logger.New(cfg.ServiceName, cfg.LogLevel, cfg.Env)
	log.Info().Str("version", Version).Msg("starting service")

	// --- observability -------------------------------------------------
	metrics := observability.NewMetrics(cfg.ServiceName)

	tracing, err := observability.NewTracing(ctx, observability.TracingOptions{
		ServiceName:  cfg.ServiceName,
		ServiceVer:   Version,
		Env:          cfg.Env,
		OTLPEndpoint: cfg.OTLPEndpoint,
		Sampling:     cfg.TraceSampling,
	})
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := tracing.Shutdown(shutdownCtx); err != nil {
			log.Warn().Err(err).Msg("tracing shutdown")
		}
	}()

	// --- datastores ----------------------------------------------------
	pool, err := database.Connect(ctx, database.Config{
		URL:         cfg.DatabaseURL,
		MaxConns:    cfg.DatabaseMaxConns,
		MinConns:    cfg.DatabaseMinConns,
		MaxConnLife: cfg.DatabaseMaxConnLife,
		AppName:     cfg.ServiceName,
	}, log)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	redis, err := cache.NewRedis(ctx, cache.Options{
		URL:      cfg.RedisURL,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err != nil {
		return fmt.Errorf("connect redis: %w", err)
	}
	defer func() { _ = redis.Close() }()

	broker, err := events.NewNATS(ctx, events.NATSOptions{
		URL:             cfg.NATSURL,
		Stream:          cfg.NATSStream,
		CredentialsFile: cfg.NATSCredentials,
		Name:            cfg.ServiceName,
		MaxBytes:        cfg.NATSMaxBytes,
	}, log)
	if err != nil {
		return fmt.Errorf("connect nats: %w", err)
	}
	defer func() { _ = broker.Close() }()

	// --- authentication -------------------------------------------------
	// TWO issuers, not one (ADR-010). IssuerKeys binds each to the one key
	// set that may sign for it: merging both key sets and then checking the
	// "iss" claim would let user-service's key sign a token claiming to come
	// from Keycloak, since the merged set matches on "kid" alone and "iss" is
	// chosen by whoever signs. Configuring only Keycloak, as this service did
	// before, rejects every patient and doctor token outright.
	auth, err := middleware.NewAuthenticatorFrom(ctx, middleware.AuthConfig{
		IssuerKeys: cfg.IssuerKeys(),
		Issuers:    cfg.TrustedIssuers(),
		Audience:   cfg.KeycloakAudience,
	})
	if err != nil {
		return fmt.Errorf("init authenticator: %w", err)
	}

	// Bind administrative roles to the admin issuer, in this service and not
	// only at the gateway.
	//
	// ADR-012 stopped user-service signing a token that CLAIMS to be Keycloak.
	// It never stopped user-service minting an honest token, under its own
	// issuer, carrying realm_access.roles = ["super_admin"]. Until this call
	// existed, adminIssuer was "" in every backend service, checkAdminIssuer
	// short-circuited to nil, and every admin-role check in this process --
	// RequireRole and every Principal.IsAdmin/HasAdminRole in the service
	// layer -- honoured that token. The gateway enforces the same rule at the
	// edge; a service reachable from inside the mesh must not be the soft
	// underbelly.
	//
	// Empty KEYCLOAK_ISSUER leaves the check disabled, which is the correct
	// behaviour for a single-issuer or test deployment and is why this is not
	// a boot failure here.
	middleware.SetAdminIssuer(cfg.KeycloakIssuer)

	// --- user directory ---------------------------------------------------
	// Where a recipient's phone and email come from. The canonical events do
	// not carry them (PHI must not fan out), so they are fetched per person
	// at enqueue time over user-service's internal gRPC surface.
	// The credential this process presents to user-service. Without it every
	// recipient lookup returns Unauthenticated, and because the consumer
	// propagates that error rather than swallowing it, the messages retry
	// instead of vanishing -- a growing backlog rather than silent loss, but
	// nothing gets delivered either way.
	meshCreds, err := buildMeshCredentials(cfg, log)
	if err != nil {
		return fmt.Errorf("init mesh credentials: %w", err)
	}

	directory, err := notification.NewUserServiceDirectory(cfg.UserServiceGRPCAddr,
		time.Duration(cfg.UserLookupTimeoutSeconds)*time.Second,
		notification.GRPCTLSConfig{
			Enabled:              cfg.UserServiceGRPCTLS,
			CAFile:               cfg.UserServiceGRPCCAFile,
			ServerName:           cfg.UserServiceGRPCServerName,
			AllowPlaintextInProd: cfg.UserServiceGRPCAllowPlaintext,
			IsProd:               cfg.IsProd(),
		}, meshCreds)
	if err != nil {
		return fmt.Errorf("init user directory: %w", err)
	}
	defer func() { _ = directory.Close() }()

	// --- domain wiring ---------------------------------------------------
	repo := notification.NewRepository(pool)

	providerRegistry, err := buildProviderRegistry(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("build notification provider registry: %w", err)
	}

	svc := notification.NewService(repo, providerRegistry, log, notification.ServiceOptions{
		MaxAttempts: cfg.MaxSendAttempts,
		BackoffBase: time.Duration(cfg.SendBackoffBaseSeconds) * time.Second,
		BackoffMax:  time.Duration(cfg.SendBackoffMaxMinutes) * time.Minute,
	})
	notifHandler := notification.NewHandler(svc, repo, log)

	// --- background workers ----------------------------------------------
	// The outbox relay runs in every replica. Row-level SKIP LOCKED makes that
	// safe, and it means no single "worker" pod to lose. This service rarely
	// enqueues outbox rows of its own (it is mostly a leaf consumer, the
	// platform's mouth, not a further publisher) but the relay is kept
	// running so the boot sequence -- and an operator's mental model of it --
	// stays identical across every service.
	relay := events.NewRelay(pool, broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	// The event consumer: turns booking/payment/doctor/prescription/
	// waitlist/consultation events into rendered, queued notifications.
	// Idempotent on envelope.ID via dedupe_key -- see consumer.go.
	consumer := notification.NewConsumer(svc, repo, directory, notification.NewLinks(cfg.PublicAppBaseURL), log)
	consumer.SetApplicationNotify(cfg.DoctorApplicationsNotifyEmail, cfg.DoctorPortalBaseURL)
	go func() {
		if err := broker.Subscribe(ctx, cfg.ServiceName+"-events", consumer.Subjects(), consumer.Handle); err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("event consumer stopped unexpectedly")
		}
	}()

	// The dispatch loop: claims queued/due notifications and attempts
	// delivery with classification-aware retry/backoff. See service.go.
	go svc.RunDispatcher(ctx, time.Duration(cfg.DispatchIntervalSeconds)*time.Second, cfg.DispatchBatchSize)

	// The reminder cron: every minute, confirmed appointments starting in
	// 55-65 minutes (and, on the same tick, ~24h out) get a reminder
	// enqueued from this service's own local projection. See reminder.go.
	reminderCron := notification.NewReminderCron(svc, repo, log)
	go reminderCron.Run(ctx, time.Duration(cfg.ReminderIntervalSeconds)*time.Second)

	// The retention/maintenance job: prunes PHI-adjacent notification
	// bodies past 90 days and hard-deletes long-invalidated device tokens.
	// See maintenance.go and the retention note in migrations/000002.
	maintenance := notification.NewMaintenanceJob(repo, log,
		time.Duration(cfg.NotificationBodyRetentionDays)*24*time.Hour,
		time.Duration(cfg.DeviceTokenRetentionDays)*24*time.Hour)
	go maintenance.Run(ctx, time.Duration(cfg.MaintenanceIntervalHours)*time.Hour)

	// --- http ------------------------------------------------------------
	srv := server.New(server.Options{
		TrustedProxies: buildTrustedProxies(cfg.TrustedProxies, log),
		ServiceName:    cfg.ServiceName,
		Version:        Version,
		Env:            cfg.Env,
		Port:           cfg.HTTPPort,
		Logger:         log,
		Metrics:        metrics,
		RequestTimeout: cfg.RequestTimeout,
		ShutdownGrace:  cfg.ShutdownGrace,
		HealthChecks: []server.HealthCheck{
			{Name: "postgres", Critical: true, Check: pool.Ping},
			{Name: "redis", Critical: true, Check: redis.Ping},
		},
	})

	srv.Router.Route("/api/v1", func(r chi.Router) {
		notifHandler.Routes(r, auth)
	})
	// Delivery callbacks carry no platform JWT, so they are gated on a shared
	// secret in the callback URL instead -- every backend here lets the URL be
	// configured. Config.Validate refuses to boot without one, so this is
	// never the empty string.
	srv.Router.Route("/webhooks", func(r chi.Router) {
		notifHandler.WebhookRoutes(r, cfg.WebhookSecret, middleware.RateLimit(redis, middleware.RateLimitConfig{
			Name: "notification-webhooks",
			// Generous enough for a burst of provider retries after an
			// outage, tight enough that a flood is cheap to shed.
			Requests: 600,
			Window:   time.Minute,
		}, log))
	})

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
}

// buildTrustedProxies parses TRUSTED_PROXIES. Returning nil hands
// server.New the platform default (private ranges only), which is the right
// answer for a docker-compose or single-ingress deployment. A malformed entry
// is logged and skipped rather than fatal -- one typo in a config map must not
// stop a pod starting -- but it is never treated as a wildcard.
func buildTrustedProxies(cidrs []string, log zerolog.Logger) *middleware.TrustedProxies {
	if len(cidrs) == 0 {
		return nil
	}
	tp, malformed := middleware.NewTrustedProxies(cidrs)
	for _, m := range malformed {
		log.Error().Str("cidr", m).Msg("ignoring malformed TRUSTED_PROXIES entry")
	}
	return tp
}

// buildMeshCredentials returns the service token source used on outbound gRPC.
//
// Production treats a missing configuration as fatal. A notification service
// that cannot resolve a recipient is not degraded, it is broken -- the same
// reasoning Config already applies to USER_SERVICE_GRPC_ADDR. Refusing to
// start is the only failure an operator cannot miss; the alternative is a
// process that looks healthy while every message retries forever.
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
