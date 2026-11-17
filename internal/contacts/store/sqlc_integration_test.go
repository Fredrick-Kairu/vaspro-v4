//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/store"
)

// TestSQLC_RoundTrip runs the starter queries as contacts_app, inside a
// tenant's transaction (and as contacts_system for the preference link
// lookup), as the service will.
func TestSQLC_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newContactsDB(t)
	tenant, other := uuid.New(), uuid.New()
	user := pgtype.UUID{Bytes: uuid.New(), Valid: true}

	var contactID, importID uuid.UUID
	err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		q := store.New(tx)
		c, err := q.CreateContact(ctx, store.CreateContactParams{
			ID: uuid.New(), TenantID: tenant, PhoneE164: phone(1),
			FirstName: pgtype.Text{String: "Ann", Valid: true}, Attributes: []byte(`{"county":"Nairobi"}`),
			Source: "api", CreatedBy: user,
		})
		if err != nil {
			return err
		}
		contactID = c.ID
		if got, err := q.GetContactByPhone(ctx, phone(1)); err != nil || got.ID != c.ID {
			t.Errorf("GetContactByPhone: %+v, %v", got, err)
		}

		g, err := q.CreateGroup(ctx, store.CreateGroupParams{ID: uuid.New(), TenantID: tenant, Name: "Customers", CreatedBy: user})
		if err != nil {
			return err
		}
		for range 2 { // repeat-safe
			if err := q.AddGroupMember(ctx, store.AddGroupMemberParams{TenantID: tenant, GroupID: g.ID, ContactID: c.ID}); err != nil {
				return err
			}
		}
		if n, err := q.CountGroupMembers(ctx, g.ID); err != nil || n != 1 {
			t.Errorf("members after adding twice: %d, %v", n, err)
		}

		if _, err := q.CreateAttributeDefinition(ctx, store.CreateAttributeDefinitionParams{TenantID: tenant, Key: "county", Label: "County", Type: "text"}); err != nil {
			return err
		}
		if _, err := q.CreateSegment(ctx, store.CreateSegmentParams{ID: uuid.New(), TenantID: tenant, Name: "Nairobi", Filter: []byte(`{"all":[]}`), CreatedBy: user}); err != nil {
			return err
		}

		// A client grant, then a change: one row, version 2.
		for _, status := range []string{"granted", "withdrawn"} {
			if _, err := q.UpsertConsent(ctx, store.UpsertConsentParams{
				TenantID: tenant, PhoneE164: phone(1), Channel: store.ContactsChannelSms, Purpose: store.ContactsPurposePromotional,
				Status: status, Actor: "client", Source: "api", RecordedBy: user, RecordedAt: now,
			}); err != nil {
				return err
			}
		}
		cs, err := q.GetConsent(ctx, store.GetConsentParams{PhoneE164: phone(1), Channel: store.ContactsChannelSms, Purpose: store.ContactsPurposePromotional})
		if err != nil || cs.Status != "withdrawn" || cs.Version != 2 {
			t.Errorf("consent after two upserts: %+v, %v", cs, err)
		}
		if err := q.InsertConsentEvent(ctx, store.InsertConsentEventParams{
			ID: uuid.New(), TenantID: tenant, PhoneE164: phone(1), Channel: store.ContactsChannelSms, Purpose: store.ContactsPurposePromotional,
			Status: "withdrawn", Actor: "client", Source: "api", RecordedBy: user, OccurredAt: now,
		}); err != nil {
			return err
		}
		if evs, err := q.ListConsentEvents(ctx, phone(1)); err != nil || len(evs) != 1 {
			t.Errorf("consent history: %d, %v", len(evs), err)
		}

		if _, err := q.AddSuppression(ctx, store.AddSuppressionParams{TenantID: tenant, PhoneE164: phone(2), Reason: "complaint", CreatedBy: user.Bytes}); err != nil {
			return err
		}
		if _, err := q.AddSuppression(ctx, store.AddSuppressionParams{
			TenantID: tenant, PhoneE164: phone(2), Channel: store.NullContactsChannel{ContactsChannel: store.ContactsChannelWhatsapp, Valid: true},
			Reason: "client_request", CreatedBy: user.Bytes,
		}); err != nil {
			return err
		}
		if s, err := q.ListSuppressionsForPhone(ctx, phone(2)); err != nil || len(s) != 2 {
			t.Errorf("suppressions: %d, %v", len(s), err)
		}

		imp, err := q.CreateImport(ctx, store.CreateImportParams{
			ID: uuid.New(), TenantID: tenant, FileName: "contacts.csv", ContentType: "text/csv",
			ObjectKey: "uploads/transient/" + tenant.String() + "/x", UploadExpiresAt: now.Add(15 * time.Minute),
			DeleteAfter: now.Add(48 * time.Hour), CreatedBy: user.Bytes,
		})
		if err != nil {
			return err
		}
		importID = imp.ID
		if imp.Status != store.ContactsImportStatusAwaitingUpload || imp.RetentionClass != "transient" {
			t.Errorf("a new import: %s, %s", imp.Status, imp.RetentionClass)
		}
		for _, rowNo := range []int32{4, 4, 2} { // the repeat is skipped
			if err := q.InsertImportError(ctx, store.InsertImportErrorParams{
				ImportID: imp.ID, TenantID: tenant, RowNo: rowNo, Field: "phone", ErrorCode: "VAL-4000", Message: "not a mobile number",
			}); err != nil {
				return err
			}
		}
		errs, err := q.ListImportErrors(ctx, imp.ID)
		if err != nil || len(errs) != 2 || errs[0].RowNo != 2 {
			t.Errorf("import errors: %+v, %v", errs, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenant transaction: %v", err)
	}

	// Another client sees none of it.
	if err := asTenant(ctx, db.app, other, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetContact(ctx, contactID); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("another client read the contact: %v", err)
		}
		if _, err := q.GetImport(ctx, importID); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("another client read the import: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A preference link, reissued, then found by its hash across clients.
	hash := func(token string) []byte { h := sha256.Sum256([]byte(token)); return h[:] }
	if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error {
		q := store.New(tx)
		for _, token := range []string{"first12chars", "second12char"} {
			if _, err := q.UpsertPreferenceLink(ctx, store.UpsertPreferenceLinkParams{
				TenantID: tenant, PhoneE164: phone(1), TokenHash: hash(token), CreatedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.FindPreferenceLink(ctx, hash("first12chars")); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("the replaced token still works: %v", err)
		}
		link, err := q.FindPreferenceLink(ctx, hash("second12char"))
		if err != nil || link.TenantID != tenant || link.PhoneE164 != phone(1) {
			t.Errorf("the current token: %+v, %v", link, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
