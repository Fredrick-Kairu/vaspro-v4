//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/store"
)

// TestSqlc_RoundTripUnderRLS: the generated queries work as the service
// will use them, in the client's transaction for endpoints and fan-out
// and as webhooks_system for the worker; a redelivered event is
// pgx.ErrNoRows; a replay keeps the event and body.
func TestSqlc_RoundTripUnderRLS(t *testing.T) {
	ctx := context.Background()
	db := newWebhooksDB(t)
	tenant := uuid.New()
	at := now

	var endpointID uuid.UUID
	inTenant := func(name string, fn func(q *store.Queries) error) {
		t.Helper()
		if err := asTenant(ctx, db.app, tenant, func(tx pgx.Tx) error { return fn(store.New(tx)) }); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	inTenant("endpoint, secrets and subscription", func(q *store.Queries) error {
		e, err := q.CreateEndpoint(ctx, store.CreateEndpointParams{
			ID: uuid.New(), TenantID: tenant, Url: "https://hooks.example.com/vaspro",
			Description: pgtype.Text{String: "orders", Valid: true}, CreatedBy: uuid.New(),
		})
		if err != nil {
			return err
		}
		endpointID = e.ID
		if e.Status != "pending_verification" {
			t.Errorf("a new endpoint is %q", e.Status)
		}
		for _, ct := range [][]byte{{1}, {2}} {
			if _, err := q.CreateEndpointSecret(ctx, store.CreateEndpointSecretParams{
				ID: uuid.New(), TenantID: tenant, EndpointID: e.ID, Ciphertext: ct,
				KmsKeyID: "webhooks-endpoint-secrets", Fingerprint: []byte{9}, CreatedBy: uuid.New(),
			}); err != nil {
				return err
			}
			if ct[0] == 1 {
				// Rotated: the first stays valid for its overlap.
				if n, err := q.ExpireCurrentEndpointSecret(ctx, store.ExpireCurrentEndpointSecretParams{EndpointID: e.ID, ExpiresAt: at.Add(24 * time.Hour)}); err != nil || n != 1 {
					t.Fatalf("rotation expired %d secrets: %v", n, err)
				}
			}
		}
		secrets, err := q.ListValidEndpointSecrets(ctx, store.ListValidEndpointSecretsParams{EndpointID: e.ID, At: at})
		if err != nil {
			return err
		}
		if len(secrets) != 2 || secrets[0].Ciphertext[0] != 2 {
			t.Errorf("valid secrets during rotation: %d, current first = %v", len(secrets), len(secrets) > 0 && secrets[0].Ciphertext[0] == 2)
		}
		after, err := q.ListValidEndpointSecrets(ctx, store.ListValidEndpointSecretsParams{EndpointID: e.ID, At: at.Add(25 * time.Hour)})
		if err != nil {
			return err
		}
		if len(after) != 1 {
			t.Errorf("valid secrets after the overlap: %d, want 1", len(after))
		}
		for range 2 {
			if err := q.AddSubscription(ctx, store.AddSubscriptionParams{TenantID: tenant, EndpointID: e.ID, EventType: "wallet.credited"}); err != nil {
				return err
			}
		}
		subs, err := q.ListSubscriptions(ctx, e.ID)
		if err != nil {
			return err
		}
		if len(subs) != 1 {
			t.Errorf("subscribing twice made %d subscriptions", len(subs))
		}
		return nil
	})

	inTenant("fan-out waits for verification", func(q *store.Queries) error {
		for _, step := range []string{"pending", "verified"} {
			if step == "verified" {
				n, err := q.MarkEndpointVerified(ctx, store.MarkEndpointVerifiedParams{ID: endpointID, At: at, Version: 1})
				if err != nil || n != 1 {
					t.Fatalf("verify: %d rows, %v", n, err)
				}
			}
			want := map[string]int{"pending": 0, "verified": 1}[step]
			for eventType, w := range map[string]int{"wallet.credited": want, "tokens.purchased": 0} {
				es, err := q.ListEndpointsForEvent(ctx, eventType)
				if err != nil {
					return err
				}
				if len(es) != w {
					t.Errorf("%s, %s: %d endpoints, want %d", step, eventType, len(es), w)
				}
			}
		}
		return nil
	})

	eventID := uuid.New()
	var first store.WebhooksDelivery
	inTenant("a delivery once, then a replay", func(q *store.Queries) error {
		params := store.InsertDeliveryParams{
			ID: uuid.New(), TenantID: tenant, EndpointID: endpointID, EventID: eventID,
			EventType: "wallet.credited", OccurredAt: at, Body: []byte(`{"id":"evt"}`), NextAttemptAt: at,
		}
		var err error
		if first, err = q.InsertDelivery(ctx, params); err != nil {
			return err
		}
		params.ID = uuid.New()
		if _, err := q.InsertDelivery(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("a redelivered event: %v, want pgx.ErrNoRows", err)
		}
		replay, err := q.CreateReplay(ctx, store.CreateReplayParams{ID: uuid.New(), NextAttemptAt: at, ReplayedBy: uuid.New(), ReplayOf: first.ID})
		if err != nil {
			return err
		}
		if replay.EventID != eventID || string(replay.Body) != string(first.Body) || uuid.UUID(replay.ReplayOf.Bytes) != first.ID {
			t.Errorf("the replay changed the event or body: %+v", replay)
		}
		return nil
	})

	t.Run("the worker claims, skipping held rows, and records attempts", func(t *testing.T) {
		for range 8 {
			seedDelivery(t, db, endpointID, tenant)
		}
		lease := at.Add(time.Minute)
		claim := func(tx pgx.Tx, max int32) []store.WebhooksDelivery {
			t.Helper()
			ds, err := store.New(tx).ClaimDueDeliveries(ctx, store.ClaimDueDeliveriesParams{LeaseUntil: lease, Now: at, MaxRows: max})
			if err != nil {
				t.Fatal(err)
			}
			return ds
		}

		// Two workers at once: the first holds its rows uncommitted, so
		// the second claims only the others.
		tx1, err := db.app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx1.Rollback(ctx) }()
		if _, err := tx1.Exec(ctx, `SET LOCAL ROLE webhooks_system`); err != nil {
			t.Fatal(err)
		}
		held := claim(tx1, 4)
		var other []store.WebhooksDelivery
		if err := asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error { other = claim(tx, 100); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(held) != 4 || len(other) != 6 {
			t.Fatalf("claimed %d and %d of 10, want 4 and 6", len(held), len(other))
		}
		seen := map[uuid.UUID]bool{}
		for _, d := range append(held, other...) {
			if seen[d.ID] {
				t.Fatalf("delivery %s claimed twice", d.ID)
			}
			seen[d.ID] = true
		}
		if err := tx1.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		if err := asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
			if n := len(claim(tx, 100)); n != 0 {
				t.Errorf("leased deliveries claimed again: %d", n)
			}
			q := store.New(tx)
			if _, err := q.CreateDeliveryAttempt(ctx, store.CreateDeliveryAttemptParams{
				ID: uuid.New(), TenantID: tenant, DeliveryID: first.ID, Attempt: 1, StartedAt: at, DurationMs: 10_000,
				RequestHeaders: []byte(`{"Vaspro-Webhook-Id":"evt"}`), Outcome: "timeout",
				Error: pgtype.Text{String: "no response within 10s", Valid: true},
			}); err != nil {
				return err
			}
			_, err := q.CreateDeliveryAttempt(ctx, store.CreateDeliveryAttemptParams{
				ID: uuid.New(), TenantID: tenant, DeliveryID: first.ID, Attempt: 2, StartedAt: at, DurationMs: 40,
				RequestHeaders: []byte(`{}`), Outcome: "succeeded", ResponseStatus: pgtype.Int4{Int32: 200, Valid: true},
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// Once the lease runs out the rows are due again.
		if err := asRole(ctx, db.app, systemRole, func(tx pgx.Tx) error {
			ds, err := store.New(tx).ClaimDueDeliveries(ctx, store.ClaimDueDeliveriesParams{LeaseUntil: lease.Add(time.Minute), Now: lease, MaxRows: 100})
			if len(ds) != 10 {
				t.Errorf("after the lease, %d claimed, want 10", len(ds))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		inTenant("the client reads its attempts", func(q *store.Queries) error {
			as, err := q.ListDeliveryAttempts(ctx, first.ID)
			if len(as) != 2 || as[0].Outcome != "timeout" || as[1].Outcome != "succeeded" {
				t.Errorf("attempts: %+v", as)
			}
			return err
		})
	})
}
