// Command server is the entrypoint for telemed-user-service: identity,
// phone-OTP authentication, sessions, and family profiles.
//
// The boot sequence follows the platform standard (see AGENT-BRIEF.md):
//
//	config -> logger -> metrics -> tracing -> postgres -> redis -> nats
//	  -> repositories -> services -> handlers -> routes -> background workers
//	  -> listen -> drain
//
// One deliberate deviation from the template: this service does not build
// internal/platform/middleware.Authenticator (the shared Keycloak-JWKS
// verifier) for its own routes. user-service is the token *issuer* on the
// OTP path, so it verifies its own tokens in-process against its own
// signing key (user.TokenIssuer.Verify) rather than fetching a JWKS document
// over HTTP -- which would otherwise be a boot-time dependency on Keycloak
// being reachable, precisely what AGENT-BRIEF says this service must not
// have. See README "50-Year Maintenance" for the full rationale.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"

	"telemed/internal/domain/user/user"
	"telemed/internal/platform/cache"
	"telemed/internal/platform/config"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
	"telemed/internal/platform/server"
	"telemed/internal/platform/servicetoken"

	userv1 "telemed/internal/pb/user/v1"
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

func main() {
	if err := run(); err != nil {
		// Write to stderr rather than the structured logger: a failure here
		// may well be the logger itself, or config that never loaded.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- configuration -------------------------------------------------
	v := config.New(serviceName)
	v.SetDefault("sms_provider", "dev")
	v.SetDefault("jwt_key_id", "user-service-1")
	v.SetDefault("jwt_issuer", "telemed-user-service")
	v.SetDefault("jwt_audience", "telemed-api")
	v.SetDefault("keycloak_base_url", "")
	v.SetDefault("keycloak_realm", "telemedicine")
	v.SetDefault("erasure_interval_minutes", 60)
	v.SetDefault("erasure_batch_size", 200)
	v.SetDefault("nic_hash_pepper_version", user.NICPepperVersionCurrent)
	for _, k := range []string{
		"sms_provider",
		"dialog_base_url", "dialog_application_id", "dialog_password", "dialog_source_address",
		"twilio_account_sid", "twilio_auth_token", "twilio_from_number",
		"nic_hash_pepper", "nic_hash_pepper_version", "nic_hash_pepper_previous",
		"jwt_private_key_pem", "jwt_key_id", "jwt_issuer", "jwt_audience",
		"keycloak_base_url", "keycloak_realm", "keycloak_admin_client_id", "keycloak_admin_client_secret",
		"erasure_interval_minutes", "erasure_batch_size",
		"trusted_proxies",
		"google_client_id",
		"doctor_service_url",
		"mesh_client_id", "mesh_client_secret", "mesh_token_url",
	} {
		_ = v.BindEnv(k)
	}

	var cfg serviceConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	// --- logging -------------------------------------------------------
	log := logger.New(cfg.ServiceName, cfg.LogLevel, cfg.Env)
	log.Info().Str("version", Version).Msg("starting service")

	// --- admin-role issuer binding ---------------------------------------
	// An administrative role is honoured only from the admin issuer.
	//
	// ADR-012 stops user-service claiming to BE Keycloak by binding each key
	// set to the issuer that may use it. It does nothing about THIS service
	// minting an entirely honest token -- correct `iss`, correct key,
	// verifying cleanly -- that asserts realm_access.roles = ["super_admin"].
	// Since this is the phone-OTP issuer and Keycloak is where SAML SSO and
	// enforced 2FA live, that is a 2FA bypass, not merely a role escalation
	// (security review F1).
	//
	// The gateway enforces this at the edge, and only for routes declared
	// `auth: "admin"`. This call is what makes middleware.RequireRole enforce
	// it here as well, so a service reached from inside the mesh -- a
	// compromised pod, a NetworkPolicy gap, an SSRF -- is not the soft
	// underbelly. It was already wired in payment-service and missing here.
	//
	// An empty KEYCLOAK_ISSUER leaves the check disabled, which is correct for
	// a single-issuer developer stack and is why this is not a boot failure.
	middleware.SetAdminIssuer(cfg.KeycloakIssuer)

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
		// One database, one schema per domain (migrations/bootstrap). This is
		// what makes an unqualified table name resolve to user's tables and
		// not another domain's -- six table names collide across domains.
		SearchPath: database.Schema("user") + ", public",
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

	// --- signing key -----------------------------------------------------
	// user-service mints its own access tokens (see package doc comment
	// above), so a signing key is mandatory in prod. In dev, a missing key
	// generates an ephemeral one for this process only, so `make run` needs
	// zero setup -- the same philosophy as the dev SMS provider.
	jwtPEM := cfg.JWTPrivateKeyPEM
	if jwtPEM == "" {
		if cfg.IsProd() {
			return fmt.Errorf("JWT_PRIVATE_KEY_PEM is required in %s", cfg.Env)
		}
		log.Warn().Msg("no JWT_PRIVATE_KEY_PEM configured; generating an ephemeral RSA key for this process (dev only -- tokens will not verify after a restart)")
		jwtPEM, err = user.GenerateEphemeralKeyPEM()
		if err != nil {
			return fmt.Errorf("generate ephemeral signing key: %w", err)
		}
	}
	tokens, err := user.NewTokenIssuer(jwtPEM, cfg.JWTKeyID, cfg.JWTIssuer, cfg.JWTAudience)
	if err != nil {
		return fmt.Errorf("init token issuer: %w", err)
	}

	// --- NIC pepper --------------------------------------------------------
	// Mandatory in prod, ephemeral in dev -- the same contract as the JWT
	// signing key above, and for the same reason: zero-setup `make run`
	// without a fallback that could ever ship. Rotating this key in prod
	// silently invalidates every stored digest, so an accidental rotation
	// (which is what an unset variable would be) must not be possible there.
	nicPepper := cfg.NICHashPepper
	if nicPepper == "" {
		if cfg.IsProd() {
			return fmt.Errorf("NIC_HASH_PEPPER is required in %s (at least %d bytes)", cfg.Env, user.NICPepperMinBytes)
		}
		log.Warn().Msg("no NIC_HASH_PEPPER configured; generating an ephemeral pepper for this process (dev only -- stored NIC hashes will not match after a restart)")
		nicPepper, err = user.GenerateEphemeralNICPepper()
		if err != nil {
			return err
		}
	}
	previousPeppers, err := parseNICPepperPrevious(cfg.NICHashPepperPrevious)
	if err != nil {
		return err
	}
	nicHasher, err := user.NewVersionedNICHasher(cfg.NICHashPepperVersion, nicPepper, previousPeppers)
	if err != nil {
		return err
	}
	// Log the version, never the pepper. During a rotation this line is the
	// only way to confirm from the outside which generation a replica is
	// writing under, and "all replicas rolled" is the precondition for
	// retiring the old pepper.
	log.Info().
		Int("nic_hash_pepper_version", nicHasher.Version()).
		Int("nic_hash_pepper_previous_count", len(previousPeppers)).
		Msg("NIC pepper loaded")

	// --- SMS provider ------------------------------------------------------
	sms, err := buildSMSProvider(cfg, log)
	if err != nil {
		return err
	}

	// --- keycloak identity mirror (degrades, never blocks boot) ------------
	kc := buildKeycloakClient(ctx, cfg, log)

	// --- domain wiring -------------------------------------------------
	repo := user.NewRepository(pool)
	outbox := events.NewOutbox(cfg.ServiceName)
	svc := user.NewService(repo, redis, outbox, sms, kc, tokens, nicHasher, log)
	if cfg.GoogleClientID != "" {
		svc.SetGoogle(user.NewGoogleTokenInfoVerifier(cfg.GoogleClientID, nil))
		log.Info().Msg("google sign-in enabled")
	} else {
		log.Warn().Msg("GOOGLE_CLIENT_ID unset; google sign-in disabled")
	}
	if docs := buildDoctorApplications(cfg, log); docs != nil {
		svc.SetDoctorApplications(docs)
		log.Info().Str("doctor_service_url", cfg.DoctorServiceURL).Msg("doctor application attach enabled")
	} else {
		log.Warn().Msg("DOCTOR_SERVICE_URL or mesh token unset; OTP will not promote approved doctor applications")
	}
	handler := user.NewHandler(svc, tokens, redis, log)

	// --- background workers ----------------------------------------------
	relay := events.NewRelay(pool, broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	reaper := user.NewReaper(svc, log,
		time.Duration(cfg.ErasureIntervalMinutes)*time.Minute, cfg.ErasureBatchSize)
	go reaper.Run(ctx)

	// admin.* account commands. admin-service decides and audits; it owns no
	// account data (ADR-004), so it asks this service to apply the change and
	// waits for the user.suspended / user.reinstated fact to come back. Before
	// this subscription existed nothing consumed those commands: the admin
	// console's "suspend user" wrote an audit row, returned 202, and changed
	// nothing at all.
	adminCommands := user.NewAdminCommandConsumer(svc, log)
	go func() {
		if err := adminCommands.Subscribe(ctx, broker); err != nil && ctx.Err() == nil {
			// Not fatal: identity and login must keep working when NATS is
			// down. But an admin suspending an account while this is broken
			// gets silence, which is the exact failure this loop was built to
			// end -- so it is logged at error level, not warn.
			log.Error().Err(err).Msg("admin command consumer stopped; account suspensions are NOT being applied")
		}
	}()

	// --- grpc ------------------------------------------------------------
	// UserService.GetUsersBatch returns name, phone, email, role and status for
	// an arbitrary list of ids. Until this interceptor existed, anything that
	// could open a TCP connection to :9091 could dump the platform's entire
	// patient directory -- a bare grpc.NewServer() has no authentication of any
	// kind, and a NetworkPolicy is a network accident, not an access control.
	//
	// The interceptor fails CLOSED: with no authenticator it refuses every
	// call. There is no exempt method -- this server registers no health
	// service, so the smallest hole available is none.
	grpcServer := grpc.NewServer(user.GRPCServerOptions(buildGRPCAuthenticator(ctx, cfg, log))...)
	userv1.RegisterUserServiceServer(grpcServer, user.NewGRPCServer(svc, log))
	// net.ListenConfig rather than net.Listen: the listener is bound to the
	// root context and stops accepting once the process is shutting down,
	// rather than outliving it.
	var lc net.ListenConfig
	grpcLis, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}
	go func() {
		log.Info().Int("port", cfg.GRPCPort).Msg("grpc server listening")
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Error().Err(err).Msg("grpc server stopped")
		}
	}()
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()

	// --- http ------------------------------------------------------------
	srv := server.New(server.Options{
		TrustedProxies: buildTrustedProxies(cfg.TrustedProxies, log),
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

	srv.Router.Get("/.well-known/jwks.json", handler.JWKS)
	srv.Router.Mount("/api/v1", handler.Routes())

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
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
