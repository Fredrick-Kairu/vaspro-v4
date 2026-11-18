//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

func TestListDeliveries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	a := activeEndpoint(t, h, m, h.recv, "*")
	bRecv := newReceiver(t)
	b := activeEndpoint(t, h, m, bRecv, "wallet.credited")

	var events []uuid.UUID
	for _, typ := range []string{"wallet.credited", "tokens.purchased", "wallet.credited", "usage.posted"} {
		h.clk.Advance(time.Second)
		events = append(events, publish(t, h, typ, m.tenant).ID)
	}
	bRecv.set(fail500)
	deliverDue(t, h) // a's four succeed; b's two fail and stay pending
	other := admin(h, uuid.New())
	activeEndpoint(t, h, other, h.recv, "*")
	publish(t, h, "wallet.credited", other.tenant)

	list := func(t *testing.T, f app.DeliveryFilter) []app.Delivery {
		t.Helper()
		got, _, err := h.svc.ListDeliveries(ctx, m.user, m.tenant, f, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("all of the account's, newest first", func(t *testing.T) {
		got := list(t, app.DeliveryFilter{})
		if len(got) != 6 {
			t.Fatalf("%d deliveries, want 6 (another account's excluded)", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i].CreatedAt.After(got[i-1].CreatedAt) {
				t.Fatalf("not newest first at %d", i)
			}
		}
	})
	t.Run("by endpoint", func(t *testing.T) {
		if got := list(t, app.DeliveryFilter{EndpointID: &b.ID}); len(got) != 2 {
			t.Fatalf("%d for b", len(got))
		}
		if got := list(t, app.DeliveryFilter{EndpointID: &a.ID}); len(got) != 4 {
			t.Fatalf("%d for a", len(got))
		}
	})
	t.Run("by status", func(t *testing.T) {
		got := list(t, app.DeliveryFilter{Status: "pending"})
		if len(got) != 2 || got[0].EndpointID != b.ID || got[0].Attempts != 1 || got[0].NextAttemptAt == nil {
			t.Fatalf("pending %+v", got)
		}
		if got := list(t, app.DeliveryFilter{Status: "succeeded"}); len(got) != 4 {
			t.Fatalf("%d succeeded", len(got))
		}
	})
	t.Run("by type and by event", func(t *testing.T) {
		if got := list(t, app.DeliveryFilter{EventType: "wallet.credited"}); len(got) != 4 {
			t.Fatalf("%d wallet.credited", len(got))
		}
		got := list(t, app.DeliveryFilter{EventID: &events[1]})
		if len(got) != 1 || got[0].EventType != "tokens.purchased" || got[0].EventID != events[1] {
			t.Fatalf("by event %+v", got)
		}
	})
	t.Run("pages of one, every row once", func(t *testing.T) {
		seen := map[uuid.UUID]bool{}
		cursor := ""
		for page := 0; ; page++ {
			got, next, err := h.svc.ListDeliveries(ctx, m.user, m.tenant, app.DeliveryFilter{}, cursor, 1)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range got {
				if seen[d.ID] {
					t.Fatalf("%s twice", d.ID)
				}
				seen[d.ID] = true
			}
			if next == "" {
				break
			}
			if page > 10 {
				t.Fatal("the cursor never ends")
			}
			cursor = next
		}
		if len(seen) != 6 {
			t.Fatalf("paged through %d, want 6", len(seen))
		}
	})
	t.Run("refusals", func(t *testing.T) {
		for name, call := range map[string]func() error{
			"a bad cursor": func() error {
				_, _, err := h.svc.ListDeliveries(ctx, m.user, m.tenant, app.DeliveryFilter{}, "not-a-cursor", 10)
				return err
			},
			"a limit over 100": func() error {
				_, _, err := h.svc.ListDeliveries(ctx, m.user, m.tenant, app.DeliveryFilter{}, "", 101)
				return err
			},
			"an unknown status": func() error {
				_, _, err := h.svc.ListDeliveries(ctx, m.user, m.tenant, app.DeliveryFilter{Status: "lost"}, "", 10)
				return err
			},
		} {
			if err := call(); err == nil || !isValidation(err) {
				t.Errorf("%s: %v, want ErrValidation", name, err)
			}
		}
		_, _, err := h.svc.ListDeliveries(ctx, uuid.New(), m.tenant, app.DeliveryFilter{}, "", 10)
		wantErr(t, err, app.ErrNotFound)
		v := viewer(h, m.tenant)
		if _, _, err := h.svc.ListDeliveries(ctx, v.user, v.tenant, app.DeliveryFilter{}, "", 10); err != nil {
			t.Fatalf("a Viewer reads the log: %v", err)
		}
	})
}

func isValidation(err error) bool { return errors.Is(err, app.ErrValidation) }

func TestGetDelivery_ShowsEveryAttemptWithItsResponse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	activeEndpoint(t, h, m, h.recv, "*")
	h.recv.then(fail500)
	env := publish(t, h, "wallet.credited", m.tenant)
	deliverDue(t, h)
	h.clk.Advance(30 * time.Second)
	deliverDue(t, h)
	d := onlyDelivery(t, h, env.ID)

	got, err := h.svc.GetDelivery(ctx, m.user, m.tenant, d.id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.Attempts != 2 || string(got.Body) != string(d.body) || got.EventID != env.ID {
		t.Fatalf("delivery %+v", got.Delivery)
	}
	if len(got.AttemptLog) != 2 {
		t.Fatalf("%d attempts", len(got.AttemptLog))
	}
	first, second := got.AttemptLog[0], got.AttemptLog[1]
	if first.Attempt != 1 || first.Outcome != "http_status" || *first.ResponseStatus != 500 || len(first.ResponseBody) == 0 {
		t.Fatalf("first attempt %+v", first)
	}
	if second.Outcome != "succeeded" || *second.ResponseStatus != 200 || second.ResponseHeaders["Content-Type"] == nil {
		t.Fatalf("second attempt %+v", second)
	}
	if first.RequestHeaders[domain.HeaderID] != env.ID.String() || first.RequestHeaders[domain.HeaderSignature] == "" {
		t.Fatalf("request headers %v", first.RequestHeaders)
	}

	other := admin(h, uuid.New())
	_, err = h.svc.GetDelivery(ctx, other.user, other.tenant, d.id)
	wantErr(t, err, app.ErrNotFound)
	_, err = h.svc.GetDelivery(ctx, m.user, m.tenant, uuid.New())
	wantErr(t, err, app.ErrNotFound)
}

func TestSendTestEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	ep := activeEndpoint(t, h, m, h.recv, "wallet.credited")

	d, err := h.svc.SendTestEvent(ctx, m.user, m.tenant, ep.ID)
	if err != nil || d.EventType != domain.TestEventType || d.Status != "pending" || d.EndpointID != ep.ID {
		t.Fatalf("SendTestEvent = %+v, %v", d, err)
	}
	if sum := deliverDue(t, h); sum.Succeeded != 1 {
		t.Fatalf("%+v", sum)
	}
	rec := h.recv.last(t)
	if err := domain.Verify(ep.Secret, rec.header.Get(domain.HeaderTimestamp), rec.header.Get(domain.HeaderSignature), rec.body, h.clk.Now()); err != nil {
		t.Fatalf("the test event doesn't verify: %v", err)
	}
	var body struct {
		Type string `json:"type"`
		Data struct {
			EndpointID uuid.UUID `json:"endpoint_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.body, &body)
	if body.Type != "webhook.test" || body.Data.EndpointID != ep.ID || rec.header.Get(domain.HeaderID) != d.EventID.String() {
		t.Fatalf("test body %s", rec.body)
	}
	if logged, _ := h.svc.GetDelivery(ctx, m.user, m.tenant, d.ID); len(logged.AttemptLog) != 1 {
		t.Fatal("the test event isn't in the delivery log")
	}

	t.Run("refusals", func(t *testing.T) {
		pending := create(t, h, m, h.recv.URL())
		_, err := h.svc.SendTestEvent(ctx, m.user, m.tenant, pending.ID)
		wantErr(t, err, app.ErrValidation)
		v := viewer(h, m.tenant)
		_, err = h.svc.SendTestEvent(ctx, v.user, v.tenant, ep.ID)
		wantErr(t, err, app.ErrPermissionDenied)
		_, err = h.svc.SendTestEvent(ctx, m.user, m.tenant, uuid.New())
		wantErr(t, err, app.ErrNotFound)
	})
}

func TestDisableEndpoint_ByTheClient(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	ep := activeEndpoint(t, h, m, h.recv, "*")
	queued := publish(t, h, "wallet.credited", m.tenant)

	v := viewer(h, m.tenant)
	_, err := h.svc.DisableEndpoint(ctx, v.user, v.tenant, ep.ID)
	wantErr(t, err, app.ErrPermissionDenied)

	h.identity.Expire(m.user, m.session) // no step-up needed to turn one off
	got, err := h.svc.DisableEndpoint(ctx, m.user, m.tenant, ep.ID)
	if err != nil || got.Status != "disabled" || got.DisabledReason != "client" || got.DisabledAt == nil {
		t.Fatalf("DisableEndpoint = %+v, %v", got, err)
	}
	if d := onlyDelivery(t, h, queued.ID); d.status != "dead" || *d.deadReason != "endpoint_disabled" {
		t.Fatalf("queued delivery %+v", d)
	}
	again, err := h.svc.DisableEndpoint(ctx, m.user, m.tenant, ep.ID)
	if err != nil || !again.DisabledAt.Equal(*got.DisabledAt) {
		t.Fatalf("a second disable: %+v, %v", again, err)
	}
	if env := publish(t, h, "wallet.credited", m.tenant); len(deliveriesFor(t, h, env.ID)) != 0 {
		t.Fatal("a disabled endpoint got a new delivery")
	}
	if _, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID); err != nil {
		t.Fatal(err)
	}
	if env := publish(t, h, "wallet.credited", m.tenant); len(deliveriesFor(t, h, env.ID)) != 1 {
		t.Fatal("re-enabled by the challenge, it should receive events again")
	}
	_, err = h.svc.DisableEndpoint(ctx, m.user, m.tenant, uuid.New())
	wantErr(t, err, app.ErrNotFound)
}
