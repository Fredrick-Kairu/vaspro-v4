# Build status

**Updated:** 2026-10-13
**Current phase:** Phase 0: Foundations and platform
**Current task:** 0.8 platform/idempotency, platform/outbox, platform/inbox, platform/events
**Branch:** p0/skeleton

## Done in the current phase
- 0.1 Repository skeleton: `Makefile`, `.golangci.yml` (`gosec` + per-service `depguard` boundary), `.editorconfig`.
- 0.2 Local stack: `deploy/compose/docker-compose.yml`, 8 services with healthchecks, verified live.
- 0.3 CI workflow: `.github/workflows/ci.yml`, all six §8.7 jobs, validated with `actionlint`. **Not yet run on actual GitHub Actions.**
- 0.4 platform/config, platform/log, platform/otel.
- 0.5 platform/problem, platform/httpx: error catalogue + generated `Code` constants; problem+json; all five middlewares.
- 0.6 platform/db: `Pool`/`WithTenantTx`. Found and fixed a real bug in the canonical RLS pattern (see below).
- 0.7 platform/ids, platform/money, platform/clock: `ids.New`/`NewPrefixed` (UUIDv7 + `msg_...`-style public IDs); `money.Allocate`/`SplitVATInclusive`/`AddVATExclusive` (no floats anywhere, `math/big` throughout, verified against the data model's worked example — 190.00 at 16% inclusive → exactly 163.79/26.21 — plus a UGX zero-decimal case and a 1000-iteration randomized property test that `Allocate`'s parts always sum back to the total); `clock.Real`/`Fake`.

## Next up
1. 0.8 platform/idempotency, platform/outbox, platform/inbox, platform/events
2. 0.9 platform/bus, platform/cache

## Blockers
- (none)

## Decisions pending (see docs/ROADMAP.md §4.3)
- OD-01 Event bus: default NATS JetStream unless decided otherwise

## Notes for next session
- **RLS pattern bug, fixed (0.6):** the bare `current_setting('app.tenant_id')::uuid` pattern data model §1 documented errors instead of failing closed on a pooled connection reused after any `WithTenantTx` call. Fixed to `nullif(current_setting('app.tenant_id', true), '')::uuid` + `WITH CHECK` + `FORCE ROW LEVEL SECURITY`, now in data model §1 and `.claude/rules/migrations.md` — use that pattern, not the one from memory.
- `gen-check` is scoped to `git diff -- ':(glob)**/*_gen.*'`. Every future generator's output must be named `*_gen.<ext>` for this to keep working.
- `platform/money`'s VAT rate type is `VATRateBasisPoints` (int64, 1600 = 16%) and its allocation weights are plain `int64` — deliberately not `shopspring/decimal` (that's scoped to fractional *token* quantities per ROADMAP §6, a different concern arriving with the ledger in Phase 3). Don't introduce a float anywhere near this package.
- `platform/ids`' prefixed-ID encoding (`"<prefix>_<uuid-string>"`) is an implementation choice, not a locked decision — data model §1 only fixes the concept. Revisit freely if a shorter/encoded form turns out to matter.
- **MinIO is not in the stack** — `deploy/compose/docker-compose.yml`'s `object-storage` service runs SeaweedFS's S3 gateway instead (same ports, 9000/9001). Postgres there is on host port **5433**, not 5432.
- `platform/log`'s `trace_id`/`request_id` only appear on `*Context` logging calls with a context that carries them. `platform/otel.Setup`'s exporters don't dial eagerly — a nil error isn't proof the collector is reachable.
