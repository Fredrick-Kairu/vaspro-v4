package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/clock"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/db"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
)

// maxCredentialBytes bounds what set-rail-credential reads from stdin;
// Daraja keys, secrets and passkeys are well under it.
const maxCredentialBytes = 4096

func runPayments(args []string) int {
	if len(args) == 0 {
		paymentsUsage()
		return 2
	}
	switch args[0] {
	case "add-rail-account":
		return addRailAccount(args[1:], os.Stdout, os.Stderr)
	case "set-rail-credential":
		return setRailCredential(args[1:], os.Stdin, os.Stdout, os.Stderr)
	case "verify-rail-account":
		return verifyRailAccount(args[1:], os.Stdout, os.Stderr)
	default:
		paymentsUsage()
		return 2
	}
}

func paymentsUsage() {
	_, _ = fmt.Fprintln(os.Stderr, "usage: vaspro payments add-rail-account -by <staff email> -rail mpesa_ke -number <paybill> -name <registered name>")
	_, _ = fmt.Fprintln(os.Stderr, "       vaspro payments set-rail-credential -by <staff email> -account <id> -kind app_key|app_secret|passkey < secret")
	_, _ = fmt.Fprintln(os.Stderr, "       vaspro payments verify-rail-account -by <staff email> -account <id>")
	_, _ = fmt.Fprintln(os.Stderr, "  VASPRO's own top-up paybill (task 4.3). Reads PAYMENTS_DATABASE_URL and PAYMENTS_IDENTITY_GRPC_ADDR;")
	_, _ = fmt.Fprintln(os.Stderr, "  set-rail-credential and verify-rail-account also PAYMENTS_CREDENTIALS_KMS_KEY, set-rail-credential")
	_, _ = fmt.Fprintln(os.Stderr, "  PAYMENTS_CREDENTIAL_FINGERPRINT_KEY, and verify-rail-account DARAJA_BASE_URL. The secret is read")
	_, _ = fmt.Fprintln(os.Stderr, "  from stdin, never the command line, so it stays out of shell history and process lists.")
}

// paymentsService connects what the payments ops commands need. The
// keys are base64 of 32 bytes; empty ones are left out, for commands
// that don't use them.
func paymentsService(ctx context.Context, stderr io.Writer, needKMS, needFingerprint, needRail bool) (*app.Service, func(), bool) {
	env := func(name string, need bool) (string, bool) {
		v := os.Getenv(name)
		if need && v == "" {
			_, _ = fmt.Fprintf(stderr, "vaspro: %s is required\n", name)
			return "", false
		}
		return v, true
	}
	dsn, ok1 := env("PAYMENTS_DATABASE_URL", true)
	identityAddr, ok2 := env("PAYMENTS_IDENTITY_GRPC_ADDR", true)
	kmsKeyB64, ok3 := env("PAYMENTS_CREDENTIALS_KMS_KEY", needKMS)
	fpKeyB64, ok4 := env("PAYMENTS_CREDENTIAL_FINGERPRINT_KEY", needFingerprint)
	darajaURL, ok5 := env("DARAJA_BASE_URL", needRail)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return nil, nil, false
	}
	deps := app.Deps{Clock: clock.Real{}}
	if needKMS {
		key, err := base64.StdEncoding.DecodeString(kmsKeyB64)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "vaspro: decoding PAYMENTS_CREDENTIALS_KMS_KEY:", err)
			return nil, nil, false
		}
		kms, err := vascrypto.NewLocalKMS(map[string][]byte{app.CredentialsKMSKeyID: key})
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "vaspro: building KMS:", err)
			return nil, nil, false
		}
		deps.KMS = kms
	}
	if needFingerprint {
		key, err := base64.StdEncoding.DecodeString(fpKeyB64)
		if err != nil || len(key) != 32 {
			_, _ = fmt.Fprintln(stderr, "vaspro: PAYMENTS_CREDENTIAL_FINGERPRINT_KEY must be base64 of 32 bytes")
			return nil, nil, false
		}
		deps.FingerprintKey = key
	}
	pool, err := db.NewPool(ctx, dsn)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "vaspro: connecting to the database:", err)
		return nil, nil, false
	}
	identity, err := adapters.DialIdentity(identityAddr)
	if err != nil {
		pool.Close()
		_, _ = fmt.Fprintln(stderr, "vaspro:", err)
		return nil, nil, false
	}
	deps.Pool, deps.Identity = pool, identity
	if needRail {
		// The verification call is OAuth only, so no timestamp is made
		// and no token cached: UTC and no cache are enough here.
		deps.Rails = map[string]app.RailClient{"mpesa_ke": adapters.NewDaraja(adapters.DarajaConfig{
			BaseURL: darajaURL, Credentials: app.NewCredentialSource(pool, deps.KMS), Location: time.UTC, Clock: clock.Real{},
		})}
	}
	return app.New(deps), func() { _ = identity.Close(); pool.Close() }, true
}

func addRailAccount(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("add-rail-account", flag.ContinueOnError)
	fs.SetOutput(stderr)
	by := fs.String("by", "", "the staff member's email (required)")
	rail := fs.String("rail", "", "the rail, e.g. mpesa_ke (required)")
	number := fs.String("number", "", "the paybill number (required)")
	name := fs.String("name", "", "the organisation name registered on the rail (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *by == "" || *rail == "" || *number == "" || *name == "" || fs.NArg() != 0 {
		paymentsUsage()
		return 2
	}
	ctx := context.Background()
	svc, closeAll, ok := paymentsService(ctx, stderr, false, false, false)
	if !ok {
		return 1
	}
	defer closeAll()
	acct, err := svc.AddRailAccount(ctx, app.AddRailAccountRequest{StaffEmail: *by, RailCode: *rail, Number: *number, DisplayName: *name})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "vaspro: adding the paybill:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "ok   added %s paybill %s (%s), %s; id %s\n", acct.RailCode, acct.Number, acct.DisplayName, acct.Status, acct.ID)
	return 0
}

func setRailCredential(args []string, stdin io.Reader, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("set-rail-credential", flag.ContinueOnError)
	fs.SetOutput(stderr)
	by := fs.String("by", "", "the staff member's email (required)")
	account := fs.String("account", "", "the rail account's id (required)")
	kind := fs.String("kind", "", "app_key, app_secret or passkey (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	id, err := uuid.Parse(*account)
	if *by == "" || *kind == "" || err != nil || fs.NArg() != 0 {
		paymentsUsage()
		return 2
	}
	secret, err := io.ReadAll(io.LimitReader(stdin, maxCredentialBytes+1))
	if err != nil || len(secret) > maxCredentialBytes {
		_, _ = fmt.Fprintln(stderr, "vaspro: reading the credential from stdin failed or it's too long")
		return 1
	}
	secret = bytes.TrimRight(secret, "\r\n")
	ctx := context.Background()
	svc, closeAll, ok := paymentsService(ctx, stderr, true, true, false)
	if !ok {
		return 1
	}
	defer closeAll()
	if err := svc.SetRailCredential(ctx, app.SetRailCredentialRequest{StaffEmail: *by, RailAccountID: id, Kind: *kind, Secret: secret}); err != nil {
		_, _ = fmt.Fprintln(stderr, "vaspro: storing the credential:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "ok   stored %s for %s; the account is pending until verify-rail-account succeeds\n", *kind, id)
	return 0
}

func verifyRailAccount(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("verify-rail-account", flag.ContinueOnError)
	fs.SetOutput(stderr)
	by := fs.String("by", "", "the staff member's email (required)")
	account := fs.String("account", "", "the rail account's id (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	id, err := uuid.Parse(*account)
	if *by == "" || err != nil || fs.NArg() != 0 {
		paymentsUsage()
		return 2
	}
	ctx := context.Background()
	svc, closeAll, ok := paymentsService(ctx, stderr, true, false, true)
	if !ok {
		return 1
	}
	defer closeAll()
	if err := svc.VerifyRailAccount(ctx, *by, id); err != nil {
		_, _ = fmt.Fprintln(stderr, "vaspro: verifying the account:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "ok   %s verified and active\n", id)
	return 0
}
