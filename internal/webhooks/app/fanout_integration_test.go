//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

func TestHandleEvent_OneDeliveryPerActiveSubscribedEndpoint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())

	byType := activeEndpoint(t, h, m, h.recv, "wallet.credited")
	all := activeEndpoint(t, h, m, h.recv, "*")
	activeEndpoint(t, h, m, h.recv, "tokens.purchased")
	create(t, h, m, h.recv.URL(), "*") // pending verification
	deleted := activeEndpoint(t, h, m, h.recv, "*")
	if err := h.svc.DeleteEndpoint(ctx, m.user, m.tenant, deleted.ID); err != nil {
		t.Fatal(err)
	}
	disabled := activeEndpoint(t, h, m, h.recv, "*")
	if _, err := h.super.Exec(ctx, `UPDATE endpoints SET status = 'disabled', disabled_at = now(), disabled_reason = 'client' WHERE id = $1`, disabled.ID); err != nil {
		t.Fatal(err)
	}
	other := admin(h, uuid.New())
	activeEndpoint(t, h, other, h.recv, "*")

	env := publish(t, h, "wallet.credited", m.tenant)
	ds := deliveriesFor(t, h, env.ID)
	var got []uuid.UUID
	for _, d := range ds {
		got = append(got, d.endpoint)
		if d.status != "pending" || d.attempts != 0 || d.next == nil || !d.next.Equal(h.clk.Now()) {
			t.Fatalf("delivery %+v: want pending, due now", d)
		}
	}
	want := []uuid.UUID{byType.ID, all.ID}
	slices.SortFunc(got, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	slices.SortFunc(want, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	if !slices.Equal(got, want) {
		t.Fatalf("delivered to %v, want only the active subscribed endpoints %v", got, want)
	}

	t.Run("the body is the envelope without its trace", func(t *testing.T) {
		wantBody, err := domain.DeliveryBody(env)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			if !bytes.Equal(d.body, wantBody) {
				t.Fatalf("body %s, want %s", d.body, wantBody)
			}
			if bytes.Contains(d.body, []byte("trace")) {
				t.Fatal("the body carries the trace ID")
			}
		}
	})

	t.Run("the same event again makes no second delivery", func(t *testing.T) {
		raw, _ := json.Marshal(env)
		for range 3 {
			if err := h.svc.HandleEvent(ctx, raw); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(deliveriesFor(t, h, env.ID)); n != 2 {
			t.Fatalf("%d deliveries after redelivery, want 2", n)
		}
		if n := count(t, h, `SELECT count(*) FROM processed_events WHERE event_id = $1`, env.ID); n != 1 {
			t.Fatalf("processed %d times", n)
		}
	})
}

func TestHandleEvent_SendsNothingThatIsntForAClient(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	activeEndpoint(t, h, m, h.recv, "*")

	mismatched, _ := events.New(uuid.New(), "wallet.credited", 1, m.tenant, h.clk.Now(), "", map[string]any{"tenant_id": uuid.New()})
	noTenant, _ := events.New(uuid.New(), "wallet.credited", 1, uuid.Nil, h.clk.Now(), "", map[string]any{"tenant_id": m.tenant})
	internal, _ := events.New(uuid.New(), "ledger.rebuild_failed", 1, m.tenant, h.clk.Now(), "", map[string]any{"tenant_id": m.tenant})
	for name, raw := range map[string][]byte{
		"not JSON":                       []byte("{nope"),
		"an internal event":              mustJSON(t, internal),
		"no tenant on the envelope":      mustJSON(t, noTenant),
		"the envelope and data disagree": mustJSON(t, mismatched),
		"an empty object":                []byte("{}"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := h.svc.HandleEvent(ctx, raw); err != nil {
				t.Fatalf("HandleEvent = %v; it should be acknowledged, not retried", err)
			}
		})
	}
	if n := count(t, h, `SELECT count(*) FROM deliveries`); n != 0 {
		t.Fatalf("%d deliveries written", n)
	}
	if n := count(t, h, `SELECT count(*) FROM processed_events`); n != 0 {
		t.Fatalf("%d events recorded as processed", n)
	}
}

func TestHandleEvent_NoEndpointsStillRecordsIt(t *testing.T) {
	h := newHarness(t)
	env := publish(t, h, "wallet.credited", uuid.New())
	if n := count(t, h, `SELECT count(*) FROM processed_events WHERE event_id = $1`, env.ID); n != 1 {
		t.Fatalf("processed %d", n)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
