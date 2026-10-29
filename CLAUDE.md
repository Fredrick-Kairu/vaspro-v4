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

## GitHub Pull Request Policy

When using the GitHub MCP server:

1. Inspect the pull request description, changed files, reviews, unresolved review comments, and CI status before recommending a merge.
2. Verify the source branch, base branch, repository owner, and repository name.
3. Never bypass branch protection, required reviews, or required CI checks.
4. Before merging, provide a concise summary of the PR, your findings, outstanding risks, and the proposed merge method.
5. Ask me explicitly for confirmation before every merge. Identify the repository, PR number, title, base branch, and merge method in the confirmation request.
6. Do not execute the merge until I explicitly confirm that specific merge in the current interaction.
7. A previous confirmation does not authorize any subsequent merge.
8. If the PR has failed checks, unresolved blocking feedback, missing required approval, or an unexpected base branch, stop and explain the issue.
9. Never merge directly into a production branch unless I explicitly authorize that target for the specific PR and all repository protections are satisfied.
10. After a merge, report the outcome and resulting commit SHA if available.
