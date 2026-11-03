-- +goose Up

-- Task 2.9 (OQ-10): VASPRO staff, an addition to data model §3. A user
-- is staff while a row exists here; rows are written only by the ops
-- CLI (vaspro identity grant-staff / revoke-staff), never by an API.
-- Global like users: no tenant_id, no RLS.
CREATE TABLE identity.staff_members (
  user_id     uuid PRIMARY KEY REFERENCES identity.users (id),
  granted_by  text NOT NULL,                     -- who ran the CLI (an operator name), for the audit trail
  granted_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE identity.staff_members;
