-- +goose Up

-- Task 3.5 (OQ-16): usage settlement. Additions to data model §5.
--
-- A token capture records how many billable events (delivered
-- messages) it covers; settlement sums them into the batch. A settled
-- hold names the batch that posted it, so per-message rows can be
-- tagged with the batch (data model §5, "Holds are not journal
-- entries").
ALTER TABLE ledger.holds
  ADD COLUMN captured_events bigint,
  ADD COLUMN usage_batch_id uuid REFERENCES ledger.usage_batches (id),
  ADD CONSTRAINT holds_captured_events_iff CHECK ((status = 'captured' AND tokens IS NOT NULL) = (captured_events IS NOT NULL)),
  ADD CONSTRAINT holds_captured_events_positive CHECK (captured_events > 0);

ALTER TABLE ledger.usage_batches
  ADD CONSTRAINT usage_batches_window CHECK (window_end > window_start),
  ADD CONSTRAINT usage_batches_events CHECK (events >= 0),
  ADD CONSTRAINT usage_batches_tokens CHECK (tokens > 0);

-- Settlement runs across clients: a narrow NOLOGIN role, reached only
-- through db.Pool.WithRoleTx and granted WITH INHERIT FALSE (1.8's
-- lesson), finds the wallets with captured, unsettled token holds and
-- sees nothing else. Each wallet is then settled in its own tenant's
-- transaction (2.9's pattern).
CREATE ROLE ledger_settlement NOLOGIN;
GRANT USAGE ON SCHEMA ledger TO ledger_settlement;
GRANT SELECT ON ledger.holds TO ledger_settlement;
CREATE POLICY settlement_read ON ledger.holds
  FOR SELECT TO ledger_settlement
  USING (status = 'captured' AND settled_at IS NULL AND tokens IS NOT NULL);
GRANT ledger_settlement TO ledger_service WITH INHERIT FALSE;
GRANT ledger_settlement TO CURRENT_USER WITH INHERIT FALSE;

-- The ledger syncs each country pack's VAT rate into tax_rates at
-- startup, as ledger_system (tax_rates is global, like currencies).
GRANT INSERT ON ledger.tax_rates TO ledger_system;

-- +goose Down
REVOKE INSERT ON ledger.tax_rates FROM ledger_system;
DROP POLICY settlement_read ON ledger.holds;
REVOKE SELECT ON ledger.holds FROM ledger_settlement;
REVOKE USAGE ON SCHEMA ledger FROM ledger_settlement;
DROP ROLE ledger_settlement;
ALTER TABLE ledger.usage_batches
  DROP CONSTRAINT usage_batches_tokens,
  DROP CONSTRAINT usage_batches_events,
  DROP CONSTRAINT usage_batches_window;
ALTER TABLE ledger.holds
  DROP CONSTRAINT holds_captured_events_positive,
  DROP CONSTRAINT holds_captured_events_iff,
  DROP COLUMN usage_batch_id,
  DROP COLUMN captured_events;
