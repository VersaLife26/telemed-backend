// Package doctor assembles the doctor domain into a modular.Module.
//
// This is the body of what used to be cmd/doctor-service/main.go with the
// shared parts removed: the logger, metrics, tracing, Redis, NATS, the
// authenticator and the HTTP server all come from the composer now. The
// domain's own wiring is unchanged, line for line.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"

	"github.com/go-chi/chi/v5"

	"telemed/internal/domain/doctor/analytics"
	"telemed/internal/domain/doctor/availability"
	"telemed/internal/domain/doctor/doctor"
	doctorv1 "telemed/internal/pb/doctor/v1"
	"telemed/internal/platform/config"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"

	"telemed/internal/platform/database"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/server"
)

// Domain is the name this module registers under.
const Domain = "doctor"

// New assembles the doctor domain.
func New(ctx context.Context, deps modular.Deps) (*modular.Module, error) {
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
		return nil, fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.BankEncryptionKey == "" {
		return nil, fmt.Errorf("config: missing required key: BANK_ENCRYPTION_KEY")
	}

	log := deps.Log.With().Str("domain", Domain).Logger()

	m := &modular.Module{Name: Domain}
	// fail releases whatever this module has already opened and hands the error
	// back. It returns only the error: the module is always nil on this path,
	// and saying so twice invited a caller to return a half-built one.
	fail := func(err error) error {
		for i := len(m.Closers) - 1; i >= 0; i-- {
			m.Closers[i]()
		}
		if m.Pool != nil {
			m.Pool.Close()
		}
		return err
	}

	// --- this domain's own pool, as this domain's own role ----------------
	dsn, err := deps.DSN(Domain)
	if err != nil {
		return nil, fmt.Errorf("doctor: %w", err)
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
		return nil, fmt.Errorf("doctor: connect postgres: %w", err)
	}
	m.Pool = pool

	// --- domain wiring ---------------------------------------------------
	loc, err := cfg.Location()
	if err != nil {
		return nil, fmt.Errorf("resolve timezone: %w", err)
	}

	enc, err := doctor.NewSecretboxEncryptor(cfg.BankEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("init bank encryptor: %w", err)
	}

	outbox := events.NewOutbox(serviceName)
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

	doctorSvc := doctor.NewService(doctorRepo, pool, outbox, deps.Redis, enc, cfg.SearchCacheTTL, log).
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

	doctorHandler := doctor.NewHandler(doctorSvc, deps.Auth, analyticsHandler)

	// --- background workers ----------------------------------------------
	// The outbox relay runs in every replica. Row-level SKIP LOCKED makes that
	// safe, and it means no single "worker" pod to lose.
	relay := events.NewRelay(pool, deps.Broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	// The availability projection consumer: folds slot.generated/booked/
	// released into doctor_availability_summary, which is what doctor search
	// filters availability against instead of joining telemed_scheduling's
	// `slots` table (ADR-004). See docs/DESIGN.md.
	availabilityConsumer := availability.NewConsumer(pool, loc, log)
	go func() {
		if err := availabilityConsumer.Run(ctx, deps.Broker); err != nil && !errors.Is(err, context.Canceled) {
			log.Error().Err(err).Msg("availability consumer stopped")
		}
	}()

	// The appointment.completed consumer: unlocks review eligibility and
	// bumps consultation_count.
	appointmentConsumer := doctor.NewEventConsumer(doctorRepo, pool, log)
	go func() {
		if err := appointmentConsumer.Run(ctx, deps.Broker); err != nil && !errors.Is(err, context.Canceled) {
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
		if err := analyticsConsumer.Run(ctx, deps.Broker); err != nil && !errors.Is(err, context.Canceled) {
			log.Error().Err(err).Msg("analytics consumer stopped")
		}
	}()

	// --- optional gRPC listener --------------------------------------------
	// Off unless the domains are split across processes. The interceptor is
	// unchanged and still fails CLOSED; see the note in the user module.
	if cfg.GRPCPort > 0 && deps.ExposeGRPC {
		// The same Authenticator as the HTTP side, so there is one place where
		// a signature becomes a principal. RoleService is required, so a
		// patient or doctor token cannot drive the internal mesh even though
		// this domain accepts those tokens on its HTTP surface.
		// contextcheck: the stream interceptor uses the STREAM\'s context, which
		// is the only correct one -- there is no request context at registration
		// time, and inheriting one would tie every stream to whichever request
		// happened to construct the server.
		//nolint:contextcheck
		grpcServer := grpc.NewServer(doctor.GRPCServerOptions(deps.Auth)...)
		doctorv1.RegisterDoctorServiceServer(grpcServer, doctor.NewGRPCServer(doctorRepo, log))

		// net.ListenConfig rather than net.Listen: the listener is bound to
		// the root context and stops accepting when the process is shutting
		// down, instead of outliving it.
		var lc net.ListenConfig
		grpcLis, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
		if err != nil {
			return nil, fail(fmt.Errorf("doctor: listen grpc: %w", err))
		}
		m.Workers = append(m.Workers, modular.Worker{
			Name: "doctor-grpc",
			Run: func(ctx context.Context) error {
				go func() {
					<-ctx.Done()
					grpcServer.GracefulStop()
				}()
				log.Info().Int("port", cfg.GRPCPort).Msg("grpc server listening")
				if err := grpcServer.Serve(grpcLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
					return err
				}
				return nil
			},
		})
	}

	// --- routes ---------------------------------------------------------
	m.API = func(r chi.Router) {
		r.Mount("/doctors", doctorHandler.Routes())
		r.Route("/internal/doctors", func(r chi.Router) {
			r.Use(middleware.RequireAuth(deps.Auth))
			r.Use(middleware.RequireRole(internalRoles...))
			r.Mount("/", doctorHandler.InternalRoutes())
		})
		// Staff schedule editing lives under the admin prefix, not /internal,
		// because the gateway never rewrites a path and its route table
		// requires every admin deps.Auth-mode route to sit beneath /api/v1/admin/.
		// The gateway routes this exact pattern here; the broader
		// /api/v1/admin/* catch-all still goes to admin-service, and chi
		// prefers the more specific match.
		r.Route("/admin/doctors", func(r chi.Router) {
			r.Use(middleware.RequireAuth(deps.Auth))
			r.Use(middleware.RequireRole(internalRoles...))
			r.Mount("/", doctorHandler.AdminScheduleRoutes())
		})
	}

	m.Health = []server.HealthCheck{
		{Name: "postgres", Critical: true, Check: pool.Ping},
	}
	return m, nil
}
