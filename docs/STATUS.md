# Build status

**Updated:** 2026-10-13
**Current phase:** Phase 0: Foundations and platform
**Current task:** 0.4 platform/config, platform/log, platform/otel
**Branch:** p0/skeleton

## Done in the current phase
- 0.1 Repository skeleton: `Makefile` (every target in ROADMAP §8.2), `.golangci.yml` (`gosec` + a `depguard` rule per service enforcing the internal/<a>-can't-import-internal/<b> boundary from ROADMAP §5.2, verified against throwaway cross-service imports), `.editorconfig`. `go.mod` and `.gitignore` already existed. `make check` is green.
- 0.2 Local stack: `deploy/compose/docker-compose.yml` with PostgreSQL 16, Redis 7, NATS (JetStream), RabbitMQ (management UI), ClickHouse, an S3-compatible object store, Mailpit and `grafana/otel-lgtm`, each with a healthcheck; `deploy/compose/env.example` and `README.md`. Verified live: `make up` brings all 8 services to `healthy`; `make down` → `make up` preserves data (tested with a postgres row); `make reset` wipes it; every service's host-mapped port answers from outside Docker too, not just its internal healthcheck.
- 0.3 CI workflow: `.github/workflows/ci.yml` with all six §8.7 jobs (fast, integration, e2e, security, build, nightly). Steps whose inputs don't exist yet (no `cmd/*`, no Dockerfiles, no fuzz/chaos tests, no staging URL) print why and exit 0 instead of failing, same no-op pattern as the Makefile. Validated with `actionlint` (clean) and by re-running the exact `make` commands each job calls (all green locally). **Not yet run on actual GitHub Actions** — no PR/push has exercised it on the real runners; first push is the real check.

## Next up
1. 0.4 platform/config, platform/log, platform/otel
2. 0.5 platform/problem, platform/httpx

## Blockers
- (none)

## Decisions pending (see docs/ROADMAP.md §4.3)
- OD-01 Event bus: default NATS JetStream unless decided otherwise

## Notes for next session
- CI tool versions pinned in `.github/workflows/ci.yml`: `golangci-lint v2.11.4` (same version validated locally against `.golangci.yml`), `gitleaks 8.30.1`, `golang.org/x/vuln/cmd/govulncheck v1.9.0`. Bump deliberately, in their own PR, per CLAUDE.md.
- The `build` job's images are built locally in CI but not pushed anywhere yet (no registry/credentials configured) — revisit when a registry is chosen.
- **MinIO is not in the stack.** `minio/minio` has been pulled from Docker Hub entirely — `docker pull` fails on every tag, before and after `docker login`, and the Hub API confirms the repo no longer exists. `deploy/compose/docker-compose.yml`'s `object-storage` service runs SeaweedFS's S3 gateway (`chrislusf/seaweedfs`) instead, exposed on the same ports (9000 API / 9001 status) MinIO would have used. Same S3 API surface, so `internal/platform` object-storage code doesn't need to know which one it's talking to. Revisit if MinIO becomes available again or a managed provider is chosen (OD-19 is unrelated — that's cards/aggregators, not object storage).
- Postgres is on host port **5433**, not 5432 — this machine already has another project's Postgres container bound to 5432. Everything else in the stack kept its usual port.
- Docker healthcheck gotcha: `wget http://localhost:<port>/...` can fail with "Connection refused" from inside a container even though the service is up, if the container's `/etc/hosts` resolves `localhost` to `::1` first and the server only binds the IPv4 wildcard (this is what happened with ClickHouse). Use `127.0.0.1` in healthchecks, not `localhost`.
- ClickHouse took 90–140s to start accepting connections on this machine under load (several unrelated docker projects were also running); its healthcheck has `start_period: 90s` to avoid flapping. `grafana/otel-lgtm` has no `wget`/`curl`-reachable readiness besides its own `/tmp/ready` marker file (it's RHEL-based, no `wget`) — healthcheck uses `test -f /tmp/ready`.
- Toolchain gap carried over from 0.1: `sqlc`, `goose`, `oapi-codegen`, `buf`, `govulncheck`, `k6`, `pnpm` still not installed. Not needed yet, but CI (0.3) should install whatever its jobs need rather than assume a matching local setup.
- `deploy/compose/.env` (gitignored) must exist for `docker compose` to pick up credentials — copy it from `deploy/compose/env.example` once per machine.
