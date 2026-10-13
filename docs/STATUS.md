# Build status

**Updated:** 2026-10-13
**Current phase:** Phase 0: Foundations and platform
**Current task:** 0.9 platform/bus, platform/cache
**Branch:** p0/skeleton

## Done in the current phase
- 0.1 Repository skeleton: `Makefile`, `.golangci.yml` (`gosec` + per-service `depguard` boundary), `.editorconfig`.
- 0.2 Local stack: `deploy/compose/docker-compose.yml`, 8 services with healthchecks, verified live.
- 0.3 CI workflow: `.github/workflows/ci.yml`, all six §8.7 jobs, validated with `actionlint`. **Not yet run on actual GitHub Actions.**
- 0.4 platform/config, platform/log, platform/otel.
- 0.5 platform/problem, platform/httpx: error catalogue + generated `Code` constants; problem+json; all five middlewares.
- 0.6 platform/db: `Pool`/`WithTenantTx`. Found and fixed a real bug in the canonical RLS pattern (see below).
- 0.7 platform/ids, platform/money, platform/clock: `ids.New`/`NewPrefixed`; `money.Allocate`/`SplitVATInclusive`/`AddVATExclusive` (no floats, verified against the data model's worked example exactly); `clock.Real`/`Fake`.
- 0.8 platform/events, platform/outbox, platform/inbox, platform/idempotency: `events.New` builds the envelope (id/type/version/tenant_id/occurred_at/trace_id/data). `outbox.Write` persists inside the caller's transaction; `Relay.RelayOnce` publishes unpublished rows oldest-first through a `Publisher` interface — deliberately narrow, so it doesn't need `platform/bus` (0.9) to exist yet. `inbox.MarkProcessed` is the dedup guard. `idempotency.Middleware`/`Store` make write endpoints replay-safe. Added catalogue code `VAL-4001` for the "same key, different body" conflict. All four integration-tested (build tag `integration`, real Postgres via `testcontainers-go`) against the exact Phase 0 scenario: simulated relay crash → event delivered twice → `inbox` applies it once.

## Next up
1. 0.9 platform/bus, platform/cache
2. 0.10 platform/packs

## Blockers
- (none)

## Decisions pending (see docs/ROADMAP.md §4.3)
- OD-01 Event bus: default NATS JetStream unless decided otherwise

## Notes for next session
- `platform/outbox.Publisher` (`Publish(ctx, subject, data) error`) is the interface 0.9's NATS-backed `platform/bus` needs to satisfy for `Relay` to actually ship events.
- `platform/idempotency.Middleware` has a documented, deliberate gap: concurrent duplicate requests on the same key both run the handler (no lock/poll on an in-flight claim) — fine until something actually issues concurrent duplicates.
- `processed_events`' DDL (`internal/platform/inbox`) was inferred from data model §1's prose, not copied from an explicit snippet like `outbox_events`/`idempotency_keys` got — sanity-check it once a real service migration creates it.
- **RLS pattern bug, fixed (0.6):** the bare `current_setting('app.tenant_id')::uuid` pattern data model §1 documented errors instead of failing closed on a reused pooled connection. Fixed to `nullif(current_setting('app.tenant_id', true), '')::uuid` + `WITH CHECK` + `FORCE ROW LEVEL SECURITY`, now in data model §1 and `.claude/rules/migrations.md` — use that, not the version from memory.
- `gen-check` is scoped to `git diff -- ':(glob)**/*_gen.*'`. Every future generator's output must be named `*_gen.<ext>` for this to keep working.
- **MinIO is not in the stack** — `deploy/compose/docker-compose.yml`'s `object-storage` service runs SeaweedFS's S3 gateway instead (same ports, 9000/9001). Postgres there is on host port **5433**, not 5432.
