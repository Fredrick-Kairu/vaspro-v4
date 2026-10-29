# 0002. Repository and deployment shape

**Date:** 2026-10-21 · **Status:** accepted

## Context

Fourteen services, a gateway, and a shared platform layer need somewhere
to live and some way to run — as fourteen-plus separate repositories and
deployable units, or as one. ROADMAP §2.1 states the choice already:
*"The whole v4 backend is one Go module in one repository (a monorepo).
Each service is a module under `internal/` with its own entry point
under `cmd/`."* §5.1 adds the deployment half: *"`cmd/<service>/main.go`
runs one service. This is the production deployment shape.
`cmd/vaspro-all/main.go` runs every service in one process. It is for
local development and CI only."* This ADR exists to record *why*, since
0.1–0.13 already built the whole platform on this shape without a formal
decision record for it.

The alternative — one repository and one deployable binary per service —
buys independent versioning and blast-radius isolation at build time, at
the cost of this project's actual bottleneck: fourteen services sharing
one data model, one event contract, and one set of platform mechanisms
(`internal/platform/*`) that every one of them must stay in lock-step
with. A cross-repo change to, say, `platform/money`'s rounding behavior
would need fourteen coordinated PRs, fourteen CI runs, and fourteen
version bumps before every service agreed on the same behavior again. A
monorepo makes that one PR, one CI run, and one commit — at the cost of
needing the boundary CLAUDE.md and `.claude/rules/go.md` already name
(*"internal/\<a\> must not import internal/\<b\>"*) to be enforced by
tooling, not goodwill.

## Decision

**One repository, one Go module, services as packages under
`internal/`.** Concretely, what's already built:

- `internal/platform/*` holds mechanisms only, never business rules
  (§5.2 rule 4) — `.golangci.yml`'s `depguard` rule enforces that
  `internal/platform` can't import any service package, and a mirrored
  rule stops one service importing another; both are lint failures, not
  review comments.
- Every service follows the same internal layout
  (`<service>.go`/`api/`/`grpc/`/`app/`/`domain/`/`store/`/`events/`/
  `adapters/`/`testdata/`, CLAUDE.md's "Service layout", §5.3) —
  `tools/scaffold` (task 0.13) generates exactly this shape via
  `make new-service NAME=<svc>`, so the convention is mechanical, not
  memorized.
- **Two deployment shapes from the same code, no separate build:**
  `cmd/<service>/main.go` runs one service standalone (production);
  `cmd/vaspro-all/main.go` runs every registered service concurrently in
  one process, via an `errgroup.Group`, shutting down together on one
  signal (local dev, CI, task 0.13). Services still only talk to each
  other through gRPC and the event bus even inside `vaspro-all` — the
  process boundary is a deployment choice, never a design shortcut
  (§5.1).
- A service owns exactly one PostgreSQL schema (ADR 0001's sibling
  decisions, D-02/D-03) and connects as a role with no grants on any
  other schema — the database enforces the same boundary the Go compiler
  and `depguard` enforce at the import level.

## Consequences

- A single `go.mod`, a single `go.sum`, a single `make check` across
  every service — the lesson already learned twice (0.9, 0.12): a new
  dependency's transitive version bumps are visible in one diff, not
  scattered across fourteen repos' independent `go.sum`s.
- Refactoring a platform package (money, clock, crypto, db) is one
  commit touching every caller, reviewed once, not fourteen version
  bumps chased down independently.
- The boundary this shape depends on (`internal/<a>` never imports
  `internal/<b>`) is exactly as strong as `depguard`'s configuration —
  weakening or missing a rule there is a real regression, not a style
  nit, since nothing else stops the import at the Go level.
- Independent per-service release cadence and blast-radius isolation,
  the main thing a multi-repo shape would have bought, don't exist here.
  A bad change in one service's code can break `make check` for the
  whole repo (by design — it's supposed to be caught before merge, not
  after a service-specific deploy).
- `cmd/vaspro-all` growing to all fourteen services is purely additive —
  each `make new-service` run inserts one import and one
  `Bootstrap`-and-register block between sentinel comments
  (`tools/scaffold`); nothing about this shape needs revisiting as
  services are added, only as the boundary rules themselves might.
