-- +goose Up

-- Task 2.3a: the built-in roles besides Admin (seeded in 20261030160000),
-- over the permission catalogue in domain/permissions.go (OQ-05, agreed
-- 2 Nov). Built-in roles have tenant_id NULL: readable by every tenant,
-- writable by none (2.1's roles policy), so they're seeded here with
-- fixed IDs code can name. Admin keeps '*'.
INSERT INTO accounts.roles (id, tenant_id, name, permissions) VALUES
  ('019a0000-0000-7000-8000-000000000002', NULL, 'Developer', '{account:read,members:read,credentials:read,credentials:manage}'),
  ('019a0000-0000-7000-8000-000000000003', NULL, 'Finance',   '{account:read,members:read,billing:read,billing:manage}'),
  ('019a0000-0000-7000-8000-000000000004', NULL, 'Viewer',    '{account:read,members:read,credentials:read,billing:read}');

-- "Which accounts am I in?" reads memberships across tenants, which the
-- tenant policies forbid. A narrow NOLOGIN role, reached only through
-- db.Pool.WithRoleTx and granted WITH INHERIT FALSE so it never applies
-- outside that transaction (1.8's lesson), may read the caller's own
-- memberships (app.user_id, set in the same transaction) and the
-- accounts they belong to. An addition to data model §4, flagged in
-- 2.1's notes. The service's own login role needs the same grant where
-- it isn't the role running migrations.
CREATE ROLE accounts_membership_lookup NOLOGIN;
GRANT USAGE ON SCHEMA accounts TO accounts_membership_lookup;
GRANT SELECT ON accounts.memberships, accounts.tenants, accounts.roles TO accounts_membership_lookup;
CREATE POLICY membership_lookup_own ON accounts.memberships
  FOR SELECT TO accounts_membership_lookup
  USING (user_id = nullif(current_setting('app.user_id', true), '')::uuid);
CREATE POLICY membership_lookup_tenants ON accounts.tenants
  FOR SELECT TO accounts_membership_lookup
  USING (id IN (SELECT tenant_id FROM accounts.memberships
                WHERE user_id = nullif(current_setting('app.user_id', true), '')::uuid));
GRANT accounts_membership_lookup TO CURRENT_USER WITH INHERIT FALSE;

-- +goose Down
DROP POLICY membership_lookup_tenants ON accounts.tenants;
DROP POLICY membership_lookup_own ON accounts.memberships;
REVOKE SELECT ON accounts.memberships, accounts.tenants, accounts.roles FROM accounts_membership_lookup;
REVOKE USAGE ON SCHEMA accounts FROM accounts_membership_lookup;
DROP ROLE accounts_membership_lookup;
DELETE FROM accounts.roles WHERE id IN (
  '019a0000-0000-7000-8000-000000000002',
  '019a0000-0000-7000-8000-000000000003',
  '019a0000-0000-7000-8000-000000000004');
