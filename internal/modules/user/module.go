// Package user assembles the user domain into a modular.Module.
//
// This is the body of what used to be cmd/user-service/main.go, minus what the
// composer now owns. The helpers it calls -- the SMS provider, the Keycloak
// mirror, the doctor-application client, the NIC pepper parser -- moved across
// verbatim in helpers.go.
//
// The user domain is special in one way: it is the platform's second token
// issuer (ADR-010). It mints its own RS256 access tokens and publishes the
// matching JWKS document, so /.well-known/jwks.json is served from here
// whatever else this process is running.
package user

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc"

	"telemed/internal/domain/user/user"
	userv1 "telemed/internal/pb/user/v1"
	"telemed/internal/platform/config"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/server"
)

// Domain is the name this module registers under.
const Domain = "user"

// New assembles the user domain.
func New(ctx context.Context, deps modular.Deps) (*modular.Module, error) {
	// --- configuration ---------------------------------------------------
	v := config.New(serviceName)
	// 9091, the port this domain's own grpc_server.go names. Without it the
	// platform default of 9090 applies, and with EXPOSE_GRPC=true and both
	// user and doctor composed into one process they race for the same port --
	// whichever builds second dies with "listen tcp :9090: bind: address
	// already in use", taking the whole container with it. scheduling and
	// payment already declare theirs; user and doctor were missed.
	v.SetDefault("grpc_port", 9091)
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
		return nil, fmt.Errorf("user: load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("user: %w", err)
	}
	log := deps.Log.With().Str("domain", Domain).Logger()

	m := &modular.Module{Name: Domain}
	// fail releases whatever this module has already opened and hands the error
	// back. It returns only the error: the module is always nil on this path,
	// and saying so twice invited a caller to return a half-built one.
	fail := func(err error) error {
		for i := len(m.Closers) - 1; i >= 0; i-- {
			m.Closers[i]()
		}
		if m.Pool != nil {
			m.Pool.Close()
		}
		return err
	}

	// --- this domain's own pool, as this domain's own role ----------------
	dsn, err := deps.DSN(Domain)
	if err != nil {
		return nil, fmt.Errorf("user: %w", err)
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
		return nil, fmt.Errorf("user: connect postgres: %w", err)
	}
	m.Pool = pool

	// --- signing key -------------------------------------------------------
	// user-service mints its own access tokens, so a signing key is mandatory
	// in prod. In dev, a missing key generates an ephemeral one for this
	// process only, so `make run` needs zero setup -- the same philosophy as
	// the dev SMS provider.
	jwtPEM := cfg.JWTPrivateKeyPEM
	if jwtPEM == "" {
		if cfg.IsProd() {
			return nil, fail(fmt.Errorf("user: JWT_PRIVATE_KEY_PEM is required in %s", cfg.Env))
		}
		log.Warn().Msg("no JWT_PRIVATE_KEY_PEM configured; generating an ephemeral RSA key for this process (dev only -- tokens will not verify after a restart)")
		jwtPEM, err = user.GenerateEphemeralKeyPEM()
		if err != nil {
			return nil, fail(fmt.Errorf("user: generate ephemeral signing key: %w", err))
		}
	}
	tokens, err := user.NewTokenIssuer(jwtPEM, cfg.JWTKeyID, cfg.JWTIssuer, cfg.JWTAudience)
	if err != nil {
		return nil, fail(fmt.Errorf("user: init token issuer: %w", err))
	}

	// --- NIC pepper --------------------------------------------------------
	// Mandatory in prod, ephemeral in dev -- the same contract as the JWT
	// signing key, and for the same reason. Rotating this key in prod silently
	// invalidates every stored digest, so an accidental rotation (which is
	// what an unset variable would be) must not be possible there.
	nicPepper := cfg.NICHashPepper
	if nicPepper == "" {
		if cfg.IsProd() {
			return nil, fail(fmt.Errorf("user: NIC_HASH_PEPPER is required in %s (at least %d bytes)", cfg.Env, user.NICPepperMinBytes))
		}
		log.Warn().Msg("no NIC_HASH_PEPPER configured; generating an ephemeral pepper for this process (dev only -- stored NIC hashes will not match after a restart)")
		nicPepper, err = user.GenerateEphemeralNICPepper()
		if err != nil {
			return nil, fail(fmt.Errorf("user: %w", err))
		}
	}
	previousPeppers, err := parseNICPepperPrevious(cfg.NICHashPepperPrevious)
	if err != nil {
		return nil, fail(fmt.Errorf("user: %w", err))
	}
	nicHasher, err := user.NewVersionedNICHasher(cfg.NICHashPepperVersion, nicPepper, previousPeppers)
	if err != nil {
		return nil, fail(fmt.Errorf("user: %w", err))
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
		return nil, fail(fmt.Errorf("user: %w", err))
	}
	// Test mode swaps delivery for capture. The real provider is still built
	// first, so a misconfiguration that would fail boot in production still
	// fails boot here rather than being masked until test mode is turned off.
	if deps.Outbox != nil {
		log.Warn().Msg("TEST MODE: OTP SMS is captured to the test outbox and NOT delivered")
		sms = newCapturingSMS(deps.Outbox, cfg.SMSProvider)
	}

	// --- keycloak identity mirror (degrades, never blocks boot) ------------
	kc := buildKeycloakClient(ctx, cfg, log)

	// --- domain wiring -----------------------------------------------------
	repo := user.NewRepository(pool)
	outbox := events.NewOutbox(serviceName)
	svc := user.NewService(repo, deps.Redis, outbox, sms, kc, tokens, nicHasher, log)
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
	handler := user.NewHandler(svc, tokens, deps.Redis, log)

	// --- the in-process directory other domains resolve users through ------
	// Registered whether or not the gRPC listener is started below: a caller
	// in this process should never go out to the network to reach a service
	// sitting in its own address space. See user.InProcessClient for why the
	// mesh token is not re-checked on that path.
	grpcImpl := user.NewGRPCServer(svc, log)
	deps.Registry.Provide(modular.KeyUserDirectory, user.NewInProcessClient(grpcImpl))

	// --- background workers -------------------------------------------------
	relay := events.NewRelay(pool, deps.Broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	reaper := user.NewReaper(svc, log,
		time.Duration(cfg.ErasureIntervalMinutes)*time.Minute, cfg.ErasureBatchSize)

	// admin.* account commands. The admin domain decides and audits; it owns
	// no account data (ADR-004), so it asks this domain to apply the change
	// and waits for the user.suspended / user.reinstated fact to come back.
	adminCommands := user.NewAdminCommandConsumer(svc, log)

	m.Workers = []modular.Worker{
		modular.SimpleWorker("user-outbox-relay", relay.Run),
		modular.SimpleWorker("user-erasure-reaper", reaper.Run),
		{
			Name: "user-admin-commands",
			Run:  func(ctx context.Context) error { return adminCommands.Subscribe(ctx, deps.Broker) },
		},
	}

	// --- optional gRPC listener --------------------------------------------
	// Only started when GRPC_PORT is set. In the default single-process
	// deployment nothing dials it -- peers resolve users through the registry
	// above -- and a port nobody uses is a port nobody should be able to
	// reach. It is still here, still authenticated, because a deployment that
	// splits the domains across processes needs it: run this binary with
	// TELEMED_DOMAINS=user and GRPC_PORT=9091 and it is the old user-service.
	//
	// GetUsersBatch returns name, phone, email, role and status for an
	// arbitrary list of ids. A bare grpc.NewServer() has no authentication of
	// any kind, and a NetworkPolicy is a network accident, not an access
	// control. The interceptor fails CLOSED and exempts no method.
	if cfg.GRPCPort > 0 && deps.ExposeGRPC {
		// contextcheck: the stream interceptor uses the STREAM\'s context, which
		// is the only correct one -- there is no request context at registration
		// time, and inheriting one would tie every stream to whichever request
		// happened to construct the server.
		//nolint:contextcheck
		grpcServer := grpc.NewServer(user.GRPCServerOptions(buildGRPCAuthenticator(ctx, cfg, log))...)
		userv1.RegisterUserServiceServer(grpcServer, grpcImpl)

		// net.ListenConfig rather than net.Listen: the listener is bound to
		// the root context and stops accepting once the process is shutting
		// down, rather than outliving it.
		var lc net.ListenConfig
		grpcLis, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%d", cfg.GRPCPort))
		if err != nil {
			return nil, fail(fmt.Errorf("user: listen grpc: %w", err))
		}
		m.Workers = append(m.Workers, modular.Worker{
			Name: "user-grpc",
			Run: func(ctx context.Context) error {
				go func() {
					<-ctx.Done()
					grpcServer.GracefulStop()
				}()
				log.Info().Int("port", cfg.GRPCPort).Msg("grpc server listening")
				return grpcServer.Serve(grpcLis)
			},
		})
	}

	// --- routes -------------------------------------------------------------
	m.API = func(r chi.Router) { r.Mount("/", handler.Routes()) }
	m.Root = func(r chi.Router) { r.Get("/.well-known/jwks.json", handler.JWKS) }

	m.Health = []server.HealthCheck{{Name: "postgres", Critical: true, Check: pool.Ping}}
	return m, nil
}
