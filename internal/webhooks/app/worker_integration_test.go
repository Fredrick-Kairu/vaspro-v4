//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

func TestDeliverDue_FailsThenSucceedsAtTheConfiguredGap(t *testing.T) {
	h := newHarness(t)
	m := admin(h, uuid.New())
	ep := activeEndpoint(t, h, m, h.recv, "wallet.credited")
	h.recv.then(fail500)
	env := publish(t, h, "wallet.credited", m.tenant)

	if sum := deliverDue(t, h); sum.Claimed != 1 || sum.Failed != 1 {
		t.Fatalf("first run %+v, want one failed", sum)
	}
	d := onlyDelivery(t, h, env.ID)
	if d.status != "pending" || d.attempts != 1 || !d.next.Equal(h.clk.Now().Add(30*time.Second)) {
		t.Fatalf("after a 500: %+v, want pending, due in 30s", d)
	}

	h.clk.Advance(29 * time.Second)
	if sum := deliverDue(t, h); sum.Claimed != 0 {
		t.Fatalf("claimed %d a second early", sum.Claimed)
	}
	h.clk.Advance(time.Second)
	if sum := deliverDue(t, h); sum.Claimed != 1 || sum.Succeeded != 1 {
		t.Fatalf("at 30s %+v, want one success", sum)
	}
	d = onlyDelivery(t, h, env.ID)
	if d.status != "succeeded" || d.attempts != 2 || d.next != nil {
		t.Fatalf("after a 200: %+v", d)
	}
	if sum := deliverDue(t, h); sum.Claimed != 0 {
		t.Fatalf("a succeeded delivery was claimed again: %+v", sum)
	}

	attempts := attemptsOf(t, h, d.id)
	if len(attempts) != 2 || attempts[0].outcome != "http_status" || *attempts[0].status != 500 ||
		attempts[1].outcome != "succeeded" || *attempts[1].status != 200 {
		t.Fatalf("attempts %+v", attempts)
	}
	got := h.recv.requests()
	if len(got) != 2 {
		t.Fatalf("the endpoint got %d requests, want 2", len(got))
	}
	for i, rec := range got {
		if !bytes.Equal(rec.body, d.body) || rec.header.Get(domain.HeaderID) != env.ID.String() {
			t.Fatalf("request %d: id %q, body %s", i, rec.header.Get(domain.HeaderID), rec.body)
		}
		if err := domain.Verify(ep.Secret, rec.header.Get(domain.HeaderTimestamp), rec.header.Get(domain.HeaderSignature), rec.body, h.clk.Now()); err != nil {
			t.Fatalf("request %d doesn't verify with the endpoint's secret: %v", i, err)
		}
		if attempts[i].headers[domain.HeaderSignature] != rec.header.Get(domain.HeaderSignature) {
			t.Fatalf("attempt %d recorded headers %v", i, attempts[i].headers)
		}
	}
	if bytes.Contains([]byte(h.logs.String()), []byte(ep.Secret[len(domain.SecretPrefix):])) {
		t.Fatal("the logs contain the secret")
	}
}

func TestDeliverDue_TheWholeScheduleThenDead(t *testing.T) {
	h := newHarness(t)
	m := admin(h, uuid.New())
	activeEndpoint(t, h, m, h.recv, "*")
	h.recv.set(fail500)
	env := publish(t, h, "wallet.credited", m.tenant)

	for i, wait := range domain.DefaultSchedule {
		if sum := deliverDue(t, h); sum.Claimed != 1 || sum.Failed != 1 {
			t.Fatalf("attempt %d: %+v", i+1, sum)
		}
		h.clk.Advance(wait - time.Second)
		if sum := deliverDue(t, h); sum.Claimed != 0 {
			t.Fatalf("attempt %d's retry was due a second early", i+1)
		}
		h.clk.Advance(time.Second)
	}
	if sum := deliverDue(t, h); sum.Claimed != 1 || sum.Dead != 1 {
		t.Fatalf("the 11th attempt: %+v, want dead", sum)
	}
	d := onlyDelivery(t, h, env.ID)
	if d.status != "dead" || d.deadReason == nil || *d.deadReason != "attempts_exhausted" || d.attempts != 11 {
		t.Fatalf("after the schedule: %+v", d)
	}
	if n := len(h.recv.requests()); n != 11 {
		t.Fatalf("the endpoint got %d requests, want 11", n)
	}
	if n := count(t, h, `SELECT count(*) FROM endpoints WHERE status = 'active'`); n != 1 {
		t.Fatal("about 71h of failure shouldn't disable the endpoint (72h does)")
	}
}

func TestDeliverDue_ASlowEndpointDoesntHoldUpOthers(t *testing.T) {
	// More slow deliveries than workers: without the per-endpoint cap
	// they could take every worker.
	const timeout = time.Second
	h := newHarness(t, pool(8, 2), requestTimeout(timeout))
	m := admin(h, uuid.New())
	slowRecv, fastRecv := newReceiver(t), newReceiver(t)
	activeEndpoint(t, h, m, slowRecv, "tokens.purchased")
	activeEndpoint(t, h, m, fastRecv, "wallet.credited")
	slowRecv.set(slow)

	for range 12 {
		publish(t, h, "tokens.purchased", m.tenant)
	}
	for range 50 {
		publish(t, h, "wallet.credited", m.tenant)
	}
	start := time.Now()
	sum := deliverDue(t, h)
	if sum.Claimed != 62 || sum.Succeeded != 50 || sum.Failed != 12 {
		t.Fatalf("%+v, want 50 succeeded and 12 timed out", sum)
	}
	fast := fastRecv.requests()
	if len(fast) != 50 {
		t.Fatalf("the fast endpoint got %d", len(fast))
	}
	slowFirst := slowRecv.requests()[0].at
	for _, r := range fast {
		if r.at.After(slowFirst.Add(timeout)) {
			t.Fatalf("a fast delivery arrived %v after the start, after the slow endpoint's first timeout", r.at.Sub(start))
		}
	}
	if n := count(t, h, `SELECT count(*) FROM delivery_attempts WHERE outcome = 'timeout'`); n != 12 {
		t.Fatalf("%d timeouts recorded, want 12", n)
	}
}

func TestDeliverDue_TwoWorkersNeverSendOneAttemptTwice(t *testing.T) {
	h := newHarness(t, pool(4, 4))
	m := admin(h, uuid.New())
	for range 3 {
		activeEndpoint(t, h, m, h.recv, "*")
	}
	h.recv.reset()
	for range 10 {
		publish(t, h, "wallet.credited", m.tenant)
	}
	warmPool(t, h.app)

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		mu    sync.Mutex
		total int
	)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sum, err := h.svc.DeliverDue(context.Background())
			if err != nil {
				t.Errorf("DeliverDue: %v", err)
			}
			mu.Lock()
			total += sum.Claimed
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if total != 30 {
		t.Fatalf("the two workers claimed %d between them, want 30", total)
	}
	if n := len(h.recv.requests()); n != 30 {
		t.Fatalf("the endpoints got %d requests, want 30", n)
	}
	if n := count(t, h, `SELECT count(*) FROM delivery_attempts`); n != 30 {
		t.Fatalf("%d attempts recorded, want 30", n)
	}
	if n := count(t, h, `SELECT count(*) FROM deliveries WHERE status = 'succeeded' AND attempts = 1`); n != 30 {
		t.Fatalf("%d deliveries succeeded once, want 30", n)
	}
}

func TestDeliverDue_DisabledAfter72HoursOfFailure(t *testing.T) {
	h := newHarness(t, schedule(72*time.Hour-time.Second, time.Second, time.Hour))
	ctx := context.Background()
	m := admin(h, uuid.New())
	failing := activeEndpoint(t, h, m, h.recv, "wallet.credited")
	otherRecv := newReceiver(t)
	other := activeEndpoint(t, h, m, otherRecv, "webhook_endpoint.disabled")
	h.recv.set(fail500)
	first := publish(t, h, "wallet.credited", m.tenant)
	failingSince := h.clk.Now()
	deliverDue(t, h)

	h.clk.Advance(72*time.Hour - time.Second)
	queued := publish(t, h, "wallet.credited", m.tenant) // will be due later than the first
	if _, err := h.super.Exec(ctx, `UPDATE deliveries SET next_attempt_at = next_attempt_at + interval '1 hour' WHERE event_id = $1`, queued.ID); err != nil {
		t.Fatal(err)
	}
	deliverDue(t, h)
	if got, _ := h.svc.GetEndpoint(ctx, m.user, m.tenant, failing.ID); got.Status != "active" {
		t.Fatalf("a second short of 72h: %q", got.Status)
	}

	h.clk.Advance(time.Second)
	deliverDue(t, h)
	got, err := h.svc.GetEndpoint(ctx, m.user, m.tenant, failing.ID)
	if err != nil || got.Status != "disabled" || got.DisabledReason != "failing" || got.DisabledAt == nil {
		t.Fatalf("at 72h: %+v, %v", got, err)
	}
	for _, id := range []uuid.UUID{first.ID, queued.ID} {
		d := onlyDelivery(t, h, id)
		if d.status != "dead" || *d.deadReason != "endpoint_disabled" {
			t.Fatalf("delivery of %s: %+v, want dead endpoint_disabled", id, d)
		}
	}

	payload, data := assertEventValid(t, h, "webhook_endpoint.disabled")
	if data["endpoint_id"] != failing.ID.String() || data["tenant_id"] != m.tenant.String() || data["dead_deliveries"] != float64(2) ||
		data["failing_since"] != failingSince.Format(time.RFC3339Nano) {
		t.Fatalf("event data %v", data)
	}

	t.Run("the event reaches the client's other endpoint", func(t *testing.T) {
		if err := h.svc.HandleEvent(ctx, payload); err != nil {
			t.Fatal(err)
		}
		var env struct{ ID uuid.UUID }
		_ = json.Unmarshal(payload, &env)
		d := onlyDelivery(t, h, env.ID)
		if d.endpoint != other.ID {
			t.Fatalf("delivered to %s, want the other endpoint", d.endpoint)
		}
		deliverDue(t, h)
		if n := len(otherRecv.requests()); n != 1 {
			t.Fatalf("the other endpoint got %d", n)
		}
	})

	t.Run("passing the challenge again re-enables it", func(t *testing.T) {
		h.recv.set(echo)
		got, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, failing.ID)
		if err != nil || got.Status != "active" || got.FailingSince != nil {
			t.Fatalf("VerifyEndpoint = %+v, %v", got, err)
		}
	})
}

func TestDeliverDue_ASuccessResetsTheFailingClock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	ep := activeEndpoint(t, h, m, h.recv, "*")
	h.recv.then(fail500)
	publish(t, h, "wallet.credited", m.tenant)
	deliverDue(t, h)
	if got, _ := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID); got.FailingSince == nil || !got.FailingSince.Equal(h.clk.Now()) {
		t.Fatalf("after a failure failing_since = %v", got.FailingSince)
	}
	h.clk.Advance(10 * time.Second) // the first delivery's retry is due at 30s
	h.recv.then(fail500)
	publish(t, h, "tokens.purchased", m.tenant)
	if sum := deliverDue(t, h); sum.Claimed != 1 || sum.Failed != 1 {
		t.Fatalf("%+v, want only the new delivery, failing", sum)
	}
	if got, _ := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID); !got.FailingSince.Equal(h.clk.Now().Add(-10 * time.Second)) {
		t.Fatalf("a second failure moved failing_since to %v; it marks the first", got.FailingSince)
	}
	publish(t, h, "usage.posted", m.tenant)
	if sum := deliverDue(t, h); sum.Claimed != 1 || sum.Succeeded != 1 {
		t.Fatalf("%+v, want only the new delivery, succeeding", sum)
	}
	if got, _ := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID); got.FailingSince != nil {
		t.Fatalf("after a success failing_since = %v", got.FailingSince)
	}
}

func TestDeliverDue_EndpointsNoLongerActive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())

	t.Run("deleted: dead and never sent", func(t *testing.T) {
		ep := activeEndpoint(t, h, m, h.recv, "wallet.credited")
		env := publish(t, h, "wallet.credited", m.tenant)
		if err := h.svc.DeleteEndpoint(ctx, m.user, m.tenant, ep.ID); err != nil {
			t.Fatal(err)
		}
		if sum := deliverDue(t, h); sum.Dead != 1 {
			t.Fatalf("%+v", sum)
		}
		d := onlyDelivery(t, h, env.ID)
		if d.status != "dead" || *d.deadReason != "endpoint_deleted" || d.attempts != 0 {
			t.Fatalf("%+v", d)
		}
		if n := len(h.recv.requests()); n != 0 {
			t.Fatalf("sent %d", n)
		}
	})

	t.Run("pending verification after a URL change: held, then sent once verified", func(t *testing.T) {
		ep := activeEndpoint(t, h, m, h.recv, "tokens.purchased")
		env := publish(t, h, "tokens.purchased", m.tenant)
		url := h.recv.srv.URL + "/moved"
		if _, err := h.svc.UpdateEndpoint(ctx, m.user, m.session, m.tenant, ep.ID, app.EndpointChanges{URL: &url}); err != nil {
			t.Fatal(err)
		}
		if sum := deliverDue(t, h); sum.Held != 1 {
			t.Fatalf("%+v, want one held", sum)
		}
		d := onlyDelivery(t, h, env.ID)
		if d.status != "pending" || d.attempts != 0 || !d.next.Equal(h.clk.Now().Add(time.Minute)) {
			t.Fatalf("held delivery %+v", d)
		}
		if n := len(h.recv.requests()); n != 0 {
			t.Fatalf("sent %d to an unverified URL", n)
		}
		if _, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID); err != nil {
			t.Fatal(err)
		}
		h.recv.reset()
		h.clk.Advance(time.Minute)
		if sum := deliverDue(t, h); sum.Succeeded != 1 {
			t.Fatalf("%+v", sum)
		}
		if got := h.recv.last(t); got.header.Get(domain.HeaderID) != env.ID.String() {
			t.Fatalf("sent %q", got.header.Get(domain.HeaderID))
		}
	})
}

func TestDeliverDue_RedirectsAreNotFollowed(t *testing.T) {
	h := newHarness(t)
	m := admin(h, uuid.New())
	activeEndpoint(t, h, m, h.recv, "*")
	h.recv.then(redirect)
	env := publish(t, h, "wallet.credited", m.tenant)
	deliverDue(t, h)
	d := onlyDelivery(t, h, env.ID)
	a := attemptsOf(t, h, d.id)
	if len(a) != 1 || a[0].outcome != "http_status" || *a[0].status != 302 {
		t.Fatalf("attempts %+v", a)
	}
	if n := len(h.recv.requests()); n != 1 {
		t.Fatalf("the endpoint got %d requests; the redirect was followed", n)
	}
}

func TestDeliverDue_SignedWithBothSecretsDuringARotation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	ep := activeEndpoint(t, h, m, h.recv, "*")
	rot, err := h.svc.RotateEndpointSecret(ctx, m.user, m.session, m.tenant, ep.ID)
	newSecret := rot.Secret
	if err != nil {
		t.Fatal(err)
	}
	publish(t, h, "wallet.credited", m.tenant)
	deliverDue(t, h)
	rec := h.recv.last(t)
	sigs := signatures(rec)
	ts := rec.header.Get(domain.HeaderTimestamp)
	if len(sigs) != 2 || domain.Verify(newSecret, ts, sigs[0], rec.body, h.clk.Now()) != nil ||
		domain.Verify(ep.Secret, ts, sigs[1], rec.body, h.clk.Now()) != nil {
		t.Fatalf("signature header %q, want the new secret's then the old one's", rec.header.Get(domain.HeaderSignature))
	}
}

func TestReplayDelivery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	ep := activeEndpoint(t, h, m, h.recv, "*")
	env := publish(t, h, "wallet.credited", m.tenant)
	deliverDue(t, h)
	original := onlyDelivery(t, h, env.ID)

	h.clk.Advance(time.Hour)
	replay, err := h.svc.ReplayDelivery(ctx, m.user, m.tenant, original.id)
	if err != nil {
		t.Fatal(err)
	}
	if replay.ReplayOf == nil || *replay.ReplayOf != original.id || replay.EventID != env.ID || replay.Status != "pending" {
		t.Fatalf("replay %+v", replay)
	}
	if sum := deliverDue(t, h); sum.Succeeded != 1 {
		t.Fatalf("%+v", sum)
	}
	got := h.recv.requests()
	if len(got) != 2 {
		t.Fatalf("%d requests", len(got))
	}
	if got[0].header.Get(domain.HeaderID) != got[1].header.Get(domain.HeaderID) || !bytes.Equal(got[0].body, got[1].body) {
		t.Fatal("a replay must send the same event ID and the same body bytes")
	}
	if got[0].header.Get(domain.HeaderTimestamp) == got[1].header.Get(domain.HeaderTimestamp) {
		t.Fatal("a replay is signed afresh, with a new timestamp")
	}
	if err := domain.Verify(ep.Secret, got[1].header.Get(domain.HeaderTimestamp), got[1].header.Get(domain.HeaderSignature), got[1].body, h.clk.Now()); err != nil {
		t.Fatalf("the replay doesn't verify: %v", err)
	}

	t.Run("refusals", func(t *testing.T) {
		v := viewer(h, m.tenant)
		_, err := h.svc.ReplayDelivery(ctx, v.user, v.tenant, original.id)
		wantErr(t, err, app.ErrPermissionDenied)
		other := admin(h, uuid.New())
		_, err = h.svc.ReplayDelivery(ctx, other.user, other.tenant, original.id)
		wantErr(t, err, app.ErrNotFound)
		_, err = h.svc.ReplayDelivery(ctx, m.user, m.tenant, uuid.New())
		wantErr(t, err, app.ErrNotFound)
		if _, err := h.super.Exec(ctx, `UPDATE endpoints SET status = 'disabled', disabled_at = now(), disabled_reason = 'client' WHERE id = $1`, ep.ID); err != nil {
			t.Fatal(err)
		}
		_, err = h.svc.ReplayDelivery(ctx, m.user, m.tenant, original.id)
		if !errors.Is(err, app.ErrValidation) {
			t.Fatalf("replay to a disabled endpoint: %v, want ErrValidation", err)
		}
		if n := len(deliveriesFor(t, h, env.ID)); n != 2 {
			t.Fatalf("refused replays wrote deliveries: %d", n)
		}
	})
}
