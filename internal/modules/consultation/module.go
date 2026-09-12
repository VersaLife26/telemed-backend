// Package consultation assembles the consultation domain into a modular.Module.
//
// This is the body of what used to be cmd/consultation-service/main.go with the
// shared parts removed: the logger, metrics, tracing, Redis, NATS, the
// authenticator and the HTTP server all come from the composer now. The
// domain's own wiring is unchanged, line for line.
package consultation

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"telemed/internal/domain/consultation/consultation"
	"telemed/internal/domain/consultation/signal"
	"telemed/internal/platform/config"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/server"
)

// Domain is the name this module registers under.
const Domain = "consultation"

// New assembles the consultation domain.
func New(ctx context.Context, deps modular.Deps) (*modular.Module, error) {
	// --- configuration -------------------------------------------------
	v := config.New(serviceName)
	registerConsultationDefaults(v)

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := validateVideoConfig(cfg); err != nil {
		return nil, err
	}

	log := deps.Log.With().Str("domain", Domain).Logger()

	m := &modular.Module{Name: Domain}

	// --- this domain's own pool, as this domain's own role ----------------
	dsn, err := deps.DSN(Domain)
	if err != nil {
		return nil, fmt.Errorf("consultation: %w", err)
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
		return nil, fmt.Errorf("consultation: connect postgres: %w", err)
	}
	m.Pool = pool

	// --- signalling -------------------------------------------------------
	//
	// Built before the service, because the in-house provider is a view onto
	// this hub. svc is assigned after, so the hooks close over the variable
	// rather than its zero value -- they cannot fire until a socket connects,
	// which cannot happen before this function returns.
	var svc *consultation.Service

	// The bus is what makes a two-replica deployment work at all: nothing
	// routes a doctor and a patient to the same process, so without it they
	// land on different ones roughly half the time and see empty rooms.
	var bus *signal.Bus
	if rc, ok := deps.RedisClient(); ok {
		bus = signal.NewBus(rc, log)
	} else {
		log.Warn().Msg("no redis client for the signalling bus: rooms are process-local, " +
			"which is correct for one replica and silently wrong for more")
	}

	hub := signal.NewHub(ctx, signal.HubOptions{
		Log: log,
		Bus: bus,
		ICEServers: func(ctx context.Context, identity string) []signal.ICEServer {
			servers, err := deps.Signal.ICE.Servers(ctx, identity)
			if err != nil {
				// Degraded, not fatal: the client still gets STUN and the
				// call may connect. Logged because for a CGNAT user it will
				// not, and nothing else would say why.
				log.Warn().Err(err).Msg("could not mint ICE servers; this call has STUN only")
			}
			return servers
		},
		// Replaces the SFU's participant webhooks, which went away with the
		// SFU. Without it nothing ever writes
		// consultation_participants.left_at and every ended consultation
		// reports all participants still joined, forever.
		//
		// There is deliberately no room-emptied hook beside it. The occupancy
		// lease already makes ListParticipants accurate to about half a
		// minute, so the stale sweeper detects an abandoned call on its own --
		// a second mechanism would buy a few minutes of latency and cost a
		// second thing to reason about when a call ends at the wrong time.
		OnPeerChange: func(ctx context.Context, roomName, identity string, joined bool) {
			if svc != nil {
				svc.NotePeerChange(ctx, roomName, identity, joined)
			}
		},
	})

	// --- video provider ---------------------------------------------------
	if err := validateInHouseConfig(cfg, deps.Signal); err != nil {
		return nil, err
	}
	video, err := buildVideoProvider(cfg, deps, hub, bus)
	if err != nil {
		return nil, err
	}

	// --- repositories / services / handlers -------------------------------
	repo := consultation.NewRepository()
	outbox := events.NewOutbox(serviceName)
	svc = consultation.NewService(repo, video, deps.Redis, pool, outbox, log, consultation.Options{
		LiveKitURL: liveKitURLFor(cfg),
		SignalURL:  deps.Signal.PublicURL,
		TokenTTL:   tokenTTLFor(cfg, deps.Signal),
		ICEServers: func(ctx context.Context, identity string) []consultation.ICEServer {
			servers, err := deps.Signal.ICE.Servers(ctx, identity)
			if err != nil {
				log.Warn().Err(err).Msg("could not mint ICE servers for join response")
			}
			return toDomainICE(servers)
		},
		EmptyRoomTimeoutSeconds:     emptyRoomTimeoutSeconds(cfg.LiveKitEmptyRoomTimeout),
		RecordingBucket:             cfg.RecordingBucket,
		DefaultConsultationDuration: time.Duration(cfg.DefaultConsultationDurationSeconds) * time.Second,
		QualityDegradeThreshold:     cfg.QualityDegradeThreshold,
	})
	handler := consultation.NewHandler(svc, log)

	// --- background workers ----------------------------------------------
	// The outbox relay runs in every replica. Row-level SKIP LOCKED makes that
	// safe, and it means no single "worker" pod to lose.
	relay := events.NewRelay(pool, deps.Broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	// appointment.confirmed pre-creates the consultation row; appointment.cancelled
	// tears one down if the call never started. Both run in their own
	// goroutines internally -- see consultation.RegisterConsumers.
	consultation.RegisterConsumers(ctx, deps.Broker, svc, log)

	// The backstop for the room_finished webhook. Three paths reach
	// endInternal and only one of them -- the webhook -- covers the case where
	// both clients died outright; a consultation stuck in 'active' never
	// publishes consultation.ended, so scheduling never completes the
	// appointment and record-service never learns when care actually ended.
	//
	// It runs in every replica; SweepStaleActive takes a Redis lease, which is
	// what makes that safe. It never ends a consultation on database evidence
	// alone -- it asks the video provider first, and a provider error means
	// "try again later", never "the room is empty".
	if cfg.StaleSweepInterval > 0 {
		sweeper := consultation.NewStaleSweeper(svc,
			cfg.StaleSweepInterval, cfg.StaleIdleTimeout, cfg.StaleSweepBatch, log)
		go sweeper.Run(ctx)
	} else {
		log.Warn().Msg("STALE_SWEEP_INTERVAL is 0: consultations left active by a lost webhook will never be closed")
	}

	if cfg.RunningLateSweepInterval > 0 {
		late := consultation.NewRunningLateSweeper(svc,
			cfg.RunningLateSweepInterval, cfg.RunningLateSweepBatch, log)
		go late.Run(ctx)
	} else {
		log.Warn().Msg("RUNNING_LATE_SWEEP_INTERVAL is 0: next patients will not be told when a consult overruns")
	}

	// --- routes ---------------------------------------------------------
	m.API = func(r chi.Router) {
		r.Mount("/consultations", handler.Routes(deps.Auth))
	}

	// The signalling websocket, mounted RAW -- outside the chi middleware
	// chain and outside /api/v1.
	//
	// This is forced, not preferred. Three separate things in the shared chain
	// break on a hijacked connection: chi's Timeout cancels the request
	// context and then writes a header to a writer that no longer exists, the
	// compression middleware replaces the ResponseWriter, and the metrics
	// middleware records one latency observation per CONSULTATION, which
	// poisons the p99. And under /api/v1 the request additionally traverses
	// the gateway's in-process proxy, whose statusRecorder implements Flusher
	// but NOT http.Hijacker -- so the upgrade fails outright with "response
	// does not implement http.Hijacker".
	m.Raw = map[string]http.Handler{SignalPath(): signal.NewHandler(signal.HandlerOptions{
		Hub:    hub,
		Secret: deps.Signal.Secret,
		Log:    log,
		// Non-empty in production, enforced by validateInHouseConfig. The
		// test surface passes nil deliberately; a consultation must not.
		AllowedOrigins: deps.Signal.AllowedOrigins,
	})}

	m.Root = func(r chi.Router) {
		webhookRateLimit := middleware.RateLimit(deps.Redis, middleware.RateLimitConfig{
			Requests: 120,
			Window:   time.Minute,
			Name:     "livekit-webhook",
		}, log)
		// LiveKit's webhook is server-to-server, verified by its own signed
		// Authorization header (see VideoProvider.VerifyWebhook) rather than a
		// platform JWT, and lives outside RequireAuth entirely. It still gets a
		// dedicated rate limit and the handler itself caps the body size.
		r.Mount("/webhooks", handler.WebhookRoutes(webhookRateLimit))
	}

	m.Closers = append(m.Closers, func() {
		if bus != nil {
			if err := bus.Close(); err != nil {
				log.Warn().Err(err).Msg("consultation: closing the signalling bus")
			}
		}
	})

	// Handed to the test surface so /test/rooms drives the REAL hub rather
	// than a second one standing beside it.
	deps.Registry.Provide(modular.KeySignalHub, hub)

	m.Health = []server.HealthCheck{
		{Name: "postgres", Critical: true, Check: pool.Ping},
	}
	return m, nil
}
