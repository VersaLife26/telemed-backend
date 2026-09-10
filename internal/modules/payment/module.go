// Package payment assembles the payment domain into a modular.Module.
//
// This is the body of what used to be cmd/payment-service/main.go with the
// shared parts removed: the logger, metrics, tracing, Redis, NATS, the
// authenticator and the HTTP server all come from the composer now. The
// domain's own wiring is unchanged, line for line.
package payment

import (
	"context"
	"fmt"
	"time"

	"github.com/go-chi/chi/v5"

	"telemed/internal/domain/payment/payment"
	"telemed/internal/platform/config"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"

	"telemed/internal/platform/database"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/server"
)

// Domain is the name this module registers under.
const Domain = "payment"

// New assembles the payment domain.
func New(ctx context.Context, deps modular.Deps) (*modular.Module, error) {
	// --- configuration -------------------------------------------------
	v := config.New(serviceName)
	payment.ApplyDefaults(v)

	var cfg payment.Settings
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	log := deps.Log.With().Str("domain", Domain).Logger()

	m := &modular.Module{Name: Domain}

	// --- this domain's own pool, as this domain's own role ----------------
	dsn, err := deps.DSN(Domain)
	if err != nil {
		return nil, fmt.Errorf("payment: %w", err)
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
		return nil, fmt.Errorf("payment: connect postgres: %w", err)
	}
	m.Pool = pool

	// --- payment rails ---------------------------------------------------
	registry, err := buildRegistry(cfg, log)
	if err != nil {
		return nil, err
	}
	log.Info().
		Strs("providers", providerNames(registry)).
		Str("default", string(registry.Default())).
		Msg("payment rails configured")

	// --- repository, pricing, services -----------------------------------
	outbox := events.NewOutbox(serviceName)
	repo := payment.NewRepository(pool, outbox)

	if err := seedCommissionRules(ctx, repo, cfg, log); err != nil {
		return nil, err
	}

	pricer := payment.NewPricer(repo, cfg.CommissionTTL, cfg.FallbackRule())
	if err := pricer.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("load commission rules: %w", err)
	}

	svc := payment.NewService(repo, pricer, registry, log, payment.Config{
		HoldPeriod:      cfg.PayoutHoldPeriod,
		DefaultProvider: payment.ProviderName(cfg.DefaultProvider),
		Currency:        payment.CurrencyLKR,
		Promo:           payment.PromoConfig{ReservationTTL: cfg.PromoReservationTTL},
		PIN: payment.PINConfig{
			TTL: cfg.PINChallengeTTL, MaxAttempts: cfg.PINMaxAttempts,
			MaxResends: cfg.PINMaxResends, ResendWindow: cfg.PINResendWindow,
		},
		Vault: payment.VaultConfig{
			Enabled:  cfg.VaultEnabled,
			Provider: payment.ProviderName(cfg.VaultProvider),
		},
	})
	if cfg.VaultEnabled {
		log.Info().Str("vault_provider", cfg.VaultProvider).Msg("saved payment methods enabled")
	} else {
		log.Warn().Msg("PAYMENT_METHODS_ENABLED is false; /api/v1/payments/methods answers 501")
	}

	destinations, err := cfg.ParsePayoutAccounts()
	if err != nil {
		return nil, err
	}
	payoutProvider := payment.ProviderName(cfg.PayoutProvider)
	if payoutProvider == "" {
		payoutProvider = registry.Default()
	}
	payouts := payment.NewPayoutRunner(repo, registry, destinations, log, payment.PayoutConfig{
		HoldPeriod:       cfg.PayoutHoldPeriod,
		Provider:         payoutProvider,
		MaxDoctorsPerRun: cfg.PayoutMaxDoctors,
		Schedule:         cfg.PayoutSchedule,
		Timezone:         cfg.Timezone,
		LeaseTTL:         cfg.PayoutLeaseTTL,
	}).WithLease(deps.Redis)

	invoices, err := payment.NewInvoiceRenderer(payment.InvoiceBranding{
		CompanyName: cfg.InvoiceCompany,
		AddressLine: cfg.InvoiceAddress,
		TaxID:       cfg.InvoiceTaxID,
		Email:       cfg.InvoiceEmail,
		Phone:       cfg.InvoicePhone,
		Timezone:    cfg.Timezone,
	})
	if err != nil {
		return nil, err
	}

	handler := payment.NewHandler(svc, payouts, invoices, log)
	webhooks := payment.NewWebhookHandler(svc, log).WithDialogAllowlist(cfg.DialogPayCIDRs())

	// --- background workers ----------------------------------------------
	relay := events.NewRelay(pool, deps.Broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	// Start blocks: it runs the JetStream consumer until ctx is cancelled.
	// Calling it inline here meant srv.Run below was never reached, so the
	// service bound no HTTP port at all and every webhook was unreachable --
	// while the logs looked completely healthy.
	consumer := payment.NewConsumer(svc, deps.Broker, payouts, log)
	go func() {
		if err := consumer.Start(ctx); err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("payment event consumer stopped")
		}
	}()

	// The maintenance loop retires abandoned promo holds and unanswered PIN
	// challenges. Neither is what makes those expiries correct -- ApplyPromo
	// releases a code's own stale holds before it reads the budget, and
	// ConfirmPIN refuses an expired challenge -- so a sweeper that is down
	// costs tidiness and reporting, never money. That is deliberate: a
	// correctness property that depends on a background goroutine still
	// running is a correctness property that fails silently.
	go runMaintenance(ctx, svc, cfg.PromoSweepInterval, log)

	// The cron scheduler starts in EVERY replica, and POST /api/v1/payouts/run
	// can fire on top of it. That is deliberate -- a scheduler that only runs
	// in one pod is a scheduler that stops when that pod is rescheduled -- and
	// it is safe because the correctness comes from Postgres: one batch per
	// (doctor, period), `payout_id IS NULL` on the claim, and prepareBatch
	// refusing to grow a batch that is not pending. The Redis lease above is
	// what keeps the normal case from generating that contention at all.
	if cfg.PayoutEnabled {
		go func() {
			if err := payouts.Start(ctx); err != nil {
				log.Error().Err(err).Msg("payout scheduler stopped")
			}
		}()
	} else {
		log.Warn().Msg("payout scheduler disabled; settlements only run via POST /api/v1/payouts/run")
	}

	// --- routes ---------------------------------------------------------
	m.API = func(r chi.Router) {
		// Webhooks sit under /api/v1 because that is the URL a payment
		// provider is configured with, and the gateway forwards the path
		// UNCHANGED -- it rewrites scheme and host only, deliberately, so a
		// route is debuggable end to end.
		//
		// Mounting them at a bare /webhooks meant every provider callback
		// arrived at the gateway as /api/v1/webhooks/stripe and reached this
		// service as a 404. No Stripe, PayHere or Dialog payment could ever
		// settle in a deployed system; only a direct call to :8085 worked,
		// which is exactly how it went unnoticed in local testing.
		//
		// They are registered OUTSIDE the authenticated group below, not
		// merely outside a middleware chain: Stripe has no Keycloak token.
		// They authenticate by signature, and are rate limited and
		// body-size capped inside the handler because an unauthenticated
		// endpoint needs both.
		r.Mount("/webhooks", webhooks.Routes(deps.Redis))

		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuth(deps.Auth))
			// A per-principal limit on top of the global one. Payment
			// endpoints call out to a rail, so they are the expensive ones
			// to abuse.
			r.Use(middleware.RateLimit(deps.Redis, middleware.RateLimitConfig{
				Name:     "payments",
				Requests: 120,
				Window:   time.Minute,
				KeyFunc:  middleware.ByPrincipal,
			}, log))

			r.Mount("/payments", handler.Routes())
			r.Mount("/payouts", handler.PayoutRoutes())
		})
	}

	m.Health = []server.HealthCheck{
		{Name: "postgres", Critical: true, Check: pool.Ping},
	}
	return m, nil
}
