-- +goose Up

-- Task 2.6: a vendor's KYC webhook names a check by its provider
-- reference and carries no account, so finding the check reads across
-- accounts, which kyc_checks' tenant policy forbids. A narrow NOLOGIN
-- role, reached only through db.Pool.WithRoleTx and granted WITH
-- INHERIT FALSE so it never applies outside that transaction (1.8's
-- lesson), may read kyc_checks and nothing else. The update that
-- follows runs in the check's own account transaction. An addition to
-- data model §4, like 2.3a's membership lookup. The service's own
-- login role needs the same grant where it isn't the role running
-- migrations.
CREATE ROLE accounts_kyc_webhook NOLOGIN;
GRANT USAGE ON SCHEMA accounts TO accounts_kyc_webhook;
GRANT SELECT ON accounts.kyc_checks TO accounts_kyc_webhook;
CREATE POLICY kyc_webhook_lookup ON accounts.kyc_checks
  FOR SELECT TO accounts_kyc_webhook
  USING (true);
GRANT accounts_kyc_webhook TO CURRENT_USER WITH INHERIT FALSE;

-- +goose Down
DROP POLICY kyc_webhook_lookup ON accounts.kyc_checks;
REVOKE SELECT ON accounts.kyc_checks FROM accounts_kyc_webhook;
REVOKE USAGE ON SCHEMA accounts FROM accounts_kyc_webhook;
DROP ROLE accounts_kyc_webhook;
