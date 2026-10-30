-- +goose Up

-- Task 2.2a: the built-in Admin role every new account's creator gets.
-- Built-in roles have tenant_id NULL: readable by every tenant, writable
-- by none (the roles RLS policy, 2.1), so they're seeded here. The ID is
-- fixed so code can name it (domain.BuiltinAdminRoleID). '*' means every
-- permission; task 2.3 defines the real role × permission matrix and the
-- other built-in roles, and may replace it.
INSERT INTO accounts.roles (id, tenant_id, name, permissions)
VALUES ('019a0000-0000-7000-8000-000000000001', NULL, 'Admin', '{*}');

-- +goose Down
DELETE FROM accounts.roles WHERE id = '019a0000-0000-7000-8000-000000000001';
