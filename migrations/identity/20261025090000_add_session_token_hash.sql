-- +goose Up

-- data model §3's identity.sessions has no bearer-token column: the
-- session's own id was the bearer. But that id is also the public
-- handle listSessions returns and deleteSession takes in its URL, so
-- anyone holding one session could read, and then use, every other
-- session of the same user, and the id lands in access logs. Raised
-- with the user (Phase 1 exit gate, 2026-10-25); decided on a separate
-- random session token, stored only as its SHA-256 — the same
-- "random, so a plain hash is enough" reasoning recovery codes use.
--
-- Nullable, with no backfill: rows created before this migration have
-- no token, never match a lookup, and so simply stop authenticating
-- (those users sign in again). No existing data is rewritten.
-- A UNIQUE index allows any number of NULLs.

ALTER TABLE identity.sessions ADD COLUMN token_hash bytea;
CREATE UNIQUE INDEX sessions_token_hash ON identity.sessions (token_hash);

-- +goose Down

DROP INDEX identity.sessions_token_hash;
ALTER TABLE identity.sessions DROP COLUMN token_hash;
