// Package scheduling assembles the scheduling domain into a modular.Module.
//
// This is the body of what used to be cmd/scheduling-service/main.go, minus
// what the composer now owns.
package scheduling

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"telemed/internal/domain/scheduling/scheduling"
	schedulingv1 "telemed/internal/pb/scheduling/v1"
	"telemed/internal/platform/config"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/server"
)

// Domain is the name this module registers under.
const Domain = "scheduling"

// New assembles the scheduling domain.
func New(ctx context.Context, deps modular.Deps) (*modular.Module, error) {
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
		return nil, fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	log := deps.Log.With().Str("domain", Domain).Logger()
	schedMetrics := scheduling.NewMetrics(deps.Metrics.Registry())

	loc, err := cfg.Location()
	if err != nil {
		return nil, err
	}

	m := &modular.Module{Name: Domain}
	fail := func(err error) (*modular.Module, error) {
		for i := len(m.Closers) - 1; i >= 0; i-- {
			m.Closers[i]()
		}
		if m.Pool != nil {
			m.Pool.Close()
		}
		return nil, err
	}

	// --- this domain's own pool, as this domain's own role ----------------
	dsn, err := deps.DSN(Domain)
	if err != nil {
		return nil, fmt.Errorf("scheduling: %w", err)
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
		return nil, fmt.Errorf("scheduling: connect postgres: %w", err)
	}
	m.Pool = pool

	// --- repositories, services, handlers --------------------------------
	var locker scheduling.SlotLocker = scheduling.NoopLocker{}
	if cfg.SlotLockEnabled {
		locker = scheduling.RedisLocker{Cache: deps.Redis}
	} else {
		log.Warn().Msg("slot lock disabled: bookings rely on Postgres alone, which is correct but slower under contention")
	}

	svc := scheduling.NewService(scheduling.Options{
		Pool:       pool,
		Repository: scheduling.NewRepository(),
		Locker:     locker,
		Queue:      scheduling.RedisWaitlistQueue{Cache: deps.Redis},
		Outbox:     events.NewOutbox(serviceName),
		Clock:      scheduling.SystemClock{},
		Location:   loc,
		Logger:     log,
		Metrics:    schedMetrics,
	})
	handler := scheduling.NewHandler(svc, cfg.KeycloakIssuer)
	// --- background workers ------------------------------------------------
	// The outbox relay runs in every replica. Row-level SKIP LOCKED makes that
	// safe, and it means no single "worker" pod to lose.
	relay := events.NewRelay(pool, deps.Broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	m.Workers = append(m.Workers, modular.SimpleWorker("scheduling-outbox-relay", relay.Run))

	if cfg.ConsumersEnabled {
		consumers := scheduling.NewConsumers(svc, deps.Broker, deps.Metrics, log)
		m.Workers = append(m.Workers, modular.Worker{
			Name: "scheduling-events",
			Run: func(ctx context.Context) error {
				if err := consumers.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					return err
				}
				return nil
			},
		})
	}

	if cfg.SchedulerEnabled {
		sched := scheduling.NewScheduler(svc, loc, log)
		if err := sched.Register(ctx); err != nil {
			return fail(fmt.Errorf("scheduling: register scheduler: %w", err))
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

	// --- optional gRPC listener ---------------------------------------------
	// Off unless the domains are split across processes; see the note in the
	// user module. The interceptor below is unchanged and still fails CLOSED.
	if cfg.GRPCPort > 0 && deps.ExposeGRPC {
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
			Authenticator: deps.Auth,
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

		var lc net.ListenConfig
		grpcLn, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
		if err != nil {
			return fail(fmt.Errorf("scheduling: listen grpc: %w", err))
		}
		m.Workers = append(m.Workers, modular.Worker{
			Name: "scheduling-grpc",
			Run: func(ctx context.Context) error {
				go func() {
					<-ctx.Done()
					grpcSrv.GracefulStop()
				}()
				log.Info().Int("port", cfg.GRPCPort).Msg("grpc server listening")
				if err := grpcSrv.Serve(grpcLn); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
					return err
				}
				return nil
			},
		})
	}

	// --- routes ---------------------------------------------------------------
	m.API = func(r chi.Router) {
		// Availability is browsable without a token: it is how a patient
		// decides whether to sign up at all. OptionalAuth still attaches a
		// principal when one is present so the request log carries it.
		r.Group(func(r chi.Router) {
			r.Use(middleware.OptionalAuth(deps.Auth))
			r.Use(middleware.RateLimit(deps.Redis, middleware.RateLimitConfig{
				Name: "slots", Requests: 120, Window: time.Minute,
			}, log))
			handler.PublicRoutes(r)
		})

		// Patient and doctor surface.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuth(deps.Auth))
			r.Use(middleware.NoStore) // appointments carry intake; no proxy keeps a copy
			r.Use(middleware.RateLimit(deps.Redis, middleware.RateLimitConfig{
				Name: "booking", Requests: 60, Window: time.Minute, KeyFunc: middleware.ByPrincipal,
			}, log))
			handler.PatientRoutes(r)
		})

		// Admin overrides: allowlist, then token, then role.
		r.Route("/admin", func(r chi.Router) {
			r.Use(middleware.IPAllowlist(cfg.AdminIPAllowlist, log))
			r.Use(middleware.RequireAuth(deps.Auth))
			r.Use(middleware.RequireRole(middleware.AdminRoles...))
			r.Use(middleware.NoStore)
			handler.AdminRoutes(r)
		})
	}

	m.Health = []server.HealthCheck{
		{Name: "postgres", Critical: true, Check: pool.Ping},
	}
	return m, nil
}
