// Command server is the entrypoint template every telemed Go service follows.
//
// The boot sequence is intentionally rigid and identical across services:
//
//	config -> logger -> metrics -> tracing -> postgres -> redis -> nats
//	  -> repositories -> services -> handlers -> routes -> background workers
//	  -> listen -> drain
//
// An operator debugging service #7 at 3am should recognise the shape of
// service #2 immediately.
package consultation

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/viper"

	"telemed/internal/domain/consultation/consultation"
	"telemed/internal/domain/consultation/signal"
	"telemed/internal/platform/config"
	"telemed/internal/platform/modular"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-consultation-service"

// Config layers this service's own settings on top of the shared base every
// telemed service starts from.
type Config struct {
	config.Base `mapstructure:",squash"`

	// VideoProvider selects the VideoProvider implementation: "livekit" or
	// "mock". "mock" needs no LiveKit server running -- the default so this
	// service is runnable and testable with nothing but Postgres/Redis/NATS.
	VideoProvider string `mapstructure:"video_provider"`

	LiveKitURL              string        `mapstructure:"livekit_url"`
	LiveKitAPIKey           string        `mapstructure:"livekit_api_key"`
	LiveKitAPISecret        string        `mapstructure:"livekit_api_secret"`
	LiveKitTokenTTL         time.Duration `mapstructure:"livekit_token_ttl"`
	LiveKitEmptyRoomTimeout int           `mapstructure:"livekit_empty_room_timeout"`

	RecordingBucket        string `mapstructure:"recording_bucket"`
	MinIOEndpoint          string `mapstructure:"minio_endpoint"`
	MinIOAccessKey         string `mapstructure:"minio_access_key"`
	MinIOSecretKey         string `mapstructure:"minio_secret_key"`
	MinIOUseSSL            bool   `mapstructure:"minio_use_ssl"`
	RecordingRetentionDays int    `mapstructure:"recording_retention_days"`

	DefaultConsultationDurationSeconds int `mapstructure:"default_consultation_duration_seconds"`
	QualityDegradeThreshold            int `mapstructure:"quality_degrade_threshold"`

	// StaleSweep* drive the backstop that ends consultations left in 'active'
	// because LiveKit's room_finished webhook never arrived. See
	// internal/consultation/sweeper.go for why the idle timeout is what it is.
	// Setting the interval to 0 disables the sweeper entirely, which is only
	// correct in a test harness.
	StaleSweepInterval time.Duration `mapstructure:"stale_sweep_interval"`
	StaleIdleTimeout   time.Duration `mapstructure:"stale_idle_timeout"`
	StaleSweepBatch    int           `mapstructure:"stale_sweep_batch"`

	// TrustedProxies are the CIDRs whose X-Forwarded-For / X-Real-IP headers
	// this service believes. Requests from anywhere else have their client
	// address taken from the TCP peer, which no header can forge. Empty means
	// the private ranges only. The rate limiter and every audit line read this
	// address, so it is a security control rather than a convenience.
	TrustedProxies []string `mapstructure:"trusted_proxies"`
}

// registerConsultationDefaults adds this service's own configuration keys to
// the shared viper instance. config.New only knows about the base keys every
// service shares; each service is responsible for its own on top, and for
// binding every key it intends to read (viper.AutomaticEnv does not discover
// env-only keys on its own).
func registerConsultationDefaults(v *viper.Viper) {
	v.SetDefault("video_provider", "mock")
	v.SetDefault("livekit_url", "ws://localhost:7880")
	v.SetDefault("livekit_api_key", "devkey")
	v.SetDefault("livekit_api_secret", "devsecret1234567890devsecret1234567890")
	v.SetDefault("livekit_token_ttl", 5*time.Minute)
	v.SetDefault("livekit_empty_room_timeout", 300)

	v.SetDefault("recording_bucket", "recordings")
	v.SetDefault("minio_endpoint", "localhost:9000")
	v.SetDefault("minio_access_key", "minioadmin")
	v.SetDefault("minio_secret_key", "minioadmin")
	v.SetDefault("minio_use_ssl", false)
	v.SetDefault("recording_retention_days", 2555) // 7 years, see docs/DESIGN.md retention policy

	v.SetDefault("default_consultation_duration_seconds", 900)
	v.SetDefault("quality_degrade_threshold", 3)

	v.SetDefault("stale_sweep_interval", consultation.DefaultSweepInterval)
	v.SetDefault("stale_idle_timeout", consultation.DefaultIdleTimeout)
	v.SetDefault("stale_sweep_batch", consultation.DefaultSweepBatch)

	for _, key := range []string{
		"video_provider",
		"livekit_url", "livekit_api_key", "livekit_api_secret", "livekit_token_ttl", "livekit_empty_room_timeout",
		"recording_bucket", "minio_endpoint", "minio_access_key", "minio_secret_key", "minio_use_ssl", "recording_retention_days",
		"default_consultation_duration_seconds", "quality_degrade_threshold",
		"stale_sweep_interval", "stale_idle_timeout", "stale_sweep_batch",
		"trusted_proxies",
	} {
		_ = v.BindEnv(key)
	}
}

// validateVideoConfig fails boot rather than at the first join request when
// VIDEO_PROVIDER=livekit is set without credentials to back it.
func validateVideoConfig(cfg Config) error {
	switch strings.ToLower(strings.TrimSpace(cfg.VideoProvider)) {
	case "inhouse":
	case "", "mock":
		// The mock provider verifies its webhook with a bearer token compared
		// against LIVEKIT_API_SECRET, whose default is published in
		// .env.example -- and if that is unset too it falls back to a literal
		// in mock_provider.go. POST /webhooks/livekit drives the consultation
		// lifecycle: force-ending a call, and writing recording_url straight
		// from the body. In production that is an unauthenticated write to
		// any consultation, reachable by deriving the room name from an
		// appointment id.
		//
		// An unset VIDEO_PROVIDER defaulting to mock is the shape that makes
		// this a live risk rather than a theoretical one, so an empty value is
		// refused in prod alongside an explicit "mock". payment-service already
		// does this for its own mock rail (mock.Guard).
		if cfg.IsProd() {
			return fmt.Errorf("config: VIDEO_PROVIDER=%q is not permitted in %s; the mock provider's webhook is authenticated by a published default token",
				cfg.VideoProvider, cfg.Env)
		}
	case "livekit":
	default:
		return fmt.Errorf("config: VIDEO_PROVIDER must be \"inhouse\", \"livekit\" or \"mock\", got %q", cfg.VideoProvider)
	}
	if strings.EqualFold(cfg.VideoProvider, "livekit") {
		if cfg.LiveKitURL == "" || cfg.LiveKitAPIKey == "" || cfg.LiveKitAPISecret == "" {
			return fmt.Errorf("config: VIDEO_PROVIDER=livekit requires LIVEKIT_URL, LIVEKIT_API_KEY and LIVEKIT_API_SECRET")
		}
		if err := validateLiveKitURL(cfg.LiveKitURL, cfg.Env); err != nil {
			return err
		}
	}
	if cfg.LiveKitEmptyRoomTimeout < 0 || cfg.LiveKitEmptyRoomTimeout > maxEmptyRoomTimeoutSeconds {
		return fmt.Errorf("config: LIVEKIT_EMPTY_ROOM_TIMEOUT must be between 0 and %d seconds, got %d",
			maxEmptyRoomTimeoutSeconds, cfg.LiveKitEmptyRoomTimeout)
	}
	return nil
}

// validateInHouseConfig refuses the three ways an in-house deployment fails
// silently rather than loudly.
//
// Each of these produces a platform that works perfectly in development, on
// one machine, and on the test page -- and then fails for real consultations
// in a way whose symptom points nowhere near the cause. That is exactly the
// class of problem a boot refusal is for.
func validateInHouseConfig(cfg Config, sig modular.SignalConfig) error {
	if !cfg.IsProd() {
		return nil
	}
	if len(sig.Secret) == 0 {
		return fmt.Errorf("config: VIDEO_PROVIDER=inhouse requires SIGNAL_SECRET in %s: "+
			"an unset secret is generated per process, so a room token minted by one replica "+
			"fails verification in another and roughly half of all calls never connect", cfg.Env)
	}
	if strings.TrimSpace(sig.PublicURL) == "" {
		return fmt.Errorf("config: VIDEO_PROVIDER=inhouse requires SIGNAL_PUBLIC_URL in %s: "+
			"the browser connects to the signalling socket directly and has nothing to resolve "+
			"a relative path against", cfg.Env)
	}
	if !strings.HasPrefix(sig.PublicURL, "wss://") {
		return fmt.Errorf("config: SIGNAL_PUBLIC_URL must be wss:// in %s, got %q: "+
			"the room token travels in the query string", cfg.Env, sig.PublicURL)
	}
	if len(sig.AllowedOrigins) == 0 {
		return fmt.Errorf("config: VIDEO_PROVIDER=inhouse requires SIGNAL_ALLOWED_ORIGINS "+
			"(or CORS_PATIENT_ORIGINS and CORS_DOCTOR_ORIGINS) in %s: CORS does not apply to a "+
			"websocket upgrade, so the Origin check is the only origin control it has", cfg.Env)
	}
	return nil
}

// maxEmptyRoomTimeoutSeconds bounds LIVEKIT_EMPTY_ROOM_TIMEOUT.
//
// LiveKit's field is uint32 seconds. A negative configured value converted
// straight to uint32 becomes ~136 years, which silently disables LiveKit's own
// teardown of an abandoned room -- one of the three things that ends a stale
// consultation. Anything past a day is indistinguishable from "never" for a
// consultation, so the range is closed at both ends and boot fails loudly
// rather than clamping.
const maxEmptyRoomTimeoutSeconds = 86400

// emptyRoomTimeoutSeconds converts the configured value to LiveKit's uint32.
// validateVideoConfig has already refused anything out of range at boot; the
// bounds here make the conversion total regardless, so it cannot wrap.
func emptyRoomTimeoutSeconds(seconds int) uint32 {
	if seconds < 0 {
		return 0
	}
	if seconds > maxEmptyRoomTimeoutSeconds {
		return maxEmptyRoomTimeoutSeconds
	}
	return uint32(seconds)
}

func buildVideoProvider(cfg Config, deps modular.Deps, hub *signal.Hub, bus *signal.Bus) (consultation.VideoProvider, error) {
	switch strings.ToLower(cfg.VideoProvider) {
	case "inhouse":
		return consultation.NewInHouseProvider(consultation.InHouseOptions{
			Hub:       hub,
			Bus:       bus,
			Secret:    deps.Signal.Secret,
			TokenTTL:  deps.Signal.TokenTTL,
			SignalURL: deps.Signal.PublicURL,
			Log:       deps.Log,
		})
	case "", "mock":
		return consultation.NewMockProvider(cfg.LiveKitAPISecret), nil
	case "livekit":
		return consultation.NewLiveKitProvider(
			cfg.LiveKitURL, cfg.LiveKitAPIKey, cfg.LiveKitAPISecret,
			cfg.LiveKitTokenTTL, emptyRoomTimeoutSeconds(cfg.LiveKitEmptyRoomTimeout),
			consultation.RecordingConfig{
				Endpoint:       cfg.MinIOEndpoint,
				UseSSL:         cfg.MinIOUseSSL,
				AccessKey:      cfg.MinIOAccessKey,
				SecretKey:      cfg.MinIOSecretKey,
				ForcePathStyle: true,
			},
		), nil
	default:
		// Unreachable: validateVideoConfig already rejected anything else.
		return nil, fmt.Errorf("config: unknown VIDEO_PROVIDER %q", cfg.VideoProvider)
	}
}

// validateLiveKitURL refuses a cleartext signalling URL outside development.
//
// This value is handed to the patient and doctor apps in the join response, and
// the clients now refuse anything that is not wss:// -- correctly, since the
// LiveKit signalling channel carries the room token. So a cleartext URL here no
// longer degrades quietly to an unencrypted call; it fails every join on every
// device, which is a total consultation outage produced by one environment
// variable.
//
// Failing at boot turns that into an error the operator sees before any patient
// does. The default is ws://localhost:7880 precisely so local development works
// without certificates, which is why dev is exempt rather than the rule being
// dropped.
func validateLiveKitURL(raw, env string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: LIVEKIT_URL is not a valid URL: %w", err)
	}

	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		if env == "dev" || env == "development" || env == "local" || env == "test" {
			return nil
		}
		return fmt.Errorf(
			"config: LIVEKIT_URL must use wss:// outside development (got %q). "+
				"The clients refuse a cleartext signalling URL, so this would fail "+
				"every consultation rather than downgrading them", raw)
	default:
		return fmt.Errorf(
			"config: LIVEKIT_URL must be a ws:// or wss:// URL, got scheme %q", u.Scheme)
	}
}

// signalRoute is the path the websocket is registered on, relative to the
// module's Raw mount (which is served from the server root, not /api/v1).
const signalRoute = "/ws/consultation"

// SignalPath is where the signalling websocket lives.
//
// Exported so the composer can log it and so routetable_contract_test can
// assert it is ABSENT from the gateway route table: it is served raw, ahead of
// the router, and a route-table entry for it would send the upgrade through
// the in-process proxy, whose ResponseWriter cannot be hijacked.
func SignalPath() string { return signalRoute }

// liveKitURLFor returns the LiveKit URL only when LiveKit is the provider.
//
// Empty otherwise, so a client cannot read a stale livekit_url and connect a
// LiveKit SDK to a socket speaking the signalling protocol -- which hangs
// rather than failing, and is much harder to diagnose than an empty field.
func liveKitURLFor(cfg Config) string {
	if strings.EqualFold(strings.TrimSpace(cfg.VideoProvider), "livekit") {
		return cfg.LiveKitURL
	}
	return ""
}

// tokenTTLFor picks the token lifetime for the configured provider.
//
// The two are genuinely different. A LiveKit access token is spent once at
// room.connect(); a signalling room token is re-presented on EVERY websocket
// reconnect, so LIVEKIT_TOKEN_TTL's five minutes would strand any client that
// changed network mid-call. JoinResult.TokenExpiresAt is computed from this,
// so it must be the value actually used or the client is told the wrong thing.
func tokenTTLFor(cfg Config, sig modular.SignalConfig) time.Duration {
	if strings.EqualFold(strings.TrimSpace(cfg.VideoProvider), "inhouse") {
		if sig.TokenTTL > 0 {
			return sig.TokenTTL
		}
		return consultation.DefaultRoomTokenTTL
	}
	return cfg.LiveKitTokenTTL
}

// toDomainICE converts between two field-identical types.
//
// They are separate on purpose: consultation would otherwise import signal for
// a struct, which is the wrong direction for a domain that is meant to depend
// only on its VideoProvider port. The module is the right place for the seam.
func toDomainICE(in []signal.ICEServer) []consultation.ICEServer {
	out := make([]consultation.ICEServer, 0, len(in))
	for _, s := range in {
		out = append(out, consultation.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	return out
}
