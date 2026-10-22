-- +goose Up

-- Both system design §3 ("API client record: name, environment,
-- scopes, IP allowlist, expiry date, spend cap, and up to two active
-- secrets") and ROADMAP task 1.7's own wording call for a spend cap
-- on identity.api_clients, but data-model.md's literal CREATE TABLE
-- has no such column. Raised with the user per
-- .claude/rules/migrations.md ("if you must differ, stop and ask");
-- decided on explicit typed columns (data model Part 1's own rule:
-- "money as bigint minor units + currency"), not a value invented by
-- either doc. Both nullable: null means no cap.

ALTER TABLE identity.api_clients
  ADD COLUMN spend_cap_minor    bigint,
  ADD COLUMN spend_cap_currency text;

-- +goose Down

ALTER TABLE identity.api_clients
  DROP COLUMN spend_cap_minor,
  DROP COLUMN spend_cap_currency;
