//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type tableRow struct {
	table string
	r     row
}

// constraintCase inserts before, then r into table, in a rolled-back
// owner transaction, and expects SQLSTATE code ("" for success).
type constraintCase struct {
	name   string
	before []tableRow
	table  string
	r      row
	code   string
}

const (
	checkViolation  = "23514"
	uniqueViolation = "23505"
	notNull         = "23502"
	fkViolation     = "23503"
)

func runConstraintCases(t *testing.T, db contactsDB, cases []constraintCase) {
	t.Helper()
	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx, err := db.super.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			for _, b := range c.before {
				mustInsert(t, tx, b.table, b.r)
			}
			wantCode(t, insert(ctx, tx, c.table, c.r), c.code)
		})
	}
}

func TestConstraints_Contacts(t *testing.T) {
	db := newContactsDB(t)
	a, b := uuid.New(), uuid.New()
	runConstraintCases(t, db, []constraintCase{
		{name: "a contact", table: "contacts", r: contact(a, 1)},
		{name: "with names, email and attributes", table: "contacts", r: with(contact(a, 1), row{"first_name": "Ann", "last_name": "Test", "email": "ann@example.com", "attributes": `{"county":"Nairobi"}`})},
		{name: "a number without +", table: "contacts", r: with(contact(a, 1), row{"phone_e164": "99900000001"}), code: checkViolation},
		{name: "a number starting +0", table: "contacts", r: with(contact(a, 1), row{"phone_e164": "+0999000001"}), code: checkViolation},
		{name: "a number too short", table: "contacts", r: with(contact(a, 1), row{"phone_e164": "+99912"}), code: checkViolation},
		{name: "a number too long", table: "contacts", r: with(contact(a, 1), row{"phone_e164": "+9990000000000001"}), code: checkViolation},
		{name: "a number with spaces", table: "contacts", r: with(contact(a, 1), row{"phone_e164": "+999 000 000 001"}), code: checkViolation},
		{name: "no number", table: "contacts", r: with(contact(a, 1), row{"phone_e164": drop}), code: notNull},
		{name: "attributes as an array", table: "contacts", r: with(contact(a, 1), row{"attributes": `[]`}), code: checkViolation},
		{name: "an unknown source", table: "contacts", r: with(contact(a, 1), row{"source": "csv"}), code: checkViolation},
		{name: "a first name over 100 characters", table: "contacts", r: with(contact(a, 1), row{"first_name": strings.Repeat("a", 101)}), code: checkViolation},
		{name: "an email over 254 characters", table: "contacts", r: with(contact(a, 1), row{"email": strings.Repeat("a", 250) + "@x.io"}), code: checkViolation},
		{name: "the same number twice for a client", before: []tableRow{{"contacts", contact(a, 1)}}, table: "contacts", r: contact(a, 1), code: uniqueViolation},
		{name: "the same number for another client", before: []tableRow{{"contacts", contact(a, 1)}}, table: "contacts", r: contact(b, 1)},
	})
}

func TestConstraints_AttributesGroupsSegments(t *testing.T) {
	db := newContactsDB(t)
	a := uuid.New()
	runConstraintCases(t, db, []constraintCase{
		{name: "an attribute", table: "attribute_definitions", r: attributeDef(a, "county")},
		{name: "a key with digits and underscores", table: "attribute_definitions", r: attributeDef(a, "loyalty_tier2")},
		{name: "a key of 40 characters", table: "attribute_definitions", r: attributeDef(a, "a"+strings.Repeat("b", 39))},
		{name: "a key of 41 characters", table: "attribute_definitions", r: attributeDef(a, "a"+strings.Repeat("b", 40)), code: checkViolation},
		{name: "a capital in a key", table: "attribute_definitions", r: attributeDef(a, "County"), code: checkViolation},
		{name: "a key starting with a digit", table: "attribute_definitions", r: attributeDef(a, "2county"), code: checkViolation},
		{name: "a hyphen in a key", table: "attribute_definitions", r: attributeDef(a, "home-county"), code: checkViolation},
		{name: "a dot in a key", table: "attribute_definitions", r: attributeDef(a, "a.b"), code: checkViolation},
		{name: "an unknown type", table: "attribute_definitions", r: with(attributeDef(a, "county"), row{"type": "json"}), code: checkViolation},
		{name: "an empty label", table: "attribute_definitions", r: with(attributeDef(a, "county"), row{"label": ""}), code: checkViolation},
		{name: "a key twice", before: []tableRow{{"attribute_definitions", attributeDef(a, "county")}}, table: "attribute_definitions", r: attributeDef(a, "county"), code: uniqueViolation},

		{name: "a group", table: "groups", r: group(a, "Customers")},
		{name: "an empty group name", table: "groups", r: group(a, ""), code: checkViolation},
		{name: "a group name in another case", before: []tableRow{{"groups", group(a, "Customers")}}, table: "groups", r: group(a, "CUSTOMERS"), code: uniqueViolation},

		{name: "a segment", table: "segments", r: segment(a, "Nairobi")},
		{name: "a filter that isn't an object", table: "segments", r: with(segment(a, "x"), row{"filter": `[1]`}), code: checkViolation},
		{name: "a segment name in another case", before: []tableRow{{"segments", segment(a, "Nairobi")}}, table: "segments", r: segment(a, "nairobi"), code: uniqueViolation},
	})
}

func TestConstraints_Consents(t *testing.T) {
	db := newContactsDB(t)
	a := uuid.New()
	runConstraintCases(t, db, []constraintCase{
		{name: "a client's grant", table: "consents", r: consent(a, 1)},
		{name: "the person's withdrawal by keyword", table: "consents", r: subjectWithdrawal(a, 1)},
		{name: "the person's choice in the preference centre", table: "consents", r: with(subjectWithdrawal(a, 1), row{"source": "preference_centre"})},
		{name: "a v3 import with no user", table: "consents", r: with(consent(a, 1), row{"source": "v3", "recorded_by": drop})},
		{name: "a keyword recorded as the client", table: "consents", r: with(consent(a, 1), row{"source": "keyword"}), code: checkViolation},
		{name: "the person by API", table: "consents", r: with(subjectWithdrawal(a, 1), row{"source": "api"}), code: checkViolation},
		{name: "the client with no user", table: "consents", r: with(consent(a, 1), row{"recorded_by": drop}), code: checkViolation},
		{name: "the person with a user", table: "consents", r: with(subjectWithdrawal(a, 1), row{"recorded_by": uuid.New()}), code: checkViolation},
		{name: "an unknown status", table: "consents", r: with(consent(a, 1), row{"status": "pending"}), code: checkViolation},
		{name: "an unknown channel", table: "consents", r: with(consent(a, 1), row{"channel": "voice"}), code: "22P02"},
		{name: "an unknown purpose", table: "consents", r: with(consent(a, 1), row{"purpose": "marketing"}), code: "22P02"},
		{name: "a malformed number", table: "consents", r: with(consent(a, 1), row{"phone_e164": "0712"}), code: checkViolation},
		{name: "evidence over 500 characters", table: "consents", r: with(consent(a, 1), row{"evidence": strings.Repeat("e", 501)}), code: checkViolation},
		{name: "a second row for one number, channel and purpose", before: []tableRow{{"consents", consent(a, 1)}}, table: "consents", r: consent(a, 1), code: uniqueViolation},
		{name: "another purpose for the same number", before: []tableRow{{"consents", consent(a, 1)}}, table: "consents", r: with(consent(a, 1), row{"purpose": "transactional"})},
		{name: "another channel for the same number", before: []tableRow{{"consents", consent(a, 1)}}, table: "consents", r: with(consent(a, 1), row{"channel": "whatsapp"})},

		{name: "a history row", table: "consent_events", r: consentEvent(a, 1)},
		{name: "a history row with an unknown status", table: "consent_events", r: with(consentEvent(a, 1), row{"status": "maybe"}), code: checkViolation},
	})
}

func TestConstraints_SuppressionsAndLinks(t *testing.T) {
	db := newContactsDB(t)
	a := uuid.New()
	link := preferenceLink(a, 1)
	runConstraintCases(t, db, []constraintCase{
		{name: "every channel", table: "suppressions", r: suppression(a, 1)},
		{name: "one channel", table: "suppressions", r: with(suppression(a, 1), row{"channel": "sms"})},
		{name: "every channel twice", before: []tableRow{{"suppressions", suppression(a, 1)}}, table: "suppressions", r: suppression(a, 1), code: uniqueViolation},
		{name: "one channel twice", before: []tableRow{{"suppressions", with(suppression(a, 1), row{"channel": "sms"})}}, table: "suppressions", r: with(suppression(a, 1), row{"channel": "sms"}), code: uniqueViolation},
		{name: "one channel beside every channel", before: []tableRow{{"suppressions", suppression(a, 1)}}, table: "suppressions", r: with(suppression(a, 1), row{"channel": "sms"})},
		{name: "an unknown reason", table: "suppressions", r: with(suppression(a, 1), row{"reason": "spam"}), code: checkViolation},
		{name: "no creator", table: "suppressions", r: with(suppression(a, 1), row{"created_by": drop}), code: notNull},

		{name: "a preference link", table: "preference_links", r: link},
		{name: "expiring when it's made", table: "preference_links", r: with(preferenceLink(a, 2), row{"expires_at": now}), code: checkViolation},
		{name: "a second link for one number", before: []tableRow{{"preference_links", link}}, table: "preference_links", r: preferenceLink(a, 1), code: uniqueViolation},
		{name: "a token hash used twice", before: []tableRow{{"preference_links", link}}, table: "preference_links", r: with(preferenceLink(a, 2), row{"token_hash": link["token_hash"]}), code: uniqueViolation},
	})
}

// TestConstraints_Imports: data model §8's tables, as given.
func TestConstraints_Imports(t *testing.T) {
	db := newContactsDB(t)
	a := uuid.New()
	imp := importRow(a)
	runConstraintCases(t, db, []constraintCase{
		{name: "awaiting upload", table: "imports", r: imp},
		{name: "an XLSX", table: "imports", r: with(importRow(a), row{"content_type": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"})},
		{name: "a final status with finished_at", table: "imports", r: finishedImport(a)},
		{name: "each final status", table: "imports", r: with(finishedImport(a), row{"status": "expired"})},
		{name: "a final status without finished_at", table: "imports", r: with(finishedImport(a), row{"finished_at": drop}), code: checkViolation},
		{name: "processing with finished_at", table: "imports", r: with(importRow(a), row{"status": "processing", "finished_at": now}), code: checkViolation},
		{name: "an unknown status", table: "imports", r: with(importRow(a), row{"status": "done"}), code: "22P02"},
		{name: "a PDF", table: "imports", r: with(importRow(a), row{"content_type": "application/pdf"}), code: checkViolation},
		{name: "a size of 0", table: "imports", r: with(importRow(a), row{"size_bytes": 0}), code: checkViolation},
		{name: "an unknown retention class", table: "imports", r: with(importRow(a), row{"retention_class": "forever"}), code: checkViolation},
		{name: "financial retention", table: "imports", r: with(importRow(a), row{"retention_class": "financial"})},
		{name: "an object key twice", before: []tableRow{{"imports", imp}}, table: "imports", r: with(importRow(a), row{"object_key": imp["object_key"]}), code: uniqueViolation},
		{name: "no delete_after", table: "imports", r: with(importRow(a), row{"delete_after": drop}), code: notNull},

		{name: "an error", before: []tableRow{{"imports", imp}}, table: "import_errors", r: importError(imp["id"].(uuid.UUID), a, 3)},
		{name: "a file-level error at row 0", before: []tableRow{{"imports", imp}}, table: "import_errors", r: with(importError(imp["id"].(uuid.UUID), a, 0), row{"field": ""})},
		{name: "an error for no import", table: "import_errors", r: importError(uuid.New(), a, 3), code: fkViolation},
		{name: "the same error twice", before: []tableRow{{"imports", imp}, {"import_errors", importError(imp["id"].(uuid.UUID), a, 3)}}, table: "import_errors", r: importError(imp["id"].(uuid.UUID), a, 3), code: uniqueViolation},
		{name: "another error on the same row", before: []tableRow{{"imports", imp}, {"import_errors", importError(imp["id"].(uuid.UUID), a, 3)}}, table: "import_errors", r: with(importError(imp["id"].(uuid.UUID), a, 3), row{"field": "email"})},
	})
}
