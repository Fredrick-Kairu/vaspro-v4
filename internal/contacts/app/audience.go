package app

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/contacts/store"
)

// MaxConsentBatch is the most numbers one CheckConsent answers (data
// model §10).
const MaxConsentBatch = 10000

// ConsentAnswer is CheckConsent's answer for one number: Number in
// E.164, or as given when it isn't valid.
type ConsentAnswer struct {
	Number  string
	Allowed bool
	Reason  string
}

// Recipient is one contact an audience may message.
type Recipient struct {
	ContactID uuid.UUID
	Number    string
}

// AudienceRequest asks for one page of a group's or segment's contacts
// with consent applied.
type AudienceRequest struct {
	GroupID, SegmentID *uuid.UUID
	Channel, Purpose   string
	PageSize           int    // 1 to 5,000; 1,000 when 0
	Cursor             string // the Next of the page before
}

// AudiencePage is one page: the recipients allowed, how many of the
// page's contacts were blocked by reason, and the cursor of the next
// page ("" on the last).
type AudiencePage struct {
	Recipients []Recipient
	Blocked    map[string]int64
	Next       string
}

// decider answers consent for numbers already in E.164, in the caller's
// tenant transaction, with one query.
func (s *Service) decide(ctx context.Context, q *store.Queries, tenant uuid.UUID, country, channel, purpose string, phones []string) (map[string]domain.Decision, error) {
	out := make(map[string]domain.Decision, len(phones))
	if len(phones) == 0 {
		return out, nil
	}
	rows, err := q.ConsentForNumbers(ctx, store.ConsentForNumbersParams{
		TenantID: tenant, Channel: store.ContactsChannel(channel), Purpose: store.ContactsPurpose(purpose), Phones: phones,
	})
	if err != nil {
		return nil, err
	}
	optIn := requiresOptIn(s.packs[country])
	for _, r := range rows {
		in := domain.DecisionInput{ValidNumber: true, Channel: channel, Purpose: purpose, Status: r.Status, RequiresOptIn: optIn}
		if r.Suppressed {
			in.Suppressions = []string{channel}
		}
		out[r.Phone] = domain.DecideConsent(in)
	}
	return out, nil
}

// CheckConsent answers, for up to MaxConsentBatch numbers in the
// request's order, whether a message on channel for purpose may go to
// each (data model §10). Messaging calls it over gRPC: it trusts the
// tenant, with no user to check.
func (s *Service) CheckConsent(ctx context.Context, tenant uuid.UUID, channel, purpose string, numbers []string) ([]ConsentAnswer, error) {
	if err := checkChannelPurpose(channel, purpose); err != nil {
		return nil, err
	}
	if len(numbers) > MaxConsentBatch {
		return nil, fmt.Errorf("%w: at most %d numbers", ErrValidation, MaxConsentBatch)
	}
	country, err := s.soleCountry()
	if err != nil {
		return nil, err
	}
	rules, err := s.phoneRules(country)
	if err != nil {
		return nil, err
	}
	out := make([]ConsentAnswer, len(numbers))
	phones := make([]string, 0, len(numbers))
	for i, raw := range numbers {
		phone, err := domain.NormalizePhone(rules, raw)
		if err != nil {
			out[i] = ConsentAnswer{Number: raw, Reason: domain.ReasonInvalidNumber}
			continue
		}
		out[i].Number = phone
		phones = append(phones, phone)
	}
	var decisions map[string]domain.Decision
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		decisions, err = s.decide(ctx, store.New(tx), tenant, country, channel, purpose, phones)
		return err
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Reason == domain.ReasonInvalidNumber {
			continue
		}
		d := decisions[out[i].Number]
		out[i].Allowed, out[i].Reason = d.Allowed, d.Reason
	}
	return out, nil
}

// ExpandAudience reads one page of a group's or segment's contacts,
// newest first, and applies consent to it, in one tenant transaction.
func (s *Service) ExpandAudience(ctx context.Context, tenant uuid.UUID, req AudienceRequest) (AudiencePage, error) {
	if err := checkChannelPurpose(req.Channel, req.Purpose); err != nil {
		return AudiencePage{}, err
	}
	if (req.GroupID == nil) == (req.SegmentID == nil) {
		return AudiencePage{}, fmt.Errorf("%w: a group or a segment", ErrValidation)
	}
	size := req.PageSize
	switch {
	case size == 0:
		size = 1000
	case size < 1 || size > 5000:
		return AudiencePage{}, fmt.Errorf("%w: page size 1 to 5,000", ErrValidation)
	}
	after, afterID, err := parseTimeCursor(req.Cursor)
	if err != nil {
		return AudiencePage{}, err
	}
	country, err := s.soleCountry()
	if err != nil {
		return AudiencePage{}, err
	}
	var rows []store.ContactsContact
	var decisions map[string]domain.Decision
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if req.GroupID != nil {
			rows, err = s.groupPage(ctx, q, *req.GroupID, after, afterID, int32(size)+1) //nolint:gosec // G115: at most 5,001
		} else {
			rows, err = s.segmentPage(ctx, q, tenant, country, *req.SegmentID, after, afterID, int32(size)+1) //nolint:gosec // G115: at most 5,001
		}
		if err != nil {
			return err
		}
		phones := make([]string, 0, len(rows))
		for _, r := range rows {
			phones = append(phones, r.PhoneE164)
		}
		decisions, err = s.decide(ctx, q, tenant, country, req.Channel, req.Purpose, phones)
		return err
	})
	if err != nil {
		return AudiencePage{}, err
	}
	page := AudiencePage{Blocked: map[string]int64{}}
	if len(rows) > size {
		rows = rows[:size]
		last := rows[len(rows)-1]
		page.Next = timeCursor(last.CreatedAt, last.ID)
	}
	for _, r := range rows {
		d := decisions[r.PhoneE164]
		if d.Allowed {
			page.Recipients = append(page.Recipients, Recipient{ContactID: r.ID, Number: r.PhoneE164})
			continue
		}
		page.Blocked[d.Reason]++
	}
	return page, nil
}

func (s *Service) groupPage(ctx context.Context, q *store.Queries, group uuid.UUID, after *time.Time, afterID uuid.UUID, limit int32) ([]store.ContactsContact, error) {
	if _, err := q.GetGroup(ctx, group); err != nil {
		return nil, notFound(err, "group")
	}
	params := store.ListContactsParams{GroupID: pgtype.UUID{Bytes: group, Valid: true}, PageSize: limit}
	if after != nil {
		params.AfterCreated = pgtype.Timestamptz{Time: *after, Valid: true}
		params.AfterID = pgtype.UUID{Bytes: afterID, Valid: true}
	}
	return q.ListContacts(ctx, params)
}
