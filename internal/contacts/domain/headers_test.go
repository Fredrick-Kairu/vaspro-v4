package domain_test

import (
	"errors"
	"testing"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
)

func TestMatchHeaders(t *testing.T) {
	defs := []domain.AttributeDef{{Key: "county", Label: "County", Type: "text"}, {Key: "joined", Label: "Date joined", Type: "date"}}

	m, unknown, err := domain.MatchHeaders([]string{"\ufeffMobile", " First Name ", "SURNAME", "e-mail", "county", "Date Joined", "Notes"}, defs)
	if err != nil {
		t.Fatal(err)
	}
	if m.Phone != 0 || m.FirstName != 1 || m.LastName != 2 || m.Email != 3 {
		t.Errorf("fields: %+v", m)
	}
	if m.Attributes[4].Key != "county" || m.Attributes[5].Key != "joined" {
		t.Errorf("attributes: %+v", m.Attributes)
	}
	if len(unknown) != 1 || unknown[0] != 6 {
		t.Errorf("unknown columns %v, want [6]", unknown)
	}

	for _, phone := range []string{"phone", "MSISDN", "mobile", "Number", "phone_number", "Phone Number"} {
		if m, _, err := domain.MatchHeaders([]string{"name", phone}, nil); err != nil || m.Phone != 1 {
			t.Errorf("%q not taken as the number: %+v, %v", phone, m, err)
		}
	}
	if _, _, err := domain.MatchHeaders([]string{"name", "city"}, nil); !errors.Is(err, domain.ErrNoPhoneColumn) {
		t.Errorf("no number column: %v", err)
	}
	if _, _, err := domain.MatchHeaders([]string{"phone", "mobile"}, nil); !errors.Is(err, domain.ErrDuplicateColumn) {
		t.Errorf("two number columns: %v", err)
	}
	if _, _, err := domain.MatchHeaders([]string{"phone", "county", "County"}, defs); !errors.Is(err, domain.ErrDuplicateColumn) {
		t.Errorf("one attribute twice: %v", err)
	}
	if m, _, _ := domain.MatchHeaders([]string{"phone"}, nil); m.FirstName != -1 || m.LastName != -1 || m.Email != -1 {
		t.Errorf("missing optional columns should be -1: %+v", m)
	}
}
