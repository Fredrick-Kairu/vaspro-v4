# Dev environment

A shared environment where testers exercise the APIs and the frontend team builds against them. It runs what's built so far: **identity**, **accounts** and the **ledger** behind the **gateway**, with a fake KYC vendor, on a private Docker network.

Only two ports are published, both on the host's loopback:

- the gateway on `127.0.0.1:18080`;
- Mailpit's web UI on `127.0.0.1:18025`.

The host's own nginx serves them as `https://<API_DOMAIN>` and `https://<MAIL_DOMAIN>`, with Let's Encrypt certificates from certbot.

**The host's nginx is shared with another project.** Treat it with care:

- add one site file and one htpasswd file, both named `vaspro`;
- always run `nginx -t` before `systemctl reload nginx`;
- **never restart nginx**, which would drop the other project's connections;
- never edit the other project's files.

This isn't staging (ROADMAP §5, OD-02): there are no real rails, no SMS engine and no real KYC vendor, and every secret is a development value.

Files:

| File | What it is |
|---|---|
| `deploy/compose/docker-compose.dev.yml` | The stack (project `vaspro-dev`) |
| `deploy/compose/env.example` | Every variable, with dev defaults and empty secrets |
| `deploy/compose/dev-secrets.sh` | Creates `deploy/compose/.env` with fresh secrets |
| `deploy/compose/db-setup.sh` | Roles, migrations and grants (the `db-setup` one-shot) |
| `deploy/docker/*.Dockerfile` | One image per binary, built from the repository root |
| `deploy/nginx/vaspro-dev.conf.template` | The two nginx server blocks |
| `deploy/nginx/render.sh` | Fills the template with the two domains and the ports |

`deploy/compose/docker-compose.yml` is the separate local stack for running services on your machine with `make run`. Nothing here changes it.

## First-time set-up on the dev host

You need Docker with the Compose plugin, `openssl`, `htpasswd` (`apache2-utils`), certbot with its nginx plugin, and sudo for nginx. The examples use `api.dev.example.com` and `mail.dev.example.com`.

**1. DNS.** Point both names at the host. Ports 80 and 443 are already open for the other project.

**2. Secrets and settings.** From the repository:

```sh
deploy/compose/dev-secrets.sh   # writes deploy/compose/.env; refuses to overwrite one
```

Then edit `deploy/compose/.env`:

- `GATEWAY_CORS_ALLOWED_ORIGINS`: the frontend team's dev-server origins (see "CORS").
- `IDENTITY_WEBAUTHN_RP_ID` and `IDENTITY_WEBAUTHN_ORIGINS`: the dashboard's domain and its exact origins.
- `GATEWAY_HOST_PORT` / `MAILPIT_HOST_PORT`: only if 18080 or 18025 is taken on the host.
- `DEV_SUBNET`: only if `10.213.0.0/24` clashes with a network on the host. Then also set `GATEWAY_TRUSTED_PROXIES` to the new subnet's first address (see "Client addresses").

**3. Start the stack:**

```sh
make dev-up      # builds the images and waits until everything is healthy
curl http://127.0.0.1:18080/readyz   # ready
```

The first `make dev-up` takes several minutes: the images build, and ClamAV loads its signatures (allow about 1 GB of memory for it). Later starts are quick. The site can't serve anything until this is up, so start it before nginx.

**4. The site file.** Render it with the two names; set the ports first if you changed them in `.env`. Then install and enable it the way this host's nginx already does it:

```sh
deploy/nginx/render.sh api.dev.example.com mail.dev.example.com > /tmp/vaspro-dev.conf
sudo install -m 644 /tmp/vaspro-dev.conf /etc/nginx/sites-available/vaspro-dev.conf
sudo ln -s /etc/nginx/sites-available/vaspro-dev.conf /etc/nginx/sites-enabled/vaspro-dev.conf
```

If the host's nginx uses `conf.d/` instead, copy the file to `/etc/nginx/conf.d/vaspro-dev.conf`. Check `ls /etc/nginx/sites-enabled /etc/nginx/conf.d`.

**5. Mailpit's password.** It lives in its own htpasswd file:

```sh
sudo htpasswd -c /etc/nginx/.htpasswd-vaspro-mail tester    # prompts for the password
sudo chown root:www-data /etc/nginx/.htpasswd-vaspro-mail     # nginx's worker group: www-data on Debian/Ubuntu, nginx on RHEL
sudo chmod 640 /etc/nginx/.htpasswd-vaspro-mail
```

- `-c` creates the file. Use it only the first time, because it overwrites.
- Without `htpasswd`, write the line yourself: `printf 'tester:%s\n' "$(openssl passwd -apr1)"`.
- Share the password with the testers. Nothing in the repository stores it.

**6. Check, then reload. Never restart.**

```sh
sudo nginx -t && sudo systemctl reload nginx
```

If `nginx -t` fails, fix or remove `vaspro-dev.conf` before anything else. Nginx keeps serving the other project with its old configuration until a reload succeeds.

**7. Certificates:**

```sh
sudo certbot --nginx -d api.dev.example.com -d mail.dev.example.com
```

Certbot adds the 443 listeners, the certificate and the HTTP-to-HTTPS redirect to `vaspro-dev.conf`, then reloads nginx itself. Its renewal timer covers these names along with the other project's.

**8. Check from outside:**

```sh
curl https://api.dev.example.com/readyz                       # ready
curl -u tester https://mail.dev.example.com/api/v1/messages   # Mailpit, after the password
```

### Changing the site file later

Re-rendering replaces certbot's 443 blocks. After installing a new render, run `nginx -t`, then `sudo certbot --nginx -d … -d …` again. Certbot reuses the existing certificate and reinstalls it, then reloads. Small changes can also be made by hand in the installed file, followed by `nginx -t` and a reload.

### Removing it

1. Delete the symlink and the site file, then the htpasswd file.
2. Run `sudo nginx -t && sudo systemctl reload nginx`.
3. Run `sudo certbot delete --cert-name api.dev.example.com`.
4. Run `make dev-down` (`$DC down -v` also deletes the data).

## Everyday commands

| Command | Does |
|---|---|
| `make dev-up` | Builds changed images, starts or updates the stack, waits for health |
| `make dev-down` | Stops it and keeps the data |
| `make dev-reset` | **Wipes all data** (database, Redis, NATS, documents, mail) and starts clean |
| `make dev-logs SVC=identity` | Follows one service's logs (omit `SVC` for all) |
| `make dev-kyc` / `make dev-kyc ID=… STATUS=…` | Lists the fake vendor's checks / decides one |
| `make dev-staff EMAIL=… BY=…` | Makes a user staff |

To deploy a new version, pull the branch and run `make dev-up`: changed images rebuild, `db-setup` applies any new migrations, and the changed services restart. Nginx needs nothing. It returns 502 for the few seconds the gateway is restarting.

Below, `$DC` stands for `docker compose -f deploy/compose/docker-compose.dev.yml --env-file deploy/compose/.env`.

## Ports

| Published (loopback only) | Service | Served by nginx as |
|---|---|---|
| `127.0.0.1:${GATEWAY_HOST_PORT}` (18080) | gateway | `https://<API_DOMAIN>`, with an 11 MB body limit |
| `127.0.0.1:${MAILPIT_HOST_PORT}` (18025) | Mailpit web UI and API | `https://<MAIL_DOMAIN>`, with basic auth |

Nothing else is published. Everything else is reachable only on the `vaspro-dev` network (`10.213.0.0/24`, `DEV_SUBNET`):

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

## Client addresses

The gateway rate-limits (1,200 requests a minute per IP), checks API clients' IP allowlists and logs requests by the caller's address. Behind nginx, the connection comes from Docker, not the client:

- **What the gateway sees:** a request through a published loopback port arrives from the network's first address, **`10.213.0.1`**. On a Linux host, Docker's port proxy, or its NAT when the proxy is off, forwards it from the bridge's address. Docker never gives that address to a container. This was confirmed on Docker Desktop's Linux VM (5 Nov), both from the Mac and from the VM's own network namespace: every request through `127.0.0.1:18080` logged `ip=10.213.0.1`. Check it once on the dev host, as described at the end of this section.
- **What it trusts:** `GATEWAY_TRUSTED_PROXIES=10.213.0.1`. Only from that address does the gateway believe `X-Forwarded-For`, and it reads the header from the right, skipping trusted hops.
- **What nginx sends:** `X-Forwarded-For $remote_addr`. That replaces any header the client sent, so a client can't choose its own address.
- **From inside the network,** containers such as the fake vendor connect directly from their own addresses. Any `X-Forwarded-For` they send is ignored.
- **The one gap:** anything running on the host can reach `127.0.0.1:18080` and set `X-Forwarded-For`, because it arrives from the trusted address too. The loopback binding keeps this to the host itself.
- **If `DEV_SUBNET` changes,** set `GATEWAY_TRUSTED_PROXIES` to the new first address. To check, make a request through `127.0.0.1:${GATEWAY_HOST_PORT}`, then run `make dev-logs SVC=gateway` and read the `ip=` of a request with no `X-Forwarded-For`.

Identity still records the gateway's own address for sign-ins and new-IP login alerts. It never reads the forwarded address.

## Start order

Compose enforces it with `depends_on`:

1. postgres, redis, nats, object-storage, clamav, mailpit become healthy;
2. `db-setup` runs once and must exit 0;
3. identity, accounts and ledger start; each waits for its own infrastructure and reports ready;
4. the gateway starts once all three are ready, and the kyc-vendor starts alongside it.

Identity and accounts dial each other's gRPC lazily, so neither waits for the other. The host's nginx is independent and only needs the two loopback ports.

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

  The ledger's `ledger_system`, `ledger_price_review`, `ledger_settlement`, `ledger_reversal_review` and `ledger_audit` reach `ledger_app` through `ledger_service`.

To look at the data: `$DC exec postgres psql -U vaspro`.

## Variables

All of them are in `deploy/compose/env.example`, grouped by service with what each one does. They come from each service's `Config` struct (`internal/<svc>/<svc>.go`). The ones you're likely to change:

| Variable | Default | Notes |
|---|---|---|
| `GATEWAY_HOST_PORT` / `MAILPIT_HOST_PORT` | `18080` / `18025` | The loopback ports. Render the nginx site with the same values |
| `DEV_SUBNET` | `10.213.0.0/24` | The private network |
| `GATEWAY_TRUSTED_PROXIES` | `10.213.0.1` | The first address of `DEV_SUBNET` (see "Client addresses") |
| `GATEWAY_CORS_ALLOWED_ORIGINS` | `http://localhost:3000,http://localhost:5173` | See below |
| `IDENTITY_WEBAUTHN_RP_ID` | `localhost` | The dashboard's domain. Passkeys only work on it or its subdomains |
| `IDENTITY_WEBAUTHN_ORIGINS` | the two localhost origins | The dashboard's exact origins, comma-separated |
| `IDENTITY_GOOGLE_*` | empty (off) | Google sign-in needs a Google OAuth client, and a redirect URL registered with Google |
| `LEDGER_SETTLEMENT_WINDOW` | `5m` | How often usage settles |
| `LEDGER_REBUILD_AT` | `21:30` | The nightly rebuild check, HH:MM UTC |
| `LEDGER_DEV_TOPUPS` | `true` | Staff-only sandbox top-ups (`/v1/ledger/staff/dev-top-ups`). Only ever true here |
| `KYCVENDOR_PUBLIC_URL` | `http://kyc-vendor:8090` | Base of the "hosted flow" link `createKycCheck` returns |

The database URLs aren't in `.env`: compose builds them from the `*_DB_PASSWORD` values. Keep those passwords URL-safe (`dev-secrets.sh` uses hex).

Secrets in `.env`, all generated by `dev-secrets.sh`:

- the six database passwords;
- the identity keys `IDENTITY_TOTP_KMS_KEY`, `IDENTITY_SIGNING_KEYS_KMS_KEY` and `IDENTITY_CHALLENGE_SIGNING_KEY`;
- `ACCOUNTS_DOCUMENTS_KMS_KEY`;
- `ACCOUNTS_KYC_WEBHOOK_SECRET`, which the fake vendor shares.

The keys are base64 of 32 random bytes. `.env` is gitignored: never commit it, and don't reuse its values anywhere else. Mailpit's password isn't in `.env`; it's in nginx's htpasswd file.

### CORS

`GATEWAY_CORS_ALLOWED_ORIGINS` lists the exact browser origins (scheme, host and port) allowed to call the API. There are no wildcards. Listed origins get `Access-Control-Allow-Origin` echoed back, and their preflights are answered with:

- methods GET, POST, PUT, PATCH, DELETE and OPTIONS;
- headers `Authorization`, `Content-Type`, `Idempotency-Key` and `X-Request-Id`;
- a 10-minute max age.

Any other origin gets no CORS headers, so the browser blocks it. When the variable is empty, the gateway adds no CORS headers at all, and an OPTIONS request is refused by request validation (400) as before. The gateway handles CORS itself, so the nginx site adds no CORS headers.

Add the frontend team's dev-server origins, then run `make dev-up` to restart the gateway. The API is cookieless (sessions are bearer tokens), so there are no credentialed CORS requests.

## Testing the flows

**Email.** Sign-up, password set and reset, and email step-up send 6-digit codes. Mailpit catches every message: open `https://<MAIL_DOMAIN>` and sign in as `tester`. Scripts can use its API with the same login:

```sh
curl -u tester:<password> "https://<MAIL_DOMAIN>/api/v1/search?query=to:someone@example.com"  # newest first
curl -u tester:<password> "https://<MAIL_DOMAIN>/api/v1/message/<ID>"
```

On the host itself, `http://127.0.0.1:18025` reaches Mailpit without the password.

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

Staff can then use `/v1/accounts/staff/...` (the KYB review queue, tier changes, resellers) and `/v1/ledger/staff/...` (token prices, reversals, dev top-ups). Approving a token price or a reversal needs a second staff member (maker-checker) and a fresh step-up.

**Finance leads** approve reversals of KES 2,000 or more. A staff member is made one by an operator:

```sh
$DC run --rm vaspro identity grant-finance-lead -by <your name> <email>
$DC run --rm vaspro identity revoke-finance-lead -by <your name> <email>
```

**Money for testing.** There are no real top-ups yet (Payments, Phase 4). Staff credit a client's main wallet from the sandbox rail with `POST /v1/ledger/staff/dev-top-ups`; the client then buys tokens through `/v1/ledger/tenants/{tenantId}/token-purchases`, which needs a token price (list or deal) for the service first.

**Documents** (KYB) are scanned by ClamAV, encrypted, and stored in SeaweedFS. Uploads are capped at 10 MB, as PDF, JPEG or PNG. Nginx refuses any request body over 11 MB (`client_max_body_size 11m`, the gateway's own largest limit) with its plain 413 page, not the API's problem+json. The gateway answers anything smaller itself (VAL-4008 for a file over 10 MB).

## Rotating keys and passwords

- **JWT signing key:** `$DC run --rm vaspro identity rotate-signing-key`. The new key signs from then on. The old one stays in `/.well-known/jwks.json` for its 24-hour overlap, so issued tokens keep working. Nothing restarts.
- **Database passwords:** change the value in `.env`, then run `make dev-up`. `db-setup` sets the new password and the services restart with the new URL.
- **`IDENTITY_CHALLENGE_SIGNING_KEY`:** change it and run `make dev-up`. Only in-flight sign-in challenges (5 minutes) are lost.
- **Mailpit's password:** `sudo htpasswd /etc/nginx/.htpasswd-vaspro-mail tester` (without `-c`). Nginx reads the file on each request, so no reload is needed.
- **`ACCOUNTS_KYC_WEBHOOK_SECRET`:** change it and run `make dev-up`. The fake vendor picks it up too.
- **LocalKMS keys** (`IDENTITY_TOTP_KMS_KEY`, `IDENTITY_SIGNING_KEYS_KMS_KEY`, `ACCOUNTS_DOCUMENTS_KMS_KEY`): data sealed under the old key becomes unreadable (TOTP secrets, signing keys, documents), and there's no re-encryption tool. In this environment, rotating one means `make dev-reset`.
- **TLS certificates:** certbot renews them on its own timer and reloads nginx.

## Known limits

- **Identity sees the gateway's IP** for sign-ins and login alerts (see "Client addresses").
- **Unreachable fake-vendor link.** The KYC "hosted flow" URL points into the private network, and the fake serves no page there. Testers decide checks with `make dev-kyc`.
- **Unused infrastructure.** RabbitMQ, ClickHouse and Grafana aren't in this stack, because nothing built uses them yet.
- **UGX wallets don't settle.** Only Kenya has a pack, so the ledger logs a settlement failure for such wallets each window (see STATUS 3.5).
