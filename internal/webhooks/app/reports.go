package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/adapters"
)

// MaxReportDays is the longest range a report covers.
const MaxReportDays = 92

// ActivityReport is a client's daily activity and the zone its days
// are in.
type ActivityReport struct {
	TimeZone string
	Days     []adapters.DailyActivity
}

// SinkEvents copies one batch of bus messages into the events table in
// one insert (task 5.6). A message that isn't an event envelope is
// logged and skipped, so one bad message can't hold up the rest; an
// insert error is returned, so the bus delivers the whole batch again.
// Every event goes in, internal ones and those with no client included;
// the source is the publishing service, the subject's first part.
func (s *Service) SinkEvents(ctx context.Context, msgs []bus.Message) error {
	if s.events == nil {
		return errors.New("app: no event store for the sink")
	}
	rows := make([]adapters.EventRow, 0, len(msgs))
	for _, m := range msgs {
		var env events.Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil || env.ID == uuid.Nil || env.Type == "" || env.OccurredAt.IsZero() {
			s.log.WarnContext(ctx, "webhooks: skipping a message that isn't an event", "subject", m.Subject)
			continue
		}
		source, _, _ := strings.Cut(m.Subject, ".")
		data := string(env.Data)
		if data == "" {
			data = "{}"
		}
		rows = append(rows, adapters.EventRow{
			ID: env.ID, Type: env.Type, Version: uint16(min(max(env.Version, 0), 1<<16-1)), //nolint:gosec // G115: clamped
			TenantID: env.TenantID, Source: source, OccurredAt: env.OccurredAt, Data: data,
		})
	}
	if err := s.events.InsertEvents(ctx, rows); err != nil {
		return fmt.Errorf("app: sinking events: %w", err)
	}
	return nil
}

// DailyActivity is a client's wallet activity per day and currency, from
// through to (YYYY-MM-DD, inclusive, at most MaxReportDays), in the time
// zone of the account's country (billing:read).
func (s *Service) DailyActivity(ctx context.Context, user, tenant uuid.UUID, from, to string) (ActivityReport, error) {
	info, err := s.tenantInfo(ctx, user, tenant, PermBillingRead)
	if err != nil {
		return ActivityReport{}, err
	}
	start, end, err := reportRange(from, to)
	if err != nil {
		return ActivityReport{}, err
	}
	pack, ok := s.packs[info.CountryCode]
	if !ok {
		return ActivityReport{}, fmt.Errorf("app: no pack for the account's country %q", info.CountryCode)
	}
	if s.events == nil {
		return ActivityReport{}, errors.New("app: no event store for reports")
	}
	days, err := s.events.DailyActivity(ctx, tenant, start, end, pack.Timezone)
	if err != nil {
		return ActivityReport{}, fmt.Errorf("app: %w", err)
	}
	return ActivityReport{TimeZone: pack.Timezone, Days: days}, nil
}

// TopupsByRail is top-ups across clients per day in zone, rail and
// currency, from through to. For staff; no API yet.
func (s *Service) TopupsByRail(ctx context.Context, from, to string, zone string) ([]adapters.TopupsByRail, error) {
	start, end, err := reportRange(from, to)
	if err != nil {
		return nil, err
	}
	if _, err := time.LoadLocation(zone); err != nil || zone == "" {
		return nil, fmt.Errorf("%w: %q isn't a time zone", ErrValidation, zone)
	}
	if s.events == nil {
		return nil, errors.New("app: no event store for reports")
	}
	out, err := s.events.TopupsByRail(ctx, start, end, zone)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	return out, nil
}

// EventsPerType counts every event per UTC day and type, from through
// to. For staff; no API yet.
func (s *Service) EventsPerType(ctx context.Context, from, to string) ([]adapters.EventsPerType, error) {
	start, end, err := reportRange(from, to)
	if err != nil {
		return nil, err
	}
	if s.events == nil {
		return nil, errors.New("app: no event store for reports")
	}
	out, err := s.events.EventsPerType(ctx, start, end)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	return out, nil
}

// reportRange reads from and to (YYYY-MM-DD, inclusive): to not before
// from, and at most MaxReportDays days.
func reportRange(from, to string) (time.Time, time.Time, error) {
	start, err1 := time.Parse(time.DateOnly, from)
	end, err2 := time.Parse(time.DateOnly, to)
	if err1 != nil || err2 != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: from and to are dates, YYYY-MM-DD", ErrValidation)
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: to is before from", ErrValidation)
	}
	if end.Sub(start) >= MaxReportDays*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: a report covers at most %d days", ErrValidation, MaxReportDays)
	}
	return start, end, nil
}
