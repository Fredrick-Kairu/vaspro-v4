-- +goose Up

-- Task 3.3 (OQ-14), the posting API.
--
-- A capture records how much of the hold was used,
-- so a partial capture (380 of 400 SMS delivered) releases the rest at
-- once and keeps only the used part held until settlement (3.5). An
-- addition to data model §5.
--
-- What a hold contributes to its account's balances.held_minor:
--   active                          amount_minor
--   captured, settled_at IS NULL    captured_minor (posted by 3.5's settlement)
--   anything else                   nothing
-- Release and a capture carrying its own posting set settled_at.
ALTER TABLE ledger.holds
  ADD COLUMN captured_minor bigint,
  ADD CONSTRAINT holds_captured_iff_status CHECK ((status = 'captured') = (captured_minor IS NOT NULL)),
  ADD CONSTRAINT holds_captured_within_amount CHECK (captured_minor > 0 AND captured_minor <= amount_minor);

-- VASPRO's balances are summed from the journal (ADR 0010), and most
-- lines on VASPRO's accounts (revenue, tax payable, clearing) belong to
-- client postings, which 3.1's system_read policy hides from
-- ledger_system. It may read every line on VASPRO's own accounts, and
-- still no client account's lines. Read-only.
CREATE POLICY system_read_vaspro_lines ON ledger.entries FOR SELECT TO ledger_system
  USING (EXISTS (SELECT 1 FROM ledger.accounts a WHERE a.id = account_id AND a.tenant_id IS NULL));

-- +goose Down
DROP POLICY system_read_vaspro_lines ON ledger.entries;
ALTER TABLE ledger.holds
  DROP CONSTRAINT holds_captured_within_amount,
  DROP CONSTRAINT holds_captured_iff_status,
  DROP COLUMN captured_minor;
