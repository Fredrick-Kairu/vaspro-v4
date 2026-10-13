# Build status

**Updated:** 2026-10-13
**Current phase:** Phase 0: Foundations and platform
**Current task:** 0.5 platform/problem, platform/httpx
**Branch:** p0/skeleton

## Done in the current phase
- 0.1 Repository skeleton: `Makefile` (every target in ROADMAP §8.2), `.golangci.yml` (`gosec` + a `depguard` rule per service enforcing the internal/<a>-can't-import-internal/<b> boundary from ROADMAP §5.2, verified against throwaway cross-service imports), `.editorconfig`. `go.mod` and `.gitignore` already existed. `make check` is green.
- 0.2 Local stack: `deploy/compose/docker-compose.yml` with PostgreSQL 16, Redis 7, NATS (JetStream), RabbitMQ (management UI), ClickHouse, an S3-compatible object store, Mailpit and `grafana/otel-lgtm`, each with a healthcheck; `deploy/compose/env.example` and `README.md`. Verified live: `make up` brings all 8 services to `healthy`; `make down` → `make up` preserves data (tested with a postgres row); `make reset` wipes it; every service's host-mapped port answers from outside Docker too, not just its internal healthcheck.
- 0.3 CI workflow: `.github/workflows/ci.yml` with all six §8.7 jobs (fast, integration, e2e, security, build, nightly). Steps whose inputs don't exist yet (no `cmd/*`, no Dockerfiles, no fuzz/chaos tests, no staging URL) print why and exit 0 instead of failing, same no-op pattern as the Makefile. Validated with `actionlint` (clean) and by re-running the exact `make` commands each job calls (all green locally). **Not yet run on actual GitHub Actions** — no PR/push has exercised it on the real runners; first push is the real check.
- 0.4 platform/config, platform/log, platform/otel: the first real Go code. `config.Load[T]()`/`MustLoad[T]()` parse env vars via `caarlos0/env` struct tags and call `T.Validate()` if implemented. `log.New(w, level)` returns a JSON `slog.Logger` whose `*Context` calls auto-attach `request_id`/`tenant_id` (set via `log.WithRequestID`/`WithTenantID`) and `trace_id` (from the active OTel span) — no field logged when its source is unset. `otel.Setup(ctx, cfg)` registers global trace/meter providers exporting OTLP/gRPC, returns a `shutdown` func; tested against an in-process fake OTLP collector (not mocks) to confirm spans/metrics are actually delivered. All three: 100%/88%/82% statement coverage, `make check` green with real lint+test now running (no longer a no-op).

## Next up
1. 0.5 platform/problem, platform/httpx
2. 0.6 platform/db

## Blockers
- (none)

## Decisions pending (see docs/ROADMAP.md §4.3)
- OD-01 Event bus: default NATS JetStream unless decided otherwise

## Notes for next session
- Two Makefile bugs found and fixed once real Go code existed (both were invisible while every package was a no-op): `gen-check`'s `git diff` wasn't excluding `go.mod` (a dependency bump isn't codegen drift, but looked like it); `test-contract`/`test-e2e` guarded on the module-wide package list instead of their own path, so they'd have tried to `go test` an empty directory and failed the moment any package anywhere existed. Both fixed in their own commits (`c8b7ceb`, `be46d02`) — worth rereading if another "no packages" guard needs adding later.
- `platform/log`'s `trace_id` comes from whatever OTel span is active in the context the caller passes to `*Context` logging calls (e.g. `logger.InfoContext(ctx, ...)`) — a plain `logger.Info(...)` never gets one. Downstream code (HTTP/gRPC middleware in 0.5+) needs to pass context through consistently for this to be useful.
- `platform/otel.Setup`'s OTLP exporters don't dial eagerly — `Setup` succeeds even against an unreachable endpoint; only `shutdown`'s flush can fail (or just time out, bounded by the context you give it). Don't treat a nil error from `Setup` as proof the collector is reachable.
- CI tool versions pinned in `.github/workflows/ci.yml`: `golangci-lint v2.11.4` (same version validated locally against `.golangci.yml`), `gitleaks 8.30.1`, `golang.org/x/vuln/cmd/govulncheck v1.9.0`. Bump deliberately, in their own PR, per CLAUDE.md.
- The `build` job's images are built locally in CI but not pushed anywhere yet (no registry/credentials configured) — revisit when a registry is chosen.
- **MinIO is not in the stack.** `minio/minio` has been pulled from Docker Hub entirely — `docker pull` fails on every tag, before and after `docker login`, and the Hub API confirms the repo no longer exists. `deploy/compose/docker-compose.yml`'s `object-storage` service runs SeaweedFS's S3 gateway (`chrislusf/seaweedfs`) instead, exposed on the same ports (9000 API / 9001 status) MinIO would have used. Same S3 API surface, so `internal/platform` object-storage code doesn't need to know which one it's talking to. Revisit if MinIO becomes available again or a managed provider is chosen (OD-19 is unrelated — that's cards/aggregators, not object storage).
- Postgres is on host port **5433**, not 5432 — this machine already has another project's Postgres container bound to 5432. Everything else in the stack kept its usual port.
- Docker healthcheck gotcha: `wget http://localhost:<port>/...` can fail with "Connection refused" from inside a container even though the service is up, if the container's `/etc/hosts` resolves `localhost` to `::1` first and the server only binds the IPv4 wildcard (this is what happened with ClickHouse). Use `127.0.0.1` in healthchecks, not `localhost`.
- ClickHouse took 90–140s to start accepting connections on this machine under load (several unrelated docker projects were also running); its healthcheck has `start_period: 90s` to avoid flapping. `grafana/otel-lgtm` has no `wget`/`curl`-reachable readiness besides its own `/tmp/ready` marker file (it's RHEL-based, no `wget`) — healthcheck uses `test -f /tmp/ready`.
- Toolchain gap carried over from 0.1: `sqlc`, `goose`, `oapi-codegen`, `buf`, `govulncheck`, `k6`, `pnpm` still not installed. Not needed yet, but CI (0.3) should install whatever its jobs need rather than assume a matching local setup.
- `deploy/compose/.env` (gitignored) must exist for `docker compose` to pick up credentials — copy it from `deploy/compose/env.example` once per machine.
