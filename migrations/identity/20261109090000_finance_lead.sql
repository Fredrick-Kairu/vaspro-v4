-- +goose Up

-- Task 3.7 (OQ-18): a finance lead is a staff member who may approve a
-- reversal at or above the country pack's threshold (system design
-- §4.3). Set only by the ops CLI (vaspro identity grant-finance-lead /
-- revoke-finance-lead), like staff itself. An addition to data model §3.
ALTER TABLE identity.staff_members
  ADD COLUMN finance_lead boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE identity.staff_members DROP COLUMN finance_lead;
