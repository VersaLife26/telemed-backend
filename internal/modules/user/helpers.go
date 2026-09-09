package user

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"telemed/internal/domain/user/user"
	"telemed/internal/platform/config"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/servicetoken"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-user-service"

// serviceConfig extends the shared config.Base with the fields this service
// owns: SMS delivery, JWT signing, the Keycloak identity-mirror admin
// connection, and the PDPA erasure reaper's cadence.
type serviceConfig struct {
	config.Base `mapstructure:",squash"`

	SMSProvider string `mapstructure:"sms_provider"` // dev | dialog | twilio

	DialogBaseURL       string `mapstructure:"dialog_base_url"`
	DialogApplicationID string `mapstructure:"dialog_application_id"`
	DialogPassword      string `mapstructure:"dialog_password"`
	DialogSourceAddress string `mapstructure:"dialog_source_address"`

	TwilioAccountSID string `mapstructure:"twilio_account_sid"`
	TwilioAuthToken  string `mapstructure:"twilio_auth_token"`
	TwilioFromNumber string `mapstructure:"twilio_from_number"`

	// NICHashPepper keys the HMAC that replaces the old bcrypt nic_hash. It is
	// held OUTSIDE the database on purpose: a Sri Lankan NIC encodes its own
	// birth date, family_members stores the plaintext DOB in the same row, and
	// the resulting search space is small enough that no work factor helps.
	// See internal/user/nic.go.
	NICHashPepper string `mapstructure:"nic_hash_pepper"`

	// NICHashPepperVersion is stamped into nic_hash_version on every digest
	// this process writes. Bump it in the same change that replaces
	// NICHashPepper, never separately: the column's whole purpose is that a
	// digest names the key that produced it.
	NICHashPepperVersion int `mapstructure:"nic_hash_pepper_version"`

	// NICHashPepperPrevious holds superseded peppers for the rotation window,
	// as a comma-separated list of "<version>:<pepper>". Verification only --
	// nothing is ever written under a retired pepper. Without it, rotating
	// NICHashPepper makes every previously stored digest unverifiable on the
	// spot; with it, old rows keep working until they are rewritten.
	NICHashPepperPrevious string `mapstructure:"nic_hash_pepper_previous"`

	JWTPrivateKeyPEM string `mapstructure:"jwt_private_key_pem"`
	JWTKeyID         string `mapstructure:"jwt_key_id"`
	JWTIssuer        string `mapstructure:"jwt_issuer"`
	JWTAudience      string `mapstructure:"jwt_audience"`

	KeycloakBaseURL           string `mapstructure:"keycloak_base_url"`
	KeycloakRealm             string `mapstructure:"keycloak_realm"`
	KeycloakAdminClientID     string `mapstructure:"keycloak_admin_client_id"`
	KeycloakAdminClientSecret string `mapstructure:"keycloak_admin_client_secret"`

	ErasureIntervalMinutes int `mapstructure:"erasure_interval_minutes"`
	ErasureBatchSize       int `mapstructure:"erasure_batch_size"`

	// TrustedProxies are the CIDRs whose X-Forwarded-For / X-Real-IP headers
	// this service believes. Everything else has its client address taken
	// from the TCP peer, which no header can forge. Empty means the private
	// ranges only. This is a security control, not a convenience: the OTP
	// rate limiter buckets on the resolved client IP, and a spoofable value
	// would let one attacker mint unlimited OTP requests.
	TrustedProxies []string `mapstructure:"trusted_proxies"`

	// GoogleClientID is the OAuth client ID of the web apps. It is the
	// audience every Google ID token must carry. Empty disables Google
	// sign-in without affecting phone OTP or email/password.
	GoogleClientID string `mapstructure:"google_client_id"`

	// DoctorServiceURL is the doctor-service HTTP base used during OTP
	// verify to look up / attach approved public applications. Empty
	// disables doctor promotion (patient OTP still works).
	DoctorServiceURL string `mapstructure:"doctor_service_url"`

	MeshClientID     string `mapstructure:"mesh_client_id"`
	MeshClientSecret string `mapstructure:"mesh_client_secret"`
	MeshTokenURL     string `mapstructure:"mesh_token_url"`
}

// buildTrustedProxies parses TRUSTED_PROXIES. Returning nil hands
// server.New the platform default (private ranges only), which is the right
// answer for a docker-compose or single-ingress deployment. A malformed entry
// is logged and skipped rather than fatal -- one typo in a config map must not
// stop a pod starting -- but it is never treated as a wildcard.
func buildTrustedProxies(cidrs []string, log zerolog.Logger) *middleware.TrustedProxies {
	if len(cidrs) == 0 {
		return nil
	}
	tp, malformed := middleware.NewTrustedProxies(cidrs)
	for _, m := range malformed {
		log.Error().Str("cidr", m).Msg("ignoring malformed TRUSTED_PROXIES entry")
	}
	return tp
}

// buildGRPCAuthenticator builds the verifier the gRPC interceptor uses, and
// deliberately returns nil rather than an error when it cannot.
//
// Two constraints pull in opposite directions here and both have to hold.
//
//  1. This service must boot without Keycloak. Phone-OTP login is the one
//     authentication path on the platform that survives an identity-provider
//     outage (ADR-010), and it does so precisely because this binary verifies
//     its own tokens in-process and never fetches a JWKS to serve HTTP. Making
//     a JWKS fetch fatal at boot would hand Keycloak the power to stop every
//     patient reaching a doctor -- the exact dependency ADR-010 exists to
//     remove.
//  2. The gRPC surface must fail closed. It carries the PII directory, so
//     "authentication is misconfigured" can never mean "serve it anyway".
//
// Returning nil satisfies both: HTTP keeps working, and
// middleware.UnaryServiceAuth answers every gRPC call with UNAVAILABLE while
// the authenticator is absent. The failure is loud in the log rather than
// silent in the traffic.
//
// Only the Keycloak issuer is bound here, and that is not a shortcut. The
// interceptor requires middleware.RoleService, and this service structurally
// cannot mint that role: token.go copies users.role verbatim and the column's
// CHECK constraint has no 'service' value. Its own key set could therefore
// never produce a token this interceptor would accept, and pointing the
// authenticator at USER_JWKS_URL would additionally have it fetch its own
// not-yet-listening HTTP port during boot.
func buildGRPCAuthenticator(ctx context.Context, cfg serviceConfig, log zerolog.Logger) *middleware.Authenticator {
	if cfg.KeycloakIssuer == "" || cfg.KeycloakJWKSURL == "" {
		log.Error().Msg("KEYCLOAK_ISSUER/KEYCLOAK_JWKS_URL are unset: the gRPC surface will REFUSE every call (fail closed)")
		return nil
	}
	auth, err := middleware.NewAuthenticatorFrom(ctx, middleware.AuthConfig{
		IssuerKeys: map[string]string{cfg.KeycloakIssuer: cfg.KeycloakJWKSURL},
		Issuers:    []string{cfg.KeycloakIssuer},
		Audience:   cfg.KeycloakAudience,
	})
	if err != nil {
		log.Error().Err(err).
			Msg("could not load the service-token key set: the gRPC surface will REFUSE every call (fail closed); HTTP and OTP login are unaffected")
		return nil
	}
	return auth
}

// buildSMSProvider selects the SMSProvider from config. It refuses to fall
// back to the dev (console-logging) provider in prod: an OTP that is never
// actually delivered is a silent, total authentication outage, not a
// degraded mode worth accepting quietly.
func buildSMSProvider(cfg serviceConfig, log zerolog.Logger) (user.SMSProvider, error) {
	switch {
	case cfg.SMSProvider == "dialog" && cfg.DialogApplicationID != "":
		log.Info().Msg("sms provider: dialog ideamart")
		return user.NewDialogSMSProvider(user.DialogConfig{
			BaseURL:       cfg.DialogBaseURL,
			ApplicationID: cfg.DialogApplicationID,
			Password:      cfg.DialogPassword,
			SourceAddress: cfg.DialogSourceAddress,
		}), nil
	case cfg.SMSProvider == "twilio" && cfg.TwilioAccountSID != "":
		log.Info().Msg("sms provider: twilio")
		return user.NewTwilioSMSProvider(user.TwilioConfig{
			AccountSID: cfg.TwilioAccountSID,
			AuthToken:  cfg.TwilioAuthToken,
			FromNumber: cfg.TwilioFromNumber,
		}), nil
	case cfg.SMSProvider == "dev":
		log.Warn().Msg("sms provider: dev (OTPs print to stdout). Explicit SMS_PROVIDER=dev — not for real patients")
		return user.NewDevSMSProvider(log), nil
	default:
		if cfg.IsProd() {
			return nil, fmt.Errorf("no SMS provider configured for prod: set SMS_PROVIDER=dialog|twilio with credentials")
		}
		log.Info().Msg("sms provider: dev (no credentials configured; OTPs print to the console)")
		return user.NewDevSMSProvider(log), nil
	}
}

// buildKeycloakClient attempts to connect to Keycloak's admin API. Any
// failure -- unreachable, misconfigured, or simply not set up yet, which is
// normal in dev -- degrades to a no-op mirror rather than failing startup.
// Registration and login proceed on this service's own JWTs either way.
func buildKeycloakClient(ctx context.Context, cfg serviceConfig, log zerolog.Logger) user.KeycloakClient {
	if cfg.KeycloakAdminClientID == "" || cfg.KeycloakBaseURL == "" {
		log.Info().Msg("keycloak admin credentials not configured; identity mirror degraded")
		return user.NewDegradedKeycloakClient(log)
	}
	gc, err := user.NewGocloakClient(ctx, user.GocloakConfig{
		BaseURL:      cfg.KeycloakBaseURL,
		Realm:        cfg.KeycloakRealm,
		ClientID:     cfg.KeycloakAdminClientID,
		ClientSecret: cfg.KeycloakAdminClientSecret,
	})
	if err != nil {
		log.Warn().Err(err).Msg("keycloak unreachable at boot; degrading identity mirror rather than crash-looping")
		return user.NewDegradedKeycloakClient(log)
	}
	log.Info().Msg("keycloak identity mirror connected")
	return gc
}

func buildDoctorApplications(cfg serviceConfig, log zerolog.Logger) user.DoctorApplications {
	if strings.TrimSpace(cfg.DoctorServiceURL) == "" {
		return nil
	}
	tokenURL := cfg.MeshTokenURL
	if tokenURL == "" && cfg.KeycloakBaseURL != "" {
		tokenURL = strings.TrimSuffix(cfg.KeycloakBaseURL, "/") +
			"/realms/" + cfg.KeycloakRealm + "/protocol/openid-connect/token"
	}
	src, err := servicetoken.New(servicetoken.Config{
		TokenURL:     tokenURL,
		ClientID:     cfg.MeshClientID,
		ClientSecret: cfg.MeshClientSecret,
		RequireTLS:   false, // mesh token fetch may be HTTP inside the compose network
	})
	if err != nil {
		log.Warn().Err(err).Msg("doctor application client unavailable")
		return nil
	}
	return user.NewHTTPDoctorApplications(cfg.DoctorServiceURL, src)
}

// parseNICPepperPrevious parses NIC_HASH_PEPPER_PREVIOUS, a comma-separated
// list of "<version>:<pepper>" pairs holding superseded NIC peppers.
//
// A malformed value is a hard error rather than a skipped entry. Silently
// ignoring one would mean the rows written under that generation stop
// verifying, which is the exact silent failure the version column was added to
// eliminate -- reintroducing it in the parser would be a poor joke.
//
// The pepper itself may contain anything except a comma, so the split is on
// the FIRST colon only; a base64 pepper containing ':' still parses.
func parseNICPepperPrevious(raw string) (map[int]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := make(map[int]string)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		version, pepper, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("NIC_HASH_PEPPER_PREVIOUS entry %d is not \"<version>:<pepper>\"", len(out)+1)
		}
		n, err := strconv.Atoi(strings.TrimSpace(version))
		if err != nil {
			return nil, fmt.Errorf("NIC_HASH_PEPPER_PREVIOUS has a non-numeric version %q", version)
		}
		if _, dup := out[n]; dup {
			return nil, fmt.Errorf("NIC_HASH_PEPPER_PREVIOUS names version %d twice", n)
		}
		out[n] = pepper
	}
	return out, nil
}
