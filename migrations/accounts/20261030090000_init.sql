-- +goose Up

-- Accounts and KYC (data model §4): a tenant is one client account.
-- Accounts records who belongs to it and with what role, which
-- services it chose at onboarding, and every verification that moved
-- it up a tier. Resellers are tenants whose clients are sub-account
-- tenants underneath them. Tables and indexes are copied exactly from
-- data model §4; the RLS for tenants and roles (below) is this task's
-- own, since §4 doesn't spell it out for tables without a plain
-- tenant_id NOT NULL.

CREATE SCHEMA accounts;

-- tenants.billing_email is citext. Database-wide extension, left in
-- place on Down (identity depends on it too).
CREATE EXTENSION IF NOT EXISTS citext;

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

-- Row-level security. The standard tenant_isolation policy (data model
-- §1) on every table with tenant_id NOT NULL. services,
-- service_country_rules and tier_caps are global and have none.

ALTER TABLE accounts.memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts.memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounts.memberships
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE accounts.tenant_services ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts.tenant_services FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounts.tenant_services
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE accounts.business_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts.business_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounts.business_profiles
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE accounts.beneficial_owners ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts.beneficial_owners FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounts.beneficial_owners
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE accounts.documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts.documents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounts.documents
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE accounts.kyc_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts.kyc_checks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounts.kyc_checks
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE accounts.tier_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts.tier_changes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounts.tier_changes
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- tenants has no tenant_id: the row's own id is the tenant. A tenant
-- sees itself and, if it is a reseller, its sub-accounts. Creating a
-- tenant runs with app.tenant_id set to the new id (or to the reseller,
-- for a sub-account). The cross-tenant "which accounts am I in?" lookup
-- gets its own narrow role when task 2.3 needs it.
ALTER TABLE accounts.tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts.tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounts.tenants
  USING      (id = nullif(current_setting('app.tenant_id', true), '')::uuid
              OR parent_tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (id = nullif(current_setting('app.tenant_id', true), '')::uuid
              OR parent_tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- roles.tenant_id is nullable: NULL is a built-in role, readable by
-- every tenant but writable by none (built-ins are seeded by migration).
ALTER TABLE accounts.roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts.roles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounts.roles
  USING      (tenant_id IS NULL
              OR tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Every schema gets outbox_events and idempotency_keys (data model §1).
-- accounts consumes no events yet (tenants are created by an explicit
-- onboarding call, not from identity's events), so there's no
-- processed_events table — add one when that changes.

CREATE TABLE accounts.outbox_events (
  id            uuid PRIMARY KEY,
  tenant_id     uuid,
  aggregate     text NOT NULL,
  aggregate_id  uuid NOT NULL,
  event_type    text NOT NULL,
  payload       jsonb NOT NULL,
  occurred_at   timestamptz NOT NULL DEFAULT now(),
  published_at  timestamptz
);
CREATE INDEX accounts_outbox_unpublished ON accounts.outbox_events (occurred_at) WHERE published_at IS NULL;

CREATE TABLE accounts.idempotency_keys (
  tenant_id     uuid NOT NULL,
  key           text NOT NULL,
  request_hash  bytea NOT NULL,
  status_code   int,
  response      jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, key)
);

-- +goose Down
DROP TABLE accounts.idempotency_keys;
DROP TABLE accounts.outbox_events;
DROP TABLE accounts.tier_changes;
DROP TABLE accounts.kyc_checks;
DROP TABLE accounts.documents;
DROP TABLE accounts.beneficial_owners;
DROP TABLE accounts.business_profiles;
DROP TABLE accounts.tier_caps;
DROP TABLE accounts.tenant_services;
DROP TABLE accounts.service_country_rules;
DROP TABLE accounts.services;
DROP TABLE accounts.memberships;
DROP TABLE accounts.roles;
DROP TABLE accounts.tenants;
DROP TYPE accounts.tenant_type;
DROP TYPE accounts.tier;
DROP SCHEMA accounts;
