// Package record assembles the record domain into a modular.Module.
//
// This is the body of what used to be cmd/record-service/main.go with the
// shared parts removed: the logger, metrics, tracing, Redis, NATS, the
// authenticator and the HTTP server all come from the composer now. The
// domain's own wiring is unchanged, line for line.
package record

import (
	"context"
	"fmt"
	"time"

	"github.com/go-chi/chi/v5"

	"telemed/internal/domain/record/access"
	"telemed/internal/domain/record/clinicalnotes"
	"telemed/internal/domain/record/fhir"
	"telemed/internal/domain/record/prescriptions"
	"telemed/internal/domain/record/records"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/scan"
	"telemed/internal/platform/storage"
	"telemed/internal/platform/usernames"

	"telemed/internal/platform/database"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/server"
)

// Domain is the name this module registers under.
const Domain = "record"

// New assembles the record domain.
func New(ctx context.Context, deps modular.Deps) (*modular.Module, error) {
	// --- configuration -------------------------------------------------
	v := newViper(serviceName)
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	log := deps.Log.With().Str("domain", Domain).Logger()

	m := &modular.Module{Name: Domain}

	// --- this domain's own pool, as this domain's own role ----------------
	dsn, err := deps.DSN(Domain)
	if err != nil {
		return nil, fmt.Errorf("record: %w", err)
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
		return nil, fmt.Errorf("record: connect postgres: %w", err)
	}
	m.Pool = pool

	// --- object storage --------------------------------------------------
	objStore, storagePing, err := buildStorage(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("init storage: %w", err)
	}

	// --- virus scanner -----------------------------------------------------
	// No scanner ships with this deployment, so every upload is stored with
	// scan_status "skipped" rather than a claim nothing verified. The
	// VirusScanner interface stays so a vendor can be dropped in behind it.
	var scanner scan.VirusScanner = scan.PassthroughScanner{}

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

	// --- repositories / services / handlers -------------------------------
	outbox := events.NewOutbox(serviceName)

	accessRepo := access.NewRepository()
	accessSvc := access.NewService(accessRepo, pool, log)
	accessConsumer := access.NewConsumer(accessRepo, pool, log)

	recordsRepo := records.NewRepository()
	recordsSvc := records.NewService(recordsRepo, pool, objStore, scanner, fhirClient, outbox, accessSvc, log)
	recordsSvc.SetNameResolver(usernames.FromRegistry(deps.Registry, log))
	recordsHandler := records.NewHandler(recordsSvc, accessSvc)

	prescriptionsRepo := prescriptions.NewRepository()
	prescriptionsSvc := prescriptions.NewService(prescriptionsRepo, pool, objStore, fhirClient, outbox, accessSvc,
		prescriptions.Config{HMACSecret: []byte(cfg.PrescriptionHMACSecret), VerifyBaseURL: cfg.VerifyBaseURL}, log)
	// The doctor domain's signature/seal lookup, in-process (see
	// modular.KeyDoctorCredentialImages). Absent when this deployment runs
	// the record domain without the doctor domain in the same process --
	// Issue() fails closed and logs loudly in that case rather than silently
	// skipping the check, matching the posture of every other optional
	// cross-domain seam this composer wires (e.g. the doctor module's own
	// holiday registrar).
	if v, ok := deps.Registry.Lookup(modular.KeyDoctorCredentialImages); ok {
		if ci, ok := v.(prescriptions.DoctorCredentialImages); ok {
			prescriptionsSvc.WithCredentialImages(ci)
		} else {
			log.Error().Msg("doctor.credential_images is registered but does not implement prescriptions.DoctorCredentialImages")
		}
	} else {
		log.Warn().Msg("doctor.credential_images unavailable: prescriptions cannot be issued until the doctor domain runs in this process")
	}
	prescriptionsHandler := prescriptions.NewHandler(prescriptionsSvc)

	clinicalNotesRepo := clinicalnotes.NewRepository()
	clinicalNotesSvc := clinicalnotes.NewService(clinicalNotesRepo, pool, fhirClient, outbox, accessSvc, log)
	clinicalNotesHandler := clinicalnotes.NewHandler(clinicalNotesSvc)

	// --- background workers ----------------------------------------------
	// The outbox relay runs in every replica. Row-level SKIP LOCKED makes that
	// safe, and it means no single "worker" pod to lose.
	relay := events.NewRelay(pool, deps.Broker, log,
		time.Duration(cfg.OutboxPollSeconds)*time.Second, cfg.OutboxBatchSize)
	go relay.Run(ctx)

	// The treating-relationship consumer builds the local read-model that
	// backs every doctor-side authorization decision (internal/access). It
	// runs in every replica too: JetStream durable consumers hand out work
	// per-message, not per-connection, so N replicas subscribing to the same
	// durable name is the normal, safe topology.
	go func() {
		if err := deps.Broker.Subscribe(ctx, serviceName+"-consultation-events", accessConsumer.Subjects(), accessConsumer.Handle); err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("consultation event subscriber exited unexpectedly")
		}
	}()

	// --- http ------------------------------------------------------------
	healthChecks := []server.HealthCheck{
		{Name: "postgres", Critical: true, Check: pool.Ping},
		{Name: "deps.Redis", Critical: true, Check: deps.Redis.Ping},
	}
	if storagePing != nil {
		healthChecks = append(healthChecks, server.HealthCheck{Name: "storage", Critical: true, Check: storagePing})
	}
	// --- routes ---------------------------------------------------------
	m.API = func(r chi.Router) {
		// Authenticated surface: records, shares, prescriptions, formulary.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuth(deps.Auth))
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
			r.Use(middleware.RateLimit(deps.Redis, middleware.RateLimitConfig{
				Requests: cfg.VerifyRateLimitPerMinute,
				Window:   time.Minute,
				Name:     "verify_prescription",
			}, log))
			r.Mount("/verify/prescriptions", prescriptionsHandler.VerifyRoutes())
		})

		// The filesystem backend's presigned URLs are only URLs if something
		// answers them. Mounted unauthenticated on purpose: the HMAC in the
		// query string IS the authorisation, exactly as it is for an S3
		// presigned URL, and the handler that minted it already made and
		// logged the access decision.
		//
		// Absent entirely under STORAGE_BACKEND=minio, where the object store
		// serves its own URLs and this route would be a second, weaker door
		// to the same objects.
		if fsStore, ok := objStore.(*storage.FilesystemStorage); ok {
			r.Group(func(r chi.Router) {
				r.Use(middleware.NoStore)
				r.Use(middleware.RateLimit(deps.Redis, middleware.RateLimitConfig{
					Requests: 120,
					Window:   time.Minute,
					Name:     "presigned_object",
				}, log))
				r.Handle(filesRoute, storage.PresignHandler(fsStore))
			})
		}
	}

	m.Health = healthChecks
	return m, nil
}
