# Build status

**Updated:** 2026-10-13
**Current phase:** Phase 0: Foundations and platform
**Current task:** 0.10 platform/packs
**Branch:** p0/skeleton

## Done in the current phase
- 0.1 Repository skeleton: `Makefile`, `.golangci.yml` (`gosec` + per-service `depguard` boundary), `.editorconfig`.
- 0.2 Local stack: `deploy/compose/docker-compose.yml`, 8 services with healthchecks, verified live.
- 0.3 CI workflow: `.github/workflows/ci.yml`, all six §8.7 jobs, validated with `actionlint`. **Not yet run on actual GitHub Actions.**
- 0.4 platform/config, platform/log, platform/otel.
- 0.5 platform/problem, platform/httpx: error catalogue + generated `Code` constants; problem+json; all five middlewares.
- 0.6 platform/db: `Pool`/`WithTenantTx`. Found and fixed a real bug in the canonical RLS pattern (see below).
- 0.7 platform/ids, platform/money, platform/clock: `ids.New`/`NewPrefixed`; `money.Allocate`/`SplitVATInclusive`/`AddVATExclusive` (no floats, verified against the data model's worked example exactly); `clock.Real`/`Fake`.
- 0.8 platform/events, platform/outbox, platform/inbox, platform/idempotency: event envelope, transactional outbox + relay (via a `Publisher` interface, deliberately decoupled from NATS), inbox dedup guard, idempotency-key middleware/store. Added catalogue code `VAL-4001`.
- 0.9 platform/bus, platform/cache: `bus.Bus` wraps NATS JetStream — `Publish` satisfies `outbox.Publisher` directly (closes the 0.8 interface), `Consume` runs a durable pull consumer with retries (`Nak` on handler error) and a hand-rolled dead-letter subject (JetStream has no client-level "move to this subject" config, so `Consume` checks delivery count itself and publishes to the dead-letter subject + `Term`s on the final attempt). `cache.Cache` embeds `*redis.Client` (same shape as `db.Pool` over `*pgxpool.Pool`) — no wrapper methods, since sessions/rate-limiting don't exist yet to narrow the surface to. Both integration-tested against real containers; `bus`'s tests include a full Postgres+NATS round trip proving `Bus.Publish` and `outbox.Relay` actually fit together, not just type-check.

## Next up
1. 0.10 platform/packs
2. 0.11 platform/crypto

## Blockers
- (none)

## Decisions pending (see docs/ROADMAP.md §4.3)
- OD-01 Event bus: default NATS JetStream unless decided otherwise

## Notes for next session
- `go mod tidy` silently downgraded `github.com/nats-io/nats.go` from `v1.54.0` to `v1.33.1` partway through 0.9 (presumably satisfying some other module's lower transitive requirement) — caught it because a diagnostic flagged a broken import after a tidy run that otherwise reported success. Re-ran `go get github.com/nats-io/nats.go@v1.54.0` explicitly and it held on subsequent tidies. If any pinned version mysteriously drops after `go mod tidy`, check `go.mod` immediately rather than trusting the command's own success output.
- `platform/bus.Consume`'s dead-lettering is entirely client-side (delivery-count check + manual publish + `Term`) — JetStream has no server-side "move to this subject after N failures" config at this API surface. Any change to retry/DLQ behavior happens in `platform/bus`, not NATS server config.
- `platform/idempotency.Middleware` has a documented, deliberate gap: concurrent duplicate requests on the same key both run the handler (no lock/poll on an in-flight claim) — fine until something actually issues concurrent duplicates.
- `processed_events`' DDL (`internal/platform/inbox`) was inferred from data model §1's prose, not copied from an explicit snippet like `outbox_events`/`idempotency_keys` got — sanity-check it once a real service migration creates it.
- **RLS pattern bug, fixed (0.6):** the bare `current_setting('app.tenant_id')::uuid` pattern data model §1 documented errors instead of failing closed on a reused pooled connection. Fixed to `nullif(current_setting('app.tenant_id', true), '')::uuid` + `WITH CHECK` + `FORCE ROW LEVEL SECURITY`, now in data model §1 and `.claude/rules/migrations.md` — use that, not the version from memory.
- `gen-check` is scoped to `git diff -- ':(glob)**/*_gen.*'`. Every future generator's output must be named `*_gen.<ext>` for this to keep working.
- **MinIO is not in the stack** — `deploy/compose/docker-compose.yml`'s `object-storage` service runs SeaweedFS's S3 gateway instead (same ports, 9000/9001). Postgres there is on host port **5433**, not 5432.
