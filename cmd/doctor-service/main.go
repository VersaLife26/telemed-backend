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
// telemed-doctor-service additionally runs a gRPC listener (doctor.v1, for
// other services to resolve a doctor without joining across the database
// boundary) and two NATS consumers: one that folds slot.generated /
// slot.booked / slot.released into the local availability projection search
// depends on, and one that reacts to appointment.completed. See
// docs/DESIGN.md for why both exist.
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

	"telemed/internal/domain/doctor/analytics"
	"telemed/internal/domain/doctor/availability"
	"telemed/internal/domain/doctor/doctor"
	doctorv1 "telemed/internal/pb/doctor/v1"
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

const serviceName = "telemed-doctor-service"

// serviceConfig extends the shared config.Base with the keys this service
// alone needs. Embedding rather than duplicating fields is what keeps every
// service's HTTP port, database URL, etc. bound the same way.
type serviceConfig struct {
	config.Base `mapstructure:",squash"`

	// BankEncryptionKey is a base64-encoded 32-byte key for the
	// doctor.Encryptor that seals bank payout details before they touch the
	// database. See docs/RUNBOOK.md for rotation.
	BankEncryptionKey string `mapstructure:"bank_encryption_key"`

	// SearchCacheTTL bounds how long a doctor-search result page is served
	// from Redis before the query is re-run.
	SearchCacheTTL time.Duration `mapstructure:"search_cache_ttl"`

	// TrustedProxies is a comma-separated CIDR list whose X-Forwarded-For this
	// service honours. Empty means the private ranges only, never the public
	// internet -- see middleware.DefaultTrustedProxies.
	TrustedProxies string `mapstructure:"trusted_proxies"`

	// SchedulingBaseURL is where scheduling-service listens, e.g.
	// http://scheduling:8083. It is used for exactly one thing: forwarding the
	// `holidays` array on PUT /doctors/me/availability to the service that owns
	// the holidays table (ADR-004 -- doctor-service must not write it).
	//
	// Empty disables forwarding. An availability save carrying leave is then
	// refused with 501 rather than accepted and dropped, because a doctor told
	// their leave saved while patients keep booking them is the failure this
	// whole path exists to prevent. Saves with no leave -- the common case --
	// are unaffected.
	SchedulingBaseURL string `mapstructure:"scheduling_base_url"`

	// SchedulingTimeout bounds the holiday forward. It sits on a doctor's Save
	// button, so it is short by design.
	SchedulingTimeout time.Duration `mapstructure:"scheduling_timeout"`
}

// ProxyCIDRs splits the trusted-proxy list, dropping empty entries so that a
// trailing comma is not read as a network.
func (c serviceConfig) ProxyCIDRs() []string {
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
	v.SetDefault("search_cache_ttl", 60*time.Second)
	v.SetDefault("trusted_proxies", "")
	v.SetDefault("scheduling_base_url", "")
	v.SetDefault("scheduling_timeout", 5*time.Second)
	_ = v.BindEnv("trusted_proxies")
	_ = v.BindEnv("bank_encryption_key")
	_ = v.BindEnv("search_cache_ttl")
	_ = v.BindEnv("scheduling_base_url")
	_ = v.BindEnv("scheduling_timeout")

	var cfg serviceConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.BankEncryptionKey == "" {
		return fmt.Errorf("config: missing required key: BANK_ENCRYPTION_KEY")
	}

	// --- logging -------------------------------------------------------
	log := logger.New(cfg.ServiceName, cfg.LogLevel, cfg.Env)
	log.Info().Str("version", Version).Msg("starting service")

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
	// Two issuers, both trusted (ADR-010): telemed-user-service signs patient
	// and doctor tokens so phone-OTP login survives a Keycloak outage, and
	// Keycloak signs admin tokens. A doctor editing their own profile and an
	// admin approving a credential arrive on the same router, so accepting one
	// issuer only would reject every doctor with a signature error.
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

	// --- domain wiring ---------------------------------------------------
	loc, err := cfg.Location()
	if err != nil {
		return fmt.Errorf("resolve timezone: %w", err)
	}

	enc, err := doctor.NewSecretboxEncryptor(cfg.BankEncryptionKey)
	if err != nil {
		return fmt.Errorf("init bank encryptor: %w", err)
	}

	outbox := events.NewOutbox(cfg.ServiceName)
	doctorRepo := doctor.NewRepository(pool)
	// The holidays seam. scheduling-service owns the table; this service
	// forwards to it and never writes it (ADR-004). See
	// internal/doctor/holidays.go for why the forward is additive, why it is
	// skipped entirely for an empty array, and why it fails the request rather
	// than degrading.
	holidayClient := doctor.NewSchedulingHolidayClient(cfg.SchedulingBaseURL, cfg.SchedulingTimeout)
	if !holidayClient.Enabled() {
		log.Warn().Msg("SCHEDULING_BASE_URL is unset: an availability save carrying holidays will be refused with 501")
	}

	doctorSvc := doctor.NewService(doctorRepo, pool, outbox, redis, enc, cfg.SearchCacheTTL, log).
		WithHolidayRegistrar(holidayClient)

	// The doctor's own analytics. It reads a projection this service maintains
	// off the event bus rather than calling scheduling, consultation and
	// payment on the request path -- see internal/analytics for why, and
	// ADR-004 for why a join is not available.
	//
	// Its endpoints are REGISTERED INTO the doctor handler's /doctors/me
	// subtree rather than mounted separately: chi panics on two Mounts sharing
	// the /doctors prefix.
	analyticsSvc := analytics.NewService(pool, loc)
	analyticsHandler := analytics.NewHandler(analyticsSvc, doctorSvc)

	doctorHandler := doctor.NewHandler(doctorSvc, auth, analyticsHandler)

	// --- background workers ----------------------------------------------
	// The outbox relay runs in every replica. Row-level SKIP LOCKED makes that
	// safe, and it means no single "worker" pod to lose.
	relay := events.NewRelay(pool, broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	// The availability projection consumer: folds slot.generated/booked/
	// released into doctor_availability_summary, which is what doctor search
	// filters availability against instead of joining telemed_scheduling's
	// `slots` table (ADR-004). See docs/DESIGN.md.
	availabilityConsumer := availability.NewConsumer(pool, loc, log)
	go func() {
		if err := availabilityConsumer.Run(ctx, broker); err != nil && !errors.Is(err, context.Canceled) {
			log.Error().Err(err).Msg("availability consumer stopped")
		}
	}()

	// The appointment.completed consumer: unlocks review eligibility and
	// bumps consultation_count.
	appointmentConsumer := doctor.NewEventConsumer(doctorRepo, pool, log)
	go func() {
		if err := appointmentConsumer.Run(ctx, broker); err != nil && !errors.Is(err, context.Canceled) {
			log.Error().Err(err).Msg("appointment.completed consumer stopped")
		}
	}()

	// The analytics projection consumer: folds appointment outcomes, call
	// durations, settled payments and payouts into the per-doctor rollups that
	// back GET /doctors/me/analytics and /earnings.
	//
	// It is a SEPARATE durable from the one above even though both see
	// appointment.completed. Sharing one would mean a change to either
	// projection's subject list silently changing what the other receives, and
	// JetStream is perfectly happy to fan one subject out to two consumers.
	analyticsConsumer := analytics.NewConsumer(pool, loc, log)
	go func() {
		if err := analyticsConsumer.Run(ctx, broker); err != nil && !errors.Is(err, context.Canceled) {
			log.Error().Err(err).Msg("analytics consumer stopped")
		}
	}()

	// --- grpc --------------------------------------------------------------
	// A bare grpc.NewServer() has no authentication of any kind: until this
	// interceptor existed, anything that could open a TCP connection to :9092
	// had full authority over DoctorService. A NetworkPolicy is a network
	// accident, not an access control -- one port-forward, one misapplied
	// policy or one compromised pod is all it takes.
	//
	// The same Authenticator as the HTTP side, so there is one place where a
	// signature becomes a principal. RoleService is required, so a patient or
	// doctor token cannot drive the internal mesh even though this service
	// accepts those tokens on its HTTP surface.
	//
	// It fails CLOSED. There is no exempt method: this server registers no
	// health service, so the smallest hole available is none.
	grpcServer := grpc.NewServer(doctor.GRPCServerOptions(auth)...)
	doctorv1.RegisterDoctorServiceServer(grpcServer, doctor.NewGRPCServer(doctorRepo, log))

	// net.ListenConfig rather than net.Listen: the listener is then bound to
	// the root context and stops accepting when the process is shutting down,
	// instead of outliving it.
	var lc net.ListenConfig
	grpcLis, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}
	go func() {
		log.Info().Int("port", cfg.GRPCPort).Msg("grpc server listening")
		if err := grpcServer.Serve(grpcLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			log.Error().Err(err).Msg("grpc server error")
		}
	}()
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	// --- http ------------------------------------------------------------
	// Only these networks' X-Forwarded-For is believed. chi's RealIP trusts the
	// header unconditionally, which makes a client-chosen address the basis of
	// rate limiting and of any IP allowlist -- i.e. decoration.
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
		HealthChecks: []server.HealthCheck{
			{Name: "postgres", Critical: true, Check: pool.Ping},
			{Name: "redis", Critical: true, Check: redis.Ping},
		},
	})

	srv.Router.Route("/api/v1", func(r chi.Router) {
		r.Mount("/doctors", doctorHandler.Routes())
		r.Route("/internal/doctors", func(r chi.Router) {
			r.Use(middleware.RequireAuth(auth))
			r.Use(middleware.RequireRole(internalRoles...))
			r.Mount("/", doctorHandler.InternalRoutes())
		})
		// Staff schedule editing lives under the admin prefix, not /internal,
		// because the gateway never rewrites a path and its route table
		// requires every admin auth-mode route to sit beneath /api/v1/admin/.
		// The gateway routes this exact pattern here; the broader
		// /api/v1/admin/* catch-all still goes to admin-service, and chi
		// prefers the more specific match.
		r.Route("/admin/doctors", func(r chi.Router) {
			r.Use(middleware.RequireAuth(auth))
			r.Use(middleware.RequireRole(internalRoles...))
			r.Mount("/", doctorHandler.AdminScheduleRoutes())
		})
	})

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
}

// internalRoles are the roles permitted on the internal admin-service-facing
// surface: every human admin role plus the machine-to-machine "service" role
// admin-service itself authenticates as.
var internalRoles = append([]middleware.Role{middleware.RoleService}, middleware.AdminRoles...)
