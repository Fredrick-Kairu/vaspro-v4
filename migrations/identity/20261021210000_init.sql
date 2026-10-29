-- +goose Up

-- Identity and access (data model §3): identity knows who a person or
-- machine is and nothing about what they may do inside an account. A
-- user is one verified email address; a phone number is optional
-- profile data. Users, passkeys, TOTP and verification tokens have no
-- tenant_id at all — they're global, not a tenant-owned resource.

CREATE SCHEMA identity;

-- citext is a database-wide extension, not schema-scoped — left in
-- place on Down (below) since dropping it could affect another
-- schema that also depends on it.
CREATE EXTENSION IF NOT EXISTS citext;

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
  password_hash      text,                           -- Argon2id; null for passkey-only users
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

-- sessions and security_events carry tenant_id as which account is
-- currently selected / which account an event happened under, not as
-- row ownership — nullable, no RLS (data model §3: "account currently
-- selected"; a user or event can span tenants across its lifetime).

CREATE TABLE identity.sessions (
  id            uuid PRIMARY KEY,
  user_id       uuid NOT NULL REFERENCES identity.users (id),
  tenant_id     uuid,                                -- account currently selected
  ip            inet,
  user_agent    text,
  step_up_at    timestamptz,                         -- last fresh MFA, for sensitive actions
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  revoked_at    timestamptz
);
CREATE INDEX sessions_active ON identity.sessions (user_id) WHERE revoked_at IS NULL;

-- Partitioned by month per data model §3's comment. Only a DEFAULT
-- partition is created here so inserts work from day one; creating and
-- rotating monthly partitions is an operational job, not part of this
-- migration (docs/STATUS.md tracks it as a follow-up).
CREATE TABLE identity.security_events (
  id           uuid NOT NULL,
  user_id      uuid,
  tenant_id    uuid,
  type         text NOT NULL,                        -- sign_in, sign_in_failed, mfa_enrolled, secret_revealed ...
  ip           inet,
  detail       jsonb NOT NULL DEFAULT '{}',
  occurred_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);
CREATE TABLE identity.security_events_default PARTITION OF identity.security_events DEFAULT;

-- api_clients, smpp_accounts and sso_connections are tenant-owned
-- resources (tenant_id NOT NULL) — full RLS, the canonical pattern
-- (.claude/rules/migrations.md): nullif(..., '') rather than the bare
-- cast, which errors instead of failing closed once a pooled connection
-- has run a prior WithTenantTx.

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
ALTER TABLE identity.api_clients ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.api_clients FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON identity.api_clients
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- api_client_secrets has no tenant_id in the data model doc at all —
-- see docs/decisions/open-questions.md. Followed exactly as specified
-- rather than adding a column the design doesn't have; no RLS here.
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
ALTER TABLE identity.smpp_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.smpp_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON identity.smpp_accounts
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

CREATE TABLE identity.sso_connections (
  id         uuid PRIMARY KEY,
  tenant_id  uuid NOT NULL,
  protocol   text NOT NULL CHECK (protocol IN ('saml', 'oidc')),
  domain     citext NOT NULL UNIQUE,                 -- users at this domain sign in by SSO
  config     jsonb NOT NULL,                         -- metadata URL, client ID; secrets in KMS
  enabled    boolean NOT NULL DEFAULT true
);
ALTER TABLE identity.sso_connections ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity.sso_connections FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON identity.sso_connections
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- signing_keys is global: published as the JWKS. No tenant_id, no RLS.
CREATE TABLE identity.signing_keys (
  kid          text PRIMARY KEY,
  algorithm    text NOT NULL,                        -- EdDSA
  public_key   bytea NOT NULL,
  private_ref  text NOT NULL,                        -- KMS reference, never the key itself
  status       text NOT NULL CHECK (status IN ('active', 'retiring', 'retired')),
  created_at   timestamptz NOT NULL DEFAULT now()
);

-- Every schema gets outbox_events and idempotency_keys (data model §1).
-- identity publishes events (user.registered and siblings, §3's Events
-- table) and every write endpoint Phase 1 adds needs an idempotency
-- home. identity doesn't consume any other schema's events yet, so no
-- processed_events table — add one if and when that changes.

CREATE TABLE identity.outbox_events (
  id           uuid PRIMARY KEY,
  tenant_id    uuid,
  aggregate    text NOT NULL,
  aggregate_id uuid NOT NULL,
  event_type   text NOT NULL,
  payload      jsonb NOT NULL,
  occurred_at  timestamptz NOT NULL DEFAULT now(),
  published_at timestamptz
);
CREATE INDEX identity_outbox_unpublished ON identity.outbox_events (occurred_at) WHERE published_at IS NULL;

CREATE TABLE identity.idempotency_keys (
  tenant_id    uuid NOT NULL,
  key          text NOT NULL,
  request_hash bytea NOT NULL,
  status_code  int,
  response     jsonb,
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, key)
);

-- +goose Down
DROP TABLE identity.idempotency_keys;
DROP TABLE identity.outbox_events;
DROP TABLE identity.signing_keys;
DROP POLICY tenant_isolation ON identity.sso_connections;
DROP TABLE identity.sso_connections;
DROP POLICY tenant_isolation ON identity.smpp_accounts;
DROP TABLE identity.smpp_accounts;
DROP TABLE identity.api_client_secrets;
DROP POLICY tenant_isolation ON identity.api_clients;
DROP TABLE identity.api_clients;
DROP TABLE identity.security_events_default;
DROP TABLE identity.security_events;
DROP TABLE identity.sessions;
DROP TABLE identity.verification_tokens;
DROP TABLE identity.user_totp;
DROP TABLE identity.user_passkeys;
DROP TABLE identity.users;
DROP TYPE identity.token_purpose;
DROP TYPE identity.environment;
DROP TYPE identity.user_status;
DROP SCHEMA identity;
