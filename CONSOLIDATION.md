# Consolidation record

This repository is the nine Go services of the VersaLife telemedicine platform
merged into one module. It is built in phases; this file records what each
phase changed and — more importantly — **every place where behaviour changed
rather than merely moved**.

Baseline: the nine repositories at their `main` HEADs on 9 September 2026, each
tagged `pre-consolidation`. All nine built clean and all 71 test packages passed
before anything moved.

---

## Phase 1 — code merge (done)

One repo, one module, one `internal/platform`. **Still nine binaries, nine
databases, nine images.** Nothing about the runtime topology changed.

| Layout | Before | After |
| --- | --- | --- |
| Module | nine repos, each `module telemed` | one `module telemed`, `go 1.26` |
| Platform | nine copies, drifted 4–181 lines | one `internal/platform` |
| Protobuf | `user/v1` triplicated; a fourth divergent copy in `telemed-infra` | one `internal/pb`, one `proto/` |
| Domains | `internal/<pkg>` per repo | `internal/domain/<service>/<pkg>` |
| Gateway | `internal/gateway` | unchanged (becomes `internal/edge` in Phase 3) |
| Entrypoints | nine `cmd/server` | `cmd/<service>` ×9, plus `cmd/signhook` |
| Migrations | nine `migrations/` | `migrations/<domain>/` — still nine databases |
| OpenAPI | nine `openapi.yaml` | `api/<service>.yaml` |

### Behaviour changes — deliberate, and each one adopted knowingly

The platform drift was **not** random. It was an un-propagated set of security
fixes: five services (`consultation`, `payment`, `notification`, `record`,
`admin`) carried hardening that four (`user`, `doctor`, `scheduling`,
`api-gateway`) did not. Merging to one platform necessarily propagates them.

| # | Change | Who it affects | Why the merged version wins |
| --- | --- | --- | --- |
| 1 | `config.IsProd()` is now case- and spelling-tolerant (`prod`/`production`/`live`, trimmed, lowercased) | user, doctor, scheduling, gateway | Every caller uses it to decide whether to **refuse** something — an unauthenticated debug provider, a missing IP allowlist. `ENV=Production` previously read as "not production" and silently re-enabled exactly those. Fails closed now. |
| 2 | `httpx.MaxPage` caps the page number at 100,000 | user, doctor, scheduling, gateway | `offset = (page-1)*perPage` in `int` arithmetic wrapped negative for absurd `?page=` values, and Postgres rejects a negative OFFSET — a 500 on every list endpoint, reachable from a query string. |
| 3 | `Principal.IsAdmin()` / `HasAdminRole()` apply the admin-issuer binding | user, doctor, scheduling, gateway | A bare `HasAnyRole(AdminRoles...)` inside a service layer bypasses `RequireRole`'s issuer check, so user-service could mint an honest token asserting `super_admin`. |
| 4 | `/health/ready` no longer returns an `error` field per dependency | user, doctor, scheduling, gateway | The endpoint is unauthenticated on every listener. A failed `pgxpool.Ping` returned pgconn's `failed to connect to \`host=… user=… database=…\``, handing an internal hostname, DB user and DB name to anyone curling the port. The name of the failing dependency is still reported; the error text is now logged instead. **This is a response-body change** — verified that no code or test in the four affected services reads that field. |
| 5 | `cache.redisOptions` split out of `NewRedis` | user, doctor, scheduling, gateway | Makes "is `REDIS_PASSWORD` actually reaching the client" assertable without a live server. Redis holds OTP hashes, every rate-limit counter and the suspension denylist. |
| 6 | Prometheus route label falls back to `logger.SafePath(r.URL.Path)` for unmatched requests | everyone but admin | An unmatched request has no chi pattern, and the raw path is the one most likely to carry an identifier — so the metrics-label fallback is masked. |

Additive, no behaviour change: `database.UniqueConstraint` (from user-service)
and the `doctor.application_*` event subjects and payloads (from user, doctor,
notification, admin) are now available module-wide.

### Structural changes that required code edits

| Change | Reason |
| --- | --- |
| `internal/analytics` → `internal/domain/admin/analytics` **and** `internal/domain/doctor/analytics` | Two unrelated packages shared one import path. The only true collision in the merge. |
| `platform/storage` is now record-service's superset; admin's narrower copy deleted | Two different `storage` packages could not coexist. record's has `Put/Get/PresignedGet/PresignedPut/Delete/Stat` plus a filesystem implementation for tests. |
| `credentialing.DocumentStore` — a new one-method interface declared at the point of use | admin must presign a scanned NIC but must never be able to `Put`, `Get` or `Delete` one. Narrowing the interface locally enforces that, and keeps the package's test fake to a single method. Method renamed `PresignGet` → `PresignedGet`. |
| `MinIOStorage.PingBucket(ctx, bucket)` added alongside `Ping(ctx)` | admin's readiness probe asserted a specific bucket exists; record's only asserts connectivity. Both semantics preserved rather than one silently replacing the other. |
| `internal/platform/dbtest` → `internal/domain/record/dbtest` | It is record's helper, not shared platform code — it only sat under `platform/` because each service was its own repo. Now mirrors how `testutil` sits inside admin. |
| New `internal/platform/repopath` | Tests reached their migrations with hard-coded `../../migrations`. Depth from a package to the repo root is no longer uniform, and a miscounted `../..` fails at run time inside an integration test as "no migrations found", which reads like a broken database. `repopath` walks up to `go.mod`, so it survives Phase 3 too. |
| gofmt under Go 1.26 | Reformatted `notification/consumer.go`, `notification/model.go` and `gateway/routes_test.go`, which were already unformatted under 1.26's gofmt before the merge. Import blocks across 87 files re-sorted because the rewritten paths sort differently. |

### Verification

```
go build ./...   PASS
go vet ./...     PASS
gofmt -l .       clean
go test ./...    41 packages ok, 0 fail
```

Baseline was 35 unique test-package paths; merged is 41. The difference is
accounted for: nine `cmd/server` packages became six distinct `cmd/<service>`
packages that have tests, and `internal/analytics` — one path in the baseline
because both services used the same name — is correctly two packages now.
No test package was dropped.

Integration tests (`-tags=integration`) and the golden-transcript replay could
not be run here: they need Docker, and the daemon is not available in this
environment. They remain the gate for Phase 2.

---

## Phase 2 — one database, eight schemas (code done; needs Docker to verify)

One database `telemed`, one schema per stateful domain: `svc_user`,
`svc_doctor`, `svc_scheduling`, `svc_consultation`, `svc_payment`,
`svc_notification`, `svc_record`, `svc_admin`. Still **nine binaries** and nine
roles — only the storage layout changed.

The `svc_` prefix is load-bearing: `user` is a reserved word in SQL, so a bare
`CREATE SCHEMA user` is legal only quoted, and every later reference then has to
stay quoted too.

### The plan said "migrations move unchanged". That was wrong.

Four migrations are schema-sensitive and had to be rewritten. Each would have
failed silently or at run time, not at migration time:

| File | What had to change | Why it would have broken |
| --- | --- | --- |
| `admin/000004_analytics.up.sql` | `SET search_path = pg_catalog, public` → `pg_catalog, svc_admin`; four `REFRESH MATERIALIZED VIEW CONCURRENTLY public.*` → `svc_admin.*` | A `SECURITY DEFINER` function pins its own `search_path` — that pin is the whole point, it stops a caller prepending a schema they control. Pinned to `public` it would resolve none of the four views after the move. |
| `admin/000009_audit_truncate_guard.up.sql` | same pin → `pg_catalog, svc_admin` | The audit chain trigger resolves `audit_chain_state` unqualified. Pinned to `public`, every audit insert fails. |
| `scheduling/000002_slots_partitioned.up.sql` | `to_regclass(format('public.%I', …))` → `svc_scheduling.%I`, and the partition `CREATE` is now qualified on both sides | The existence check and the create would have disagreed: the check looks in `public`, the create lands wherever `search_path` points. Result is a fresh `CREATE TABLE` attempt on every call, failing on the second. |
| `scheduling/000004_slot_partitions_bootstrap.up.sql` | `to_regclass('public.slots_default')` → `svc_scheduling.slots_default` | Same class of bug. |

`pgcrypto` is now created once in `public` by the bootstrap migration, so the
`CREATE EXTENSION IF NOT EXISTS` calls in `user/000002` and `admin/000002`
become no-ops. An extension is a database-global object; a second `CREATE` in
another schema errors rather than duplicating.

### New

| Path | What |
| --- | --- |
| `migrations/bootstrap/` | Creates the eight schemas and `pgcrypto`. Runs once, as superuser, before any domain. |
| `scripts/migrate.sh` | Bootstrap, then each domain into its own schema with `search_path` as a **connection parameter**. Each domain keeps its own `schema_migrations` table inside its own schema, so the eight histories stay independent and one domain can roll back without touching the others. |
| roles & grants | Provisioned by `telemed-infra/infra/postgres/init/01-init-databases.sh`, rewritten for schemas, with `scripts/verify-db-privileges.sh` proving every `telemed_<domain>_app` role has USAGE on its own schema and no other, sees zero tables elsewhere, and owns nothing. Kept in infra rather than here so there is one source of truth for the privilege model. |
| `database.Schema` / `SearchPathFor` / `WithSearchPath` / `EnsureSchema` | One source of truth for schema names, shared by production, the migration runner and every integration-test helper. `WithSearchPath` handles both DSN forms — the scheduling suite uses the keyword form against a unix socket, and `url.Parse` does not reject it, it silently drops the parameter. |

### Least privilege is **not** lost in this phase

The plan recorded L1 — losing the nine per-database least-privilege roles — as
the migration's one unavoidable loss. It is not lost here. Nine processes still
connect as nine roles; the boundary just moved from `CONNECT` on a database to
`USAGE` on a schema, and `REVOKE ALL ON SCHEMA … FROM PUBLIC` is what makes that
hold, because PUBLIC gets USAGE on a new schema by default.

**L1 bites in Phase 3, and it is avoidable there too**: a single binary can hold
one connection pool per domain, each connecting as that domain's own role,
instead of one pool as one role with USAGE on all eight schemas. That keeps
server-side enforcement instead of moving it into application code. The roles
this phase creates are what make that option available — the decision belongs
before Phase 3 lands, not after.

### Deferred by design

`specialties`, `working_hours`, `doctor_schedule_settings` and `drugs` stay
duplicated across schemas. Unifying them is a data-model change that alters
behaviour — `svc_doctor.doctor_schedule_settings.buffer_minutes` is NULLable on
purpose, where NULL means "no preference" and 0 means "back-to-back", and
`svc_scheduling`'s is `NOT NULL DEFAULT 5`, which collapses that distinction and
would hand every doctor with no preference a five-minute buffer. Consolidation
must not smuggle in a product change.

### Verification

```
go build ./...                 PASS
go vet ./...                   PASS
go vet -tags=integration ./... PASS   (compiles the integration suites too)
gofmt -l .                     clean
go test ./...                  42 packages ok, 0 fail
```

**Not verified here:** anything that needs a live Postgres. The Docker daemon is
not available in this environment, so `-tags=integration` compiles but does not
run, `scripts/migrate.sh` has not been executed against a real database, and
`scripts/init-schema-roles.sh` has not proved its own assertions. Those are the
gate for accepting Phase 2 — the SQL rewrites above are exactly the kind of
change that compiles fine and fails on contact with a server.

## Phase 3 — one binary (done)

`cmd/telemed` is the platform. The nine `cmd/*-service` entrypoints are gone;
`cmd/signhook` (a separate CLI) remains.

### Shape

Each domain's boot sequence moved out of its `main.go` into
`internal/modules/<domain>`, which returns a `modular.Module`: its pool, its
routes, its workers, its health checks, its closers. `cmd/telemed` builds the
shared dependencies once — logger, metrics, tracing, Redis, NATS, the
authenticator — and asks each enabled domain for a Module.

The wiring inside each module was **moved, not retyped**. It is the same code,
sliced out of the same file, with only the parts the composer now owns removed.

### One binary, still any topology

`TELEMED_DOMAINS` selects what a process runs. Default is everything.

```
telemed                                            # the whole platform
TELEMED_DOMAINS=user  GRPC_PORT=9091 HTTP_PORT=8081 # the old user-service
TELEMED_DOMAINS=edge                HTTP_PORT=8080  # the old api-gateway
```

That is the rollback path, and it needs no second build. A process that does not
hold a domain reaches it exactly as before — over gRPC, with the mesh
credential — because each module asks the registry first and falls back to
dialling.

### Least privilege is kept, not lost

The plan recorded **L1** — losing the nine per-database least-privilege roles —
as the migration's one unavoidable loss. It is not lost, because nothing forces
one process to hold one connection identity.

`dsnResolver` in `cmd/telemed/main.go` gives every domain **its own pool**, as
`telemed_<domain>_app`, with `search_path` pinned to `svc_<domain>`. That role
has no `USAGE` on any other schema, so a bug in the payment domain still cannot
read the user directory even though the code shares an address space — enforced
by Postgres, not by application code. `scripts/verify-db-privileges.sh` in
telemed-infra proves it, and `TestDSNResolver_GivesEachDomainItsOwnRoleAndSchema`
fails if two domains ever share a role.

### The gRPC mesh

Only two production call sites ever dialled: the admin and notification domains
resolving users. Both now get `user.InProcessClient` from the module registry
when the user domain is in the process.

**On the mesh token.** The gRPC interceptor exists because "not routed from the
internet" is a network accident, not an access control — anything that could
open a TCP connection to :9091 could dump the patient directory. A Go method
call from another package in the same binary is not reachable that way, and a
process minting a token for itself and immediately verifying it authenticates
nothing. The in-process path therefore does not re-check it. What does stay
true, and is tested:

- the listener keeps its interceptor whenever it is started, and it still fails
  closed (`grpc_auth_test.go`, unchanged and passing);
- the listener is **off by default** — `EXPOSE_GRPC` gates it. In one process
  nothing dials :9091, and a PII surface nothing uses is a surface nothing
  should be able to reach;
- the adapter is only handed out inside the process that owns the user domain.
- the per-caller authorisation that actually matters was never in the
  interceptor: it is in the user service, which both paths call.

### The edge

`internal/gateway` keeps its name and its 103-route policy table. `Upstream`
gained a `Handler` field; when it is set, `newInProcessProxy` calls the domain's
router instead of dialling it, and reports the same outcome to the same circuit
breaker and the same dependency metric. Auth class, rate-limit class, timeout
class, body cap, admin IP allowlist and `TRUSTED_PROXIES` are untouched — the
gateway's own test suite, including the 444-line route-table contract test,
passes unmodified.

**The one real regression, and its mitigation.** Nine processes each had their
own goroutine budget, so a payment stampede degraded payments. One process
shares one budget. `NewInProcessUpstream` takes a per-domain concurrency limit —
the global `MaxInFlight` divided across the upstreams — so one domain cannot
consume the whole budget and starve OTP verification. Over the limit it returns
the same 503 the breaker already returns, so no caller learns a new failure mode.

### What deliberately did not change

NATS and the transactional outbox. A business change and its event are still
written in one Postgres transaction and relayed afterwards. Replacing that with
a direct call because the consumer now shares an address space would reintroduce
exactly the failure the outbox prevents — "slot booked, nobody notified" — while
looking like a simplification.

### Verification

```
go build ./...                 PASS   (one binary: cmd/telemed, plus cmd/signhook)
go vet ./...                   PASS
go vet -tags=integration ./... PASS
gofmt -l .                     clean
go test ./...                  43 packages ok, 0 fail
```

New tests: `inprocess_test.go` (the adapter satisfies the generated interface,
delegates, and passes errors back unchanged so a NOT_FOUND still stops rather
than retries) and `cmd/telemed/main_test.go` (no two domains share a database
role; `TELEMED_DOMAINS` parsing; the user domain is built first).

**Still not verified:** anything needing live infrastructure. The process has
never been started — no Postgres, no Redis, no NATS, no Docker daemon here — so
boot order, shutdown draining and the golden-transcript replay remain the gate
before this is deployed.
