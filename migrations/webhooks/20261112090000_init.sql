-- +goose Up

-- Task 5.1: the webhooks schema, copied from data model §8 (OQ-23).
-- ClickHouse's events table comes with 5.6, in its own migrations.

CREATE SCHEMA webhooks;

CREATE TABLE webhooks.endpoints (
  id               uuid PRIMARY KEY,
  tenant_id        uuid NOT NULL,
  url              text NOT NULL,            -- https; the address is checked again at every connection
  description      text,
  status           text NOT NULL DEFAULT 'pending_verification'
                   CHECK (status IN ('pending_verification', 'active', 'disabled')),
  verified_at      timestamptz,              -- the last passed challenge; cleared when the URL changes
  failing_since    timestamptz,              -- the first failed attempt since the last success; NULL when healthy
  disabled_at      timestamptz,
  disabled_reason  text CHECK (disabled_reason IN ('failing', 'client')),
  created_by       uuid NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  deleted_at       timestamptz,              -- kept so the delivery log still names it
  version          int NOT NULL DEFAULT 1,
  CHECK (status <> 'active' OR verified_at IS NOT NULL),
  CHECK ((status = 'disabled') = (disabled_at IS NOT NULL)),
  CHECK ((status = 'disabled') = (disabled_reason IS NOT NULL)),
  CHECK (url ~ '^https?://\S+$' AND char_length(url) <= 2048),  -- the app allows http only in development
  UNIQUE (id, tenant_id)                     -- children reference (id, tenant_id), so they share its tenant
);
CREATE INDEX endpoints_tenant ON webhooks.endpoints (tenant_id) WHERE deleted_at IS NULL;

CREATE TABLE webhooks.endpoint_secrets (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL,
  endpoint_id  uuid NOT NULL,
  ciphertext   bytea NOT NULL,               -- platform/crypto.Envelope: signing needs the secret back, so not a hash
  kms_key_id   text NOT NULL,
  fingerprint  bytea NOT NULL,               -- HMAC of the secret under a separate key, to recognise it without decrypting
  created_by   uuid NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz,                  -- NULL while current; rotation sets it 24 hours ahead
  FOREIGN KEY (endpoint_id, tenant_id) REFERENCES webhooks.endpoints (id, tenant_id)
);
CREATE UNIQUE INDEX endpoint_secrets_current ON webhooks.endpoint_secrets (endpoint_id) WHERE expires_at IS NULL;

CREATE TABLE webhooks.subscriptions (
  tenant_id    uuid NOT NULL,
  endpoint_id  uuid NOT NULL,
  event_type   text NOT NULL,                -- an event sent to clients, or '*' for all of them
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (endpoint_id, event_type),
  FOREIGN KEY (endpoint_id, tenant_id) REFERENCES webhooks.endpoints (id, tenant_id),
  CHECK (event_type = '*' OR event_type ~ '^[a-z][a-z_]*\.[a-z][a-z_]*$')
);

CREATE TABLE webhooks.deliveries (
  id               uuid PRIMARY KEY,
  tenant_id        uuid NOT NULL,
  endpoint_id      uuid NOT NULL,
  event_id         uuid NOT NULL,            -- the bus event's ID, sent as Vaspro-Webhook-Id
  event_type       text NOT NULL,
  occurred_at      timestamptz NOT NULL,
  body             bytea NOT NULL,           -- the exact bytes sent; every attempt and every replay sends them again
  status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'dead')),
  dead_reason      text CHECK (dead_reason IN ('attempts_exhausted', 'endpoint_disabled', 'endpoint_deleted')),
  attempts         int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_attempt_at  timestamptz,
  leased_until     timestamptz,              -- a worker's claim; an expired lease is claimed again
  replay_of        uuid,                     -- the delivery this replays, same tenant
  replayed_by      uuid,
  created_at       timestamptz NOT NULL DEFAULT now(),
  completed_at     timestamptz,
  CHECK ((status = 'pending') = (next_attempt_at IS NOT NULL)),
  CHECK ((status = 'pending') = (completed_at IS NULL)),
  CHECK ((status = 'dead') = (dead_reason IS NOT NULL)),
  CHECK ((replay_of IS NULL) = (replayed_by IS NULL)),
  CHECK (event_type ~ '^[a-z][a-z_]*\.[a-z][a-z_]*$'),
  CHECK (leased_until IS NULL OR status = 'pending'),
  CHECK (status <> 'succeeded' OR attempts >= 1),
  UNIQUE (id, tenant_id),
  FOREIGN KEY (endpoint_id, tenant_id) REFERENCES webhooks.endpoints (id, tenant_id),
  FOREIGN KEY (replay_of, tenant_id) REFERENCES webhooks.deliveries (id, tenant_id)
);
CREATE UNIQUE INDEX deliveries_once ON webhooks.deliveries (endpoint_id, event_id) WHERE replay_of IS NULL;
CREATE INDEX deliveries_due ON webhooks.deliveries (next_attempt_at) WHERE status = 'pending';
CREATE INDEX deliveries_log ON webhooks.deliveries (tenant_id, created_at DESC, id DESC);
CREATE INDEX deliveries_event ON webhooks.deliveries (tenant_id, event_id);

CREATE TABLE webhooks.delivery_attempts (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  delivery_id         uuid NOT NULL,
  attempt             int NOT NULL CHECK (attempt >= 1),
  started_at          timestamptz NOT NULL,
  duration_ms         int NOT NULL CHECK (duration_ms >= 0),
  request_headers     jsonb NOT NULL,        -- as sent, signature included; never the secret
  outcome             text NOT NULL CHECK (outcome IN ('succeeded', 'http_status', 'timeout', 'connect', 'tls', 'refused_url')),
  response_status     int,
  response_headers    jsonb,
  response_body       bytea CHECK (octet_length(response_body) <= 4096),  -- the first 4 KB
  response_truncated  boolean NOT NULL DEFAULT false,
  error               text,                  -- what went wrong when there was no response
  UNIQUE (delivery_id, attempt),
  CHECK ((outcome IN ('succeeded', 'http_status')) = (response_status IS NOT NULL)),
  CHECK (outcome <> 'succeeded' OR response_status BETWEEN 200 AND 299),
  CHECK (outcome <> 'http_status' OR response_status NOT BETWEEN 200 AND 299),
  CHECK ((outcome IN ('timeout', 'connect', 'tls', 'refused_url')) = (error IS NOT NULL)),
  CHECK (NOT response_truncated OR response_body IS NOT NULL),
  FOREIGN KEY (delivery_id, tenant_id) REFERENCES webhooks.deliveries (id, tenant_id)
);

CREATE TABLE webhooks.outbox_events (
  id            uuid PRIMARY KEY,
  tenant_id     uuid,
  aggregate     text NOT NULL,
  aggregate_id  uuid NOT NULL,
  event_type    text NOT NULL,
  payload       jsonb NOT NULL,
  occurred_at   timestamptz NOT NULL DEFAULT now(),
  published_at  timestamptz
);
CREATE INDEX webhooks_outbox_unpublished ON webhooks.outbox_events (occurred_at) WHERE published_at IS NULL;

CREATE TABLE webhooks.processed_events (
  event_id      uuid PRIMARY KEY,
  processed_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhooks.idempotency_keys (
  tenant_id     uuid NOT NULL,
  key           text NOT NULL,
  request_hash  bytea NOT NULL,
  status_code   int,
  response      jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, key)
);

-- Roles (the ledger's and payments' pattern). webhooks_service holds the
-- service's privileges; its login role is granted it (with inheritance)
-- and doesn't own the tables, so these grants are a real limit. Updates
-- are by column: a delivery's body, event and endpoint, a secret's
-- ciphertext and an attempt, once recorded, are never rewritten, and
-- nothing but a subscription or an idempotency key is ever deleted.
--
-- webhooks_system is the delivery worker: it claims due deliveries
-- across clients, reads their endpoints and secrets, records attempts,
-- and disables a failing endpoint. It's NOLOGIN, granted WITH INHERIT
-- FALSE so it applies only inside db.Pool.WithRoleTx (1.8's lesson).
CREATE ROLE webhooks_service NOLOGIN;
CREATE ROLE webhooks_system NOLOGIN;
GRANT webhooks_system TO webhooks_service WITH INHERIT FALSE;
GRANT webhooks_system TO CURRENT_USER WITH INHERIT FALSE;

GRANT USAGE ON SCHEMA webhooks TO webhooks_service, webhooks_system;
GRANT SELECT ON ALL TABLES IN SCHEMA webhooks TO webhooks_service, webhooks_system;

GRANT INSERT ON webhooks.endpoints, webhooks.endpoint_secrets, webhooks.subscriptions,
  webhooks.deliveries, webhooks.delivery_attempts, webhooks.outbox_events,
  webhooks.processed_events, webhooks.idempotency_keys
  TO webhooks_service;
GRANT UPDATE ON webhooks.outbox_events, webhooks.idempotency_keys TO webhooks_service;
GRANT DELETE ON webhooks.subscriptions, webhooks.idempotency_keys TO webhooks_service;
GRANT UPDATE (url, description, status, verified_at, failing_since, disabled_at, disabled_reason,
  updated_at, deleted_at, version)
  ON webhooks.endpoints TO webhooks_service;
GRANT UPDATE (expires_at) ON webhooks.endpoint_secrets TO webhooks_service;

GRANT INSERT ON webhooks.delivery_attempts, webhooks.outbox_events TO webhooks_system;
GRANT UPDATE (status, failing_since, disabled_at, disabled_reason, updated_at, version)
  ON webhooks.endpoints TO webhooks_system;

-- Column updates the service and the worker share.
GRANT UPDATE (status, dead_reason, attempts, next_attempt_at, leased_until, completed_at)
  ON webhooks.deliveries TO webhooks_service, webhooks_system;

-- Row-level security: the standard policy on every client table (data
-- model §1), plus the worker's reach across clients. outbox_events,
-- processed_events and idempotency_keys follow the other schemas.
ALTER TABLE webhooks.endpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhooks.endpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhooks.endpoints
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_read ON webhooks.endpoints FOR SELECT TO webhooks_system USING (true);
CREATE POLICY system_update ON webhooks.endpoints FOR UPDATE TO webhooks_system USING (true) WITH CHECK (true);

ALTER TABLE webhooks.endpoint_secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhooks.endpoint_secrets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhooks.endpoint_secrets
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_read ON webhooks.endpoint_secrets FOR SELECT TO webhooks_system USING (true);

ALTER TABLE webhooks.subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhooks.subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhooks.subscriptions
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE webhooks.deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhooks.deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhooks.deliveries
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_read ON webhooks.deliveries FOR SELECT TO webhooks_system USING (true);
CREATE POLICY system_update ON webhooks.deliveries FOR UPDATE TO webhooks_system USING (true) WITH CHECK (true);

ALTER TABLE webhooks.delivery_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhooks.delivery_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON webhooks.delivery_attempts
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_read ON webhooks.delivery_attempts FOR SELECT TO webhooks_system USING (true);
CREATE POLICY system_insert ON webhooks.delivery_attempts FOR INSERT TO webhooks_system WITH CHECK (true);

-- +goose Down
DROP TABLE webhooks.idempotency_keys;
DROP TABLE webhooks.processed_events;
DROP TABLE webhooks.outbox_events;
DROP TABLE webhooks.delivery_attempts;
DROP TABLE webhooks.deliveries;
DROP TABLE webhooks.subscriptions;
DROP TABLE webhooks.endpoint_secrets;
DROP TABLE webhooks.endpoints;
DROP SCHEMA webhooks;
DROP ROLE webhooks_system;
DROP ROLE webhooks_service;
