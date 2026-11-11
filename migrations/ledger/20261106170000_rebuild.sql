-- +goose Up

-- Task 3.8 (OQ-19): the nightly rebuild. Additions to data model §5.
--
-- One row per date records the run: the replica that inserts it runs
-- the checks, the others skip, and a run that died is taken over after
-- an hour. Global, like currencies: no tenant, no RLS.
CREATE TABLE ledger.rebuild_runs (
  id                    uuid PRIMARY KEY,
  run_date              date NOT NULL UNIQUE,
  status                text NOT NULL CHECK (status IN ('running', 'passed', 'failed')),
  started_at            timestamptz NOT NULL,
  finished_at           timestamptz,
  accounts_checked      bigint CHECK (accounts_checked >= 0),
  transactions_checked  bigint CHECK (transactions_checked >= 0),
  differences           integer CHECK (differences >= 0),
  CHECK ((status = 'running') = (finished_at IS NULL)),
  CHECK (status = 'running' OR (accounts_checked IS NOT NULL AND transactions_checked IS NOT NULL AND differences IS NOT NULL)),
  CHECK (status <> 'passed' OR differences = 0),
  CHECK (status <> 'failed' OR differences > 0)
);
GRANT SELECT, INSERT, UPDATE ON ledger.rebuild_runs TO ledger_service;

-- The rebuild reads every client's rows: a narrow NOLOGIN role, reached
-- only through db.Pool.WithRoleTx and granted WITH INHERIT FALSE (1.8's
-- lesson), that reads the journal and its copies and writes nothing.
CREATE ROLE ledger_audit NOLOGIN;
GRANT USAGE ON SCHEMA ledger TO ledger_audit;
GRANT SELECT ON ledger.accounts, ledger.balances, ledger.transactions, ledger.entries,
  ledger.holds, ledger.token_lots, ledger.lot_consumptions TO ledger_audit;
CREATE POLICY audit_read ON ledger.accounts FOR SELECT TO ledger_audit USING (true);
CREATE POLICY audit_read ON ledger.balances FOR SELECT TO ledger_audit USING (true);
CREATE POLICY audit_read ON ledger.transactions FOR SELECT TO ledger_audit USING (true);
CREATE POLICY audit_read ON ledger.entries FOR SELECT TO ledger_audit USING (true);
CREATE POLICY audit_read ON ledger.holds FOR SELECT TO ledger_audit USING (true);
CREATE POLICY audit_read ON ledger.token_lots FOR SELECT TO ledger_audit USING (true);
CREATE POLICY audit_read ON ledger.lot_consumptions FOR SELECT TO ledger_audit USING (true);
GRANT ledger_audit TO ledger_service WITH INHERIT FALSE;
GRANT ledger_audit TO CURRENT_USER WITH INHERIT FALSE;

-- +goose Down
DROP POLICY audit_read ON ledger.lot_consumptions;
DROP POLICY audit_read ON ledger.token_lots;
DROP POLICY audit_read ON ledger.holds;
DROP POLICY audit_read ON ledger.entries;
DROP POLICY audit_read ON ledger.transactions;
DROP POLICY audit_read ON ledger.balances;
DROP POLICY audit_read ON ledger.accounts;
REVOKE SELECT ON ledger.accounts, ledger.balances, ledger.transactions, ledger.entries,
  ledger.holds, ledger.token_lots, ledger.lot_consumptions FROM ledger_audit;
REVOKE USAGE ON SCHEMA ledger FROM ledger_audit;
DROP ROLE ledger_audit;
DROP TABLE ledger.rebuild_runs;
