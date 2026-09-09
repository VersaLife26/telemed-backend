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
//
// telemed-admin-service is a JSON API only: doctor credentialing, platform
// analytics, disputes, configuration, and the append-only audit log. There
// is no server-rendered UI here -- telemed-admin-web (Next.js) is the only
// client. See README.md for what this service owns and what it does not.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"telemed/internal/domain/admin/adminusers"
	"telemed/internal/domain/admin/analytics"
	"telemed/internal/domain/admin/appointments"
	"telemed/internal/domain/admin/audit"
	"telemed/internal/domain/admin/content"
	"telemed/internal/domain/admin/credentialing"
	"telemed/internal/domain/admin/directory"
	"telemed/internal/domain/admin/disputes"
	"telemed/internal/domain/admin/finance"
	"telemed/internal/domain/admin/notifications"
	"telemed/internal/domain/admin/rbac"
	"telemed/internal/domain/admin/sysconfig"
	"telemed/internal/domain/admin/users"
	"telemed/internal/platform/cache"
	"telemed/internal/platform/config"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
	"telemed/internal/platform/server"
	"telemed/internal/platform/servicetoken"
	"telemed/internal/platform/storage"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-admin-service"

func main() {
	if err := run(); err != nil {
		// Write to stderr rather than the structured logger: a failure here
		// may well be the logger itself, or config that never loaded.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// The root context is cancelled on SIGTERM so background workers stop in
	// step with the HTTP server instead of being killed mid-transaction.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- configuration -------------------------------------------------
	v := config.New(serviceName)
	v.SetDefault("admin_ip_allowlist", "")
	v.SetDefault("minio_endpoint", "localhost:9000")
	v.SetDefault("minio_access_key", "minioadmin")
	v.SetDefault("minio_secret_key", "minioadmin")
	v.SetDefault("minio_use_ssl", false)
	// telemed-infra creates doctor-credentials; doctor-documents never
	// existed, so the old default pointed the document viewer -- and the
	// MinIO health check -- at a bucket that is not there.
	v.SetDefault("minio_doctor_docs_bucket", "doctor-credentials")
	// Short by default and hard-capped in credentialing.NewService: a
	// presigned URL to a scanned NIC is a bearer credential.
	v.SetDefault("doc_presign_ttl", credentialing.DefaultPresignTTL)
	v.SetDefault("keycloak_realm", "telemedicine")
	v.SetDefault("mesh_client_id", "telemed-api")
	v.SetDefault("user_service_grpc_addr", "localhost:9091")
	v.SetDefault("user_lookup_timeout_seconds", 3)
	for _, k := range []string{
		"admin_ip_allowlist", "minio_endpoint", "minio_access_key", "minio_secret_key",
		"minio_use_ssl", "minio_doctor_docs_bucket", "doc_presign_ttl",
		"user_service_grpc_addr", "user_lookup_timeout_seconds", "trusted_proxies",
		"user_service_grpc_tls", "user_service_grpc_ca_file",
		"user_service_grpc_server_name", "user_service_grpc_allow_plaintext",
		"keycloak_base_url", "keycloak_realm",
		"keycloak_admin_client_id", "keycloak_admin_client_secret",
		"mesh_client_id", "mesh_client_secret", "mesh_token_url",
		"doctor_service_url",
	} {
		_ = v.BindEnv(k)
	}

	var cfg appConfig
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

	redisCache, err := cache.NewRedis(ctx, cache.Options{
		URL:      cfg.RedisURL,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err != nil {
		return fmt.Errorf("connect redis: %w", err)
	}
	defer func() { _ = redisCache.Close() }()

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

	objectStore, err := storage.New(storage.Config{
		Endpoint:  cfg.MinIOEndpoint,
		AccessKey: cfg.MinIOAccessKey,
		SecretKey: cfg.MinIOSecretKey,
		Secure:    cfg.MinIOUseSSL,
	})
	if err != nil {
		return fmt.Errorf("init object storage: %w", err)
	}

	// --- authentication -------------------------------------------------
	// TWO issuers, not one (ADR-010). Keycloak signs the admin tokens this
	// service's own surface requires, and user-service signs patient and
	// doctor tokens -- which still reach here on any route an ops user shares
	// with a doctor, and which a single-issuer authenticator rejects outright.
	//
	// IssuerKeys binds each issuer to the ONE key set that may sign for it.
	// Merging both key sets and then checking the "iss" claim would let
	// user-service's key sign a token claiming to come from Keycloak, since
	// the merged set matches on "kid" alone and "iss" is chosen by whoever
	// signs -- i.e. the patient issuer could mint an admin token, on the most
	// privileged surface on the platform.
	auth, err := middleware.NewAuthenticatorFrom(ctx, middleware.AuthConfig{
		IssuerKeys: cfg.IssuerKeys(),
		Issuers:    cfg.TrustedIssuers(),
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

	// --- user directory ---------------------------------------------------
	// The credential this process presents to user-service. Without it every
	// lookup comes back Unauthenticated: that surface authenticates every
	// method and has no exempt ones, by design -- it is a full PII directory.
	meshCreds, err := buildMeshCredentials(cfg, log)
	if err != nil {
		return fmt.Errorf("init mesh credentials: %w", err)
	}

	userDirectory, err := directory.New(cfg.UserServiceGRPCAddr,
		time.Duration(cfg.UserLookupTimeoutSeconds)*time.Second,
		directory.TLSConfig{
			Enabled:              cfg.UserServiceGRPCTLS,
			CAFile:               cfg.UserServiceGRPCCAFile,
			ServerName:           cfg.UserServiceGRPCServerName,
			AllowPlaintextInProd: cfg.UserServiceGRPCAllowPlaintext,
			IsProd:               cfg.IsProd(),
		}, meshCreds)
	if err != nil {
		return fmt.Errorf("init user directory: %w", err)
	}
	defer func() { _ = userDirectory.Close() }()

	// --- wiring: repositories -> services -> handlers -------------------
	outbox := events.NewOutbox(cfg.ServiceName)

	auditRepo := audit.NewRepository(pool)
	auditSvc := audit.NewService(auditRepo)
	// The bulk CSV export gets its own, narrower role list than the rest of the
	// audit subtree -- see rbac.GroupAuditExport. Passing it in from here keeps
	// internal/rbac the single source of truth for who may call what, so the
	// matrix test exercises the list the router actually enforces.
	auditHandler := audit.NewHandler(auditSvc, rbac.Roles(rbac.GroupAuditExport)...)

	adminUsersRepo := adminusers.NewRepository(pool)
	adminUsersSvc := adminusers.NewService(adminUsersRepo, buildIdentityProvider(ctx, cfg, log), log)
	adminUsersHandler := adminusers.NewHandler(adminUsersSvc)

	configRepo := sysconfig.NewRepository(pool)
	configSvc := sysconfig.NewService(configRepo)
	configHandler := sysconfig.NewHandler(configSvc)

	credRepo := credentialing.NewRepository(pool)
	credSvc := credentialing.NewService(pool, credRepo, objectStore, outbox, cfg.MinIODoctorDocsBucket, cfg.DocPresignTTL)
	if cfg.DoctorServiceURL != "" {
		if src, ok := meshCreds.(*servicetoken.Source); ok && src != nil {
			credSvc.SetApplicationVerifier(credentialing.NewHTTPApplicationVerifier(cfg.DoctorServiceURL, src))
			log.Info().Str("doctor_service_url", cfg.DoctorServiceURL).Msg("doctor application verify wired")
		} else {
			log.Warn().Msg("DOCTOR_SERVICE_URL set but mesh token unavailable; application approve will not hit doctor-service")
		}
	}
	credHandler := credentialing.NewHandler(credSvc)
	credProjector := credentialing.NewProjector(credRepo, userDirectory, log)

	notifRepo := notifications.NewRepository(pool)
	notifSvc := notifications.NewService(notifRepo, log)
	notifHandler := notifications.NewHandler(notifSvc)

	disputesRepo := disputes.NewRepository(pool)
	disputesSvc := disputes.NewService(disputesRepo, userDirectory)
	disputesHandler := disputes.NewHandler(disputesSvc)

	contentRepo := content.NewRepository(pool)
	contentSvc := content.NewService(pool, contentRepo, outbox)
	contentHandler := content.NewHandler(contentSvc)

	analyticsRepo := analytics.NewRepository(pool)
	analyticsSvc := analytics.NewService(analyticsRepo)
	analyticsHandler := analytics.NewHandler(analyticsSvc)
	analyticsProjector := analytics.NewProjector(analyticsRepo, log)
	refresher := analytics.NewRefresher(analyticsRepo, log)

	usersRepo := users.NewRepository(pool)
	usersSvc := users.NewService(pool, usersRepo, outbox)
	usersHandler := users.NewHandler(usersSvc)
	usersProjector := users.NewProjector(usersRepo, userDirectory, log)

	apptRepo := appointments.NewRepository(pool)
	apptSvc := appointments.NewService(pool, apptRepo, outbox)
	apptHandler := appointments.NewHandler(apptSvc)

	financeRepo := finance.NewRepository(pool)
	financeSvc := finance.NewService(pool, financeRepo, outbox, configSvc)
	financeHandler := finance.NewHandler(financeSvc)

	// --- background workers ----------------------------------------------
	// The outbox relay runs in every replica. Row-level SKIP LOCKED makes that
	// safe, and it means no single "worker" pod to lose.
	relay := events.NewRelay(pool, broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	// Every projector and the analytics refresher run in their own
	// goroutine, exactly like the relay: NATS durable consumers survive a
	// restart, and any one of these blocking until ctx is cancelled is by
	// design, not an oversight.
	go runProjector(log, "credentialing", func() error { return credProjector.Subscribe(ctx, broker) })
	go runProjector(log, "admin-notifications", func() error { return notifSvc.Subscribe(ctx, broker) })
	go runProjector(log, "analytics", func() error { return analyticsProjector.Subscribe(ctx, broker) })
	go runProjector(log, "users", func() error { return usersProjector.Subscribe(ctx, broker) })
	go func() {
		if err := refresher.RunHourly(ctx); err != nil {
			log.Error().Err(err).Msg("analytics refresher stopped")
		}
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
			{Name: "redis", Critical: true, Check: redisCache.Ping},
			// MinIO is non-critical: an outage should stop admins opening
			// NEW credentialing documents, not take the whole API down.
			{Name: "minio", Critical: false, Check: func(c context.Context) error {
				return objectStore.PingBucket(c, cfg.MinIODoctorDocsBucket)
			}},
		},
	})

	adminCIDRs := splitCSV(cfg.AdminIPAllowlist)

	srv.Router.Route("/api/v1/admin", func(r chi.Router) {
		// Every route: authenticate, then the service-wide IP allowlist,
		// then resolve/enforce the local admin account (active + per-admin
		// IP scope), then auto-audit every state-changing request, then
		// never let a browser or proxy cache the response. RequireRole is
		// applied per route group below via rbac.Roles, so the exact same
		// table drives both the tested matrix (internal/rbac) and what is
		// actually enforced here.
		r.Use(middleware.RequireAuth(auth))
		r.Use(middleware.IPAllowlist(adminCIDRs, log))
		r.Use(adminusers.RequireActiveAdminUser(adminUsersSvc, log))
		r.Use(audit.Middleware(auditSvc, log))
		r.Use(middleware.NoStore)

		// Any authenticated admin may read their own row; no GroupAdminUsers gate.
		r.Get("/me", adminUsersHandler.Me)

		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupCredentialing)...)).Mount("/doctors", credHandler.Routes())
		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupCredentialing)...)).Mount("/notifications", notifHandler.Routes())
		// "/admin-users", not "/admins". Under the /admin prefix the pair
		// would otherwise read /admin/admins and /admin/users -- one word
		// apart, one listing the handful of staff accounts and the other
		// listing every patient and doctor on the platform. That is a
		// confusion worth spending three characters to remove, on a console
		// where the two screens look alike and only one of them is PHI.
		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupAdminUsers)...)).Mount("/admin-users", adminUsersHandler.Routes())
		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupUsers)...)).Mount("/users", usersHandler.Routes())
		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupAppointments)...)).Mount("/appointments", apptHandler.Routes())
		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupFinance)...)).Mount("/finance", financeHandler.Routes())
		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupContent)...)).Mount("/content", contentHandler.Routes())
		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupDisputes)...)).Mount("/disputes", disputesHandler.Routes())
		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupConfig)...)).Mount("/configs", configHandler.Routes())
		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupAnalytics)...)).Mount("/analytics", analyticsHandler.Routes())
		r.With(middleware.RequireRole(rbac.Roles(rbac.GroupAudit)...)).Mount("/audit", auditHandler.Routes())
	})

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
}

// runProjector wraps subscribe in a restart-free error log: a durable NATS
// consumer that returns because the broker connection dropped should not
// crash the whole service, it should log and let the outer ctx cancellation
// (SIGTERM) be the only reason it stops.
func runProjector(log zerolog.Logger, name string, subscribe func() error) {
	if err := subscribe(); err != nil && !errors.Is(err, context.Canceled) {
		log.Error().Err(err).Str("projector", name).Msg("event projector stopped unexpectedly")
	}
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
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

// buildTrustedProxies parses TRUSTED_PROXIES. Returning nil hands server.New
// the platform default (private ranges only), which is the right answer for a
// docker-compose or single-ingress deployment. A malformed entry is logged and
// skipped rather than fatal -- one typo in a config map must not stop a pod
// starting -- but it is never treated as a wildcard, because on this service
// the resolved address gates the admin IP allowlist.
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

// buildIdentityProvider connects the Keycloak admin client used to create and
// re-role admin accounts.
//
// Unlike user-service, an unreachable Keycloak here does NOT degrade into a
// working service with a quiet warning. There, Keycloak mirrors an account
// that already exists and works, so degrading keeps patients logging in. Here
// it IS the admin identity provider, and the operations that need it --
// creating a login, changing the realm role that authorizes every route --
// have no meaningful half-success. UnavailableProvider refuses them loudly at
// the first attempt rather than writing rows for admins who cannot sign in, or
// reporting a demotion that never reached the realm.
//
// The rest of the service is unaffected: listing admins and deactivating one
// both work from the database alone.
func buildIdentityProvider(ctx context.Context, cfg appConfig, log zerolog.Logger) adminusers.IdentityProvider {
	if cfg.KeycloakBaseURL == "" || cfg.KeycloakAdminClientID == "" {
		return adminusers.NewUnavailableProvider(log)
	}
	p, err := adminusers.NewGocloakProvider(ctx, adminusers.KeycloakConfig{
		BaseURL:      cfg.KeycloakBaseURL,
		Realm:        cfg.KeycloakRealm,
		ClientID:     cfg.KeycloakAdminClientID,
		ClientSecret: cfg.KeycloakAdminClientSecret,
	})
	if err != nil {
		log.Error().Err(err).Msg("adminusers: keycloak admin client unavailable; " +
			"creating and re-roling admins is disabled until it is reachable")
		return adminusers.NewUnavailableProvider(log)
	}
	log.Info().Str("realm", cfg.KeycloakRealm).Msg("adminusers: keycloak admin client connected")
	return p
}

// buildMeshCredentials returns the service token source used on outbound gRPC.
//
// In production a missing configuration is fatal. That is the lesson of the
// break this fixes: a client with no credential does not fail at boot, it
// fails on every call afterwards, and both ends keep building, starting and
// passing their tests while the integration is dead. Refusing to start is the
// only failure mode an operator cannot miss.
//
// Outside production it degrades to nil, so a developer running one service
// against a mesh with no Keycloak still gets a working process.
func buildMeshCredentials(cfg appConfig, log zerolog.Logger) (credentials.PerRPCCredentials, error) {
	tokenURL := cfg.MeshTokenURL
	if tokenURL == "" && cfg.KeycloakBaseURL != "" {
		tokenURL = strings.TrimSuffix(cfg.KeycloakBaseURL, "/") +
			"/realms/" + cfg.KeycloakRealm + "/protocol/openid-connect/token"
	}

	src, err := servicetoken.New(servicetoken.Config{
		TokenURL:     tokenURL,
		ClientID:     cfg.MeshClientID,
		ClientSecret: cfg.MeshClientSecret,
		// The token is a mesh-wide bearer credential, so it may only travel
		// over TLS -- except where this deployment has already made the
		// explicit decision to run the mesh in plaintext.
		RequireTLS: cfg.UserServiceGRPCTLS,
	})
	if errors.Is(err, servicetoken.ErrNotConfigured) {
		if cfg.IsProd() {
			return nil, fmt.Errorf("MESH_CLIENT_ID/MESH_CLIENT_SECRET and KEYCLOAK_BASE_URL are "+
				"required in production: user-service authenticates every gRPC method, so a "+
				"client without a service token fails on every call: %w", err)
		}
		log.Warn().Msg("mesh service token not configured; outbound gRPC will be " +
			"unauthenticated and user-service will refuse it")
		return nil, nil //nolint:nilnil // an absent credential is a valid non-prod state
	}
	if err != nil {
		return nil, err
	}
	log.Info().Str("client_id", cfg.MeshClientID).Msg("mesh service token source ready")
	return src, nil
}
