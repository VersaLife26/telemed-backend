// Command telemed is the platform, in one process.
//
// It composes the eight business domains and the edge (the former API gateway)
// into a single binary. The boot sequence is the same one every service
// followed before consolidation, done once instead of nine times:
//
//	config -> logger -> metrics -> tracing -> redis -> nats -> authenticator
//	  -> per-domain pools, wiring and workers -> edge route table -> listen -> drain
//
// WHICH DOMAINS RUN
// All of them by default. TELEMED_DOMAINS selects a subset, so the same image
// is also every one of the old nine services:
//
//	TELEMED_DOMAINS=user GRPC_PORT=9091 HTTP_PORT=8081 telemed   # user-service
//	TELEMED_DOMAINS=edge HTTP_PORT=8080 telemed                  # api-gateway
//
// That is the rollback path, and it needs no second build. A process that does
// not hold a domain reaches it the way it always did -- over the network, with
// the mesh credential -- because the module asks the registry first and falls
// back to dialling.
//
// WHAT DID NOT CHANGE
// NATS and the transactional outbox. A business change and the event
// announcing it are still written in one Postgres transaction and relayed
// afterwards. Replacing that with a direct function call because the consumer
// now happens to be in the same address space would reintroduce exactly the
// failure the outbox exists to prevent -- "slot booked, nobody notified" --
// and would do it while looking like a simplification.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"telemed/internal/gateway"
	"telemed/internal/platform/cache"
	"telemed/internal/platform/config"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/logger"
	platmw "telemed/internal/platform/middleware"
	"telemed/internal/platform/modular"
	"telemed/internal/platform/observability"
	"telemed/internal/platform/server"

	adminmod "telemed/internal/modules/admin"
	consultationmod "telemed/internal/modules/consultation"
	doctormod "telemed/internal/modules/doctor"
	notificationmod "telemed/internal/modules/notification"
	paymentmod "telemed/internal/modules/payment"
	recordmod "telemed/internal/modules/record"
	schedulingmod "telemed/internal/modules/scheduling"
	usermod "telemed/internal/modules/user"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed"

// builders is the domain registry, in construction order.
//
// ORDER IS LOAD-BEARING. user comes first because it publishes the in-process
// user directory that admin and notification look up; built the other way
// round they would find an empty registry and dial a gRPC port that this
// process is not even listening on. Everything after user is independent, and
// the order among those is only for reproducible logs.
var builders = []struct {
	name  string
	build func(context.Context, modular.Deps) (*modular.Module, error)
}{
	{usermod.Domain, usermod.New},
	{doctormod.Domain, doctormod.New},
	{schedulingmod.Domain, schedulingmod.New},
	{consultationmod.Domain, consultationmod.New},
	{paymentmod.Domain, paymentmod.New},
	{recordmod.Domain, recordmod.New},
	{notificationmod.Domain, notificationmod.New},
	{adminmod.Domain, adminmod.New},
}

func main() {
	if err := run(); err != nil {
		// stderr rather than the structured logger: a failure here may well be
		// the logger itself, or config that never loaded.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// The root context is cancelled on SIGTERM so background workers stop in
	// step with the HTTP server instead of being killed mid-transaction.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- configuration -----------------------------------------------------
	v := config.New(serviceName)
	v.SetDefault("telemed_domains", "all")
	v.SetDefault("expose_grpc", false)
	v.SetDefault("db_max_conns_per_domain", 8)
	for _, k := range []string{"telemed_domains", "expose_grpc", "db_max_conns_per_domain", "routes_file"} {
		_ = v.BindEnv(k)
	}
	var base config.Base
	if err := v.Unmarshal(&base); err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	enabled, wantEdge, err := selectDomains(v.GetString("telemed_domains"))
	if err != nil {
		return err
	}

	// --- logging -----------------------------------------------------------
	log := logger.New(serviceName, base.LogLevel, base.Env)
	log.Info().
		Str("version", Version).
		Strs("domains", enabled).
		Bool("edge", wantEdge).
		Msg("starting telemed")

	// --- admin-role issuer binding -----------------------------------------
	// ADR-012 stopped user-service signing a token that CLAIMS to be Keycloak.
	// It never stopped it minting an honest token, under its own issuer,
	// carrying realm_access.roles = ["super_admin"]. Without this call
	// adminIssuer is "", checkAdminIssuer short-circuits to nil, and every
	// admin-role check in this process honours that token. Set once, for every
	// domain, which is one of the things one process makes easier to get right.
	platmw.SetAdminIssuer(base.KeycloakIssuer)

	// --- observability -----------------------------------------------------
	metrics := observability.NewMetrics(serviceName)
	gwMetrics := gateway.NewGatewayMetrics(metrics)

	tracing, err := observability.NewTracing(ctx, observability.TracingOptions{
		ServiceName:  serviceName,
		ServiceVer:   Version,
		Env:          base.Env,
		OTLPEndpoint: base.OTLPEndpoint,
		Sampling:     base.TraceSampling,
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

	// --- shared datastores --------------------------------------------------
	// One Redis client and one NATS connection for the process. Eight of each
	// to the same servers is waste that also multiplies the failure modes.
	redis, err := cache.NewRedis(ctx, cache.Options{
		URL:      base.RedisURL,
		Password: base.RedisPassword,
		DB:       base.RedisDB,
	})
	if err != nil {
		return fmt.Errorf("connect redis: %w", err)
	}
	defer func() { _ = redis.Close() }()

	broker, err := events.NewNATS(ctx, events.NATSOptions{
		URL:             base.NATSURL,
		Stream:          base.NATSStream,
		CredentialsFile: base.NATSCredentials,
		Name:            serviceName,
		MaxBytes:        base.NATSMaxBytes,
	}, log)
	if err != nil {
		return fmt.Errorf("connect nats: %w", err)
	}
	defer func() { _ = broker.Close() }()

	// --- authentication ------------------------------------------------------
	// TWO issuers, not one (ADR-010). IssuerKeys binds each to the one key set
	// that may sign for it: merging both key sets and then checking "iss" would
	// let user-service's key sign a token claiming to come from Keycloak, since
	// a merged set matches on "kid" alone and "iss" is chosen by whoever signs.
	auth, err := platmw.NewAuthenticatorFrom(ctx, platmw.AuthConfig{
		IssuerKeys: base.IssuerKeys(),
		Issuers:    base.TrustedIssuers(),
		Audience:   base.KeycloakAudience,
	})
	if err != nil {
		return fmt.Errorf("init authenticator: %w", err)
	}

	// --- domains --------------------------------------------------------------
	deps := modular.Deps{
		Log:         log,
		Metrics:     metrics,
		Redis:       redis,
		Broker:      broker,
		Auth:        auth,
		Version:     Version,
		Env:         base.Env,
		Registry:    modular.NewRegistry(),
		DSN:         dsnResolver(base.DatabaseURL),
		ExposeGRPC:  v.GetBool("expose_grpc"),
		MaxConns:    poolSize(v.GetInt("db_max_conns_per_domain")),
		MinConns:    1,
		MaxConnLife: base.DatabaseMaxConnLife,
	}

	var (
		mods   []*modular.Module
		checks []server.HealthCheck
	)
	closeMods := func() {
		// Reverse order: a domain built later may depend on one built earlier.
		for i := len(mods) - 1; i >= 0; i-- {
			for j := len(mods[i].Closers) - 1; j >= 0; j-- {
				mods[i].Closers[j]()
			}
			if mods[i].Pool != nil {
				mods[i].Pool.Close()
			}
		}
	}
	defer closeMods()

	for _, b := range builders {
		if !contains(enabled, b.name) {
			continue
		}
		mod, err := b.build(ctx, deps)
		if err != nil {
			return fmt.Errorf("build %s domain: %w", b.name, err)
		}
		mods = append(mods, mod)
		for _, hc := range mod.Health {
			// Eight domains each report a "postgres" check. Prefixing keeps
			// them distinguishable in the readiness body, which is the only
			// place an operator learns WHICH pool is down.
			hc.Name = mod.Name + ":" + hc.Name
			checks = append(checks, hc)
		}
		log.Info().Str("domain", mod.Name).Int("workers", len(mod.Workers)).Msg("domain ready")
	}
	checks = append(checks, server.HealthCheck{Name: "redis", Critical: true, Check: redis.Ping})

	// --- http ------------------------------------------------------------------
	gwCfg, err := gateway.Load(gateway.NewViper(serviceName))
	if err != nil {
		return fmt.Errorf("load edge config: %w", err)
	}
	corsOrigins := append(append(append([]string{},
		gwCfg.CORSPatientOrigins...), gwCfg.CORSDoctorOrigins...), gwCfg.CORSAdminOrigins...)

	srv := server.New(server.Options{
		TrustedProxies: buildTrustedProxies(base.TrustedProxies, log),
		ServiceName:    serviceName,
		Version:        Version,
		Env:            base.Env,
		Port:           base.HTTPPort,
		Logger:         log,
		Metrics:        metrics,
		RequestTimeout: base.RequestTimeout,
		ShutdownGrace:  base.ShutdownGrace,
		CORSOrigins:    corsOrigins,
		HealthChecks:   checks,
	})

	// Each domain's routes go on a router of their own, mounted at the same
	// /api/v1 the standalone service used. The edge then dispatches to that
	// router by the route table's `upstream` field, with the path unchanged --
	// which is why no route, and no client, has to know the difference.
	domainHandlers := make(map[string]chi.Router, len(mods))
	for _, mod := range mods {
		dr := chi.NewRouter()
		if mod.API != nil {
			dr.Route("/api/v1", mod.API)
		}
		if mod.Root != nil {
			mod.Root(dr)
		}
		domainHandlers[mod.Name] = dr
	}

	if wantEdge {
		edgeChecks, err := mountEdge(srv.Router, gwCfg, domainHandlers, deps, gwMetrics, redis, log)
		if err != nil {
			return err
		}
		srv.AddHealthChecks(edgeChecks...)
	} else {
		// No edge in this process: serve the domains directly, which is what a
		// single-domain deployment behind the real gateway wants.
		for _, mod := range mods {
			srv.Router.Mount("/", domainHandlers[mod.Name])
		}
	}

	// --- workers ---------------------------------------------------------------
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	var wg sync.WaitGroup
	for _, mod := range mods {
		for _, w := range mod.Workers {
			wg.Add(1)
			go func(name string, run func(context.Context) error) {
				defer wg.Done()
				if err := run(workerCtx); err != nil && workerCtx.Err() == nil {
					// Not fatal, and deliberately so: a stopped consumer must
					// not take down a process that is still serving requests.
					// It IS an error-level line, because the failure mode --
					// events piling up unprocessed -- is otherwise silent.
					log.Error().Err(err).Str("worker", name).Msg("background worker stopped unexpectedly")
				}
			}(w.Name, w.Run)
		}
	}

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}

	stopWorkers()
	// Bounded: a worker that ignores its context must not hold the process
	// open past the shutdown grace the orchestrator is counting down.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(base.ShutdownGrace):
		log.Warn().Msg("background workers did not stop within the shutdown grace")
	}
	return nil
}

// mountEdge puts the former API gateway in front of the in-process domains.
//
// The route table, the auth classes, the rate-limit classes, the timeout
// classes, the admin IP allowlist and the body caps are all unchanged -- the
// same routes.default.json, validated by the same contract test. The only
// difference is what the last inch does: a domain in this process is called,
// not dialled.
func mountEdge(
	r chi.Router,
	cfg gateway.Config,
	domains map[string]chi.Router,
	deps modular.Deps,
	gwMetrics *gateway.GatewayMetrics,
	redis *cache.RedisCache,
	log zerolog.Logger,
) ([]server.HealthCheck, error) {
	routes, err := gateway.LoadRoutes(cfg.RoutesFile)
	if err != nil {
		return nil, fmt.Errorf("load route table: %w", err)
	}
	log.Info().Int("routes", len(routes)).Str("routes_file", cfg.RoutesFile).Msg("route table loaded")

	// The eight upstream names the route table uses, and where each resolves.
	specs := []struct{ name, url string }{
		{"user-service", cfg.UpstreamUserURL},
		{"doctor-service", cfg.UpstreamDoctorURL},
		{"scheduling-service", cfg.UpstreamSchedulingURL},
		{"consultation-service", cfg.UpstreamConsultationURL},
		{"payment-service", cfg.UpstreamPaymentURL},
		{"notification-service", cfg.UpstreamNotificationURL},
		{"record-service", cfg.UpstreamRecordURL},
		{"admin-service", cfg.UpstreamAdminURL},
	}

	// Per-domain share of the process's in-flight budget. The gateway's global
	// LoadShed still caps the total; this stops one domain consuming all of it
	// and starving the others -- the property eight separate processes gave for
	// free, and the one real regression of putting them together.
	perDomain := cfg.MaxInFlight / len(specs)
	if perDomain < 1 {
		perDomain = 1
	}

	upstreams := make(map[string]*gateway.Upstream, len(specs))
	var checks []server.HealthCheck

	for _, spec := range specs {
		name := spec.name
		breaker := gateway.NewBreaker(redis, gateway.BreakerConfig{
			Name:             name,
			FailureThreshold: cfg.CircuitFailureThreshold,
			OpenDuration:     time.Duration(cfg.CircuitOpenSeconds) * time.Second,
			ProbeGrace:       time.Duration(cfg.CircuitProbeSeconds) * time.Second,
		}, log)
		breaker.OnState(func(state gateway.BreakerState) { gwMetrics.CircuitStateSet(name, state) })

		// "user-service" in the route table, "user" as a domain.
		if dr, ok := domains[strings.TrimSuffix(name, "-service")]; ok {
			upstreams[name] = gateway.NewInProcessUpstream(name, dr, breaker, perDomain)
			log.Info().Str("upstream", name).Int("max_in_flight", perDomain).Msg("upstream resolved in-process")
		} else {
			// Not loaded here: dial it, exactly as the standalone gateway did.
			up, err := gateway.NewUpstream(name, spec.url, breaker, 2, 100*time.Millisecond,
				func() { gwMetrics.RetriesTotal.WithLabelValues(name).Inc() }, log)
			if err != nil {
				return nil, fmt.Errorf("build upstream %s: %w", name, err)
			}
			upstreams[name] = up
			log.Info().Str("upstream", name).Str("url", spec.url).Msg("upstream resolved over the network")
		}

		checks = append(checks, server.HealthCheck{
			Name:     "upstream:" + name,
			Critical: false, // one dead backend must not pull the whole edge from rotation
			Check: func(ctx context.Context) error {
				if breaker.Snapshot(ctx) == gateway.StateOpen {
					return fmt.Errorf("circuit breaker open")
				}
				return nil
			},
		})
	}

	if err := gateway.Mount(r, gateway.Deps{
		Config:        cfg,
		Cache:         redis,
		Authenticator: deps.Auth,
		Metrics:       deps.Metrics,
		GatewayMetric: gwMetrics,
		Logger:        log,
		Upstreams:     upstreams,
		Routes:        routes,
	}); err != nil {
		return nil, fmt.Errorf("mount routes: %w", err)
	}

	r.Get("/openapi.yaml", gateway.ServeOpenAPI)
	r.Get("/docs", gateway.ServeDocsUI)
	return checks, nil
}

// selectDomains parses TELEMED_DOMAINS.
//
// "all" (the default) is every domain plus the edge. A comma-separated list
// selects a subset; "edge" in that list runs the gateway surface.
func selectDomains(raw string) (domains []string, edge bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "all") {
		for _, b := range builders {
			domains = append(domains, b.name)
		}
		return domains, true, nil
	}
	known := map[string]bool{"edge": true}
	for _, b := range builders {
		known[b.name] = true
	}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, false, fmt.Errorf("TELEMED_DOMAINS names %q, which is not a domain", name)
		}
		if name == "edge" {
			edge = true
			continue
		}
		domains = append(domains, name)
	}
	if len(domains) == 0 && !edge {
		return nil, false, errors.New("TELEMED_DOMAINS selected nothing to run")
	}
	return domains, edge, nil
}

// dsnResolver maps a domain to the connection string it must use.
//
// THIS IS THE FUNCTION THAT KEEPS LEAST PRIVILEGE.
// Before consolidation each service held a connection to its own database as
// its own role, and a compromised payment path could not read the user
// directory because Postgres refused the connection. One process could easily
// have collapsed that into one pool as one role with access to everything --
// that is the loss the consolidation plan recorded as unavoidable, and it is
// not, because nothing forces one process to hold one connection identity.
//
// Each domain gets its own pool as telemed_<domain>_app with search_path pinned
// to svc_<domain>. The boundary is still enforced by the server: that role has
// no USAGE on any other schema (infra/postgres/init/01-init-databases.sh, and
// scripts/verify-db-privileges.sh proves it), so a bug in one domain cannot
// read another's rows even though the code shares an address space.
//
// A per-domain override wins when set: TELEMED_DB_URL_PAYMENT, and so on. The
// derived form assumes the roles share a password, which is how the platform
// provisions them; the override is there for a deployment where they do not.
func dsnResolver(baseDSN string) func(string) (string, error) {
	return func(domain string) (string, error) {
		if override := os.Getenv("TELEMED_DB_URL_" + strings.ToUpper(domain)); override != "" {
			return override, nil
		}
		if baseDSN == "" {
			return "", errors.New("DATABASE_URL is not set, and no TELEMED_DB_URL_* override was given")
		}
		u, err := url.Parse(baseDSN)
		if err != nil {
			return "", fmt.Errorf("parse DATABASE_URL: %w", err)
		}
		password, _ := u.User.Password()
		u.User = url.UserPassword("telemed_"+domain+"_app", password)
		q := u.Query()
		q.Set("search_path", database.SearchPathFor(domain))
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
}

// buildTrustedProxies parses TRUSTED_PROXIES. Returning nil hands server.New
// the platform default (private ranges only). A malformed entry is logged and
// skipped rather than fatal -- one typo in a config map must not stop a pod
// starting -- but it is never treated as a wildcard.
func buildTrustedProxies(cidrs []string, log zerolog.Logger) *platmw.TrustedProxies {
	if len(cidrs) == 0 {
		return nil
	}
	parsed, malformed := platmw.NewTrustedProxies(cidrs)
	for _, bad := range malformed {
		log.Error().Str("cidr", bad).Msg("ignoring malformed TRUSTED_PROXIES entry")
	}
	return parsed
}

// poolSize clamps the per-domain connection ceiling into an int32.
//
// Not a cast: a negative or absurd value from the environment would wrap, and
// pgxpool would then either refuse to start or open a pool far larger than the
// database's own connection limit -- with eight domains multiplying it.
func poolSize(n int) int32 {
	const maxPerDomain = 1000
	if n < 1 {
		return 1
	}
	if n > maxPerDomain {
		return maxPerDomain
	}
	return int32(n)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
