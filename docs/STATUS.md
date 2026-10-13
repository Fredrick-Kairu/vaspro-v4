# Build status

**Updated:** 2026-10-13
**Current phase:** Phase 0: Foundations and platform
**Current task:** 0.6 platform/db
**Branch:** p0/skeleton

## Done in the current phase
- 0.1 Repository skeleton: `Makefile` (every target in ROADMAP §8.2), `.golangci.yml` (`gosec` + a `depguard` rule per service enforcing the internal/<a>-can't-import-internal/<b> boundary from ROADMAP §5.2, verified against throwaway cross-service imports), `.editorconfig`.
- 0.2 Local stack: `deploy/compose/docker-compose.yml` with PostgreSQL 16, Redis 7, NATS (JetStream), RabbitMQ (management UI), ClickHouse, an S3-compatible object store, Mailpit and `grafana/otel-lgtm`, each with a healthcheck. Verified live: all 8 services reach `healthy`; data persists across `down`/`up`, wiped by `reset`.
- 0.3 CI workflow: `.github/workflows/ci.yml`, all six §8.7 jobs. Steps with no inputs yet print why and exit 0. Validated with `actionlint`; **not yet run on actual GitHub Actions**.
- 0.4 platform/config, platform/log, platform/otel: `config.Load[T]()`/`MustLoad[T]()` (env vars + optional `Validate()`); `log.New` (JSON slog, auto-attaches `request_id`/`tenant_id`/`trace_id` from context); `otel.Setup` (OTLP/gRPC trace+metric providers, tested against a real in-process fake collector).
- 0.5 platform/problem, platform/httpx: `docs/errors/catalogue.yaml` (11 codes, one per family — deliberately partial, each service adds its own before its own phase) + `tools/errcodes` generating `problem`'s `Code` constants and lookup table. `problem.New`/`Respond` write RFC 9457 problem+json. `httpx` has all five named middlewares (`RequestID`, `Recover`, `Timeout`, `BodyLimit`, `CORS`) — `Timeout` reimplements `net/http.TimeoutHandler`'s goroutine/buffering pattern since that stdlib handler can't emit anything but a fixed plain-text timeout body; tested under `-race`.

## Next up
1. 0.6 platform/db
2. 0.7 platform/ids, platform/money, platform/clock

## Blockers
- (none)

## Decisions pending (see docs/ROADMAP.md §4.3)
- OD-01 Event bus: default NATS JetStream unless decided otherwise

## Notes for next session
- **MinIO is not in the stack** — pulled from Docker Hub entirely (confirmed via the Hub API, not just a pull error). `deploy/compose/docker-compose.yml`'s `object-storage` service runs SeaweedFS's S3 gateway instead, same ports (9000/9001). Revisit if MinIO returns or a managed provider is chosen.
- Postgres is on host port **5433**, not 5432 (another local project already holds 5432); everything else kept its default port.
- Toolchain gap: `sqlc`, `goose`, `oapi-codegen`, `buf`, `govulncheck`, `k6`, `pnpm` still not installed locally. 0.6 (`platform/db`) needs `goose` and probably `sqlc` — install before starting it.
- `platform/log`'s `trace_id`/`request_id` only appear when the caller uses a `*Context` logging call (`InfoContext`, etc.) with a context that actually carries them — a plain `logger.Info(...)` never gets them. `httpx.RequestID` and `httpx.Recover` already do this correctly; keep doing so in every handler/middleware from here on.
- `platform/otel.Setup`'s OTLP exporters don't dial eagerly — a nil error from `Setup` is not proof the collector is reachable; only `shutdown`'s flush can fail.
