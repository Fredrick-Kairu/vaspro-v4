-- +goose Up

-- Task 2.9 (OQ-10): the staff review queue lists checks across
-- accounts, which the tenant policies forbid. A narrow NOLOGIN role,
-- reached only through db.Pool.WithRoleTx and granted WITH INHERIT
-- FALSE (1.8's lesson), may read kyc_checks and the accounts that have
-- a check, and nothing else. It reads every check, not only those in
-- review, so a decision on a check someone already decided can say so
-- (VAL-4010) rather than "not found". Decisions and every other staff
-- write run in the target account's own transaction. An addition to
-- data model §4, like 2.3a's, 2.6's and 2.7's roles; the service's own
-- login role needs the same grant where it isn't the role running
-- migrations.
CREATE ROLE accounts_staff_review NOLOGIN;
GRANT USAGE ON SCHEMA accounts TO accounts_staff_review;
GRANT SELECT ON accounts.kyc_checks, accounts.tenants TO accounts_staff_review;
CREATE POLICY staff_review_checks ON accounts.kyc_checks
  FOR SELECT TO accounts_staff_review
  USING (true);
CREATE POLICY staff_review_tenants ON accounts.tenants
  FOR SELECT TO accounts_staff_review
  USING (id IN (SELECT tenant_id FROM accounts.kyc_checks));
GRANT accounts_staff_review TO CURRENT_USER WITH INHERIT FALSE;

-- +goose Down
DROP POLICY staff_review_tenants ON accounts.tenants;
DROP POLICY staff_review_checks ON accounts.kyc_checks;
REVOKE SELECT ON accounts.kyc_checks, accounts.tenants FROM accounts_staff_review;
REVOKE USAGE ON SCHEMA accounts FROM accounts_staff_review;
DROP ROLE accounts_staff_review;
