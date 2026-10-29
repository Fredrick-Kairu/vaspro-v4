-- +goose Up

-- data-model.md's identity.user_passkeys only lists credential_id,
-- public_key, sign_count, name, created_at, last_used_at. The
-- go-webauthn/webauthn library (ROADMAP §6) unconditionally checks a
-- stored "flags" byte (backup-eligible/backup-state/user-verified)
-- against every login response — without persisting it, every login
-- after registration would fail that check. Raised with the user per
-- .claude/rules/migrations.md ("if you must differ, stop and ask");
-- decided on explicit typed columns over one opaque blob column,
-- matching this repo's style elsewhere and the library's own
-- documented "recommended" persistence shape (see its webauthn
-- package doc, "Storage" section). No RLS: user_passkeys has no
-- tenant_id, same as users/user_totp — user-owned, not tenant-owned.

ALTER TABLE identity.user_passkeys
  ADD COLUMN flags              smallint NOT NULL DEFAULT 0,  -- CredentialFlags.ProtocolValue() octet
  ADD COLUMN attestation_type   text NOT NULL DEFAULT '',
  ADD COLUMN attestation_format text NOT NULL DEFAULT '',
  ADD COLUMN transports         text[] NOT NULL DEFAULT '{}',
  ADD COLUMN clone_warning      boolean NOT NULL DEFAULT false; -- set when a sign-count regression is detected

-- +goose Down

ALTER TABLE identity.user_passkeys
  DROP COLUMN flags,
  DROP COLUMN attestation_type,
  DROP COLUMN attestation_format,
  DROP COLUMN transports,
  DROP COLUMN clone_warning;
