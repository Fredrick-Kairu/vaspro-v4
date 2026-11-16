package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/inbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/store"
)

// HandleEvent fans one bus event out to its client's endpoints (task
// 5.4, data model §8): one delivery, due now, for each active endpoint
// subscribed to its type or to every type, written in the same
// transaction that records the event as processed, so a redelivered
// event makes no second delivery. An event that isn't for a client
// (internal, no tenant, malformed) is acknowledged with nothing written.
// An error means the bus should deliver the event again.
func (s *Service) HandleEvent(ctx context.Context, raw []byte) error {
	var env events.Envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.ID == uuid.Nil {
		s.log.WarnContext(ctx, "webhooks: ignoring a malformed event")
		return nil
	}
	tenant, ok := domain.Recipient(env)
	if !ok {
		if domain.IsClientEventType(env.Type) {
			s.log.WarnContext(ctx, "webhooks: a client event that doesn't name its client consistently isn't sent",
				"event_id", env.ID, "event_type", env.Type)
		}
		return nil
	}
	body, err := domain.DeliveryBody(env)
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}

	created := 0
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		already, err := inbox.MarkProcessed(ctx, tx, env.ID)
		if err != nil || already {
			return err
		}
		q := store.New(tx)
		endpoints, err := q.ListEndpointsForEvent(ctx, env.Type)
		if err != nil {
			return fmt.Errorf("app: finding endpoints for %s: %w", env.Type, err)
		}
		now := s.clk.Now()
		for _, ep := range endpoints {
			id, err := ids.New()
			if err != nil {
				return fmt.Errorf("app: %w", err)
			}
			_, err = q.InsertDelivery(ctx, store.InsertDeliveryParams{
				ID: id, TenantID: tenant, EndpointID: ep.ID, EventID: env.ID, EventType: env.Type,
				OccurredAt: env.OccurredAt, Body: body, NextAttemptAt: now,
			})
			switch {
			case errors.Is(err, pgx.ErrNoRows): // already queued for this endpoint
			case err != nil:
				return fmt.Errorf("app: queueing a delivery: %w", err)
			default:
				created++
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if created > 0 {
		s.log.DebugContext(ctx, "webhooks: event queued", "event_id", env.ID, "event_type", env.Type, "tenant_id", tenant, "deliveries", created)
	}
	return nil
}
