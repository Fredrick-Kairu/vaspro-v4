-- +goose Up

-- Task 6.1: the contacts schema, copied from data model §10 (OQ-26).
-- imports and import_errors are copied from §8 exactly (D-23, ADR 0012).

CREATE EXTENSION IF NOT EXISTS citext;  -- shared; never dropped by Down

CREATE SCHEMA contacts;

CREATE TABLE contacts.contacts (
  id            uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL,
  phone_e164    text NOT NULL CHECK (phone_e164 ~ '^\+[1-9][0-9]{6,14}$'),
  first_name    text CHECK (char_length(first_name) <= 100),
  last_name     text CHECK (char_length(last_name) <= 100),
  email         citext CHECK (char_length(email) <= 254),
  attributes    jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(attributes) = 'object'),
  source        text NOT NULL CHECK (source IN ('api', 'dashboard', 'import', 'v3')),
  created_by    uuid,                          -- NULL for the v3 import
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  version       int NOT NULL DEFAULT 1,
  UNIQUE (tenant_id, phone_e164),
  UNIQUE (id, tenant_id)                       -- children reference (id, tenant_id), so they share its tenant
);
CREATE INDEX contacts_tenant_created ON contacts.contacts (tenant_id, created_at DESC, id DESC);
CREATE INDEX contacts_attributes ON contacts.contacts USING gin (attributes jsonb_path_ops);

CREATE TABLE contacts.attribute_definitions (   -- at most 50 per client
  tenant_id   uuid NOT NULL,
  key         text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_]{0,39}$'),
  label       text NOT NULL CHECK (char_length(label) BETWEEN 1 AND 100),
  type        text NOT NULL CHECK (type IN ('text', 'number', 'date', 'boolean')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, key)
);                                               -- a text value is at most 255 characters; numbers are decimal strings

CREATE TABLE contacts.groups (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL,
  name        text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
  description text CHECK (char_length(description) <= 500),
  created_by  uuid,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  version     int NOT NULL DEFAULT 1,
  UNIQUE (id, tenant_id)
);
CREATE UNIQUE INDEX groups_name ON contacts.groups (tenant_id, lower(name));

CREATE TABLE contacts.group_members (
  tenant_id   uuid NOT NULL,
  group_id    uuid NOT NULL,
  contact_id  uuid NOT NULL,
  added_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (group_id, contact_id),
  FOREIGN KEY (group_id, tenant_id) REFERENCES contacts.groups (id, tenant_id) ON DELETE CASCADE,
  FOREIGN KEY (contact_id, tenant_id) REFERENCES contacts.contacts (id, tenant_id) ON DELETE CASCADE
);
CREATE INDEX group_members_contact ON contacts.group_members (tenant_id, contact_id);

CREATE TABLE contacts.segments (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL,
  name        text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
  filter      jsonb NOT NULL CHECK (jsonb_typeof(filter) = 'object'),
  created_by  uuid,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  version     int NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX segments_name ON contacts.segments (tenant_id, lower(name));

CREATE TYPE contacts.channel AS ENUM ('sms', 'whatsapp', 'email', 'telegram', 'rcs');
CREATE TYPE contacts.purpose AS ENUM ('transactional', 'promotional');

CREATE TABLE contacts.consents (                 -- the current state; consent_events is the history
  tenant_id    uuid NOT NULL,
  phone_e164   text NOT NULL CHECK (phone_e164 ~ '^\+[1-9][0-9]{6,14}$'),
  channel      contacts.channel NOT NULL,
  purpose      contacts.purpose NOT NULL,
  status       text NOT NULL CHECK (status IN ('granted', 'withdrawn')),
  actor        text NOT NULL CHECK (actor IN ('client', 'subject')),
  source       text NOT NULL CHECK (source IN ('api', 'dashboard', 'keyword', 'preference_centre', 'v3')),
  evidence     text CHECK (char_length(evidence) <= 500),   -- how the client obtained it, when the client records it
  recorded_by  uuid,                            -- the client's user; NULL when the person acted
  recorded_at  timestamptz NOT NULL,
  version      int NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, phone_e164, channel, purpose),
  CHECK ((actor = 'subject') = (source IN ('keyword', 'preference_centre'))),
  CHECK ((actor = 'client') = (recorded_by IS NOT NULL) OR source = 'v3')
);

CREATE TABLE contacts.consent_events (           -- append-only
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL,
  phone_e164   text NOT NULL,
  channel      contacts.channel NOT NULL,
  purpose      contacts.purpose NOT NULL,
  status       text NOT NULL CHECK (status IN ('granted', 'withdrawn')),
  actor        text NOT NULL CHECK (actor IN ('client', 'subject')),
  source       text NOT NULL,
  evidence     text,
  recorded_by  uuid,
  occurred_at  timestamptz NOT NULL
);
CREATE INDEX consent_events_number ON contacts.consent_events (tenant_id, phone_e164, occurred_at DESC);

CREATE TABLE contacts.suppressions (             -- the client's own do-not-contact list
  tenant_id    uuid NOT NULL,
  phone_e164   text NOT NULL CHECK (phone_e164 ~ '^\+[1-9][0-9]{6,14}$'),
  channel      contacts.channel,                -- NULL: every channel
  reason       text NOT NULL CHECK (reason IN ('client_request', 'complaint', 'invalid', 'other')),
  note         text CHECK (char_length(note) <= 500),
  created_by   uuid NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (tenant_id, phone_e164, channel)
);

CREATE TABLE contacts.preference_links (
  tenant_id     uuid NOT NULL,
  phone_e164    text NOT NULL CHECK (phone_e164 ~ '^\+[1-9][0-9]{6,14}$'),
  token_hash    bytea NOT NULL UNIQUE,          -- SHA-256 of the 12-character token
  created_at    timestamptz NOT NULL,
  expires_at    timestamptz NOT NULL,
  last_used_at  timestamptz,
  PRIMARY KEY (tenant_id, phone_e164),
  CHECK (expires_at > created_at)
);


-- §8: uploaded files and retention.
-- contacts.imports carries the columns every upload table uses. Payments adds the same
-- file columns to payments.payout_batches in Phase 13, with retention_class 'financial'.
CREATE TYPE contacts.import_status AS ENUM
  ('awaiting_upload', 'scanning', 'validating', 'processing',
   'completed', 'completed_with_errors', 'failed', 'cancelled', 'expired');

CREATE TABLE contacts.imports (
  id                 uuid PRIMARY KEY,
  tenant_id          uuid NOT NULL,
  group_id           uuid,                         -- add the imported contacts to this group, if set
  status             contacts.import_status NOT NULL DEFAULT 'awaiting_upload',
  file_name          text NOT NULL,                -- shown back to the client; never used in a path
  content_type       text NOT NULL CHECK (content_type IN ('text/csv',
                       'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet')),
  size_bytes         bigint CHECK (size_bytes > 0),  -- known after upload; capped by config
  sha256             bytea,                        -- of the plaintext
  object_key         text NOT NULL UNIQUE,         -- uploads/transient/<tenant>/<id>
  error_report_key   text,                         -- rejected rows as CSV; same retention as the upload
  retention_class    text NOT NULL DEFAULT 'transient' CHECK (retention_class IN ('transient', 'financial')),
  rows_total         int,
  rows_valid         int NOT NULL DEFAULT 0,
  rows_failed        int NOT NULL DEFAULT 0,
  upload_expires_at  timestamptz NOT NULL,         -- end of the signed upload link
  finished_at        timestamptz,                  -- reached a final status
  delete_after       timestamptz NOT NULL,         -- set at creation, reset at the final status
  deleted_at         timestamptz,                  -- files removed; the row and its results stay
  created_by         uuid NOT NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  version            bigint NOT NULL DEFAULT 0,
  CHECK ((finished_at IS NULL) = (status IN ('awaiting_upload', 'scanning', 'validating', 'processing')))
);
CREATE INDEX imports_tenant ON contacts.imports (tenant_id, created_at DESC);
CREATE INDEX imports_to_purge ON contacts.imports (delete_after) WHERE deleted_at IS NULL;

CREATE TABLE contacts.import_errors (             -- one row per problem; no file needed to show them
  import_id   uuid NOT NULL REFERENCES contacts.imports (id),
  tenant_id   uuid NOT NULL,
  row_no      int NOT NULL,
  field       text NOT NULL DEFAULT '',
  error_code  text NOT NULL,                       -- VAL-...
  message     text NOT NULL,                       -- names the problem; never repeats the row's values
  PRIMARY KEY (import_id, row_no, field, error_code)
);

-- Every schema carries these (data model §1). contacts consumes no
-- events yet, so no processed_events.
CREATE TABLE contacts.outbox_events (
  id            uuid PRIMARY KEY,
  tenant_id     uuid,
  aggregate     text NOT NULL,
  aggregate_id  uuid NOT NULL,
  event_type    text NOT NULL,
  payload       jsonb NOT NULL,
  occurred_at   timestamptz NOT NULL DEFAULT now(),
  published_at  timestamptz
);
CREATE INDEX outbox_unpublished ON contacts.outbox_events (occurred_at) WHERE published_at IS NULL;

CREATE TABLE contacts.idempotency_keys (
  tenant_id     uuid NOT NULL,
  key           text NOT NULL,
  request_hash  bytea NOT NULL,
  status_code   int,
  response      jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, key)
);

-- The person's own withdrawal can only be undone by the person (data
-- model §10): the client's API, an import or a v3 load can't overwrite
-- it, whatever path the write takes.
-- +goose StatementBegin
CREATE FUNCTION contacts.subject_withdrawal_sticks() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'withdrawn' AND OLD.actor = 'subject' AND NEW.actor <> 'subject' THEN
    RAISE EXCEPTION 'consent withdrawn by the person can only be changed by the person'
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER consents_subject_withdrawal_sticks
  BEFORE UPDATE ON contacts.consents
  FOR EACH ROW EXECUTE FUNCTION contacts.subject_withdrawal_sticks();

-- Roles (the webhooks pattern). contacts_service holds the service's
-- privileges; its login role is granted it (with inheritance) and
-- doesn't own the tables, so these grants are a real limit. Updates are
-- by column: a contact's tenant and number, a consent's key, an import's
-- object key and file name are never rewritten. Consent history and
-- import errors are insert-only.
--
-- contacts_system claims imports and sweeps their files across clients,
-- and finds a preference link by its hash. It sees nothing else. It's
-- NOLOGIN, granted WITH INHERIT FALSE so it applies only inside
-- db.Pool.WithRoleTx (1.8's lesson).
CREATE ROLE contacts_service NOLOGIN;
CREATE ROLE contacts_system NOLOGIN;
GRANT contacts_system TO contacts_service WITH INHERIT FALSE;
GRANT contacts_system TO CURRENT_USER WITH INHERIT FALSE;

GRANT USAGE ON SCHEMA contacts TO contacts_service, contacts_system;
GRANT SELECT ON ALL TABLES IN SCHEMA contacts TO contacts_service;
GRANT INSERT ON ALL TABLES IN SCHEMA contacts TO contacts_service;
GRANT DELETE ON contacts.contacts, contacts.attribute_definitions, contacts.groups,
  contacts.group_members, contacts.segments, contacts.suppressions, contacts.idempotency_keys
  TO contacts_service;
GRANT UPDATE (first_name, last_name, email, attributes, updated_at, version) ON contacts.contacts TO contacts_service;
GRANT UPDATE (label) ON contacts.attribute_definitions TO contacts_service;
GRANT UPDATE (name, description, updated_at, version) ON contacts.groups TO contacts_service;
GRANT UPDATE (name, filter, updated_at, version) ON contacts.segments TO contacts_service;
GRANT UPDATE (status, actor, source, evidence, recorded_by, recorded_at, version) ON contacts.consents TO contacts_service;
GRANT UPDATE (token_hash, created_at, expires_at, last_used_at) ON contacts.preference_links TO contacts_service;
GRANT UPDATE (status, size_bytes, sha256, error_report_key, rows_total, rows_valid, rows_failed,
  finished_at, delete_after, deleted_at, version)
  ON contacts.imports TO contacts_service;
GRANT UPDATE ON contacts.outbox_events, contacts.idempotency_keys TO contacts_service;

GRANT SELECT ON contacts.imports, contacts.preference_links TO contacts_system;
GRANT UPDATE (status, finished_at, delete_after, deleted_at, version) ON contacts.imports TO contacts_system;
GRANT UPDATE (last_used_at) ON contacts.preference_links TO contacts_system;
GRANT INSERT ON contacts.outbox_events TO contacts_system;

-- Row-level security: the standard policy on every client table (data
-- model §1), plus contacts_system's reach across clients.
ALTER TABLE contacts.contacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.contacts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.contacts
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE contacts.attribute_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.attribute_definitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.attribute_definitions
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE contacts.groups ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.groups FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.groups
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE contacts.group_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.group_members FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.group_members
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE contacts.segments ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.segments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.segments
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE contacts.consents ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.consents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.consents
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE contacts.consent_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.consent_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.consent_events
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE contacts.suppressions ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.suppressions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.suppressions
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE contacts.preference_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.preference_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.preference_links
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_read ON contacts.preference_links FOR SELECT TO contacts_system USING (true);
CREATE POLICY system_update ON contacts.preference_links FOR UPDATE TO contacts_system USING (true) WITH CHECK (true);

ALTER TABLE contacts.imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.imports FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.imports
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_read ON contacts.imports FOR SELECT TO contacts_system USING (true);
CREATE POLICY system_update ON contacts.imports FOR UPDATE TO contacts_system USING (true) WITH CHECK (true);

ALTER TABLE contacts.import_errors ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts.import_errors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON contacts.import_errors
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- +goose Down
DROP TABLE contacts.idempotency_keys;
DROP TABLE contacts.outbox_events;
DROP TABLE contacts.import_errors;
DROP TABLE contacts.imports;
DROP TYPE contacts.import_status;
DROP TABLE contacts.preference_links;
DROP TABLE contacts.suppressions;
DROP TABLE contacts.consent_events;
DROP TABLE contacts.consents;
DROP FUNCTION contacts.subject_withdrawal_sticks();
DROP TYPE contacts.purpose;
DROP TYPE contacts.channel;
DROP TABLE contacts.segments;
DROP TABLE contacts.group_members;
DROP TABLE contacts.groups;
DROP TABLE contacts.attribute_definitions;
DROP TABLE contacts.contacts;
DROP SCHEMA contacts;
DROP ROLE contacts_system;
DROP ROLE contacts_service;
