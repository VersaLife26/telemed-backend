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
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/spf13/viper"

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

	consultationdomain "telemed/internal/domain/consultation/consultation"
	websignal "telemed/internal/domain/consultation/signal"
	"telemed/internal/platform/testkit"

	adminmod "telemed/internal/modules/admin"
	consultationmod "telemed/internal/modules/consultation"
	doctormod "telemed/internal/modules/doctor"
	notificationmod "telemed/internal/modules/notification"
	paymentmod "telemed/internal/modules/payment"
	recordmod "telemed/internal/modules/record"
	schedulingmod "telemed/internal/modules/scheduling"
	testkitmod "telemed/internal/modules/testkit"
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
	// Subcommands are handled before any config is read: `healthcheck` runs in
	// a container whose environment is the server's, and must not fail because
	// the server's own config is momentarily invalid.
	if code, handled := runSubcommand(os.Args); handled {
		os.Exit(code)
	}
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
	v.SetDefault("ice_stun_urls", []string{"stun:stun.l.google.com:19302"})
	v.SetDefault("ice_turn_ttl", websignal.DefaultTURNTTL)
	v.SetDefault("ice_turn_mode", "static")
	v.SetDefault("signal_token_ttl", consultationdomain.DefaultRoomTokenTTL)
	for _, k := range []string{
		"telemed_domains", "expose_grpc", "db_max_conns_per_domain", "routes_file",
		"signal_secret",
		"ice_stun_urls", "ice_turn_urls", "ice_turn_username", "ice_turn_credential",
		"ice_turn_secret", "ice_turn_ttl", "ice_turn_mode",
		"signal_public_url", "signal_token_ttl", "signal_allowed_origins",
		"cloudflare_turn_key_id", "cloudflare_turn_api_token",
		"cors_patient_origins", "cors_doctor_origins",
	} {
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
	platmw.SetAdminIssuer(base.AdminTokenIssuer())

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

	// The signalling configuration, built ONCE for the process.
	//
	// Not per module, and not inside the test-mode block it used to live in:
	// SIGNAL_SECRET falls back to a generated key, so two readers would each
	// generate their own and a token minted by one would not verify in the
	// other. The consultation domain needs it whether or not the test surface
	// is mounted.
	deps.Signal = modular.SignalConfig{
		Secret:         signalSecret(v.GetString("signal_secret"), log),
		ICE:            buildICEProvider(v, log),
		AllowedOrigins: signalOrigins(v, log),
		PublicURL:      v.GetString("signal_public_url"),
		TokenTTL:       v.GetDuration("signal_token_ttl"),
	}

	// Built BEFORE any domain, because the domains read it while wiring their
	// providers: user swaps its SMS provider for the capturing one and
	// notification does the same per channel. Created after them it would be
	// nil at exactly the moment it is consulted, and every OTP would go out
	// over a real SMS gateway with the test page reporting an empty outbox.
	if base.TestModeEnabled() {
		deps.Outbox = testkit.NewOutbox(testkit.DefaultCapacity)
		log.Warn().
			Msg("TELEMED_TEST_MODE is on: OTP and email are captured instead of delivered, " +
				"and /api/v1/test/* is served without authentication")
	} else if v.GetBool("telemed_test_mode") {
		// Set, but overridden. Said out loud, because the operator who set it
		// is expecting the test surface and would otherwise spend the morning
		// wondering why /test 404s.
		log.Warn().
			Msg("TELEMED_TEST_MODE is set but ENV is production-like: the test surface stays off")
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

	// Attach the administrator role resolver, if the admin domain is in this
	// process to provide one.
	//
	// After the loop, not inside it: the authenticator is built before any
	// domain (they all need it) and the table the resolver reads belongs to a
	// domain that may not be enabled here. A process running without the admin
	// domain simply has no resolver, and an Access token arrives with no roles
	// -- which is the correct answer, because nothing in that process could
	// have told it otherwise.
	if v, ok := deps.Registry.Lookup(modular.KeyAdminDirectory); ok {
		resolver, ok := v.(platmw.RoleResolver)
		if !ok {
			return fmt.Errorf("registry: %s does not implement middleware.RoleResolver", modular.KeyAdminDirectory)
		}
		auth.WithRoleResolver(resolver)
		log.Info().Str("admin_issuer", base.AdminTokenIssuer()).
			Msg("administrator roles resolve from admin_users")
	}
	if deps.Outbox != nil {
		// The consultation domain's hub, when it is loaded here, so the test
		// page drives the stack that actually carries consultations.
		var sharedHub *websignal.Hub
		if hv, ok := deps.Registry.Lookup(modular.KeySignalHub); ok {
			sharedHub, _ = hv.(*websignal.Hub)
		}

		tk, err := testkitmod.New(ctx, deps, testkitmod.Options{
			Outbox: deps.Outbox,
			// deps.Signal, not a second read of the environment. SIGNAL_SECRET
			// falls back to a generated key, so reading it twice would produce
			// two different secrets and a token minted by the consultation
			// domain would not verify on the test socket.
			SignalSecret: deps.Signal.Secret,
			ICE:          deps.Signal.ICE,
			Hub:          sharedHub,
			// Any origin: the test page is opened from a laptop, a phone on
			// the LAN and whatever host:port `next dev` picked, and an origin
			// allowlist there only makes the tool harder to use without
			// protecting anything the room token does not already.
			AllowedOrigins: nil,
			Env:            base.Env,
			Version:        Version,
		})
		if err != nil {
			return fmt.Errorf("build test surface: %w", err)
		}
		mods = append(mods, tk)
	}

	checks = append(checks, server.HealthCheck{Name: "redis", Critical: true, Check: redis.Ping})

	// --- http ------------------------------------------------------------------
	gwCfg, err := gateway.Load(gateway.NewViper(serviceName))
	if err != nil {
		return fmt.Errorf("load edge config: %w", err)
	}
	corsOrigins := append(append(append([]string{},
		gwCfg.CORSPatientOrigins...), gwCfg.CORSDoctorOrigins...), gwCfg.CORSAdminOrigins...)

	raw := map[string]http.Handler{}
	for _, mod := range mods {
		for path, h := range mod.Raw {
			raw[path] = h
		}
	}

	srv := server.New(server.Options{
		TrustedProxies: buildTrustedProxies(base.TrustedProxies, log),
		RawHandlers:    raw,
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
		// The route table only knows /api/v1. A domain's Root routes -- user's
		// JWKS document, the provider webhooks -- were served by the standalone
		// service on its own port, which in one process is this port. Without
		// this the process cannot fetch its own signing key, and every token
		// it mints is rejected as unverifiable.
		//
		// Each domain's Root goes on a router of its own rather than onto the
		// edge directly: notification and consultation both claim /webhooks as
		// a subtree, and chi refuses to mount the same path twice.
		var roots []*chi.Mux
		for _, mod := range mods {
			if mod.Root != nil {
				rr := chi.NewRouter()
				mod.Root(rr)
				roots = append(roots, rr)
			}
		}
		edgeChecks, err := mountEdge(srv.Router, gwCfg, domainHandlers, roots, deps, gwMetrics, redis, log)
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
	r *chi.Mux,
	cfg gateway.Config,
	domains map[string]chi.Router,
	roots []*chi.Mux,
	deps modular.Deps,
	gwMetrics *gateway.GatewayMetrics,
	redis *cache.RedisCache,
	log zerolog.Logger,
) ([]server.HealthCheck, error) {
	routes, err := gateway.LoadRoutes(cfg.RoutesFile)
	if err != nil {
		return nil, fmt.Errorf("load route table: %w", err)
	}
	// The test surface's routes ship in the table so the contract test can see
	// them and assert what they are, but they are dropped outright unless the
	// module was actually built. Dropping them here rather than letting Mount
	// fail on a missing upstream is deliberate: a table entry naming an
	// upstream that does not exist must still be a boot failure for every
	// OTHER route, because that is a typo, and only this one name is
	// legitimately absent half the time.
	if _, ok := domains[testkitmod.Domain]; !ok {
		routes = slices.DeleteFunc(routes, func(r gateway.RouteRule) bool {
			return r.Upstream == testkitmod.Domain+"-service"
		})
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
	// The test surface is an upstream only when it was actually built. Its
	// routes are in the table unconditionally, so without this entry a
	// non-test process would fail Mount with "unknown upstream" -- which is
	// the right failure to have made impossible rather than to have handled.
	if _, ok := domains[testkitmod.Domain]; ok {
		specs = append(specs, struct{ name, url string }{testkitmod.Domain + "-service", ""})
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

	// Root-level paths the table does not know fall through to the domains'
	// own root routers. Only outside /api/v1: the route table stays the sole
	// authority for the API surface, so a domain handler that is not in the
	// table remains unreachable through the edge.
	tableNotFound := r.NotFoundHandler()
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.URL.Path, "/api/") {
			for _, root := range roots {
				if root.Match(chi.NewRouteContext(), req.Method, req.URL.Path) {
					root.ServeHTTP(w, req)
					return
				}
			}
		}
		tableNotFound(w, req)
	})

	r.Get("/openapi.yaml", gateway.ServeOpenAPI)
	r.Get("/docs", gateway.ServeDocsUI)
	return checks, nil
}

// signalSecret resolves the key that signs signalling room tokens.
//
// An unset SIGNAL_SECRET generates a random one per process rather than
// falling back to a constant. A constant would be published in this repository
// and therefore forgeable by anyone who read it, and the alternative -- failing
// boot -- would make the test surface something you configure before you can
// use it, which is the opposite of its purpose. The cost of a per-process key
// is that tokens do not survive a restart, which for a 30-minute test token is
// not a cost at all.
//
// Set it explicitly when more than one replica serves the test surface: two
// processes with two random keys cannot verify each other's tokens.
func signalSecret(configured string, log zerolog.Logger) []byte {
	if configured != "" {
		return []byte(configured)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing means the OS entropy source is gone, and every
		// token, session and OTP this process would go on to mint is
		// unsafe. There is nothing to degrade to.
		panic(fmt.Sprintf("generate signal secret: %v", err))
	}
	log.Info().Msg("SIGNAL_SECRET unset: generated a per-process key (room tokens will not survive a restart)")
	return buf
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

// buildICEProvider selects how TURN credentials are minted.
//
// Two modes, because the two services do genuinely different things: coturn
// verifies an HMAC it never issued, so minting is local arithmetic; Cloudflare
// issues credentials over an authenticated API, so minting is a network call
// that can fail and must be cached.
func buildICEProvider(v *viper.Viper, log zerolog.Logger) websignal.ICEProvider {
	stun := v.GetStringSlice("ice_stun_urls")
	switch strings.ToLower(strings.TrimSpace(v.GetString("ice_turn_mode"))) {
	case "cloudflare":
		return websignal.NewCloudflareTURN(
			v.GetString("cloudflare_turn_key_id"),
			v.GetString("cloudflare_turn_api_token"),
			v.GetDuration("ice_turn_ttl"),
			stun,
			log,
		)
	default:
		return websignal.ICEConfig{
			STUNURLs:       stun,
			TURNURLs:       v.GetStringSlice("ice_turn_urls"),
			TURNUsername:   v.GetString("ice_turn_username"),
			TURNCredential: v.GetString("ice_turn_credential"),
			TURNSecret:     v.GetString("ice_turn_secret"),
			TURNTTL:        v.GetDuration("ice_turn_ttl"),
		}
	}
}

// signalOrigins is the websocket upgrade's origin allowlist.
//
// CORS does not apply to a websocket handshake, so Origin checking is the ONLY
// origin control a websocket has. Defaults to the patient and doctor CORS
// origins, because those are the two surfaces that place calls.
//
// An empty result means "any origin", which is what the dev test surface
// wants and what a consultation must never have.
func signalOrigins(v *viper.Viper, log zerolog.Logger) []string {
	explicit := v.GetStringSlice("signal_allowed_origins")
	if len(explicit) > 0 {
		return explicit
	}
	out := append([]string{}, v.GetStringSlice("cors_patient_origins")...)
	out = append(out, v.GetStringSlice("cors_doctor_origins")...)
	if len(out) == 0 {
		log.Warn().Msg("no signalling origin allowlist: the consultation websocket will accept any Origin " +
			"(set SIGNAL_ALLOWED_ORIGINS, or CORS_PATIENT_ORIGINS and CORS_DOCTOR_ORIGINS)")
	}
	return out
}
