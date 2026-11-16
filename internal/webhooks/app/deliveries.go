package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/store"
)

// Delivery log paging (task 5.5).
const (
	DefaultDeliveryPage = 50
	MaxDeliveryPage     = 100
)

// DeliveryFilter narrows the delivery log; zero values don't filter.
type DeliveryFilter struct {
	EndpointID *uuid.UUID
	Status     string
	EventType  string
	EventID    *uuid.UUID
}

// Attempt is one recorded attempt at a delivery.
type Attempt struct {
	Attempt           int
	StartedAt         time.Time
	DurationMs        int
	RequestHeaders    map[string]string
	Outcome           string
	ResponseStatus    *int
	ResponseHeaders   map[string][]string
	ResponseBody      []byte
	ResponseTruncated bool
	Error             string
}

// DeliveryDetail is a delivery, the body it sends, and its attempts.
type DeliveryDetail struct {
	Delivery
	Body       []byte
	AttemptLog []Attempt // every attempt, oldest first
}

// ListDeliveries is the account's delivery log, newest first
// (credentials:read): limit rows (DefaultDeliveryPage when 0, at most
// MaxDeliveryPage) after cursor, and the cursor for the next page, ""
// at the end.
func (s *Service) ListDeliveries(ctx context.Context, user, tenant uuid.UUID, f DeliveryFilter, cursor string, limit int) ([]Delivery, string, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsRead); err != nil {
		return nil, "", err
	}
	if limit == 0 {
		limit = DefaultDeliveryPage
	}
	if limit < 1 || limit > MaxDeliveryPage {
		return nil, "", fmt.Errorf("%w: limit must be 1 to %d", ErrValidation, MaxDeliveryPage)
	}
	switch f.Status {
	case "", "pending", "succeeded", "dead":
	default:
		return nil, "", fmt.Errorf("%w: status must be pending, succeeded or dead", ErrValidation)
	}
	params := store.ListDeliveriesParams{
		EndpointID: optionalUUID(f.EndpointID), Status: text(f.Status), EventType: text(f.EventType),
		EventID: optionalUUID(f.EventID), MaxRows: int32(limit) + 1, //nolint:gosec // G115: at most MaxDeliveryPage
	}
	if cursor != "" {
		at, id, err := decodeDeliveryCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		params.BeforeCreatedAt, params.BeforeID = timestamptz(at), pgtype.UUID{Bytes: id, Valid: true}
	}
	var rows []store.WebhooksDelivery
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = store.New(tx).ListDeliveries(ctx, params)
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("app: listing deliveries: %w", err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = encodeDeliveryCursor(last.CreatedAt, last.ID)
	}
	out := make([]Delivery, len(rows))
	for i, r := range rows {
		out[i] = toDelivery(r)
	}
	return out, next, nil
}

// encodeDeliveryCursor names the last row of a page: its creation time
// (Unix nanoseconds) and ID.
func encodeDeliveryCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at.UnixNano(), 10) + "|" + id.String()))
}

func decodeDeliveryCursor(c string) (time.Time, uuid.UUID, error) {
	bad := fmt.Errorf("%w: not a cursor this list gave", ErrValidation)
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	nanos, idText, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, bad
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	id, err := uuid.Parse(idText)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	return time.Unix(0, n).UTC(), id, nil
}

// GetDelivery is one delivery with the body it sends and every attempt,
// each with what came back (credentials:read).
func (s *Service) GetDelivery(ctx context.Context, user, tenant, id uuid.UUID) (DeliveryDetail, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsRead); err != nil {
		return DeliveryDetail{}, err
	}
	var out DeliveryDetail
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		d, err := q.GetDelivery(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: delivery %s", ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("app: reading delivery %s: %w", id, err)
		}
		attempts, err := q.ListDeliveryAttempts(ctx, id)
		if err != nil {
			return fmt.Errorf("app: reading attempts: %w", err)
		}
		out = DeliveryDetail{Delivery: toDelivery(d), Body: d.Body, AttemptLog: make([]Attempt, 0, len(attempts))}
		for _, a := range attempts {
			att, err := toAttempt(a)
			if err != nil {
				return err
			}
			out.AttemptLog = append(out.AttemptLog, att)
		}
		return nil
	})
	return out, err
}

func toAttempt(a store.WebhooksDeliveryAttempt) (Attempt, error) {
	out := Attempt{
		Attempt: int(a.Attempt), StartedAt: a.StartedAt, DurationMs: int(a.DurationMs), Outcome: a.Outcome,
		ResponseBody: a.ResponseBody, ResponseTruncated: a.ResponseTruncated, Error: a.Error.String,
	}
	if err := json.Unmarshal(a.RequestHeaders, &out.RequestHeaders); err != nil {
		return Attempt{}, fmt.Errorf("app: reading an attempt's request headers: %w", err)
	}
	if len(a.ResponseHeaders) > 0 {
		if err := json.Unmarshal(a.ResponseHeaders, &out.ResponseHeaders); err != nil {
			return Attempt{}, fmt.Errorf("app: reading an attempt's response headers: %w", err)
		}
	}
	if a.ResponseStatus.Valid {
		v := int(a.ResponseStatus.Int32)
		out.ResponseStatus = &v
	}
	return out, nil
}

// SendTestEvent queues a webhook.test event to an active endpoint
// (credentials:manage). It goes through the ordinary delivery path and
// log, but never the bus.
func (s *Service) SendTestEvent(ctx context.Context, user, tenant, endpoint uuid.UUID) (Delivery, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsManage); err != nil {
		return Delivery{}, err
	}
	id, err := ids.New()
	if err != nil {
		return Delivery{}, fmt.Errorf("app: %w", err)
	}
	eventID, err := ids.New()
	if err != nil {
		return Delivery{}, fmt.Errorf("app: %w", err)
	}
	now := s.clk.Now()
	body, err := domain.TestEventBody(eventID, tenant, endpoint, now)
	if err != nil {
		return Delivery{}, fmt.Errorf("app: %w", err)
	}
	var out Delivery
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		ep, err := q.GetEndpoint(ctx, endpoint)
		if err != nil {
			return notFound(err, endpoint)
		}
		if ep.Status != "active" {
			return fmt.Errorf("%w: a test event can be sent only to an active endpoint; verify it first", ErrValidation)
		}
		d, err := q.InsertDelivery(ctx, store.InsertDeliveryParams{
			ID: id, TenantID: tenant, EndpointID: endpoint, EventID: eventID, EventType: domain.TestEventType,
			OccurredAt: now, Body: body, NextAttemptAt: now,
		})
		if err != nil {
			return fmt.Errorf("app: queueing the test event: %w", err)
		}
		out = toDelivery(d)
		return nil
	})
	if err != nil {
		return Delivery{}, err
	}
	s.log.InfoContext(ctx, "webhooks: test event queued", "delivery_id", id, "endpoint_id", endpoint, "tenant_id", tenant)
	return out, nil
}

// DisableEndpoint turns an endpoint off at the client's request
// (credentials:manage; no step-up, since it only stops sending). Its
// pending deliveries are dead from then on, in the same transaction.
// Passing the challenge (VerifyEndpoint) re-enables it. Disabling a
// disabled endpoint changes nothing.
func (s *Service) DisableEndpoint(ctx context.Context, user, tenant, id uuid.UUID) (Endpoint, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsManage); err != nil {
		return Endpoint{}, err
	}
	var out Endpoint
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetEndpointForUpdate(ctx, id); err != nil {
			return notFound(err, id)
		}
		now := s.clk.Now()
		n, err := q.DisableEndpointByClient(ctx, store.DisableEndpointByClientParams{At: now, ID: id})
		if err != nil {
			return fmt.Errorf("app: disabling the endpoint: %w", err)
		}
		if n > 0 {
			if _, err := q.KillPendingDeliveries(ctx, store.KillPendingDeliveriesParams{
				DeadReason: text("endpoint_disabled"), At: now, EndpointID: id,
			}); err != nil {
				return fmt.Errorf("app: ending the endpoint's deliveries: %w", err)
			}
		}
		out, err = loadEndpoint(ctx, q, id)
		return err
	})
	if err != nil {
		return Endpoint{}, err
	}
	s.log.InfoContext(ctx, "webhooks: endpoint disabled by the client", "endpoint_id", id, "tenant_id", tenant)
	return out, nil
}

func optionalUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}
