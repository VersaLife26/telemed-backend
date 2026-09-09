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
package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"telemed/internal/domain/payment/payment"
	dialogprovider "telemed/internal/domain/payment/payment/provider/dialog"
	mockprovider "telemed/internal/domain/payment/payment/provider/mock"
	payhereprovider "telemed/internal/domain/payment/payment/provider/payhere"
	stripeprovider "telemed/internal/domain/payment/payment/provider/stripe"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-payment-service"

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
