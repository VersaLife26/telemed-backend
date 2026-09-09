package payment

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"telemed/internal/platform/config"
)

// Settings is the service configuration: the platform base plus everything
// specific to money.
//
// Every secret here is read from the environment and never logged, never
// returned by an endpoint, and never written to the outbox. logger.Redact
// already scrubs the key names used below if one ever reaches a log map by
// accident.
type Settings struct {
	config.Base `mapstructure:",squash"`

	// --- rails ---------------------------------------------------------
	DefaultProvider string `mapstructure:"payment_default_provider"`

	StripeSecretKey     string        `mapstructure:"stripe_secret_key"`
	StripeWebhookSecret string        `mapstructure:"stripe_webhook_secret"`
	StripeTolerance     time.Duration `mapstructure:"stripe_webhook_tolerance"`
	// StripeStrictAPIVersion makes stripe-go reject an event whose API version
	// is from a different release train than the SDK. Off by default; see the
	// note in the Stripe provider for why.
	StripeStrictAPIVersion bool `mapstructure:"stripe_strict_api_version"`

	PayHereMerchantID     string `mapstructure:"payhere_merchant_id"`
	PayHereMerchantSecret string `mapstructure:"payhere_merchant_secret"`
	PayHereAppID          string `mapstructure:"payhere_app_id"`
	PayHereAppSecret      string `mapstructure:"payhere_app_secret"`
	PayHereBaseURL        string `mapstructure:"payhere_base_url"`
	PayHereNotifyURL      string `mapstructure:"payhere_notify_url"`
	PayHereReturnURL      string `mapstructure:"payhere_return_url"`
	PayHereCancelURL      string `mapstructure:"payhere_cancel_url"`

	DialogApplicationID string        `mapstructure:"dialog_application_id"`
	DialogPassword      string        `mapstructure:"dialog_password"`
	DialogBaseURL       string        `mapstructure:"dialog_base_url"`
	DialogWebhookSecret string        `mapstructure:"dialog_webhook_secret"`
	DialogAllowUnsigned bool          `mapstructure:"dialog_allow_unsigned_webhooks"`
	DialogTolerance     time.Duration `mapstructure:"dialog_webhook_tolerance"`
	// DialogWebhookCIDRs is the carrier's egress ranges, comma separated. It
	// is the compensating control for a rail that signs nothing natively, so
	// it is required whenever the Dialog rail is enabled -- there is no
	// "unset means allow everything" here, because that is precisely how a
	// documented security control becomes decoration. `0.0.0.0/0,::/0` is the
	// explicit, visible opt-out for local development.
	DialogWebhookCIDRs string `mapstructure:"dialog_webhook_cidrs"`

	EnableMock        bool   `mapstructure:"payment_enable_mock"`
	MockWebhookSecret string `mapstructure:"mock_webhook_secret"`
	MockAutoSucceed   bool   `mapstructure:"mock_auto_succeed"`
	MockFeeBps        int    `mapstructure:"mock_fee_bps"`

	// --- commission ----------------------------------------------------
	// CommissionRules is the first-boot seed only. After the first boot the
	// database is authoritative and editing this does nothing.
	CommissionRules string        `mapstructure:"commission_rules"`
	CommissionTTL   time.Duration `mapstructure:"commission_cache_ttl"`
	FallbackRateBps int           `mapstructure:"commission_fallback_bps"`
	FallbackFeeBps  int           `mapstructure:"commission_fallback_fee_bps"`

	// --- payouts -------------------------------------------------------
	PayoutSchedule   string        `mapstructure:"payout_schedule"`
	PayoutHoldPeriod time.Duration `mapstructure:"payout_hold_period"`
	PayoutProvider   string        `mapstructure:"payout_provider"`
	PayoutMaxDoctors int           `mapstructure:"payout_max_doctors_per_run"`
	PayoutEnabled    bool          `mapstructure:"payout_scheduler_enabled"`
	// PayoutLeaseTTL bounds how long one replica may hold the settlement
	// lease. It must comfortably exceed a real run: a lease that expires
	// mid-run readmits a second replica to the very concurrency it exists to
	// remove.
	PayoutLeaseTTL time.Duration `mapstructure:"payout_lease_ttl"`
	// PayoutAccounts maps doctor UUIDs to rail account handles, as JSON:
	// {"<doctor-uuid>":"acct_1234"}. It is the settlement destination lookup
	// until doctor-service exposes one.
	PayoutAccounts string `mapstructure:"payout_accounts"`

	// TrustedProxies is the comma-separated CIDR list whose X-Forwarded-For we
	// believe. It matters more here than in most services: the webhook routes
	// are unauthenticated and rate limited by client IP, so a forgeable IP is a
	// bypassable limiter. Empty means the private ranges only -- never the
	// public internet.
	TrustedProxies string `mapstructure:"trusted_proxies"`

	// --- promotions ----------------------------------------------------
	// PromoReservationTTL is how long an applied-but-unpaid code is held for
	// the patient. It is also the ceiling on how stale a quote the rail can
	// be handed, because the hold is refreshed on every intent creation.
	PromoReservationTTL time.Duration `mapstructure:"promo_reservation_ttl"`
	PromoSweepInterval  time.Duration `mapstructure:"promo_sweep_interval"`

	// --- carrier billing ------------------------------------------------
	// PINChallengeTTL must not exceed the carrier's own PIN lifetime, or the
	// platform accepts a code the carrier has already forgotten and fails at
	// the debit instead of at the door. Ideamart documents five minutes.
	PINChallengeTTL time.Duration `mapstructure:"pin_challenge_ttl"`
	PINMaxAttempts  int           `mapstructure:"pin_max_attempts"`
	// PINMaxResends bounds how many PIN challenges may be opened against one
	// payment inside PINResendWindow. MaxAttempts is per challenge and a
	// challenge is free to reopen, so without this the 3-guess allowance
	// against a 4-digit PIN multiplies -- and every reopen drives a real
	// carrier SMS to a number taken from the request body.
	PINMaxResends   int           `mapstructure:"pin_max_resends"`
	PINResendWindow time.Duration `mapstructure:"pin_resend_window"`

	// --- saved payment methods ------------------------------------------
	// VaultEnabled turns the saved-card surface on. Off by default: a
	// deployment with no card rail should answer 501 rather than 500.
	VaultEnabled  bool   `mapstructure:"payment_methods_enabled"`
	VaultProvider string `mapstructure:"payment_methods_provider"`

	// --- invoices ------------------------------------------------------
	InvoiceCompany string `mapstructure:"invoice_company_name"`
	InvoiceAddress string `mapstructure:"invoice_address"`
	InvoiceTaxID   string `mapstructure:"invoice_tax_id"`
	InvoiceEmail   string `mapstructure:"invoice_email"`
	InvoicePhone   string `mapstructure:"invoice_phone"`
}

// ApplyDefaults registers this service's defaults and env bindings on the
// viper instance returned by config.New.
func ApplyDefaults(v *viper.Viper) {
	v.SetDefault("service_name", "telemed-payment-service")
	v.SetDefault("http_port", 8085)
	v.SetDefault("grpc_port", 9095)

	v.SetDefault("payment_default_provider", string(ProviderStripe))
	v.SetDefault("stripe_webhook_tolerance", 5*time.Minute)
	v.SetDefault("stripe_strict_api_version", false)
	v.SetDefault("payhere_base_url", "https://sandbox.payhere.lk")
	v.SetDefault("dialog_base_url", "https://api.ideamart.io")
	v.SetDefault("dialog_webhook_tolerance", 5*time.Minute)
	v.SetDefault("dialog_allow_unsigned_webhooks", false)

	v.SetDefault("payment_enable_mock", false)
	v.SetDefault("mock_auto_succeed", false)
	v.SetDefault("mock_fee_bps", 0)

	v.SetDefault("commission_cache_ttl", 5*time.Minute)
	// 20% is the rate the platform documentation names for a GP consultation.
	v.SetDefault("commission_fallback_bps", 2000)
	// Stripe's Sri Lanka card rate is around 3%. It is a modelled figure; the
	// real fee overrides it whenever the rail reports one.
	v.SetDefault("commission_fallback_fee_bps", 300)
	v.SetDefault("commission_rules", `[{"default":true,"commission":20,"provider_fee":3}]`)

	v.SetDefault("payout_schedule", "0 2 * * *")
	v.SetDefault("payout_hold_period", 24*time.Hour)
	v.SetDefault("payout_max_doctors_per_run", 500)
	v.SetDefault("payout_scheduler_enabled", true)
	v.SetDefault("payout_lease_ttl", 15*time.Minute)
	v.SetDefault("payout_accounts", "{}")

	v.SetDefault("promo_reservation_ttl", 30*time.Minute)
	v.SetDefault("promo_sweep_interval", 5*time.Minute)

	v.SetDefault("pin_challenge_ttl", 5*time.Minute)
	v.SetDefault("pin_max_attempts", 3)
	v.SetDefault("pin_max_resends", 5)
	v.SetDefault("pin_resend_window", time.Hour)

	v.SetDefault("payment_methods_enabled", false)
	v.SetDefault("payment_methods_provider", string(ProviderStripe))

	v.SetDefault("trusted_proxies", "")

	v.SetDefault("invoice_company_name", "Telemed Lanka (Pvt) Ltd")
	v.SetDefault("invoice_address", "Colombo, Sri Lanka")

	for _, k := range []string{
		"payment_default_provider",
		"stripe_secret_key", "stripe_webhook_secret", "stripe_webhook_tolerance", "stripe_strict_api_version",
		"payhere_merchant_id", "payhere_merchant_secret", "payhere_app_id", "payhere_app_secret",
		"payhere_base_url", "payhere_notify_url", "payhere_return_url", "payhere_cancel_url",
		"dialog_application_id", "dialog_password", "dialog_base_url", "dialog_webhook_secret",
		"dialog_allow_unsigned_webhooks", "dialog_webhook_tolerance", "dialog_webhook_cidrs",
		"payment_enable_mock", "mock_webhook_secret", "mock_auto_succeed", "mock_fee_bps",
		"commission_rules", "commission_cache_ttl", "commission_fallback_bps", "commission_fallback_fee_bps",
		"payout_schedule", "payout_hold_period", "payout_provider", "payout_max_doctors_per_run",
		"payout_scheduler_enabled", "payout_accounts", "payout_lease_ttl",
		"promo_reservation_ttl", "promo_sweep_interval",
		"pin_challenge_ttl", "pin_max_attempts", "pin_max_resends", "pin_resend_window",
		"payment_methods_enabled", "payment_methods_provider",
		"trusted_proxies",
		"invoice_company_name", "invoice_address", "invoice_tax_id", "invoice_email", "invoice_phone",
	} {
		_ = v.BindEnv(k)
	}
}

// Validate checks the settings that must be right before the first patient
// tries to pay. It fails at boot rather than at the till.
func (s Settings) Validate() error {
	if err := s.Base.Validate(); err != nil {
		return err
	}
	def := ProviderName(s.DefaultProvider)
	if !def.Valid() {
		return fmt.Errorf("config: PAYMENT_DEFAULT_PROVIDER %q is not one of stripe, payhere, dialog, mock", s.DefaultProvider)
	}
	if def == ProviderMock && !s.EnableMock {
		return fmt.Errorf("config: PAYMENT_DEFAULT_PROVIDER is mock but PAYMENT_ENABLE_MOCK is false")
	}
	if s.EnableMock && s.IsProd() {
		return fmt.Errorf("config: PAYMENT_ENABLE_MOCK must never be set in a production environment")
	}
	if s.FallbackRateBps < 0 || s.FallbackRateBps > BasisPointDenominator {
		return fmt.Errorf("config: COMMISSION_FALLBACK_BPS %d is outside 0-10000", s.FallbackRateBps)
	}
	if _, err := ParseSeedRules(s.CommissionRules, time.Now()); err != nil {
		return err
	}
	if _, err := s.ParsePayoutAccounts(); err != nil {
		return err
	}
	if s.PayoutProvider != "" && !ProviderName(s.PayoutProvider).Valid() {
		return fmt.Errorf("config: PAYOUT_PROVIDER %q is not a known rail", s.PayoutProvider)
	}
	if s.PayoutLeaseTTL <= time.Minute {
		return fmt.Errorf("config: PAYOUT_LEASE_TTL %s is too short; a lease that expires mid-run "+
			"puts a second replica into the settlement job it exists to keep out", s.PayoutLeaseTTL)
	}
	if s.PINMaxResends <= 0 || s.PINMaxResends > 20 {
		return fmt.Errorf("config: PIN_MAX_RESENDS must be between 1 and 20, got %d -- each resend is a real "+
			"carrier SMS to a caller-supplied number and resets the guess counter", s.PINMaxResends)
	}
	if s.PINMaxAttempts <= 0 || s.PINMaxAttempts > 10 {
		return fmt.Errorf("config: PIN_MAX_ATTEMPTS %d is outside 1-10; a four-digit PIN with a "+
			"generous allowance is a brute-force target", s.PINMaxAttempts)
	}
	if s.PINChallengeTTL <= 0 || s.PINChallengeTTL > 15*time.Minute {
		return fmt.Errorf("config: PIN_CHALLENGE_TTL %s is outside 1s-15m; the carrier's own PIN "+
			"lifetime is about five minutes and accepting one it has forgotten fails at the debit", s.PINChallengeTTL)
	}
	if err := s.validateDialog(); err != nil {
		return err
	}
	if s.PromoReservationTTL <= 0 {
		return fmt.Errorf("config: PROMO_RESERVATION_TTL must be positive")
	}
	if s.VaultEnabled && !ProviderName(s.VaultProvider).Valid() {
		return fmt.Errorf("config: PAYMENT_METHODS_PROVIDER %q is not a known rail", s.VaultProvider)
	}
	return nil
}

// validateDialog refuses to boot a carrier-billing rail that nothing
// authenticates.
//
// Ideamart does not sign its callbacks. Our HMAC is optional by configuration
// and the IP allowlist is the documented fallback, so exactly one of two
// things has to be true at boot: a webhook secret is configured, or the
// carrier's egress ranges are. Neither means POST /webhooks/dialog will mark
// any appointment paid for anyone who can reach it -- and the callback body
// needs only a reference the patient's own intent response already handed
// them.
//
// This fails at boot rather than at the till, which is the whole point of
// having a Validate.
func (s Settings) validateDialog() error {
	if strings.TrimSpace(s.DialogApplicationID) == "" {
		return nil // the rail is not enabled; nothing to protect
	}
	if len(s.DialogCIDRs()) == 0 {
		return fmt.Errorf("config: DIALOG_WEBHOOK_CIDRS must list the carrier's egress ranges when " +
			"DIALOG_APPLICATION_ID is set. Ideamart signs nothing natively, so the network is the " +
			"compensating control the design depends on; set it to 0.0.0.0/0,::/0 to opt out explicitly " +
			"in development")
	}
	if s.DialogAllowUnsigned && s.IsProd() {
		return fmt.Errorf("config: DIALOG_ALLOW_UNSIGNED_WEBHOOKS must never be set in production; " +
			"an unsigned carrier callback marks a consultation paid on the strength of a reference the " +
			"patient was already given")
	}
	if s.DialogAllowUnsigned && strings.TrimSpace(s.DialogWebhookSecret) != "" {
		return fmt.Errorf("config: DIALOG_ALLOW_UNSIGNED_WEBHOOKS is set while DIALOG_WEBHOOK_SECRET is " +
			"configured; a configured secret is always required, so unset one of them")
	}
	return nil
}

// DialogCIDRs splits the carrier egress allowlist.
func (s Settings) DialogCIDRs() []string {
	return splitCIDRs(s.DialogWebhookCIDRs)
}

// ProxyCIDRs splits the trusted-proxy list.
func (s Settings) ProxyCIDRs() []string { return splitCIDRs(s.TrustedProxies) }

func splitCIDRs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParsePayoutAccounts decodes the doctor -> account handle map.
func (s Settings) ParsePayoutAccounts() (StaticDestinations, error) {
	raw := strings.TrimSpace(s.PayoutAccounts)
	if raw == "" || raw == "{}" {
		return StaticDestinations{}, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("config: PAYOUT_ACCOUNTS is not a JSON object of doctor_id -> account: %w", err)
	}
	out := make(StaticDestinations, len(m))
	for k, v := range m {
		id, err := uuid.Parse(k)
		if err != nil {
			return nil, fmt.Errorf("config: PAYOUT_ACCOUNTS key %q is not a UUID", k)
		}
		out[id] = v
	}
	return out, nil
}

// FallbackRule is the rule used only when the database has no matching row.
// It exists so a pricing lookup fails closed at a sane rate rather than open at
// zero commission.
func (s Settings) FallbackRule() CommissionRule {
	return CommissionRule{
		ID:             uuid.Nil,
		RuleKey:        "fallback",
		Version:        0,
		Scope:          ScopeDefault,
		RateBps:        s.FallbackRateBps,
		ProviderFeeBps: s.FallbackFeeBps,
		Rounding:       DefaultRounding,
		Note:           "configuration fallback; no database rule matched",
	}
}
