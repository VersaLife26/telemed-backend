// Command server is the telemed-payment-service entrypoint.
//
// The boot sequence is the one every telemed service follows, in the same
// order, so an operator debugging this at 3am recognises the shape from the
// other nine:
//
//	config -> logger -> metrics -> tracing -> postgres -> redis -> nats
//	  -> providers -> repositories -> services -> handlers -> routes
//	  -> background workers -> listen -> drain
//
// The one addition here is the commission rule seed, which runs after the pool
// is up and before the first request can be served: a payment service that
// starts without a pricing rule would take money it cannot split.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"telemed/internal/domain/payment/payment"
	dialogprovider "telemed/internal/domain/payment/payment/provider/dialog"
	mockprovider "telemed/internal/domain/payment/payment/provider/mock"
	payhereprovider "telemed/internal/domain/payment/payment/provider/payhere"
	stripeprovider "telemed/internal/domain/payment/payment/provider/stripe"
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

const serviceName = "telemed-payment-service"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- configuration -------------------------------------------------
	v := config.New(serviceName)
	payment.ApplyDefaults(v)

	var cfg payment.Settings
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
	// Two issuers, both trusted: user-service mints patient and doctor tokens
	// so that a phone-OTP login does not depend on Keycloak being up, and
	// Keycloak mints admin tokens. A payment endpoint is reached by both.
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

	// --- payment rails ---------------------------------------------------
	registry, err := buildRegistry(cfg, log)
	if err != nil {
		return err
	}
	log.Info().
		Strs("providers", providerNames(registry)).
		Str("default", string(registry.Default())).
		Msg("payment rails configured")

	// --- repository, pricing, services -----------------------------------
	outbox := events.NewOutbox(cfg.ServiceName)
	repo := payment.NewRepository(pool, outbox)

	if err := seedCommissionRules(ctx, repo, cfg, log); err != nil {
		return err
	}

	pricer := payment.NewPricer(repo, cfg.CommissionTTL, cfg.FallbackRule())
	if err := pricer.Refresh(ctx); err != nil {
		return fmt.Errorf("load commission rules: %w", err)
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
		return err
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
	}).WithLease(redis)

	invoices, err := payment.NewInvoiceRenderer(payment.InvoiceBranding{
		CompanyName: cfg.InvoiceCompany,
		AddressLine: cfg.InvoiceAddress,
		TaxID:       cfg.InvoiceTaxID,
		Email:       cfg.InvoiceEmail,
		Phone:       cfg.InvoicePhone,
		Timezone:    cfg.Timezone,
	})
	if err != nil {
		return err
	}

	handler := payment.NewHandler(svc, payouts, invoices, log)
	webhooks := payment.NewWebhookHandler(svc, log).WithDialogAllowlist(cfg.DialogCIDRs())

	// --- background workers ----------------------------------------------
	relay := events.NewRelay(pool, broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	// Start blocks: it runs the JetStream consumer until ctx is cancelled.
	// Calling it inline here meant srv.Run below was never reached, so the
	// service bound no HTTP port at all and every webhook was unreachable --
	// while the logs looked completely healthy.
	consumer := payment.NewConsumer(svc, broker, log)
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

	// --- http ------------------------------------------------------------
	// Only these networks' X-Forwarded-For is believed. The webhook routes are
	// unauthenticated and rate limited by client IP, so an IP an attacker can
	// choose is a rate limiter an attacker can bypass. nil means the private
	// ranges only, never the public internet.
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
		r.Mount("/webhooks", webhooks.Routes(redis))

		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuth(auth))
			// A per-principal limit on top of the global one. Payment
			// endpoints call out to a rail, so they are the expensive ones
			// to abuse.
			r.Use(middleware.RateLimit(redis, middleware.RateLimitConfig{
				Name:     "payments",
				Requests: 120,
				Window:   time.Minute,
				KeyFunc:  middleware.ByPrincipal,
			}, log))

			r.Mount("/payments", handler.Routes())
			r.Mount("/payouts", handler.PayoutRoutes())
		})
	})

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
}

// buildRegistry constructs every rail the configuration enables.
//
// A rail with no credentials is simply not registered. A request naming it then
// fails with "provider stripe is not configured", which an operator can act on,
// rather than with an authentication error from deep inside an SDK.
func buildRegistry(cfg payment.Settings, log zerolog.Logger) (*payment.Registry, error) {
	reg := payment.NewRegistry(payment.ProviderName(cfg.DefaultProvider))

	if cfg.StripeSecretKey != "" {
		p, err := stripeprovider.New(stripeprovider.Config{
			SecretKey:                cfg.StripeSecretKey,
			WebhookSecret:            cfg.StripeWebhookSecret,
			Tolerance:                cfg.StripeTolerance,
			IgnoreAPIVersionMismatch: !cfg.StripeStrictAPIVersion,
		})
		if err != nil {
			return nil, fmt.Errorf("configure stripe: %w", err)
		}
		reg.Register(p)
	} else {
		log.Warn().Msg("STRIPE_SECRET_KEY is unset; the Stripe rail is not available")
	}

	if cfg.PayHereMerchantID != "" {
		p, err := payhereprovider.New(payhereprovider.Config{
			MerchantID:     cfg.PayHereMerchantID,
			MerchantSecret: cfg.PayHereMerchantSecret,
			AppID:          cfg.PayHereAppID,
			AppSecret:      cfg.PayHereAppSecret,
			BaseURL:        cfg.PayHereBaseURL,
			NotifyURL:      cfg.PayHereNotifyURL,
			ReturnURL:      cfg.PayHereReturnURL,
			CancelURL:      cfg.PayHereCancelURL,
		})
		if err != nil {
			return nil, fmt.Errorf("configure payhere: %w", err)
		}
		reg.Register(p)
	} else {
		log.Warn().Msg("PAYHERE_MERCHANT_ID is unset; the PayHere rail is not available")
	}

	if cfg.DialogApplicationID != "" {
		p, err := dialogprovider.New(dialogprovider.Config{
			ApplicationID: cfg.DialogApplicationID,
			Password:      cfg.DialogPassword,
			BaseURL:       cfg.DialogBaseURL,
			WebhookSecret: cfg.DialogWebhookSecret,
			Tolerance:     cfg.DialogTolerance,
			AllowUnsigned: cfg.DialogAllowUnsigned,
		})
		if err != nil {
			return nil, fmt.Errorf("configure dialog: %w", err)
		}
		reg.Register(p)
		if cfg.DialogAllowUnsigned {
			// The claim in this line used to be false: there was no IP
			// allowlist anywhere in this service. It is true now -- Settings.
			// Validate refuses to boot the rail without DIALOG_WEBHOOK_CIDRS,
			// and /webhooks/dialog fails closed on it -- so the count is
			// logged to make the size of the trusted surface visible.
			log.Warn().Int("allowlisted_cidrs", len(cfg.DialogCIDRs())).
				Msg("DIALOG_ALLOW_UNSIGNED_WEBHOOKS is set; Dialog callbacks are protected only by IP allowlisting")
		}
	} else {
		log.Warn().Msg("DIALOG_APPLICATION_ID is unset; carrier billing is not available")
	}

	if cfg.EnableMock {
		if err := mockprovider.Guard(cfg.Env); err != nil {
			return nil, err
		}
		m := mockprovider.New(mockprovider.Config{
			WebhookSecret: cfg.MockWebhookSecret,
			AutoSucceed:   cfg.MockAutoSucceed,
			FeeBps:        cfg.MockFeeBps,
		})
		reg.Register(m)
		log.Warn().Str("webhook_secret_set", boolLabel(cfg.MockWebhookSecret != "")).
			Msg("mock payment rail enabled; this must never happen in production")
	}

	if len(reg.Names()) == 0 {
		return nil, errors.New("no payment rail is configured; set STRIPE_SECRET_KEY, PAYHERE_MERCHANT_ID, DIALOG_APPLICATION_ID or PAYMENT_ENABLE_MOCK")
	}
	if _, err := reg.Get(""); err != nil {
		return nil, fmt.Errorf("default provider: %w", err)
	}
	return reg, nil
}

// runMaintenance sweeps expired promo reservations and PIN challenges until
// the context is cancelled. Errors are logged and the loop continues: a
// transient database error must not stop the sweeper for the life of the
// process.
func runMaintenance(ctx context.Context, svc *payment.Service, interval time.Duration, log zerolog.Logger) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := svc.SweepExpiredPromoReservations(ctx, 200); err != nil {
				log.Error().Err(err).Msg("promo reservation sweep failed")
			} else if n > 0 {
				log.Info().Int("released", n).Msg("released expired promo reservations")
			}
			if n, err := svc.SweepExpiredPINChallenges(ctx, 200); err != nil {
				log.Error().Err(err).Msg("pin challenge sweep failed")
			} else if n > 0 {
				log.Info().Int("expired", n).Msg("retired unanswered PIN challenges")
			}
		}
	}
}

func boolLabel(b bool) string {
	if b {
		return "configured"
	}
	return "generated"
}

func providerNames(r *payment.Registry) []string {
	names := r.Names()
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return out
}

// seedCommissionRules populates an empty commission_rules table from
// COMMISSION_RULES.
//
// It runs only when the table is empty. After that the database is the sole
// authority: rates change by inserting a new rule version, not by editing an
// environment variable and redeploying, because the latter leaves no record of
// what the rate used to be.
func seedCommissionRules(ctx context.Context, repo *payment.Repository, cfg payment.Settings, log zerolog.Logger) error {
	n, err := repo.CountRules(ctx)
	if err != nil {
		return fmt.Errorf("count commission rules: %w", err)
	}
	if n > 0 {
		log.Info().Int("rules", n).Msg("commission rules present; skipping seed")
		return nil
	}

	rules, err := payment.ParseSeedRules(cfg.CommissionRules, time.Now().UTC())
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		return errors.New("commission_rules table is empty and COMMISSION_RULES seeds nothing; the service cannot price a consultation")
	}
	inserted, err := repo.SeedRules(ctx, rules)
	if err != nil {
		return fmt.Errorf("seed commission rules: %w", err)
	}
	log.Warn().
		Int("inserted", inserted).
		Msg("seeded commission rules from COMMISSION_RULES; subsequent rate changes must be new rule versions in the database")
	return nil
}
