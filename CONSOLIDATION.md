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

## Phase 2 — one database, nine schemas (next)

Still nine binaries. `telemed` with schemas `svc_user … svc_admin`; the 51
migration files move unchanged apart from a `search_path` header. The four
tables whose names collide with **incompatible** schemas — `specialties`,
`working_hours`, `doctor_schedule_settings`, `drugs` — stay separate by design.
Unifying them is a data-model change, not a consolidation, and is filed as
follow-up work.

## Phase 3 — one binary

`cmd/telemed`; gRPC clients become in-process adapters **carrying the
`grpcauth` authorisation checks**; `internal/gateway` becomes `internal/edge`
over the unchanged 103-route policy table. NATS and the transactional outbox
stay exactly as they are.
