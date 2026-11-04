# Dev environment

A shared environment where testers exercise the APIs and the frontend team builds against them. It runs what's built so far: **identity**, **accounts** and the **ledger** behind the **gateway**, with a fake KYC vendor, on a private Docker network. Only the gateway is reachable from outside.

It isn't staging (ROADMAP §5, OD-02): there are no real rails, no SMS engine and no real KYC vendor, and every secret is a development value.

Files:

| File | What it is |
|---|---|
| `deploy/compose/docker-compose.dev.yml` | The stack (project `vaspro-dev`) |
| `deploy/compose/env.example` | Every variable, with dev defaults and empty secrets |
| `deploy/compose/dev-secrets.sh` | Creates `deploy/compose/.env` with fresh secrets |
| `deploy/compose/db-setup.sh` | Roles, migrations and grants (the `db-setup` one-shot) |
| `deploy/docker/*.Dockerfile` | One image per binary, built from the repository root |

`deploy/compose/docker-compose.yml` is the separate local stack for running services on your machine with `make run`. It publishes every infrastructure port, and nothing here changes it.

## First time

```sh
deploy/compose/dev-secrets.sh   # writes deploy/compose/.env; refuses to overwrite one
# edit .env: GATEWAY_CORS_ALLOWED_ORIGINS, IDENTITY_WEBAUTHN_* (see below)
make dev-up                     # builds the images and waits until everything is healthy
```

The first `make dev-up` takes several minutes: the images build, and ClamAV loads its signatures (allow about 1 GB of memory for it). Later starts are quick.

Then check it: `curl http://<host>:8081/readyz` should print `ok`.

| Command | Does |
|---|---|
| `make dev-up` | Builds changed images, starts or updates the stack, waits for health |
| `make dev-down` | Stops it and keeps the data |
| `make dev-reset` | **Wipes all data** (database, Redis, NATS, documents, mail) and starts clean |
| `make dev-logs SVC=identity` | Follows one service's logs (omit `SVC` for all) |
| `make dev-kyc` / `make dev-kyc ID=… STATUS=…` | Lists the fake vendor's checks / decides one |
| `make dev-staff EMAIL=… BY=…` | Makes a user staff |

To deploy a new version, pull the branch and run `make dev-up`: changed images rebuild, `db-setup` applies any new migrations, and the changed services restart.

## Ports

Only the gateway is published, on `GATEWAY_PUBLISH` (default `8081`, all interfaces). Everything else is reachable only on the `vaspro-dev` network:

| Service | Address on the network | Purpose |
|---|---|---|
| gateway | `gateway:8081` | The API (`/v1/...`), `/healthz`, `/readyz` |
| identity | `identity:8080` HTTP, `identity:9090` gRPC | |
| accounts | `accounts:8082` HTTP, `accounts:9091` gRPC | |
| ledger | `ledger:8083` HTTP, `ledger:9092` gRPC | |
| kyc-vendor | `kyc-vendor:8090` | The fake KYC vendor |
| postgres | `postgres:5432` | One database; one schema per service |
| redis | `redis:6379` | DB 0 identity sessions, DB 1 gateway rate limits |
| nats | `nats:4222` | Event bus (JetStream) |
| object-storage | `object-storage:8333` | SeaweedFS S3, documents bucket |
| clamav | `clamav:3310` | Virus scanning of uploads |
| mailpit | `mailpit:1025` SMTP, `mailpit:8025` UI and API | Catches all email |

The gRPC ports are plaintext and trust the caller (mTLS is Phase 17), so never publish them.

Every service has `/healthz` (the process is up; use it as a liveness probe) and `/readyz` (its dependencies are reachable). Compose's health checks use `/readyz`.

## Start order

Compose enforces it with `depends_on`:

1. postgres, redis, nats, object-storage, clamav, mailpit become healthy;
2. `db-setup` runs once and must exit 0;
3. identity, accounts and ledger start; each waits for its own infrastructure and reports ready;
4. the gateway starts once all three are ready, and the kyc-vendor starts alongside it.

Identity and accounts dial each other's gRPC lazily, so neither waits for the other.

## Database

`db-setup` runs as the PostgreSQL superuser and can be run again safely. For each of `identity`, `accounts` and `ledger`, it creates:

- `<svc>_owner`: runs the goose migrations, so it owns the schema and its tables. It has `CREATEROLE`, because the migrations create their own narrow roles, and `BYPASSRLS`, because migrations seed rows no tenant may write (accounts' built-in roles). Nothing else connects as it.
- `<svc>_app`: the service's login role. It owns nothing, so row-level security and the ledger's grants bind it.

Both roles have `search_path = <svc>, public`, and the connection URLs also set it. Goose records each service's progress in `goose.<svc>_version`.

Grants made after the migrations:

- identity and accounts: DML on every table and sequence in their schema, plus default privileges, so new migrations' tables are covered.
- ledger: `ledger_app` is granted `ledger_service`. The ledger's migrations already give that role exactly what the service may do: no UPDATE or DELETE on the journal, and no writes to `balance_minor`.
- These narrow roles, `WITH INHERIT FALSE`, reached only through `SET ROLE` inside `WithRoleTx`:
  - for `identity_app`: `identity_token_exchange` and `identity_sso_domain_lookup`;
  - for `accounts_app`: `accounts_membership_lookup`, `accounts_kyc_webhook`, `accounts_registration_lookup` and `accounts_staff_review`.

  The ledger's `ledger_system`, `ledger_price_review` and `ledger_settlement` reach `ledger_app` through `ledger_service`.

To look at the data: `docker compose -f deploy/compose/docker-compose.dev.yml --env-file deploy/compose/.env exec postgres psql -U vaspro`. Below, `$DC` stands for that `docker compose -f … --env-file …` prefix.

## Variables

All of them are in `deploy/compose/env.example`, grouped by service with what each one does. They come from each service's `Config` struct (`internal/<svc>/<svc>.go`). The ones you're likely to change:

| Variable | Default | Notes |
|---|---|---|
| `GATEWAY_PUBLISH` | `8081` | `host:port` or a port. Use `127.0.0.1:8081` once a proxy (Caddy) sits in front |
| `GATEWAY_CORS_ALLOWED_ORIGINS` | `http://localhost:3000,http://localhost:5173` | See below |
| `IDENTITY_WEBAUTHN_RP_ID` | `localhost` | The dashboard's domain. Passkeys only work on it or its subdomains |
| `IDENTITY_WEBAUTHN_ORIGINS` | the two localhost origins | The dashboard's exact origins, comma-separated |
| `IDENTITY_GOOGLE_*` | empty (off) | Google sign-in needs a Google OAuth client, and a redirect URL registered with Google |
| `LEDGER_SETTLEMENT_WINDOW` | `5m` | How often usage settles |
| `KYCVENDOR_PUBLIC_URL` | `http://kyc-vendor:8090` | Base of the "hosted flow" link `createKycCheck` returns |

The database URLs aren't in `.env`: compose builds them from the `*_DB_PASSWORD` values. Keep those passwords URL-safe (`dev-secrets.sh` uses hex).

Secrets in `.env`, all generated by `dev-secrets.sh`:

- the six database passwords;
- the identity keys `IDENTITY_TOTP_KMS_KEY`, `IDENTITY_SIGNING_KEYS_KMS_KEY` and `IDENTITY_CHALLENGE_SIGNING_KEY`;
- `ACCOUNTS_DOCUMENTS_KMS_KEY`;
- `ACCOUNTS_KYC_WEBHOOK_SECRET`, which the fake vendor shares.

The keys are base64 of 32 random bytes. `.env` is gitignored: never commit it, and don't reuse its values anywhere else.

### CORS

`GATEWAY_CORS_ALLOWED_ORIGINS` lists the exact browser origins (scheme, host and port) allowed to call the API. There are no wildcards. Listed origins get `Access-Control-Allow-Origin` echoed back, and their preflights are answered with:

- methods GET, POST, PUT, PATCH, DELETE and OPTIONS;
- headers `Authorization`, `Content-Type`, `Idempotency-Key` and `X-Request-Id`;
- a 10-minute max age.

Any other origin gets no CORS headers, so the browser blocks it. When the variable is empty, the gateway adds no CORS headers at all, and an OPTIONS request is refused by request validation (400) as before.

Add the frontend team's dev-server origins, then run `make dev-up` to restart the gateway. The API is cookieless (sessions are bearer tokens), so there are no credentialed CORS requests.

## Testing the flows

**Email.** Sign-up, password set and reset, and email step-up send 6-digit codes. Mailpit catches every message. Until the Caddy step exposes its UI, read the messages through its API:

```sh
$DC exec -T mailpit wget -qO- 'http://127.0.0.1:8025/api/v1/search?query=to:someone@example.com' # list (newest first)
$DC exec -T mailpit wget -qO- 'http://127.0.0.1:8025/api/v1/message/<ID>'                       # one message
```

**Sign-up to an account:**

1. `POST /v1/auth/signups` → the code by email → `POST /v1/auth/email-verifications`.
2. `POST /v1/auth/password-resets` → code → `POST /v1/auth/password-reset-confirmations`.
3. `POST /v1/auth/sessions` returns a `session_token`; send it as `Authorization: Bearer …`.
4. `POST /v1/accounts/tenants`, then `PUT /v1/auth/sessions/current/tenant`.

Every write needs an `Idempotency-Key` header. The contracts are `docs/api/{identity,accounts,ledger}.yaml`.

**KYC.** After `POST /v1/accounts/tenants/{id}/kyc-checks`, the check is pending until the fake vendor decides it:

```sh
make dev-kyc                                   # list checks: reference (our check ID), provider_ref, status
make dev-kyc ID=<check id> STATUS=passed       # or review, failed, expired
```

The vendor posts a signed webhook to accounts through the gateway. A passed individual check moves a sandbox account to Starter. The fake keeps its checks in memory, so after it restarts, a check started earlier can't be decided: start a new one.

**Staff.** Staff are granted by an operator, never through the API:

```sh
make dev-staff EMAIL=<user's email> BY=<your name>
$DC run --rm vaspro identity revoke-staff -by <your name> <email>
```

Staff can then use `/v1/accounts/staff/...` (the KYB review queue, tier changes, resellers) and `/v1/ledger/staff/token-prices`. Approving a token price needs a second staff member (maker-checker) and a fresh step-up.

**Documents** (KYB) are scanned by ClamAV, encrypted, and stored in SeaweedFS. Uploads are capped at 10 MB, as PDF, JPEG or PNG.

## Rotating keys and passwords

- **JWT signing key:** `$DC run --rm vaspro identity rotate-signing-key`. The new key signs from then on. The old one stays in `/.well-known/jwks.json` for its 24-hour overlap, so issued tokens keep working. Nothing restarts.
- **Database passwords:** change the value in `.env`, then run `make dev-up`. `db-setup` sets the new password and the services restart with the new URL.
- **`IDENTITY_CHALLENGE_SIGNING_KEY`:** change it and run `make dev-up`. Only in-flight sign-in challenges (5 minutes) are lost.
- **`ACCOUNTS_KYC_WEBHOOK_SECRET`:** change it and run `make dev-up`. The fake vendor picks it up too.
- **LocalKMS keys** (`IDENTITY_TOTP_KMS_KEY`, `IDENTITY_SIGNING_KEYS_KMS_KEY`, `ACCOUNTS_DOCUMENTS_KMS_KEY`): data sealed under the old key becomes unreadable (TOTP secrets, signing keys, documents), and there's no re-encryption tool. In this environment, rotating one means `make dev-reset`.

## Known limits

- **Caller IP.** The gateway rate-limits (1,200 requests a minute per IP) and checks API clients' IP allowlists by the connection's address. Once a reverse proxy sits in front, every caller shares the proxy's IP. Trusting `X-Forwarded-For` from the proxy would be a gateway change, decided with the Caddy step.
- **Plain HTTP.** There's no TLS until the Caddy step.
- **Unreachable fake-vendor link.** The KYC "hosted flow" URL points into the private network, and the fake serves no page there. Testers decide checks with `make dev-kyc`.
- **Unused infrastructure.** RabbitMQ, ClickHouse and Grafana aren't in this stack, because nothing built uses them yet.
- **UGX wallets don't settle.** Only Kenya has a pack, so the ledger logs a settlement failure for such wallets each window (see STATUS 3.5).
