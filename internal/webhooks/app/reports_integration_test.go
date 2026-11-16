//go:build integration

package app_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/bus"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/app"
)

// message is an event as the bus carries it, on subject.
func message(t *testing.T, subject, eventType string, tenant uuid.UUID, at time.Time, data map[string]any) bus.Message {
	t.Helper()
	if tenant != uuid.Nil {
		data["tenant_id"] = tenant
	}
	env, err := events.New(uuid.New(), eventType, 1, tenant, at, "trace", data)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return bus.Message{Subject: subject, Data: raw}
}

// reportFixture is a fixed set of events for two clients across two
// Nairobi days.
type reportFixture struct {
	a, b   uuid.UUID
	msgs   []bus.Message
	firstA bus.Message // tenant a's first top-up
}

func newReportFixture(t *testing.T) reportFixture {
	a, b := uuid.New(), uuid.New()
	day := func(h, m int) time.Time { return time.Date(2026, 11, 10, h, m, 0, 0, time.UTC) }
	f := reportFixture{a: a, b: b}
	f.firstA = message(t, "ledger.wallet.credited", "wallet.credited", a, day(6, 0),
		map[string]any{"transaction_type": "topup", "currency": "KES", "amount_minor": 100000, "rail": "mpesa_ke"})
	f.msgs = []bus.Message{
		f.firstA,
		message(t, "ledger.wallet.credited", "wallet.credited", a, day(10, 0),
			map[string]any{"transaction_type": "topup", "currency": "KES", "amount_minor": 50000, "rail": "sandbox"}),
		message(t, "ledger.tokens.purchased", "tokens.purchased", a, day(11, 0),
			map[string]any{"currency": "KES", "cost_minor": 80000, "tokens": "100000"}),
		message(t, "ledger.usage.posted", "usage.posted", a, day(12, 0),
			map[string]any{"currency": "KES", "value_minor": 3000, "events": 5}),
		message(t, "ledger.wallet.credited", "wallet.credited", a, day(13, 0),
			map[string]any{"transaction_type": "refund", "currency": "KES", "amount_minor": 2000}),
		// 22:30 UTC is 01:30 the next day in Nairobi.
		message(t, "ledger.wallet.credited", "wallet.credited", a, day(22, 30),
			map[string]any{"transaction_type": "topup", "currency": "KES", "amount_minor": 7000}),
		message(t, "accounts.tenant.created", "tenant.created", a, day(5, 0), map[string]any{"display_name": "A"}),
		message(t, "ledger.ledger.rebuild_passed", "ledger.rebuild_passed", uuid.Nil, day(21, 30), map[string]any{"run_date": "2026-11-10"}),
		message(t, "ledger.wallet.credited", "wallet.credited", b, day(9, 0),
			map[string]any{"transaction_type": "topup", "currency": "KES", "amount_minor": 999999, "rail": "mpesa_ke"}),
	}
	return f
}

func activityOf(t *testing.T, h harness, user, tenant uuid.UUID, from, to string) app.ActivityReport {
	t.Helper()
	got, err := h.svc.DailyActivity(context.Background(), user, tenant, from, to)
	if err != nil {
		t.Fatalf("DailyActivity: %v", err)
	}
	return got
}

func TestSinkEvents_TheSameEventTwiceIsCountedOnce(t *testing.T) {
	var ch *sql.DB
	h := newHarness(t, withClickHouse(t, &ch))
	ctx := context.Background()
	f := newReportFixture(t)
	reader := h.member(f.a, app.PermBillingRead)

	// The whole set, then the first top-up again in its own batch, and
	// twice within one batch.
	for _, batch := range [][]bus.Message{f.msgs, {f.firstA}, {f.firstA, f.firstA}} {
		if err := h.svc.SinkEvents(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	check := func(t *testing.T, when string) {
		t.Helper()
		got := activityOf(t, h, reader, f.a, "2026-11-10", "2026-11-10")
		if len(got.Days) != 1 || got.Days[0].CreditedMinor != 152000 || got.Days[0].Topups != 2 {
			t.Fatalf("%s: %+v, want 152,000 credited in 2 top-ups (the repeated event once)", when, got.Days)
		}
	}
	check(t, "before a merge")
	optimize(t, ch)
	check(t, "after OPTIMIZE ... FINAL")
}

// optimize forces ClickHouse to merge every part, collapsing copies on
// disk.
func optimize(t *testing.T, ch *sql.DB) {
	t.Helper()
	if _, err := ch.ExecContext(context.Background(), "OPTIMIZE TABLE events FINAL"); err != nil {
		t.Fatal(err)
	}
}

// member is a user in tenant with exactly perms.
func (h harness) member(tenant uuid.UUID, perms ...string) uuid.UUID {
	user := uuid.New()
	h.accounts.Grant(user, tenant, perms...)
	return user
}

func TestDailyActivity_OnAFixedEventSet(t *testing.T) {
	var ch *sql.DB
	h := newHarness(t, withClickHouse(t, &ch))
	ctx := context.Background()
	f := newReportFixture(t)
	if err := h.svc.SinkEvents(ctx, f.msgs); err != nil {
		t.Fatal(err)
	}
	reader := h.member(f.a, app.PermBillingRead)

	got := activityOf(t, h, reader, f.a, "2026-11-09", "2026-11-12")
	if got.TimeZone != "Africa/Nairobi" || len(got.Days) != 2 {
		t.Fatalf("report %+v, want two Nairobi days", got)
	}
	d1, d2 := got.Days[0], got.Days[1]
	if d1.Day.Format("2006-01-02") != "2026-11-10" || d1.Currency != "KES" || d1.CreditedMinor != 152000 || d1.Topups != 2 ||
		d1.TokenPurchasesMinor != 80000 || d1.UsageMinor != 3000 || d1.UsageEvents != 5 {
		t.Fatalf("10 Nov: %+v", d1)
	}
	if d2.Day.Format("2006-01-02") != "2026-11-11" || d2.CreditedMinor != 7000 || d2.Topups != 1 || d2.TokenPurchasesMinor != 0 {
		t.Fatalf("11 Nov (the 22:30 UTC top-up, 01:30 in Nairobi): %+v", d2)
	}

	t.Run("another client's events never appear", func(t *testing.T) {
		readerB := h.member(f.b, app.PermBillingRead)
		got := activityOf(t, h, readerB, f.b, "2026-11-09", "2026-11-12")
		if len(got.Days) != 1 || got.Days[0].CreditedMinor != 999999 {
			t.Fatalf("b's report %+v", got.Days)
		}
	})
	t.Run("a narrower range", func(t *testing.T) {
		if got := activityOf(t, h, reader, f.a, "2026-11-11", "2026-11-11"); len(got.Days) != 1 || got.Days[0].CreditedMinor != 7000 {
			t.Fatalf("11 Nov alone: %+v", got.Days)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		_, err := h.svc.DailyActivity(ctx, h.member(f.a, app.PermCredentialsRead), f.a, "2026-11-10", "2026-11-10")
		wantErr(t, err, app.ErrPermissionDenied)
		_, err = h.svc.DailyActivity(ctx, uuid.New(), f.a, "2026-11-10", "2026-11-10")
		wantErr(t, err, app.ErrNotFound)
		for name, r := range map[string][2]string{
			"not a date":     {"10/11/2026", "2026-11-10"},
			"to before from": {"2026-11-10", "2026-11-09"},
			"over 92 days":   {"2026-01-01", "2026-04-03"},
			"empty":          {"", ""},
		} {
			if _, err := h.svc.DailyActivity(ctx, reader, f.a, r[0], r[1]); !isValidation(err) {
				t.Errorf("%s: %v, want ErrValidation", name, err)
			}
		}
		if _, err := h.svc.DailyActivity(ctx, reader, f.a, "2026-01-01", "2026-04-02"); err != nil {
			t.Errorf("exactly 92 days: %v", err)
		}
	})
}

func TestStaffReports_OnAFixedEventSet(t *testing.T) {
	var ch *sql.DB
	h := newHarness(t, withClickHouse(t, &ch))
	ctx := context.Background()
	f := newReportFixture(t)
	if err := h.svc.SinkEvents(ctx, f.msgs); err != nil {
		t.Fatal(err)
	}

	rails, err := h.svc.TopupsByRail(ctx, "2026-11-10", "2026-11-11", "Africa/Nairobi")
	if err != nil {
		t.Fatal(err)
	}
	type railKey struct{ day, rail string }
	gotRails := map[railKey][2]int64{}
	for _, r := range rails {
		gotRails[railKey{r.Day.Format("2006-01-02"), r.Rail}] = [2]int64{r.Topups, r.AmountMinor}
	}
	wantRails := map[railKey][2]int64{
		{"2026-11-10", "mpesa_ke"}:     {2, 1099999},
		{"2026-11-10", "sandbox"}:      {1, 50000},
		{"2026-11-11", "unattributed"}: {1, 7000},
	}
	if len(gotRails) != len(wantRails) {
		t.Fatalf("top-ups by rail %v, want %v", gotRails, wantRails)
	}
	for k, w := range wantRails {
		if gotRails[k] != w {
			t.Fatalf("top-ups by rail %v, want %v", gotRails, wantRails)
		}
	}

	types, err := h.svc.EventsPerType(ctx, "2026-11-10", "2026-11-10")
	if err != nil {
		t.Fatal(err)
	}
	gotTypes := map[string]int64{}
	for _, e := range types {
		gotTypes[e.Type] = e.Events
	}
	want := map[string]int64{"wallet.credited": 5, "tokens.purchased": 1, "usage.posted": 1, "tenant.created": 1, "ledger.rebuild_passed": 1}
	if len(gotTypes) != len(want) {
		t.Fatalf("events per type %v, want %v", gotTypes, want)
	}
	for k, w := range want {
		if gotTypes[k] != w {
			t.Fatalf("events per type %v, want %v", gotTypes, want)
		}
	}
	if _, err := h.svc.TopupsByRail(ctx, "2026-11-10", "2026-11-11", "Not/AZone"); !isValidation(err) {
		t.Fatalf("an unknown zone: %v", err)
	}
}

func TestSinkEvents_SkipsAMalformedMessage(t *testing.T) {
	var ch *sql.DB
	h := newHarness(t, withClickHouse(t, &ch))
	ctx := context.Background()
	f := newReportFixture(t)
	batch := []bus.Message{{Subject: "ledger.wallet.credited", Data: []byte("{not json")}, {Subject: "ledger.x", Data: []byte(`{}`)}, f.firstA}
	if err := h.svc.SinkEvents(ctx, batch); err != nil {
		t.Fatalf("a malformed message should be skipped, not fail the batch: %v", err)
	}
	reader := h.member(f.a, app.PermBillingRead)
	if got := activityOf(t, h, reader, f.a, "2026-11-10", "2026-11-10"); len(got.Days) != 1 || got.Days[0].CreditedMinor != 100000 {
		t.Fatalf("%+v", got.Days)
	}
	var n int64
	if err := ch.QueryRowContext(ctx, "SELECT count() FROM events FINAL").Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d events stored (%v), want 1", n, err)
	}
	var source string
	if err := ch.QueryRowContext(ctx, "SELECT source FROM events FINAL LIMIT 1").Scan(&source); err != nil || source != "ledger" {
		t.Fatalf("source %q (%v), want the subject's service", source, err)
	}
}
