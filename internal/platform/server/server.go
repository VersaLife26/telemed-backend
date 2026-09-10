// Package server assembles the HTTP stack: a chi router with the standard
// middleware chain, health endpoints, and graceful shutdown.
//
// Every telemed service calls server.New, registers its routes, and calls Run.
// Keeping the assembly in one place is why an operator can attach to any of the
// ten services and find the same /health, /metrics and log format.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
)

// HealthCheck probes one dependency. Return an error to mark the service not
// ready; readiness failures pull the pod out of the load balancer without
// killing it, which is what you want during a brief database failover.
type HealthCheck struct {
	Name  string
	Check func(context.Context) error
	// Critical checks fail readiness. Non-critical ones are reported in the
	// body but do not remove the pod from rotation -- a notification provider
	// being down must not stop patients joining consultations.
	Critical bool
}

// Options configures the server.
type Options struct {
	ServiceName    string
	Version        string
	Env            string
	Port           int
	Logger         zerolog.Logger
	Metrics        *observability.Metrics
	RequestTimeout time.Duration

	// RawHandlers maps a path prefix to a handler served AHEAD of the router
	// and its middleware, for endpoints that hijack the connection.
	//
	// The chain below is built for requests that finish: chimw.Timeout
	// cancels the context after RequestTimeout, chimw.Compress and the metrics
	// middleware replace the ResponseWriter, and the latency histogram assumes
	// a response. A websocket satisfies none of that, so it is routed around
	// the chain rather than exempted inside it -- an exemption is a condition
	// somebody later gets wrong, a separate door is not.
	//
	// A raw handler therefore has no request id, no access log, no metrics and
	// no timeout, and is responsible for its own. Longest matching prefix
	// wins; everything unmatched goes to the router as before.
	RawHandlers   map[string]http.Handler
	ShutdownGrace time.Duration
	CORSOrigins   []string
	HealthChecks  []HealthCheck
	// TrustedProxies are the networks whose X-Forwarded-For we believe. nil
	// means the private ranges only -- never the public internet.
	TrustedProxies *middleware.TrustedProxies
}

// Server wraps the router and the lifecycle.
type Server struct {
	Router *chi.Mux
	opts   Options
	http   *http.Server

	mu     sync.RWMutex
	ready  bool
	checks []HealthCheck
}

// AddHealthChecks appends to the readiness probe after construction.
//
// The edge's per-upstream checks cannot be known when the server is built:
// they are created while wiring the upstreams, which needs the router the
// server owns. Guarded by the same lock the probe reads under.
func (s *Server) AddHealthChecks(checks ...HealthCheck) {
	if len(checks) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, checks...)
}

// New builds the router with the standard middleware chain already applied.
func New(o Options) *Server {
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 30 * time.Second
	}
	if o.ShutdownGrace <= 0 {
		o.ShutdownGrace = 20 * time.Second
	}

	r := chi.NewRouter()
	s := &Server{Router: r, opts: o, checks: o.HealthChecks}

	// Order matters. RequestID first so every later line can reference it;
	// Recoverer before anything that can panic; Timeout last so it wraps only
	// handler work, not middleware setup.
	r.Use(chimw.RequestID)
	// Deliberately not chimw.RealIP: it is deprecated because it rewrites
	// RemoteAddr from client-controlled headers (GHSA-3fxj-6jh8-hvhx). Our
	// admin IP allowlist depends on this value, so it must not be forgeable.
	r.Use(middleware.RealIP(o.TrustedProxies))
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.RequestLogger(o.Logger))
	r.Use(recoverer(o.Logger))
	if o.Metrics != nil {
		r.Use(o.Metrics.Middleware)
	}
	r.Use(corsMiddleware(o.CORSOrigins))
	r.Use(chimw.Compress(5, "application/json"))
	r.Use(chimw.Timeout(o.RequestTimeout))

	// otelhttp propagates trace context from the gateway through to the
	// database spans without any per-handler code.
	r.Use(func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, o.ServiceName)
	})

	s.mountOperational()
	return s
}

// handler returns what the http.Server actually serves: the raw handlers in
// front of the router, or just the router when there are none.
//
// http.ServeMux rather than a chi middleware doing the same job, because a
// middleware would still sit inside the chain it exists to escape.
func (s *Server) handler() http.Handler {
	if len(s.opts.RawHandlers) == 0 {
		return s.Router
	}
	mux := http.NewServeMux()
	mux.Handle("/", s.Router)
	for prefix, h := range s.opts.RawHandlers {
		// Registered as an exact path, not a subtree: a raw handler is one
		// endpoint, and a trailing-slash subtree pattern would take every path
		// beneath it out of the router -- and out of its access log, metrics
		// and timeout -- which is the opposite of a narrow exception.
		mux.Handle(prefix, h)
		s.opts.Logger.Warn().
			Str("path", prefix).
			Msg("raw handler mounted ahead of the middleware chain: no request timeout, log or metrics on this path")
	}
	return mux
}

// mountOperational installs /health, /health/live, /health/ready and /metrics.
func (s *Server) mountOperational() {
	// Liveness answers "is the process wedged?". It must never touch a
	// dependency: a database outage should not make Kubernetes restart every
	// pod, which turns a recoverable incident into an outage.
	s.Router.Get("/health/live", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, r, http.StatusOK, map[string]string{"status": "alive", "service": s.opts.ServiceName})
	})

	s.Router.Get("/health/ready", s.readinessHandler)
	s.Router.Get("/health", s.readinessHandler)

	if s.opts.Metrics != nil {
		s.Router.Handle("/metrics", s.opts.Metrics.Handler())
	}

	s.Router.Get("/version", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, r, http.StatusOK, map[string]string{
			"service": s.opts.ServiceName,
			"version": s.opts.Version,
			"env":     s.opts.Env,
			"go":      runtimeVersion(),
		})
	})
}

// checkResult is what a readiness probe is told about one dependency.
//
// There is deliberately no Error field. See readinessHandler: this response is
// served unauthenticated on every service listener, and the error text from a
// failed pgxpool.Ping is a connection string.
type checkResult struct {
	Status string `json:"status"`
}

func (s *Server) readinessHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ready := s.ready
	checks := s.checks
	s.mu.RUnlock()

	results := make(map[string]checkResult, len(checks)+1)
	status := http.StatusOK

	if !ready {
		// Set before the listener starts and cleared on SIGTERM, so a draining
		// pod stops accepting new traffic before it stops serving in-flight
		// requests.
		results["lifecycle"] = checkResult{Status: "draining"}
		status = http.StatusServiceUnavailable
	}

	for _, c := range checks {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		err := c.Check(ctx)
		cancel()

		if err != nil {
			// The error text is logged, never served.
			//
			// /health, /health/ready and /metrics are mounted on the ROOT
			// router with no authentication, on every service listener --
			// admin-service's IP allowlist applies only inside /api/v1/admin,
			// so it does not cover them. A pgxpool.Ping failure returns
			// pgconn's `failed to connect to \`host=... user=... database=...\``,
			// which hands an internal hostname, a database username and a
			// database name to anyone who curls the port during an incident.
			// Redis and MinIO leak their endpoints the same way.
			//
			// A readiness probe needs to know WHICH dependency is down, which
			// the name already says. It does not need the connection string.
			s.opts.Logger.Error().Err(err).
				Str("dependency", c.Name).
				Bool("critical", c.Critical).
				Msg("readiness check failed")
			results[c.Name] = checkResult{Status: "down"}
			if s.opts.Metrics != nil {
				s.opts.Metrics.DependencyUp.WithLabelValues(c.Name).Set(0)
			}
			if c.Critical {
				status = http.StatusServiceUnavailable
			}
			continue
		}
		results[c.Name] = checkResult{Status: "up"}
		if s.opts.Metrics != nil {
			s.opts.Metrics.DependencyUp.WithLabelValues(c.Name).Set(1)
		}
	}

	overall := "ok"
	if status != http.StatusOK {
		overall = "degraded"
	}
	httpx.JSON(w, r, status, map[string]any{
		"status":  overall,
		"service": s.opts.ServiceName,
		"version": s.opts.Version,
		"checks":  results,
	})
}

// AddHealthCheck registers a dependency probe after construction.
func (s *Server) AddHealthCheck(c HealthCheck) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, c)
}

// Run starts the listener and blocks until SIGINT/SIGTERM, then drains.
//
// The shutdown sequence is deliberate:
//  1. flip readiness to false so the load balancer stops sending new requests
//  2. wait a beat for that to propagate
//  3. stop accepting connections and let in-flight requests finish
//
// Skipping step 1 is why naive services drop requests on every deploy.
func (s *Server) Run(ctx context.Context) error {
	s.http = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.opts.Port),
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// Long enough for a slow 3G client in Sri Lanka to receive a PDF, short
		// enough that a stuck write cannot pin a connection forever.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return ctx },
	}

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		s.opts.Logger.Info().
			Int("port", s.opts.Port).
			Str("version", s.opts.Version).
			Msg("http server listening")
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	s.setReady(true)

	select {
	case err := <-errCh:
		return fmt.Errorf("server: listen: %w", err)
	case <-sigCtx.Done():
		s.opts.Logger.Info().Msg("shutdown signal received, draining")
	}

	s.setReady(false)

	// Give the ingress one readiness interval to notice before we stop
	// accepting. Two seconds covers the default 1s probe period with margin.
	drainDelay := 2 * time.Second
	if s.opts.Env == "dev" {
		drainDelay = 0
	}
	time.Sleep(drainDelay)

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.ShutdownGrace)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server: graceful shutdown: %w", err)
	}
	s.opts.Logger.Info().Msg("http server stopped cleanly")
	return nil
}

func (s *Server) setReady(v bool) {
	s.mu.Lock()
	s.ready = v
	s.mu.Unlock()
}

// recoverer turns a handler panic into a 500 plus a stack trace in the log,
// rather than killing the process and every other in-flight request with it.
func recoverer(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A panic recovery path has no downstream call to pass a context
			// to -- it only logs and writes a response.
			//nolint:contextcheck // nothing downstream of a recover() takes a context.
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the documented way for a handler to
				// bail out; re-panicking preserves that contract.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.Error().
					Interface("panic", rec).
					Bytes("stack", stack()).
					Str("path", r.URL.Path).
					Str("request_id", chimw.GetReqID(r.Context())).
					Msg("recovered from handler panic")
				httpx.Error(w, r, httpx.ErrInternal)
			}()
			next.ServeHTTP(w, r)
		})
	}
}
