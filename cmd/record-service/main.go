// Command server is the entrypoint for telemed-record-service: medical
// document storage, e-prescriptions with QR verification, clinical (SOAP)
// notes with their append-only amendment trail, and the authorization layer
// that gates all three.
//
// The boot sequence is intentionally rigid and identical across services:
//
//	config -> logger -> metrics -> tracing -> postgres -> redis -> nats
//	  -> repositories -> services -> handlers -> routes -> background workers
//	  -> listen -> drain
//
// An operator debugging service #7 at 3am should recognise the shape of
// service #2 immediately.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"telemed/internal/domain/record/access"
	"telemed/internal/domain/record/clinicalnotes"
	"telemed/internal/domain/record/fhir"
	"telemed/internal/domain/record/prescriptions"
	"telemed/internal/domain/record/records"
	"telemed/internal/platform/cache"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
	"telemed/internal/platform/scan"
	"telemed/internal/platform/server"
	"telemed/internal/platform/storage"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-record-service"

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
	v := newViper(serviceName)
	var cfg Config
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
		// One database, one schema per domain (migrations/bootstrap). This is
		// what makes an unqualified table name resolve to record's tables and
		// not another domain's -- six table names collide across domains.
		SearchPath: database.Schema("record") + ", public",
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

	// --- object storage --------------------------------------------------
	objStore, storagePing, err := buildStorage(ctx, cfg)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}

	// --- virus scanner -----------------------------------------------------
	var scanner scan.VirusScanner = scan.PassthroughScanner{}
	if cfg.ClamAVAddr != "" {
		scanner = scan.NewClamAV(cfg.ClamAVAddr, cfg.ClamAVTimeout)
		log.Info().Str("clamav_addr", cfg.ClamAVAddr).Msg("virus scanning enabled")
	} else {
		log.Warn().Msg("CLAMAV_ADDR not set: uploads will be stored with scan_status=skipped")
	}

	// --- FHIR client ------------------------------------------------------
	var fhirClient fhir.Client
	if cfg.FHIRBaseURL != "" {
		fhirClient = fhir.NewMedplum(fhir.MedplumConfig{
			BaseURL: cfg.FHIRBaseURL, ClientID: cfg.FHIRClientID, ClientSecret: cfg.FHIRClientSecret,
		})
		log.Info().Str("fhir_base_url", cfg.FHIRBaseURL).Msg("fhir integration enabled (medplum)")
	} else {
		fhirClient = fhir.NewNoOp(log)
		log.Warn().Msg("FHIR_BASE_URL not set: fhir integration is a no-op")
	}

	// --- authentication -------------------------------------------------
	// TWO issuers, not one (ADR-010): telemed-user-service signs patient and
	// doctor tokens so phone-OTP login survives a Keycloak outage, and
	// Keycloak signs admin tokens. A token is accepted only when its
	// signature verifies against one of these key sets AND its "iss" claim is
	// on the allowlist -- the signature alone does not say which issuer minted
	// it. Configuring only Keycloak here rejects every patient and doctor
	// token, which on this service means no patient can open their own
	// records and no doctor can issue a prescription.
	// IssuerKeys, not JWKSURLs: each issuer is bound to the one key set that
	// may sign for it. Merging both key sets and then checking "iss" against
	// an allowlist would let user-service's key sign a token claiming to come
	// from Keycloak -- the merged set matches on "kid" alone and "iss" is
	// chosen by whoever signs -- which is exactly the privilege escalation
	// ADR-010 says two issuers must never permit.
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

	// --- repositories / services / handlers -------------------------------
	outbox := events.NewOutbox(cfg.ServiceName)

	accessRepo := access.NewRepository()
	accessSvc := access.NewService(accessRepo, pool, log)
	accessConsumer := access.NewConsumer(accessRepo, pool, log)

	recordsRepo := records.NewRepository()
	recordsSvc := records.NewService(recordsRepo, pool, objStore, scanner, fhirClient, outbox, accessSvc, log)
	recordsHandler := records.NewHandler(recordsSvc, accessSvc)

	prescriptionsRepo := prescriptions.NewRepository()
	prescriptionsSvc := prescriptions.NewService(prescriptionsRepo, pool, objStore, fhirClient, outbox, accessSvc,
		prescriptions.Config{HMACSecret: []byte(cfg.PrescriptionHMACSecret), VerifyBaseURL: cfg.VerifyBaseURL}, log)
	prescriptionsHandler := prescriptions.NewHandler(prescriptionsSvc)

	clinicalNotesRepo := clinicalnotes.NewRepository()
	clinicalNotesSvc := clinicalnotes.NewService(clinicalNotesRepo, pool, fhirClient, outbox, accessSvc, log)
	clinicalNotesHandler := clinicalnotes.NewHandler(clinicalNotesSvc)

	// --- background workers ----------------------------------------------
	// The outbox relay runs in every replica. Row-level SKIP LOCKED makes that
	// safe, and it means no single "worker" pod to lose.
	relay := events.NewRelay(pool, broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	// The treating-relationship consumer builds the local read-model that
	// backs every doctor-side authorization decision (internal/access). It
	// runs in every replica too: JetStream durable consumers hand out work
	// per-message, not per-connection, so N replicas subscribing to the same
	// durable name is the normal, safe topology.
	go func() {
		if err := broker.Subscribe(ctx, cfg.ServiceName+"-consultation-events", accessConsumer.Subjects(), accessConsumer.Handle); err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("consultation event subscriber exited unexpectedly")
		}
	}()

	// --- http ------------------------------------------------------------
	healthChecks := []server.HealthCheck{
		{Name: "postgres", Critical: true, Check: pool.Ping},
		{Name: "redis", Critical: true, Check: redisCache.Ping},
	}
	if storagePing != nil {
		healthChecks = append(healthChecks, server.HealthCheck{Name: "storage", Critical: true, Check: storagePing})
	}

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
		HealthChecks:   healthChecks,
	})

	srv.Router.Route("/api/v1", func(r chi.Router) {
		// Authenticated surface: records, shares, prescriptions, formulary.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuth(auth))
			r.Use(middleware.NoStore) // clinical/financial data is never cached
			r.Mount("/records", recordsHandler.Routes())
			r.Mount("/shares", recordsHandler.ShareRoutes())
			r.Mount("/prescriptions", prescriptionsHandler.Routes())
			r.Mount("/drugs", prescriptionsHandler.DrugRoutes())
			// Clinical notes and the ICD-10 reference the picker searches.
			// Both sit inside the same NoStore group: a SOAP note must not
			// be cached by any proxy, and the ICD-10 list is small enough
			// that the client's own debounce is all the caching it needs.
			r.Mount("/clinical-notes", clinicalNotesHandler.Routes())
			r.Mount("/icd10", clinicalNotesHandler.ICD10Routes())
		})

		// Public, unauthenticated verification endpoint -- a pharmacist's
		// scanner calls this with no login. It is backed by a database read
		// with no authentication in front of it, so AGENT-BRIEF requires it
		// be rate-limited hard; the limit is per-IP since there is no
		// principal to key on.
		r.Group(func(r chi.Router) {
			r.Use(middleware.NoStore)
			r.Use(middleware.RateLimit(redisCache, middleware.RateLimitConfig{
				Requests: cfg.VerifyRateLimitPerMinute,
				Window:   time.Minute,
				Name:     "verify_prescription",
			}, log))
			r.Mount("/verify/prescriptions", prescriptionsHandler.VerifyRoutes())
		})
	})

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
}

// buildStorage constructs the Storage implementation selected by
// STORAGE_BACKEND and, for MinIO, provisions the bucket topology
// (AGENT-BRIEF: private, SSE-S3 AES-256, versioning on) before the server
// starts accepting traffic. It also returns a health-check probe when the
// backend supports one.
func buildStorage(ctx context.Context, cfg Config) (storage.Storage, func(context.Context) error, error) {
	switch cfg.StorageBackend {
	case "filesystem":
		fsStore, err := storage.NewFilesystem(cfg.FilesystemStorageDir, "")
		if err != nil {
			return nil, nil, err
		}
		return fsStore, fsStore.Ping, nil
	default: // "minio", enforced by Config.Validate
		minioStore, err := storage.New(storage.Config{
			Endpoint: cfg.MinIOEndpoint, AccessKey: cfg.MinIOAccessKey, SecretKey: cfg.MinIOSecretKey,
			Secure: cfg.MinIOSecure, Region: cfg.MinIORegion,
		})
		if err != nil {
			return nil, nil, err
		}
		if err := minioStore.EnsureBuckets(ctx, storage.AllBuckets, cfg.MinIORegion); err != nil {
			return nil, nil, fmt.Errorf("ensure buckets: %w", err)
		}
		return minioStore, minioStore.Ping, nil
	}
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
