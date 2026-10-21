# 0004. Code generation: sqlc, oapi-codegen, buf

**Date:** 2026-10-21 · **Status:** accepted

## Context

CLAUDE.md's workflow is contracts first: *"Contracts first (OpenAPI in
docs/api/, migrations, sqlc queries, proto, events in docs/events/,
error codes in docs/errors/catalogue.yaml), then failing tests, then
code."* Three of those contract types need a generator to turn a
hand-written source-of-truth file into Go code a service actually
compiles against: SQL queries, OpenAPI specs, and proto service
definitions. The tech stack table (ROADMAP §6) already names the three
tools — `sqlc`, `oapi-codegen`, `buf` — and `Makefile`'s `gen` target
already wires all three in behind existence checks (*"no sqlc.yaml yet,
skipping sqlc"*, and so on) so the target is a safe no-op until each
tool's first real input exists. This ADR records why these three,
specifically, and the convention that makes `make gen-check` able to
tell "generated code is stale" apart from "something else changed."

The alternative to each is hand-writing the generated layer: raw
`database/sql`/`pgx` calls instead of `sqlc`, a hand-rolled HTTP router
and request-struct decoding instead of `oapi-codegen`'s strict server,
hand-written gRPC stubs instead of `buf generate`. All three generators
exist for the same reason: the hand-written version drifts from its
source of truth (a SQL typo that only surfaces at runtime; a handler
that silently stops matching its OpenAPI spec) in a way a generator
structurally can't, because the generator's output *is* a function of
the source file — `make gen-check`'s whole job (diffing `**/*_gen.*`
after regenerating) only works because that's true.

## Decision

**`sqlc`** generates typed Go from hand-written SQL in each service's
`store/queries/*.sql` (§5.3) against that service's own migrations —
queries are real SQL, reviewed as SQL, not an ORM's query-builder DSL.
**`oapi-codegen`**, in **strict server** mode, generates each service's
`api/` request/response types and a server interface from
`docs/api/<service>.yaml` (OpenAPI 3.1, the source of truth per D-01 and
`.claude/rules/api.md`) — handlers implement the generated interface,
so a handler that doesn't match its own spec is a compile error, not a
runtime surprise caught by an e2e test. **`buf`** generates gRPC stubs
from `proto/` for service-to-service calls (D-01), with `buf`'s own
linting and breaking-change detection as a second gate beyond plain
`protoc`.

**Convention every generator must follow:** output is named
`*_gen.<ext>` — `platform/problem/codes_gen.go` (task 0.5's
`tools/errcodes`, the first generator this repo actually built) is the
existing example. `make gen-check`'s diff is deliberately scoped to
`git diff -- ':(glob)**/*_gen.*'` rather than the whole tree, specifically
because an unscoped version broke on unrelated changes mid-task more
than once before landing on this (`Makefile`'s own comment on
`gen-check`). Every future generator — `sqlc`, `oapi-codegen`, `buf`,
and any later one — has to keep using this suffix for that check to mean
anything.

## Consequences

- Generated files are never hand-edited — `.claude/rules/api.md`:
  *"regenerate code with make gen, never edit generated files."* A fix
  belongs in the source file (`.sql`, `.yaml`, `.proto`) and a
  `make gen` run, not a patch to the `_gen.` output.
- `make gen-check` in CI (§8.7) catches a committed source file whose
  generated output wasn't regenerated to match — a real, mechanical
  check, not a reviewer having to notice by eye.
- Nothing in `make gen` fails before its input exists — each branch
  checks for its config/input file first and prints why it's skipping,
  so `make check` stays green through Phase 0 even though sqlc, buf and
  OpenAPI have no real inputs yet. The first real service to add
  `sqlc.yaml`, a `proto/*.proto`, or a `docs/api/*.yaml` file is what
  turns each branch from a no-op into a real generator run — expect that
  to surface the first real friction with any of these three tools.
- `tools/scaffold`'s generated service template (task 0.13) deliberately
  skips sqlc entirely (no query needs it yet — the demo writes through
  `platform/outbox.Write` and one inline `INSERT`) and has no OpenAPI
  spec beyond the ops-only `/healthz`/`/readyz` endpoints, which are
  intentionally outside the versioned API contract. The first real
  service (identity, task 1.1) is what exercises `sqlc`, `oapi-codegen`
  and `buf` for real, not Phase 0's scaffolding.
