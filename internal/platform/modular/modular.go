// Package modular is the seam that lets one binary run any set of domains.
//
// WHY THIS EXISTS
// Before consolidation each domain had its own cmd/server/main.go, and each of
// those files did the same nine things in the same order: load config, build a
// logger, open Postgres, open Redis, connect NATS, build the authenticator,
// wire repositories and services, start background workers, mount routes.
// Eight copies of that sequence is eight places for it to drift, and it had
// already drifted -- one service's readiness probe leaked its connection
// string because the fix landed in the other five.
//
// A Module is one domain's answer to "what do I need running, and what do I
// serve". It owns the parts that are genuinely its own -- its database pool,
// its routes, its workers -- and is handed the parts that are not. The
// composer in cmd/telemed builds the shared dependencies once and asks each
// enabled domain for a Module.
//
// THE POINT OF "ANY SET"
// The default is every domain in one process, which is what consolidation was
// for. But the same binary run with TELEMED_DOMAINS=user is a user-service and
// nothing else, so the nine-process topology is still reachable -- one image,
// one code path, and a rollback that does not need a second build. It is also
// how a domain gets scaled on its own the day one of them needs it.
package modular

import (
	"context"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
	"telemed/internal/platform/server"
)

// Deps are the things every domain is given rather than building.
//
// Redis, NATS and the authenticator are shared deliberately. Eight Redis
// clients to one Redis, or eight JWKS pollers hitting the same two issuers,
// is waste that also multiplies the failure modes -- and the authenticator in
// particular must resolve a token the same way for every domain, which is
// easiest to guarantee when there is one of it.
type Deps struct {
	Log     zerolog.Logger
	Metrics *observability.Metrics
	Redis   cache.Cache
	Broker  *events.NATSBroker
	Auth    *middleware.Authenticator
	Version string
	Env     string

	// Registry lets a domain look up an in-process peer instead of dialling
	// it. See Registry.
	Registry *Registry

	// DSN returns the connection string a domain must use.
	//
	// A domain does NOT read DATABASE_URL for itself any more. In one process
	// there is one environment, so eight domains reading one DATABASE_URL
	// would all connect as the same role to the same schema -- which is
	// exactly the least-privilege collapse this design exists to avoid. The
	// composer owns the mapping from domain to role and search_path, and this
	// is how a module asks for its own.
	DSN func(domain string) (string, error)

	// ExposeGRPC starts the domains' gRPC listeners.
	//
	// Off by default. In one process the peers that used to dial those ports
	// resolve each other through Registry instead, and a port nothing uses is
	// a port nothing should be able to reach -- user-service's is a full PII
	// directory. Set it when the domains are split across processes and the
	// mesh really is a network again.
	ExposeGRPC bool

	// Pool sizing, shared. Eight pools in one process must not each default to
	// the twenty connections a standalone service assumed.
	MaxConns    int32
	MinConns    int32
	MaxConnLife time.Duration
}

// Module is one domain, assembled and ready to be mounted and started.
type Module struct {
	// Name is the domain, e.g. "user". It is the schema suffix, the metrics
	// label, and the value TELEMED_DOMAINS selects on.
	Name string

	// Pool is this domain's own database pool, connected as this domain's own
	// least-privilege role with search_path pinned to its own schema.
	//
	// One pool per domain, not one shared pool, and that is the whole reason
	// the single binary does not forfeit the per-database least-privilege
	// model from security review F14. The boundary is still enforced by
	// Postgres -- telemed_payment_app has no USAGE on svc_user, so a bug in
	// the payment domain cannot read the user directory even though the code
	// now shares an address space. Nil for a stateless domain.
	Pool database.Pool

	// API is mounted under /api/v1.
	API func(chi.Router)

	// Root is mounted at the root, for the handful of paths that are not
	// under /api/v1: user's JWKS document, notification's provider webhooks,
	// payment's gateway callbacks.
	Root func(chi.Router)

	// Health checks are merged into the one readiness probe. Names are
	// prefixed with the domain by the composer, so eight "postgres" checks
	// stay distinguishable.
	Health []server.HealthCheck

	// Workers are the background loops: outbox relays, event consumers,
	// dispatchers, crons. Each is run in its own goroutine and is expected to
	// return when its context is cancelled.
	Workers []Worker

	// Closers run in reverse order during shutdown, after the workers' context
	// is cancelled. The pool is closed by the composer; anything else the
	// domain opened belongs here.
	Closers []func()
}

// Worker is one named background loop.
//
// Named because "a goroutine stopped" is not a useful log line at three in the
// morning: the composer logs which one, and whether the context was already
// cancelled (an orderly shutdown) or not (a real failure).
type Worker struct {
	Name string
	Run  func(context.Context) error
}

// SimpleWorker adapts a loop that cannot fail.
func SimpleWorker(name string, run func(context.Context)) Worker {
	return Worker{Name: name, Run: func(ctx context.Context) error { run(ctx); return nil }}
}

// Registry holds the in-process implementations domains expose to each other.
//
// WHY NOT JUST CALL THE OTHER DOMAIN'S SERVICE
// Because the caller must not care whether the callee is in this process. The
// admin and notification domains need to resolve a user's name and phone; they
// were written against userv1.UserServiceClient and should stay that way. When
// the user domain is loaded here, the registry hands them an adapter that
// calls it directly; when it is not -- TELEMED_DOMAINS=admin, say -- they dial
// it over gRPC exactly as before. One code path in the caller either way.
type Registry struct {
	values map[string]any
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{values: map[string]any{}} }

// Provide records an in-process implementation under a key.
func (r *Registry) Provide(key string, value any) {
	if r == nil {
		return
	}
	r.values[key] = value
}

// Lookup returns the implementation recorded under key, if any.
func (r *Registry) Lookup(key string) (any, bool) {
	if r == nil {
		return nil, false
	}
	v, ok := r.values[key]
	return v, ok
}

// Keys under which domains publish themselves.
const (
	// KeyUserDirectory is a userv1.UserServiceClient backed by the user
	// domain's own gRPC service implementation.
	KeyUserDirectory = "user.directory"
)
