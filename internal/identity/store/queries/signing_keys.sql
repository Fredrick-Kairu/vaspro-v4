-- name: CreateSigningKey :one
INSERT INTO identity.signing_keys (kid, algorithm, public_key, private_ref, status)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListActiveSigningKeys :many
SELECT * FROM identity.signing_keys WHERE status = 'active' ORDER BY created_at;

-- name: GetActiveSigningKey :one
-- RotateSigningKey/the token endpoint both need exactly the one
-- current signing key; the rotation logic guarantees at most one row
-- ever has status = 'active'.
SELECT * FROM identity.signing_keys WHERE status = 'active';

-- name: ListPublishableSigningKeys :many
-- The JWKS endpoint publishes active and retiring keys — retiring so a
-- token signed just before rotation still verifies until it expires —
-- never retired, and never any private material (public_key only is
-- used by the caller; private_ref stays in the row but unused here).
SELECT * FROM identity.signing_keys WHERE status IN ('active', 'retiring') ORDER BY created_at;

-- name: DemoteSigningKeyToRetiring :exec
UPDATE identity.signing_keys
SET status = 'retiring', retiring_since = @now::timestamptz
WHERE kid = @kid AND status = 'active';

-- name: RetireStaleSigningKeys :execrows
-- The "sweep on rotate" — called every time RotateSigningKey runs,
-- rather than a separate scheduler, so no new background-job
-- infrastructure is needed for this.
UPDATE identity.signing_keys
SET status = 'retired'
WHERE status = 'retiring' AND retiring_since < @cutoff::timestamptz;
