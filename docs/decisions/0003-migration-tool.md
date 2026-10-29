# 0003. Migration tool: goose

**Date:** 2026-10-21 · **Status:** accepted

## Context

Every service owns one PostgreSQL schema (D-02) and needs a way to apply
and roll back its own `migrations/<service>/` directory in order,
exactly once, both in production and inside `testcontainers`-backed
tests (`.claude/rules/migrations.md`, `.claude/rules/tests.md`). The tech
stack table (ROADMAP §6) already names `pressly/goose/v3` directly; this
ADR exists to record why, since 0.6 and 0.12 both already built on it
without a formal decision record.

The realistic alternatives were `golang-migrate` (the other widely-used
Go migration CLI+library) and a schema-diffing tool like Atlas. Against
`golang-migrate`: goose's SQL migration format (`-- +goose Up` /
`-- +goose Down` comment markers in a plain `.sql` file) is simpler to
hand-write and review than `golang-migrate`'s paired
`NNN_name.up.sql`/`NNN_name.down.sql` file convention, and goose ships a
`Provider` API (`goose.NewProvider(dialect, *sql.DB, fs.FS)`) that avoids
package-level mutable state — important once `platform/testkit` needed
to run migrations from Go code inside tests that might run concurrently,
not just a CLI invoked once at deploy time. Against Atlas: schema-diffing
tools infer migrations from a declared target schema, which fights
`.claude/rules/migrations.md`'s rule that a migration is written by hand
to match `docs/design/data-model.md` exactly, including the RLS policy
form that data model §1 and this project's own 0.6 bug-fix made
non-negotiable (the `nullif(current_setting(...), '')::uuid` pattern,
not the bare cast) — a generated migration has no way to know that
constraint matters.

## Decision

**`pressly/goose/v3`**, used two ways:

- **The CLI**, for real operations: `make migrate` runs
  `goose -dir migrations/<schema> postgres "$DATABASE_URL_<schema>" up`
  once per schema directory (`Makefile`).
- **The library's `Provider` API**, for tests:
  `platform/testkit.StartPostgres(t, testkit.WithMigrations(dir))`
  (task 0.12) opens a `database/sql` connection via `pgx/v5/stdlib`,
  builds a `goose.NewProvider(goose.DialectPostgres, db, os.DirFS(dir))`,
  and calls `.Up(ctx)` — the per-instance Provider API specifically, not
  goose's older global `goose.SetDialect`/`goose.Up` functions, so two
  tests starting Postgres concurrently can't race on shared package
  state.

Every migration file lives under `migrations/<schema>/`, one directory
per PostgreSQL schema (`.claude/rules/migrations.md`), named with goose's
own numeric-prefix convention (`tools/scaffold`'s generated
`00001_init.sql` is the first example, task 0.13). Every migration has a
`-- +goose Down` section unless it's irreversible, in which case a
comment says so, per the same rule file.

## Consequences

- Both production deploys and tests apply migrations through the exact
  same tool and the exact same `.sql` files — nothing is "translated"
  between a test-only migration format and a production one.
- Writing a migration means writing SQL by hand against
  `docs/design/data-model.md`, not generating one from a diff — slower
  per migration, but the thing that makes the RLS pattern and the money/
  timestamp/naming conventions in `.claude/rules/migrations.md` actually
  enforceable: a human (or Claude, under that rule file) has to apply
  them every time, deliberately.
- `platform/testkit`'s `WithMigrations` option only works because goose
  ships a Go library with a non-global-state API; swapping migration
  tools later would mean rewriting that one function, not every test
  that calls it (they only see `testkit.WithMigrations(dir)`).
- goose is pinned in two places that must move together: the CLI binary
  (installed separately, confirmed working at `v3.29.0` per 0.6's notes)
  and the `github.com/pressly/goose/v3` library dependency in `go.mod`
  (added at the same `v3.29.0` in task 0.12, specifically to match).
  Bumping one without the other risks the CLI and the test-time
  `Provider` API disagreeing about what a migration file means.
