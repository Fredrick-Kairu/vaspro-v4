-- +goose Up

-- Task 4.1: the payments schema for Phase 4 (wallet top-ups), copied
-- from data model §7. Phase 13's tables (payment links, payouts,
-- batches, beneficiaries, provider floats) come with Phase 13.
-- Additions agreed on 11 Nov (OQ-22, task 4.1's plan) are marked
-- "Addition" below.

CREATE SCHEMA payments;

CREATE TABLE payments.rails (                    -- global; mirrors the country pack
  code                text PRIMARY KEY,        -- mpesa_ke, airtel_money_ke, mpesa_tz, mtn_momo_ug ...
  country_code        char(2),
  kind                text NOT NULL CHECK (kind IN ('mobile_money', 'card', 'bank', 'aggregator', 'value_provider')),
  capabilities        text[] NOT NULL,         -- stk, c2b, b2c, b2b, reversal, status_query, balance_query, name_check
  status              text NOT NULL CHECK (status IN ('live', 'paused', 'retired')),
  -- Addition: only the capabilities §7 lists.
  CHECK (capabilities <@ ARRAY['stk', 'c2b', 'b2c', 'b2b', 'reversal', 'status_query', 'balance_query', 'name_check']::text[])
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
  UNIQUE (rail_code, account_type, number),
  -- Addition: uses are the three §7 names, at least one; only VASPRO's
  -- own accounts take top-ups; an account turns active only after a
  -- test call with its credentials worked (§7, "Why it is built this way").
  CHECK (cardinality(uses) > 0 AND uses <@ ARRAY['collections', 'payouts', 'vaspro_topups']::text[]),
  CHECK (NOT ('vaspro_topups' = ANY (uses)) OR tenant_id IS NULL),
  CHECK (status <> 'active' OR verified_at IS NOT NULL)
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
  retired_at          timestamptz,
  -- Addition: retired exactly when it has a retirement time.
  CHECK ((status = 'retired') = (retired_at IS NOT NULL))
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
  UNIQUE (rail_account_id, rail_receipt),
  -- Addition: what each method, purpose and status carries.
  CHECK (currency ~ '^[A-Z]{3}$'),
  CHECK (method <> 'stk_push' OR payer_phone_e164 IS NOT NULL),
  CHECK (purpose <> 'invoice' OR invoice_id IS NOT NULL),
  CHECK (purpose <> 'payment_link' OR payment_link_id IS NOT NULL),
  CHECK (status <> 'succeeded' OR rail_receipt IS NOT NULL),
  CHECK (status <> 'failed' OR error_code IS NOT NULL),
  CHECK ((status IN ('succeeded', 'failed', 'expired', 'cancelled')) = (completed_at IS NOT NULL)),
  CHECK (expires_at > created_at)
);
CREATE INDEX payment_intents_pending ON payments.payment_intents (expires_at) WHERE status = 'pending';
-- Addition: a callback finds its intent by the rail's request ID.
CREATE UNIQUE INDEX payment_intents_rail_request ON payments.payment_intents (rail_account_id, rail_request_id)
  WHERE rail_request_id IS NOT NULL;

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
  UNIQUE (rail_account_id, rail_receipt),      -- a repeated callback is ignored
  -- Addition: money received is positive; a client's own collection is
  -- owned and never matched; a top-up on VASPRO's paybill is unowned
  -- and matched to a client, or in suspense with no client at all.
  CHECK (amount_minor > 0),
  CHECK (currency ~ '^[A-Z]{3}$'),
  CHECK (matched_tenant_id IS NULL OR tenant_id IS NULL),
  CHECK (match_status <> 'client_owned' OR (tenant_id IS NOT NULL AND matched_tenant_id IS NULL)),
  CHECK (match_status <> 'matched' OR matched_tenant_id IS NOT NULL),
  CHECK (match_status <> 'suspense' OR (tenant_id IS NULL AND matched_tenant_id IS NULL))
);
CREATE INDEX collection_notices_tenant ON payments.collection_notices (tenant_id, paid_at DESC);
-- Partition by month when volume needs it; deduplication then moves to a small key table.
-- Addition: the staff suspense queue.
CREATE INDEX collection_notices_suspense ON payments.collection_notices (paid_at) WHERE match_status = 'suspense';

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
  UNIQUE (rail_account_id, period_start, period_end),
  CHECK (period_end > period_start)            -- Addition
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
  match_status        text NOT NULL CHECK (match_status IN ('matched', 'unmatched', 'ignored')),
  -- Addition: amounts are sizes (direction gives the sign); a matched
  -- line names what it matched, and only a matched line does.
  CHECK (amount_minor >= 0 AND charges_minor >= 0),
  CHECK ((match_status = 'matched') = (matched_id IS NOT NULL)),
  CHECK ((matched_type IS NULL) = (matched_id IS NULL)),
  CHECK (matched_type IS NULL OR matched_type IN ('collection_notice', 'payment_intent', 'payout'))
);
CREATE INDEX statement_lines_receipt ON payments.statement_lines (rail_receipt);

CREATE TABLE payments.recon_exceptions (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid,
  rail_account_id     uuid NOT NULL REFERENCES payments.rail_accounts (id),
  kind                text NOT NULL CHECK (kind IN ('missing_in_vaspro', 'missing_on_rail',
                        'amount_mismatch', 'status_mismatch', 'duplicate',
                        'ledger_mismatch')),   -- Addition (OQ-22): a notice and provider clearing disagree
  statement_line_id   bigint REFERENCES payments.statement_lines (id),
  reference_type      text,
  reference_id        uuid,
  status              text NOT NULL CHECK (status IN ('open', 'resolved', 'written_off')),
  resolution          text,
  resolved_by         uuid,
  opened_at           timestamptz NOT NULL DEFAULT now(),
  resolved_at         timestamptz,
  -- Addition: an open exception has no resolution; a closed one says
  -- who closed it, when and how.
  CHECK ((status = 'open' AND resolution IS NULL AND resolved_by IS NULL AND resolved_at IS NULL)
      OR (status <> 'open' AND resolution IS NOT NULL AND resolved_by IS NOT NULL AND resolved_at IS NOT NULL))
);
CREATE INDEX recon_open ON payments.recon_exceptions (opened_at) WHERE status = 'open';

-- Addition (OQ-22): each client's account number on VASPRO's paybill.
-- Six characters from an alphabet without 0, 1, I or O, plus two check
-- characters (4.3). Permanent: nothing may update one.
CREATE TABLE payments.topup_accounts (
  id                  uuid PRIMARY KEY,
  tenant_id           uuid NOT NULL,
  rail_account_id     uuid NOT NULL REFERENCES payments.rail_accounts (id),
  account_number      text NOT NULL CHECK (account_number ~ '^[2-9A-HJ-NP-Z]{8}$'),
  created_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, rail_account_id),
  UNIQUE (rail_account_id, account_number)
);

-- Addition (OQ-22): every STK result callback. Unique per checkout, so a
-- repeat is ignored even for a failure (which has no receipt); one that
-- arrives before its intent's rail_request_id is saved waits here, and
-- is applied once the intent is found.
CREATE TABLE payments.stk_callbacks (
  id                  uuid PRIMARY KEY,
  rail_account_id     uuid NOT NULL REFERENCES payments.rail_accounts (id),
  checkout_request_id text NOT NULL,
  merchant_request_id text,
  result_code         text NOT NULL,           -- the rail's own code
  raw                 jsonb NOT NULL,          -- personal data (the payer's phone)
  received_at         timestamptz NOT NULL DEFAULT now(),
  payment_intent_id   uuid,
  applied_at          timestamptz,
  UNIQUE (rail_account_id, checkout_request_id),
  CHECK ((applied_at IS NULL) = (payment_intent_id IS NULL))
);
CREATE INDEX stk_callbacks_unapplied ON payments.stk_callbacks (received_at) WHERE applied_at IS NULL;

-- Every schema gets outbox_events and idempotency_keys (data model §1).
-- No processed_events: payments consumes no events in Phase 4 (2.1's
-- and 3.1's precedent).
CREATE TABLE payments.outbox_events (
  id            uuid PRIMARY KEY,
  tenant_id     uuid,
  aggregate     text NOT NULL,
  aggregate_id  uuid NOT NULL,
  event_type    text NOT NULL,
  payload       jsonb NOT NULL,
  occurred_at   timestamptz NOT NULL DEFAULT now(),
  published_at  timestamptz
);
CREATE INDEX payments_outbox_unpublished ON payments.outbox_events (occurred_at) WHERE published_at IS NULL;

CREATE TABLE payments.idempotency_keys (
  tenant_id     uuid NOT NULL,
  key           text NOT NULL,
  request_hash  bytea NOT NULL,
  status_code   int,
  response      jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, key)
);

-- Roles (the ledger's pattern, 3.1). payments_service holds the
-- service's privileges; its login role is granted it (with inheritance)
-- and doesn't own the tables, so these grants are a real limit. Updates
-- are by column: an amount, receipt, tenant or ciphertext, once
-- recorded, is never rewritten, and nothing but an idempotency key is
-- ever deleted.
--
-- payments_system works on rows with no tenant: VASPRO's paybill, its
-- credentials, unmatched notices, VASPRO's statements, parked STK
-- callbacks; it also looks intents and account numbers up across
-- tenants for callbacks. payments_operations is staff: it reads the
-- suspense queue and reconciliation and resolves exceptions, and has no
-- grant on credentials. Both are NOLOGIN, granted WITH INHERIT FALSE so
-- they apply only inside db.Pool.WithRoleTx (1.8's lesson).
CREATE ROLE payments_service NOLOGIN;
CREATE ROLE payments_system NOLOGIN;
CREATE ROLE payments_operations NOLOGIN;
GRANT payments_system, payments_operations TO payments_service WITH INHERIT FALSE;
GRANT payments_system, payments_operations TO CURRENT_USER WITH INHERIT FALSE;

GRANT USAGE ON SCHEMA payments TO payments_service, payments_system, payments_operations;

GRANT SELECT ON ALL TABLES IN SCHEMA payments TO payments_service, payments_system;
GRANT INSERT ON payments.rails, payments.rail_accounts, payments.rail_credentials,
  payments.payment_intents, payments.collection_notices, payments.statement_imports,
  payments.statement_lines, payments.recon_exceptions, payments.topup_accounts,
  payments.outbox_events, payments.idempotency_keys
  TO payments_service;
GRANT UPDATE ON payments.rails, payments.outbox_events, payments.idempotency_keys TO payments_service;
GRANT DELETE ON payments.idempotency_keys TO payments_service;

GRANT INSERT ON payments.rail_accounts, payments.rail_credentials, payments.collection_notices,
  payments.statement_imports, payments.statement_lines, payments.recon_exceptions,
  payments.stk_callbacks, payments.outbox_events
  TO payments_system;
GRANT UPDATE ON payments.outbox_events TO payments_system;
GRANT UPDATE (payment_intent_id, applied_at) ON payments.stk_callbacks TO payments_system;

-- Column updates the service and payments_system share.
GRANT UPDATE (status, display_name, c2b_registered_at, verified_at, last_balance_minor,
  balance_checked_at, low_balance_minor, version)
  ON payments.rail_accounts TO payments_service, payments_system;
GRANT UPDATE (status, retired_at) ON payments.rail_credentials TO payments_service, payments_system;
GRANT UPDATE (status, rail_request_id, rail_receipt, result_code, error_code, completed_at)
  ON payments.payment_intents TO payments_service;
GRANT UPDATE (match_status, matched_tenant_id, ledger_transaction_id, payment_intent_id)
  ON payments.collection_notices TO payments_service, payments_system;
GRANT UPDATE (status, line_count) ON payments.statement_imports TO payments_service, payments_system;
GRANT UPDATE (match_status, matched_type, matched_id) ON payments.statement_lines TO payments_service, payments_system;
GRANT UPDATE (status, resolution, resolved_by, resolved_at)
  ON payments.recon_exceptions TO payments_service, payments_system, payments_operations;

GRANT SELECT ON payments.rails, payments.rail_accounts, payments.payment_intents,
  payments.collection_notices, payments.statement_imports, payments.statement_lines,
  payments.recon_exceptions, payments.topup_accounts
  TO payments_operations;

-- Row-level security (OQ-22, task 4.1's plan). rails is global, with
-- none, like accounts.services; outbox_events and idempotency_keys
-- follow the other schemas.

-- payment_intents and topup_accounts: the standard policy, plus reads
-- across tenants for callback lookups and staff.
ALTER TABLE payments.payment_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments.payment_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payments.payment_intents
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_read ON payments.payment_intents FOR SELECT TO payments_system USING (true);
CREATE POLICY operations_read ON payments.payment_intents FOR SELECT TO payments_operations USING (true);

ALTER TABLE payments.topup_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments.topup_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payments.topup_accounts
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_read ON payments.topup_accounts FOR SELECT TO payments_system USING (true);
CREATE POLICY operations_read ON payments.topup_accounts FOR SELECT TO payments_operations USING (true);

-- rail_accounts: a tenant reads its own and VASPRO's, writes its own;
-- payments_system writes VASPRO's (the ledger's accounts policy).
ALTER TABLE payments.rail_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments.rail_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_read ON payments.rail_accounts FOR SELECT
  USING (tenant_id IS NULL OR tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_insert ON payments.rail_accounts FOR INSERT
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_update ON payments.rail_accounts FOR UPDATE
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_write ON payments.rail_accounts TO payments_system
  USING (tenant_id IS NULL) WITH CHECK (tenant_id IS NULL);
CREATE POLICY operations_read ON payments.rail_accounts FOR SELECT TO payments_operations USING (true);

-- rail_credentials: a tenant its own only; VASPRO's only payments_system.
ALTER TABLE payments.rail_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments.rail_credentials FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payments.rail_credentials
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_write ON payments.rail_credentials TO payments_system
  USING (tenant_id IS NULL) WITH CHECK (tenant_id IS NULL);

-- collection_notices (§7: "their policy also checks matched_tenant_id"):
-- a tenant sees its own collections and the top-ups matched to it, and
-- records a top-up for itself; suspense belongs to payments_system.
ALTER TABLE payments.collection_notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments.collection_notices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_read ON payments.collection_notices FOR SELECT
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
      OR matched_tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_insert ON payments.collection_notices FOR INSERT
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
      OR (tenant_id IS NULL AND matched_tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid));
CREATE POLICY tenant_update ON payments.collection_notices FOR UPDATE
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
      OR matched_tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
      OR (tenant_id IS NULL AND matched_tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid));
CREATE POLICY system_write ON payments.collection_notices TO payments_system
  USING (tenant_id IS NULL) WITH CHECK (tenant_id IS NULL);
CREATE POLICY operations_read ON payments.collection_notices FOR SELECT TO payments_operations USING (true);

-- statement_imports and recon_exceptions: a tenant its own; VASPRO's
-- paybill payments_system; staff read all and resolve exceptions.
ALTER TABLE payments.statement_imports ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments.statement_imports FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payments.statement_imports
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_write ON payments.statement_imports TO payments_system
  USING (tenant_id IS NULL) WITH CHECK (tenant_id IS NULL);
CREATE POLICY operations_read ON payments.statement_imports FOR SELECT TO payments_operations USING (true);

ALTER TABLE payments.recon_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments.recon_exceptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payments.recon_exceptions
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_write ON payments.recon_exceptions TO payments_system
  USING (tenant_id IS NULL) WITH CHECK (tenant_id IS NULL);
CREATE POLICY operations_read ON payments.recon_exceptions FOR SELECT TO payments_operations USING (true);
CREATE POLICY operations_resolve ON payments.recon_exceptions FOR UPDATE TO payments_operations
  USING (true) WITH CHECK (true);

-- statement_lines: no tenant_id; a line follows its import.
ALTER TABLE payments.statement_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments.statement_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payments.statement_lines
  USING (EXISTS (SELECT 1 FROM payments.statement_imports i WHERE i.id = import_id
                   AND i.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid))
  WITH CHECK (EXISTS (SELECT 1 FROM payments.statement_imports i WHERE i.id = import_id
                   AND i.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid));
CREATE POLICY system_write ON payments.statement_lines TO payments_system
  USING (EXISTS (SELECT 1 FROM payments.statement_imports i WHERE i.id = import_id AND i.tenant_id IS NULL))
  WITH CHECK (EXISTS (SELECT 1 FROM payments.statement_imports i WHERE i.id = import_id AND i.tenant_id IS NULL));
CREATE POLICY operations_read ON payments.statement_lines FOR SELECT TO payments_operations USING (true);

-- stk_callbacks: payments_system only.
ALTER TABLE payments.stk_callbacks ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments.stk_callbacks FORCE ROW LEVEL SECURITY;
CREATE POLICY system_write ON payments.stk_callbacks TO payments_system USING (true) WITH CHECK (true);

-- +goose Down
DROP TABLE payments.idempotency_keys;
DROP TABLE payments.outbox_events;
DROP TABLE payments.stk_callbacks;
DROP TABLE payments.topup_accounts;
DROP TABLE payments.recon_exceptions;
DROP TABLE payments.statement_lines;
DROP TABLE payments.statement_imports;
DROP TABLE payments.collection_notices;
DROP TABLE payments.payment_intents;
DROP TYPE payments.intent_status;
DROP TYPE payments.intent_purpose;
DROP TABLE payments.rail_credentials;
DROP TABLE payments.rail_accounts;
DROP TABLE payments.rails;
DROP SCHEMA payments;
DROP ROLE payments_operations;
DROP ROLE payments_system;
DROP ROLE payments_service;
