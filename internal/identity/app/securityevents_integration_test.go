//go:build integration

package app_test

import (
	"context"
	"net"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

func TestSignIn_FirstSignIn_RecordsEventButNoAlert(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	if _, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", net.ParseIP("203.0.113.1"), "test-agent"); err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	if _, ok := sender.LastOfKind(email.KindLoginAlert); ok {
		t.Fatal("expected no login alert on an account's very first sign-in")
	}
}

func TestSignIn_SameIPTwice_NoAlert(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")
	ip := net.ParseIP("203.0.113.1")

	if _, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", ip, "test-agent"); err != nil {
		t.Fatalf("first SignIn: %v", err)
	}
	if _, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", ip, "test-agent"); err != nil {
		t.Fatalf("second SignIn: %v", err)
	}

	if _, ok := sender.LastOfKind(email.KindLoginAlert); ok {
		t.Fatal("expected no login alert when signing in from the same IP again")
	}
}

func TestSignIn_NewIPOnSecondSignIn_SendsAlert(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	if _, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", net.ParseIP("203.0.113.1"), "test-agent"); err != nil {
		t.Fatalf("first SignIn: %v", err)
	}
	if _, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", net.ParseIP("198.51.100.9"), "test-agent"); err != nil {
		t.Fatalf("second SignIn: %v", err)
	}

	sent, ok := sender.LastOfKind(email.KindLoginAlert)
	if !ok {
		t.Fatal("expected a login alert when signing in from a never-seen IP")
	}
	if sent.To != "ada@example.com" {
		t.Fatalf("got alert To %q, want ada@example.com", sent.To)
	}
	if sent.IP != "198.51.100.9" {
		t.Fatalf("got alert IP %q, want 198.51.100.9", sent.IP)
	}
}

func TestSignIn_NoIP_NeverAlerts(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	setPassword(t, ctx, svc, sender, "ada@example.com", "a-strong-password")

	if _, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", net.ParseIP("203.0.113.1"), "test-agent"); err != nil {
		t.Fatalf("first SignIn: %v", err)
	}
	if _, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", nil, "test-agent"); err != nil {
		t.Fatalf("second SignIn (no ip): %v", err)
	}

	if _, ok := sender.LastOfKind(email.KindLoginAlert); ok {
		t.Fatal("expected no login alert when the caller IP is unknown")
	}
}

func TestListSecurityEvents_RecordsSignInsNewestFirst(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	if _, err := svc.SignIn(ctx, "ada@example.com", "a-strong-password", net.ParseIP("198.51.100.9"), "test-agent"); err != nil {
		t.Fatalf("second SignIn: %v", err)
	}

	events, err := svc.ListSecurityEvents(ctx, userID)
	if err != nil {
		t.Fatalf("ListSecurityEvents: %v", err)
	}
	// signedInUserID itself signs in once (203.0.113.1-less, no ip),
	// plus the explicit SignIn above — at least 2 sign_in events.
	if len(events) < 2 {
		t.Fatalf("got %d events, want at least 2", len(events))
	}
	if events[0].Type != "sign_in" {
		t.Fatalf("got newest event type %q, want sign_in", events[0].Type)
	}
	newIP, _ := events[0].Detail["new_ip"].(bool)
	if !newIP {
		t.Fatalf("got newest event detail %v, want new_ip true (different ip from the first sign-in)", events[0].Detail)
	}
	if events[0].OccurredAt.Before(events[len(events)-1].OccurredAt) {
		t.Fatalf("expected events newest first, got %v before %v", events[0].OccurredAt, events[len(events)-1].OccurredAt)
	}
}

func TestListSecurityEvents_ScopedToOwnUser(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	adaID := signedInUserID(t, ctx, svc, sender, "ada@example.com")
	signedInUserID(t, ctx, svc, sender, "bob@example.com")

	events, err := svc.ListSecurityEvents(ctx, adaID)
	if err != nil {
		t.Fatalf("ListSecurityEvents: %v", err)
	}
	for _, ev := range events {
		if ev.Type != "sign_in" {
			t.Fatalf("got event type %q, want only sign_in for this account", ev.Type)
		}
	}
	if len(events) != 1 {
		t.Fatalf("got %d events for ada, want exactly 1 (bob's own sign-in must not appear)", len(events))
	}
}

func TestApiClientLifecycle_RecordsSecurityEvents(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	client, err := svc.CreateApiClient(ctx, userID, "test client", "sandbox", []string{"sms:send"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateApiClient: %v", err)
	}
	if _, err := svc.RotateApiClientSecret(ctx, userID, client.ID); err != nil {
		t.Fatalf("RotateApiClientSecret: %v", err)
	}
	if err := svc.RevokeApiClient(ctx, userID, client.ID); err != nil {
		t.Fatalf("RevokeApiClient: %v", err)
	}

	events, err := svc.ListSecurityEvents(ctx, userID)
	if err != nil {
		t.Fatalf("ListSecurityEvents: %v", err)
	}
	wantTypes := map[string]bool{
		"api_client_created":        false,
		"api_client_secret_rotated": false,
		"api_client_revoked":        false,
	}
	for _, ev := range events {
		if _, ok := wantTypes[ev.Type]; ok {
			wantTypes[ev.Type] = true
		}
	}
	for eventType, seen := range wantTypes {
		if !seen {
			t.Fatalf("expected a %q security event, got events %v", eventType, events)
		}
	}
}

func TestSMPPAccountLifecycle_RecordsSecurityEvents(t *testing.T) {
	ctx := context.Background()
	svc, _, sender, _, _ := newTestService(t)
	userID := signedInUserID(t, ctx, svc, sender, "ada@example.com")

	account, err := svc.CreateSMPPAccount(ctx, userID, "ACME01", "sandbox", []string{"203.0.113.0/24"}, 2, 50)
	if err != nil {
		t.Fatalf("CreateSMPPAccount: %v", err)
	}
	if _, err := svc.RotateSMPPAccountPassword(ctx, userID, account.ID); err != nil {
		t.Fatalf("RotateSMPPAccountPassword: %v", err)
	}
	if err := svc.SuspendSMPPAccount(ctx, userID, account.ID); err != nil {
		t.Fatalf("SuspendSMPPAccount: %v", err)
	}
	if err := svc.ReactivateSMPPAccount(ctx, userID, account.ID); err != nil {
		t.Fatalf("ReactivateSMPPAccount: %v", err)
	}

	events, err := svc.ListSecurityEvents(ctx, userID)
	if err != nil {
		t.Fatalf("ListSecurityEvents: %v", err)
	}
	wantTypes := map[string]bool{
		"smpp_account_created":          false,
		"smpp_account_password_rotated": false,
		"smpp_account_suspended":        false,
		"smpp_account_reactivated":      false,
	}
	for _, ev := range events {
		if _, ok := wantTypes[ev.Type]; ok {
			wantTypes[ev.Type] = true
		}
	}
	for eventType, seen := range wantTypes {
		if !seen {
			t.Fatalf("expected a %q security event, got events %v", eventType, events)
		}
	}
}
