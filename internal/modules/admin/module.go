// Package admin assembles the admin domain into a modular.Module.
//
// This is the body of what used to be cmd/admin-service/main.go with the
// shared parts removed: the logger, metrics, tracing, Redis, NATS, the
// authenticator and the HTTP server all come from the composer now. The
// domain's own wiring is unchanged, line for line.
package admin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/grpc/credentials"

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
	"telemed/internal/platform/config"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/servicetoken"
	"telemed/internal/platform/storage"

	"github.com/rs/zerolog"

	userv1 "telemed/internal/pb/user/v1"
	"telemed/internal/platform/database"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/server"
)

// Domain is the name this module registers under.
const Domain = "admin"

// New assembles the admin domain.
func New(ctx context.Context, deps modular.Deps) (*modular.Module, error) {
	// --- configuration -------------------------------------------------
	v := config.New(serviceName)
	v.SetDefault("admin_ip_allowlist", "")
	v.SetDefault("storage_backend", "minio")
	v.SetDefault("filesystem_storage_dir", "./data/objects")
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
	v.SetDefault("mesh_client_id", "telemed-api")
	v.SetDefault("user_service_grpc_addr", "localhost:9091")
	v.SetDefault("user_lookup_timeout_seconds", 3)
	for _, k := range []string{
		"admin_ip_allowlist", "storage_backend", "filesystem_storage_dir",
		"filesystem_presign_secret", "public_api_base_url",
		"minio_endpoint", "minio_access_key", "minio_secret_key",
		"minio_use_ssl", "minio_doctor_docs_bucket", "doc_presign_ttl",
		"user_service_grpc_addr", "user_lookup_timeout_seconds", "trusted_proxies",
		"user_service_grpc_tls", "user_service_grpc_ca_file",
		"user_service_grpc_server_name", "user_service_grpc_allow_plaintext",
		"mesh_client_id", "mesh_client_secret", "mesh_token_url",
		"doctor_service_url",
	} {
		_ = v.BindEnv(k)
	}

	var cfg appConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("admin: %w", err)
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
		return nil, fmt.Errorf("admin: connect postgres: %w", err)
	}
	m.Pool = pool

	// STORAGE_BACKEND is honoured here for the same reason it is in the record
	// domain: the two read the same objects. This used to call storage.New
	// unconditionally, so a filesystem deployment built a MinIO client against
	// an empty endpoint and the process died at boot.
	//
	// EnsureBuckets is not passed: the record domain owns bucket creation, and
	// admin holds a deliberately narrow view of this store (credentialing
	// takes a one-method DocumentStore that can only presign a GET).
	objectStore, _, err := storage.Build(ctx, storage.BuildOptions{
		Backend:           cfg.StorageBackend,
		FilesystemDir:     cfg.FilesystemStorageDir,
		FilesystemBaseURL: strings.TrimSuffix(cfg.PublicAPIBaseURL, "/") + "/api/v1/files",
		FilesystemSecret:  []byte(cfg.FilesystemPresignSecret),
		MinIOEndpoint:     cfg.MinIOEndpoint,
		MinIOAccessKey:    cfg.MinIOAccessKey,
		MinIOSecretKey:    cfg.MinIOSecretKey,
		MinIOSecure:       cfg.MinIOUseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("init object storage: %w", err)
	}

	// --- user directory ---------------------------------------------------
	// Resolved in-process when the user domain is loaded here, and dialled
	// over gRPC when it is not. The caller does not change either way: this is
	// a directory.GRPCClient in both cases, applying the same timeout and the
	// same NOT_FOUND mapping.
	userDirectory, err := userDirectoryFor(cfg, deps, log)
	if err != nil {
		return nil, fail(fmt.Errorf("admin: init user directory: %w", err))
	}
	m.Closers = append(m.Closers, func() { _ = userDirectory.Close() })

	// --- wiring: repositories -> services -> handlers -------------------
	outbox := events.NewOutbox(serviceName)

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

	// The role resolver for administrators whose identity provider mints no
	// roles. Provided rather than installed: the authenticator is shared by
	// every domain and is built by the composer, which attaches this once all
	// domains exist. Bound to the admin issuer so it can never widen a token
	// from any other one.
	deps.Registry.Provide(modular.KeyAdminDirectory,
		adminusers.NewDirectory(adminUsersRepo, cfg.AdminTokenIssuer(), 0, log))

	configRepo := sysconfig.NewRepository(pool)
	configSvc := sysconfig.NewService(configRepo)
	configHandler := sysconfig.NewHandler(configSvc)

	// The mesh credential is needed here even when the user directory is
	// in-process, but only for one thing: approving a doctor application calls
	// doctor-service over HTTP, which is a different integration with its own
	// token requirement.
	//
	// So the requirement follows that integration rather than the domain. With
	// DOCTOR_SERVICE_URL unset there is no outbound call to authenticate, and
	// buildMeshCredentials' production refusal -- which exists because a
	// credential-less client fails on every call instead of at boot -- would be
	// refusing to start over a credential nothing in this process would present.
	// Where the integration IS configured the prod requirement is unchanged and
	// still fatal; that is the case the refusal was written for.
	var meshCreds credentials.PerRPCCredentials
	if cfg.DoctorServiceURL != "" {
		var err error
		meshCreds, err = buildMeshCredentials(cfg, log)
		if err != nil {
			return nil, fail(fmt.Errorf("admin: init mesh credentials: %w", err))
		}
	}

	credRepo := credentialing.NewRepository(pool)
	credSvc := credentialing.NewService(pool, credRepo, objectStore, outbox, cfg.MinIODoctorDocsBucket, cfg.DocPresignTTL)
	if cfg.DoctorServiceURL != "" {
		if src, ok := meshCreds.(*servicetoken.Source); ok && src != nil {
			verifier := credentialing.NewHTTPApplicationVerifier(cfg.DoctorServiceURL, src)
			credSvc.SetApplicationVerifier(verifier)
			credSvc.SetPendingApplicationSource(verifier)
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
	relay := events.NewRelay(pool, deps.Broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	// Every projector and the analytics refresher run in their own
	// goroutine, exactly like the relay: NATS durable consumers survive a
	// restart, and any one of these blocking until ctx is cancelled is by
	// design, not an oversight.
	go runProjector(log, "credentialing", func() error { return credProjector.Subscribe(ctx, deps.Broker) })
	go runProjector(log, "admin-notifications", func() error { return notifSvc.Subscribe(ctx, deps.Broker) })
	go runProjector(log, "analytics", func() error { return analyticsProjector.Subscribe(ctx, deps.Broker) })
	go runProjector(log, "users", func() error { return usersProjector.Subscribe(ctx, deps.Broker) })
	go func() {
		if err := refresher.RunHourly(ctx); err != nil {
			log.Error().Err(err).Msg("analytics refresher stopped")
		}
	}()

	adminCIDRs := splitCSV(cfg.AdminIPAllowlist)
	// --- routes ---------------------------------------------------------
	// cmd/telemed mounts every domain at /api/v1; the standalone admin service
	// mounted at /api/v1/admin, and the route table and console still use that.
	m.API = func(api chi.Router) {
		api.Route("/admin", func(r chi.Router) {
			// Every route: authenticate, then the service-wide IP allowlist,
			// then resolve/enforce the local admin account (active + per-admin
			// IP scope), then auto-audit every state-changing request, then
			// never let a browser or proxy cache the response. RequireRole is
			// applied per route group below via rbac.Roles, so the exact same
			// table drives both the tested matrix (internal/rbac) and what is
			// actually enforced here.
			r.Use(middleware.RequireAuth(deps.Auth))
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
	}

	m.Health = []server.HealthCheck{
		{Name: "postgres", Critical: true, Check: pool.Ping},
		{Name: "minio", Critical: false, Check: func(c context.Context) error {
			return storage.BucketProbe(objectStore, cfg.MinIODoctorDocsBucket)(c)
		}},
	}
	return m, nil
}

// userDirectoryFor prefers the in-process user service over a network hop.
//
// The mesh token the gRPC path presents proves "I am inside the mesh". A call
// from another package in this binary already is, so the in-process path does
// not re-check it -- see user.InProcessClient for the full argument. The
// per-caller authorisation that actually matters was never in the interceptor;
// it is in the user service, which both paths call.
func userDirectoryFor(cfg appConfig, deps modular.Deps, log zerolog.Logger) (*directory.GRPCClient, error) {
	timeout := time.Duration(cfg.UserLookupTimeoutSeconds) * time.Second

	if v, ok := deps.Registry.Lookup(modular.KeyUserDirectory); ok {
		if client, ok := v.(userv1.UserServiceClient); ok {
			log.Info().Msg("user directory resolved in-process; no gRPC dial")
			return directory.NewInProcess(client, timeout), nil
		}
	}

	// The credential this process presents to user-service. Without it every
	// lookup comes back Unauthenticated: that surface authenticates every
	// method and has no exempt ones, by design -- it is a full PII directory.
	meshCreds, err := buildMeshCredentials(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("init mesh credentials: %w", err)
	}
	return directory.New(cfg.UserServiceGRPCAddr, timeout,
		directory.TLSConfig{
			Enabled:              cfg.UserServiceGRPCTLS,
			CAFile:               cfg.UserServiceGRPCCAFile,
			ServerName:           cfg.UserServiceGRPCServerName,
			AllowPlaintextInProd: cfg.UserServiceGRPCAllowPlaintext,
			IsProd:               cfg.IsProd(),
		}, meshCreds)
}
