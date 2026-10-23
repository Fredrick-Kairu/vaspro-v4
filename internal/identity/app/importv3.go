package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// V3UserRecord is one v3 user+profile pair as this task's importer
// reads it — v3's own column names (data model §3's "From v3" table:
// user.email_address, user.full_names, user.password;
// profile.msisdn, joined by profile.user_id), not v4's. V3ID is for
// the importer's own report only; it's never stored (identity.users
// has no v3 reference column — see ImportV3Users's own doc comment
// on why none was added).
type V3UserRecord struct {
	V3ID         string
	EmailAddress string
	FullNames    string
	Password     string // v3's own bcrypt(md5(...)) hash, copied verbatim
	Msisdn       string
}

// V3ImportOutcome is one record's fate.
type V3ImportOutcome struct {
	V3ID   string
	Email  string
	Status string // "imported", "skipped", or "flagged"
	Reason string // set for "flagged" and "skipped"
}

// V3ImportReport is ImportV3Users's result.
type V3ImportReport struct {
	Imported int
	Skipped  int
	Flagged  int
	Outcomes []V3ImportOutcome
}

// ImportV3Users imports records into identity.users (task 1.13).
// Idempotent by email, not by a dedicated v3-id column: email is
// already the data model's own UNIQUE sign-in identity, so a record
// whose (normalized) email is already in identity.users — whether
// from an earlier run of this same importer or a duplicate within
// this batch — comes back from CreateV3ImportedUser as pgx.ErrNoRows
// and is reported "skipped," not re-inserted or errored. Adding a
// separate v3_id column (data-model.md has none) would only matter if
// two different v3 emails ever needed to map to one v4 user, which
// isn't this task's problem to solve.
//
// A record with no usable email is "flagged," never imported — task
// 1.13's own instruction ("users without a usable email are
// flagged"); the v3 password hash is still copied in as-is for anyone
// who *is* imported, verified lazily and upgraded to Argon2id on that
// account's first real v4 sign-in (Service.SignIn, same task).
func (s *Service) ImportV3Users(ctx context.Context, records []V3UserRecord) (*V3ImportReport, error) {
	report := &V3ImportReport{Outcomes: make([]V3ImportOutcome, 0, len(records))}

	for _, rec := range records {
		email, err := domain.NormalizeEmail(rec.EmailAddress)
		if err != nil {
			report.Flagged++
			report.Outcomes = append(report.Outcomes, V3ImportOutcome{
				V3ID: rec.V3ID, Email: rec.EmailAddress, Status: "flagged",
				Reason: fmt.Sprintf("no usable email: %v", err),
			})
			continue
		}

		outcome, err := s.importOneV3User(ctx, rec, email)
		if err != nil {
			return nil, err
		}
		switch outcome.Status {
		case "imported":
			report.Imported++
		case "skipped":
			report.Skipped++
		}
		report.Outcomes = append(report.Outcomes, outcome)
	}

	return report, nil
}

func (s *Service) importOneV3User(ctx context.Context, rec V3UserRecord, email string) (V3ImportOutcome, error) {
	fullName := strings.TrimSpace(rec.FullNames)

	var phone pgtype.Text
	if e164, ok := domain.NormalizeV3MSISDN(rec.Msisdn); ok {
		phone = pgtype.Text{String: e164, Valid: true}
	}

	id, err := ids.New()
	if err != nil {
		return V3ImportOutcome{}, fmt.Errorf("app: generating user id for v3 import: %w", err)
	}

	_, err = store.New(s.pool).CreateV3ImportedUser(ctx, store.CreateV3ImportedUserParams{
		ID:           id,
		Email:        email,
		FullName:     fullName,
		PhoneE164:    phone,
		PasswordHash: pgtype.Text{String: rec.Password, Valid: rec.Password != ""},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return V3ImportOutcome{V3ID: rec.V3ID, Email: email, Status: "skipped", Reason: "email already in use"}, nil
	}
	if err != nil {
		return V3ImportOutcome{}, fmt.Errorf("app: importing v3 user %s: %w", rec.V3ID, err)
	}
	return V3ImportOutcome{V3ID: rec.V3ID, Email: email, Status: "imported"}, nil
}
