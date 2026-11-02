-- +goose Up

-- Task 2.2b: a sub-account always has a parent and nothing else has one
-- (an addition to data model §4, approved in the 2.2b plan). Whether the
-- parent is a reseller is the app's check, made under a lock on the
-- parent row (app.CreateSubAccount). Every existing row is direct with
-- no parent, so nothing is rewritten.
ALTER TABLE accounts.tenants
  ADD CONSTRAINT tenants_parent_iff_sub_account
  CHECK ((type = 'sub_account') = (parent_tenant_id IS NOT NULL));

-- +goose Down
ALTER TABLE accounts.tenants DROP CONSTRAINT tenants_parent_iff_sub_account;
