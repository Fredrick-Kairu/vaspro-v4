-- +goose Up

-- Task 3.2: the ledger's database guards (data model §5, OQ-13, ADR
-- 0010). Each refuses with check_violation (23514) unless noted, so
-- the service can map them to one error.
--
-- 1. A transaction balances per currency and has at least one debit and
--    one credit, checked at commit (deferred constraint triggers).
-- 2. An entry's currency is its account's.
-- 3. Entries are inserted in the same database transaction (and
--    savepoint) as their transaction row, so a posted transaction can't
--    gain lines later.
-- 4. transactions, entries and lot_consumptions are append-only:
--    UPDATE, DELETE and TRUNCATE raise, for the table owner too. The
--    service's role has no such grants either (3.1's migration).
-- 5. The entries trigger keeps client accounts' balances in step on
--    each account's normal side; VASPRO's accounts get no balance row
--    and no per-posting update (ADR 0010). A client account's balance
--    row is created with the account.
-- 6. Main, service and bonus wallets never go below zero available
--    (balance >= 0 and held <= balance), checked at commit.
--
-- The deferred triggers are DEFERRABLE INITIALLY DEFERRED: SET
-- CONSTRAINTS ... IMMEDIATE only makes them stricter. Only the table
-- owner can disable a trigger.

-- 1. Balanced per currency, checked at commit. Fires once per entry and
-- once per transaction row, so a transaction with no entries fails too.
-- +goose StatementBegin
CREATE FUNCTION ledger.check_transaction_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  txn uuid;
  bad record;
BEGIN
  IF TG_TABLE_NAME = 'entries' THEN
    txn := NEW.transaction_id;
  ELSE
    txn := NEW.id;
  END IF;

  IF NOT EXISTS (SELECT 1 FROM ledger.entries WHERE transaction_id = txn AND direction = 'D')
     OR NOT EXISTS (SELECT 1 FROM ledger.entries WHERE transaction_id = txn AND direction = 'C') THEN
    RAISE EXCEPTION 'ledger: transaction % has no debit or no credit', txn
      USING ERRCODE = 'check_violation';
  END IF;

  SELECT currency,
         sum(CASE direction WHEN 'D' THEN amount_minor ELSE -amount_minor END) AS difference
    INTO bad
    FROM ledger.entries
   WHERE transaction_id = txn
   GROUP BY currency
  HAVING sum(CASE direction WHEN 'D' THEN amount_minor ELSE -amount_minor END) <> 0
   LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'ledger: transaction % does not balance in %: debits minus credits is %',
      txn, bad.currency, bad.difference
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER entries_balanced
  AFTER INSERT ON ledger.entries
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION ledger.check_transaction_balanced();

CREATE CONSTRAINT TRIGGER transactions_balanced
  AFTER INSERT ON ledger.transactions
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION ledger.check_transaction_balanced();

-- 2. An entry's currency is its account's. An account the caller can't
-- see is left to RLS and the foreign key to refuse, so their errors
-- aren't masked.
-- +goose StatementBegin
CREATE FUNCTION ledger.check_entry_currency() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  account_currency char(3);
BEGIN
  SELECT currency INTO account_currency FROM ledger.accounts WHERE id = NEW.account_id;
  IF FOUND AND account_currency <> NEW.currency THEN
    RAISE EXCEPTION 'ledger: entry currency % differs from account %''s currency %',
      NEW.currency, NEW.account_id, account_currency
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TRIGGER entries_currency
  BEFORE INSERT ON ledger.entries
  FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_currency();

-- 3. Entries only with their transaction. Rows written by one
-- (sub)transaction share its xmin, so an entry whose xmin differs from
-- its transaction row's was added later or from another savepoint. Not
-- posted_at: 3.10 imports v3 history with its original dates.
-- +goose StatementBegin
CREATE FUNCTION ledger.check_entry_with_transaction() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  same boolean;
BEGIN
  SELECT t.xmin = e.xmin INTO same
    FROM ledger.transactions t, ledger.entries e
   WHERE t.id = NEW.transaction_id AND e.id = NEW.id;
  IF same IS NOT TRUE THEN
    RAISE EXCEPTION 'ledger: entries must be inserted in the same transaction (and savepoint) as transaction %',
      NEW.transaction_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER entries_same_transaction
  AFTER INSERT ON ledger.entries
  FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_with_transaction();

-- 4. Append-only journal.
-- +goose StatementBegin
CREATE FUNCTION ledger.refuse_journal_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'ledger.% is append-only: % refused; post a correcting transaction instead',
    TG_TABLE_NAME, TG_OP;
END $$;
-- +goose StatementEnd

CREATE TRIGGER transactions_append_only
  BEFORE UPDATE OR DELETE ON ledger.transactions
  FOR EACH ROW EXECUTE FUNCTION ledger.refuse_journal_change();
CREATE TRIGGER transactions_no_truncate
  BEFORE TRUNCATE ON ledger.transactions
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.refuse_journal_change();
CREATE TRIGGER entries_append_only
  BEFORE UPDATE OR DELETE ON ledger.entries
  FOR EACH ROW EXECUTE FUNCTION ledger.refuse_journal_change();
CREATE TRIGGER entries_no_truncate
  BEFORE TRUNCATE ON ledger.entries
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.refuse_journal_change();
CREATE TRIGGER lot_consumptions_append_only
  BEFORE UPDATE OR DELETE ON ledger.lot_consumptions
  FOR EACH ROW EXECUTE FUNCTION ledger.refuse_journal_change();
CREATE TRIGGER lot_consumptions_no_truncate
  BEFORE TRUNCATE ON ledger.lot_consumptions
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.refuse_journal_change();

-- 5. Balance sync. SECURITY DEFINER because the service's role may not
-- write balance_minor (3.1's grants); the owner still runs under the
-- caller's app.tenant_id and RLS. search_path is pinned, as a definer
-- function must.
-- +goose StatementBegin
CREATE FUNCTION ledger.apply_entry_to_balance() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  acct record;
BEGIN
  SELECT type, tenant_id INTO acct FROM ledger.accounts WHERE id = NEW.account_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'ledger: account % not found for its balance', NEW.account_id
      USING ERRCODE = 'check_violation';
  END IF;
  IF acct.tenant_id IS NULL THEN
    RETURN NULL;  -- VASPRO's own accounts: no per-posting balance (ADR 0010)
  END IF;

  UPDATE ledger.balances
     SET balance_minor = balance_minor
                         + CASE WHEN NEW.direction = ledger.normal_side(acct.type)
                                THEN NEW.amount_minor ELSE -NEW.amount_minor END,
         version = version + 1,
         updated_at = now()
   WHERE account_id = NEW.account_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'ledger: client account % has no balance row', NEW.account_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION ledger.apply_entry_to_balance() FROM PUBLIC;

CREATE TRIGGER entries_apply_balance
  AFTER INSERT ON ledger.entries
  FOR EACH ROW EXECUTE FUNCTION ledger.apply_entry_to_balance();

-- +goose StatementBegin
CREATE FUNCTION ledger.create_balance_row() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  INSERT INTO ledger.balances (account_id) VALUES (NEW.id);
  RETURN NULL;
END $$;
-- +goose StatementEnd
REVOKE EXECUTE ON FUNCTION ledger.create_balance_row() FROM PUBLIC;

CREATE TRIGGER accounts_create_balance
  AFTER INSERT ON ledger.accounts
  FOR EACH ROW WHEN (NEW.tenant_id IS NOT NULL)
  EXECUTE FUNCTION ledger.create_balance_row();

-- 6. No client wallet below zero available, checked at commit on the
-- row's final values, so the order of lines within one posting doesn't
-- matter. A row the check can't see fails closed.
-- +goose StatementBegin
CREATE FUNCTION ledger.check_wallet_not_negative() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  b record;
BEGIN
  SELECT bl.balance_minor, bl.held_minor, a.type
    INTO b
    FROM ledger.balances bl JOIN ledger.accounts a ON a.id = bl.account_id
   WHERE bl.account_id = NEW.account_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'ledger: balance of account % not visible to the wallet check', NEW.account_id
      USING ERRCODE = 'check_violation';
  END IF;
  IF ledger.is_client_wallet(b.type)
     AND (b.balance_minor < 0 OR b.held_minor > b.balance_minor) THEN
    RAISE EXCEPTION 'ledger: wallet % would go below zero (balance %, held %)',
      NEW.account_id, b.balance_minor, b.held_minor
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER balances_wallet_not_negative
  AFTER INSERT OR UPDATE ON ledger.balances
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION ledger.check_wallet_not_negative();

-- +goose Down
DROP TRIGGER balances_wallet_not_negative ON ledger.balances;
DROP FUNCTION ledger.check_wallet_not_negative();
DROP TRIGGER accounts_create_balance ON ledger.accounts;
DROP FUNCTION ledger.create_balance_row();
DROP TRIGGER entries_apply_balance ON ledger.entries;
DROP FUNCTION ledger.apply_entry_to_balance();
DROP TRIGGER lot_consumptions_no_truncate ON ledger.lot_consumptions;
DROP TRIGGER lot_consumptions_append_only ON ledger.lot_consumptions;
DROP TRIGGER entries_no_truncate ON ledger.entries;
DROP TRIGGER entries_append_only ON ledger.entries;
DROP TRIGGER transactions_no_truncate ON ledger.transactions;
DROP TRIGGER transactions_append_only ON ledger.transactions;
DROP FUNCTION ledger.refuse_journal_change();
DROP TRIGGER entries_same_transaction ON ledger.entries;
DROP FUNCTION ledger.check_entry_with_transaction();
DROP TRIGGER entries_currency ON ledger.entries;
DROP FUNCTION ledger.check_entry_currency();
DROP TRIGGER transactions_balanced ON ledger.transactions;
DROP TRIGGER entries_balanced ON ledger.entries;
DROP FUNCTION ledger.check_transaction_balanced();
