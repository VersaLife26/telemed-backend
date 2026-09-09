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
	"time"

	"github.com/go-chi/chi/v5"
	"telemed/internal/domain/consultation/consultation"
	"telemed/internal/platform/config"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"

	"telemed/internal/platform/database"
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
	fail := func(err error) (*modular.Module, error) {
		for i := len(m.Closers) - 1; i >= 0; i-- {
			m.Closers[i]()
		}
		if m.Pool != nil {
			m.Pool.Close()
		}
		return nil, err
	}
	_ = fail

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

	// --- video provider ---------------------------------------------------
	video, err := buildVideoProvider(cfg)
	if err != nil {
		return nil, err
	}

	// --- repositories / services / handlers -------------------------------
	repo := consultation.NewRepository()
	outbox := events.NewOutbox(serviceName)
	svc := consultation.NewService(repo, video, deps.Redis, pool, outbox, log, consultation.Options{
		LiveKitURL:                  cfg.LiveKitURL,
		TokenTTL:                    cfg.LiveKitTokenTTL,
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

	// --- routes ---------------------------------------------------------
	m.API = func(r chi.Router) {
		r.Mount("/consultations", handler.Routes(deps.Auth))
	}

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

	m.Health = []server.HealthCheck{
		{Name: "postgres", Critical: true, Check: pool.Ping},
	}
	return m, nil
}
