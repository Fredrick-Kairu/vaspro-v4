-- +goose Up

-- Task 1.8's OAuth2 client-credentials token endpoint has to look up
-- an identity.api_clients row by a bare client_id, with no session
-- and so no tenant context to set — but api_clients has FORCE ROW
-- LEVEL SECURITY (1.1), which fails closed (zero rows, no error) with
-- no app.tenant_id set, regardless of the query's own WHERE clause.
-- Raised with the user: rather than BYPASSRLS on the whole service
-- (which would also blind RLS to any future bug in a tenant-scoped
-- code path, the exact class of mistake task 1.7 found and fixed),
-- decided on a narrow, SELECT-only, additional permissive policy for
-- one dedicated role. identity_token_exchange is NOLOGIN — it is never
-- a connection credential to provision or rotate; the one call site
-- (platform/db.Pool.WithRoleTx) reaches it via SET LOCAL ROLE from
-- whatever role the service's own connection already authenticated
-- as, reverting automatically at the end of that one transaction.
-- GRANT ... TO CURRENT_USER grants it to whichever role actually runs
-- this migration, which in every environment is the same role the
-- service later connects as (there's no way for this migration to
-- know that role's name otherwise).

CREATE ROLE identity_token_exchange NOLOGIN;
GRANT USAGE ON SCHEMA identity TO identity_token_exchange;
GRANT SELECT ON identity.api_clients, identity.api_client_secrets TO identity_token_exchange;

-- Postgres combines multiple permissive policies for the same command
-- with OR, so this is additive only for identity_token_exchange's own
-- SELECTs: tenant_isolation still fully applies to every other role,
-- and to every other command for this role too (it has no INSERT/
-- UPDATE/DELETE grant at all).
CREATE POLICY token_exchange_read ON identity.api_clients
  FOR SELECT TO identity_token_exchange
  USING (true);

-- WITH INHERIT FALSE matters: a default (inheriting) GRANT would make
-- identity_token_exchange's privileges ambiently active on every
-- query CURRENT_USER's connection ever runs, not just inside the one
-- db.Pool.WithRoleTx transaction — found by testing (a "plain
-- connection, no tenant set" check unexpectedly saw a cross-tenant row
-- until this was added). Non-inheriting membership still lets SET
-- LOCAL ROLE succeed; it just stops the privilege from applying
-- automatically without it.
GRANT identity_token_exchange TO CURRENT_USER WITH INHERIT FALSE;

-- Signing-key rotation (RotateSigningKey) needs to know *when* a key
-- was demoted from active to retiring, to later retire it once the
-- overlap window has passed — data-model.md's literal
-- identity.signing_keys has no such column.
ALTER TABLE identity.signing_keys
  ADD COLUMN retiring_since timestamptz;

-- +goose Down

ALTER TABLE identity.signing_keys
  DROP COLUMN retiring_since;

REVOKE identity_token_exchange FROM CURRENT_USER;
DROP POLICY token_exchange_read ON identity.api_clients;
REVOKE SELECT ON identity.api_clients, identity.api_client_secrets FROM identity_token_exchange;
REVOKE USAGE ON SCHEMA identity FROM identity_token_exchange;
DROP ROLE identity_token_exchange;
