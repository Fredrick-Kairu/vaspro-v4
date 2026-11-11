-- +goose Up

-- Task 3.1: the ledger schema, copied from data model §5. Additions the
-- design doesn't spell out, agreed on 4 Nov (OQ-13), are marked
-- "Addition" below. The database guards (balance, immutability, no
-- wallet below zero, balance sync) are task 3.2's migration.

CREATE SCHEMA ledger;
-- token_prices' EXCLUDE needs btree_gist for its equality columns.
-- Database-wide extension, left in place by Down like citext (2.1).
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

-- Every schema gets outbox_events and idempotency_keys (data model §1).
-- No processed_events: ledger consumes no events yet (2.1's precedent).
CREATE TABLE ledger.outbox_events (
  id            uuid PRIMARY KEY,
  tenant_id     uuid,
  aggregate     text NOT NULL,
  aggregate_id  uuid NOT NULL,
  event_type    text NOT NULL,
  payload       jsonb NOT NULL,
  occurred_at   timestamptz NOT NULL DEFAULT now(),
  published_at  timestamptz
);
CREATE INDEX ledger_outbox_unpublished ON ledger.outbox_events (occurred_at) WHERE published_at IS NULL;

CREATE TABLE ledger.idempotency_keys (
  tenant_id     uuid NOT NULL,
  key           text NOT NULL,
  request_hash  bytea NOT NULL,
  status_code   int,
  response      jsonb,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, key)
);

-- Currencies (data model §5's comments; platform/money).
INSERT INTO ledger.currencies (code, minor_exponent) VALUES ('KES', 2), ('TZS', 2), ('UGX', 0);

-- Addition: the account type rules (OQ-13). The one source is
-- internal/ledger/domain/accounttype.go; these copies serve the CHECKs
-- and 3.2's triggers, and a store test keeps them equal for every type.
--
-- normal_side: the side on which a line raises the balance. A plpgsql
-- CASE with no ELSE raises case_not_found, so a type added to the enum
-- without a side here fails loudly rather than defaulting.
-- +goose StatementBegin
CREATE FUNCTION ledger.normal_side(t ledger.account_type) RETURNS char(1)
LANGUAGE plpgsql IMMUTABLE STRICT AS $$
BEGIN
  CASE t
    WHEN 'main_wallet', 'service_wallet', 'bonus', 'revenue_share_payable', 'commission_payable',
         'suspense', 'reversal_clearing', 'revenue', 'tax_payable' THEN RETURN 'C';
    WHEN 'receivable', 'provider_clearing' THEN RETURN 'D';
  END CASE;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger.is_client_owned(t ledger.account_type) RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT AS $$
  SELECT t IN ('main_wallet', 'service_wallet', 'bonus', 'receivable',
               'revenue_share_payable', 'commission_payable')
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger.is_client_wallet(t ledger.account_type) RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT AS $$
  SELECT t IN ('main_wallet', 'service_wallet', 'bonus')
$$;
-- +goose StatementEnd

-- Addition: what each account type must carry. Client types belong to a
-- tenant and VASPRO's don't; service wallets and revenue name a
-- service; provider clearing, and only it, names a rail.
ALTER TABLE ledger.accounts
  ADD CONSTRAINT accounts_owner CHECK ((tenant_id IS NOT NULL) = ledger.is_client_owned(type)),
  ADD CONSTRAINT accounts_service_code CHECK (type NOT IN ('service_wallet', 'revenue') OR service_code IS NOT NULL),
  ADD CONSTRAINT accounts_rail CHECK ((type = 'provider_clearing') = (rail IS NOT NULL));

-- Roles (addition, OQ-13).
--
-- ledger_service holds the service's privileges. The service's login
-- role is granted it (with inheritance) and does not own the tables, so
-- the grants below are a real limit: no UPDATE, DELETE or TRUNCATE on
-- the journal (transactions, entries, lot_consumptions), and on
-- balances only held_minor (3.2's trigger moves balance_minor). The
-- table owner is still stopped by 3.2's triggers.
--
-- ledger_system writes VASPRO's own rows (tenant_id NULL): its accounts,
-- list prices, standard rate cards and system postings. NOLOGIN,
-- reached only through db.Pool.WithRoleTx, and granted WITH INHERIT
-- FALSE so its policies never apply outside that transaction (1.8's
-- lesson). Granting it to ledger_service lets the login role SET ROLE
-- to it without inheriting it.
CREATE ROLE ledger_service NOLOGIN;
CREATE ROLE ledger_system NOLOGIN;
GRANT ledger_system TO ledger_service WITH INHERIT FALSE;
GRANT ledger_system TO CURRENT_USER WITH INHERIT FALSE;

GRANT USAGE ON SCHEMA ledger TO ledger_service, ledger_system;
GRANT SELECT ON ALL TABLES IN SCHEMA ledger TO ledger_service, ledger_system;
GRANT INSERT ON ledger.accounts, ledger.token_prices, ledger.rate_cards, ledger.transactions,
  ledger.entries, ledger.token_lots, ledger.lot_consumptions, ledger.holds, ledger.usage_batches,
  ledger.auto_buy_rules, ledger.reversals, ledger.outbox_events, ledger.idempotency_keys
  TO ledger_service;
GRANT UPDATE ON ledger.accounts, ledger.token_prices, ledger.rate_cards, ledger.token_lots,
  ledger.holds, ledger.usage_batches, ledger.auto_buy_rules, ledger.reversals,
  ledger.outbox_events, ledger.idempotency_keys
  TO ledger_service;
GRANT UPDATE (held_minor, version, updated_at) ON ledger.balances TO ledger_service;
GRANT DELETE ON ledger.idempotency_keys TO ledger_service;
GRANT INSERT ON ledger.accounts, ledger.token_prices, ledger.rate_cards, ledger.transactions,
  ledger.entries, ledger.outbox_events
  TO ledger_system;
GRANT UPDATE ON ledger.accounts, ledger.token_prices, ledger.rate_cards, ledger.outbox_events
  TO ledger_system;

-- Row-level security (OQ-13). currencies and tax_rates are global and
-- have none, like accounts.services (2.1); outbox_events and
-- idempotency_keys follow the other schemas.

-- The five tables with tenant_id NOT NULL: the standard policy.
ALTER TABLE ledger.token_lots ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.token_lots FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ledger.token_lots
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE ledger.holds ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.holds FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ledger.holds
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE ledger.usage_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.usage_batches FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ledger.usage_batches
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE ledger.auto_buy_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.auto_buy_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ledger.auto_buy_rules
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE ledger.reversals ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.reversals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ledger.reversals
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- accounts, token_prices, rate_cards: a tenant reads its own rows and
-- VASPRO's, and writes only its own; ledger_system writes VASPRO's.
ALTER TABLE ledger.accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_read ON ledger.accounts FOR SELECT
  USING (tenant_id IS NULL OR tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_insert ON ledger.accounts FOR INSERT
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_update ON ledger.accounts FOR UPDATE
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_write ON ledger.accounts TO ledger_system
  USING (tenant_id IS NULL) WITH CHECK (tenant_id IS NULL);

ALTER TABLE ledger.token_prices ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.token_prices FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_read ON ledger.token_prices FOR SELECT
  USING (tenant_id IS NULL OR tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_insert ON ledger.token_prices FOR INSERT
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_update ON ledger.token_prices FOR UPDATE
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_write ON ledger.token_prices TO ledger_system
  USING (tenant_id IS NULL) WITH CHECK (tenant_id IS NULL);

ALTER TABLE ledger.rate_cards ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.rate_cards FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_read ON ledger.rate_cards FOR SELECT
  USING (tenant_id IS NULL OR tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_insert ON ledger.rate_cards FOR INSERT
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_update ON ledger.rate_cards FOR UPDATE
  USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_write ON ledger.rate_cards TO ledger_system
  USING (tenant_id IS NULL) WITH CHECK (tenant_id IS NULL);

-- transactions: a tenant reads and inserts its own; system postings
-- (tenant_id NULL) only as ledger_system. No UPDATE or DELETE policy.
ALTER TABLE ledger.transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.transactions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_read ON ledger.transactions FOR SELECT
  USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY tenant_insert ON ledger.transactions FOR INSERT
  WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY system_read ON ledger.transactions FOR SELECT TO ledger_system
  USING (tenant_id IS NULL);
CREATE POLICY system_insert ON ledger.transactions FOR INSERT TO ledger_system
  WITH CHECK (tenant_id IS NULL);

-- entries: no tenant_id; a line belongs to its transaction. A tenant
-- inserts lines into its own postings, on its own accounts or VASPRO's
-- (a client posting credits VASPRO's revenue and tax), and reads the
-- lines of its own postings only, so it never sees another tenant's
-- lines on a shared VASPRO account. ledger_system's postings use
-- VASPRO's accounts only. No UPDATE or DELETE policy, ever.
ALTER TABLE ledger.entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_read ON ledger.entries FOR SELECT
  USING (EXISTS (SELECT 1 FROM ledger.transactions t
                 WHERE t.id = transaction_id
                   AND t.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid));
CREATE POLICY tenant_insert ON ledger.entries FOR INSERT
  WITH CHECK (EXISTS (SELECT 1 FROM ledger.transactions t
                      WHERE t.id = transaction_id
                        AND t.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
          AND EXISTS (SELECT 1 FROM ledger.accounts a
                      WHERE a.id = account_id
                        AND (a.tenant_id IS NULL
                             OR a.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)));
CREATE POLICY system_read ON ledger.entries FOR SELECT TO ledger_system
  USING (EXISTS (SELECT 1 FROM ledger.transactions t WHERE t.id = transaction_id AND t.tenant_id IS NULL));
CREATE POLICY system_insert ON ledger.entries FOR INSERT TO ledger_system
  WITH CHECK (EXISTS (SELECT 1 FROM ledger.transactions t WHERE t.id = transaction_id AND t.tenant_id IS NULL)
          AND EXISTS (SELECT 1 FROM ledger.accounts a WHERE a.id = account_id AND a.tenant_id IS NULL));

-- balances: no tenant_id; a row follows its account. Only client
-- accounts have balance rows (ADR 0010).
ALTER TABLE ledger.balances ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.balances FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ledger.balances
  USING      (EXISTS (SELECT 1 FROM ledger.accounts a
                      WHERE a.id = account_id
                        AND a.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid))
  WITH CHECK (EXISTS (SELECT 1 FROM ledger.accounts a
                      WHERE a.id = account_id
                        AND a.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid));

-- lot_consumptions: no tenant_id; a row follows its lot.
ALTER TABLE ledger.lot_consumptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger.lot_consumptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_read ON ledger.lot_consumptions FOR SELECT
  USING (EXISTS (SELECT 1 FROM ledger.token_lots l
                 WHERE l.id = lot_id
                   AND l.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid));
CREATE POLICY tenant_insert ON ledger.lot_consumptions FOR INSERT
  WITH CHECK (EXISTS (SELECT 1 FROM ledger.token_lots l
                      WHERE l.id = lot_id
                        AND l.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid));

-- +goose Down
DROP TABLE ledger.idempotency_keys;
DROP TABLE ledger.outbox_events;
DROP TABLE ledger.reversals;
DROP TABLE ledger.auto_buy_rules;
DROP TABLE ledger.usage_batches;
DROP TABLE ledger.holds;
DROP TABLE ledger.lot_consumptions;
DROP TABLE ledger.token_lots;
DROP TABLE ledger.entries;
DROP TABLE ledger.transactions;
DROP TYPE ledger.txn_type;
DROP TABLE ledger.tax_rates;
DROP TABLE ledger.rate_cards;
DROP TABLE ledger.token_prices;
DROP TABLE ledger.balances;
DROP TABLE ledger.accounts;
DROP FUNCTION ledger.is_client_wallet(ledger.account_type);
DROP FUNCTION ledger.is_client_owned(ledger.account_type);
DROP FUNCTION ledger.normal_side(ledger.account_type);
DROP TYPE ledger.account_type;
DROP TABLE ledger.currencies;
DROP SCHEMA ledger;
DROP ROLE ledger_system;
DROP ROLE ledger_service;
