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
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"telemed/internal/domain/consultation/signal"
	"telemed/internal/platform/cache"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
	"telemed/internal/platform/server"
	"telemed/internal/platform/testkit"
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

	// Outbox is the test surface's capture buffer, and is non-nil ONLY when
	// config.Base.TestModeEnabled reported true.
	//
	// A domain that can deliver something to a human -- an OTP, an email --
	// checks this and, when it is set, captures instead of delivering. Nil is
	// the normal case and means "deliver for real", so the check reads the
	// safe way round: forgetting it delivers, it does not leak.
	Outbox *testkit.Outbox

	// DSN returns the connection string a domain must use.
	//
	// A domain does NOT read DATABASE_URL for itself any more. In one process
	// there is one environment, so eight domains reading one DATABASE_URL
	// would all connect as the same role to the same schema -- which is
	// exactly the least-privilege collapse this design exists to avoid. The
	// composer owns the mapping from domain to role and search_path, and this
	// is how a module asks for its own.
	DSN func(domain string) (string, error)

	// Signal is the platform's own WebRTC signalling configuration.
	//
	// Read ONCE by the composer and handed down, not read per module. That is
	// load-bearing: SIGNAL_SECRET defaults to a randomly generated key when
	// unset, so two modules reading it independently would each generate their
	// own, and a room token minted by one would fail verification in the
	// other -- intermittently, and looking exactly like a network fault.
	Signal SignalConfig

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

	// Raw maps a path prefix to a handler mounted AHEAD of the shared
	// middleware chain, for endpoints that hijack the connection.
	//
	// Everything in that chain assumes a request that begins, produces a
	// response, and ends. A websocket does none of those: the 30-second
	// request timeout would cancel its context mid-call, the compression
	// middleware and the metrics wrapper replace the ResponseWriter with one
	// whose Hijack support is incidental rather than guaranteed, and the
	// latency histogram would record one observation per consultation. A
	// long-lived connection needs the chain skipped, not tuned, so this is a
	// separate door rather than an exemption inside the existing one.
	//
	// The trade is explicit: a Raw handler gets no request id, no logging, no
	// metrics and no request timeout, and owns all four itself.
	Raw map[string]http.Handler

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

// SignalConfig is what a domain needs to run or mint for the signalling stack.
//
// This package importing a domain package is a smell, and it is taken
// knowingly: the alternative is flattening ICEProvider -- an interface with
// two genuinely different implementations -- into a config struct here, which
// would put the choice between coturn and a hosted service in the wrong place
// entirely.
type SignalConfig struct {
	// Secret signs and verifies room tokens. Never empty in practice: the
	// composer generates one when unset.
	Secret []byte
	// ICE mints the STUN/TURN servers handed to each browser.
	ICE signal.ICEProvider
	// AllowedOrigins gates the websocket upgrade. Empty means any origin,
	// which is correct for the dev test surface and wrong for a consultation.
	AllowedOrigins []string
	// PublicURL is the absolute ws(s):// address browsers connect to.
	PublicURL string
	// TokenTTL is how long a room token lasts. It must outlast a
	// consultation, because the token is re-presented on every reconnect.
	TokenTTL time.Duration
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

// RedisClient exposes the underlying go-redis client when the shared cache is
// backed by a real Redis.
//
// Deps.Redis is deliberately the narrow cache.Cache interface -- get, set,
// incr, lock, sorted sets -- and almost every domain should want nothing more.
// Pub/sub is the exception: it is not a cache operation, it does not belong on
// that interface, and adding it there would put four methods no other domain
// uses in front of all of them. This is the escape hatch, and ok is false for
// a test fake, which is the caller's cue to degrade rather than panic.
func (d Deps) RedisClient() (redis.UniversalClient, bool) {
	c, ok := d.Redis.(interface{ Client() redis.UniversalClient })
	if !ok {
		return nil, false
	}
	return c.Client(), true
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

	// KeyAdminDirectory is a middleware.RoleResolver backed by the admin
	// domain's admin_users table. The composer attaches it to the shared
	// authenticator so a token from an identity provider that carries no
	// roles -- Cloudflare Access -- still arrives at a handler with the roles
	// this platform granted that person.
	KeyAdminDirectory = "admin.directory"

	// KeySignalHub is the *signal.Hub the consultation domain runs. The
	// developer test surface looks it up so /test/rooms exercises the real
	// signalling stack rather than a second one standing beside it.
	KeySignalHub = "consultation.signalhub"

	// KeyDoctorDirectory is a Names(ctx, []uuid.UUID) map[uuid.UUID]string
	// resolver backed by the doctor domain's doctors table. Scheduling and
	// consultation use it to label a patient's counterpart_name.
	KeyDoctorDirectory = "doctor.directory"

	// KeyDoctorAccountActivator is a doctor.AccountActivator implemented by
	// the user domain. Doctor-service calls it synchronously on admin Accept
	// so a login account exists before the reviewer leaves the queue.
	KeyDoctorAccountActivator = "user.doctor_account_activator"

	// KeyDoctorApplications is a user.DoctorApplications implementation
	// backed by the doctor domain's doctor.Service for in-process account creation/attach.
	KeyDoctorApplications = "doctor.applications"

	// KeyDoctorApplicationVerifier is an implementation of credentialing.ApplicationVerifier
	// and credentialing.PendingApplicationSource backed by doctor.Service.
	KeyDoctorApplicationVerifier = "doctor.application_verifier"

	// KeyDoctorCredentialImages is a prescriptions.DoctorCredentialImages
	// implementation backed by the doctor domain's doctor_documents table.
	// The record domain looks this up to resolve a doctor's uploaded
	// signature and seal object keys when rendering a prescription PDF --
	// only the two keys cross the boundary in-process, never a row scanned
	// by record-service's own SQL (ADR-004), and never a network hop while
	// both domains run in this one process.
	KeyDoctorCredentialImages = "doctor.credential_images"
)
