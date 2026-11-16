//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/app"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
)

func TestCreateEndpoint_PendingWithItsSecretShownOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())

	ep := create(t, h, m, h.recv.URL(), "wallet.credited", "tokens.purchased", "wallet.credited")
	if ep.Status != "pending_verification" || ep.VerifiedAt != nil || ep.TenantID != m.tenant || ep.CreatedBy != m.user {
		t.Fatalf("created %+v", ep.Endpoint)
	}
	if !domain.ValidSecret(ep.Secret) {
		t.Fatalf("secret %q isn't a whsec_ secret", ep.Secret)
	}
	if !slices.Equal(ep.Events, []string{"tokens.purchased", "wallet.credited"}) || ep.Description != "orders" {
		t.Fatalf("events %v, description %q", ep.Events, ep.Description)
	}
	if n := len(h.recv.requests()); n != 0 {
		t.Fatalf("creating called the endpoint %d times; it should wait for verify", n)
	}

	got, err := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID)
	if err != nil || got.URL != h.recv.URL() || !slices.Equal(got.Events, ep.Events) {
		t.Fatalf("GetEndpoint = %+v, %v", got, err)
	}
	list, err := h.svc.ListEndpoints(ctx, m.user, m.tenant)
	if err != nil || len(list) != 1 || list[0].ID != ep.ID {
		t.Fatalf("ListEndpoints = %+v, %v", list, err)
	}

	t.Run("the secret is sealed at rest and never logged", func(t *testing.T) {
		var ciphertext, fingerprint []byte
		var keyID string
		if err := h.super.QueryRow(ctx, `SELECT ciphertext, fingerprint, kms_key_id FROM endpoint_secrets WHERE endpoint_id = $1`, ep.ID).
			Scan(&ciphertext, &fingerprint, &keyID); err != nil {
			t.Fatal(err)
		}
		raw := ep.Secret[len(domain.SecretPrefix):]
		if bytes.Contains(ciphertext, []byte(ep.Secret)) || bytes.Contains(ciphertext, []byte(raw)) {
			t.Fatal("the ciphertext contains the secret")
		}
		if keyID != app.SecretsKMSKeyID || !bytes.Equal(fingerprint, vascrypto.HMACSign(fingerprintKey, []byte(ep.Secret))) {
			t.Fatalf("key %q, fingerprint doesn't match the secret", keyID)
		}
		if strings.Contains(h.logs.String(), raw) {
			t.Fatal("the logs contain the secret")
		}
	})
}

func TestVerifyEndpoint_EchoActivates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	ep := create(t, h, m, h.recv.URL())

	got, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID)
	if err != nil || got.Status != "active" || got.VerifiedAt == nil || !got.VerifiedAt.Equal(h.clk.Now()) {
		t.Fatalf("VerifyEndpoint = %+v, %v", got, err)
	}

	rec := h.recv.last(t)
	if err := domain.Verify(ep.Secret, rec.header.Get(domain.HeaderTimestamp), rec.header.Get(domain.HeaderSignature), rec.body, h.clk.Now()); err != nil {
		t.Fatalf("the challenge doesn't verify with the endpoint's secret: %v", err)
	}
	var body struct {
		ID       uuid.UUID `json:"id"`
		Type     string    `json:"type"`
		TenantID uuid.UUID `json:"tenant_id"`
		Data     struct {
			Challenge string `json:"challenge"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "webhook.verification" || body.TenantID != m.tenant || len(body.Data.Challenge) < 32 ||
		rec.header.Get(domain.HeaderID) != body.ID.String() {
		t.Fatalf("challenge body %+v, id header %q", body, rec.header.Get(domain.HeaderID))
	}
	if strings.Contains(h.logs.String(), body.Data.Challenge) {
		t.Fatal("the logs contain the challenge")
	}

	t.Run("checking an active endpoint again keeps it active", func(t *testing.T) {
		h.clk.Advance(time.Minute)
		again, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID)
		if err != nil || again.Status != "active" || !again.VerifiedAt.Equal(h.clk.Now()) {
			t.Fatalf("VerifyEndpoint = %+v, %v", again, err)
		}
		h.recv.set(wrong)
		_, err = h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID)
		wantErr(t, err, app.ErrVerificationFailed)
		if got, _ := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID); got.Status != "active" {
			t.Fatalf("a failed re-check left it %q; deliveries decide disabling, not the check", got.Status)
		}
	})
}

func TestVerifyEndpoint_FailuresLeaveItPending(t *testing.T) {
	cases := []struct {
		mode    string
		outcome adapters.Outcome
	}{
		{wrong, ""},
		{notJSON, ""},
		{fail500, adapters.OutcomeHTTPStatus},
		{redirect, adapters.OutcomeHTTPStatus},
		{slow, adapters.OutcomeTimeout},
	}
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			ep := create(t, h, m, h.recv.URL())
			h.recv.set(c.mode)
			_, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID)
			var vf *app.VerificationFailure
			if !errors.As(err, &vf) || !errors.Is(err, app.ErrVerificationFailed) {
				t.Fatalf("VerifyEndpoint = %v, want a VerificationFailure", err)
			}
			if vf.Outcome != c.outcome {
				t.Fatalf("outcome %q, want %q", vf.Outcome, c.outcome)
			}
			if strings.Contains(err.Error(), h.recv.URL()) {
				t.Fatalf("the error %q names the URL", err)
			}
			got, _ := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID)
			if got.Status != "pending_verification" || got.VerifiedAt != nil {
				t.Fatalf("after %s: %q", c.mode, got.Status)
			}
		})
	}
	if n := count(t, h, `SELECT count(*) FROM endpoints WHERE status = 'active'`); n != 0 {
		t.Fatalf("%d endpoints active", n)
	}
	if n := count(t, h, `SELECT count(*) FROM delivery_attempts`); n != 0 {
		t.Fatalf("a challenge isn't a delivery, yet %d attempts were written", n)
	}
}

func TestVerifyEndpoint_URLChangedMeanwhileIsNotVerified(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	ep := create(t, h, m, h.recv.URL())
	other := h.recv.srv.URL + "/moved"
	h.recv.beforeAnswer(func() {
		if _, err := h.svc.UpdateEndpoint(ctx, m.user, m.session, m.tenant, ep.ID, app.EndpointChanges{URL: &other}); err != nil {
			t.Errorf("UpdateEndpoint: %v", err)
		}
	})
	_, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID)
	wantErr(t, err, app.ErrVerificationFailed)
	got, _ := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID)
	if got.URL != other || got.Status != "pending_verification" {
		t.Fatalf("after a URL change mid-challenge: %q at %q", got.Status, got.URL)
	}
}

func TestEndpoints_RefusedURLs(t *testing.T) {
	h := newHarness(t, secure())
	ctx := context.Background()
	m := admin(h, uuid.New())
	for name, url := range map[string]string{
		"plain http":        "http://hooks.example.com/in",
		"user information":  "https://u:p@hooks.example.com/in",
		"loopback":          "https://127.0.0.1/in",
		"cloud metadata":    "https://169.254.169.254/latest/meta-data",
		"a private address": "https://10.0.0.5/in",
		"IPv6 loopback":     "https://[::1]/in",
		"too long":          "https://hooks.example.com/" + strings.Repeat("a", 2048),
		"not a URL":         "hooks",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.CreateEndpoint(ctx, m.user, m.session, m.tenant, app.NewEndpoint{URL: url, Events: []string{"*"}})
			wantErr(t, err, app.ErrURLNotAllowed)
		})
	}
	if n := count(t, h, `SELECT count(*) FROM endpoints`); n != 0 {
		t.Fatalf("%d endpoints written for refused URLs", n)
	}

	t.Run("a name resolving to loopback is refused when connecting", func(t *testing.T) {
		port := h.recv.srv.URL[strings.LastIndex(h.recv.srv.URL, ":"):]
		ep := create(t, h, m, "https://localhost"+port+"/in")
		_, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID)
		wantErr(t, err, app.ErrURLNotAllowed)
		if n := len(h.recv.requests()); n != 0 {
			t.Fatalf("the endpoint was reached %d times", n)
		}
		got, _ := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID)
		if got.Status != "pending_verification" {
			t.Fatalf("status %q", got.Status)
		}
	})

	t.Run("a URL change is checked too", func(t *testing.T) {
		ep := create(t, h, m, "https://hooks.example.com/in")
		bad := "https://192.168.1.1/in"
		_, err := h.svc.UpdateEndpoint(ctx, m.user, m.session, m.tenant, ep.ID, app.EndpointChanges{URL: &bad})
		wantErr(t, err, app.ErrURLNotAllowed)
	})
}

func TestRotateEndpointSecret(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	ep := create(t, h, m, h.recv.URL())
	if _, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID); err != nil {
		t.Fatal(err)
	}

	rot, err := h.svc.RotateEndpointSecret(ctx, m.user, m.session, m.tenant, ep.ID)
	newSecret := rot.Secret
	if err != nil || !domain.ValidSecret(newSecret) || newSecret == ep.Secret || !rot.PreviousValidUntil.Equal(h.clk.Now().Add(domain.SecretOverlap)) {
		t.Fatalf("RotateEndpointSecret = %+v, %v", rot.PreviousValidUntil, err)
	}
	check := func(t *testing.T) received {
		t.Helper()
		if _, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID); err != nil {
			t.Fatal(err)
		}
		return h.recv.last(t)
	}
	verifies := func(rec received, secret, sig string) bool {
		return domain.Verify(secret, rec.header.Get(domain.HeaderTimestamp), sig, rec.body, h.clk.Now()) == nil
	}

	t.Run("during the overlap both sign, the new one first", func(t *testing.T) {
		rec := check(t)
		sigs := signatures(rec)
		if len(sigs) != 2 {
			t.Fatalf("signature header %q, want two values", rec.header.Get(domain.HeaderSignature))
		}
		if !verifies(rec, newSecret, sigs[0]) || verifies(rec, ep.Secret, sigs[0]) || !verifies(rec, ep.Secret, sigs[1]) {
			t.Fatal("want the new secret's signature first and the old one's second")
		}
	})

	t.Run("a rotation inside the overlap ends the oldest at once", func(t *testing.T) {
		h.clk.Advance(time.Hour)
		rot3, err := h.svc.RotateEndpointSecret(ctx, m.user, m.session, m.tenant, ep.ID)
		third := rot3.Secret
		if err != nil {
			t.Fatal(err)
		}
		rec := check(t)
		sigs := signatures(rec)
		if len(sigs) != 2 || !verifies(rec, third, sigs[0]) || !verifies(rec, newSecret, sigs[1]) {
			t.Fatalf("after a second rotation: %d values, want the third and second secrets", len(sigs))
		}
		if verifies(rec, ep.Secret, rec.header.Get(domain.HeaderSignature)) {
			t.Fatal("the first secret still verifies")
		}
		newSecret = third
	})

	t.Run("after 24 hours only the new one signs", func(t *testing.T) {
		h.clk.Advance(domain.SecretOverlap + time.Second)
		rec := check(t)
		sigs := signatures(rec)
		if len(sigs) != 1 || !verifies(rec, newSecret, sigs[0]) {
			t.Fatalf("signature header %q, want the current secret's alone", rec.header.Get(domain.HeaderSignature))
		}
	})

	if strings.Contains(h.logs.String(), newSecret[len(domain.SecretPrefix):]) {
		t.Fatal("the logs contain a rotated secret")
	}
}

func TestUpdateEndpoint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	ep := create(t, h, m, h.recv.URL())
	if _, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID); err != nil {
		t.Fatal(err)
	}

	t.Run("subscriptions are replaced, and the description changed, with no step-up", func(t *testing.T) {
		h.identity.Expire(m.user, m.session)
		defer h.identity.StepUp(m.user, m.session)
		desc := ""
		got, err := h.svc.UpdateEndpoint(ctx, m.user, m.session, m.tenant, ep.ID,
			app.EndpointChanges{Description: &desc, Events: []string{"payment_intent.succeeded", "payment_intent.failed"}})
		if err != nil || got.Description != "" || got.Status != "active" ||
			!slices.Equal(got.Events, []string{"payment_intent.failed", "payment_intent.succeeded"}) {
			t.Fatalf("UpdateEndpoint = %+v, %v", got, err)
		}
	})
	t.Run("an internal event type is refused, changing nothing", func(t *testing.T) {
		_, err := h.svc.UpdateEndpoint(ctx, m.user, m.session, m.tenant, ep.ID, app.EndpointChanges{Events: []string{"ledger.rebuild_failed"}})
		wantErr(t, err, app.ErrValidation)
		got, _ := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID)
		if len(got.Events) != 2 {
			t.Fatalf("events %v", got.Events)
		}
	})
	t.Run("a new URL needs the challenge again", func(t *testing.T) {
		url := h.recv.srv.URL + "/v2"
		got, err := h.svc.UpdateEndpoint(ctx, m.user, m.session, m.tenant, ep.ID, app.EndpointChanges{URL: &url})
		if err != nil || got.URL != url || got.Status != "pending_verification" || got.VerifiedAt != nil {
			t.Fatalf("UpdateEndpoint = %+v, %v", got, err)
		}
	})
	t.Run("a disabled endpoint is re-enabled by passing the challenge", func(t *testing.T) {
		if _, err := h.super.Exec(ctx, `UPDATE endpoints SET status = 'disabled', disabled_at = now(), disabled_reason = 'failing', failing_since = now() WHERE id = $1`, ep.ID); err != nil {
			t.Fatal(err)
		}
		got, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID)
		if err != nil || got.Status != "active" || got.DisabledAt != nil || got.DisabledReason != "" || got.FailingSince != nil {
			t.Fatalf("VerifyEndpoint = %+v, %v", got, err)
		}
	})
}

func TestDeleteEndpoint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	ep := create(t, h, m, h.recv.URL())
	if err := h.svc.DeleteEndpoint(ctx, m.user, m.tenant, ep.ID); err != nil {
		t.Fatal(err)
	}
	_, err := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID)
	wantErr(t, err, app.ErrNotFound)
	wantErr(t, h.svc.DeleteEndpoint(ctx, m.user, m.tenant, ep.ID), app.ErrNotFound)
	if list, _ := h.svc.ListEndpoints(ctx, m.user, m.tenant); len(list) != 0 {
		t.Fatalf("listed %d after deleting", len(list))
	}
	if n := count(t, h, `SELECT count(*) FROM endpoints WHERE id = $1 AND deleted_at IS NOT NULL`, ep.ID); n != 1 {
		t.Fatal("the row should stay, marked deleted, for the delivery log")
	}
}

func TestEndpoints_WhoMayDoWhat(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tenant := uuid.New()
	owner := admin(h, tenant)
	ep := create(t, h, owner, h.recv.URL())
	url := h.recv.srv.URL + "/other"
	desc := "x"
	before := count(t, h, `SELECT count(*) FROM endpoints`) + count(t, h, `SELECT count(*) FROM endpoint_secrets`) +
		count(t, h, `SELECT count(*) FROM subscriptions`)
	requests := len(h.recv.requests())

	ops := func(m member) map[string]func() error {
		return map[string]func() error{
			"create": func() error {
				_, err := h.svc.CreateEndpoint(ctx, m.user, m.session, m.tenant, app.NewEndpoint{URL: h.recv.URL(), Events: []string{"*"}})
				return err
			},
			"verify": func() error { _, err := h.svc.VerifyEndpoint(ctx, m.user, m.tenant, ep.ID); return err },
			"change the URL": func() error {
				_, err := h.svc.UpdateEndpoint(ctx, m.user, m.session, m.tenant, ep.ID, app.EndpointChanges{URL: &url})
				return err
			},
			"change the description": func() error {
				_, err := h.svc.UpdateEndpoint(ctx, m.user, m.session, m.tenant, ep.ID, app.EndpointChanges{Description: &desc})
				return err
			},
			"rotate": func() error {
				_, err := h.svc.RotateEndpointSecret(ctx, m.user, m.session, m.tenant, ep.ID)
				return err
			},
			"delete": func() error { return h.svc.DeleteEndpoint(ctx, m.user, m.tenant, ep.ID) },
		}
	}
	reads := func(m member) map[string]func() error {
		return map[string]func() error{
			"get":  func() error { _, err := h.svc.GetEndpoint(ctx, m.user, m.tenant, ep.ID); return err },
			"list": func() error { _, err := h.svc.ListEndpoints(ctx, m.user, m.tenant); return err },
		}
	}

	t.Run("a Viewer reads but changes nothing", func(t *testing.T) {
		v := viewer(h, tenant)
		for name, read := range reads(v) {
			if err := read(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		for name, op := range ops(v) {
			if err := op(); !errors.Is(err, app.ErrPermissionDenied) {
				t.Errorf("%s: %v, want ErrPermissionDenied", name, err)
			}
		}
	})
	t.Run("a stranger finds nothing", func(t *testing.T) {
		s := member{user: uuid.New(), session: uuid.New(), tenant: tenant}
		h.identity.StepUp(s.user, s.session)
		for name, op := range mergeOps(ops(s), reads(s)) {
			if err := op(); !errors.Is(err, app.ErrNotFound) {
				t.Errorf("%s: %v, want ErrNotFound", name, err)
			}
		}
	})
	t.Run("a stale step-up can't create, change the URL or rotate", func(t *testing.T) {
		stale := admin(h, tenant)
		h.identity.Expire(stale.user, stale.session)
		all := ops(stale)
		for _, name := range []string{"create", "change the URL", "rotate"} {
			if err := all[name](); !errors.Is(err, app.ErrStepUpRequired) {
				t.Errorf("%s: %v, want ErrStepUpRequired", name, err)
			}
		}
	})
	t.Run("another account's endpoint isn't found from one's own", func(t *testing.T) {
		other := admin(h, uuid.New())
		for name, op := range mergeOps(ops(other), reads(other)) {
			if name == "create" || name == "list" {
				continue
			}
			if err := op(); !errors.Is(err, app.ErrNotFound) {
				t.Errorf("%s: %v, want ErrNotFound", name, err)
			}
		}
	})

	after := count(t, h, `SELECT count(*) FROM endpoints`) + count(t, h, `SELECT count(*) FROM endpoint_secrets`) +
		count(t, h, `SELECT count(*) FROM subscriptions`)
	if after != before {
		t.Fatalf("refused calls wrote %d rows", after-before)
	}
	if got, _ := h.svc.GetEndpoint(ctx, owner.user, tenant, ep.ID); got.URL != h.recv.URL() || got.Description != "orders" {
		t.Fatalf("refused calls changed the endpoint: %+v", got)
	}
	if n := len(h.recv.requests()); n != requests {
		t.Fatalf("refused calls reached the endpoint %d times", n-requests)
	}

	t.Run("accounts or identity down fails closed", func(t *testing.T) {
		h.identity.Fail()
		_, err := h.svc.CreateEndpoint(ctx, owner.user, owner.session, tenant, app.NewEndpoint{URL: h.recv.URL(), Events: []string{"*"}})
		if err == nil || errors.Is(err, app.ErrStepUpRequired) {
			t.Fatalf("create with identity down: %v, want a failure that isn't a plain refusal", err)
		}
		h.accounts.Fail()
		if _, err := h.svc.ListEndpoints(ctx, owner.user, tenant); err == nil {
			t.Fatal("list passed with accounts down")
		}
	})
}

func mergeOps(a, b map[string]func() error) map[string]func() error {
	out := map[string]func() error{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func TestCreateEndpoint_Refusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := admin(h, uuid.New())
	for name, in := range map[string]app.NewEndpoint{
		"no events":              {URL: h.recv.URL()},
		"an internal event":      {URL: h.recv.URL(), Events: []string{"recon.exception_opened"}},
		"a description over 255": {URL: h.recv.URL(), Events: []string{"*"}, Description: strings.Repeat("d", 256)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.CreateEndpoint(ctx, m.user, m.session, m.tenant, in)
			wantErr(t, err, app.ErrValidation)
		})
	}
	if n := count(t, h, `SELECT count(*) FROM endpoints`); n != 0 {
		t.Fatalf("%d endpoints written", n)
	}
}

func TestCreateEndpoint_Cap(t *testing.T) {
	h := newHarness(t, maxEndpoints(3))
	ctx := context.Background()
	m := admin(h, uuid.New())
	var eps []app.CreatedEndpoint
	for range 3 {
		eps = append(eps, create(t, h, m, h.recv.URL()))
	}
	_, err := h.svc.CreateEndpoint(ctx, m.user, m.session, m.tenant, app.NewEndpoint{URL: h.recv.URL(), Events: []string{"*"}})
	wantErr(t, err, app.ErrValidation)

	other := admin(h, uuid.New())
	create(t, h, other, h.recv.URL()) // another client has its own cap

	if err := h.svc.DeleteEndpoint(ctx, m.user, m.tenant, eps[0].ID); err != nil {
		t.Fatal(err)
	}
	create(t, h, m, h.recv.URL())
}

func TestCreateEndpoint_CapHoldsUnderConcurrency(t *testing.T) {
	h := newHarness(t, maxEndpoints(16))
	ctx := context.Background()
	m := admin(h, uuid.New())
	warmPool(t, h.app)

	const n = 20
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		created int
		refused int
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := h.svc.CreateEndpoint(ctx, m.user, m.session, m.tenant, app.NewEndpoint{URL: h.recv.URL(), Events: []string{"*"}})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, app.ErrValidation):
				refused++
			default:
				t.Errorf("CreateEndpoint: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if created != 16 || refused != 4 {
		t.Fatalf("created %d and refused %d, want 16 and 4", created, refused)
	}
	if got := count(t, h, `SELECT count(*) FROM endpoints WHERE tenant_id = $1`, m.tenant); got != 16 {
		t.Fatalf("%d endpoints stored", got)
	}
}
