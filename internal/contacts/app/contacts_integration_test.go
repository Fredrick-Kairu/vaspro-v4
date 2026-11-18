//go:build integration

package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/tools/fakes/contactaccess"
)

// The numbers are ROADMAP §8.4's reserved KE test block, +254 700 000 0xx
// and its mirror +254 100 000 0xx; never real ones.

func ptr(s string) *string { return &s }

func TestContacts_CreateMergeAndRead(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	if _, err := h.svc.CreateAttribute(ctx, m.user, m.tenant, domain.AttributeDef{Key: "county", Label: "County", Type: "text"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateAttribute(ctx, m.user, m.tenant, domain.AttributeDef{Key: "visits", Label: "Visits", Type: "number"}); err != nil {
		t.Fatal(err)
	}

	c, created, err := h.svc.CreateContact(ctx, m.user, m.tenant, app.ContactInput{
		Phone: "0700 000 001", Fields: domain.ContactFields{FirstName: "Ann"},
		Attributes: map[string]string{"county": "Nairobi", "visits": "3"},
	})
	if err != nil || !created {
		t.Fatalf("CreateContact: %v, created %v", err, created)
	}
	if c.Phone != "+254700000001" || c.FirstName != "Ann" || string(c.Attributes["visits"]) != "3" || c.Source != "api" {
		t.Fatalf("contact: %+v", c)
	}

	t.Run("the same number in another format merges", func(t *testing.T) {
		again, created, err := h.svc.CreateContact(ctx, m.user, m.tenant, app.ContactInput{
			Phone: "+254 700 000 001", Fields: domain.ContactFields{LastName: "Test"}, Attributes: map[string]string{"visits": "4"},
		})
		if err != nil || created || again.ID != c.ID {
			t.Fatalf("merge: %+v, created %v, %v", again, created, err)
		}
		if again.FirstName != "Ann" || again.LastName != "Test" || string(again.Attributes["county"]) != `"Nairobi"` || string(again.Attributes["visits"]) != "4" {
			t.Fatalf("merged: %+v", again)
		}
	})

	t.Run("refusals write nothing", func(t *testing.T) {
		for name, in := range map[string]struct {
			in   app.ContactInput
			want error
		}{
			"a landline":              {app.ContactInput{Phone: "0200000001"}, app.ErrPhone},
			"letters":                 {app.ContactInput{Phone: "07000000O1"}, app.ErrPhone},
			"an unknown attribute":    {app.ContactInput{Phone: "0700000002", Attributes: map[string]string{"age": "3"}}, app.ErrValue},
			"a number that isn't one": {app.ContactInput{Phone: "0700000002", Attributes: map[string]string{"visits": "lots"}}, app.ErrValue},
			"an email with a name":    {app.ContactInput{Phone: "0700000002", Fields: domain.ContactFields{Email: "Ann <a@example.com>"}}, app.ErrValue},
		} {
			if _, _, err := h.svc.CreateContact(ctx, m.user, m.tenant, in.in); !errors.Is(err, in.want) {
				t.Errorf("%s: %v, want %v", name, err, in.want)
			}
		}
		if n := h.count(t, `SELECT count(*) FROM contacts`); n != 1 {
			t.Fatalf("%d contacts, want 1", n)
		}
	})

	t.Run("update: clear a field, set and remove attributes", func(t *testing.T) {
		got, err := h.svc.UpdateContact(ctx, m.user, m.tenant, c.ID, app.ContactPatch{
			LastName: ptr(""), Email: ptr("ann@example.com"),
			Attributes: map[string]*string{"county": nil, "visits": ptr("10")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.LastName != "" || got.Email != "ann@example.com" || got.FirstName != "Ann" || len(got.Attributes) != 1 || string(got.Attributes["visits"]) != "10" {
			t.Fatalf("updated: %+v", got)
		}
		if _, err := h.svc.UpdateContact(ctx, m.user, m.tenant, uuid.New(), app.ContactPatch{FirstName: ptr("X")}); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("an unknown contact: %v", err)
		}
	})

	t.Run("deleting an attribute removes its values", func(t *testing.T) {
		if err := h.svc.DeleteAttribute(ctx, m.user, m.tenant, "visits"); err != nil {
			t.Fatal(err)
		}
		got, err := h.svc.GetContact(ctx, m.user, m.tenant, c.ID)
		if err != nil || len(got.Attributes) != 0 {
			t.Fatalf("after deleting the attribute: %+v, %v", got.Attributes, err)
		}
	})

	if strings.Contains(h.logs.String(), "700000001") {
		t.Fatal("a phone number reached the logs")
	}
}

func TestContacts_ListPagesNewestFirst(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	other := h.manager()
	for i := range 7 {
		h.clk.Advance(1)
		if _, _, err := h.svc.CreateContact(ctx, m.user, m.tenant, app.ContactInput{Phone: fmt.Sprintf("07000000%02d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := h.svc.CreateContact(ctx, other.user, other.tenant, app.ContactInput{Phone: "0700000099"}); err != nil {
		t.Fatal(err)
	}
	var seen []string
	cursor := ""
	for range 10 {
		page, next, err := h.svc.ListContacts(ctx, m.user, m.tenant, nil, app.Page{Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page {
			seen = append(seen, c.Phone)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 7 || seen[0] != "+254700000006" || seen[6] != "+254700000000" {
		t.Fatalf("pages: %v", seen)
	}
	for _, bad := range []app.Page{{Limit: 101}, {Limit: -1}, {Cursor: "!!"}} {
		if _, _, err := h.svc.ListContacts(ctx, m.user, m.tenant, nil, bad); !errors.Is(err, app.ErrValidation) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestContacts_Groups(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	var ids []uuid.UUID
	for i := range 3 {
		c, _, err := h.svc.CreateContact(ctx, m.user, m.tenant, app.ContactInput{Phone: fmt.Sprintf("01000000%02d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	g, err := h.svc.CreateGroup(ctx, m.user, m.tenant, "Customers", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CreateGroup(ctx, m.user, m.tenant, "CUSTOMERS", ""); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("a name in another case: %v", err)
	}

	if _, err := h.svc.AddGroupMembers(ctx, m.user, m.tenant, g.ID, []uuid.UUID{ids[0], uuid.New()}); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("an unknown contact: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM group_members`); n != 0 {
		t.Fatalf("%d members added despite the unknown contact", n)
	}
	added, err := h.svc.AddGroupMembers(ctx, m.user, m.tenant, g.ID, []uuid.UUID{ids[0], ids[1], ids[1]})
	if err != nil || added != 2 {
		t.Fatalf("added %d, %v", added, err)
	}
	added, err = h.svc.AddGroupMembers(ctx, m.user, m.tenant, g.ID, []uuid.UUID{ids[1], ids[2]})
	if err != nil || added != 1 {
		t.Fatalf("again: added %d, %v", added, err)
	}
	got, err := h.svc.GetGroup(ctx, m.user, m.tenant, g.ID)
	if err != nil || got.Members != 3 {
		t.Fatalf("group: %+v, %v", got, err)
	}
	members, _, err := h.svc.ListContacts(ctx, m.user, m.tenant, &g.ID, app.Page{})
	if err != nil || len(members) != 3 {
		t.Fatalf("members: %d, %v", len(members), err)
	}
	if err := h.svc.RemoveGroupMember(ctx, m.user, m.tenant, g.ID, ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.DeleteContact(ctx, m.user, m.tenant, ids[1]); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.svc.GetGroup(ctx, m.user, m.tenant, g.ID); got.Members != 1 {
		t.Fatalf("after removing one and deleting another: %d members", got.Members)
	}
	renamed, err := h.svc.UpdateGroup(ctx, m.user, m.tenant, g.ID, ptr("VIP"), ptr("Our best"))
	if err != nil || renamed.Name != "VIP" || renamed.Description != "Our best" {
		t.Fatalf("renamed: %+v, %v", renamed, err)
	}
	if err := h.svc.DeleteGroup(ctx, m.user, m.tenant, g.ID); err != nil {
		t.Fatal(err)
	}
	if n := h.count(t, `SELECT count(*) FROM contacts`); n != 2 {
		t.Fatalf("deleting the group removed contacts: %d left", n)
	}
	if _, _, err := h.svc.ListContacts(ctx, m.user, m.tenant, &g.ID, app.Page{}); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("a deleted group's members: %v", err)
	}
}

func TestContacts_Attributes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	for _, bad := range []domain.AttributeDef{
		{Key: "County", Label: "County", Type: "text"},
		{Key: "county", Label: "", Type: "text"},
		{Key: "county", Label: "County", Type: "json"},
	} {
		if _, err := h.svc.CreateAttribute(ctx, m.user, m.tenant, bad); !errors.Is(err, app.ErrValidation) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	for i := range domain.MaxAttributes {
		if _, err := h.svc.CreateAttribute(ctx, m.user, m.tenant, domain.AttributeDef{Key: fmt.Sprintf("a%d", i), Label: "A", Type: "text"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.svc.CreateAttribute(ctx, m.user, m.tenant, domain.AttributeDef{Key: "one_more", Label: "A", Type: "text"}); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("the 51st: %v", err)
	}
	if _, err := h.svc.CreateAttribute(ctx, m.user, m.tenant, domain.AttributeDef{Key: "a1", Label: "A", Type: "text"}); !errors.Is(err, app.ErrValidation) {
		t.Fatalf("a key twice: %v", err)
	}
	if err := h.svc.DeleteAttribute(ctx, m.user, m.tenant, "missing"); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("an unknown key: %v", err)
	}
	defs, err := h.svc.ListAttributes(ctx, m.user, m.tenant)
	if err != nil || len(defs) != domain.MaxAttributes {
		t.Fatalf("listed %d, %v", len(defs), err)
	}
}

// TestContacts_Who: a reader reads but writes nothing; a stranger finds
// nothing; accounts down fails closed.
func TestContacts_Who(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := h.manager()
	c, _, err := h.svc.CreateContact(ctx, m.user, m.tenant, app.ContactInput{Phone: "0700000001"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := h.svc.CreateGroup(ctx, m.user, m.tenant, "G", "")
	if err != nil {
		t.Fatal(err)
	}
	reader := uuid.New()
	h.accounts.Grant(reader, m.tenant, app.PermRead)
	if _, err := h.svc.GetContact(ctx, reader, m.tenant, c.ID); err != nil {
		t.Fatalf("a reader reading: %v", err)
	}
	writes := map[string]func(user uuid.UUID) error{
		"create": func(u uuid.UUID) error {
			_, _, err := h.svc.CreateContact(ctx, u, m.tenant, app.ContactInput{Phone: "0700000002"})
			return err
		},
		"update": func(u uuid.UUID) error {
			_, err := h.svc.UpdateContact(ctx, u, m.tenant, c.ID, app.ContactPatch{FirstName: ptr("X")})
			return err
		},
		"delete": func(u uuid.UUID) error { return h.svc.DeleteContact(ctx, u, m.tenant, c.ID) },
		"group":  func(u uuid.UUID) error { _, err := h.svc.CreateGroup(ctx, u, m.tenant, "H", ""); return err },
		"add members": func(u uuid.UUID) error {
			_, err := h.svc.AddGroupMembers(ctx, u, m.tenant, g.ID, []uuid.UUID{c.ID})
			return err
		},
		"attribute": func(u uuid.UUID) error {
			_, err := h.svc.CreateAttribute(ctx, u, m.tenant, domain.AttributeDef{Key: "k", Label: "K", Type: "text"})
			return err
		},
		"start import": func(u uuid.UUID) error { _, _, err := h.svc.CreateImport(ctx, u, m.tenant, csvImport()); return err },
	}
	for name, write := range writes {
		if err := write(reader); !errors.Is(err, app.ErrPermissionDenied) {
			t.Errorf("a reader's %s: %v", name, err)
		}
		if err := write(uuid.New()); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("a stranger's %s: %v", name, err)
		}
	}
	if _, err := h.svc.GetContact(ctx, uuid.New(), m.tenant, c.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("a stranger reading: %v", err)
	}
	// Another account's contact isn't found from one's own.
	other := h.manager()
	if _, err := h.svc.GetContact(ctx, other.user, other.tenant, c.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("another account's contact: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM contacts`) + h.count(t, `SELECT count(*) FROM groups`) - 2; n != 0 {
		t.Fatalf("refused calls wrote %d rows", n)
	}
	h.accounts.Fail()
	_, err = h.svc.GetContact(ctx, m.user, m.tenant, c.ID)
	if err == nil || errors.Is(err, app.ErrNotFound) || !errors.Is(err, contactaccess.ErrDown) {
		t.Fatalf("accounts down: %v, want the failure passed on", err)
	}
}
