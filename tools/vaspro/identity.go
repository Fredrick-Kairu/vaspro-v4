package main

import (
	"context"
	"encoding/base64"
	"flag"
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
	if len(args) == 0 {
		identityUsage()
		return 2
	}
	switch args[0] {
	case "rotate-signing-key":
		return rotateSigningKey(os.Stdout, os.Stderr)
	case "grant-staff", "revoke-staff", "grant-finance-lead", "revoke-finance-lead":
		return changeStaff(args[0], args[1:], os.Stdout, os.Stderr)
	default:
		identityUsage()
		return 2
	}
}

func identityUsage() {
	_, _ = fmt.Fprintln(os.Stderr, "usage: vaspro identity rotate-signing-key")
	_, _ = fmt.Fprintln(os.Stderr, "       vaspro identity grant-staff -by <operator> <email>")
	_, _ = fmt.Fprintln(os.Stderr, "       vaspro identity revoke-staff -by <operator> <email>")
	_, _ = fmt.Fprintln(os.Stderr, "       vaspro identity grant-finance-lead -by <operator> <email>")
	_, _ = fmt.Fprintln(os.Stderr, "       vaspro identity revoke-finance-lead -by <operator> <email>")
	_, _ = fmt.Fprintln(os.Stderr, "  reads IDENTITY_DATABASE_URL (and, to rotate, IDENTITY_SIGNING_KEYS_KMS_KEY) from the environment")
}

// changeStaff grants or revokes VASPRO staff (task 2.9, OQ-10) or a
// staff member's finance-lead flag (task 3.7, OQ-18). It is the only
// way anyone becomes either: there is no API for it. -by names
// the operator running it, for the security event.
func changeStaff(action string, args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	by := fs.String("by", "", "the operator making the change (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *by == "" || fs.NArg() != 1 {
		identityUsage()
		return 2
	}
	email := fs.Arg(0)

	dsn := os.Getenv("IDENTITY_DATABASE_URL")
	if dsn == "" {
		_, _ = fmt.Fprintln(stderr, "vaspro: IDENTITY_DATABASE_URL is required")
		return 2
	}
	ctx := context.Background()
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "vaspro: connecting to the database:", err)
		return 1
	}
	defer pool.Close()

	svc := app.New(app.Deps{Pool: pool, Clock: clock.Real{}})
	change := map[string]func(context.Context, string, string) error{
		"grant-staff": svc.GrantStaff, "revoke-staff": svc.RevokeStaff,
		"grant-finance-lead": svc.GrantFinanceLead, "revoke-finance-lead": svc.RevokeFinanceLead,
	}[action]
	if err := change(ctx, email, *by); err != nil {
		_, _ = fmt.Fprintf(stderr, "vaspro: %s: %v\n", action, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "ok   %s %s\n", action, email)
	return 0
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
