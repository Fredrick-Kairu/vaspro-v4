# VASPRO v4 Build Roadmap: A to Z

> **Version** 1.0 · **Date** 12 Oct 2026 · **Owner** Fred · **Lives at** `docs/ROADMAP.md` in the `vaspro-v4` repository
>
> This roadmap is the master plan for building VASPRO v4 in Go. It covers every stage from an empty folder to Kenya go-live, the v3 switch-off, and then Tanzania and Uganda. It says what to build in each phase, how to test it, what must be true before moving on, and how to drive the work with Claude Code in VS Code.

---

## Contents

0. [How to use this roadmap](#0-how-to-use-this-roadmap)
1. [Before you start: urgent v3 actions](#1-before-you-start-urgent-v3-actions)
2. [Where everything lives](#2-where-everything-lives)
3. [Set up the project in VS Code](#3-set-up-the-project-in-vs-code)
4. [Ground truth: design sources and decisions](#4-ground-truth-design-sources-and-decisions)
5. [Architecture for the build](#5-architecture-for-the-build)
6. [Tech stack](#6-tech-stack)
7. [How we work with Claude Code](#7-how-we-work-with-claude-code)
8. [Testing strategy](#8-testing-strategy)
9. [Phase map and milestones](#9-phase-map-and-milestones)
10. [Stage A: finish the design](#10-stage-a-finish-the-design)
11. [Build phases 0 to 22](#11-build-phases-0-to-22)
12. [Parallel tracks](#12-parallel-tracks)
13. [v3 migration and cutover](#13-v3-migration-and-cutover)
14. [Risk register](#14-risk-register)
15. [Appendix: bootstrap files](#15-appendix-bootstrap-files)

---

## 0. How to use this roadmap

### For Fred

- Work top to bottom. A phase starts only when the phase before it has passed its **exit gate**, unless the phase map (section 9) says two phases can run in parallel.
- Each phase has a ready-made **kickoff prompt**. Paste it into Claude Code to start the phase.
- `docs/STATUS.md` is the one-page record of where the build is. Claude Code updates it at the end of every task, and you read it at the start of every day.
- A decision that changes the design is recorded as an ADR (Architecture Decision Record) in `docs/decisions/` before any code depends on it. Then the affected design doc is updated.

### For Claude Code

These rules apply to every session. `CLAUDE.md` repeats the short version.

1. Read `docs/STATUS.md` first. Work only on the current phase and task named there.
2. Before starting a phase, read that phase's section in this file and the design sections it lists under **Read first**.
3. **Contracts first.** Write the OpenAPI operation, migration, event schema and error codes before the handler or business logic.
4. **Tests first.** Write a failing test for each behaviour, then make it pass. Do not mark a task done while any test in `make check` is failing.
5. Never edit a migration that has been merged to `main`. Add a new migration instead.
6. Never put secrets, tokens, real phone numbers or real customer data in code, tests, fixtures or logs. Use the test numbers in section 8.4.
7. Never connect to v3 production, the v3 database or the production SMS engine. Work against fakes, sandboxes and staging only.
8. If the design is silent, contradicts itself, or a task needs a decision listed as open in section 4.3, **stop and ask**. Write the question in `docs/decisions/open-questions.md`, and do not guess on money, security, compliance or the SMS engine contract.
9. Keep each session to one task. At the end, run `make check`, update `docs/STATUS.md`, and propose a commit message.
10. Prefer small, reviewable changes: one migration, one endpoint, or one adapter per commit where possible.

### Markers used in this file

| Marker | Meaning |
|---|---|
| `[ ]` | To do |
| `[~]` | In progress |
| `[x]` | Done |
| `[!]` | Blocked: the reason goes in `docs/STATUS.md` |
| `[>]` | Deferred: moved to the later task named on the line |
| **Exit gate** | Conditions that must all hold before the next phase starts |
| **Kickoff prompt** | Text to paste into Claude Code to start the phase |

Tick boxes in this file as phases complete, and keep the detailed task-level progress in `docs/STATUS.md`.

---

## 1. Before you start: urgent v3 actions

These come out of the v3 code review. They protect live clients today and do not depend on v4.

- [ ] **Clients' M-Pesa credentials in source code.** `app/utils/MpesaTools.php` holds several clients' Daraja consumer keys and secrets, passkeys and initiator passwords. Ask each affected client to revoke and reissue them with Safaricom. Move the new ones into a secrets store.
- [ ] **VASPRO's own secrets in source code.** `app/config/enviroment.php` holds database passwords, M-Pesa keys, RabbitMQ credentials, gateway passwords and partner tokens. Rotate them all and load them from the environment or a secrets manager.
- [ ] **TLS checks off.** v3's Daraja calls set `CURLOPT_SSL_VERIFYPEER` to false. Turn verification back on.
- [ ] **Payout VAT bug.** v3 records VAT on payout fees as `rate × 0.016`, which is 1.6% rather than 16%. Confirm with finance and correct it.
- [ ] **Restrict access** to the v3 repository while secrets remain in its history. Rewriting git history is optional once everything has been rotated.

---

## 2. Where everything lives

### 2.1 Repository layout

The whole v4 backend is one Go module in one repository (a monorepo). Each service is a module under `internal/` with its own entry point under `cmd/`.

```text
vaspro-v4/
├── CLAUDE.md                     # Claude Code reads this at the start of every session (keep under 200 lines)
├── CLAUDE.local.md               # your personal notes for Claude; gitignored
├── .claude/
│   ├── settings.json             # shared permissions for Claude Code
│   ├── rules/                    # instructions that load only for matching files
│   │   ├── go.md
│   │   ├── migrations.md
│   │   ├── tests.md
│   │   ├── api.md
│   │   └── frontend.md
│   └── skills/                   # project slash commands
│       ├── start-task/SKILL.md   # /start-task
│       ├── finish-task/SKILL.md  # /finish-task
│       ├── review/SKILL.md       # /review
│       └── start-phase/SKILL.md  # /start-phase
├── docs/
│   ├── ROADMAP.md                # this file
│   ├── STATUS.md                 # current phase, task, blockers (imported by CLAUDE.md)
│   ├── design/
│   │   ├── requirements.md       # exported from the requirements doc
│   │   ├── system-design.md      # exported from the system design doc
│   │   └── data-model.md         # exported from the data model doc
│   ├── decisions/                # ADRs: 0001-event-bus.md, ...; open-questions.md
│   ├── api/                      # OpenAPI 3.1, one file per service + bundle
│   ├── events/                   # JSON Schema per event type + catalogue.md
│   ├── errors/                   # catalogue.yaml: every error code, cause, fix
│   ├── packs/                    # country pack docs
│   └── runbooks/                 # operations: incidents, cutover, rotation
├── cmd/
│   ├── vaspro-all/               # runs every service in one process (local and CI)
│   ├── gateway/
│   ├── identity/
│   ├── accounts/
│   ├── ledger/
│   └── ...                       # one folder per service
├── internal/
│   ├── platform/                 # shared libraries only; no business logic
│   │   ├── config/  log/  otel/  httpx/  problem/  db/  ids/  money/  clock/
│   │   ├── idempotency/  outbox/  inbox/  events/  bus/  cache/  packs/
│   │   ├── crypto/  authz/  testkit/
│   ├── identity/                 # one folder per service, same internal layout (section 5.3)
│   ├── accounts/
│   ├── ledger/
│   ├── payments/
│   ├── billing/
│   ├── messaging/
│   ├── ussd/
│   ├── contacts/
│   ├── applications/
│   ├── verify/
│   ├── journeys/
│   ├── surveys/
│   ├── webhooks/
│   └── monitoring/
├── migrations/                   # one folder per PostgreSQL schema
│   ├── identity/  accounts/  ledger/  ...
├── packs/                        # country packs as versioned data
│   ├── schema.json
│   ├── ke/  tz/  ug/
├── proto/                        # gRPC contracts between services
├── tools/
│   ├── fakes/                    # fake SMS engine, Daraja, KYC, SDP, eTIMS, WhatsApp...
│   ├── migrate-v3/               # v3 importers
│   └── scaffold/                 # make new-service
├── test/
│   ├── e2e/                      # black-box API scenarios
│   ├── contract/                 # golden payloads (SMS engine, rails, webhooks)
│   └── load/                     # k6 scripts
├── web/
│   ├── dashboard/                # client dashboard (Vue 3)
│   ├── console/                  # staff console (Vue 3)
│   └── portal/                   # developer portal
├── deploy/
│   ├── compose/                  # docker-compose for local development
│   └── k8s/                      # deployment manifests (later)
├── Makefile
├── go.mod
├── .golangci.yml
├── .editorconfig
└── .github/workflows/ci.yml
```

### 2.2 Where to save this roadmap, and why

| File | Location | Loaded how | Why |
|---|---|---|---|
| This roadmap | `docs/ROADMAP.md` | Read on demand. `CLAUDE.md` tells Claude to open the current phase's section | It is long. Claude Code loads everything `CLAUDE.md` imports at the start of every session, and imports don't reduce that cost, so importing this file would spend context on phases that aren't in progress |
| Status | `docs/STATUS.md` | Imported into `CLAUDE.md` with `@docs/STATUS.md` | Short, and Claude needs it every session |
| Project instructions | `CLAUDE.md` at the repository root | Loaded automatically at the start of every session | Claude Code reads `./CLAUDE.md` (or `./.claude/CLAUDE.md`) by itself. Anthropic's guidance is to keep it under 200 lines |
| Topic rules | `.claude/rules/*.md` with a `paths:` header | Load only when Claude reads or edits a matching file | Go rules load for `.go` files, migration rules for `migrations/`, and so on |
| Personal notes | `CLAUDE.local.md` | Loaded automatically; gitignored | For your own preferences, not shared with the team |
| Design docs | `docs/design/*.md` | Read on demand by section | The exported requirements, system design and data model |

To check what Claude has loaded, type `/context` in the Claude Code panel and look under **Memory files**.

---

## 3. Set up the project in VS Code

Follow these steps once. Each ends with a check.

### 3.1 Install the tools

On Windows, do all of this inside **WSL2 (Ubuntu)**, and open the project from WSL in VS Code with the WSL extension. Docker, Go and `make` behave the same as on Linux and macOS there.

| Tool | Why | Check |
|---|---|---|
| Git | Version control | `git --version` |
| Go (current stable release) | Backend | `go version` |
| Docker Desktop, or Docker Engine with the Compose plugin | Local PostgreSQL, Redis, NATS, RabbitMQ, ClickHouse, MinIO, Mailpit | `docker compose version` |
| GNU Make | One command for every task | `make --version` |
| Node.js LTS and pnpm | Frontend apps, OpenAPI tooling | `node -v` and `pnpm -v` |
| `golangci-lint` | Linting | `golangci-lint version` |
| `sqlc` | Type-safe SQL to Go | `sqlc version` |
| `goose` | Migrations | `goose -version` |
| `oapi-codegen` | OpenAPI to Go server and client | `oapi-codegen -version` |
| `buf` | gRPC contracts | `buf --version` |
| `govulncheck` | Vulnerability scan | `govulncheck -version` |
| k6 | Load tests | `k6 version` |
| VS Code 1.94 or later | Editor; the Claude Code extension needs at least this version | Help → About |

Install the Go tools with `go install` at pinned versions, then record those versions in `tools/tools.go` or in the `Makefile`, so every machine and CI uses the same ones.

### 3.2 Install the VS Code extensions

| Extension | Marketplace ID | Why |
|---|---|---|
| Claude Code | `anthropic.claude-code` | Claude Code inside VS Code |
| Go | `golang.go` | Go language support, tests, debugging |
| Docker (Container Tools) | search "Docker" | Manage the local stack |
| YAML | `redhat.vscode-yaml` | Country packs, OpenAPI, CI |
| Vue (Official) | `Vue.volar` | Frontend |
| ESLint and Prettier | `dbaeumer.vscode-eslint`, `esbenp.prettier-vscode` | Frontend lint and format |
| EditorConfig | `EditorConfig.EditorConfig` | Same formatting everywhere |
| SQL client (e.g. SQLTools with its PostgreSQL driver) | search "SQLTools" | Inspect local databases |
| WSL (Windows only) | `ms-vscode-remote.remote-wsl` | Work inside WSL2 |

To install Claude Code: press `Ctrl+Shift+X` (`Cmd+Shift+X` on Mac), search for **Claude Code**, and click **Install**. If it doesn't appear, run **Developer: Reload Window** from the Command Palette.

### 3.3 Create the repository

```bash
mkdir vaspro-v4 && cd vaspro-v4
git init -b main
go mod init github.com/<your-org>/vaspro-v4     # use your real GitHub org or user
mkdir -p docs/{design,decisions,api,events,errors,packs,runbooks} .claude/{rules,skills} \
         cmd internal/platform migrations packs proto tools/fakes test/{e2e,contract,load} \
         web deploy/compose
```

Then create a **private** GitHub repository named `vaspro-v4`, push `main`, and protect it: require a pull request, a passing CI check, and no force pushes.

**Check:** `git status` is clean and `go env GOMOD` prints the path to `go.mod`.

### 3.4 Add the roadmap, design docs and bootstrap files

1. Save this file as `docs/ROADMAP.md`.
2. Save the exported design docs as `docs/design/system-design.md` and `docs/design/data-model.md`. For `docs/design/requirements.md`, download the requirements doc as Markdown and save it.
3. Create `CLAUDE.md`, `docs/STATUS.md`, `.claude/settings.json`, the rules and the skills from the appendix (section 15). If you received the starter kit, these files are already in it. Otherwise, open Claude Code and say:

   > Create every file listed in section 15 of docs/ROADMAP.md exactly as written there, at the paths given. Do not change their content.

4. Add `.gitignore` entries for `CLAUDE.local.md`, `.env`, `.env.*`, `secrets/`, `bin/`, `coverage.out`, `node_modules/` and `dist/`.
5. Commit: `chore: bootstrap roadmap, design docs and Claude Code setup`.

**Check:** the tree matches section 2.1 for `docs/` and `.claude/`.

### 3.5 Open Claude Code and confirm it sees the project

1. Open the `vaspro-v4` folder in VS Code (`code .` from the terminal, or **File → Open Folder**).
2. Click the **Spark** icon in the editor toolbar, in the Activity Bar, or **✻ Claude Code** in the status bar.
3. Sign in the first time. Any paid Claude plan (Pro, Max, Team or Enterprise) or a Claude Console account works.
4. Type `/context` and confirm that `CLAUDE.md` and `docs/STATUS.md` are listed under **Memory files**.
5. Type `/start-task` and confirm the skill loads. If it doesn't, check the folder name `.claude/skills/start-task/SKILL.md`.

Useful in the panel:
- Type `@` and part of a filename to point Claude at a file, for example `@docs/design/data-model.md`.
- Select lines and press `Alt+K` (`Option+K` on Mac) to reference that selection.
- `/plan` switches to plan mode: Claude proposes a plan, and nothing changes until you accept it. Use it at the start of every task.

### 3.6 Start Phase 0

Paste the Phase 0 kickoff prompt from section 11. Claude Code will plan the work, then build the repository skeleton, local stack and platform libraries task by task.

### 3.7 Daily rhythm

| When | What |
|---|---|
| Start of day | Open `docs/STATUS.md`. Run `make up` and `make check`. Start a new Claude Code conversation |
| Each task | `/start-task` → review the plan → let Claude work → `/review` in a new conversation → `/finish-task` → commit |
| End of day | Make sure `docs/STATUS.md` says what's next. Push the branch |
| End of phase | Go through the phase's exit gate line by line. Tick the phase in this file. Tag the repository `phase-N-done` |

---

## 4. Ground truth: design sources and decisions

### 4.1 Sources, in order of authority

| # | Source | Repo copy | Live doc |
|---|---|---|---|
| 1 | ADRs and this roadmap's decisions tables | `docs/decisions/`, section 4 | — |
| 2 | Data model | `docs/design/data-model.md` | https://claude.ai/code/artifact/2210198c-313f-4a2d-9692-931aa5dac3c6 |
| 3 | System design: core flows | `docs/design/system-design.md` | https://claude.ai/code/artifact/e41284c4-aa32-45a8-ba2e-484b736454a4 |
| 4 | Requirements | `docs/design/requirements.md` | https://claude.ai/code/artifact/91f2fbc8-54d8-4131-a47a-cf10c8f83f85 |
| 5 | v3 code, for behaviour and contracts only | separate repo, read-only | — |

When two sources disagree, the higher one wins, and the disagreement goes into `docs/decisions/open-questions.md` so the lower one gets fixed. Once the design docs are committed to the repo, change the repo copy first and mirror the change back to the live doc.

### 4.2 Locked decisions

Code may rely on these. Changing one needs an ADR.

| ID | Decision | Source |
|---|---|---|
| D-01 | Backend in Go; REST + JSON outside, gRPC between services; OpenAPI 3.1 is the source of truth for public APIs | System design §1 |
| D-02 | PostgreSQL 16, one schema per service; a service never reads another service's schema | Data model §1 |
| D-03 | Row-level security on `tenant_id`, set per transaction with `app.tenant_id` | Data model §1 |
| D-04 | UUIDv7 keys; prefixed public IDs such as `msg_…` and `inv_…` | Data model §1 |
| D-05 | Money as `bigint` minor units plus an ISO currency code; never floats | Data model §1 |
| D-06 | Transactional outbox for every event; an inbox (`processed_events`) wherever events are consumed; idempotency keys on every write API | Data model §1 |
| D-07 | Email is the sign-in identity. People use passkeys or TOTP; machines use OAuth2 client credentials with 15–60 minute JWTs; high-value calls also carry an HMAC signature | System design §2–3 |
| D-08 | Errors are RFC 9457 problem+json with code families AUTH, KYC, VAL, BAL, CMP, RTE, DLR, RATE, SYS, LIM, PAY | System design §8 |
| D-09 | Country rules live in versioned country packs (KE first, then TZ and UG) | System design §1 |
| D-10 | Double-entry ledger; main wallet plus one token wallet per service; holds are not journal entries; usage settles in batches | Data model §5 |
| D-11 | A token is one unit of a service, bought at the client's price: the list price with volume tiers, or a negotiated deal (`ledger.token_prices`, maker-checker). The price is locked in the lot at purchase, so a new deal applies from the next purchase. Airtime tokens stay at one currency unit. Supersedes "tokens are worth the same as money" | Decision 21 Oct; data model §5 |
| D-12 | VAT is charged on use, not on top-up or token purchase | Decision 12 Oct |
| D-13 | VASPRO never holds client money. Collections settle into the client's own paybill or till. Payouts leave from the client's own shortcode with the client's own credentials. VASPRO earns a fee per transaction | Decision 12 Oct; data model §7 |
| D-14 | SMS sending stays in the existing Go SMS engine, unchanged. v4 chooses the route from the country pack and hands off exactly as v3 does | Decision 12 Oct; system design §5.2 |
| D-15 | v4 builds its own adapters for WhatsApp, Telegram, Messenger, email, RCS and premium MT through the operator SDP | Decision 12 Oct |
| D-16 | Sandbox and live share the same API; the credential decides the environment | System design §9 |
| D-17 | Reversals, credit notes, payout batches and allowlisting use maker-checker | System design §4.3, §13 |
| D-18 | Scope is everything in the requirements except Voice and IVR | Requirements |
| D-19 | Kenya reaches v3 parity and v3 is switched off before new channels and countries | Requirements, phasing |
| D-20 | Allocating money to a service is final for the client. No transaction moves tokens back to the main wallet; only an approved reversal claws back unused tokens | Decision 21 Oct |
| D-21 | Code, shortcode and sender ID rentals are billed by invoice from the billable assets register (auto-pay or Pay now), not bought with tokens. Paying VASPRO (ledger, billing) and client collections and payouts (payments, no ledger except VASPRO's fee) are separate flows | Decision 21 Oct; data model §6–7 |

### 4.3 Open decisions and when they block

If a decision isn't made by its **needed by** phase, the build uses the default and records it in an ADR, so the work isn't blocked.

| ID | Open decision | Needed by | Default if not decided |
|---|---|---|---|
| OD-01 | Event bus: NATS JetStream or Kafka | Phase 0 | NATS JetStream behind the `platform/bus` interface |
| OD-02 | Hosting, cloud and data residency | Phase 8 (staging); Phase 17 (production) | Containers, cloud-agnostic; Kubernetes manifests |
| OD-03 | KYC vendor | Phase 2 | Fake adapter until chosen; interface fixed |
| OD-04 | Tier 1 caps (values) | Phase 7 | Configurable; conservative placeholders |
| OD-05 | Pricing: list token prices and volume tiers, rate cards and plan prices | Phase 3 (structure); M2 (values) | Values loaded as data |
| OD-06 | Rate cards with or without VAT, per country | Phase 3 | VAT included (as in the worked example) |
| OD-07 | Settlement window length | Phase 3 | 5 minutes, configurable |
| OD-08 | eTIMS: one usage invoice per month or per batch | Phase 9 | Monthly |
| OD-09 | Billing day and proration | Phase 9 | Billing day per client, default the 1st; daily proration |
| OD-10 | SMS engine questions: correlator format and length; single sends through direct gateway calls or engine queues; how Safaricom bulk DLRs reach v4; contact-group campaigns without `VASPRO_BULK_COMPOSE` | Phase 7 (ask in Stage A) | **No default. Phase 7 cannot start until answered** |
| OD-11 | How shortcode MO reaches v4 today (queue, engine or gateway callback) | Phase 12 | No default; confirm from v3 and the engine owners |
| OD-12 | Confirm payouts leave from the client's own shortcode; who sends v3 payouts today | Phase 13 | Client's own shortcode (as modelled) |
| OD-13 | Bank payouts: which banks' APIs, or an aggregator account in the client's name | Phase 13 | Defer bank payouts to after parity |
| OD-14 | Shared USSD parent codes per network | Phase 11 | Configurable; sandbox code only |
| OD-15 | v3 migration approach | Plan by Phase 10; execute in Phase 18 | Client waves with dual running (section 13) |
| OD-16 | Accept old v3 API tokens through a compatibility route, and for how long | Phase 1 (design); Phase 18 (enable) | Yes, for 90 days after a client's migration |
| OD-17 | On-call tool and independent paging provider | Phase 16 | Any provider outside VASPRO's SMS routes |
| OD-18 | Holds in PostgreSQL or Redis at peak | After the Phase 7 load test | PostgreSQL |
| OD-19 | Card and aggregator provider | Phase 4 (optional), Phase 13 | M-Pesa only until chosen |
| OD-20 | Frontend stack | Track F0 | Vue 3 + TypeScript + Vite |
| OD-21 | VAT on payout fees (v3 records 1.6%) | Phase 13 | 16%, as the pack's VAT rate |
| OD-22 | v3 collection and payout history depth to import | Phase 18 | 24 months |

---

## 5. Architecture for the build

### 5.1 One repository, many services, one binary for local work

- Each of the 14 services is a Go package under `internal/<service>` with a `Run(ctx, deps)` entry point.
- `cmd/<service>/main.go` runs one service. This is the production deployment shape.
- `cmd/vaspro-all/main.go` runs every service in one process. It is for local development and CI only, and makes the whole system start with one command.
- Services still talk only through gRPC and the event bus, even inside `vaspro-all`. The process boundary is a deployment choice, not a design shortcut.

### 5.2 Boundary rules (enforced by lint and tests)

1. `internal/<a>` never imports `internal/<b>`. Only `internal/platform/...` is shared. A `depguard` rule in `.golangci.yml` enforces this.
2. A service connects only to its own PostgreSQL schema, as a database role that has no grants on other schemas. A test checks the grants.
3. Cross-service reads go through gRPC (`proto/`) or through events the service keeps its own copy of.
4. `internal/platform` holds mechanisms, never business rules. Money arithmetic belongs there; deciding what to charge does not.

### 5.3 Inside one service

```text
internal/<service>/
├── <service>.go        # Run(ctx, deps): wires everything, starts servers and consumers
├── api/                # generated from docs/api/<service>.yaml (oapi-codegen) + thin handlers
├── grpc/               # generated from proto/ + thin handlers
├── app/                # use cases: one file per command or query; orchestrates domain + store
├── domain/             # entities, value objects, state machines; pure Go, no I/O
├── store/              # sqlc queries (store/queries/*.sql) and generated code
├── events/             # event types this service publishes and consumes
├── adapters/           # outbound integrations (Daraja, SMS engine, KYC vendor...) behind interfaces
└── testdata/           # golden files and fixtures
```

Domain code holds the rules and is tested without a database. Use cases are tested against a real PostgreSQL in testcontainers. Adapters are tested against fakes and golden payloads.

### 5.4 Environments

| Environment | What it is | Data |
|---|---|---|
| Local | `make up` + `vaspro-all` on your machine | Fakes, seed data |
| CI | GitHub Actions; testcontainers | Throwaway |
| Product sandbox | Part of the product (D-16): clients test with sandbox credentials against the simulators in `tools/fakes` | Client test data |
| Staging | A full deployment, connected to rail and operator sandboxes and the SMS engine's test setup | Synthetic, plus anonymised v3 copies for migration rehearsal |
| Production | Live | Live |

The **product sandbox** and **staging** are different things. Sandbox is a feature clients use. Staging is where VASPRO tests releases.

---

## 6. Tech stack

Pin every version in `go.mod`, `package.json` and the `Makefile`. Upgrade on purpose, in its own pull request.

| Concern | Choice |
|---|---|
| Language | Go (current stable); `log/slog` for logs |
| HTTP router | `go-chi/chi/v5` |
| Public API contract | OpenAPI 3.1 in `docs/api/`; `oapi-codegen` strict server; request validation middleware |
| Internal calls | gRPC with `buf` |
| Database | PostgreSQL 16; `jackc/pgx/v5`; `sqlc` for queries; `pressly/goose/v3` migrations per schema |
| Event bus | NATS JetStream (`nats.go`, `jetstream` package), behind `platform/bus` (OD-01) |
| SMS engine hand-off | RabbitMQ via `rabbitmq/amqp091-go`, plus HTTP calls, exactly per system design §5.2 |
| Cache, sessions, rate limits | Redis 7, `redis/go-redis/v9` |
| Analytics events | ClickHouse, `ClickHouse/clickhouse-go/v2` |
| Object storage | S3-compatible; MinIO locally |
| Secrets and encryption | Cloud KMS or Vault in production; a local key-encryption key in development; envelope encryption in `platform/crypto` |
| IDs | `google/uuid` (UUIDv7) |
| Decimals (tokens) | `shopspring/decimal` |
| Phone numbers | `nyaruka/phonenumbers` |
| Auth | `go-webauthn/webauthn`, `pquerna/otp`, `golang.org/x/crypto/argon2` and `bcrypt` (v3 import), `lestrrat-go/jwx/v3` (task 1.8: `v2` was deprecated upstream by the time this table's pin was first exercised, with nothing else in the repo yet depending on its API; `v4` was tried first but unconditionally imports Go's still-experimental `encoding/json/v2`/`jsontext` — gated behind `GOEXPERIMENT=jsonv2`, not enabled on this toolchain — so landed on `v3`, the more conservative maintained major with no such dependency) |
| Config | `caarlos0/env` |
| Observability | OpenTelemetry SDK; Prometheus metrics; locally the `grafana/otel-lgtm` container |
| Testing | `testing`, `stretchr/testify`, `testcontainers-go`, `pgregory.net/rapid` (property tests), Go native fuzzing, k6 |
| Quality | `golangci-lint` (with `gosec`, `depguard`), `govulncheck`, `gitleaks`, `trivy` for images |
| Frontend | Vue 3, TypeScript, Vite, Pinia, Vue Router, Vitest, Playwright; API client generated with `openapi-typescript` |
| API reference | Rendered from OpenAPI (for example Scalar or Redoc) |
| Local email | Mailpit |
| Product analytics and support | PostHog (Phase 20) |

---

## 7. How we work with Claude Code

### 7.1 The loop for every task

1. **Start.** Open a new conversation and run `/start-task`. Claude reads `docs/STATUS.md`, the phase section and the design sections, then proposes a plan in plan mode.
2. **Agree the plan.** Check that it lists the contracts, tests and files it will touch. Correct it before accepting.
3. **Contracts.** Claude writes or updates the OpenAPI operation, migration, sqlc queries, proto, event schema and error codes.
4. **Failing tests.** Claude writes tests that describe the behaviour, runs them, and shows them failing for the right reason.
5. **Implement** until those tests pass.
6. **Verify.** `make check` must be fully green: lint, unit, integration, contract and generation checks.
7. **Review.** Start a new conversation and run `/review`. A fresh session reviews the diff against the design and the checklist in section 8.6, without having written the code.
8. **Fix** what the review found and run `make check` again.
9. **Finish.** `/finish-task` updates `docs/STATUS.md`, ticks the task, and proposes a commit message.
10. **Commit and open a pull request.** CI must pass before merging.

### 7.2 Prompt templates

**Start a task** (when not using `/start-task`):

> Read docs/STATUS.md and the "Phase N" section of docs/ROADMAP.md. The current task is "<task>". Read the design sections listed under Read first. Then, in plan mode, list: the contracts you will add or change, the tests you will write first, the files you will touch, and any open question that blocks you. Don't write code until I accept the plan.

**Write the failing tests first:**

> Write the tests for <behaviour> first, covering <cases>. Use testkit for PostgreSQL. Run them and show me they fail for the expected reason. Don't implement yet.

**Implement:**

> Implement the minimum to make these tests pass, following the layout in section 5.3 of the roadmap. Run make check and fix everything it reports.

**Review** (in a new conversation):

> /review <branch or files>

**When CI fails:**

> CI failed on <job>. Read the log I pasted, find the root cause, fix it, and run the same check locally. Don't disable or skip any test.

**Close a phase:**

> We are closing Phase N. Go through its exit gate in docs/ROADMAP.md line by line. For each line, show the evidence (test name, command output or file) or say it's missing. Don't tick anything that lacks evidence.

### 7.3 Session hygiene

- One task per conversation. Start a new conversation for the next task, so the context stays focused.
- Keep `CLAUDE.md` short. Put rules that apply to only part of the code into `.claude/rules/` with a `paths:` header.
- Point Claude at exact design sections with `@docs/design/data-model.md` plus the section name, rather than asking it to read everything.
- Ask Claude to record what it learned in `docs/STATUS.md` under **Notes for next session**, not in chat.
- If Claude repeats a mistake, add a one-line rule to the relevant `.claude/rules/` file.

### 7.4 Git workflow

- Branch per task: `p<phase>/<short-name>`, for example `p3/ledger-holds`.
- Conventional commits: `feat(ledger): add holds with expiry`, `test(messaging): golden payloads for VASPRO_QUICK`, `fix(...)`, `chore(...)`, `docs(...)`.
- Each pull request lists: what changed, the tests added, the design sections followed, and any ADR.
- Merge only with green CI. Squash-merge to keep `main` readable.
- Tag the end of every phase: `phase-3-done`.

### 7.5 When Claude must stop and ask

- The design doesn't cover the case, or two sources disagree.
- The task touches money movement, tax, credentials, the SMS engine contract or personal data in a way the design doesn't spell out.
- A test can only pass by weakening an assertion, skipping a test, or adding a broad exception.
- A migration would rewrite or drop data.
- An open decision in section 4.3 blocks the task and has no default.

---

## 8. Testing strategy

### 8.1 Test levels

| Level | What it proves | Tool | Location | Runs |
|---|---|---|---|---|
| Unit | Domain rules, state machines, parsers, calculations | `go test` | next to the code | Every save, every PR |
| Property | Invariants hold for any input sequence (ledger balances, money splits) | `rapid` | `*_prop_test.go` | Every PR |
| Fuzz | Parsers never panic and reject bad input (DLR strings, MSISDNs, CSV imports, callbacks) | Go fuzzing | `*_fuzz_test.go` | Short run on every PR; long run nightly |
| Repository | SQL, constraints, triggers and row-level security behave | `testcontainers-go` + real PostgreSQL | `store/*_test.go` | Every PR |
| Integration | One service end to end with its database, bus and fakes | testkit | `internal/<svc>/..._int_test.go` (build tag `integration`) | Every PR |
| Contract | Our payloads match the outside world exactly (SMS engine, Daraja, SDP, eTIMS, webhooks) and our API matches OpenAPI | golden files; OpenAPI validation | `test/contract/` | Every PR |
| End to end | Business scenarios across services through the public API | Go tests against `vaspro-all` | `test/e2e/` (build tag `e2e`) | Every PR to `main`, nightly |
| Frontend | Components and user journeys | Vitest, Playwright | `web/*/` | Every PR touching `web/` |
| Load | Throughput and latency targets | k6 | `test/load/` | Before each milestone; nightly on staging |
| Security | Known vulnerabilities, secrets, insecure code; authz negative tests | `govulncheck`, `gosec`, `gitleaks`, `trivy`; OWASP ZAP on staging | CI and nightly | Every PR, plus before milestones |
| Resilience | Behaviour when PostgreSQL, NATS, Redis, RabbitMQ or a rail fails | Fault injection in e2e (stop containers) | `test/e2e/chaos_*` | Nightly, and before milestones |
| Migration | Every migration applies cleanly from empty and from the previous release; v3 importers reproduce v3 totals | goose; importer dry runs | `migrations/`, `tools/migrate-v3/` | Every PR; dry runs per phase |

### 8.2 Make targets

| Target | Does |
|---|---|
| `make up` / `make down` / `make reset` | Start, stop or wipe the local stack |
| `make gen` | Run sqlc, oapi-codegen, buf and the error-code generator |
| `make gen-check` | Fail if generated code is out of date |
| `make migrate` | Apply all schemas' migrations locally |
| `make lint` | `golangci-lint`, OpenAPI lint, pack validation, `gitleaks` |
| `make test` | Unit, property and short fuzz tests |
| `make test-int` | Repository and integration tests |
| `make test-contract` | Golden payload and OpenAPI conformance tests |
| `make test-e2e` | End-to-end scenarios against `vaspro-all` |
| `make check` | `gen-check` + `lint` + `test` + `test-int` + `test-contract`. **Must be green before a task is done** |
| `make load SCENARIO=…` | Run a k6 scenario |
| `make run` / `make run SVC=ledger` | Run everything, or one service |
| `make new-service NAME=…` | Scaffold a service from the template |

### 8.3 Quality gates

- Coverage: at least 85% of lines in `domain/` and `app/` for ledger, payments, billing and messaging; at least 70% elsewhere. Coverage is a floor, not a goal. The scenario lists below matter more.
- Zero lint errors, zero `govulncheck` findings that are reachable, and zero secrets found by `gitleaks`.
- Every public endpoint has an OpenAPI example that runs green in the e2e suite.
- Every error code that code can return is listed in `docs/errors/catalogue.yaml`. A test fails on unknown codes.
- Every event published has a JSON Schema in `docs/events/`. A test validates published payloads against it.
- No test is skipped on `main` without a linked issue.

### 8.4 Test data and fakes

- **Reserved test numbers.** One block per country, for example `+254 700 000 000` to `+254 700 000 099`, each mapped to a scripted outcome (delivered, absent subscriber, rejected, delayed DLR, no DLR). Fakes and the product sandbox use the same table, from `packs/<cc>/sandbox.yaml`.
- **Fakes in `tools/fakes/`:**
  - The **SMS engine fake** consumes the same RabbitMQ queues and answers with DLR callbacks.
  - The **Daraja fake** covers STK, C2B, B2C, B2B, status query, balance and reversal.
  - Fakes for KYC, SDP, eTIMS, WhatsApp and the airtime providers.

  Each fake is a small Go service, and the product sandbox reuses them as its simulated operators and rails.
- **Clock.** All time comes from `platform/clock`, so tests can move time for send windows, expiries, billing runs and dunning.
- **Factories** in `platform/testkit` create valid tenants, users, wallets and messages with one call.
- **No production data in tests.** v3 copies used for migration rehearsal are anonymised and stay out of the repository.

### 8.5 Golden files from v3

The SMS engine and the payment rails cannot change, so v4 must reproduce v3's outgoing payloads exactly. In Stage A, capture real examples, with personal data replaced:
- one message from each queue in system design §5.2
- one request to each HTTP gateway
- one DLR callback of each kind
- one Daraja callback of each kind

Store them in `test/contract/v3/`. Contract tests compare v4's output to these field by field: names, types, encodings and queue, exchange and routing keys.

### 8.6 Definition of done for every module

- [ ] Migrations, sqlc queries, OpenAPI operations, proto, events and error codes are written and reviewed.
- [ ] Row-level security is on for every client-owned table, and a test shows tenant A can't read or change tenant B's rows.
- [ ] Unit, property, repository and integration tests cover every rule in the design section, including failure paths.
- [ ] Idempotency is tested: the same request twice gives one effect.
- [ ] Every event the module publishes is written through the outbox, validates against its schema, and is consumed idempotently.
- [ ] Logs carry `request_id`, `tenant_id` and the trace ID, and contain no secrets or full personal data.
- [ ] Metrics exist for throughput, errors and latency, with at least one alert rule.
- [ ] The v3 importer for the module's tables exists and has a dry-run test, where the module replaces v3 data.
- [ ] OpenAPI examples run green in e2e, and the developer docs page is drafted.
- [ ] `make check` is green, `/review` findings are resolved, and `docs/STATUS.md` is updated.

### 8.7 CI pipeline

1. **Fast job:** `make gen-check lint test`.
2. **Integration job:** `make test-int test-contract` with Docker available to testcontainers.
3. **E2E job** (pull requests to `main`): start `vaspro-all` and the fakes, then run `make test-e2e`.
4. **Security job:** `govulncheck`, `gitleaks`, and `trivy` on built images.
5. **Build job:** build each `cmd/*` binary and container image, tagged with the commit.
6. **Nightly:** long fuzz runs, chaos scenarios, k6 smoke run on staging, and the Daraja sandbox contract run.

---

## 9. Phase map and milestones

Sizes are relative: **S** under a week, **M** 2–3 weeks, **L** 4–6 weeks, **XL** more than 6 weeks of focused build. They are estimates to calibrate after Phase 1, not commitments.

| Phase | Name | Depends on | Size | Milestone |
|---|---|---|---|---|
| A | Finish the design (data model Parts 3–4, catalogues, OpenAPI, packs) | — | M | Runs alongside Phases 0–2 |
| 0 | Foundations and platform | — | L | **M0 platform ready** |
| 1 | Identity, access and API gateway | 0 | L | |
| 2 | Accounts, KYC and onboarding | 1 | M | |
| 3 | Ledger and wallets | 0, 2 | L | |
| 4 | Payments I: wallet top-ups | 3 | M | |
| 5 | Webhooks, events and reporting | 0 | M | Can run alongside 3–4 |
| 6 | Contacts and consent | 2 | M | Can run alongside 4–5 |
| 7 | Messaging (SMS) and SMS engine adapter | 3, 4, 5, 6, OD-10 | XL | **M1 internal alpha** |
| 8 | Developer portal and sandbox | 7 | M | |
| 9 | Billing and invoicing | 3, 4 | L | |
| 10 | Applications and staff console | 2, 9 | L | |
| 11 | USSD gateway | 3, 10 | L | |
| 12 | Shortcode MO/MT, keywords, inbox, subscriptions, premium | 7, 10 | L | |
| 13 | Payments II: client collections, payouts, airtime, data and bills | 4, 9 | L | |
| 14 | Surveys and rewards | 7, 11, 13 | M | |
| 15 | WhatsApp, Telegram, Messenger, and OTP service (v3 parity) | 7 | L | |
| 16 | Monitoring and alerting engine, operations readiness | 5, all parity phases | M | **M2 closed beta (Kenya parity)** |
| 17 | Hardening and launch readiness | 16 | M | |
| 18 | v3 migration and cutover | 17 | L | **M3 Kenya GA; v3 off** |
| 19 | Growth I: Verify advanced, lookup, fraud, email, RCS, failover, analytics | 18 | L | |
| 20 | Growth II: journeys, AI agents, unified inbox, AI support, MCP, integrations, white-label | 18 | XL | |
| 21 | Tanzania and Uganda | 18 | L | **M4 TZ live; M5 UG live** |
| 22 | Continuous operations | 18 | Ongoing | |

**Milestones**

| Milestone | Means | Who uses it |
|---|---|---|
| M0 | Every platform library works and is tested; a new service can be scaffolded and run | The build team |
| M1 | Staff accounts send real SMS through the existing engine, with correct charges and delivery states | VASPRO staff |
| M2 | Every v3 feature works on v4 for a group of friendly clients | 5–10 selected clients |
| M3 | All Kenyan clients are on v4 and v3 is switched off | Everyone |
| M4, M5 | Tanzania, then Uganda, launched from their packs | New markets |

---

## 10. Stage A: finish the design

Stage A runs alongside Phases 0–2. Each item must be done before the first phase that needs it.

- [ ] **A1. Data model Part 3:** messaging, USSD, contacts and consent, applications, including the ClickHouse tables and the hand-off log for the SMS engine. *Needed by Phase 6.*
- [ ] **A2. Data model Part 4:** verify, journeys, surveys and rewards, webhooks, monitoring, sales and referrals. *Webhooks needed by Phase 5.*
- [ ] **A3. Event catalogue:** every event type with a JSON Schema, its producer and consumers, in `docs/events/`. *Needed by Phase 0 for the envelope; per service before its phase.*
- [ ] **A4. Error-code catalogue:** every code with HTTP status, title, cause, fix, and `retryable` and `billable` flags, in `docs/errors/catalogue.yaml`. *Needed by Phase 0.*
- [ ] **A5. OpenAPI 3.1 per service** in `docs/api/`, written just in time: each service's spec is reviewed before its phase starts.
- [ ] **A6. Country pack schema and KE pack v1:** numbering, operators, currency, tax, send windows, compliance keywords, routes per operator (hand-off, SMSC, Safaricom package and sender mapping), rails, test numbers. *Schema needed by Phase 0; routes needed by Phase 7.*
- [ ] **A7. SMS engine contract confirmed** with whoever runs the engine: answers to OD-10 and OD-11, and the v3 golden samples from section 8.5. *Needed by Phase 7.*
- [ ] **A8. Open decisions** in section 4.3 either decided or explicitly left on their default, each with an ADR.

**Kickoff prompt (Stage A, per item):**

> We're doing Stage A item A<n> from docs/ROADMAP.md. Read the related sections of docs/design/system-design.md and docs/design/data-model.md. Draft the output in the format the data model already uses (or as OpenAPI 3.1 / JSON Schema / YAML as the item says). List every assumption you make as an open question in docs/decisions/open-questions.md. Don't write application code.

---

## 11. Build phases 0 to 22

Each phase lists its goal, what to read first, the tasks, the tests specific to it (on top of the definition of done in section 8.6), the exit gate and the kickoff prompt.

---

### Phase 0: Foundations and platform

**Goal.** An empty repository becomes a working platform: local stack, CI, and the shared libraries every service needs, all tested.

**Read first.** Data model §1 (shared rules) and §2 (schema map); system design §1 and §8; this roadmap sections 5–8.

**Tasks**

- [x] 0.1 Repository skeleton: `go.mod`, `Makefile` with every target in section 8.2, `.golangci.yml` (including `depguard` boundaries and `gosec`), `.editorconfig`, `.gitignore`.
- [x] 0.2 Local stack in `deploy/compose/`: PostgreSQL 16, Redis 7, NATS with JetStream, RabbitMQ with the management UI, ClickHouse, MinIO, Mailpit, `grafana/otel-lgtm`. Health checks; `make up` waits until everything is healthy. (MinIO swapped for SeaweedFS's S3 gateway — see docs/STATUS.md; same S3 API.)
- [x] 0.3 CI workflow with the jobs in section 8.7.
- [x] 0.4 `platform/config` (environment variables, validation at start-up), `platform/log` (`slog` JSON with request, tenant and trace IDs), `platform/otel` (traces and metrics).
- [x] 0.5 `platform/problem` and `platform/httpx`: problem+json writer; error type with code families; generated Go constants from `docs/errors/catalogue.yaml`; middleware for request ID, panic recovery (turning panics into `SYS-` problems without stack traces), timeouts, body limits, strict CORS.
- [x] 0.6 `platform/db`: pgx pool; `WithTenantTx(ctx, tenantID, fn)` that runs `SET LOCAL app.tenant_id`; one database role per service; migrations per schema with goose; sqlc configuration.
- [x] 0.7 `platform/ids` (UUIDv7, prefixed public IDs), `platform/money` (minor units, currency exponents, allocation without losing a cent, VAT split), `platform/clock`.
- [x] 0.8 `platform/idempotency` (HTTP middleware and store), `platform/outbox` (write in the business transaction; relay to the bus), `platform/inbox` (`processed_events`), `platform/events` (envelope: `id`, `type`, `version`, `tenant_id`, `occurred_at`, `trace_id`, `data`).
- [x] 0.9 `platform/bus` (NATS JetStream publish, durable consumers, retries, dead-letter subject) and `platform/cache` (Redis).
- [x] 0.10 `platform/packs`: pack JSON Schema, loader, version pinning, and a `vaspro packs validate` command. A KE pack v0 with numbering, currency, tax and send windows.
- [x] 0.11 `platform/crypto`: KMS interface with a local implementation, envelope encryption, HMAC helpers, Argon2id.
- [x] 0.12 `platform/testkit`: PostgreSQL, NATS, Redis and RabbitMQ through testcontainers with migrations applied; factories; golden-file helper; fake clock; an RLS assertion helper.
- [x] 0.13 Service template and `make new-service`; `cmd/vaspro-all`; health and readiness endpoints; graceful shutdown.
- [x] 0.14 ADRs: 0001 event bus, 0002 repository and deployment shape, 0003 migration tool, 0004 code generation, 0005 error catalogue format.

**Tests specific to this phase**

- Outbox: an event is written only if the business transaction commits; when the relay crashes after publishing but before marking the row, the event is published again and the consumer's inbox applies it once.
- Idempotency: the same key with the same body returns the first response; the same key with a different body returns a `VAL-` conflict; keys expire as configured.
- Problem+json: golden output for each code family; a panic returns `SYS-9000` with no stack trace in the body.
- RLS helper: a sample table proves cross-tenant reads and writes fail.
- Money: property test that allocating any amount over any ratios sums back exactly; VAT split matches the data model's worked example (304.00 → 262.07 revenue and 41.93 VAT at 16% inclusive); UGX with zero decimals.
- UUIDv7 IDs sort by creation time; public ID prefixes round-trip.
- Pack loader rejects an invalid pack with a clear error.
- `depguard` fails the build if one service imports another.

**Exit gate (M0)**

- [ ] `make up` brings the stack up healthy on a clean machine.
- [ ] `make check` is green in CI.
- [ ] `make new-service NAME=demo` produces a service that runs inside `vaspro-all`, serves `/healthz`, writes an event through the outbox, and consumes it idempotently, with tests.
- [x] ADRs 0001–0005 are merged.

**Kickoff prompt**

> We're starting Phase 0 of docs/ROADMAP.md. Read sections 0 and 2–8 and the Phase 0 section, plus data model §1–2 and system design §1 and §8. In plan mode, break Phase 0 into tasks 0.1–0.14 in the order you'd do them, list the tests for each, and name anything you need from me. Then start with task 0.1 only.

---

### Phase 1: Identity, access and API gateway

**Goal.** People and machines can prove who they are, and every request goes through one gateway that enforces credentials, scopes, limits and idempotency.

**Read first.** System design §2–3 and §9; data model §3; requirements, core requirement 1.

**Tasks**

- [x] 1.1 `identity` migrations and sqlc queries from data model §3.
- [x] 1.2 Sign-up with email verification. Email is unique and case-insensitive; phone is optional.
- [x] 1.3 Password sign-in with Argon2id; TOTP enrolment and verification; recovery codes.
- [x] 1.4 Passkeys (WebAuthn): registration, sign-in, and management of several passkeys.
- [x] 1.5 Sessions in PostgreSQL with a Redis cache; list and revoke sessions; sign out everywhere.
- [x] 1.6 Step-up MFA for sensitive actions, as listed in system design §3.
- [x] 1.7 API clients: create, rotate (two active secrets), revoke; scopes, IP allowlist, expiry, spend cap; the secret is shown once and stored hashed.
- [x] 1.8 OAuth2 client-credentials token endpoint; JWT signing keys with rotation; JWKS endpoint.
- [x] 1.9 HMAC request signing for high-value calls: signature over method, path, body and timestamp, valid for 5 minutes, with replay protection.
- [x] 1.10 SMPP accounts for the existing SMPP front end (system design §3, §5.2).
- [x] 1.11 Gateway: routing, JWT checks, scopes, Redis rate limits with `Retry-After`, IP allowlists, sandbox or live environment taken from the credential, idempotency on writes, OpenAPI request validation, audit events.
- [x] 1.12 Security events log; login alerts by email.
- [x] 1.13 v3 importer: users and profiles. v3 passwords are bcrypt of MD5: verify that way once, then re-hash with Argon2id. Users without a usable email are flagged.
- [x] 1.14 Design (not enable) the v3 token compatibility route (OD-16).
- [>] 1.15 OIDC single sign-on for clients who used Okta in v3, if time allows; otherwise in Phase 20. **Deferred to 20.8** (2026-10-23): the design leaves domain verification, tenant binding, account linking and IdP secret storage open, see OQ-03.

**Tests specific to this phase**

- TOTP against the RFC 6238 test vectors, including time-step drift of ±1.
- WebAuthn registration and assertion with a virtual authenticator; a sign counter that goes backwards is rejected.
- v3 password import: a known bcrypt(MD5) hash verifies once and is replaced by Argon2id; the old hash no longer works afterwards.
- Lockout after the configured number of failures; unlock by time and by staff.
- JWT: expired, wrong audience, unknown key ID, and a key rotated out but still in its overlap window.
- HMAC: wrong signature, timestamp older than 5 minutes, replayed nonce.
- Rate limit returns 429 with `Retry-After`; a missing scope returns 403 with an `AUTH-` code; a sandbox credential cannot reach live routes.
- Fuzz test for email normalisation.
- E2E: sign up → verify email → enrol TOTP → sign in → create API client → get a token → call a protected endpoint → revoke the client → the call fails.

**Exit gate**

- [ ] The e2e scenario above is green.
- [ ] A negative-test pass covers every endpoint without a token, with a wrong scope, and with another tenant's IDs.
- [ ] No secret, password or token appears in any log line; a test greps test logs for them.

**Kickoff prompt**

> We're starting Phase 1 of docs/ROADMAP.md. Read the Phase 1 section, system design §2, §3 and §9, and data model §3. Check docs/api/identity.yaml and docs/api/gateway.yaml exist and cover tasks 1.2–1.11; if not, draft them first and stop for my review. Then plan tasks 1.1–1.14 in order, with the tests for each.

---

### Phase 2: Accounts, KYC and onboarding

**Goal.** A sign-up becomes a sandbox account; the client picks services; verification moves the account up tiers; teams and resellers work.

**Read first.** System design §2; data model §4; requirements, core requirements 6 and 8.

**Tasks**

- [ ] 2.1 `accounts` migrations and queries from data model §4.
- [ ] 2.2 Tenant created in the sandbox at sign-up; tenant types (direct, reseller, sub-account).
- [ ] 2.3 Memberships, roles and invitations; one email can belong to several accounts.
- [ ] 2.4 Service catalogue, service selection at onboarding, and the tier each service needs per country.
- [ ] 2.5 Tier state machine with `tier_changes` written in the same transaction as the tier.
- [ ] 2.6 KYC adapter interface, fake vendor and webhook handling; the real vendor when OD-03 is decided.
- [ ] 2.7 KYB: business profile, directors and beneficial owners, registry lookup adapter.
- [ ] 2.8 Documents: upload to object storage, hash, type and size checks, virus-scan hook, reuse across applications.
- [ ] 2.9 Staff review queue for KYC and KYB decisions marked "review".
- [ ] 2.10 Data residency tag per tenant.
- [ ] 2.11 v3 importers: clients, user-client links, permissions to roles, resellers to parent tenants.

**Tests specific to this phase**

- Table test of every tier transition, allowed and forbidden.
- Every tier change has a `tier_changes` row; a test tries to change the tier without one and fails.
- KYC webhooks are idempotent and verified; out-of-order results don't demote an account.
- Role × permission matrix test for every role.
- Document checks: wrong type, too large, duplicate (same hash reused, not stored twice).
- E2E: sign up → pick SMS and USSD → Tier 1 KYC through the fake vendor → account promoted → `tenant.tier_changed` published.

**Exit gate**

- [ ] The onboarding e2e is green.
- [ ] The v3 client importer dry run on an anonymised copy reports counts that match v3, and lists every flagged record.

**Kickoff prompt**

> We're starting Phase 2 of docs/ROADMAP.md. Read the Phase 2 section, system design §2 and data model §4. Check docs/api/accounts.yaml covers tasks 2.2–2.9. Plan tasks 2.1–2.11 with tests first, and start with 2.1.

---

### Phase 3: Ledger and wallets

**Goal.** Every unit of money and tokens moves through balanced, immutable journal entries, and it can be proven.

**Read first.** System design §4 (all subsections); data model §5, including the worked example.

**Tasks**

- [ ] 3.1 `ledger` migrations: currencies, accounts, balances, token prices, rate cards, tax rates, transactions, entries, token lots, lot consumptions, holds, usage batches, auto-buy rules, reversals.
- [ ] 3.2 Database guards: a deferred trigger that rejects unbalanced transactions; a trigger and grants that block updates and deletes on transactions and entries; a trigger that stops client wallets going below zero.
- [ ] 3.3 Posting API over gRPC: post a transaction, place a hold, capture, release, query balances. Every call is idempotent.
- [ ] 3.4 Token purchase from the main wallet into a service wallet, creating a lot at the client's price (negotiated deal if one is in force, otherwise the list price and its volume tier); FIFO use of lots. Staff console screen for entering and approving deals (maker-checker).
- [ ] 3.5 Usage settlement: usage batches per wallet per settlement window, splitting revenue and VAT from the lots actually used.
- [ ] 3.6 Hold expiry job; auto-buy rules.
- [ ] 3.7 Reversals with maker-checker, as in system design §4.3.
- [ ] 3.8 Nightly job that rebuilds every balance from the entries and raises a P1 event on any difference.
- [ ] 3.9 Wallet and token APIs for the dashboard (balances, statements, buying tokens).
- [ ] 3.10 v3 importer: opening balances, bonuses and commissions as one migration transaction per client; v3 transaction history as read-only migration records.

**Tests specific to this phase**

- Property test: for any random sequence of valid operations, debits equal credits per currency, and every balance equals the replay of its entries.
- Concurrency: 200 goroutines hold against one wallet at once; nothing is overspent; run with `-race`.
- Golden test of the worked example: client deal KES 0.80 per SMS unit; KES 13,000 top-up, buy 10,000 units (8,000.00), 400-SMS campaign at 1 unit each, 380 delivered → SMS wallet 304.00, revenue 262.07, VAT 41.93, 20 units released. Also the tax-exclusive variant (352.64). Then a new deal of KES 0.50: the next 10,000 units cost 5,000.00 and the 9,620 units left at 80 cents are used first.
- Pricing: a client with a deal in force always gets the deal price; without one, the list tier matching the quantity applies; a deal that has ended falls back to the list price; an unapproved price row is never used; two active prices for the same client, service and tier cannot overlap.
- Token price change: lots bought before the change keep their price; use draws from the oldest lot first.
- An update or delete on `entries` fails; a transaction that doesn't balance fails at commit.
- A reversal approved by its own maker fails at the database.
- Tamper test: change a balance row directly; the nightly rebuild detects it and publishes the P1 event.
- UGX amounts (zero decimals) and fractional tokens round correctly.

**Exit gate**

- [ ] All ledger invariants above are green.
- [ ] Benchmark recorded: holds and captures per second on the reference machine, kept as a baseline.
- [ ] Finance has reviewed the worked examples and the reversal rules.
- [ ] The v3 balance import dry run reproduces v3 totals per client to the cent.

**Kickoff prompt**

> We're starting Phase 3 of docs/ROADMAP.md. Read the Phase 3 section, system design §4 and data model §5 in full, including the worked example. Start with the migrations and the database guards (3.1–3.2), with their tests written first. Money correctness matters more than speed here: stop and ask about anything the design doesn't state.

---

### Phase 4: Payments I, wallet top-ups

**Goal.** Clients top up their main wallet by M-Pesa STK push and paybill, and every shilling is matched or sent to suspense, then reconciled.

**Read first.** System design §4.1 and §13; data model §7 (rails, rail accounts, credentials, intents, collection notices, reconciliation).

**Tasks**

- [ ] 4.1 `payments` migrations for rails, rail accounts, rail credentials, payment intents, collection notices, statement imports and lines, recon exceptions.
- [ ] 4.2 Daraja adapter: OAuth token cached in Redis, STK push, STK query, C2B URL registration, validation and confirmation callbacks. Callbacks are checked by source and shared secret.
- [ ] 4.3 VASPRO's own paybill as a rail account; a unique account number per client.
- [ ] 4.4 Top-up by STK: intent → callback → ledger top-up posting (debit provider clearing, credit main wallet).
- [ ] 4.5 Top-up by paybill (C2B): match on account number; unmatched money goes to suspense; staff match it later, once.
- [ ] 4.6 Daily reconciliation of VASPRO's paybill against provider clearing in the ledger.
- [ ] 4.7 Airtel Money KE adapter, if the account is ready; otherwise left for later.
- [ ] 4.8 A minimal hosted checkout page, reused by Pay now in Phase 9.

**Tests specific to this phase**

- Fake Daraja outcomes: success, cancelled by user, timeout, insufficient funds, wrong PIN.
- A duplicate callback is ignored. A callback that arrives before the STK request is stored is still handled.
- An unmatched C2B payment lands in suspense; matching it credits the wallet exactly once.
- The amount on the callback differs from the intent: the callback amount wins and the mismatch is flagged.
- Reconciliation: a rail line missing from VASPRO, and a VASPRO record missing from the rail, each open an exception.
- Nightly job against the real Daraja sandbox (not on every PR).

**Exit gate**

- [ ] A real Daraja sandbox top-up credits a wallet end to end on staging.
- [ ] A day of fake traffic reconciles with zero unexplained differences.

**Kickoff prompt**

> We're starting Phase 4 of docs/ROADMAP.md. Read the Phase 4 section, system design §4.1 and §13, and data model §7. Build the Daraja fake in tools/fakes first, with the outcomes listed in the Phase 4 tests, then the adapter against it. Plan tasks 4.1–4.8 with tests first.

---

### Phase 5: Webhooks, events and reporting

**Goal.** Every state change reaches the client as a signed, retried, replayable webhook, and lands in ClickHouse for reports.

**Read first.** System design §8 and §9; data model Part 4 (webhooks); `docs/events/`.

**Tasks**

- [ ] 5.1 `webhooks` migrations: endpoints, subscriptions, deliveries, attempts.
- [ ] 5.2 Endpoint registration with a verification challenge; secret rotation.
- [ ] 5.3 Signing: HMAC-SHA256 over the timestamp and body, sent in headers.
- [ ] 5.4 Delivery worker: exponential backoff, maximum attempts, dead letter, automatic disabling after repeated failures (with a client alert), manual replay.
- [ ] 5.5 Delivery log API for the dashboard and the portal's webhook inspector.
- [ ] 5.6 ClickHouse sink: every event into an events table, de-duplicated on event ID; first report queries.
- [ ] 5.7 Verification snippets for clients in Go, PHP, Node and Python.

**Tests specific to this phase**

- The snippets in every language verify signatures produced by our signer (golden).
- An endpoint that returns 500 and then 200 gets the event once successfully; the retry schedule matches the configuration.
- A slow endpoint times out without blocking other deliveries.
- A replay sends the same event ID, so clients can de-duplicate.
- ClickHouse: the same event delivered twice is counted once.

**Exit gate**

- [ ] Events from Phases 1–4 reach a test endpoint signed and verified.
- [ ] The delivery log shows every attempt with its response.

**Kickoff prompt**

> We're starting Phase 5 of docs/ROADMAP.md. Read the Phase 5 section, the webhooks part of the data model, and docs/events/. Plan tasks 5.1–5.7 with tests first.

---

### Phase 6: Contacts and consent

**Goal.** Clients manage contacts, groups and segments, and every send can check consent and opt-outs per channel and purpose.

**Read first.** Data model Part 3 (contacts); system design §5; requirements, contacts and consent hub.

**Tasks**

- [ ] 6.1 `contacts` migrations: contacts, attributes, groups, segments, consents, suppression lists.
- [ ] 6.2 Imports from CSV and XLSX, streamed, with a per-row error report.
- [ ] 6.3 Segments from attribute filters.
- [ ] 6.4 Consent per channel and purpose; opt-out keywords from the pack; suppression lists; a preference-centre link.
- [ ] 6.5 A consent-check API used by messaging: one call for a batch of numbers.
- [ ] 6.6 v3 importer: contact groups and contacts.

**Tests specific to this phase**

- Number normalisation for KE formats (`07…`, `01…`, `254…`, `+254…`) to E.164; invalid numbers reported with the row.
- Duplicates within a file and across imports are merged.
- An import of 1,000,000 rows finishes within the agreed time, with flat memory use.
- Table test of consent outcomes for channel × purpose × state.
- Opt-out keyword variants from the pack are recognised.

**Exit gate**

- [ ] The batch consent check meets its latency target for 10,000 numbers.
- [ ] The v3 contacts import dry run matches v3 counts.

**Kickoff prompt**

> We're starting Phase 6 of docs/ROADMAP.md. Read the Phase 6 section and the contacts part of data model Part 3. Plan tasks 6.1–6.6 with tests first.

---

### Phase 7: Messaging (SMS) and the SMS engine adapter

**Goal.** v4 accepts, checks, charges, routes and hands off SMS to the existing engine exactly as v3 does, then reads delivery reports, settles and reports. **This phase ends at M1.**

**Blocked until** OD-10 is answered and the v3 golden samples (section 8.5) are in `test/contract/v3/`.

**Read first.** System design §5, §5.1 and **§5.2 (SMS engine interface)**, §8; data model Part 3 (messaging); KE pack routes.

**Tasks**

- [ ] 7.1 `messaging` migrations: message requests, campaigns, routes chosen, hand-off log, DLR mapping, client limits. Per-message events go to ClickHouse.
- [ ] 7.2 Send API (single and batch) with idempotency keys.
- [ ] 7.3 Validation: E.164, operator detection, GSM-7 or UCS-2 encoding, segment count, sender ID ownership.
- [ ] 7.4 Compliance: consent (Phase 6), send windows from the pack in the client's time zone, tier caps, sandbox sending only to verified numbers.
- [ ] 7.5 Client limits (system design §5.1): hard and soft limits, warning at 80%, `LIM-` errors.
- [ ] 7.6 Rating and holds through the ledger; settlement through usage batches; release on non-billable failures.
- [ ] 7.7 Route choice from the country pack: hand-off type, queue or gateway, SMSC, Safaricom package and sender mapping, priority.
- [ ] 7.8 **SMS engine adapter**: publish to the RabbitMQ queues with the exchange, routing key, persistent delivery and JSON fields v3 uses; make the HTTP gateway calls v3 makes; carry the v4 message ID as the correlator.
- [ ] 7.9 DLR intake for every format in system design §5.2; mapping raw values to statuses and `DLR-` reasons through the pack's tables; duplicates ignored.
- [ ] 7.10 Retries on the pack's next route, up to the pack's limit; expiry when no DLR arrives.
- [ ] 7.11 Campaigns: scheduled sends; recipients expanded by v4 from contacts (not `VASPRO_BULK_COMPOSE`); one up-front balance check; pacing.
- [ ] 7.12 Message timeline in the API and dashboard.
- [ ] 7.13 Sandbox: the simulator adapter replaces the engine for sandbox credentials.
- [ ] 7.14 Shadow mode: for a copy of v3 traffic, v4 computes route, price and compliance decisions without sending, and a report compares them with v3.

**Tests specific to this phase**

- Segment counting golden table: GSM-7 basic and extended characters, UCS-2 with emoji, boundaries at 160/153 and 70/67.
- Send windows across midnight and in each client time zone.
- Holds: partial delivery captures only delivered, billable messages.
- **Contract tests:** each payload v4 publishes or sends matches the v3 golden samples field for field. Queue, exchange and routing key names, and `delivery_mode: 2`, are checked against a real RabbitMQ in testcontainers.
- Correlator round trip: a DLR carrying the correlator finds the right message every time.
- Fuzz test of DLR parsing with malformed Kannel strings.
- A DLR that arrives before the hand-off is logged is still applied; a duplicate DLR changes nothing.
- Load: a campaign of 1,000,000 recipients is expanded and handed off within the target, with flat memory.
- Resilience: with RabbitMQ stopped, messages stay queued in v4, an alert fires, and nothing is charged twice when it returns.
- Staging test with the **real** SMS engine and test numbers on each network before M1.

**Exit gate (M1)**

- [ ] Staff accounts send real SMS on Safaricom, Airtel and Telkom through the existing engine, with correct charges, VAT and final statuses.
- [ ] The shadow-mode report shows v4's decisions match v3's, or every difference is explained and accepted.
- [ ] Load and resilience results are recorded in `docs/runbooks/messaging.md`.

**Kickoff prompt**

> We're starting Phase 7 of docs/ROADMAP.md. First confirm that OD-10 is answered in docs/decisions/ and that test/contract/v3/ has the golden samples; if either is missing, stop and tell me. Then read system design §5–5.2 and §8, the messaging part of data model Part 3, and the KE pack routes. Plan tasks 7.1–7.14. Build the SMS engine fake and the contract tests (7.8) before the send API.

---

### Phase 8: Developer portal and sandbox

**Goal.** A developer can sign up, test every API in the sandbox, and go live by swapping keys.

**Read first.** System design §9; requirements, core requirement 9.

**Tasks**

- [ ] 8.1 API reference rendered from OpenAPI; guides per use case; changelog and versioning policy.
- [ ] 8.2 Sandbox credentials; simulators backed by `tools/fakes` with the reserved test numbers; free sandbox credit.
- [ ] 8.3 API explorer that runs calls with the user's sandbox key.
- [ ] 8.4 Webhook inspector on top of Phase 5's delivery log.
- [ ] 8.5 Go-live checklist: KYC tier, first top-up, sender ID, verified callback URL; then live credentials.
- [ ] 8.6 SDKs generated from OpenAPI for Go, PHP, Node, Python and Java, plus a Postman collection.
- [ ] 8.7 First staging environment (OD-02).

**Tests specific to this phase**

- Every OpenAPI operation's example runs green against the sandbox in CI (the docs are tests).
- SDK smoke tests in each language run in CI.
- Isolation: a sandbox credential can never reach the real engine or a real rail; the adapter registry refuses it, and a test proves it.

**Exit gate**

- [ ] A developer who hasn't seen the system completes "send an SMS and receive the DLR webhook" in the sandbox using only the portal.

**Kickoff prompt**

> We're starting Phase 8 of docs/ROADMAP.md. Read the Phase 8 section and system design §9. Plan tasks 8.1–8.7 with tests first, starting with sandbox isolation (8.2).

---

### Phase 9: Billing and invoicing

**Goal.** Every recurring charge becomes one invoice per client per cycle; usage gets a monthly tax invoice; unpaid invoices are chased; Kenya invoices go through eTIMS.

**Read first.** System design §11 and §4.3 (credit notes); data model §6.

**Tasks**

- [ ] 9.1 `billing` migrations from data model §6.
- [ ] 9.2 Billing profiles; plans and subscriptions (including recurring top-ups).
- [ ] 9.3 Billable assets register, fed by Applications (Phase 10) and USSD (Phase 11) through events.
- [ ] 9.4 Daily billing run, safe to repeat; proration; gapless numbering per country.
- [ ] 9.5 Usage accruals from `usage.posted` events; monthly usage invoice issued as paid.
- [ ] 9.6 Ledger postings for each invoice kind, as in data model §6.
- [ ] 9.7 Auto-pay (wallet, then card or standing order); Pay now links and the hosted checkout from Phase 4.
- [ ] 9.8 Dunning with per-asset suspension and reactivation on payment.
- [ ] 9.9 Credit notes with maker-checker.
- [ ] 9.10 eTIMS adapter, fake and retries; PDFs in object storage; invoice emails.
- [ ] 9.11 v3 importer: open invoices with their balances; paid invoices as history.

**Tests specific to this phase**

- Running the billing job twice for the same day creates one invoice per client.
- Proration golden tests for an asset added mid-cycle and one released mid-cycle.
- 50 invoices issued in parallel get consecutive numbers with no gaps and no duplicates.
- A redelivered `usage.posted` event is counted once.
- eTIMS fake: accepted, rejected, timeout then retry.
- Dunning: reminders on schedule; only the unpaid asset is suspended; paying reactivates it at once.
- The ledger postings for each invoice kind match the table in data model §6.
- Pay now link: works once, then stops when the invoice is paid, voided or expired.
- PDF snapshot test.

**Exit gate**

- [ ] Finance signs off sample invoices of every kind.
- [ ] KRA's eTIMS sandbox accepts an invoice and a credit note.

**Kickoff prompt**

> We're starting Phase 9 of docs/ROADMAP.md. Read the Phase 9 section, system design §11 and data model §6. Plan tasks 9.1–9.11 with tests first, starting with the billing run's idempotency.

---

### Phase 10: Applications and staff console

**Goal.** Sender IDs, shortcodes and dedicated USSD codes are applied for, tracked, approved and paid for through one tracked workflow, with staff working from a console instead of email.

**Read first.** System design §7; data model Part 3 (applications); requirements, core requirement 5.

**Tasks**

- [ ] 10.1 `applications` migrations: applications, per-network items, MNO options, ops tickets, SLA timers.
- [ ] 10.2 Client wizard driven by the pack; documents reused from KYB.
- [ ] 10.3 State machine with the 6 main and 4 side states in system design §7.
- [ ] 10.4 Ops tickets; staff record the MNO's available options; the client picks; staff apply until approved.
- [ ] 10.5 Payment window after approval; payment creates the billable asset (Phase 9) exactly once.
- [ ] 10.6 Notifications on each state change (email, dashboard, webhook).
- [ ] 10.7 Staff console (web/console): review queue, checklists, operator batches, bulk status updates, renewals.
- [ ] 10.8 v3 importer: sender IDs and their networks, shortcodes, USSD codes; v3's numeric statuses mapped to named states.
- [ ] 10.9 Plan the v3 migration waves (OD-15) and write `docs/runbooks/cutover.md` v1.

**Tests specific to this phase**

- Table test of every state transition and who may make it.
- SLA timers fire and escalate.
- An expired payment window releases the option and notifies the client.
- Payment creates one billable asset even if the payment event is delivered twice.
- Applications API has parity with the wizard.

**Exit gate**

- [ ] A sender ID goes from draft to active in staging, with a staff user acting through the console.
- [ ] The v3 importer maps every v3 status, and reports any record it can't.

**Kickoff prompt**

> We're starting Phase 10 of docs/ROADMAP.md. Read the Phase 10 section, system design §7 and the applications part of data model Part 3. Plan tasks 10.1–10.9 with tests first, starting with the state machine.

---

### Phase 11: USSD gateway

**Goal.** Clients build, test and publish USSD services themselves on shared, extension-range and dedicated codes, billed per session.

**Read first.** System design §6; data model Part 3 (USSD); requirements, core requirement 3.

**Tasks**

- [ ] 11.1 `ussd` migrations: codes, apps, menu versions. Live sessions in Redis; session events in ClickHouse.
- [ ] 11.2 Operator USSD gateway adapters (Safaricom, Airtel, Telkom) as defined in the KE pack.
- [ ] 11.3 Session engine: state, timeouts per network, response budget, failover URL, default message.
- [ ] 11.4 No-code menu builder: menus, inputs, validation, M-Pesa checkout, survey questions, airtime reward; versions with publish and rollback.
- [ ] 11.5 Developer callback mode (CON/END).
- [ ] 11.6 Shared channels and extension ranges (OD-14); dedicated codes through Applications.
- [ ] 11.7 Per-session billing from the USSD token wallet; behaviour when the wallet is empty.
- [ ] 11.8 Handset simulator for the dashboard and sandbox.
- [ ] 11.9 v3 importer: USSD apps and codes.

**Tests specific to this phase**

- Session state machine, including timeouts and network drops.
- A callback that misses the response budget goes to the failover URL, then to the default message.
- Publishing a menu version doesn't affect sessions already in progress; rollback restores the previous version.
- Contract tests for each operator's USSD request and response format.
- Load: 10,000 concurrent sessions within the latency budget.

**Exit gate**

- [ ] A client builds a menu, tests it in the simulator, and runs it live on a shared code in staging.

**Kickoff prompt**

> We're starting Phase 11 of docs/ROADMAP.md. Read the Phase 11 section, system design §6 and the USSD part of data model Part 3. Plan tasks 11.1–11.9 with tests first, starting with the session engine.

---

### Phase 12: Shortcode MO/MT, keywords, inbox, subscriptions and premium

**Goal.** v3's two-way and premium features run on v4: MO messages, keywords, the inbox, subscriptions, replies, and premium MT charged by the operator.

**Blocked until** OD-11 (how MO reaches v4) is answered.

**Read first.** System design §12; data model Part 3 (messaging, subscriptions).

**Tasks**

- [ ] 12.1 MO intake through the path confirmed in OD-11; becomes `message.received`.
- [ ] 12.2 Keywords on shared and dedicated shortcodes; routing MO to the client (inbox, webhook, survey, journey).
- [ ] 12.3 Inbox with replies.
- [ ] 12.4 Subscription lifecycle: opt-in, confirmation stating the price, STOP; operator subscription notifications.
- [ ] 12.5 Reply MT within the operator's linkId window.
- [ ] 12.6 **Operator SDP adapter** (v4's own, not the SMS engine) for premium MT, on-demand and subscription, with offer codes and tariffs.
- [ ] 12.7 Charging outcomes and retries; revenue share payable in the ledger; monthly statements.
- [ ] 12.8 Scheduled and birthday messages (v3 parity).
- [ ] 12.9 v3 importers: keywords, subscription products, subscribers and their audit trail.

**Tests specific to this phase**

- A reply after the linkId window is rejected with the right code.
- A premium MT to a number with no active subscription is rejected with a `CMP-` code before reaching the operator.
- Charging outcomes: charged, insufficient airtime with retry, failed.
- Revenue share totals per shortcode and offer code match the charged MTs.
- SDP contract tests against golden samples from v3's on-demand path.

**Exit gate**

- [ ] A subscription service runs end to end in staging: opt-in, a premium MT charged, STOP.

**Kickoff prompt**

> We're starting Phase 12 of docs/ROADMAP.md. First confirm OD-11 is answered; if not, stop. Read the Phase 12 section and system design §12. Plan tasks 12.1–12.9 with tests first.

---

### Phase 13: Payments II: client collections, payouts, airtime, data and bills

**Goal.** Clients collect into their own paybills and tills and pay out from their own shortcodes, with VASPRO driving the rails, charging its fee, and reconciling. Airtime, data and bills work as VASPRO products.

**Blocked until** OD-12 is confirmed.

**Read first.** System design §13; data model §7.

**Tasks**

- [ ] 13.1 Client rail accounts; credentials entered by clients, envelope-encrypted, verified with a test call, and rotated.
- [ ] 13.2 Client collections: STK push to the client's paybill or till; C2B registration; `payment.received` webhooks.
- [ ] 13.3 Payment links for the client's own customers.
- [ ] 13.4 Payouts: single by API; bulk by file with row-by-row validation; approval with maker-checker; name checks; beneficiaries and allowlist.
- [ ] 13.5 Fee holds on the payments token wallet; capture on success; release on failure.
- [ ] 13.6 Status query before any retry; timeout and result callbacks; payout attempts log.
- [ ] 13.7 Balance checks on client shortcodes; low-balance alerts.
- [ ] 13.8 Airtime, data and bills through VASPRO's providers, drawing on the airtime wallet; provider float monitoring.
- [ ] 13.9 Reconciliation per client rail account.
- [ ] 13.10 v3 importers: client paybills and settings (without credentials), collection history (OD-22), payout batches and transactions, B2B transfers, utilities.

**Tests specific to this phase**

- Never pay twice: simulate a lost response after the rail accepted a payout; the system queries status and does not resubmit.
- Maker-checker: the creator of a batch can't approve it (database check and API test).
- Bulk file of 10,000 rows: every invalid row reported before approval; the approved batch runs with a result per row.
- Credentials never appear in logs, API responses or error messages: a test scans them.
- Rotating credentials doesn't interrupt payouts in flight.
- A failed airtime purchase releases its value hold.
- Reconciliation opens an exception for each kind of mismatch.

**Exit gate**

- [ ] A staging client completes a collection and a bulk payout with approval against the Daraja sandbox, and both reconcile.

**Kickoff prompt**

> We're starting Phase 13 of docs/ROADMAP.md. First confirm OD-12 is decided. Read the Phase 13 section, system design §13 and data model §7. Plan tasks 13.1–13.10 with tests first, starting with 13.6 (never pay twice).

---

### Phase 14: Surveys and rewards

**Goal.** v3's surveys with incentives and its rewards run on v4.

**Read first.** Data model Part 4 (surveys); system design §6 (USSD) and §13 (airtime).

**Tasks**

- [ ] 14.1 `surveys` migrations: surveys, questions, options, responses, incentives, consent.
- [ ] 14.2 Survey delivery over SMS, USSD and (after Phase 15) WhatsApp; linear and branching questions.
- [ ] 14.3 Incentives paid as airtime or data through Phase 13.
- [ ] 14.4 Results and exports; survey summaries in ClickHouse.
- [ ] 14.5 v3 importers: survey apps, questionnaires, responses, rewards.

**Tests specific to this phase**

- Each respondent gets an incentive once, even if they answer twice.
- Branching logic table tests.
- Survey over USSD within the session timeout.

**Exit gate**

- [ ] A v3 survey imported into staging runs end to end and pays its incentive.

**Kickoff prompt**

> We're starting Phase 14 of docs/ROADMAP.md. Read the Phase 14 section and the surveys part of data model Part 4. Plan tasks 14.1–14.5 with tests first.

---

### Phase 15: WhatsApp, Telegram, Messenger and the OTP service (v3 parity)

**Goal.** The channels v3 already offers, rebuilt as v4 adapters behind the same orchestrator.

**Read first.** System design §5; requirements, additional features (WhatsApp, Verify).

**Tasks**

- [ ] 15.1 WhatsApp Cloud API adapter: embedded signup, templates and their approval status, the 24-hour window, media, and per-message pricing by category and country.
- [ ] 15.2 Telegram adapter; Messenger adapter.
- [ ] 15.3 Inbound messages for each channel into the inbox and `message.received`.
- [ ] 15.4 OTP service at v3 parity: send and check codes over SMS, with expiry and attempt limits.
- [ ] 15.5 v3 importers: WhatsApp accounts and templates, Telegram accounts and members.
- [ ] 15.6 Migration plan for clients on v3's WhatsApp provider.

**Tests specific to this phase**

- Meta webhook signatures verified; template status sync; sending outside the 24-hour window needs a template.
- Pricing category golden tests per country.
- OTP: codes expire; attempt limits; a code works once.

**Exit gate**

- [ ] Each channel sends and receives in staging; WhatsApp is billed correctly by category.

**Kickoff prompt**

> We're starting Phase 15 of docs/ROADMAP.md. Read the Phase 15 section and system design §5. Plan tasks 15.1–15.6 with tests first, one adapter at a time.

---

### Phase 16: Monitoring and alerting engine, operations readiness

**Goal.** Problems are spotted and routed to the right person before clients notice. **This phase ends at M2.**

**Read first.** System design §10; data model Part 4 (monitoring).

**Tasks**

- [ ] 16.1 `monitoring` migrations: rules, incidents, notifications.
- [ ] 16.2 Rule engine over metrics and events; the default rules in system design §10.
- [ ] 16.3 Incidents with severity, escalation and error reports.
- [ ] 16.4 Paging through an independent provider (OD-17), never through VASPRO's own SMS routes.
- [ ] 16.5 Client alerts: limits, low token balance, low payout balance.
- [ ] 16.6 Public status page; maintenance windows.
- [ ] 16.7 Automatic actions with limits (pause a route, lock an abused key, throttle a spike); a person approves the larger actions.
- [ ] 16.8 Runbooks for every P1 alert in `docs/runbooks/`.
- [ ] 16.9 Closed beta with 5–10 friendly v3 clients on v4.

**Tests specific to this phase**

- Rule evaluation table tests; flapping suppression; maintenance windows silence alerts.
- Chaos: with every SMS route down, a P1 page still reaches on-call through the independent path.
- Automatic actions stop at their limits and ask for approval above them.

**Exit gate (M2)**

- [ ] Every v3 feature in the parity list (requirements, "Carry forward") works for beta clients.
- [ ] Every P1 alert has a tested runbook.
- [ ] Beta clients report no blocking issues for two consecutive weeks.

**Kickoff prompt**

> We're starting Phase 16 of docs/ROADMAP.md. Read the Phase 16 section and system design §10. Plan tasks 16.1–16.9 with tests first, starting with independent paging.

---

### Phase 17: Hardening and launch readiness

**Goal.** v4 is secure, fast enough, recoverable and compliant before all of Kenya moves to it.

**Tasks**

- [ ] 17.1 Security review against OWASP ASVS level 2; external penetration test; fix every high and critical finding.
- [ ] 17.2 Load tests at 3× v3's peak traffic for SMS, USSD, top-ups and payouts; tune.
- [ ] 17.3 Backup and restore drill for PostgreSQL and ClickHouse; recovery time and recovery point measured.
- [ ] 17.4 Failure drills: lose a database node, the bus, Redis, RabbitMQ, a rail.
- [ ] 17.5 Compliance: ODPC registration and data catalogue; retention rules; KRA eTIMS live; CA rules for sender IDs and promotional hours.
- [ ] 17.6 Production environment (OD-02); secrets in the KMS; access reviews.
- [ ] 17.7 Support playbooks; client-facing documentation complete.

**Exit gate**

- [ ] No open high or critical security findings.
- [ ] Load targets met with headroom.
- [ ] Restore drill meets the agreed recovery targets.
- [ ] Compliance checklist signed off.

**Kickoff prompt**

> We're starting Phase 17 of docs/ROADMAP.md. Read the Phase 17 section. Produce the hardening plan as a checklist with owners and evidence needed for each item, then start with the load test scripts in test/load/.

---

### Phase 18: v3 migration and cutover

**Goal.** Every Kenyan client moves to v4 in planned waves, with a way back, and v3 is switched off. **This phase ends at M3.** Section 13 has the detailed plan.

**Tasks**

- [ ] 18.1 Final importer runs on a fresh anonymised copy; reconciliation of every count and balance.
- [ ] 18.2 Compatibility routes for old v3 API tokens and endpoints, if OD-16 says yes.
- [ ] 18.3 Wave plan: internal accounts, then beta clients, then small clients, then large clients.
- [ ] 18.4 For each wave: freeze, import, verify balances, switch traffic, watch, confirm.
- [ ] 18.5 Rollback procedure tested in staging.
- [ ] 18.6 Switch v3 off; archive its data under the retention rules.

**Exit gate (M3)**

- [ ] Every client migrated and verified; v3 receives no traffic for 30 days; v3 is archived.

**Kickoff prompt**

> We're starting Phase 18 of docs/ROADMAP.md. Read sections 13 and the Phase 18 section, and docs/runbooks/cutover.md. Produce the wave plan with the checks for each step. Don't run anything against v3 production; the runbook is executed by people.

---

### Phase 19: Growth I

**Goal.** New capabilities beyond v3: stronger verification, fraud controls, lookup, email, RCS, failover and analytics.

**Tasks**

- [ ] 19.1 Verify: WhatsApp and email delivery, TOTP fallback, SIM-swap check through network APIs, pay per successful verification.
- [ ] 19.2 Fraud controls: SMS pumping detection, velocity limits, country permissions per account, phishing-link filtering.
- [ ] 19.3 Number lookup: validity, operator, ported status.
- [ ] 19.4 Email channel with SPF, DKIM and DMARC set-up.
- [ ] 19.5 RCS as a pluggable channel, switched off in Kenya.
- [ ] 19.6 Channel failover: WhatsApp first, SMS if not delivered in time.
- [ ] 19.7 Analytics dashboards and branded short links with click tracking.

**Tests**: OTP brute-force limits; pumping rules against replayed attack traffic; failover sends exactly one fallback.

**Exit gate**: each feature live for at least one client, with its monitoring rules.

**Kickoff prompt**

> We're starting Phase 19 of docs/ROADMAP.md. Read the Phase 19 section and the requirements' additional features table. Plan one feature at a time with its design note first, since these features have less design detail than the parity phases.

---

### Phase 20: Growth II

**Goal.** Journeys, AI agents, a unified inbox, AI-assisted support and the integrations ecosystem.

**Tasks**

- [ ] 20.1 Journey builder: event triggers, waits, branches, multiple channels.
- [ ] 20.2 AI agents on WhatsApp and USSD, grounded on the client's content, with hand-off to a person.
- [ ] 20.3 Unified inbox with assignment, SLAs and canned replies.
- [ ] 20.4 AI-assisted customer support and product analytics (PostHog): in-app help grounded on docs and account state, support ticket triage.
- [ ] 20.5 MCP server for clients' AI agents.
- [ ] 20.6 Integrations: Shopify, WooCommerce, HubSpot, Salesforce, Zoho, Google Sheets, Zapier.
- [ ] 20.7 White-label reseller portal.
- [ ] 20.8 Enterprise SSO (SAML and OIDC), if not done in Phase 1. Phase 1 deferred it (1.15); start from OQ-03 in `docs/decisions/open-questions.md`.

**Exit gate**: each feature has a design note, tests and monitoring before release.

**Kickoff prompt**

> We're starting Phase 20 of docs/ROADMAP.md. Pick the next item with me, then write its design note in docs/design/ before any code.

---

### Phase 21: Tanzania and Uganda

**Goal.** Launch each country from its pack, with no changes to the core.

**Tasks**

- [ ] 21.1 TZ pack: numbering, operators, TZS, VAT, sender ID rules, send windows, routes (confirm with the engine owners whether the TZ RabbitMQ path in system design §5.2 is used), rails (M-Pesa TZ and others).
- [ ] 21.2 UG pack: as above with UGX (zero decimals); no inbound A2P SMS, so two-way runs on USSD or WhatsApp.
- [ ] 21.3 Local legal entity or partner, tax registration, invoicing rules.
- [ ] 21.4 Data residency per tenant (OD-02).
- [ ] 21.5 Sandbox test numbers and simulators for each country.

**Tests**: the full e2e suite runs per country pack; any test that assumes Kenya is a bug.

**Exit gate (M4, M5)**: first live clients in each country, billed and reconciled correctly.

**Kickoff prompt**

> We're starting Phase 21 of docs/ROADMAP.md for <country>. Read the Phase 21 section and packs/ke/ as the reference. Draft packs/<cc>/ and list every value that needs confirmation from local counsel or partners.

---

### Phase 22: Continuous operations

- Weekly dependency updates in their own pull requests; monthly `govulncheck` review.
- Quarterly restore drill and credential rotation.
- Error-rate and cost reviews per route and rail.
- Keep the docs, ADRs and this roadmap current; add a new phase section for each major feature.

---

## 12. Parallel tracks

### 12.1 Frontend

Built alongside the backend phases, against the generated TypeScript API client.

| Track | With phase | Delivers |
|---|---|---|
| F0 | 0–1 | App shell for `web/dashboard`, `web/console` and `web/portal`; design system; auth screens; i18n set-up (English first, then Swahili and French) |
| F1 | 1–2 | Sign-up, sign-in with passkey or TOTP, onboarding service picker, KYC steps, team management |
| F2 | 3–4 | Wallet, token purchase, top-up, statements |
| F3 | 6–7 | Contacts, imports, campaigns, send, message timeline, limits |
| F4 | 9–11 | Invoices and Pay now; applications wizard; USSD builder and simulator; staff console |
| F5 | 12–15 | Inbox, keywords, subscriptions, payouts and approvals, surveys, WhatsApp set-up |
| F6 | 16+ | Alerts and status; analytics |

Frontend tests: Vitest for components; Playwright for the key journeys (sign up, top up, send a campaign, approve a payout).

### 12.2 Security and compliance

- Threat model for each service before its phase starts: a short STRIDE note in `docs/design/`.
- Personal data catalogue kept in step with the migrations.
- Access to production is logged; staff actions are audited.

### 12.3 Observability

- Every phase adds its dashboards and alert rules; Phase 16 turns them into the full engine.
- One `request_id` traced from gateway to engine hand-off and back to the webhook.

### 12.4 Documentation

- Public API docs grow with each phase (Phase 8 publishes them).
- Every P1 alert gets a runbook.

### 12.5 Migration preparation

- Each phase that replaces v3 data writes its importer and dry-runs it on an anonymised v3 copy. The phase lists the importer as a task.
- Discrepancies go to `docs/runbooks/migration-findings.md` as they are found, not at the end.

---

## 13. v3 migration and cutover

**Principles**

- Migrate by client, in waves, not all at once.
- Every wave is reversible until it is confirmed.
- Money is checked to the cent before and after each client moves.
- Clients keep their sender IDs, shortcodes, USSD codes and paybills; only the platform behind them changes.

**Per-client steps**

1. **Prepare.** Import users, account, contacts, assets, subscriptions and settings ahead of time. Ask the client to create v4 API credentials and re-enter rail credentials (v3's are not migrated).
2. **Freeze.** Pause the client's scheduled campaigns and payouts on v3.
3. **Move money.** Post the closing v3 balance as the v4 opening balance, then compare.
4. **Switch.** Point the client's traffic to v4: API base URL or the compatibility route, callback URLs, USSD and shortcode routing, C2B confirmation URLs.
5. **Watch.** Monitor for 72 hours with the client's dashboard and alerts.
6. **Confirm** with the client, or roll back by switching routing back to v3 and re-posting the balance there.

**Waves**

| Wave | Who | Gate to the next wave |
|---|---|---|
| 0 | VASPRO's own accounts | One week clean |
| 1 | Beta clients from M2 | Two weeks clean |
| 2 | Small clients by volume | No P1 incidents for a week |
| 3 | Large and payment-heavy clients, one by one | Each confirmed individually |

**Switch-off**: when v3 has had no traffic for 30 days, archive its database under the retention rules and decommission it.

---

## 14. Risk register

| Risk | Effect | Mitigation |
|---|---|---|
| SMS engine contract misunderstood | Messages lost or wrongly charged | Golden samples from v3; contract tests; staging test with the real engine; shadow mode before M1 |
| Engine questions (OD-10, OD-11) unanswered | Phases 7 and 12 blocked | Ask in Stage A; escalate early |
| Ledger defects | Money errors | Property tests, database guards, nightly rebuild, finance sign-off |
| Double payouts | Client money lost | Idempotency, status query before retry, maker-checker |
| Credentials exposure | Fraud on client accounts | Envelope encryption, no logging, rotation, v3 credentials not migrated |
| Scope creep from growth features | Parity delayed | Growth features only after M3 (D-19) |
| Operator or regulator changes | Rework | Country packs as data; adapters per operator |
| Single developer bottleneck | Delays | Small tasks, full test suite, docs current, `STATUS.md` always up to date |
| Migration surprises | Cutover delays | Importers built per phase with dry runs; findings logged early |
| AI-written code that looks right but isn't | Hidden bugs | Tests first, fresh-session `/review`, human review of money and security code |

---

## 15. Appendix: bootstrap files

Create these exactly as shown. After the first commit, the files in the repository are the source of truth; update them there, not here.

### 15.1 `CLAUDE.md`

~~~markdown
# VASPRO v4

VASPRO v4 is a multi-country communications and payments platform (SMS, USSD, WhatsApp, payments, surveys) for Kenya first, then Tanzania and Uganda. It replaces v3 (PHP/Phalcon). Backend in Go; one repo, one Go module, one package per service under internal/.

@docs/STATUS.md

## How to work here
- Read docs/STATUS.md (above) first. Work only on the current phase and task.
- Before a phase or task, read its section in docs/ROADMAP.md and the design sections it lists. Do not read the whole roadmap.
- Design sources, highest authority first: docs/decisions/ (ADRs), docs/design/data-model.md, docs/design/system-design.md, docs/design/requirements.md.
- Contracts first (OpenAPI in docs/api/, migrations, sqlc queries, proto, events in docs/events/, error codes in docs/errors/catalogue.yaml), then failing tests, then code.
- A task is done only when `make check` is green and docs/STATUS.md is updated.
- One task per conversation. Use plan mode to agree the plan before changing files.

## Stop and ask when
- The design is silent or two sources disagree. Write the question in docs/decisions/open-questions.md.
- Money, tax, credentials, personal data or the SMS engine contract are involved and the design doesn't spell it out.
- A test can only pass by weakening or skipping it.
- A migration would rewrite or drop data, or edit a migration already on main.
- An open decision in section 4.3 of docs/ROADMAP.md blocks the task.

## Never
- Put secrets, tokens, real phone numbers or customer data in code, tests, fixtures or logs.
- Connect to v3 production, the v3 database or the production SMS engine.
- Import one service's package from another (internal/<a> must not import internal/<b>); share code only through internal/platform.
- Read another service's database schema.
- Use floats for money. Money is int64 minor units plus a currency code.

## Commands
- `make up` / `make down` / `make reset`: local stack
- `make gen`: sqlc, oapi-codegen, buf, error codes
- `make test`, `make test-int`, `make test-contract`, `make test-e2e`
- `make check`: everything a task needs to be done
- `make run` or `make run SVC=<service>`
- `make new-service NAME=<service>`

## Architecture in brief
- 14 services: identity, accounts, ledger, payments, billing, messaging, ussd, contacts, applications, verify, journeys, surveys, webhooks, monitoring; plus the gateway.
- Services talk through gRPC (proto/) and the event bus (NATS JetStream). Events go out through the transactional outbox; consumers use processed_events.
- PostgreSQL 16, one schema per service, row-level security on tenant_id via `SET LOCAL app.tenant_id`.
- Errors are RFC 9457 problem+json with VASPRO codes (AUTH, KYC, VAL, BAL, CMP, RTE, DLR, RATE, SYS, LIM, PAY).
- Country rules live in packs/<cc>/, never in code.
- SMS sending is done by the existing Go SMS engine, unchanged. v4 picks the route from the pack and hands off exactly as v3 does (docs/design/system-design.md §5.2).
- VASPRO never holds client money: collections go to the client's own paybill; payouts leave from the client's own shortcode.

## Service layout
internal/<svc>/{<svc>.go, api/, grpc/, app/, domain/, store/, events/, adapters/, testdata/}
domain/ is pure Go and has no I/O. app/ holds use cases. store/ is sqlc. adapters/ sit behind interfaces with fakes in tools/fakes/.

## Git
- Branch per task: p<phase>/<short-name>. Conventional commits. Never push to main directly.
~~~

### 15.2 `docs/STATUS.md`

~~~markdown
# Build status

**Updated:** YYYY-MM-DD
**Current phase:** Phase 0: Foundations and platform
**Current task:** 0.1 Repository skeleton
**Branch:** p0/skeleton

## Done in the current phase
- (none yet)

## Next up
1. 0.1 Repository skeleton
2. 0.2 Local stack

## Blockers
- (none)

## Decisions pending (see docs/ROADMAP.md §4.3)
- OD-01 Event bus: default NATS JetStream unless decided otherwise

## Notes for next session
- (Claude writes short notes here at the end of each task)
~~~

### 15.3 `.claude/settings.json`

~~~json
{
  "permissions": {
    "allow": [
      "Bash(make *)",
      "Bash(go test *)",
      "Bash(go build *)",
      "Bash(go vet *)",
      "Bash(go mod tidy)",
      "Bash(golangci-lint run *)",
      "Bash(sqlc *)",
      "Bash(goose *)",
      "Bash(buf *)",
      "Bash(docker compose *)",
      "Bash(git status)",
      "Bash(git diff *)",
      "Bash(git log *)",
      "Bash(pnpm *)"
    ],
    "deny": [
      "Read(./.env)",
      "Read(./.env.*)",
      "Read(./secrets/**)",
      "Bash(git push *)"
    ]
  }
}
~~~

Pushing stays with you. Adjust the lists as you learn what Claude needs to run.

### 15.4 `.claude/rules/go.md`

~~~markdown
---
paths:
  - "**/*.go"
---
# Go rules
- Errors: wrap with %w and context; return platform/problem errors with a catalogue code at API boundaries. Never panic in request paths.
- context.Context is the first parameter of anything that does I/O.
- Money: platform/money types only. Tokens: shopspring/decimal. No float64 for either.
- Time: platform/clock only; never time.Now() directly.
- IDs: platform/ids (UUIDv7). Public IDs use the service's prefix.
- Logging: slog with request_id, tenant_id and trace_id. Never log secrets, tokens, full phone numbers or message bodies.
- Database: every client-owned query runs inside db.WithTenantTx. No string-built SQL; sqlc only.
- Events: publish only through platform/outbox in the same transaction as the change.
- Adapters live behind interfaces; tests use the fakes in tools/fakes.
- Keep functions small; domain/ has no imports of store/, adapters/ or net/http.
~~~

### 15.5 `.claude/rules/migrations.md`

~~~markdown
---
paths:
  - "migrations/**"
---
# Migration rules
- One folder per schema. File names: goose timestamps with a short description.
- Never edit a migration that is on main. Add a new one.
- Follow docs/design/data-model.md exactly: names, types, constraints, indexes. If you must differ, stop and ask.
- Every client-owned table: tenant_id uuid NOT NULL, ENABLE ROW LEVEL SECURITY, and the tenant_isolation policy.
- Money columns are bigint *_minor plus a currency. Timestamps are timestamptz.
- Every migration has a Down section unless it is irreversible; say so in a comment if it is.
- Add a repository test for every constraint, trigger and policy you create.
~~~

### 15.6 `.claude/rules/tests.md`

~~~markdown
---
paths:
  - "**/*_test.go"
  - "test/**"
---
# Test rules
- Write the test before the code and show it failing for the right reason.
- Table-driven tests for rules and state machines; name each case after the behaviour.
- Use platform/testkit for PostgreSQL, NATS, Redis and RabbitMQ (testcontainers). No shared state between tests.
- Use the reserved test numbers from packs/<cc>/sandbox.yaml. Never real numbers.
- Control time with the fake clock. No sleeps to wait for async work; poll with a timeout helper.
- Golden files live in testdata/; update them only on purpose (`-update` flag) and review the diff.
- Never skip or weaken a test to get green. If a test is wrong, say why and fix the test in its own commit.
- Integration tests use the build tag `integration`; end-to-end tests use `e2e`.
~~~

### 15.7 `.claude/rules/api.md`

~~~markdown
---
paths:
  - "docs/api/**"
  - "internal/*/api/**"
---
# API rules
- OpenAPI 3.1 in docs/api/ is the source of truth; regenerate code with make gen, never edit generated files.
- Every write operation requires an Idempotency-Key header.
- Errors are application/problem+json with a code from docs/errors/catalogue.yaml.
- Every operation has a realistic example using reserved test numbers; examples run in e2e.
- Paths are versioned (/v1/...); breaking changes need a new version and an ADR.
- Pagination is cursor-based; amounts are minor units with a currency.
~~~

### 15.8 `.claude/rules/frontend.md`

~~~markdown
---
paths:
  - "web/**"
---
# Frontend rules
- Vue 3 + TypeScript + Vite; Composition API with <script setup>; Pinia for state.
- Call the backend only through the generated API client; never hand-write request types.
- Format money from minor units with the currency's exponent; never do money maths in the browser beyond display.
- All user-facing text goes through i18n.
- Components get Vitest tests; key journeys get Playwright tests.
- Accessibility: labels on every input, keyboard navigation, sufficient contrast.
~~~

### 15.9 `.claude/skills/start-task/SKILL.md`

~~~markdown
---
name: start-task
description: Start the current build task from docs/STATUS.md with a plan
disable-model-invocation: true
---
1. Read docs/STATUS.md. Identify the current phase and task. If $ARGUMENTS names a task, use that instead.
2. Read that phase's section in docs/ROADMAP.md, then only the design sections it lists under "Read first" that relate to this task.
3. Check whether any open decision (docs/ROADMAP.md §4.3) or open question blocks the task. If one does and has no default, stop and say so.
4. Produce a plan with:
   - contracts to add or change (OpenAPI, migrations, sqlc, proto, events, error codes)
   - tests to write first, as a list of behaviours
   - files to create or change
   - risks and questions
5. Wait for approval before changing any file.
~~~

### 15.10 `.claude/skills/finish-task/SKILL.md`

~~~markdown
---
name: finish-task
description: Verify, record and wrap up the current build task
disable-model-invocation: true
---
1. Run `make check`. If anything fails, fix it and run again. Do not skip or weaken tests.
2. Go through the definition of done in docs/ROADMAP.md §8.6 for this task and state what is met and what isn't.
3. Update docs/STATUS.md: tick the task, set the next task, record blockers, and add up to five short notes for the next session.
4. Tick the task's checkbox in docs/ROADMAP.md if it is complete.
5. Propose a conventional commit message and a pull request description (what changed, tests added, design sections followed, ADRs).
~~~

### 15.11 `.claude/skills/review/SKILL.md`

~~~markdown
---
name: review
description: Fresh-eyes review of a change against the VASPRO v4 design and checklists
disable-model-invocation: true
---
Review the changes in $ARGUMENTS (a branch, commit range or files; default: the diff against main). You did not write this code; review it as a strict senior engineer.

Check, and report each finding with file, line, severity and a concrete fix:
1. Matches the design: data model names and constraints, system design flows, ADRs.
2. Money: minor units, no floats, balanced postings, idempotency, holds released on every failure path.
3. Security: authz on every endpoint, tenant isolation (RLS and tenant_id filters), no secrets or personal data in logs or errors, input validation.
4. Contracts: OpenAPI, events and error codes updated and consistent with the code; SMS engine and rail payloads match the golden files.
5. Tests: every rule and failure path covered; no skipped or weakened tests; no sleeps; deterministic time.
6. Boundaries: no cross-service imports or schema access; domain/ free of I/O.
7. Operability: logs, metrics, alerts; migrations reversible or marked.
End with a verdict: approve, or request changes with the must-fix list first.
~~~

### 15.12 `.claude/skills/start-phase/SKILL.md`

~~~markdown
---
name: start-phase
description: Kick off a new build phase from docs/ROADMAP.md
disable-model-invocation: true
---
1. Confirm the previous phase's exit gate in docs/ROADMAP.md is fully ticked, with evidence. If not, list what is missing and stop.
2. Read the section for phase $ARGUMENTS in docs/ROADMAP.md and the design sections under "Read first".
3. Check every "Blocked until" condition and every open decision the phase needs. Report any that are unresolved.
4. Check the phase's contracts exist (OpenAPI for its services, data model sections, event schemas). If any is missing, propose drafting it first.
5. Break the phase into tasks in order, each with its tests, and write them into docs/STATUS.md as the plan for the phase.
6. Wait for approval, then start the first task with /start-task.
~~~

### 15.13 `docs/decisions/0000-template.md`

~~~markdown
# NNNN. Title

**Date:** YYYY-MM-DD · **Status:** proposed | accepted | superseded by NNNN

## Context
What forces the decision. Link the design sections and open decision (OD-xx).

## Decision
What we decided, in one or two sentences, then details.

## Consequences
What becomes easier or harder; what has to change in code, docs or operations.
~~~

### 15.14 `docs/decisions/open-questions.md`

~~~markdown
# Open questions

Questions found during the build that the design doesn't answer. Each one gets an ID, the date, who raised it, the task it blocks, and the answer once known (then turn it into an ADR if it changes the design).

| ID | Date | Question | Blocks | Answer |
|---|---|---|---|---|
~~~
