// Command server is the telemed-scheduling-service entrypoint.
//
// The boot sequence is the platform's, unchanged:
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
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"telemed/internal/domain/scheduling/scheduling"
	schedulingv1 "telemed/internal/pb/scheduling/v1"
	"telemed/internal/platform/cache"
	"telemed/internal/platform/config"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
	"telemed/internal/platform/server"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

// BuildTime is stamped at build time.
var BuildTime = "unknown"

const serviceName = "telemed-scheduling-service"

func main() {
	if err := run(); err != nil {
		// Write to stderr rather than the structured logger: a failure here
		// may well be the logger itself, or config that never loaded.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// Config is the platform base plus this service's own knobs.
type Config struct {
	config.Base `mapstructure:",squash"`

	// SlotLockEnabled toggles the Redis early-reject lock. Correctness does not
	// depend on it (ADR-007) and the concurrency test proves that, so an
	// operator can switch it off during a Redis incident without stopping
	// bookings.
	SlotLockEnabled bool `mapstructure:"slot_lock_enabled"`

	// SchedulerEnabled toggles the cron jobs. Off in a replica dedicated to
	// serving traffic, or when running migrations in a maintenance pod.
	SchedulerEnabled bool `mapstructure:"scheduler_enabled"`

	// ConsumersEnabled toggles the JetStream subscriptions.
	ConsumersEnabled bool `mapstructure:"consumers_enabled"`

	// AdminIPAllowlist restricts the /api/v1/admin tree, in addition to the
	// role check. Defence at the edge that disappears the moment somebody
	// reaches the origin is not defence.
	AdminIPAllowlist []string `mapstructure:"admin_ip_allowlist"`

	CORSOrigins []string `mapstructure:"cors_origins"`

	// TrustedProxies is a comma-separated CIDR list whose X-Forwarded-For this
	// service honours. Empty means the private ranges only, never the public
	// internet -- see middleware.DefaultTrustedProxies.
	TrustedProxies string `mapstructure:"trusted_proxies"`
}

// ProxyCIDRs splits the trusted-proxy list, dropping empty entries so that a
// trailing comma is not read as a network.
func (c Config) ProxyCIDRs() []string {
	raw := strings.TrimSpace(c.TrustedProxies)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// reflectionAllowed reports whether gRPC server reflection may be registered.
//
// It is an ALLOWLIST of development environments, not a denylist of production
// ones, and that direction is the whole point. A denylist keyed on IsProd()
// leaves reflection on in staging, in any environment somebody names
// "prod-eu-west", and in a pod whose ENV was never set -- which is exactly the
// shape of F17, where an unset variable silently turned a documented security
// control into decoration. An unrecognised environment is treated as
// production, because the cost of being wrong that way is one operator running
// grpcurl with a .proto file instead of without one.
func reflectionAllowed(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "dev", "development", "local", "test":
		return true
	default:
		return false
	}
}

func run() error {
	// The root context is cancelled on SIGTERM so background workers stop in
	// step with the HTTP server instead of being killed mid-transaction.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- configuration -------------------------------------------------
	v := config.New(serviceName)
	v.SetDefault("http_port", 8083)
	v.SetDefault("grpc_port", 9093)
	v.SetDefault("slot_lock_enabled", true)
	v.SetDefault("scheduler_enabled", true)
	v.SetDefault("consumers_enabled", true)
	v.SetDefault("trusted_proxies", "")
	for _, k := range []string{"slot_lock_enabled", "scheduler_enabled", "consumers_enabled",
		"admin_ip_allowlist", "cors_origins", "trusted_proxies"} {
		_ = v.BindEnv(k)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// --- logging -------------------------------------------------------
	log := logger.New(cfg.ServiceName, cfg.LogLevel, cfg.Env)
	log.Info().Str("version", Version).Str("built", BuildTime).Msg("starting service")

	// --- admin-role issuer binding ---------------------------------------
	// An administrative role is honoured only from the admin issuer.
	//
	// ADR-012 stops user-service claiming to BE Keycloak by binding each key
	// set to the issuer that may use it. It does nothing about user-service
	// minting an entirely honest token -- correct `iss`, correct key, verifying
	// cleanly -- that asserts realm_access.roles = ["super_admin"]. Since
	// user-service is the phone-OTP issuer and Keycloak is where SAML SSO and
	// enforced 2FA live, that is a 2FA bypass, not merely a role escalation
	// (security review F1).
	//
	// The gateway enforces this at the edge, and only for routes declared
	// `auth: "admin"`. This call is what makes middleware.RequireRole enforce
	// it here as well, so a service reached from inside the mesh -- a
	// compromised pod, a NetworkPolicy gap, an SSRF -- is not the soft
	// underbelly. It was already wired in payment-service and missing here.
	//
	// An empty KEYCLOAK_ISSUER leaves the check disabled, which is correct for
	// a single-issuer developer stack and is why this is not a boot failure.
	middleware.SetAdminIssuer(cfg.KeycloakIssuer)

	loc, err := cfg.Location()
	if err != nil {
		return err
	}

	// --- observability -------------------------------------------------
	metrics := observability.NewMetrics(cfg.ServiceName)
	schedMetrics := scheduling.NewMetrics(metrics.Registry())

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
	// Two issuers, both trusted (ADR-010): telemed-user-service signs patient
	// and doctor tokens so phone-OTP login survives a Keycloak outage, and
	// Keycloak signs admin tokens. Booking is a patient action, so trusting
	// Keycloak alone here would reject every booking with a signature error.
	auth, err := middleware.NewAuthenticatorFrom(ctx, middleware.AuthConfig{
		// IssuerKeys, not JWKSURLs: each issuer is bound to the ONE key set
		// allowed to sign for it. Merging both key sets and then checking the
		// "iss" claim looks equivalent and is not -- the merged set matches on
		// "kid" alone, so user-service's key would verify a token whose "iss"
		// says Keycloak, and the allowlist reads a claim the signer chooses.
		// That would hand the patient issuer the power to mint admin tokens,
		// which is exactly what ADR-010 says two issuers must never allow.
		IssuerKeys: cfg.IssuerKeys(),
		Audience:   cfg.KeycloakAudience,
	})
	if err != nil {
		return fmt.Errorf("init authenticator: %w", err)
	}

	// --- repositories, services, handlers --------------------------------
	var locker scheduling.SlotLocker = scheduling.NoopLocker{}
	if cfg.SlotLockEnabled {
		locker = scheduling.RedisLocker{Cache: redis}
	} else {
		log.Warn().Msg("slot lock disabled: bookings rely on Postgres alone, which is correct but slower under contention")
	}

	svc := scheduling.NewService(scheduling.Options{
		Pool:       pool,
		Repository: scheduling.NewRepository(),
		Locker:     locker,
		Queue:      scheduling.RedisWaitlistQueue{Cache: redis},
		Outbox:     events.NewOutbox(cfg.ServiceName),
		Clock:      scheduling.SystemClock{},
		Location:   loc,
		Logger:     log,
		Metrics:    schedMetrics,
	})
	handler := scheduling.NewHandler(svc, cfg.KeycloakIssuer)

	// --- background workers ----------------------------------------------
	// The outbox relay runs in every replica. Row-level SKIP LOCKED makes that
	// safe, and it means no single "worker" pod to lose.
	relay := events.NewRelay(pool, broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	if cfg.ConsumersEnabled {
		consumers := scheduling.NewConsumers(svc, broker, metrics, log)
		go func() {
			if err := consumers.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error().Err(err).Msg("event consumer stopped")
			}
		}()
	}

	if cfg.SchedulerEnabled {
		sched := scheduling.NewScheduler(svc, loc, log)
		if err := sched.Register(ctx); err != nil {
			return fmt.Errorf("register scheduler: %w", err)
		}
		sched.Start(ctx)

		// Make sure the partition runway exists before the first booking rather
		// than at the first cron tick. A missing partition sends every insert
		// into slots_default, which works but is not what anyone wants.
		bootCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if _, err := svc.EnsurePartitionRunway(bootCtx); err != nil {
			log.Error().Err(err).Msg("could not ensure slot partitions at boot")
		}
		cancel()
	}

	// --- grpc -------------------------------------------------------------
	//
	// This server shipped as a bare grpc.NewServer() with reflection on: no
	// interceptor, no credentials, nothing. Anything that could open a TCP
	// connection to 9093 had full authority over every appointment on the
	// platform, and reflection handed it the schema to do it with. The
	// NetworkPolicy was the only control, and a network boundary is an
	// accident of deployment, not an access control.
	//
	// Every method now requires a verified service token. The interceptor
	// fails CLOSED -- a server with no authenticator refuses every call rather
	// than serving them unauthenticated -- and it puts the verified principal
	// on the context so a handler reads the CALLER's identity instead of
	// believing an actor_id in the request.
	//
	// AllowUnauthenticated is deliberately empty. Kubernetes probes this
	// service over HTTP (/health on 8083), so there is no health method that
	// needs a hole, and a hole here is a hole in the whole surface.
	grpcAuth := middleware.ServiceAuthConfig{
		Authenticator: auth,
		RequiredRole:  middleware.RoleService,
	}
	grpcSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(middleware.UnaryServiceAuth(grpcAuth)),
		grpc.ChainStreamInterceptor(middleware.StreamServiceAuth(grpcAuth)),
		// gRPC's default inbound message cap is 4 MiB. Nothing this service
		// accepts over gRPC is remotely that large -- the widest request is a
		// GetAppointment carrying one UUID -- and an unexamined 4 MiB budget is
		// what every future method would inherit. 256 KiB is generous for
		// everything on this surface today and bounds what the transport will
		// assemble in memory before any interceptor or handler runs.
		grpc.MaxRecvMsgSize(scheduling.MaxGRPCRecvBytes),
	)
	schedulingv1.RegisterSchedulingServiceServer(grpcSrv, scheduling.NewGRPCServer(svc))

	// Reflection makes grpcurl work against a running pod, which is the first
	// thing anyone reaches for during an incident -- and it also hands anyone
	// who reaches the port a self-describing API, which is how F5's exploit was
	// written. The interceptor above is the control either way; this only
	// removes the map.
	if reflectionAllowed(cfg.Env) {
		reflection.Register(grpcSrv)
		log.Warn().Str("env", cfg.Env).Msg("grpc reflection is ON: development environment")
	} else {
		log.Info().Str("env", cfg.Env).Msg("grpc reflection disabled outside development")
	}

	// net.ListenConfig rather than net.Listen: the listener is bound to the
	// root context and stops accepting when the process is shutting down.
	var lc net.ListenConfig
	grpcLn, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}
	grpcErr := make(chan error, 1)
	go func() {
		log.Info().Int("port", cfg.GRPCPort).Msg("grpc server listening")
		if err := grpcSrv.Serve(grpcLn); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			grpcErr <- err
		}
	}()
	defer grpcSrv.GracefulStop()

	// --- http ------------------------------------------------------------
	// AdminIPAllowlist is only a control if the address it matches cannot be
	// chosen by the caller. chi's deprecated RealIP rewrites RemoteAddr from
	// an unauthenticated header; this resolves the peer and believes forwarding
	// headers only from networks we name.
	proxies := middleware.DefaultTrustedProxies()
	if cidrs := cfg.ProxyCIDRs(); len(cidrs) > 0 {
		parsed, malformed := middleware.NewTrustedProxies(cidrs)
		for _, bad := range malformed {
			log.Error().Str("cidr", bad).Msg("ignoring malformed TRUSTED_PROXIES entry")
		}
		proxies = parsed
	}

	srv := server.New(server.Options{
		TrustedProxies: proxies,
		ServiceName:    cfg.ServiceName,
		Version:        Version,
		Env:            cfg.Env,
		Port:           cfg.HTTPPort,
		Logger:         log,
		Metrics:        metrics,
		RequestTimeout: cfg.RequestTimeout,
		ShutdownGrace:  cfg.ShutdownGrace,
		CORSOrigins:    cfg.CORSOrigins,
		HealthChecks: []server.HealthCheck{
			{Name: "postgres", Critical: true, Check: pool.Ping},
			// Redis is NOT critical. The slot lock is an optimisation and the
			// waitlist index has a Postgres fallback, so a Redis outage must
			// not pull this pod out of rotation and stop every booking on the
			// platform (ADR-007).
			{Name: "redis", Critical: false, Check: redis.Ping},
		},
	})

	srv.Router.Route("/api/v1", func(r chi.Router) {
		// Availability is browsable without a token: it is how a patient
		// decides whether to sign up at all. OptionalAuth still attaches a
		// principal when one is present so the request log carries it.
		r.Group(func(r chi.Router) {
			r.Use(middleware.OptionalAuth(auth))
			r.Use(middleware.RateLimit(redis, middleware.RateLimitConfig{
				Name: "slots", Requests: 120, Window: time.Minute,
			}, log))
			handler.PublicRoutes(r)
		})

		// Patient and doctor surface.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuth(auth))
			r.Use(middleware.NoStore) // appointments carry intake; no proxy keeps a copy
			r.Use(middleware.RateLimit(redis, middleware.RateLimitConfig{
				Name: "booking", Requests: 60, Window: time.Minute, KeyFunc: middleware.ByPrincipal,
			}, log))
			handler.PatientRoutes(r)
		})

		// Admin overrides: allowlist, then token, then role.
		r.Route("/admin", func(r chi.Router) {
			r.Use(middleware.IPAllowlist(cfg.AdminIPAllowlist, log))
			r.Use(middleware.RequireAuth(auth))
			r.Use(middleware.RequireRole(middleware.AdminRoles...))
			r.Use(middleware.NoStore)
			handler.AdminRoutes(r)
		})
	})

	select {
	case err := <-grpcErr:
		return fmt.Errorf("grpc server: %w", err)
	default:
	}

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
}
