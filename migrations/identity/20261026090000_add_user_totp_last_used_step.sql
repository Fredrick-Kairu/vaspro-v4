-- +goose Up

-- data model §3's identity.user_totp has no record of which codes were
-- used, so a valid TOTP code could be accepted any number of times
-- inside its ±1-step window (~90s). RFC 6238 §5.2 says a verifier must
-- not accept the second attempt of an OTP after the first succeeded.
-- Raised with the user (2026-10-26); decided on a column here rather
-- than a Redis key, so a Redis flush can't reopen the window.
--
-- last_used_step is the 30-second time step (unix seconds / 30) of the
-- last accepted code. A code is accepted only for a later step, claimed
-- with one conditional UPDATE so two concurrent uses can't both win.
-- Nullable, no backfill: NULL means no code has been used yet.

ALTER TABLE identity.user_totp ADD COLUMN last_used_step bigint;

-- +goose Down

ALTER TABLE identity.user_totp DROP COLUMN last_used_step;
