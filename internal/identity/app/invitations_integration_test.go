//go:build integration

package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/app"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/email"
)

// invite is what accounts does: find or create the invitee, then send.
func invite(t *testing.T, svc *app.Service, addr string, inviter uuid.UUID) (uuid.UUID, error) {
	t.Helper()
	ctx := context.Background()
	id, err := svc.EnsureInvitee(ctx, addr)
	if err != nil {
		return id, err
	}
	return id, svc.SendInvitation(ctx, id, inviter, "Acme")
}

func TestEnsureInvitee(t *testing.T) {
	svc, pool, sender, _, _ := newTestService(t)
	ctx := context.Background()
	ada := signedInUserID(t, ctx, svc, sender, "ada@example.com")
	bob := signedInUserID(t, ctx, svc, sender, "bob@example.com")
	if _, err := pool.Exec(ctx, `UPDATE users SET full_name = 'Ada Lovelace' WHERE id = $1`, ada); err != nil {
		t.Fatal(err)
	}

	t.Run("an existing user keeps their ID and is left as they are", func(t *testing.T) {
		got, err := invite(t, svc, " BOB@example.com ", ada)
		if err != nil || got != bob {
			t.Fatalf("got %s, %v; want bob %s", got, err, bob)
		}
		sent, _ := sender.LastOfKind(email.KindInvitation)
		if sent.To != "bob@example.com" || sent.AccountName != "Acme" || sent.InviterName != "Ada Lovelace" {
			t.Fatalf("got email %+v", sent)
		}
	})
	t.Run("a new address becomes an unverified user with no name", func(t *testing.T) {
		got, err := invite(t, svc, "carol@example.com", ada)
		if err != nil || got == uuid.Nil {
			t.Fatalf("got %s, %v", got, err)
		}
		var verified bool
		var name string
		if err := pool.QueryRow(ctx, `SELECT email_verified_at IS NOT NULL, full_name FROM users WHERE id = $1`, got).Scan(&verified, &name); err != nil {
			t.Fatal(err)
		}
		if verified || name != "" {
			t.Fatalf("verified %v, name %q; want an unverified user with no name", verified, name)
		}
		sent, _ := sender.LastOfKind(email.KindInvitation)
		if sent.To != "carol@example.com" || sent.AccountName != "Acme" {
			t.Fatalf("got email %+v", sent)
		}
		again, err := invite(t, svc, "carol@example.com", ada)
		if err != nil || again != got {
			t.Fatalf("inviting again: got %s, %v; want the same user", again, err)
		}
	})
	t.Run("a malformed address is refused", func(t *testing.T) {
		if _, err := invite(t, svc, "not an email", ada); !errors.Is(err, app.ErrValidation) {
			t.Fatalf("got %v, want ErrValidation", err)
		}
	})
	t.Run("an unknown invitee or inviter is not found", func(t *testing.T) {
		if err := svc.SendInvitation(ctx, uuid.New(), ada, "Acme"); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
		if err := svc.SendInvitation(ctx, bob, uuid.New(), "Acme"); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
	t.Run("the invitation is in the inviter's security log", func(t *testing.T) {
		events, err := svc.ListSecurityEvents(ctx, ada)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range events {
			if e.Type == "user_invited" {
				n++
			}
		}
		if n != 3 {
			t.Fatalf("%d user_invited events, want 3", n)
		}
	})
}

func TestInvitedUser_SignsUpVerifiesAndSignsIn(t *testing.T) {
	svc, pool, sender, _, _ := newTestService(t)
	ctx := context.Background()
	ada := signedInUserID(t, ctx, svc, sender, "ada@example.com")
	carol, err := invite(t, svc, "carol@example.com", ada)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.SignUp(ctx, "carol@example.com", "Carol Shaw", nil); err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	var name string
	if err := pool.QueryRow(ctx, `SELECT full_name FROM users WHERE id = $1`, carol).Scan(&name); err != nil || name != "Carol Shaw" {
		t.Fatalf("name %q, %v; want the one given at sign-up", name, err)
	}
	signedIn := signedInUserID(t, ctx, svc, sender, "carol@example.com")
	if signedIn != carol {
		t.Fatalf("carol signed in as %s, invited as %s", signedIn, carol)
	}

	t.Run("signing up again doesn't rename an existing user", func(t *testing.T) {
		if err := svc.SignUp(ctx, "carol@example.com", "Someone Else", nil); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT full_name FROM users WHERE id = $1`, carol).Scan(&name); err != nil || name != "Carol Shaw" {
			t.Fatalf("name %q, %v", name, err)
		}
	})
}

func TestGetUsers(t *testing.T) {
	svc, _, sender, _, _ := newTestService(t)
	ctx := context.Background()
	ada := signedInUserID(t, ctx, svc, sender, "ada@example.com")
	bob := signedInUserID(t, ctx, svc, sender, "bob@example.com")

	got, err := svc.GetUsers(ctx, []uuid.UUID{ada, bob, uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v, want ada and bob only", got)
	}
	emails := map[uuid.UUID]string{}
	for _, u := range got {
		emails[u.ID] = u.Email
	}
	if emails[ada] != "ada@example.com" || emails[bob] != "bob@example.com" {
		t.Fatalf("got %+v", got)
	}
}
