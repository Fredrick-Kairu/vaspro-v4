-- +goose Up

-- Task 2.2c (decision Q3, 30 Oct): until now API clients and SMPP
-- accounts were created with the creating user's own ID as tenant_id, a
-- placeholder until real accounts existed (1.7, 1.10). From 2.2c they
-- belong to the session's selected account, so every row created
-- before this migration is a placeholder: revoke the API clients and
-- their secrets, and suspend the SMPP accounts. Their tenant_id matches
-- no real account, so the API can't reach them again. Only development
-- and test data exists (STATUS, Q3). Tokens already minted expire
-- within the hour, as for any revocation (1.8).
--
-- Both tables have FORCE ROW LEVEL SECURITY, which applies to the table
-- owner too: run as an owner that isn't a superuser, with no
-- app.tenant_id set, these updates would match no rows and succeed
-- silently (STATUS notes, 1.7). FORCE is lifted for this transaction's
-- updates only and restored before it commits.
ALTER TABLE identity.api_clients NO FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.smpp_accounts NO FORCE ROW LEVEL SECURITY;

UPDATE identity.api_client_secrets SET revoked_at = now() WHERE revoked_at IS NULL;
UPDATE identity.api_clients SET revoked_at = now() WHERE revoked_at IS NULL;
UPDATE identity.smpp_accounts SET status = 'suspended' WHERE status = 'active';

ALTER TABLE identity.api_clients FORCE ROW LEVEL SECURITY;
ALTER TABLE identity.smpp_accounts FORCE ROW LEVEL SECURITY;

-- +goose Down

-- Nothing to undo: which rows were active before can't be recovered,
-- and the placeholders must not come back.
SELECT 1;
