-- +goose Up

-- Task 3.6 (OQ-17): hold expiry and auto-buy. Additions to data model §5.
ALTER TABLE ledger.auto_buy_rules
  ADD CONSTRAINT auto_buy_rules_below_tokens CHECK (below_tokens >= 0),
  ADD CONSTRAINT auto_buy_rules_buy_tokens CHECK (buy_tokens > 0);

-- ledger_settlement (3.5) is the role of the ledger's background jobs:
-- the expiry job also finds, across clients, active holds; it applies
-- its own clock's cutoff (expires_at plus a settlement window), so the
-- database's now() doesn't decide what expires. Each hold is then
-- expired in its own tenant's transaction.
CREATE POLICY expiry_read ON ledger.holds
  FOR SELECT TO ledger_settlement
  USING (status = 'active');

-- +goose Down
DROP POLICY expiry_read ON ledger.holds;
ALTER TABLE ledger.auto_buy_rules
  DROP CONSTRAINT auto_buy_rules_buy_tokens,
  DROP CONSTRAINT auto_buy_rules_below_tokens;
