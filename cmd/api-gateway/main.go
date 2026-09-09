// Command server is the telemed-api-gateway entrypoint.
//
// The boot sequence follows the same shape every telemed service uses, minus
// the two stages a gateway has no business running:
//
//	config -> logger -> metrics -> tracing -> redis -> keycloak jwks
//	  -> route table -> upstreams+breakers -> router -> listen -> drain
//
// There is no postgres stage and no outbox relay. The gateway owns no
// database: everything it needs to survive a restart or scale to N replicas
// either lives in Redis (rate limits, circuit-breaker state) or is re-derived
// from configuration and the route table at boot. A gateway that owns state
// is not a gateway.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"telemed/internal/gateway"
	"telemed/internal/platform/cache"
	"telemed/internal/platform/logger"
	platmw "telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
	"telemed/internal/platform/server"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-api-gateway"

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
	// step with the HTTP server instead of being killed mid-request.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- configuration -------------------------------------------------
	v := gateway.NewViper(serviceName)
	cfg, err := gateway.Load(v)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// --- logging -------------------------------------------------------
	log := logger.New(cfg.ServiceName, cfg.LogLevel, cfg.Env)
	log.Info().Str("version", Version).Msg("starting service")

	// --- observability -------------------------------------------------
	metrics := observability.NewMetrics(cfg.ServiceName)
	gwMetrics := gateway.NewGatewayMetrics(metrics)

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

	// --- datastore: redis only -------------------------------------------
	// Rate-limit counters and circuit-breaker state, shared across every
	// gateway replica. Nothing else. See the package doc for why there is no
	// postgres stage here.
	redis, err := cache.NewRedis(ctx, cache.Options{
		URL:      cfg.RedisURL,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err != nil {
		return fmt.Errorf("connect redis: %w", err)
	}
	defer func() { _ = redis.Close() }()

	// --- authentication -------------------------------------------------
	// The JWT is verified once, here, so every downstream service can trust
	// the identity the gateway forwards. Downstream services still verify
	// independently -- defence in depth -- but the gateway rejects the
	// obviously-bad early.
	//
	// TWO issuers, not one (ADR-010): telemed-user-service signs patient and
	// doctor tokens so phone-OTP login survives a Keycloak outage, and
	// Keycloak signs admin tokens. A token is accepted only when its signature
	// verifies against one of these key sets AND its "iss" claim is on the
	// allowlist -- both issuers publish keys we trust, so the signature alone
	// does not say which one minted it, and without the allowlist the patient
	// issuer could mint an admin token. Configuring only Keycloak here rejects
	// every patient and doctor request at the edge.
	// IssuerKeys ONLY -- no JWKSURLs fallback. An unbound key set can verify a
	// token for any issuer on the allowlist, and this is the edge: it is the
	// component that decides whether a request reaches the admin surface at
	// all. Config.Validate refuses to boot without at least one bound issuer,
	// so there is no silent degradation into the permissive form.
	auth, err := platmw.NewAuthenticatorFrom(ctx, platmw.AuthConfig{
		IssuerKeys: cfg.IssuerKeys(),
		Issuers:    cfg.TrustedIssuers(),
		Audience:   cfg.KeycloakAudience,
	})
	if err != nil {
		return fmt.Errorf("init authenticator: %w", err)
	}

	// An administrative role is only honoured from the admin issuer.
	//
	// The gateway already binds the issuer per rule for auth mode "admin"
	// (RequireTokenIssuer). This covers the other path: an "authenticated"
	// rule carrying an admin role reaches RequireRole with no issuer check,
	// and without this call checkAdminIssuer returns nil for every caller --
	// so a token from the phone-OTP issuer asserting super_admin would be
	// honoured. That is the 2FA bypass ADR-012 exists to prevent.
	platmw.SetAdminIssuer(cfg.KeycloakIssuer)

	// --- route table -------------------------------------------------
	routes, err := gateway.LoadRoutes(cfg.RoutesFile)
	if err != nil {
		return fmt.Errorf("load route table: %w", err)
	}
	log.Info().Int("routes", len(routes)).Str("routes_file", cfg.RoutesFile).Msg("route table loaded")

	// --- upstreams + circuit breakers -------------------------------------
	upstreams, healthChecks, err := buildUpstreams(cfg, redis, gwMetrics, log)
	if err != nil {
		return fmt.Errorf("build upstreams: %w", err)
	}

	// --- http --------------------------------------------------------------
	corsOrigins := append(append(append([]string{},
		cfg.CORSPatientOrigins...), cfg.CORSDoctorOrigins...), cfg.CORSAdminOrigins...)

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
		CORSOrigins:    corsOrigins,
		HealthChecks:   append([]server.HealthCheck{{Name: "redis", Critical: true, Check: redis.Ping}}, healthChecks...),
	})

	if err := gateway.Mount(srv.Router, gateway.Deps{
		Config:        cfg,
		Cache:         redis,
		Authenticator: auth,
		Metrics:       metrics,
		GatewayMetric: gwMetrics,
		Logger:        log,
		Upstreams:     upstreams,
		Routes:        routes,
	}); err != nil {
		return fmt.Errorf("mount routes: %w", err)
	}

	srv.Router.Get("/openapi.yaml", gateway.ServeOpenAPI)
	srv.Router.Get("/docs", gateway.ServeDocsUI)

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
}

// buildTrustedProxies parses TRUSTED_PROXIES. Returning nil hands server.New
// the platform default (private ranges only), which is the right answer for a
// docker-compose or single-ingress deployment. A malformed entry is logged and
// skipped rather than fatal -- one typo in a config map must not stop a pod
// starting -- but it is never treated as a wildcard.
//
// This matters more here than in any backend service: the gateway terminates
// the client connection, so the address it resolves is the one the admin IP
// allowlist and every rate-limit bucket key on.
func buildTrustedProxies(cidrs []string, log zerolog.Logger) *platmw.TrustedProxies {
	if len(cidrs) == 0 {
		return nil
	}
	tp, malformed := platmw.NewTrustedProxies(cidrs)
	for _, m := range malformed {
		log.Error().Str("cidr", m).Msg("ignoring malformed TRUSTED_PROXIES entry")
	}
	return tp
}

// buildUpstreams constructs one Upstream (connection-pooled client + circuit
// breaker) per backend service in the port table, and one read-only health
// check per upstream so /health/ready can report which service is down from
// the gateway alone.
func buildUpstreams(cfg gateway.Config, redis *cache.RedisCache, gm *gateway.GatewayMetrics, log zerolog.Logger) (map[string]*gateway.Upstream, []server.HealthCheck, error) {
	specs := []struct {
		name string
		url  string
	}{
		{"user-service", cfg.UpstreamUserURL},
		{"doctor-service", cfg.UpstreamDoctorURL},
		{"scheduling-service", cfg.UpstreamSchedulingURL},
		{"consultation-service", cfg.UpstreamConsultationURL},
		{"payment-service", cfg.UpstreamPaymentURL},
		{"notification-service", cfg.UpstreamNotificationURL},
		{"record-service", cfg.UpstreamRecordURL},
		{"admin-service", cfg.UpstreamAdminURL},
	}

	upstreams := make(map[string]*gateway.Upstream, len(specs))
	var checks []server.HealthCheck

	for _, s := range specs {
		breaker := gateway.NewBreaker(redis, gateway.BreakerConfig{
			Name:             s.name,
			FailureThreshold: cfg.CircuitFailureThreshold,
			OpenDuration:     time.Duration(cfg.CircuitOpenSeconds) * time.Second,
			ProbeGrace:       time.Duration(cfg.CircuitProbeSeconds) * time.Second,
		}, log)
		name := s.name
		breaker.OnState(func(state gateway.BreakerState) {
			gm.CircuitStateSet(name, state)
		})

		up, err := gateway.NewUpstream(s.name, s.url, breaker, 2, 100*time.Millisecond,
			func() { gm.RetriesTotal.WithLabelValues(name).Inc() }, log)
		if err != nil {
			return nil, nil, fmt.Errorf("build upstream %s: %w", s.name, err)
		}
		upstreams[s.name] = up

		checks = append(checks, server.HealthCheck{
			Name:     "upstream:" + s.name,
			Critical: false, // one dead backend must not pull the whole gateway from rotation
			Check: func(ctx context.Context) error {
				if breaker.Snapshot(ctx) == gateway.StateOpen {
					return fmt.Errorf("circuit breaker open")
				}
				return nil
			},
		})
	}
	return upstreams, checks, nil
}
