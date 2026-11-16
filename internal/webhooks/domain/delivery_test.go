package domain_test

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

func TestDefaultSchedule_IsOQ23s(t *testing.T) {
	want := []time.Duration{
		30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour,
		3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 24 * time.Hour,
	}
	if !slices.Equal(domain.DefaultSchedule, want) {
		t.Fatalf("DefaultSchedule = %v, want %v", domain.DefaultSchedule, want)
	}
	if got := domain.MaxAttempts(domain.DefaultSchedule); got != 11 {
		t.Fatalf("MaxAttempts = %d, want 11: one at once, then a retry after each wait", got)
	}
}

func TestParseSchedule(t *testing.T) {
	got, err := domain.ParseSchedule("30s, 2m,10m,30m,1h,3h,6h,12h,24h,24h")
	if err != nil || !slices.Equal(got, domain.DefaultSchedule) {
		t.Fatalf("ParseSchedule = %v, %v", got, err)
	}
	for _, bad := range []string{"", " ", "30s,,2m", "30s,x", "0s", "-1m", "30"} {
		if _, err := domain.ParseSchedule(bad); err == nil {
			t.Errorf("ParseSchedule(%q) accepted it", bad)
		}
	}
}

func TestNextAttempt_ExactWithoutJitter(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	schedule := []time.Duration{30 * time.Second, 2 * time.Minute, 24 * time.Hour}
	for made, wait := range schedule {
		next, ok := domain.NextAttempt(schedule, made+1, now, 0, nil)
		if !ok || !next.Equal(now.Add(wait)) {
			t.Fatalf("after attempt %d: %v %v, want %v", made+1, next, ok, now.Add(wait))
		}
	}
	if _, ok := domain.NextAttempt(schedule, 4, now, 0, nil); ok {
		t.Fatal("after the 4th attempt (three waits spent) there should be no other")
	}
}

func TestNextAttempt_JitterStaysWithinItsFraction(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	rnd := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a seeded source, so the draws repeat; retry jitter isn't a secret
	gap := domain.DefaultSchedule[3]   // 30m
	lo, hi := now.Add(gap*9/10), now.Add(gap*11/10)
	sawBelow, sawAbove := false, false
	for range 1000 {
		next, ok := domain.NextAttempt(domain.DefaultSchedule, 4, now, 0.1, rnd.Float64)
		if !ok || next.Before(lo) || next.After(hi) {
			t.Fatalf("next %v outside [%v, %v]", next, lo, hi)
		}
		sawBelow = sawBelow || next.Before(now.Add(gap))
		sawAbove = sawAbove || next.After(now.Add(gap))
	}
	if !sawBelow || !sawAbove {
		t.Fatal("jitter should spread both ways")
	}
}

func TestShouldDisable(t *testing.T) {
	since := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	after := 72 * time.Hour
	for name, c := range map[string]struct {
		at   time.Time
		want bool
	}{
		"a second short":   {since.Add(after - time.Second), false},
		"exactly 72 hours": {since.Add(after), true},
		"a second past":    {since.Add(after + time.Second), true},
	} {
		if got := domain.ShouldDisable(since, c.at, after); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func envelope(t *testing.T, eventType string, tenant uuid.UUID, data any) events.Envelope {
	t.Helper()
	env, err := events.New(uuid.New(), eventType, 1, tenant, time.Date(2026, 11, 16, 9, 0, 0, 123000000, time.UTC), "trace-abc", data)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestDeliveryBody_IsTheEnvelopeWithoutTheTrace(t *testing.T) {
	tenant := uuid.New()
	env := envelope(t, "wallet.credited", tenant, map[string]any{"tenant_id": tenant, "amount_minor": 500})
	body, err := domain.DeliveryBody(env)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"data", "id", "occurred_at", "tenant_id", "type", "version"}) {
		t.Fatalf("keys %v", keys)
	}
	if string(got["id"]) != `"`+env.ID.String()+`"` || string(got["occurred_at"]) != `"2026-11-16T09:00:00.123Z"` {
		t.Fatalf("id %s, occurred_at %s", got["id"], got["occurred_at"])
	}
	var data map[string]any
	_ = json.Unmarshal(got["data"], &data)
	if data["amount_minor"] != float64(500) {
		t.Fatalf("data %s", got["data"])
	}
}

func TestRecipient(t *testing.T) {
	tenant := uuid.New()
	cases := []struct {
		name string
		env  events.Envelope
		ok   bool
	}{
		{"a client event names its client", envelope(t, "wallet.credited", tenant, map[string]any{"tenant_id": tenant}), true},
		{"an internal event goes nowhere", envelope(t, "ledger.rebuild_failed", tenant, map[string]any{"tenant_id": tenant}), false},
		{"no tenant on the envelope", envelope(t, "wallet.credited", uuid.Nil, map[string]any{"tenant_id": tenant}), false},
		{"no tenant in the data", envelope(t, "wallet.credited", tenant, map[string]any{"amount_minor": 1}), false},
		{"the envelope and data disagree", envelope(t, "wallet.credited", tenant, map[string]any{"tenant_id": uuid.New()}), false},
		{"a malformed data tenant", envelope(t, "wallet.credited", tenant, map[string]any{"tenant_id": "x"}), false},
		{"data that isn't an object", envelope(t, "wallet.credited", tenant, []int{1}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := domain.Recipient(c.env)
			if ok != c.ok || (ok && got != tenant) {
				t.Fatalf("Recipient = %v, %v, want %v", got, ok, c.ok)
			}
		})
	}
}
