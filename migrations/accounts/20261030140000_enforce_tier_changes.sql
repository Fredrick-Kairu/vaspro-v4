-- +goose Up

-- Task 2.5. Data model §4: "A tenant's tier only changes through
-- tier_changes, written in the same transaction as the update to
-- tenants.tier." App code (app.Service.ChangeTier) does this; these
-- triggers make the database refuse anything else, so a forgotten
-- tier_changes row can't slip through another code path. An addition
-- to data model §4, approved in 2.5's plan.

-- +goose StatementBegin
CREATE FUNCTION accounts.require_tier_change_row() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  -- now() is the transaction's start time, the same for every
  -- statement in it, and tier_changes.created_at defaults to it — so
  -- a matching row with created_at = now() was written in this
  -- transaction. (Nothing may set tier_changes.created_at explicitly.)
  IF NOT EXISTS (
    SELECT 1 FROM accounts.tier_changes tc
    WHERE tc.tenant_id = NEW.id
      AND tc.from_tier = OLD.tier
      AND tc.to_tier = NEW.tier
      AND tc.created_at = now()
  ) THEN
    RAISE EXCEPTION 'tenant % changed tier from % to % without a tier_changes row in the same transaction',
      NEW.id, OLD.tier, NEW.tier
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END
$$;
-- +goose StatementEnd

-- Deferred to commit, so the tier_changes row may be written before or
-- after the update.
CREATE CONSTRAINT TRIGGER tenants_tier_change_recorded
  AFTER UPDATE OF tier ON accounts.tenants
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW
  WHEN (OLD.tier IS DISTINCT FROM NEW.tier)
  EXECUTE FUNCTION accounts.require_tier_change_row();

-- +goose StatementBegin
CREATE FUNCTION accounts.require_sandbox_at_creation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  -- Every tenant starts in the sandbox (system design §2); any higher
  -- tier is reached through tier_changes, the v3 import included.
  IF NEW.tier <> 'sandbox' THEN
    RAISE EXCEPTION 'tenant % must be created in the sandbox tier, not %', NEW.id, NEW.tier
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER tenants_start_in_sandbox
  BEFORE INSERT ON accounts.tenants
  FOR EACH ROW
  EXECUTE FUNCTION accounts.require_sandbox_at_creation();

-- +goose Down
DROP TRIGGER tenants_start_in_sandbox ON accounts.tenants;
DROP FUNCTION accounts.require_sandbox_at_creation();
DROP TRIGGER tenants_tier_change_recorded ON accounts.tenants;
DROP FUNCTION accounts.require_tier_change_row();
