//go:build integration

package app_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/payments/app"
	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
)

func TestRailAccountOps(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if err := h.svc.SyncRails(ctx); err != nil {
		t.Fatal(err)
	}
	staffID := h.staff.Add("ops@example.com")
	add := app.AddRailAccountRequest{StaffEmail: "ops@example.com", RailCode: "mpesa_ke", Number: darajaApp.ShortCode, DisplayName: "VASPRO"}

	t.Run("only staff add a paybill", func(t *testing.T) {
		bad := add
		bad.StaffEmail = "client@example.com"
		_, err := h.svc.AddRailAccount(ctx, bad)
		wantErr(t, err, app.ErrNotStaff)
		if n := count(t, h, `SELECT count(*) FROM rail_accounts`); n != 0 {
			t.Fatal("a refused add wrote an account")
		}
	})
	for name, mutate := range map[string]func(*app.AddRailAccountRequest){
		"an unknown rail": func(r *app.AddRailAccountRequest) { r.RailCode = "nope" },
		"no number":       func(r *app.AddRailAccountRequest) { r.Number = " " },
		"no name":         func(r *app.AddRailAccountRequest) { r.DisplayName = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := add
			mutate(&bad)
			_, err := h.svc.AddRailAccount(ctx, bad)
			wantErr(t, err, app.ErrValidation)
		})
	}

	acct, err := h.svc.AddRailAccount(ctx, add)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Status != "pending_verification" || acct.AccountType != "paybill" || acct.Number != darajaApp.ShortCode {
		t.Fatalf("added %+v", acct)
	}
	if n := count(t, h, `SELECT count(*) FROM rail_accounts WHERE id = $1 AND tenant_id IS NULL AND created_by = $2 AND uses = '{vaspro_topups}'`, acct.ID, staffID); n != 1 {
		t.Fatal("the paybill isn't VASPRO's top-up paybill created by the staff member")
	}

	set := func(kind, secret string) error {
		return h.svc.SetRailCredential(ctx, app.SetRailCredentialRequest{StaffEmail: "ops@example.com", RailAccountID: acct.ID, Kind: kind, Secret: []byte(secret)})
	}

	t.Run("credential refusals", func(t *testing.T) {
		err := h.svc.SetRailCredential(ctx, app.SetRailCredentialRequest{StaffEmail: "client@example.com", RailAccountID: acct.ID, Kind: "app_key", Secret: []byte("x")})
		wantErr(t, err, app.ErrNotStaff)
		wantErr(t, set("certificate", "x"), app.ErrValidation)
		wantErr(t, set("app_key", ""), app.ErrValidation)
		err = h.svc.SetRailCredential(ctx, app.SetRailCredentialRequest{StaffEmail: "ops@example.com", RailAccountID: uuid.New(), Kind: "app_key", Secret: []byte("x")})
		wantErr(t, err, app.ErrNotFound)
		if n := count(t, h, `SELECT count(*) FROM rail_credentials`); n != 0 {
			t.Fatal("a refused credential was stored")
		}
	})

	t.Run("verification needs all three credentials", func(t *testing.T) {
		if err := set("app_key", darajaApp.ConsumerKey); err != nil {
			t.Fatal(err)
		}
		wantErr(t, h.svc.VerifyRailAccount(ctx, "ops@example.com", acct.ID), app.ErrValidation)
	})

	if err := set("app_secret", "fake-wrong-secret-value"); err != nil {
		t.Fatal(err)
	}
	if err := set("passkey", darajaApp.Passkey); err != nil {
		t.Fatal(err)
	}

	t.Run("credentials are sealed, fingerprinted and read back exactly", func(t *testing.T) {
		rows, err := h.super.Query(ctx, `SELECT kind, ciphertext, fingerprint, kms_key_id, created_by FROM rail_credentials WHERE status = 'active'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		secrets := map[string]string{"app_key": darajaApp.ConsumerKey, "app_secret": "fake-wrong-secret-value", "passkey": darajaApp.Passkey}
		n := 0
		for rows.Next() {
			var kind, keyID string
			var ct, fp []byte
			var by uuid.UUID
			if err := rows.Scan(&kind, &ct, &fp, &keyID, &by); err != nil {
				t.Fatal(err)
			}
			n++
			if bytes.Contains(ct, []byte(secrets[kind])) {
				t.Fatalf("%s is stored in the clear", kind)
			}
			if !vascrypto.HMACVerify(fingerprintKey, []byte(secrets[kind]), fp) || keyID != app.CredentialsKMSKeyID || by != staffID {
				t.Fatalf("%s: fingerprint, key or maker wrong", kind)
			}
		}
		if n != 3 {
			t.Fatalf("%d active credentials, want 3", n)
		}
		got, err := h.creds.DarajaCredentials(ctx, acct.ID)
		if err != nil || got.ConsumerKey != darajaApp.ConsumerKey || got.ConsumerSecret != "fake-wrong-secret-value" ||
			got.Passkey != darajaApp.Passkey || got.ShortCode != darajaApp.ShortCode {
			t.Fatalf("read back %v", err)
		}
	})

	t.Run("a refused test call leaves the account pending", func(t *testing.T) {
		wantErr(t, h.svc.VerifyRailAccount(ctx, "ops@example.com", acct.ID), app.ErrVerificationFailed)
		if n := count(t, h, `SELECT count(*) FROM rail_accounts WHERE id = $1 AND status = 'pending_verification'`, acct.ID); n != 1 {
			t.Fatal("the account changed status")
		}
		if n := count(t, h, `SELECT count(*) FROM outbox_events`); n != 0 {
			t.Fatal("an event was written for a failed verification")
		}
	})

	t.Run("rotating a credential retires the old one", func(t *testing.T) {
		if err := set("app_secret", darajaApp.ConsumerSecret); err != nil {
			t.Fatal(err)
		}
		if n := count(t, h, `SELECT count(*) FROM rail_credentials WHERE kind = 'app_secret' AND status = 'retired' AND retired_at IS NOT NULL`); n != 1 {
			t.Fatal("the old secret wasn't retired")
		}
	})

	t.Run("only staff verify", func(t *testing.T) {
		wantErr(t, h.svc.VerifyRailAccount(ctx, "client@example.com", acct.ID), app.ErrNotStaff)
	})

	t.Run("a working test call makes the account active", func(t *testing.T) {
		if err := h.svc.VerifyRailAccount(ctx, "ops@example.com", acct.ID); err != nil {
			t.Fatal(err)
		}
		if n := count(t, h, `SELECT count(*) FROM rail_accounts WHERE id = $1 AND status = 'active' AND verified_at = $2`, acct.ID, h.clk.Now()); n != 1 {
			t.Fatal("the account isn't active and verified now")
		}
		ev := assertEventValid(t, h, "rail_account.verified", acct.ID)
		if ev["verified_by"] != staffID.String() || ev["number"] != darajaApp.ShortCode {
			t.Fatalf("event %v", ev)
		}
		user, tenant := uuid.New(), uuid.New()
		h.accounts.Grant(user, tenant, "KE", "KES", app.PermBillingRead)
		if d, err := h.svc.TopupDetails(ctx, user, tenant); err != nil || d.PaybillNumber != darajaApp.ShortCode {
			t.Fatalf("the verified paybill is offered: %+v %v", d, err)
		}
	})

	t.Run("a new credential puts the account back to pending", func(t *testing.T) {
		if err := set("passkey", darajaApp.Passkey); err != nil {
			t.Fatal(err)
		}
		if n := count(t, h, `SELECT count(*) FROM rail_accounts WHERE id = $1 AND status = 'pending_verification' AND verified_at IS NULL`, acct.ID); n != 1 {
			t.Fatal("the account stayed active with unverified credentials")
		}
		user, tenant := uuid.New(), uuid.New()
		h.accounts.Grant(user, tenant, "KE", "KES", app.PermBillingRead)
		_, err := h.svc.TopupDetails(ctx, user, tenant)
		wantErr(t, err, app.ErrTopupUnavailable)
	})

	t.Run("no credential appears in the logs", func(t *testing.T) {
		logs := h.logs.String()
		for _, secret := range []string{darajaApp.ConsumerSecret, darajaApp.Passkey, darajaApp.ConsumerKey, "fake-wrong-secret-value"} {
			if strings.Contains(logs, secret) {
				t.Fatalf("a credential was logged")
			}
		}
	})
}
