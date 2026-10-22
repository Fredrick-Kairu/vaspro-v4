package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
)

// signingKeysKMSKeyID must match identity.go's own constant — kept
// independent so this tool doesn't import the identity package (a
// long-running service) just for one constant string.
const signingKeysKMSKeyID = "identity-signing-keys"

func runIdentity(args []string) int {
	if len(args) == 0 || args[0] != "rotate-signing-key" {
		identityUsage()
		return 2
	}
	return rotateSigningKey(os.Stdout, os.Stderr)
}

func identityUsage() {
	_, _ = fmt.Fprintln(os.Stderr, "usage: vaspro identity rotate-signing-key")
	_, _ = fmt.Fprintln(os.Stderr, "  reads IDENTITY_DATABASE_URL and IDENTITY_SIGNING_KEYS_KMS_KEY from the environment")
}

// rotateSigningKey is an ops action, not a public HTTP endpoint (task
// 1.8's design: nothing public mutates credentials silently). It
// connects only to Postgres — no NATS/Redis/SMTP/WebAuthn, since
// app.Service.RotateSigningKey never touches any of those — and reuses
// the real app.Service logic rather than reimplementing the rotation
// steps here, so there's one place this behavior can diverge from.
func rotateSigningKey(stdout, stderr *os.File) int {
	ctx := context.Background()

	dsn := os.Getenv("IDENTITY_DATABASE_URL")
	if dsn == "" {
		_, _ = fmt.Fprintln(stderr, "vaspro: IDENTITY_DATABASE_URL is required")
		return 2
	}
	kmsKeyB64 := os.Getenv("IDENTITY_SIGNING_KEYS_KMS_KEY")
	if kmsKeyB64 == "" {
		_, _ = fmt.Fprintln(stderr, "vaspro: IDENTITY_SIGNING_KEYS_KMS_KEY is required")
		return 2
	}
	kmsKey, err := base64.StdEncoding.DecodeString(kmsKeyB64)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "vaspro: decoding IDENTITY_SIGNING_KEYS_KMS_KEY:", err)
		return 1
	}

	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "vaspro: connecting to the database:", err)
		return 1
	}
	defer pool.Close()

	kms, err := vascrypto.NewLocalKMS(map[string][]byte{signingKeysKMSKeyID: kmsKey})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "vaspro: building KMS:", err)
		return 1
	}

	svc := app.New(app.Deps{
		Pool:             pool,
		Clock:            clock.Real{},
		Envelope:         vascrypto.NewEnvelope(kms),
		SigningKeysKeyID: signingKeysKMSKeyID,
	})

	if err := svc.RotateSigningKey(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "vaspro: rotating the signing key:", err)
		return 1
	}

	kid, _, err := svc.ActiveSigningKey(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "vaspro: reading the new active signing key:", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "ok   rotated; new active kid:", kid)
	return 0
}
