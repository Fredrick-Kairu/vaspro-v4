package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/store"
)

// Delivery is one event's delivery to one endpoint, as a client sees it.
type Delivery struct {
	ID            uuid.UUID
	EndpointID    uuid.UUID
	EventID       uuid.UUID
	EventType     string
	OccurredAt    time.Time
	Status        string
	DeadReason    string
	Attempts      int
	NextAttemptAt *time.Time
	ReplayOf      *uuid.UUID
	CreatedAt     time.Time
	CompletedAt   *time.Time
}

// ReplayDelivery sends a delivery again (credentials:manage; data model
// §8): a new delivery, due now, with the same event ID and the same body
// bytes, signed afresh when it goes. Its endpoint must be active.
func (s *Service) ReplayDelivery(ctx context.Context, user, tenant, id uuid.UUID) (Delivery, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsManage); err != nil {
		return Delivery{}, err
	}
	newID, err := ids.New()
	if err != nil {
		return Delivery{}, fmt.Errorf("app: %w", err)
	}
	var out Delivery
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		d, err := q.GetDelivery(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: delivery %s", ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("app: reading delivery %s: %w", id, err)
		}
		ep, err := q.GetEndpoint(ctx, d.EndpointID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("app: reading the delivery's endpoint: %w", err)
		}
		if err != nil || ep.Status != "active" {
			return fmt.Errorf("%w: a delivery can be replayed only to an active endpoint", ErrValidation)
		}
		r, err := q.CreateReplay(ctx, store.CreateReplayParams{ID: newID, NextAttemptAt: s.clk.Now(), ReplayedBy: user, ReplayOf: id})
		if err != nil {
			return fmt.Errorf("app: queueing the replay: %w", err)
		}
		out = toDelivery(r)
		return nil
	})
	if err != nil {
		return Delivery{}, err
	}
	s.log.InfoContext(ctx, "webhooks: delivery replayed", "delivery_id", out.ID, "replay_of", id, "tenant_id", tenant)
	return out, nil
}

func toDelivery(d store.WebhooksDelivery) Delivery {
	out := Delivery{
		ID: d.ID, EndpointID: d.EndpointID, EventID: d.EventID, EventType: d.EventType, OccurredAt: d.OccurredAt, Status: d.Status,
		DeadReason: d.DeadReason.String, Attempts: int(d.Attempts), NextAttemptAt: timePtr(d.NextAttemptAt),
		CreatedAt: d.CreatedAt, CompletedAt: timePtr(d.CompletedAt),
	}
	if d.ReplayOf.Valid {
		v := uuid.UUID(d.ReplayOf.Bytes)
		out.ReplayOf = &v
	}
	return out
}
