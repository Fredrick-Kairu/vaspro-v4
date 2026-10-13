# Build status

**Updated:** 2026-10-13
**Current phase:** Phase 0: Foundations and platform
**Current task:** 0.7 platform/ids, platform/money, platform/clock
**Branch:** p0/skeleton

## Done in the current phase
- 0.1 Repository skeleton: `Makefile` (every target in ROADMAP §8.2), `.golangci.yml` (`gosec` + a `depguard` rule per service enforcing the internal/<a>-can't-import-internal/<b> boundary from ROADMAP §5.2, verified against throwaway cross-service imports), `.editorconfig`.
- 0.2 Local stack: `deploy/compose/docker-compose.yml` with PostgreSQL 16, Redis 7, NATS (JetStream), RabbitMQ (management UI), ClickHouse, an S3-compatible object store, Mailpit and `grafana/otel-lgtm`, each with a healthcheck. Verified live: all 8 services reach `healthy`; data persists across `down`/`up`, wiped by `reset`.
- 0.3 CI workflow: `.github/workflows/ci.yml`, all six §8.7 jobs. Steps with no inputs yet print why and exit 0. Validated with `actionlint`; **not yet run on actual GitHub Actions**.
- 0.4 platform/config, platform/log, platform/otel: `config.Load[T]()`/`MustLoad[T]()` (env vars + optional `Validate()`); `log.New` (JSON slog, auto-attaches `request_id`/`tenant_id`/`trace_id` from context); `otel.Setup` (OTLP/gRPC trace+metric providers, tested against a real in-process fake collector).
- 0.5 platform/problem, platform/httpx: `docs/errors/catalogue.yaml` (11 codes, one per family — deliberately partial, each service adds its own before its own phase) + `tools/errcodes` generating `problem`'s `Code` constants and lookup table. `problem.New`/`Respond` write RFC 9457 problem+json. `httpx` has all five named middlewares (`RequestID`, `Recover`, `Timeout`, `BodyLimit`, `CORS`) — `Timeout` reimplements `net/http.TimeoutHandler`'s goroutine/buffering pattern since that stdlib handler can't emit anything but a fixed plain-text timeout body; tested under `-race`.
- 0.6 platform/db: `Pool`/`WithTenantTx` (pgx; sets `app.tenant_id` via `set_config(..., true)`, never string-built SQL). The integration test (build tag `integration`, `testcontainers-go`'s postgres module — `platform/testkit` doesn't exist until 0.12) connects as a dedicated non-superuser role to prove real cross-tenant read/write isolation, not just that the code runs. **Found and fixed a real bug in the canonical RLS pattern itself** (`docs/design/data-model.md` §1 and `.claude/rules/migrations.md`) in the process — see below. No real migrations or `sqlc.yaml` yet (nothing to migrate/generate until 1.1); `goose` installed and pinned, `sqlc` install still pending (see below).

## Next up
1. 0.7 platform/ids, platform/money, platform/clock
2. 0.8 platform/idempotency, platform/outbox, platform/inbox, platform/events

## Blockers
- (none)

## Decisions pending (see docs/ROADMAP.md §4.3)
- OD-01 Event bus: default NATS JetStream unless decided otherwise

## Notes for next session
- **RLS pattern bug, fixed:** the bare `current_setting('app.tenant_id')::uuid` pattern data model §1 documented errors instead of failing closed, on any pooled connection that previously ran a `WithTenantTx` — Postgres returns `''` (not `NULL`) from `current_setting(name, true)` once a session has touched that setting and the transaction ended, and `''::uuid` doesn't parse. Fixed to `nullif(current_setting('app.tenant_id', true), '')::uuid`, plus added `WITH CHECK` (the original only had `USING`, so a cross-tenant *write* would have been silently invisible rather than rejected) and `FORCE ROW LEVEL SECURITY` (binds the table owner too). Every future migration must use the corrected pattern now in data model §1 and `.claude/rules/migrations.md` — don't copy the old one from memory.
- `sqlc` (`go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1`) was still stuck installing (near-zero CPU usage after 25+ minutes — looked hung, not just slow) when this task finished. Not needed until 1.1 (first real `store/queries/*.sql`), but **verify/retry the install before starting that task.** `goose@v3.29.0` installed fine.
- `gen-check` was fixed a second time: it's now scoped to `git diff -- ':(glob)**/*_gen.*'` instead of the whole tree (or a growing list of one-off exclusions). Every future code generator's output must be named `*_gen.<ext>` for this to keep working — set that in whatever config sqlc/buf/oapi-codegen get.
- **MinIO is not in the stack** — pulled from Docker Hub entirely (confirmed via the Hub API, not just a pull error). `deploy/compose/docker-compose.yml`'s `object-storage` service runs SeaweedFS's S3 gateway instead, same ports (9000/9001).
- Postgres (compose) is on host port **5433**, not 5432 (another local project already holds 5432).
- `platform/log`'s `trace_id`/`request_id` only appear on `*Context` logging calls with a context that actually carries them.
- `platform/otel.Setup`'s OTLP exporters don't dial eagerly — a nil error from `Setup` is not proof the collector is reachable.
