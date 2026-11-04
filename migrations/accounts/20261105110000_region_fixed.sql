-- +goose Up

-- Task 2.10 (OQ-11). Data model §4 gives tenants.region as the data
-- residency tag. Until hosting is decided (OD-02) nothing moves an
-- account's data, so the tag must never change after creation, or it
-- would claim the data lives somewhere it doesn't; moving an account
-- is task 21.4. A sub-account is always in its reseller's region. An
-- addition to data model §4, approved in 2.10's plan. No data changes.

-- +goose StatementBegin
CREATE FUNCTION accounts.refuse_region_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'tenant % can''t move from region % to %: an account''s region is fixed at creation',
    NEW.id, OLD.region, NEW.region
    USING ERRCODE = 'check_violation';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER tenants_region_fixed
  BEFORE UPDATE OF region ON accounts.tenants
  FOR EACH ROW
  WHEN (OLD.region IS DISTINCT FROM NEW.region)
  EXECUTE FUNCTION accounts.refuse_region_change();

-- +goose StatementBegin
CREATE FUNCTION accounts.require_parent_region() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  parent_region text;
BEGIN
  IF NEW.parent_tenant_id IS NULL THEN
    RETURN NEW;
  END IF;
  -- Runs as the inserting role, so RLS applies: the parent is visible
  -- inside the reseller's transaction (app.CreateSubAccount). A parent
  -- that can't be seen can't be checked, so the insert is refused.
  SELECT region INTO parent_region FROM accounts.tenants WHERE id = NEW.parent_tenant_id;
  IF parent_region IS NULL OR parent_region <> NEW.region THEN
    RAISE EXCEPTION 'sub-account % must be in its reseller''s region (%), not %',
      NEW.id, coalesce(parent_region, 'unknown'), NEW.region
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER tenants_sub_account_region
  BEFORE INSERT ON accounts.tenants
  FOR EACH ROW
  EXECUTE FUNCTION accounts.require_parent_region();

-- +goose Down
DROP TRIGGER tenants_sub_account_region ON accounts.tenants;
DROP FUNCTION accounts.require_parent_region();
DROP TRIGGER tenants_region_fixed ON accounts.tenants;
DROP FUNCTION accounts.refuse_region_change();
