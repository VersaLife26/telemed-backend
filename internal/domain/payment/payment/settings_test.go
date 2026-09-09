package payment

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/config"
)

func baseSettings() Settings {
	v := viper.New()
	ApplyDefaults(v)

	var s Settings
	_ = v.Unmarshal(&s)
	s.ServiceName = "telemed-payment-service"
	s.DatabaseURL = "postgres://telemed:telemed@localhost:5433/telemed_payment?sslmode=disable"
	s.HTTPPort = 8085
	s.Timezone = "Asia/Colombo"
	s.Env = "dev"
	return s
}

// TestDefaultsMatchThePlatformPortAllocation guards against the ports drifting
// out of step with the table every service and the compose file share.
func TestDefaultsMatchThePlatformPortAllocation(t *testing.T) {
	t.Parallel()

	v := viper.New()
	ApplyDefaults(v)

	assert.Equal(t, "telemed-payment-service", v.GetString("service_name"))
	assert.Equal(t, 8085, v.GetInt("http_port"))
	assert.Equal(t, 9095, v.GetInt("grpc_port"))
}

func TestSettingsValidate(t *testing.T) {
	t.Parallel()

	t.Run("defaults are valid", func(t *testing.T) {
		require.NoError(t, baseSettings().Validate())
	})

	t.Run("unknown provider is rejected", func(t *testing.T) {
		s := baseSettings()
		s.DefaultProvider = "paypal"
		assert.Error(t, s.Validate())
	})

	t.Run("mock as default requires mock enabled", func(t *testing.T) {
		s := baseSettings()
		s.DefaultProvider = "mock"
		s.EnableMock = false
		assert.Error(t, s.Validate())
		s.EnableMock = true
		assert.NoError(t, s.Validate())
	})

	t.Run("mock is refused in production", func(t *testing.T) {
		s := baseSettings()
		s.Env = "prod"
		s.EnableMock = true
		err := s.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "production")
	})

	t.Run("malformed commission rules are caught at boot", func(t *testing.T) {
		s := baseSettings()
		s.CommissionRules = `[{"commission":20}]` // names no scope
		assert.Error(t, s.Validate())
	})

	t.Run("malformed payout accounts are caught at boot", func(t *testing.T) {
		s := baseSettings()
		s.PayoutAccounts = `{"not-a-uuid":"acct_1"}`
		assert.Error(t, s.Validate())

		s.PayoutAccounts = `["acct_1"]`
		assert.Error(t, s.Validate())
	})

	t.Run("unknown payout rail is rejected", func(t *testing.T) {
		s := baseSettings()
		s.PayoutProvider = "western-union"
		assert.Error(t, s.Validate())
	})

	t.Run("a fallback rate outside 0-100 percent is rejected", func(t *testing.T) {
		s := baseSettings()
		s.FallbackRateBps = 10_001
		assert.Error(t, s.Validate())
	})

	t.Run("an invalid timezone is rejected", func(t *testing.T) {
		s := baseSettings()
		s.Timezone = "Mars/Olympus_Mons"
		assert.Error(t, s.Validate())
	})
}

func TestParsePayoutAccounts(t *testing.T) {
	t.Parallel()

	doctorID := uuid.New()
	s := baseSettings()
	s.PayoutAccounts = `{"` + doctorID.String() + `":"acct_1234"}`

	dest, err := s.ParsePayoutAccounts()
	require.NoError(t, err)

	account, err := dest.PayoutAccount(t.Context(), doctorID)
	require.NoError(t, err)
	assert.Equal(t, "acct_1234", account)

	_, err = dest.PayoutAccount(t.Context(), uuid.New())
	assert.ErrorIs(t, err, ErrNoDestination)
}

func TestFallbackRule(t *testing.T) {
	t.Parallel()

	s := baseSettings()
	rule := s.FallbackRule()
	assert.Equal(t, 2000, rule.RateBps, "20%% is the rate the platform documentation names for a GP consultation")
	assert.Equal(t, 300, rule.ProviderFeeBps)
	assert.Equal(t, DefaultRounding, rule.Rounding)

	split, err := ComputeSplit(500_000, rule)
	require.NoError(t, err)
	assert.True(t, split.Balanced())
}

// TestBaseIsEmbeddedSquashed catches the mapstructure tag going missing, which
// would silently leave DATABASE_URL empty and produce a confusing boot failure.
func TestBaseIsEmbeddedSquashed(t *testing.T) {
	t.Parallel()

	v := viper.New()
	ApplyDefaults(v)
	v.Set("database_url", "postgres://x/y")
	v.Set("payout_hold_period", 6*time.Hour)

	var s Settings
	require.NoError(t, v.Unmarshal(&s))
	assert.Equal(t, "postgres://x/y", s.DatabaseURL, "config.Base must unmarshal through the squash tag")
	assert.Equal(t, 6*time.Hour, s.PayoutHoldPeriod)
	assert.IsType(t, config.Base{}, s.Base)
}

// --- F9: carrier billing must not boot without a compensating control -------

// TestDialogRailRefusesToBootWithoutACompensatingControl.
//
// Dialog Ideamart signs nothing natively. The provider's own comment and the
// boot log both claimed that where signing could not be arranged the endpoint
// fell back to "IP-allowlist-only protection" -- and no IP allowlist existed
// anywhere in this service. `middleware.IPAllowlist` was never called,
// DIALOG_WEBHOOK_CIDRS did not exist, and /webhooks/dialog was mounted with a
// rate limiter and nothing else. The fallback was a sentence in a comment.
//
// So the rail now has a boot-time precondition. These cases are the whole
// decision table.
func TestDialogRailRefusesToBootWithoutACompensatingControl(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		env      string
		appID    string
		secret   string
		unsigned bool
		cidrs    string
		wantErr  string
	}{
		{
			name:  "the rail is not enabled at all, so there is nothing to protect",
			appID: "", cidrs: "",
		},
		{
			name:  "signed and allowlisted",
			appID: "APP", secret: "s3cret", cidrs: "203.94.64.0/18",
		},
		{
			name:  "signed but with no allowlist at all",
			appID: "APP", secret: "s3cret", cidrs: "",
			wantErr: "DIALOG_WEBHOOK_CIDRS",
		},
		{
			name:  "unsigned with an allowlist, in development",
			appID: "APP", unsigned: true, cidrs: "203.94.64.0/18", env: "dev",
		},
		{
			name:  "unsigned with no allowlist is the exact hole in the review",
			appID: "APP", unsigned: true, cidrs: "",
			wantErr: "DIALOG_WEBHOOK_CIDRS",
		},
		{
			name:  "unsigned is never acceptable in production",
			appID: "APP", unsigned: true, cidrs: "203.94.64.0/18", env: "production",
			wantErr: "must never be set in production",
		},
		{
			name:  "a secret AND the unsigned downgrade is the trap that let a caller opt out",
			appID: "APP", secret: "s3cret", unsigned: true, cidrs: "203.94.64.0/18",
			wantErr: "unset one of them",
		},
		{
			name:  "an explicit wide-open allowlist is allowed, because it is visible",
			appID: "APP", secret: "s3cret", cidrs: "0.0.0.0/0,::/0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := baseSettings()
			if tc.env != "" {
				s.Env = tc.env
			}
			s.DialogPayApplicationID = tc.appID
			s.DialogPayWebhookSecret = tc.secret
			s.DialogPayAllowUnsigned = tc.unsigned
			s.DialogPayWebhookCIDRs = tc.cidrs

			err := s.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err, "this configuration must not be allowed to boot")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestDialogCIDRsSplitsAndTrims(t *testing.T) {
	t.Parallel()

	s := baseSettings()
	s.DialogPayWebhookCIDRs = " 203.94.64.0/18 , 2402:4000::/32 ,, "
	assert.Equal(t, []string{"203.94.64.0/18", "2402:4000::/32"}, s.DialogPayCIDRs())

	s.DialogPayWebhookCIDRs = ""
	assert.Empty(t, s.DialogPayCIDRs())
}

// TestPayoutLeaseTTLMustOutliveARun: a lease that expires mid-run readmits a
// second replica to the settlement job, which is the concurrency the lease
// exists to remove.
func TestPayoutLeaseTTLMustOutliveARun(t *testing.T) {
	t.Parallel()

	s := baseSettings()
	assert.Equal(t, 15*time.Minute, s.PayoutLeaseTTL, "the default must be generous, not tight")

	s.PayoutLeaseTTL = 10 * time.Second
	err := s.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PAYOUT_LEASE_TTL")
}
