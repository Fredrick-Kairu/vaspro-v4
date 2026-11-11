# VASPRO v4 Data Model

Oct 12, 2026 · @Fred Kairu

This is the v4 data model. Part 1 sets the rules every service schema follows, and designs the three foundation services the rest depend on: identity, accounts and KYC, and the ledger. Each service gets an entity-relationship diagram, PostgreSQL table definitions with keys and indexes, its events, and how its v3 tables map across. Part 2 adds billing and payments (sections 6 and 7), and Parts 3 and 4 follow in the same format. Flows referred to here are in [VASPRO v4 System Design: Core Flows](https://claude.ai/code/artifact/e41284c4-aa32-45a8-ba2e-484b736454a4).

## Decisions applied (12 and 21 Oct 2026)

| Decision | Effect on the data model |
| --- | --- |
| Each client buys tokens at its own price, fixed at purchase (21 Oct; replaces "tokens are worth the same as money") | A token is one unit of a service: one SMS unit, one USSD session unit. Its price comes from `token_prices` when the client buys: the list price with its volume tiers, or the client's negotiated deal. Each purchase is a lot with the price locked, so a new deal applies from the next purchase and units already bought keep their price. Airtime tokens stay at one currency unit each. |
| VAT is charged on use | Buying tokens moves the client's own money between wallets and carries no tax. Each usage posting splits its value into revenue and VAT payable, and monthly usage invoices (Part 2) are built from those lines. |
| VASPRO does not hold client collections | There is no collection wallet in the ledger. Collections are records of payments into the client's own paybill (Payments, Part 2), not ledger money. Premium MT revenue share is still money VASPRO owes clients, so it stays as a payable. |
| SMS sending stays in the existing SMS engine | Messaging (Part 3) keeps no SMPP binds or failover state. It stores the country-pack routes it chooses from, a log of each hand-off to the engine, and the DLR mapping tables. |
| Payouts leave from the client's own shortcode, like collections | No payout float in the ledger. Payments stores each client's rail accounts and encrypted credentials, and VASPRO's fee on each payout is held and settled in the client's payments token wallet like any other usage. |
| Allocating money to a service is final for the client (21 Oct) | No transaction type moves tokens back to the main wallet. Only an approved reversal can claw back unused tokens, for fraud, a chargeback or closing an account. |
| Code, shortcode and sender ID rentals are billed by invoice (21 Oct) | Rentals stay in Billing's billable assets register and monthly invoices, paid by auto-pay from the main wallet or by Pay now. They are not token purchases. USSD sessions and SMS still draw on their token wallets. |

## 1. Shared rules

Every service schema follows the same rules, so any engineer can read any schema and the same classes of bug are designed out everywhere.

| Rule | Choice | Why |
| --- | --- | --- |
| Engine and ownership | PostgreSQL 16, one schema per service. A service never reads another service's schema; it calls its API or consumes its events | Clear ownership; services can be split onto separate databases later without rewrites |
| Tenancy | `tenant_id uuid NOT NULL` on every client-owned row, enforced by PostgreSQL row-level security on the session setting `app.tenant_id` | A forgotten `WHERE` clause cannot leak another client's data |
| Primary keys | UUIDv7 (time-ordered). APIs expose prefixed forms such as `msg_…` and `inv_…` | Index-friendly, sortable by time, nothing to enumerate |
| Money | `bigint` in minor units with a `char(3)` ISO currency code; never float. A currency table holds the exponent (KES and TZS 2, UGX 0) | Exact arithmetic in every currency |
| Time | `timestamptz`, stored in UTC; each tenant stores its time zone | One clock; screens convert |
| Identity data | Emails as `citext` (case-insensitive), phones in E.164 | Email is the unique sign-in identity; any country's phone fits |
| Statuses | Enum types or text with a `CHECK` list; no magic numbers | Replaces v3 codes such as 11, 14 and 50 |
| Audit | `created_at`, `updated_at`, and `created_by` where a person acts; sensitive actions also go to an append-only security or audit log | Who did what, when |
| Deletion | Money and journal rows are never updated or deleted. Other rows use `deleted_at` only where a user can restore them | The ledger is append-only by design |
| Personal data | Columns holding personal data are listed in a data catalogue. Files (IDs, certificates) live in encrypted object storage, and the database keeps only a reference and hash. Secrets are Argon2id hashes or KMS-encrypted | Data-subject requests and breach impact stay manageable |
| Concurrency | Mutable rows carry `version` for optimistic locking; balance changes lock the balance row inside one transaction | No double spending under load |
| Idempotency | An `idempotency_keys` table in each schema stores the request hash and the first response | Retries return the original result |
| Events | A transactional outbox: the event row is written in the same database transaction as the change, and a relay publishes it to the event bus | No lost events, and no events for changes that rolled back |
| Naming | snake\_case, plural table names, `_id` for references, `_at` for timestamps, `_minor` for money | Predictable to read and query |
| Consuming events | A schema that consumes events keeps a processed\_events table (event\_id as primary key), written in the same transaction as the change the event causes | A redelivered event is never applied twice |

The two tables every schema carries, and the row-level security pattern:

```sql
CREATE TABLE outbox_events (
  id            uuid PRIMARY KEY,               -- UUIDv7
  tenant_id     uuid,
  aggregate     text NOT NULL,                  -- e.g. 'wallet', 'user'
  aggregate_id  uuid NOT NULL,
  event_type    text NOT NULL,                  -- e.g. 'tokens.purchased'
  payload       jsonb NOT NULL,
  occurred_at   timestamptz NOT NULL DEFAULT now(),
  published_at  timestamptz
);
CREATE INDEX outbox_unpublished ON outbox_events (occurred_at) WHERE published_at IS NULL;

CREATE TABLE idempotency_keys (
  tenant_id     uuid NOT NULL,
  key           text NOT NULL,
  request_hash  bytea NOT NULL,                 -- same key with a different body is rejected
  status_code   int,
  response      jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, key)
);

-- On every client-owned table:
ALTER TABLE <table> ENABLE ROW LEVEL SECURITY;
ALTER TABLE <table> FORCE ROW LEVEL SECURITY; -- binds the table owner too, not just other roles
CREATE POLICY tenant_isolation ON <table>
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
```

`current_setting(name, true)` (the `missing_ok` form) is required, not optional: with a pooled connection, a query that runs outside `db.WithTenantTx` must fail closed to zero rows, not error — and plain `current_setting(name)` errors instead when the setting was never touched in that session. `nullif(..., '')` is required too, for a reason that only shows up under connection pooling and is easy to miss testing against a single fresh connection: once a session has run `SET LOCAL`/`set_config(name, value, true)` inside any transaction, `current_setting(name, true)` after that transaction ends returns `''` (empty string), not `NULL`, even though the setting is unset again — casting `''::uuid` errors, so without `nullif` a later query on the same pooled connection, outside any `WithTenantTx`, raises an error instead of returning no rows. `WITH CHECK` (not just `USING`) is what makes an `INSERT`/`UPDATE` into another tenant's rows fail, not merely invisible.

## 2. Schema map

Each of the 14 services owns one schema; only high-volume event data and short-lived state live outside PostgreSQL. This part designs the first three; the rest follow in the order the build needs them.

| Service | Schema | Owns | Also uses | Designed in |
| --- | --- | --- | --- | --- |
| Identity and access | `identity` | Users, passkeys, TOTP, sessions, API clients, SMPP accounts, SSO, signing keys | Redis: rate limits | Part 1 |
| Accounts and KYC | `accounts` | Tenants, memberships, roles, service selections, KYC and KYB checks, documents | Object storage: documents | Part 1 |
| Ledger and wallets | `ledger` | Ledger accounts, balances, token prices and lots, rate cards, tax rates, holds, journal, reversals | — | Part 1 |
| Billing and invoicing | `billing` | Billable assets, plans, invoices, credit notes, Pay now links, dunning | Object storage: PDFs | Part 2 |
| Payments | `payments` | Payment intents, client paybills and tills, collection notices, payouts, rail credentials, reconciliation | — | Part 2 |
| Messaging orchestrator | `messaging` | Message requests, campaigns, routes, client limits, MT subscriptions | ClickHouse: per-message events; Redis: limit counters | Part 3 |
| USSD gateway | `ussd` | Codes, apps, menu versions | Redis: live sessions; ClickHouse: session events | Part 3 |
| Contacts and consent | `contacts` | Contacts, groups, consents, suppression lists | — | Part 3 |
| Applications | `applications` | Applications, per-network items, MNO options, ops tickets | Object storage | Part 3 |
| Verify and lookup | `verify` | Verifications, lookups | Redis | Part 4 |
| Journeys and AI agents | `journeys` | Flows, runs, conversations | — | Part 4 |
| Surveys and rewards | `surveys` | Surveys, questions, responses, incentives | — | Part 4 |
| Webhooks and reporting | `webhooks` | Endpoints, deliveries | ClickHouse: reports | Part 4 |
| Monitoring and alerting | `monitoring` | Rules, incidents, notifications | Prometheus, ClickHouse | Part 4 |

## 3. Identity and access (`identity`)

Identity knows who a person or machine is and nothing about what they may do inside an account; that belongs to Accounts (section 4). A user is one verified email address, and a phone number is optional profile data.

&#91;embedded content: identity schema · 11 tables\]

```sql
CREATE SCHEMA identity;
CREATE TYPE identity.user_status AS ENUM ('active', 'locked', 'disabled');
CREATE TYPE identity.environment AS ENUM ('sandbox', 'live');
CREATE TYPE identity.token_purpose AS ENUM
  ('verify_email', 'reset_password', 'change_email', 'verify_phone', 'step_up');

CREATE TABLE identity.users (
  id                 uuid PRIMARY KEY,
  email              citext NOT NULL UNIQUE,         -- the sign-in identity
  email_verified_at  timestamptz,
  full_name          text NOT NULL,
  phone_e164         text CHECK (phone_e164 ~ '^\+[1-9][0-9]{6,14}$'),
  phone_verified_at  timestamptz,
  password_hash      text,                           -- Argon2id; null for passkey-only or Google-only users
  status             identity.user_status NOT NULL DEFAULT 'active',
  locale             text NOT NULL DEFAULT 'en',
  last_sign_in_at    timestamptz,
  version            int NOT NULL DEFAULT 1,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX users_phone ON identity.users (phone_e164) WHERE phone_e164 IS NOT NULL;  -- not unique

CREATE TABLE identity.user_passkeys (
  id             uuid PRIMARY KEY,
  user_id        uuid NOT NULL REFERENCES identity.users (id),
  credential_id  bytea NOT NULL UNIQUE,
  public_key     bytea NOT NULL,
  sign_count     bigint NOT NULL DEFAULT 0,
  name           text,                               -- e.g. 'Work laptop'
  created_at     timestamptz NOT NULL DEFAULT now(),
  last_used_at   timestamptz
);
CREATE INDEX user_passkeys_user ON identity.user_passkeys (user_id);

CREATE TABLE identity.user_totp (
  user_id               uuid PRIMARY KEY REFERENCES identity.users (id),
  secret_ciphertext     bytea NOT NULL,              -- encrypted with a KMS key
  confirmed_at          timestamptz,
  recovery_code_hashes  text[] NOT NULL DEFAULT '{}'
);

CREATE TABLE identity.verification_tokens (
  id          uuid PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES identity.users (id),
  purpose     identity.token_purpose NOT NULL,
  target      text NOT NULL,                         -- the email or phone being checked
  token_hash  bytea NOT NULL UNIQUE,
  attempts    smallint NOT NULL DEFAULT 0,
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz
);

CREATE TABLE identity.sessions (
  id            uuid PRIMARY KEY,
  user_id       uuid NOT NULL REFERENCES identity.users (id),
  tenant_id     uuid,                                -- account currently selected
  ip            inet,
  user_agent    text,
  auth_method   text NOT NULL CHECK (auth_method IN ('password', 'passkey', 'google', 'sso')),
  step_up_at    timestamptz,                         -- last fresh MFA, for sensitive actions
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  revoked_at    timestamptz
);
CREATE INDEX sessions_active ON identity.sessions (user_id) WHERE revoked_at IS NULL;

CREATE TABLE identity.security_events (
  id           uuid NOT NULL,
  user_id      uuid,
  tenant_id    uuid,
  type         text NOT NULL,                        -- sign_in, sign_in_failed, mfa_enrolled, secret_revealed ...
  ip           inet,
  detail       jsonb NOT NULL DEFAULT '{}',
  occurred_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);                   -- one partition per month

CREATE TABLE identity.api_clients (
  id            uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL,
  name          text NOT NULL,
  environment   identity.environment NOT NULL,
  scopes        text[] NOT NULL,                     -- e.g. {sms:send, reports:read}
  ip_allowlist  cidr[] NOT NULL DEFAULT '{}',
  expires_at    timestamptz,
  created_by    uuid NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  revoked_at    timestamptz
);
CREATE INDEX api_clients_tenant ON identity.api_clients (tenant_id) WHERE revoked_at IS NULL;

CREATE TABLE identity.api_client_secrets (
  id             uuid PRIMARY KEY,
  api_client_id  uuid NOT NULL REFERENCES identity.api_clients (id),
  secret_hash    text NOT NULL,                      -- Argon2id; the secret is shown once
  last4          char(4) NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  expires_at     timestamptz,                        -- end of the rotation overlap
  revoked_at     timestamptz
);
CREATE INDEX api_client_secrets_active ON identity.api_client_secrets (api_client_id)
  WHERE revoked_at IS NULL;

CREATE TABLE identity.smpp_accounts (
  id             uuid PRIMARY KEY,
  tenant_id      uuid NOT NULL,
  system_id      text NOT NULL UNIQUE,
  password_hash  text NOT NULL,
  ip_allowlist   cidr[] NOT NULL,
  max_binds      smallint NOT NULL,
  max_tps        int NOT NULL,
  environment    identity.environment NOT NULL,
  status         text NOT NULL CHECK (status IN ('active', 'suspended'))
);

CREATE TABLE identity.sso_connections (
  id         uuid PRIMARY KEY,
  tenant_id  uuid NOT NULL,
  protocol   text NOT NULL CHECK (protocol IN ('saml', 'oidc')),
  domain     citext NOT NULL UNIQUE,                 -- users at this domain sign in by SSO
  config     jsonb NOT NULL,                         -- metadata URL, client ID; secrets in KMS
  enabled    boolean NOT NULL DEFAULT true
);

CREATE TABLE identity.user_identities (            -- external sign-in accounts linked to a user
  id              uuid PRIMARY KEY,
  user_id         uuid NOT NULL REFERENCES identity.users (id),
  provider        text NOT NULL CHECK (provider IN ('google')),   -- more later, e.g. microsoft, apple
  subject         text NOT NULL,                     -- the provider's stable user ID (OIDC sub), never the email
  email_at_link   citext NOT NULL,                   -- the email the provider reported when linked
  hosted_domain   text,                              -- Google Workspace domain (hd claim), if any
  linked_at       timestamptz NOT NULL DEFAULT now(),
  last_used_at    timestamptz,
  UNIQUE (provider, subject),
  UNIQUE (user_id, provider)                         -- one Google account per user
);

CREATE TABLE identity.signing_keys (                -- global: published as the JWKS
  kid          text PRIMARY KEY,
  algorithm    text NOT NULL,                        -- EdDSA
  public_key   bytea NOT NULL,
  private_ref  text NOT NULL,                        -- KMS reference, never the key itself
  status       text NOT NULL CHECK (status IN ('active', 'retiring', 'retired')),
  created_at   timestamptz NOT NULL DEFAULT now()
);
```

- **SMPP passwords** are limited to 8 characters by the SMPP 3.4 protocol, so SMPP accounts always require an IP allowlist and TLS.
- **Up to two active secrets per API client:** the service enforces this, and the rotation overlap ends at `expires_at`.
- **Sessions** live in PostgreSQL for listing and revoking, and are cached in Redis for the gateway's per-request check.

**Events**

| Event | When |
| --- | --- |
| `user.registered` | Sign-up completed |
| `user.email_verified` | Email link used |
| `user.mfa_enrolled` | Passkey or TOTP added |
| `user.locked` | Too many failed sign-ins, or staff action |
| `api_client.created`, `api_client.secret_rotated`, `api_client.revoked` | Credential lifecycle |
| `session.revoked` | Sign-out everywhere, or admin action |

**From v3**

| v3 table or column | v4 | Notes |
| --- | --- | --- |
| `user` (email\_address, full\_names, password) | `identity.users` | Users without a unique email are flagged and must add one at first v4 sign-in |
| `user.password` | `identity.users.password_hash` | v3 stores bcrypt of an MD5 of the password. v4 checks it that way once, then re-hashes with Argon2id |
| `profile` (msisdn, network) | `identity.users.phone_e164` | Kept as optional profile data; `msisdn_hash` is dropped |
| `google2fa_secret`, `google2fa_enabled` | `identity.user_totp` | Secret re-encrypted with KMS on import |
| `user_login` (codes and attempts) | `identity.verification_tokens`, `security_events` | Codes are stored hashed; attempt counters move to Redis |
| `user_client_map.api_token`, `access_token` | `identity.api_clients`, `api_client_secrets` | Not migrated: clients create new credentials. A v3-compatible gateway route can accept old tokens for a set period |
| `clients.allow_okta_login` | `identity.sso_connections` | One row per client that used Okta |

## 4. Accounts and KYC (`accounts`)

A tenant is one client account. Accounts records who belongs to it and with what role, which services it chose at onboarding, and every verification that moved it up a tier. Resellers are tenants whose clients are sub-account tenants underneath them.

&#91;embedded content: accounts schema · 12 tables\]

```sql
CREATE SCHEMA accounts;
CREATE TYPE accounts.tier AS ENUM ('sandbox', 'starter', 'business', 'financial');
CREATE TYPE accounts.tenant_type AS ENUM ('direct', 'reseller', 'sub_account');

CREATE TABLE accounts.tenants (
  id                uuid PRIMARY KEY,
  type              accounts.tenant_type NOT NULL DEFAULT 'direct',
  parent_tenant_id  uuid REFERENCES accounts.tenants (id),   -- a reseller's clients
  legal_name        text,
  display_name      text NOT NULL,
  country_code      char(2) NOT NULL,                       -- selects the country pack
  timezone          text NOT NULL,                          -- e.g. Africa/Nairobi
  default_currency  char(3) NOT NULL,
  region            text NOT NULL,                          -- data residency tag
  tier              accounts.tier NOT NULL DEFAULT 'sandbox',
  status            text NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'suspended', 'archived')),
  billing_email     citext,
  version           int NOT NULL DEFAULT 1,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tenants_parent ON accounts.tenants (parent_tenant_id) WHERE parent_tenant_id IS NOT NULL;

CREATE TABLE accounts.roles (
  id           uuid PRIMARY KEY,
  tenant_id    uuid REFERENCES accounts.tenants (id),    -- null = built-in role
  name         text NOT NULL,                            -- Admin, Developer, Finance, Viewer ...
  permissions  text[] NOT NULL,                          -- e.g. {sms:send, payouts:approve}
  UNIQUE NULLS NOT DISTINCT (tenant_id, name)
);

CREATE TABLE accounts.memberships (
  tenant_id   uuid NOT NULL REFERENCES accounts.tenants (id),
  user_id     uuid NOT NULL,                             -- identity.users
  role_id     uuid NOT NULL REFERENCES accounts.roles (id),
  status      text NOT NULL CHECK (status IN ('invited', 'active', 'removed')),
  invited_by  uuid,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_id)
);
CREATE INDEX memberships_user ON accounts.memberships (user_id);   -- 'which accounts am I in?'

CREATE TABLE accounts.services (                       -- global catalogue
  code              text PRIMARY KEY,                  -- sms, whatsapp, ussd, airtime, payouts ...
  name              text NOT NULL,
  default_min_tier  accounts.tier NOT NULL
);

CREATE TABLE accounts.service_country_rules (          -- global: country pack overrides
  service_code  text NOT NULL REFERENCES accounts.services (code),
  country_code  char(2) NOT NULL,
  min_tier      accounts.tier NOT NULL,
  enabled       boolean NOT NULL DEFAULT true,
  PRIMARY KEY (service_code, country_code)
);

CREATE TABLE accounts.tenant_services (                -- choices made at onboarding
  tenant_id     uuid NOT NULL REFERENCES accounts.tenants (id),
  service_code  text NOT NULL REFERENCES accounts.services (code),
  status        text NOT NULL CHECK (status IN ('selected', 'enabled', 'suspended')),
  selected_at   timestamptz NOT NULL DEFAULT now(),
  enabled_at    timestamptz,
  PRIMARY KEY (tenant_id, service_code)
);

CREATE TABLE accounts.tier_caps (                      -- global: staff-set limits, e.g. Tier 1 SMS per day
  country_code  char(2) NOT NULL,
  tier          accounts.tier NOT NULL,
  metric        text NOT NULL,                         -- sms_per_day, spend_per_day_minor ...
  period        text NOT NULL CHECK (period IN ('day', 'month')),
  value         bigint NOT NULL,
  PRIMARY KEY (country_code, tier, metric, period)
);

CREATE TABLE accounts.business_profiles (
  id                   uuid PRIMARY KEY,
  tenant_id            uuid NOT NULL UNIQUE REFERENCES accounts.tenants (id),
  registry             text NOT NULL,                  -- BRS, BRELA, URSB
  registration_number  text NOT NULL,
  registered_name      text NOT NULL,
  tax_id               text,                           -- KRA PIN, TIN
  registered_address   text,
  verified_at          timestamptz
);
CREATE INDEX business_profiles_registration ON accounts.business_profiles (registry, registration_number);

CREATE TABLE accounts.beneficial_owners (
  id                   uuid PRIMARY KEY,
  tenant_id            uuid NOT NULL,
  business_profile_id  uuid NOT NULL REFERENCES accounts.business_profiles (id),
  full_name            text NOT NULL,
  role                 text NOT NULL CHECK (role IN ('director', 'owner', 'signatory')),
  ownership_pct        numeric(5,2),
  screened_at          timestamptz,
  screening_result     text CHECK (screening_result IN ('clear', 'match', 'review'))
);

CREATE TABLE accounts.documents (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES accounts.tenants (id),
  type         text NOT NULL,          -- national_id, passport, incorporation_cert, tax_certificate, authorisation_letter
  storage_key  text NOT NULL,          -- encrypted object storage; the file is never in the database
  sha256       bytea NOT NULL,
  mime_type    text NOT NULL,
  size_bytes   int NOT NULL,
  status       text NOT NULL CHECK (status IN ('received', 'accepted', 'rejected', 'expired')),
  expires_at   date,
  uploaded_by  uuid NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX documents_tenant_type ON accounts.documents (tenant_id, type);

CREATE TABLE accounts.kyc_checks (
  id            uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL REFERENCES accounts.tenants (id),
  kind          text NOT NULL CHECK (kind IN ('individual', 'business', 'screening')),
  subject_id    uuid NOT NULL,         -- a user, business profile or beneficial owner
  provider      text NOT NULL,         -- adapter name; the vendor is still to be chosen
  provider_ref  text,
  status        text NOT NULL CHECK (status IN ('pending', 'passed', 'review', 'failed', 'expired')),
  result        jsonb NOT NULL DEFAULT '{}',   -- summary only; the full report stays with the provider
  reviewed_by   uuid,
  decided_at    timestamptz,
  expires_at    timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, provider_ref)
);
CREATE INDEX kyc_checks_tenant ON accounts.kyc_checks (tenant_id, created_at DESC);
CREATE INDEX kyc_checks_review ON accounts.kyc_checks (created_at) WHERE status = 'review';  -- staff queue

CREATE TABLE accounts.tier_changes (
  id            uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL REFERENCES accounts.tenants (id),
  from_tier     accounts.tier NOT NULL,
  to_tier       accounts.tier NOT NULL,
  reason        text NOT NULL,
  kyc_check_id  uuid REFERENCES accounts.kyc_checks (id),
  changed_by    uuid,                  -- null when automatic
  created_at    timestamptz NOT NULL DEFAULT now()
);
```

- **A tenant's tier only changes through `tier_changes`,** written in the same transaction as the update to `tenants.tier`, so every promotion and demotion has a reason and, where relevant, a KYC check behind it.
- **Documents are uploaded once** and referenced by Applications (Part 3) and reversals (section 5), so a client never uploads the same certificate twice.
- **Business profiles are not unique per registration number.** An agency may run several accounts for one company, so duplicates are flagged for review instead of blocked.

**Events**

| Event | When |
| --- | --- |
| `tenant.created` | Sign-up creates the sandbox account |
| `tenant.tier_changed` | Any promotion or demotion, with the reason |
| `tenant.suspended`, `tenant.reactivated` | Staff action or unpaid invoices |
| `service.selected`, `service.enabled` | Onboarding choices, and the tier being reached |
| `kyc.check_started`, `kyc.check_decided` | Each verification run |
| `membership.invited`, `membership.joined`, `membership.removed` | Team changes |

**From v3**

| v3 table | v4 | Notes |
| --- | --- | --- |
| `clients` (client\_name, client\_email, country\_id) | `accounts.tenants` | `country_id` becomes an ISO country code; `client_email` becomes `billing_email` |
| `user_client_map` | `accounts.memberships` | One row per user per client; `permission_acl` becomes a role |
| `client_authentication`, `client_auth_roles`, `user_permission` | `accounts.roles` | Permission names are redefined per v4 service, such as `sms:send` |
| `client_resellers`, `client_reseller_accounts` | `tenants.type`, `parent_tenant_id` | Resellers become parent tenants and their clients sub-accounts |
| `client_services` (unit\_cost, discount) | `tenant_services` plus `ledger.token_prices` | The selection stays here; client-specific prices move to the ledger's token prices |
| Sign-up `national_id`, `date_of_birth` | `kyc_checks`, `documents` | Self-declared v3 values are not trusted; Tier 1 KYC verifies again |
| `client_billingmode` | Billing (Part 2) | Pre-paid or post-paid becomes a billing plan setting |
| `clients_leads`, `client_referral` | Part 4 | Sales and referral data is not a foundation concern |

## 5. Ledger and wallets (`ledger`)

Every shilling, shilling-worth of tokens and tax amount moves only through balanced journal entries: each transaction's debits equal its credits. Wallet balances are a copy kept in step inside the same database transaction, and a nightly job rebuilds them from the journal to prove they still agree.

&#91;embedded content: ledger schema · 14 tables\]

**Account types.** Each client gets one main wallet per currency, one service wallet per service it uses, and a bonus account. VASPRO's own side holds clearing, suspense, revenue, tax and payable accounts. There is no collection wallet, because client collections never pass through VASPRO.

| Type | Owner | Holds |
| --- | --- | --- |
| `main_wallet` | Client, one per currency | Money paid in by top-up |
| `service_wallet` | Client, one per service and currency | Money turned into tokens for that service |
| `bonus` | Client | Promo credit and migrated v3 bonus; usable on any service unless restricted |
| `receivable` | Client | Unpaid invoices and recorded debts |
| `revenue_share_payable`, `commission_payable` | Client | Premium MT revenue share and reseller commissions owed to the client |
| `provider_clearing` | VASPRO, one per rail | Money a rail (e.g. M-Pesa KE) owes VASPRO until it settles |
| `suspense`, `reversal_clearing` | VASPRO | Unmatched incoming money; approved reversals not yet paid back |
| `revenue` | VASPRO, one per service | Net usage revenue |
| `tax_payable` | VASPRO, one per tax and country | VAT collected on use |

```sql
CREATE SCHEMA ledger;
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE ledger.currencies (
  code            char(3) PRIMARY KEY,       -- KES, TZS, UGX
  minor_exponent  smallint NOT NULL          -- KES 2, TZS 2, UGX 0
);

CREATE TYPE ledger.account_type AS ENUM (
  'main_wallet', 'service_wallet', 'bonus', 'receivable',
  'revenue_share_payable', 'commission_payable',
  'provider_clearing', 'suspense', 'reversal_clearing', 'revenue', 'tax_payable');

CREATE TABLE ledger.accounts (
  id            uuid PRIMARY KEY,
  tenant_id     uuid,                        -- null for VASPRO's own accounts
  type          ledger.account_type NOT NULL,
  service_code  text,                        -- service wallets, revenue, restricted bonus
  rail          text,                        -- provider_clearing, e.g. 'mpesa_ke'
  currency      char(3) NOT NULL REFERENCES ledger.currencies (code),
  status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'frozen', 'closed')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (tenant_id, type, service_code, rail, currency)
);

CREATE TABLE ledger.balances (               -- a copy of the journal, updated in the same transaction
  account_id     uuid PRIMARY KEY REFERENCES ledger.accounts (id),
  balance_minor  bigint NOT NULL DEFAULT 0,
  held_minor     bigint NOT NULL DEFAULT 0 CHECK (held_minor >= 0),
  version        bigint NOT NULL DEFAULT 0,
  updated_at     timestamptz NOT NULL DEFAULT now()
);
-- Available = balance_minor - held_minor. A trigger stops client wallets going below zero.

CREATE TABLE ledger.token_prices (           -- what a client pays per token: list price, volume tier or negotiated deal
  id               uuid PRIMARY KEY,
  tenant_id        uuid,                     -- null = list price for everyone; set = this client's negotiated deal
  service_code     text NOT NULL,            -- sms, ussd, payments, airtime ...
  currency         char(3) NOT NULL REFERENCES ledger.currencies (code),
  min_tokens       numeric(18,6) NOT NULL DEFAULT 0,   -- volume tier: applies to purchases of at least this many
  minor_per_token  numeric(18,6) NOT NULL CHECK (minor_per_token > 0),  -- e.g. 80 = KES 0.80 per SMS unit
  deal_reference   text,                     -- the signed quote or agreement behind a negotiated price
  status           text NOT NULL CHECK (status IN ('pending_approval', 'active', 'withdrawn')),
  effective_from   timestamptz NOT NULL,
  effective_to     timestamptz,              -- a deal "until 31 March" ends here; the list price applies after
  created_by       uuid NOT NULL,
  approved_by      uuid,
  CHECK (status <> 'active' OR approved_by IS NOT NULL),
  CHECK (approved_by IS NULL OR approved_by <> created_by),   -- maker-checker for every price
  EXCLUDE USING gist (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid) WITH =,
                     service_code WITH =, currency WITH =, min_tokens WITH =,
                     tstzrange(effective_from, effective_to) WITH &&)
    WHERE (status = 'active')                -- one active price per tier at any moment
);
CREATE INDEX token_prices_lookup ON ledger.token_prices (service_code, currency, tenant_id, effective_from DESC)
  WHERE status = 'active';
-- Price at purchase: the client's own active rows for the service if any are in force, otherwise the list rows;
-- of those, the row with the highest min_tokens not above the quantity bought. The price is copied into the lot.

CREATE TABLE ledger.rate_cards (             -- how many tokens one event uses; prices are in token_prices
  id                  uuid PRIMARY KEY,
  tenant_id           uuid,                  -- null = standard; set = a rare per-client exception
  service_code        text NOT NULL,
  country_code        char(2) NOT NULL,
  operator_code       text,                  -- null = any operator in the country
  variant             text,                  -- promotional, transactional, international ...
  tokens_per_event    numeric(18,6) NOT NULL,   -- e.g. 1 per local SMS segment, 3 per international
  prices_include_tax  boolean NOT NULL,
  effective_from      timestamptz NOT NULL,
  effective_to        timestamptz
);
CREATE INDEX rate_cards_lookup ON ledger.rate_cards
  (service_code, country_code, operator_code, tenant_id, effective_from DESC);

CREATE TABLE ledger.tax_rates (
  country_code    char(2) NOT NULL,
  tax_code        text NOT NULL,             -- VAT
  rate            numeric(6,4) NOT NULL,     -- 0.1600 for Kenya
  effective_from  date NOT NULL,
  effective_to    date,
  PRIMARY KEY (country_code, tax_code, effective_from)
);

CREATE TYPE ledger.txn_type AS ENUM (
  'topup', 'token_purchase', 'usage', 'refund', 'bonus_grant', 'invoice_payment',
  'reversal', 'payback', 'adjustment', 'revenue_share', 'migration');

CREATE TABLE ledger.transactions (           -- immutable; a correction is a new transaction
  id               uuid PRIMARY KEY,
  tenant_id        uuid,
  type             ledger.txn_type NOT NULL,
  idempotency_key  text NOT NULL,
  reference_type   text,                     -- payment_intent, usage_batch, invoice, reversal ...
  reference_id     uuid,
  reverses_id      uuid REFERENCES ledger.transactions (id),
  description      text NOT NULL,
  created_by       uuid,                     -- null for system postings
  posted_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (tenant_id, idempotency_key)
);
CREATE INDEX transactions_tenant ON ledger.transactions (tenant_id, posted_at DESC);
CREATE INDEX transactions_reference ON ledger.transactions (reference_type, reference_id);

CREATE TABLE ledger.entries (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  transaction_id  uuid NOT NULL REFERENCES ledger.transactions (id),
  account_id      uuid NOT NULL REFERENCES ledger.accounts (id),
  direction       char(1) NOT NULL CHECK (direction IN ('D', 'C')),
  amount_minor    bigint NOT NULL CHECK (amount_minor > 0),
  tokens          numeric(18,6),             -- on service wallet lines
  tax_code        text,                      -- on tax lines
  currency        char(3) NOT NULL
);
CREATE INDEX entries_account ON ledger.entries (account_id, id);
CREATE INDEX entries_transaction ON ledger.entries (transaction_id);
-- A deferred constraint trigger rejects any transaction whose debits and credits differ per currency.
-- Updates and deletes on transactions and entries are blocked by trigger and by database grants.

CREATE TABLE ledger.token_lots (             -- each purchase and its locked price, used first in, first out
  id                uuid PRIMARY KEY,
  tenant_id         uuid NOT NULL,
  account_id        uuid NOT NULL REFERENCES ledger.accounts (id),       -- the service wallet
  transaction_id    uuid NOT NULL REFERENCES ledger.transactions (id),
  tokens_bought     numeric(18,6) NOT NULL,
  tokens_remaining  numeric(18,6) NOT NULL CHECK (tokens_remaining >= 0),
  minor_per_token   numeric(18,6) NOT NULL,   -- locked at purchase
  token_price_id    uuid NOT NULL REFERENCES ledger.token_prices (id),   -- the list, tier or deal row used
  purchased_at      timestamptz NOT NULL,
  expires_at        timestamptz                -- unused today; ready if tokens ever expire
);
CREATE INDEX token_lots_open ON ledger.token_lots (account_id, purchased_at) WHERE tokens_remaining > 0;

CREATE TABLE ledger.lot_consumptions (
  lot_id          uuid NOT NULL REFERENCES ledger.token_lots (id),
  transaction_id  uuid NOT NULL REFERENCES ledger.transactions (id),
  tokens          numeric(18,6) NOT NULL,
  value_minor     bigint NOT NULL,
  PRIMARY KEY (lot_id, transaction_id)
);

CREATE TABLE ledger.holds (
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  account_id      uuid NOT NULL REFERENCES ledger.accounts (id),
  reference_type  text NOT NULL,             -- message_request, campaign, payout, ussd_session
  reference_id    uuid NOT NULL,
  amount_minor    bigint NOT NULL CHECK (amount_minor > 0),
  tokens          numeric(18,6),
  status          text NOT NULL CHECK (status IN ('active', 'captured', 'released', 'expired')),
  created_at      timestamptz NOT NULL DEFAULT now(),
  expires_at      timestamptz NOT NULL,
  settled_at      timestamptz,
  UNIQUE (reference_type, reference_id)
);
CREATE INDEX holds_active ON ledger.holds (account_id) WHERE status = 'active';

CREATE TABLE ledger.usage_batches (          -- one journal posting per wallet per settlement window
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL,
  account_id      uuid NOT NULL REFERENCES ledger.accounts (id),
  window_start    timestamptz NOT NULL,
  window_end      timestamptz NOT NULL,
  events          bigint NOT NULL,           -- e.g. delivered messages in the window
  tokens          numeric(18,6) NOT NULL,
  transaction_id  uuid REFERENCES ledger.transactions (id),
  UNIQUE (account_id, window_start)
);

CREATE TABLE ledger.auto_buy_rules (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL,
  service_code       text NOT NULL,
  currency           char(3) NOT NULL,
  below_tokens       numeric(18,6) NOT NULL,
  buy_tokens         numeric(18,6) NOT NULL,
  enabled            boolean NOT NULL DEFAULT true,
  last_triggered_at  timestamptz,
  UNIQUE (tenant_id, service_code, currency)
);

CREATE TABLE ledger.reversals (
  id                       uuid PRIMARY KEY,
  tenant_id                uuid NOT NULL,
  original_transaction_id  uuid NOT NULL REFERENCES ledger.transactions (id),
  amount_minor             bigint NOT NULL CHECK (amount_minor > 0),
  reason                   text NOT NULL,
  evidence_document_id     uuid,             -- accounts.documents
  status                   text NOT NULL CHECK (status IN ('requested', 'approved', 'rejected',
                             'posted', 'payback_pending', 'paid_back', 'failed')),
  requested_by             uuid NOT NULL,
  approved_by              uuid,
  reversal_transaction_id  uuid REFERENCES ledger.transactions (id),
  payback_reference        text,             -- the Payments service's reference
  created_at               timestamptz NOT NULL DEFAULT now(),
  decided_at               timestamptz,
  CHECK (approved_by IS NULL OR approved_by <> requested_by)   -- maker-checker
);
```

> **Built in tasks 3.1–3.2 (4 Nov 2026, OQ-13, ADR 0010).** Additions to the schema above:
> - **RLS:**
>   - the five tables with `tenant_id NOT NULL` use the standard policy;
>   - `accounts`, `token_prices` and `rate_cards`: a tenant reads its own rows and VASPRO's (tenant_id NULL), and writes only its own;
>   - `transactions`: a tenant's own rows only;
>   - `entries` follow their transaction; a client posting may use the tenant's accounts or VASPRO's;
>   - `balances` follow their account, and `lot_consumptions` their lot;
>   - VASPRO's rows are written only by the NOLOGIN role `ledger_system` (`WithRoleTx`).
> - **Grants:** the service's role `ledger_service` has no UPDATE, DELETE or TRUNCATE on `transactions`, `entries` or `lot_consumptions`, and on `balances` may write only `held_minor`.
> - **CHECKs on accounts:** client types have a tenant and VASPRO's don't; service wallets and revenue name a service; only provider clearing names a rail.
> - **Signs and balance rows:**
>   - balances are kept on each account's normal side (receivable and provider clearing debit-normal, all else credit-normal);
>   - only client accounts have a balance row, kept by a trigger on `entries`;
>   - VASPRO's accounts are summed from their entries (ADR 0010).
> - **Guards:**
>   - each transaction has a debit and a credit and balances per currency, checked at commit;
>   - an entry's currency is its account's;
>   - entries are inserted with their transaction (same database transaction and savepoint);
>   - `transactions`, `entries` and `lot_consumptions` are append-only, TRUNCATE included;
>   - main, service and bonus wallets keep `balance_minor >= 0` and `held_minor <= balance_minor` at commit.

> **Built in task 3.3 (4 Nov 2026, OQ-14).**
> - **`holds.captured_minor`:** set exactly when the hold is captured, from 1 to `amount_minor`.
> - **What a hold contributes to `held_minor`:**
>   - while active, its amount;
>   - once captured, until settled (`settled_at`), the captured amount;
>   - nothing after.
> - **Settlement:** release and a capture carrying its own posting settle the hold at once; 3.5's settlement settles plain captures.
> - **`ledger_system`** reads every line on VASPRO's own accounts, so their balances can be summed from the journal.

> **Built in task 3.4 (4 Nov 2026, OQ-15).**
> - **Rounding:** a purchase costs tokens × price, rounded half-up. A draw from a lot is valued by cumulative rounding, so a lot's draws always sum to its cost.
> - **Token holds:**
>   - service-wallet holds reserve tokens: `balances.held_tokens`, and `holds.captured_tokens` for a capture;
>   - the wallet guard also refuses holding more tokens than the open lots have left, checked on `balances` and `token_lots`;
>   - money holds (main and bonus wallets) keep using `held_minor`.
> - **`token_lots` CHECKs:** bought > 0, remaining ≤ bought, price > 0.
> - **Service wallets:** money moves only through lots, so the generic posting API can't touch them.

> **Built in task 3.5 (4 Nov 2026, OQ-16).**
> - **The batch:** settlement runs once per window (OD-07). Per service wallet, one `usage_batches` row and one `usage` posting cover every token hold captured and unsettled when it runs.
> - **The posting:** it draws the captured tokens FIFO and splits the value drawn into revenue and VAT (the pack's rate in force, VAT-inclusive by the pack's default).
> - **VAT excluded:** the VAT comes from the main wallet, and what it can't cover goes to the receivable.
> - **New columns:**
>   - `holds.captured_events` (the delivered events a token capture covers);
>   - `holds.usage_batch_id`.
> - **New CHECKs on batches:** the window runs forward, events ≥ 0, tokens > 0.
> - **Money holds** are captured only with their posting.
> - **Packs** carry `tax.vat_effective_from`.

> **Built in task 3.6 (4 Nov 2026, OQ-17).**
> - **Expiry:** the expiry job expires active holds past `expires_at` plus one settlement window, and releases what they held; a capture after expiry is refused.
> - **Auto-buy** runs inside a token hold: the rule buys its amount first when the hold would leave fewer tokens than its level or can't be covered, and only if the purchase lets the hold through.
> - **CHECKs:** `auto_buy_rules.below_tokens ≥ 0`, `buy_tokens > 0`.
> - **`ledger_settlement`** also reads active holds, for the expiry job.

> **Built in task 3.7 (9 Nov 2026, OQ-18).** Additions to `reversals`:
> - **Columns:** `kind` (`payback`, `chargeback`, `operator`), `currency`, `clawback_services` (the wallets the maker named, in order), `rejected_by` and `rejection_reason`, `payback_transaction_id`, `payback_attempts`, `last_payback_error`.
> - **CHECKs:** each status carries exactly its decision columns; a rejection is never by its maker; `payback_pending` and `paid_back` only for `payback`, `posted` only for the others; `paid_back` names its pay-back transaction.
> - **Postings:** the reversal (`reverses_id` the top-up) debits the main wallet up to its available balance, then the named wallets' unused, unheld tokens, newest lot first (recorded in `lot_consumptions`); it credits reversal clearing for a pay-back, or the rail's provider clearing for a chargeback or operator reversal. The pay-back moves reversal clearing to provider clearing. No VAT moves: top-ups carry none.
> - **Cap:** the live reversals of a top-up never exceed what it credited, checked under the main wallet's balance lock.
> - **`ledger_reversal_review`** reads reversals across clients for the staff queue. `identity.staff_members.finance_lead` marks finance leads.

> **Built in task 3.8 (9 Nov 2026, OQ-19).** The nightly rebuild compares every copy with the journal and corrects nothing.
> - **`ledger.rebuild_runs`** (an addition): one row per date (`running`, `passed`, `failed`, with counts), claimed by the replica that runs it.
> - **Checks:** every transaction and currency balances; balance rows against entries on the normal side; held money and tokens against holds; each lot's remaining against its consumptions; each service wallet's money against its lots' remaining value; the wallet floor.
> - **Events:** `ledger.rebuild_failed` (P1) or `ledger.rebuild_passed`.
> - **`ledger_audit`** reads every client's journal and copies, only for the rebuild.

> **Built in task 3.9 (9 Nov 2026, OQ-20).** No schema change. The dashboard reads balances, statements (each line with the balance after it, from one snapshot), the prices in force and quotes, and buys tokens and sets auto-buy rules, with access checked by accounts (billing:read, billing:manage). Dev environments can credit main wallets from VASPRO's `sandbox` rail clearing (LEDGER_DEV_TOPUPS), as ordinary top-ups.

**Worked example (Kenya, a client deal of KES 0.80 per SMS, renegotiated to KES 0.50; prices include VAT at 16%)**

| Step | Debit | Credit | KES | SMS units |
| --- | --- | --- | --- | --- |
| 1. Client tops up by M-Pesa | Provider clearing (M-Pesa KE) | Main wallet | 13,000.00 | — |
| 2. Buys 10,000 SMS units at its deal of KES 0.80 | Main wallet | SMS service wallet | 8,000.00 | 10,000 added, lot A at 80 cents each |
| 3. Sends a 400-SMS campaign at 1 unit per SMS | No journal entry; a hold reserves the units | — | 320.00 held | 400 held |
| 4. Settlement: 380 delivered, 20 failed (not billable) | SMS service wallet 304.00 | Revenue (SMS) 262.07 and VAT payable 41.93 | 304.00 | 380 used from lot A, 20 released |
| 5. New deal of KES 0.50 from 1 Nov; buys 10,000 more | Main wallet | SMS service wallet | 5,000.00 | 10,000 added, lot B at 50 cents; lot A's 9,620 keep 80 cents and are used first |

Buying tokens (steps 2 and 5) carries no VAT, because it only moves the client's own money; VAT is charged when units are used (step 4), on the price of the lot they came from. If a country's prices are set to exclude tax, step 4 debits 352.64: 304.00 for usage plus 48.64 VAT.

**Why it is built this way**

- **Holds are not journal entries.** A hold only raises held\_minor, and the journal is touched once per wallet per settlement window, when delivered usage is posted as one usage transaction. Per-message detail stays in ClickHouse, tagged with the batch ID. This keeps the journal at millions of rows a year instead of billions. Payout fees settle the same way, because the payout itself leaves from the client's own shortcode. Airtime, data and bill purchases are the exception: each posts its own transaction, because VASPRO pays a provider for each one.
- **Prices are configuration, per client.** A list price, a volume tier or a negotiated deal is a `token_prices` row with start and end dates, approved by a second staff member. The price is found when tokens are bought and locked in the lot. Usage draws from the oldest lot first, so revenue and VAT are always calculated on the price actually paid, and a new deal never changes units already bought.
- **Allocation is final.** No transaction type moves tokens back to the main wallet. Only an approved reversal can claw back unused tokens, for fraud, a chargeback or closing an account.
- **Fractional tokens are allowed** (`numeric(18,6)`), so a rate card can use part of a unit and a price can carry fractions of a cent.
- **Proof of correctness:** each night the job rebuilds every balance from the entries. Any difference, or any transaction that does not balance, raises a P1 incident through the alerting engine.

**Events**

| Event | When |
| --- | --- |
| `wallet.credited` | A top-up or refund lands in the main wallet |
| `tokens.purchased` | Main wallet to service wallet, manual or auto-buy |
| `tokens.low`, `tokens.depleted` | Service wallet below the client's alert level, or at zero |
| `usage.posted` | A settlement batch posted, with tokens used and VAT |
| `refund.posted` | Tokens or money returned with a reason code |
| `reversal.requested`, `reversal.approved`, `reversal.posted`, `reversal.paid_back` | Reversal lifecycle (design doc 4.3) |
| `autobuy.triggered` | An auto-buy rule ran |

**From v3**

| v3 table or column | v4 | Notes |
| --- | --- | --- |
| `client_balance.balance` | Main wallet | Opening balance posted as one `migration` transaction per client, against VASPRO's `migration_clearing` (OQ-21, ADR 0011) |
| `client_balance.bonus`, `client_app_bonus` | Bonus account | Money, usable on any service unless restricted (OQ-21; system design §4.2's "bonus tokens" wording is superseded) |
| `client_balance.subscription_balance` | `revenue_share_payable` | Assumed to be premium subscription revenue owed to the client; to confirm |
| `client_balance.alert_threshold` | Client alert rule (Monitoring, Part 4) | Not ledger data |
| `user_balance.commissions` | `commission_payable` | Reseller and agent commissions |
| `transactions` (reference types 1 to 30) | `ledger.v3_history` | v3 history is imported read-only, with the v3 reference kept, into its own table, never journal entries (OQ-21, ADR 0011: entries would count it twice alongside the opening balance). Balances are checked against v3 totals before cut-over |
| `client_services.unit_cost`, `discount` | `token_prices` with `tenant_id` set | Each client's current SMS price (unit\_cost less discount) becomes its negotiated deal from cut-over |
| `manual_payments` | `topup` transactions with `created_by` | v4 adds maker-checker for manual loads |
| `invoices`, `invoice_status`, `mpesa_transactions` | Billing and Payments (Part 2) |  |

## 6. Billing and invoicing (billing)

Billing turns everything recurring into one invoice per client per cycle, issues the monthly usage invoice from the ledger's usage postings, and chases what is unpaid. It owns the register of billable assets the billing run reads, so a new or released code is billed correctly without anyone touching it. It holds no balances: every payment it records points to a ledger transaction.

&#91;embedded content: billing schema · 14 tables\]

```sql
CREATE SCHEMA billing;

CREATE TABLE billing.billing_profiles (          -- one per client account
  tenant_id           uuid PRIMARY KEY,
  currency            char(3) NOT NULL,
  country_code        char(2) NOT NULL,        -- decides tax, eTIMS and the number series
  billing_day         smallint NOT NULL DEFAULT 1 CHECK (billing_day BETWEEN 1 AND 28),
  mode                text NOT NULL DEFAULT 'prepaid' CHECK (mode IN ('prepaid', 'postpaid')),
  credit_limit_minor  bigint NOT NULL DEFAULT 0,   -- postpaid only
  payment_terms_days  smallint NOT NULL DEFAULT 7,
  auto_pay            boolean NOT NULL DEFAULT false,
  auto_pay_sources    text[] NOT NULL DEFAULT '{main_wallet}',  -- tried in order: main_wallet, card, standing_order
  billing_email       citext NOT NULL,
  billing_name        text NOT NULL,
  tax_pin             text,                    -- KRA PIN in Kenya, TIN elsewhere
  postal_address      text,
  version             bigint NOT NULL DEFAULT 0,
  updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE billing.plans (                     -- global catalogue: bundles and support plans
  id                  uuid PRIMARY KEY,
  code                text NOT NULL,
  name                text NOT NULL,
  country_code        char(2) NOT NULL,
  currency            char(3) NOT NULL,
  interval            text NOT NULL CHECK (interval IN ('month', 'year')),
  price_minor         bigint NOT NULL CHECK (price_minor >= 0),
  prices_include_tax  boolean NOT NULL,
  includes            jsonb NOT NULL DEFAULT '{}',   -- e.g. {"sms_tokens": 5000}
  effective_from      timestamptz NOT NULL,
  effective_to        timestamptz,
  UNIQUE (code, country_code, effective_from)
);

CREATE TABLE billing.subscriptions (             -- plans and recurring top-ups a client signed up for
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  kind                text NOT NULL CHECK (kind IN ('plan', 'recurring_topup')),
  plan_id             uuid REFERENCES billing.plans (id),
  topup_minor         bigint,
  status              text NOT NULL CHECK (status IN ('active', 'paused', 'cancelled')),
  started_on          date NOT NULL,
  next_bill_on        date NOT NULL,
  cancelled_at        timestamptz,
  created_by          uuid NOT NULL,
  CHECK (kind <> 'plan' OR plan_id IS NOT NULL),
  CHECK (kind <> 'recurring_topup' OR topup_minor > 0)
);
CREATE INDEX subscriptions_due ON billing.subscriptions (next_bill_on) WHERE status = 'active';

CREATE TYPE billing.asset_kind AS ENUM (
  'ussd_code', 'ussd_shared_channel', 'ussd_extension_range', 'shortcode', 'sender_id');

CREATE TABLE billing.billable_assets (           -- the register the billing run reads
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  kind                billing.asset_kind NOT NULL,
  label               text NOT NULL,           -- '*384*12#', '22384', 'ACME'
  country_code        char(2) NOT NULL,
  operator_code       text,                    -- null where one fee covers every network
  source_type         text NOT NULL,           -- application_item, ussd_code ...
  source_id           uuid NOT NULL,           -- the record in Applications or USSD
  fee_minor           bigint NOT NULL CHECK (fee_minor >= 0),
  currency            char(3) NOT NULL,
  prices_include_tax  boolean NOT NULL,
  interval            text NOT NULL DEFAULT 'month' CHECK (interval IN ('month', 'year')),
  status              text NOT NULL CHECK (status IN ('active', 'suspended', 'released')),
  billing_starts_on   date NOT NULL,           -- the day it went live after payment
  next_bill_on        date NOT NULL,
  suspended_at        timestamptz,
  released_on         date,
  version             bigint NOT NULL DEFAULT 0,
  UNIQUE (source_type, source_id)
);
CREATE INDEX billable_assets_due ON billing.billable_assets (next_bill_on) WHERE status <> 'released';

CREATE TABLE billing.usage_accruals (            -- running monthly totals from ledger usage.posted events
  tenant_id           uuid NOT NULL,
  period_month        date NOT NULL,           -- first day of the month, in the client's time zone
  service_code        text NOT NULL,
  currency            char(3) NOT NULL,
  tokens              numeric(18,6) NOT NULL DEFAULT 0,
  net_minor           bigint NOT NULL DEFAULT 0,
  tax_minor           bigint NOT NULL DEFAULT 0,
  batches             int NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, period_month, service_code, currency)
);

CREATE TABLE billing.number_series (             -- gapless numbers per country and document type
  country_code        char(2) NOT NULL,
  doc_type            text NOT NULL CHECK (doc_type IN ('invoice', 'credit_note')),
  prefix              text NOT NULL,           -- e.g. 'VKE-INV-'
  next_value          bigint NOT NULL,
  PRIMARY KEY (country_code, doc_type)
);
-- Locked with SELECT ... FOR UPDATE in the transaction that issues the document, so a rollback leaves no gap.

CREATE TYPE billing.invoice_kind AS ENUM ('recurring', 'usage', 'one_off', 'topup_request');
CREATE TYPE billing.invoice_status AS ENUM (
  'draft', 'issued', 'partially_paid', 'paid', 'overdue', 'void');

CREATE TABLE billing.invoices (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  number              text UNIQUE,             -- set at issue; drafts have none
  kind                billing.invoice_kind NOT NULL,
  status              billing.invoice_status NOT NULL DEFAULT 'draft',
  source_key          text NOT NULL,           -- e.g. 'recurring:2026-11', 'usage:2026-10'
  country_code        char(2) NOT NULL,
  currency            char(3) NOT NULL,
  period_start        date,
  period_end          date,
  subtotal_minor      bigint NOT NULL DEFAULT 0,
  tax_minor           bigint NOT NULL DEFAULT 0,
  total_minor         bigint NOT NULL DEFAULT 0,
  paid_minor          bigint NOT NULL DEFAULT 0,
  credited_minor      bigint NOT NULL DEFAULT 0,
  bill_to             jsonb NOT NULL,          -- name, address and tax PIN as they were at issue
  issued_at           timestamptz,
  due_at              timestamptz,
  paid_at             timestamptz,
  voided_at           timestamptz,
  void_reason         text,
  ledger_transaction_id uuid,                  -- the receivable posting, where there is one
  pdf_object_key      text,
  etims_status        text CHECK (etims_status IN ('not_required', 'pending', 'accepted', 'failed')),
  etims_number        text,                    -- the number KRA's control unit returns
  created_at          timestamptz NOT NULL DEFAULT now(),
  version             bigint NOT NULL DEFAULT 0,
  UNIQUE (tenant_id, source_key),              -- one invoice per client per cycle, even if the run repeats
  CHECK (paid_minor + credited_minor <= total_minor)
);
CREATE INDEX invoices_tenant ON billing.invoices (tenant_id, issued_at DESC);
CREATE INDEX invoices_open ON billing.invoices (due_at) WHERE status IN ('issued', 'partially_paid', 'overdue');

CREATE TABLE billing.invoice_lines (
  id                  uuid PRIMARY KEY,
  invoice_id          uuid NOT NULL REFERENCES billing.invoices (id),
  tenant_id           uuid NOT NULL,
  line_no             smallint NOT NULL,
  item_type           text NOT NULL CHECK (item_type IN ('asset', 'subscription', 'usage', 'one_off', 'topup')),
  item_id             uuid,                    -- billable_assets or subscriptions; null for one-off lines
  description         text NOT NULL,
  quantity            numeric(18,6) NOT NULL DEFAULT 1,
  unit_price_minor    bigint NOT NULL,
  proration           numeric(8,6) NOT NULL DEFAULT 1,   -- share of the period charged, e.g. 0.533333
  net_minor           bigint NOT NULL,
  tax_code            text,                    -- null on top-up lines
  tax_rate            numeric(6,4),
  tax_minor           bigint NOT NULL DEFAULT 0,
  service_period      daterange,
  UNIQUE (invoice_id, line_no)
);
CREATE UNIQUE INDEX invoice_lines_once ON billing.invoice_lines (item_type, item_id, service_period)
  WHERE item_type IN ('asset', 'subscription');   -- never billed twice for the same period

CREATE TABLE billing.invoice_payments (          -- how each invoice was paid, in part or in full
  id                  uuid PRIMARY KEY,
  invoice_id          uuid NOT NULL REFERENCES billing.invoices (id),
  tenant_id           uuid NOT NULL,
  source              text NOT NULL CHECK (source IN ('auto_pay_wallet', 'auto_pay_card',
                        'auto_pay_standing_order', 'pay_now', 'wallet', 'manual')),
  amount_minor        bigint NOT NULL CHECK (amount_minor > 0),
  payment_intent_id   uuid,                    -- Payments, for Pay now and card
  ledger_transaction_id uuid NOT NULL,
  paid_at             timestamptz NOT NULL,
  created_by          uuid
);

CREATE TABLE billing.credit_notes (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  invoice_id          uuid NOT NULL REFERENCES billing.invoices (id),
  number              text UNIQUE,
  reason              text NOT NULL,
  net_minor           bigint NOT NULL CHECK (net_minor > 0),
  tax_minor           bigint NOT NULL DEFAULT 0,
  reversal_id         uuid,                    -- ledger.reversals, when money goes back
  status              text NOT NULL CHECK (status IN ('draft', 'issued')),
  etims_status        text,
  etims_number        text,
  created_by          uuid NOT NULL,
  approved_by         uuid,
  issued_at           timestamptz,
  CHECK (approved_by IS NULL OR approved_by <> created_by)
);

CREATE TABLE billing.pay_links (                 -- the signed Pay now link in invoice emails
  id                  uuid PRIMARY KEY,
  invoice_id          uuid NOT NULL REFERENCES billing.invoices (id),
  tenant_id           uuid NOT NULL,
  token_hash          bytea NOT NULL UNIQUE,   -- the link carries the token; only its hash is kept
  status              text NOT NULL CHECK (status IN ('active', 'used', 'revoked', 'expired')),
  expires_at          timestamptz NOT NULL,
  last_opened_at      timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE billing.dunning_steps (
  id                  uuid PRIMARY KEY,
  invoice_id          uuid NOT NULL REFERENCES billing.invoices (id),
  tenant_id           uuid NOT NULL,
  step                text NOT NULL CHECK (step IN ('reminder_before', 'due', 'reminder_after',
                        'final_notice', 'suspend_assets')),
  scheduled_for       timestamptz NOT NULL,
  done_at             timestamptz,
  outcome             text,                    -- sent, skipped_paid, failed
  UNIQUE (invoice_id, step)
);
CREATE INDEX dunning_due ON billing.dunning_steps (scheduled_for) WHERE done_at IS NULL;

CREATE TABLE billing.etims_submissions (         -- Kenya: each invoice and credit note sent to KRA
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  document_type       text NOT NULL CHECK (document_type IN ('invoice', 'credit_note')),
  document_id         uuid NOT NULL,
  attempt             smallint NOT NULL,
  request             jsonb NOT NULL,
  response            jsonb,
  outcome             text NOT NULL CHECK (outcome IN ('pending', 'accepted', 'rejected', 'error')),
  submitted_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (document_type, document_id, attempt)
);

CREATE TABLE billing.billing_runs (              -- one daily run; safe to repeat
  id                  uuid PRIMARY KEY,
  run_date            date NOT NULL UNIQUE,
  status              text NOT NULL CHECK (status IN ('running', 'completed', 'failed')),
  invoices_created    int NOT NULL DEFAULT 0,
  errors              jsonb NOT NULL DEFAULT '[]',
  started_at          timestamptz NOT NULL DEFAULT now(),
  finished_at         timestamptz
);

CREATE TABLE billing.processed_events (          -- inbox: each consumed event is applied once
  event_id            uuid PRIMARY KEY,
  processed_at        timestamptz NOT NULL DEFAULT now()
);
```

**How each invoice kind touches the ledger**

| Invoice kind | On issue | On payment |
| --- | --- | --- |
| Recurring: codes, shortcodes, sender IDs, plans | Debit receivable; credit revenue for the service and VAT payable. For rentals the invoice is the tax point | Debit main wallet (auto-pay) or provider clearing (Pay now); credit receivable |
| One-off: setup fees, approved applications | As recurring | As recurring |
| Usage (monthly) | No posting: revenue and VAT were posted at each settlement. It is the tax document for tokens already used, so it is issued as paid | None |
| Top-up request (recurring top-up) | No posting and no VAT: a request to pay, not a tax invoice, because VAT is charged on use | Debit provider clearing; credit main wallet, like any top-up |

**Why it is built this way**

- **A rerun cannot double-bill.** Each invoice has a source\_key such as recurring:2026-11 that is unique per client, and the partial unique index on invoice\_lines stops an asset or plan being billed twice for the same period.
- **The register drives billing.** Applications (Part 3) creates the billable\_assets row when the client pays for an approved code and it goes live. Releasing the code ends it. An asset added mid-cycle is prorated on its first invoice (proposal from revision 2).
- **Suspension is per asset.** The last dunning step suspends only the unpaid assets and emits asset.suspended, so USSD and Messaging stop serving those codes. Payment reactivates them at once.
- **Issued invoices are never edited.** Corrections are credit notes with maker-checker. An invoice can be voided only before any payment and before KRA has accepted it.
- **Gapless numbering.** number\_series is locked inside the issuing transaction. KRA's own eTIMS number is stored next to VASPRO's, and failed submissions retry from etims\_submissions.
- **Usage accruals come from events.** Billing consumes the ledger's usage.posted events and records each event ID in processed\_events, so a redelivered event is never counted twice.
- **Pay now links** carry a random token. Only its hash is stored, and the link stops working once the invoice is paid, voided or expired.

**Events**

| Event | When |
| --- | --- |
| invoice.issued | Numbered, stored as PDF and sent |
| invoice.paid, invoice.partially\_paid | A payment is allocated |
| invoice.overdue | Unpaid after the due date |
| invoice.voided | Voided before any payment |
| credit\_note.issued | Issued, and accepted by eTIMS in Kenya |
| asset.billing\_started, asset.released | An asset enters or leaves the register |
| asset.suspended, asset.reactivated | Dunning suspension, and payment |
| billing.run\_completed | The daily run finished, with counts and errors |

Billing consumes usage.posted and reversal.posted from the ledger, application.paid from Applications, and payment\_intent.succeeded from Payments.

**From v3**

| v3 table or column | v4 | Notes |
| --- | --- | --- |
| invoices (description, amount, balance, quantity, status, service\_id, payment\_id, completed\_on) | invoices plus one invoice\_lines row each | Open v3 invoices come across with their unpaid balance; paid ones as read-only history. v3 invoices carry no tax lines, so imported ones are marked as pre-v4 |
| invoice\_status | invoice\_payments | One row per v3 payment reference |
| client\_billingmode | billing\_profiles.mode | Prepaid or postpaid |
| Code and sender ID fees charged through transactions (reference types 5 and 6) | billable\_assets | Created during the Applications migration (Part 3) for every live code |

## 7. Payments (payments)

Payments drives the rails for clients, but the money moves between the client's own accounts and their customers: collections land in the client's paybill or till, and payouts leave from the client's own B2C or B2B shortcode (decided 12 Oct). So this schema holds records, credentials and reconciliation, not balances. Only two flows touch VASPRO's ledger: top-ups and Pay now payments into VASPRO's own paybill, and VASPRO's fee on each payout. Airtime, data and bills are VASPRO's own products and draw on the client's airtime wallet.

&#91;embedded content: payments schema · 15 tables\]

```sql
CREATE SCHEMA payments;

CREATE TABLE payments.rails (                    -- global; mirrors the country pack
  code                text PRIMARY KEY,        -- mpesa_ke, airtel_money_ke, mpesa_tz, mtn_momo_ug ...
  country_code        char(2),
  kind                text NOT NULL CHECK (kind IN ('mobile_money', 'card', 'bank', 'aggregator', 'value_provider')),
  capabilities        text[] NOT NULL,         -- stk, c2b, b2c, b2b, reversal, status_query, balance_query, name_check
  status              text NOT NULL CHECK (status IN ('live', 'paused', 'retired'))
);

CREATE TABLE payments.rail_accounts (            -- a paybill, till or payout shortcode: the client's, or VASPRO's own
  id                  uuid PRIMARY KEY,
  tenant_id           uuid,                    -- null for VASPRO's top-up paybill
  rail_code           text NOT NULL REFERENCES payments.rails (code),
  account_type        text NOT NULL CHECK (account_type IN ('paybill', 'till', 'b2c_shortcode',
                        'b2b_shortcode', 'bank_account', 'merchant')),
  number              text NOT NULL,           -- shortcode or till; bank accounts masked
  store_number        text,                    -- the till's head office, for buy-goods STK
  display_name        text NOT NULL,           -- organisation name registered on the rail
  uses                text[] NOT NULL,         -- collections, payouts, vaspro_topups
  status              text NOT NULL CHECK (status IN ('pending_verification', 'active', 'suspended', 'removed')),
  c2b_registered_at   timestamptz,             -- confirmation URL registered with the rail
  verified_at         timestamptz,             -- a test call with the stored credentials succeeded
  last_balance_minor  bigint,                  -- from the rail's balance query
  balance_checked_at  timestamptz,
  low_balance_minor   bigint,                  -- the client's alert level for payouts
  created_by          uuid,
  created_at          timestamptz NOT NULL DEFAULT now(),
  version             bigint NOT NULL DEFAULT 0,
  UNIQUE (rail_code, account_type, number)
);

CREATE TABLE payments.rail_credentials (         -- KMS envelope-encrypted; never logged or returned by any API
  id                  uuid PRIMARY KEY,
  rail_account_id     uuid NOT NULL REFERENCES payments.rail_accounts (id),
  tenant_id           uuid,
  kind                text NOT NULL CHECK (kind IN ('app_key', 'app_secret', 'passkey',
                        'initiator_name', 'initiator_password', 'api_key', 'certificate')),
  ciphertext          bytea NOT NULL,
  kms_key_id          text NOT NULL,
  fingerprint         bytea NOT NULL,          -- keyed hash, to compare without decrypting
  status              text NOT NULL CHECK (status IN ('active', 'retired')),
  created_by          uuid NOT NULL,
  created_at          timestamptz NOT NULL DEFAULT now(),
  retired_at          timestamptz
);
CREATE UNIQUE INDEX rail_credentials_active ON payments.rail_credentials (rail_account_id, kind)
  WHERE status = 'active';
-- Rail access tokens are cached in Redis until they expire, never stored here.

CREATE TYPE payments.intent_purpose AS ENUM ('wallet_topup', 'invoice', 'client_collection', 'payment_link');
CREATE TYPE payments.intent_status AS ENUM ('created', 'pending', 'succeeded', 'failed', 'expired', 'cancelled');

CREATE TABLE payments.payment_intents (          -- one attempt to collect: STK push, card or bank
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  purpose             payments.intent_purpose NOT NULL,
  rail_account_id     uuid NOT NULL REFERENCES payments.rail_accounts (id),
                                               -- VASPRO's for top-ups and invoices; the client's for its own collections
  method              text NOT NULL CHECK (method IN ('stk_push', 'card', 'bank_transfer')),
  amount_minor        bigint NOT NULL CHECK (amount_minor > 0),
  currency            char(3) NOT NULL,
  payer_phone_e164    text,
  account_reference   text NOT NULL,
  invoice_id          uuid,                    -- Billing
  payment_link_id     uuid,
  status              payments.intent_status NOT NULL DEFAULT 'created',
  rail_request_id     text,                    -- e.g. CheckoutRequestID
  rail_receipt        text,                    -- e.g. the M-Pesa receipt
  result_code         text,                    -- the rail's own code
  error_code          text,                    -- PAY-...
  idempotency_key     text NOT NULL,
  expires_at          timestamptz NOT NULL,
  created_at          timestamptz NOT NULL DEFAULT now(),
  completed_at        timestamptz,
  UNIQUE (tenant_id, idempotency_key),
  UNIQUE (rail_account_id, rail_receipt)
);
CREATE INDEX payment_intents_pending ON payments.payment_intents (expires_at) WHERE status = 'pending';

CREATE TABLE payments.collection_notices (       -- each confirmation a rail sends for money received
  id                  uuid PRIMARY KEY,
  rail_account_id     uuid NOT NULL REFERENCES payments.rail_accounts (id),
  tenant_id           uuid,                    -- the account owner; null on VASPRO's paybill
  rail_receipt        text NOT NULL,
  transaction_type    text,                    -- PayBill, BuyGoods
  amount_minor        bigint NOT NULL,
  currency            char(3) NOT NULL,
  bill_ref            text,                    -- the account number the payer typed
  payer_phone_e164    text,                    -- personal data
  payer_name          text,                    -- personal data
  paid_at             timestamptz NOT NULL,
  account_balance_minor bigint,                -- balance the rail reports after this payment
  payment_intent_id   uuid,                    -- set when it completes an STK push
  match_status        text NOT NULL CHECK (match_status IN ('client_owned', 'matched', 'suspense', 'refunded')),
  matched_tenant_id   uuid,                    -- VASPRO's paybill: whose wallet it credited
  ledger_transaction_id uuid,                  -- VASPRO's paybill only
  raw                 jsonb NOT NULL,
  received_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (rail_account_id, rail_receipt)       -- a repeated callback is ignored
);
CREATE INDEX collection_notices_tenant ON payments.collection_notices (tenant_id, paid_at DESC);
-- Partition by month when volume needs it; deduplication then moves to a small key table.

CREATE TABLE payments.payment_links (            -- the client's own pay pages for its customers
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  rail_account_id     uuid NOT NULL REFERENCES payments.rail_accounts (id),
  token_hash          bytea NOT NULL UNIQUE,
  title               text NOT NULL,
  amount_minor        bigint,                  -- null lets the payer choose
  currency            char(3) NOT NULL,
  customer_ref        text,
  max_uses            int,
  use_count           int NOT NULL DEFAULT 0,
  status              text NOT NULL CHECK (status IN ('active', 'used_up', 'expired', 'disabled')),
  expires_at          timestamptz,
  created_by          uuid NOT NULL,
  created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TYPE payments.payout_kind AS ENUM ('b2c', 'b2b', 'bank', 'airtime', 'data', 'bill');
CREATE TYPE payments.payout_status AS ENUM ('awaiting_approval', 'approved', 'submitted',
  'pending_query', 'succeeded', 'failed', 'cancelled', 'reversed');

CREATE TABLE payments.payout_batches (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  rail_account_id     uuid REFERENCES payments.rail_accounts (id),   -- null for airtime, data and bills
  kind                payments.payout_kind NOT NULL,
  source              text NOT NULL CHECK (source IN ('api', 'file', 'dashboard')),
  file_object_key     text,
  row_count           int NOT NULL,
  valid_count         int NOT NULL DEFAULT 0,
  total_minor         bigint NOT NULL,
  currency            char(3) NOT NULL,
  fee_tokens          numeric(18,6) NOT NULL DEFAULT 0,
  status              text NOT NULL CHECK (status IN ('validating', 'awaiting_approval', 'approved',
                        'rejected', 'processing', 'completed', 'completed_with_failures', 'cancelled')),
  created_by          uuid NOT NULL,
  approved_by         uuid,
  approved_at         timestamptz,
  report_object_key   text,
  created_at          timestamptz NOT NULL DEFAULT now(),
  CHECK (approved_by IS NULL OR approved_by <> created_by)   -- maker-checker
);

CREATE TABLE payments.batch_row_errors (         -- problems shown to the client before approval
  batch_id            uuid NOT NULL REFERENCES payments.payout_batches (id),
  row_no              int NOT NULL,
  field               text NOT NULL DEFAULT '',
  error_code          text NOT NULL,           -- VAL-...
  message             text NOT NULL,
  PRIMARY KEY (batch_id, row_no, field, error_code)
);

CREATE TABLE payments.payouts (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  batch_id            uuid REFERENCES payments.payout_batches (id),
  row_no              int,                     -- position in the uploaded file
  rail_account_id     uuid REFERENCES payments.rail_accounts (id),   -- the client's shortcode it leaves from
  kind                payments.payout_kind NOT NULL,
  recipient_type      text NOT NULL CHECK (recipient_type IN ('msisdn', 'paybill', 'till',
                        'bank_account', 'meter', 'account')),
  recipient           text NOT NULL,           -- E.164 number, shortcode, account or meter
  recipient_account_ref text,                  -- the account number when paying a paybill
  provider_code       text,                    -- airtime, data and bills
  expected_name       text,
  returned_name       text,
  name_check          text CHECK (name_check IN ('match', 'mismatch', 'not_available')),
  amount_minor        bigint NOT NULL CHECK (amount_minor > 0),
  currency            char(3) NOT NULL,
  fee_tokens          numeric(18,6) NOT NULL DEFAULT 0,
  fee_hold_id         uuid,                    -- ledger hold on the payments token wallet
  value_hold_id       uuid,                    -- ledger hold on the airtime wallet; airtime, data and bills only
  status              payments.payout_status NOT NULL,
  idempotency_key     text NOT NULL,
  client_reference    text,
  narration           text,
  rail_request_id     text,                    -- e.g. ConversationID
  rail_receipt        text,
  result_code         text,
  error_code          text,                    -- PAY-...
  attempts            smallint NOT NULL DEFAULT 0,
  next_check_at       timestamptz,             -- when to query the rail before any retry
  submitted_at        timestamptz,
  completed_at        timestamptz,
  created_by          uuid,
  created_at          timestamptz NOT NULL DEFAULT now(),
  version             bigint NOT NULL DEFAULT 0,
  UNIQUE (tenant_id, idempotency_key),
  CHECK (kind IN ('airtime', 'data', 'bill') OR rail_account_id IS NOT NULL)
);
CREATE INDEX payouts_batch ON payments.payouts (batch_id, row_no);
CREATE INDEX payouts_to_check ON payments.payouts (next_check_at) WHERE status IN ('submitted', 'pending_query');
CREATE INDEX payouts_tenant ON payments.payouts (tenant_id, created_at DESC);

CREATE TABLE payments.payout_attempts (          -- every call to and from the rail, for support and disputes
  id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  payout_id           uuid NOT NULL REFERENCES payments.payouts (id),
  attempt_no          smallint NOT NULL,
  call                text NOT NULL CHECK (call IN ('submit', 'status_query', 'result_callback',
                        'timeout_callback', 'reversal')),
  request             jsonb,                   -- credentials removed before saving
  response            jsonb,
  http_status         int,
  occurred_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE payments.beneficiaries (            -- saved and allowlisted recipients
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  recipient_type      text NOT NULL,
  recipient           text NOT NULL,
  account_ref         text,
  name                text NOT NULL,
  allowlisted         boolean NOT NULL DEFAULT false,
  added_by            uuid NOT NULL,
  approved_by         uuid,                    -- allowlisting is maker-checker too
  created_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (tenant_id, recipient_type, recipient, account_ref),
  CHECK (approved_by IS NULL OR approved_by <> added_by)
);

CREATE TABLE payments.statement_imports (
  id                  uuid PRIMARY KEY,
  rail_account_id     uuid NOT NULL REFERENCES payments.rail_accounts (id),
  tenant_id           uuid,
  period_start        timestamptz NOT NULL,
  period_end          timestamptz NOT NULL,
  source              text NOT NULL CHECK (source IN ('api', 'file')),
  object_key          text,
  line_count          int NOT NULL DEFAULT 0,
  status              text NOT NULL CHECK (status IN ('importing', 'matched', 'failed')),
  imported_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (rail_account_id, period_start, period_end)
);

CREATE TABLE payments.statement_lines (
  id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  import_id           uuid NOT NULL REFERENCES payments.statement_imports (id),
  rail_receipt        text NOT NULL,
  direction           char(1) NOT NULL CHECK (direction IN ('I', 'O')),   -- in, out
  amount_minor        bigint NOT NULL,
  charges_minor       bigint NOT NULL DEFAULT 0,   -- the rail's own fee, paid by the account holder
  occurred_at         timestamptz NOT NULL,
  matched_type        text,                    -- collection_notice, payment_intent, payout
  matched_id          uuid,
  match_status        text NOT NULL CHECK (match_status IN ('matched', 'unmatched', 'ignored'))
);
CREATE INDEX statement_lines_receipt ON payments.statement_lines (rail_receipt);

CREATE TABLE payments.recon_exceptions (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid,
  rail_account_id     uuid NOT NULL REFERENCES payments.rail_accounts (id),
  kind                text NOT NULL CHECK (kind IN ('missing_in_vaspro', 'missing_on_rail',
                        'amount_mismatch', 'status_mismatch', 'duplicate')),
  statement_line_id   bigint REFERENCES payments.statement_lines (id),
  reference_type      text,
  reference_id        uuid,
  status              text NOT NULL CHECK (status IN ('open', 'resolved', 'written_off')),
  resolution          text,
  resolved_by         uuid,
  opened_at           timestamptz NOT NULL DEFAULT now(),
  resolved_at         timestamptz
);
CREATE INDEX recon_open ON payments.recon_exceptions (opened_at) WHERE status = 'open';

CREATE TABLE payments.provider_floats (          -- VASPRO's prepaid balance with each airtime and bills provider
  provider_code       text PRIMARY KEY,
  country_code        char(2) NOT NULL,
  currency            char(3) NOT NULL,
  balance_minor       bigint,
  low_level_minor     bigint NOT NULL,
  checked_at          timestamptz
);

-- Plus the shared outbox_events, idempotency_keys and processed_events tables.
```

> **Agreed for Phase 4 (11 Nov 2026, OQ-22), built from task 4.1.** Additions to the schema above:
> - **`payments.topup_accounts`** (`tenant_id`, `rail_account_id`, `account_number`, unique per rail account): each client's account number on VASPRO's paybill, 8 characters with 2 check characters.
> - **`payments.stk_callbacks`**: every STK result callback, unique by `CheckoutRequestID`, so a repeat is ignored even for a failure (which has no receipt), and one that arrives before the intent's `rail_request_id` is saved waits here until it is.
> - **`recon_exceptions.kind`** gains `ledger_mismatch`: a notice with no provider-clearing entry, or an entry with no notice.
> - **Roles:** NOLOGIN `payments_system` (rows with no tenant: VASPRO's paybill, its credentials, unmatched notices) and `payments_operations` (staff: suspense and reconciliation), both `WITH INHERIT FALSE`, used only through `WithRoleTx`.
> - **Callbacks** are checked by source (Safaricom's published ranges) and a path token, HMAC(secret, rail account ID), since Daraja doesn't sign them.
> - **Events** besides those below: `collection_notice.matched` (a staff match from suspense) and `recon.statement_imported`.
>
> **Built in task 4.1 (11 Nov 2026).** Phase 4's tables only; payment links, payouts, batches, beneficiaries and provider floats come with Phase 13. Further additions:
> - **CHECKs:** `rails.capabilities` within §7's list; `rail_accounts.uses` non-empty and within its three names, `vaspro_topups` only with no tenant, `active` only once `verified_at` is set; a credential is `retired` exactly when it has `retired_at`; an intent's currency is three capitals, an STK push has a phone, an invoice or link payment names it, `succeeded` has a receipt, `failed` an error code, a final status (`succeeded`, `failed`, `expired`, `cancelled`) exactly when `completed_at` is set, and it expires after it's created; a notice's amount is positive, a client's own collection is owned and unmatched, a match names its client, suspense has no client, and a notice is never both owned and matched; a statement period ends after it starts; statement lines have non-negative amounts and name what they matched exactly when matched; an exception is open exactly when it has no resolution, resolver or time.
> - **Indexes:** a unique `(rail_account_id, rail_request_id)` on intents, for callbacks; the suspense queue on notices; unapplied STK callbacks.
> - **Grants:** the service role `payments_service` updates only named columns (an intent's status and rail results, a notice's match and ledger posting, a credential's retirement, an account's verification and balance), so an amount, receipt, tenant or ciphertext is never rewritten, and deletes nothing but idempotency keys. `payments_operations` has no grant on credentials.
> - **RLS:** standard on intents and account numbers (with reads across tenants for `payments_system` and staff); `rail_accounts` like the ledger's accounts (a tenant reads its own and VASPRO's); credentials own-only, VASPRO's for `payments_system` alone; notices by `tenant_id` or `matched_tenant_id`, a tenant inserting only a top-up matched to itself, VASPRO's paybill rows for `payments_system`; statements and exceptions own-only, VASPRO's for `payments_system`, all readable by staff, who resolve exceptions; statement lines follow their import; STK callbacks `payments_system` only.
>
> **Built in task 4.3 (11 Nov 2026, OQ-22).** No schema change.
> - **Rails** mirror the country packs' new `rails` block at startup; one the packs no longer list is retired, never deleted.
> - **Credentials** of VASPRO's paybill are entered by staff through the ops CLI (`vaspro payments`), `created_by` the staff member found by email. Storing one retires the one it replaces **and returns an active account to `pending_verification`** until a test call (a fresh OAuth fetch) works again with the new set; only then is it `active` (`rail_account.verified`).
> - **Account numbers** are issued on first use: 6 random characters and 2 check characters (Σ (i+1)·vᵢ mod 1021), which catch every single-character typo and every swap of neighbours. Only an active paybill is ever offered.
>
> **Built in task 4.4 (11 Nov 2026, OQ-22).** No schema change.
> - **STK top-ups:** the intent is recorded `created` before the push and `pending` with the `CheckoutRequestID` after it. A callback is recorded in `stk_callbacks` first and applied once the intent is found, so one that beats the push's response isn't lost.
> - **A confirmed payment** becomes a `matched` notice (the rail's amount, not the intent's) and one ledger `topup`, keyed `topup:<notice id>` and referencing the notice.
> - **Exceptions about a client's top-up** (`amount_mismatch`, `status_mismatch`) carry that client's `tenant_id` and reference the intent; reconciliation's own exceptions on VASPRO's paybill (4.6) have none.
> - **A success after a reported failure or expiry** is still credited, and flagged. **A payment reported only by STK query** (no receipt, no amount) is never credited: it's flagged once and waits for a confirmation.

**Where the money goes**

| Flow | Rail account | Ledger |
| --- | --- | --- |
| Wallet top-up, or Pay now on a VASPRO invoice | VASPRO's paybill | Debit provider clearing; credit main wallet, or receivable for an invoice. An unmatched payment goes to suspense |
| Client collection: STK push, C2B, payment link | The client's paybill or till | None. Recorded, sent to the client as a signed payment.received webhook, and reconciled |
| B2C, B2B or bank payout | The client's own shortcode or bank account | Only VASPRO's fee: a hold on the payments token wallet at approval, captured on success through the usual usage batch |
| Airtime, data or a bill | VASPRO's provider float | A hold on the airtime wallet; each success posts its own transaction |

**Why it is built this way**

- **Credentials are envelope-encrypted** with KMS and decrypted only inside the rail adapter at call time. The API never returns them. Rotating adds a new active row and retires the old one, and an account turns active only after a test call works. v3 keeps several clients' Daraja keys, passkeys and initiator passwords in its source code; none are migrated from there. Each client re-enters them, and the old ones are revoked with Safaricom.
- **No double payouts.** The idempotency key is unique per client, and a rail receipt is unique per account. If a submit times out, the payout moves to pending\_query and the rail is asked for the outcome before any retry.
- **Fees are tokens.** A rate card sets how many payments tokens each payout kind uses, and the client's price per token, list or negotiated in `ledger.token_prices`, turns that into money. The rail's own charges hit the client's account directly and show up in statement\_lines.charges\_minor.
- **Row-level security with VASPRO's paybill.** Notices on VASPRO's paybill have no owner until matched, so their policy also checks matched\_tenant\_id. Staff reach unmatched ones through a separate operations role.
- **Reconciliation runs daily per rail account.** For client accounts it proves that every payout and collection VASPRO relayed is real. For VASPRO's paybill it also checks provider clearing in the ledger.
- **Balances on client shortcodes** are polled within the rail's limits, so the dashboard can warn before a payout fails and section 10 of the design doc can alert on a low balance.

**Events**

| Event | When |
| --- | --- |
| payment\_intent.succeeded, payment\_intent.failed | An STK push, card or bank payment ends (expired counts as failed) |
| payment.received | Money lands in a client's paybill or till |
| topup.unmatched | A payment to VASPRO's paybill could not be matched and went to suspense |
| payout\_batch.awaiting\_approval, payout\_batch.completed | Batch lifecycle |
| payout.completed, payout.failed | The rail's final result, with its receipt or PAY- code |
| rail\_account.verified, rail\_account.balance\_low | Account checks |
| recon.exception\_opened | A mismatch between a rail statement and VASPRO's records |

**From v3**

| v3 table or column | v4 | Notes |
| --- | --- | --- |
| client\_mpesa\_paybill, client\_paybill (paybill, store\_number, type\_id, organisation\_name, org\_acc\_balance) | rail\_accounts | type\_id becomes account\_type; org\_acc\_balance becomes last\_balance\_minor |
| client\_mpesa\_paybill.access\_token, expires\_on | Redis cache | Not migrated |
| client\_mpesa\_paybill.pushUrl | Webhook endpoint (Part 4) | Notices go out as signed webhooks |
| client\_mpesa\_paybill.markup\_amount, bulk\_payment\_settings.rate | ledger.token\_prices for the payments service, per client | VASPRO's fee per transaction |
| Daraja keys, passkeys and initiators in MpesaTools.php | rail\_credentials | Re-entered by each client; the exposed ones are revoked |
| bulk\_payment\_settings (merchant\_code, auth\_type, approved\_ips, max\_alert\_amount) | rail\_accounts, client limits (Part 3), API client IP allowlist, low\_balance\_minor | One v3 table splits four ways |
| mpesa\_transactions, mpesa\_transactions\_failed | collection\_notices | Imported read-only; history depth to agree |
| mpesa\_transactions\_summary | Not migrated | Reports query notices directly |
| mpesa\_checkout, mpesa\_checkout\_api\_request | payment\_intents | CheckoutRequestID becomes rail\_request\_id |
| bulk\_payment\_request | payout\_batches | v3 records VAT on its fee as rate × 0.016, which is 1.6%, not 16%; to confirm |
| bulk\_payment\_transaction, mpesa\_withdrawal\_dlr | payouts, payout\_attempts |  |
| b2b\_transfer, b2b\_transfer\_response | payouts with kind b2b |  |
| client\_b2b\_constraint | beneficiaries, allowlisted |  |
| utilities\_transactions | payouts with kind airtime, data or bill |  |
| utility\_provider\_float | provider\_floats |  |

## 8. Next parts and open points

| Part | Services | Why in this order |
| --- | --- | --- |
| 2 | Billing and invoicing, Payments | Done: sections 6 and 7 |
| 3 | Messaging, USSD, Contacts and consent, Applications | The core traffic, including the ClickHouse event tables and the ops-ticket workflow |
| 4 | Verify, Journeys, Surveys and rewards, Webhooks, Monitoring, sales and referrals | Built on top of Parts 1 to 3 |

**Open points raised by Part 1**

- [ ] **Prices with or without VAT:** should rate cards include VAT or add it on top, per country? The model supports both; it changes the hold amount and how invoices read.
- [ ] **eTIMS for usage:** one consolidated invoice per month for usage, or one per settlement batch? To confirm with a tax adviser against KRA's rules.
- [ ] **Settlement window length:** for example 5 minutes or 1 hour. Shorter keeps balances fresher; longer writes fewer journal rows.
- [ ] **Where holds live at peak:** PostgreSQL rows as designed, or Redis with rows written in the background. Decide after load testing.
- [ ] **v3 `subscription_balance`:** confirm it is premium revenue owed to clients.
- [ ] **Old v3 API tokens:** accept them through a compatibility route during migration, and if so for how long?
- [ ] **Token expiry and refunds** (open since revision 2): `token_lots.expires_at` is ready if needed.

**Open points raised by Part 2**

- **Who sends payouts today:** v3 writes bulk\_payment\_transaction rows with a processed flag, and something outside v3 sends them. Is that a separate worker to keep, like the SMS engine, or does v4's Payments service call the rails itself, as modelled?
- **VAT on payout fees:** v3 records it as 1.6% of the fee. Confirm it should be 16%, and whether past invoices need correcting.
- **Bank payouts:** from the client's own bank account through which banks' APIs, or through an aggregator account held in the client's name?
- **Billing day and proration** (open since revision 2): one day per client, as modelled with the 1st as default, or one day for everyone?
- **Recurring top-ups:** issue them as a request to pay without VAT, as modelled, rather than as a tax invoice. To confirm with the tax adviser.
- **History depth:** how many months of v3 collections and payouts to import.
- **Balance checks:** how often to query the balance of each client's payout shortcode, within the rail's limits.
