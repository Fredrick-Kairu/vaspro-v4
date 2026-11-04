-- +goose Up

-- Task 3.4b: staff list and find token prices across every client (the
-- maker-checker queue), which token_prices' tenant policies forbid. A
-- narrow NOLOGIN role, reached only through db.Pool.WithRoleTx and
-- granted WITH INHERIT FALSE (1.8's lesson), may read token_prices and
-- nothing else; approving or withdrawing then runs in the price's own
-- tenant's transaction (ledger_system's for a list price), as 2.9's
-- staff do. An addition to data model §5, like accounts_staff_review.
CREATE ROLE ledger_price_review NOLOGIN;
GRANT USAGE ON SCHEMA ledger TO ledger_price_review;
GRANT SELECT ON ledger.token_prices TO ledger_price_review;
CREATE POLICY price_review_read ON ledger.token_prices
  FOR SELECT TO ledger_price_review
  USING (true);
GRANT ledger_price_review TO ledger_service WITH INHERIT FALSE;
GRANT ledger_price_review TO CURRENT_USER WITH INHERIT FALSE;

-- +goose Down
DROP POLICY price_review_read ON ledger.token_prices;
REVOKE SELECT ON ledger.token_prices FROM ledger_price_review;
REVOKE USAGE ON SCHEMA ledger FROM ledger_price_review;
DROP ROLE ledger_price_review;
