-- +goose Up

-- Task 3.7 (OQ-18): reversals with maker-checker. Additions to data
-- model §5.
--
-- kind says how the money goes back: 'payback' through the rail (the
-- reversal credits reversal_clearing until Payments reports the
-- pay-back), or 'chargeback' / 'operator', where the rail already moved
-- it and the reversal credits provider_clearing directly. The maker
-- names the service wallets to claw unused tokens back from, in order.
-- A rejection records who rejected it and why, never as approved_by.
ALTER TABLE ledger.reversals
  ADD COLUMN kind text NOT NULL CHECK (kind IN ('payback', 'chargeback', 'operator')),
  ADD COLUMN currency char(3) NOT NULL REFERENCES ledger.currencies (code),
  ADD COLUMN clawback_services text[] NOT NULL DEFAULT '{}',
  ADD COLUMN rejected_by uuid,
  ADD COLUMN rejection_reason text,
  ADD COLUMN payback_transaction_id uuid REFERENCES ledger.transactions (id),
  ADD COLUMN payback_attempts integer NOT NULL DEFAULT 0 CHECK (payback_attempts >= 0),
  ADD COLUMN last_payback_error text,
  ADD CONSTRAINT reversals_rejector CHECK (rejected_by IS NULL OR rejected_by <> requested_by),
  ADD CONSTRAINT reversals_requested CHECK (status <> 'requested' OR
    (approved_by IS NULL AND rejected_by IS NULL AND rejection_reason IS NULL AND reversal_transaction_id IS NULL AND decided_at IS NULL)),
  ADD CONSTRAINT reversals_rejected CHECK (status <> 'rejected' OR
    (rejected_by IS NOT NULL AND rejection_reason IS NOT NULL AND approved_by IS NULL AND reversal_transaction_id IS NULL AND decided_at IS NOT NULL)),
  ADD CONSTRAINT reversals_posted CHECK (status NOT IN ('posted', 'payback_pending', 'paid_back') OR
    (approved_by IS NOT NULL AND rejected_by IS NULL AND reversal_transaction_id IS NOT NULL AND decided_at IS NOT NULL)),
  ADD CONSTRAINT reversals_paid_back CHECK ((status = 'paid_back') = (payback_transaction_id IS NOT NULL)),
  ADD CONSTRAINT reversals_kind_status CHECK (
    (kind = 'payback' AND status <> 'posted') OR (kind <> 'payback' AND status NOT IN ('payback_pending', 'paid_back')));
CREATE INDEX reversals_original ON ledger.reversals (original_transaction_id);

-- Staff list reversals across clients (the maker-checker queue), which
-- the tenant policy forbids: a narrow NOLOGIN role, reached only through
-- db.Pool.WithRoleTx and granted WITH INHERIT FALSE (1.8's lesson), like
-- ledger_price_review. Deciding runs in the client's own transaction.
CREATE ROLE ledger_reversal_review NOLOGIN;
GRANT USAGE ON SCHEMA ledger TO ledger_reversal_review;
GRANT SELECT ON ledger.reversals TO ledger_reversal_review;
CREATE POLICY reversal_review_read ON ledger.reversals
  FOR SELECT TO ledger_reversal_review
  USING (true);
GRANT ledger_reversal_review TO ledger_service WITH INHERIT FALSE;
GRANT ledger_reversal_review TO CURRENT_USER WITH INHERIT FALSE;

-- +goose Down
DROP POLICY reversal_review_read ON ledger.reversals;
REVOKE SELECT ON ledger.reversals FROM ledger_reversal_review;
REVOKE USAGE ON SCHEMA ledger FROM ledger_reversal_review;
DROP ROLE ledger_reversal_review;
DROP INDEX ledger.reversals_original;
ALTER TABLE ledger.reversals
  DROP CONSTRAINT reversals_kind_status,
  DROP CONSTRAINT reversals_paid_back,
  DROP CONSTRAINT reversals_posted,
  DROP CONSTRAINT reversals_rejected,
  DROP CONSTRAINT reversals_requested,
  DROP CONSTRAINT reversals_rejector,
  DROP COLUMN last_payback_error,
  DROP COLUMN payback_attempts,
  DROP COLUMN payback_transaction_id,
  DROP COLUMN rejection_reason,
  DROP COLUMN rejected_by,
  DROP COLUMN clawback_services,
  DROP COLUMN currency,
  DROP COLUMN kind;
