-- name: CreateUserTOTP :one
INSERT INTO identity.user_totp (user_id, secret_ciphertext)
VALUES ($1, $2)
RETURNING *;

-- name: GetUserTOTP :one
SELECT * FROM identity.user_totp WHERE user_id = $1;

-- name: UpsertUserTOTP :one
-- Re-enrolling (scanning a fresh QR code) replaces any pending secret
-- and resets confirmed_at — a half-finished enrollment must not leave a
-- stale secret that could confirm later by accident. last_used_step is
-- cleared too: no code of the new secret has been used.
INSERT INTO identity.user_totp (user_id, secret_ciphertext, recovery_code_hashes)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE SET
  secret_ciphertext = EXCLUDED.secret_ciphertext,
  confirmed_at = NULL,
  recovery_code_hashes = EXCLUDED.recovery_code_hashes,
  last_used_step = NULL
RETURNING *;

-- name: ConfirmUserTOTP :exec
UPDATE identity.user_totp SET confirmed_at = now() WHERE user_id = $1;

-- name: ConsumeRecoveryCode :one
-- Atomic single-use: only removes (and returns a row) if codeHash was
-- actually present, so a race between two concurrent uses of the same
-- code can't both succeed. recovery_code_hashes is text[] (data model
-- §3) — hashes are stored hex-encoded, not raw bytea.
UPDATE identity.user_totp
SET recovery_code_hashes = array_remove(recovery_code_hashes, @code_hash::text)
WHERE user_id = @user_id AND @code_hash::text = ANY(recovery_code_hashes)
RETURNING *;

-- name: ClaimTOTPStep :execrows
-- RFC 6238 §5.2 replay protection: records step as used only if it's
-- later than the last one used. 0 rows means the code (or a newer one)
-- was already used. Atomic, so two concurrent uses of one code can't
-- both succeed — the same reasoning as ConsumeRecoveryCode.
UPDATE identity.user_totp
SET last_used_step = @step::bigint
WHERE user_id = @user_id
  AND (last_used_step IS NULL OR last_used_step < @step::bigint);
