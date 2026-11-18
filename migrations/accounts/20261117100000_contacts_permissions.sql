-- +goose Up

-- Task 6.2 (OQ-26): the contacts permissions for the built-in roles.
-- Admin's '*' already covers them. These rows are VASPRO's own seeded
-- reference data (20261103090000), not client data; custom roles are
-- untouched. Approved 17 Nov.
UPDATE accounts.roles SET permissions = permissions || '{contacts:read,contacts:manage}'
  WHERE id = '019a0000-0000-7000-8000-000000000002' AND tenant_id IS NULL;   -- Developer
UPDATE accounts.roles SET permissions = permissions || '{contacts:read}'
  WHERE id IN ('019a0000-0000-7000-8000-000000000003',                      -- Finance
               '019a0000-0000-7000-8000-000000000004') AND tenant_id IS NULL; -- Viewer

-- +goose Down
UPDATE accounts.roles
  SET permissions = array_remove(array_remove(permissions, 'contacts:read'), 'contacts:manage')
  WHERE id IN ('019a0000-0000-7000-8000-000000000002', '019a0000-0000-7000-8000-000000000003',
               '019a0000-0000-7000-8000-000000000004') AND tenant_id IS NULL;
