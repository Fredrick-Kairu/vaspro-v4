package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/outbox"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/store"
)

// Summary counts what one DeliverDue did.
type Summary struct {
	Claimed   int
	Succeeded int
	Failed    int // attempts that failed and will be retried
	Dead      int // deliveries that ended without success
	Held      int // put off, the endpoint waiting for verification
}

// errLostClaim: another worker recorded this attempt first (its lease
// ran out while this one was sending). Nothing of this attempt is kept.
var errLostClaim = errors.New("app: the delivery was recorded by another worker")

// DeliverDue sends the deliveries that are due (task 5.4): it claims up
// to BatchSize of them across clients, leasing them so no other worker
// takes them, and sends them on at most Workers goroutines, no more than
// PerEndpoint at once to any endpoint, so a slow endpoint holds up only
// its own deliveries. It returns once the batch is done. Every replica
// may run it.
func (s *Service) DeliverDue(ctx context.Context) (Summary, error) {
	if s.sender == nil {
		return Summary{}, errors.New("app: no sender for endpoints")
	}
	now := s.clk.Now()
	var claimed []store.WebhooksDelivery
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		claimed, err = store.New(tx).ClaimDueDeliveries(ctx, store.ClaimDueDeliveriesParams{
			LeaseUntil: now.Add(s.lease), Now: now, MaxRows: int32(s.batch), //nolint:gosec // G115: BatchSize is small configuration
		})
		return err
	})
	if err != nil {
		return Summary{}, fmt.Errorf("app: claiming due deliveries: %w", err)
	}

	var (
		mu  sync.Mutex
		sum = Summary{Claimed: len(claimed)}
		wg  sync.WaitGroup
	)
	global := make(chan struct{}, s.workers)
	perEndpoint := map[uuid.UUID]chan struct{}{}
	for _, d := range claimed {
		slots, ok := perEndpoint[d.EndpointID]
		if !ok {
			slots = make(chan struct{}, s.perEndpoint)
			perEndpoint[d.EndpointID] = slots
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The endpoint's slot first, so waiting for it never holds a
			// worker another endpoint could use.
			slots <- struct{}{}
			defer func() { <-slots }()
			global <- struct{}{}
			defer func() { <-global }()
			result, err := s.deliver(ctx, d)
			if err != nil && !errors.Is(err, errLostClaim) {
				s.log.ErrorContext(ctx, "webhooks: delivery failed to record", "delivery_id", d.ID, "tenant_id", d.TenantID, "error", err)
			}
			mu.Lock()
			defer mu.Unlock()
			switch result {
			case resultSucceeded:
				sum.Succeeded++
			case resultFailed:
				sum.Failed++
			case resultDead:
				sum.Dead++
			case resultHeld:
				sum.Held++
			}
		}()
	}
	wg.Wait()
	return sum, nil
}

type deliveryResult int

const (
	resultNone deliveryResult = iota
	resultSucceeded
	resultFailed
	resultDead
	resultHeld
)

// deliver makes one attempt at a claimed delivery, or ends or holds it
// if its endpoint isn't active.
func (s *Service) deliver(ctx context.Context, d store.WebhooksDelivery) (deliveryResult, error) {
	var (
		secrets []string
		url     string
		result  = resultNone
	)
	err := s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		ep, err := q.GetEndpointForDelivery(ctx, d.EndpointID)
		if err != nil {
			return fmt.Errorf("app: reading the delivery's endpoint: %w", err)
		}
		now := s.clk.Now()
		switch {
		case ep.DeletedAt.Valid, ep.Status == "disabled":
			reason := "endpoint_disabled"
			if ep.DeletedAt.Valid {
				reason = "endpoint_deleted"
			}
			if _, err := q.EndDelivery(ctx, store.EndDeliveryParams{DeadReason: text(reason), At: now, ID: d.ID}); err != nil {
				return fmt.Errorf("app: ending the delivery: %w", err)
			}
			result = resultDead
		case ep.Status != "active":
			if _, err := q.PostponeDelivery(ctx, store.PostponeDeliveryParams{NextAttemptAt: now.Add(s.holdFor), ID: d.ID}); err != nil {
				return fmt.Errorf("app: holding the delivery: %w", err)
			}
			result = resultHeld
		default:
			url = ep.Url
			secrets, err = s.signingSecrets(ctx, q, ep.ID)
			return err
		}
		return nil
	})
	if err != nil || result != resultNone {
		return result, err
	}

	at := s.clk.Now()
	sig, err := domain.Sign(secrets, at, d.Body)
	if err != nil {
		return resultNone, fmt.Errorf("app: signing the delivery: %w", err)
	}
	headers := map[string]string{
		domain.HeaderID: d.EventID.String(), domain.HeaderTimestamp: sig.Timestamp, domain.HeaderSignature: sig.Value,
	}
	resp := s.sender.Send(ctx, adapters.Request{URL: url, Headers: headers, Body: d.Body})
	return s.record(ctx, d, at, headers, resp)
}

// record writes one attempt, moves the delivery on, and keeps the
// endpoint's health: a success clears failing_since; a failure sets it,
// and past DisableAfter disables the endpoint, ends its pending
// deliveries and publishes webhook_endpoint.disabled.
func (s *Service) record(ctx context.Context, d store.WebhooksDelivery, at time.Time, headers map[string]string, resp adapters.Response) (deliveryResult, error) {
	sent := map[string]string{"Content-Type": adapters.ContentType, "User-Agent": adapters.UserAgent}
	for k, v := range headers {
		sent[k] = v
	}
	reqHeaders, err := json.Marshal(sent)
	if err != nil {
		return resultNone, fmt.Errorf("app: %w", err)
	}
	var respHeaders []byte
	if resp.Headers != nil {
		if respHeaders, err = json.Marshal(resp.Headers); err != nil {
			return resultNone, fmt.Errorf("app: %w", err)
		}
	}
	attemptID, err := ids.New()
	if err != nil {
		return resultNone, fmt.Errorf("app: %w", err)
	}
	attempt := d.Attempts + 1
	succeeded := resp.Outcome == adapters.OutcomeSucceeded

	result := resultNone
	err = s.pool.WithRoleTx(ctx, systemRole, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		_, err := q.CreateDeliveryAttempt(ctx, store.CreateDeliveryAttemptParams{
			ID: attemptID, TenantID: d.TenantID, DeliveryID: d.ID, Attempt: attempt, StartedAt: at,
			DurationMs:     int32(min(resp.Duration.Milliseconds(), 1<<31-1)), //nolint:gosec // G115: clamped above
			RequestHeaders: reqHeaders, Outcome: string(resp.Outcome), ResponseStatus: status(resp),
			ResponseHeaders: respHeaders, ResponseBody: resp.Body, ResponseTruncated: resp.Truncated, Error: text(resp.Error),
		})
		if isUniqueViolation(err) {
			return errLostClaim
		}
		if err != nil {
			return fmt.Errorf("app: recording the attempt: %w", err)
		}

		now := s.clk.Now()
		params := store.RecordDeliveryResultParams{ID: d.ID, AttemptsPrev: d.Attempts}
		switch next, retry := domain.NextAttempt(s.schedule, int(attempt), now, s.jitter, s.randFloat); {
		case succeeded:
			params.Status, params.CompletedAt, result = "succeeded", timestamptz(now), resultSucceeded
		case retry:
			params.Status, params.NextAttemptAt, result = "pending", timestamptz(next), resultFailed
		default:
			params.Status, params.CompletedAt, params.DeadReason, result = "dead", timestamptz(now), text("attempts_exhausted"), resultDead
		}
		n, err := q.RecordDeliveryResult(ctx, params)
		if err != nil {
			return fmt.Errorf("app: moving the delivery on: %w", err)
		}
		if n == 0 {
			return errLostClaim
		}
		if succeeded {
			if err := q.ClearEndpointFailing(ctx, d.EndpointID); err != nil {
				return fmt.Errorf("app: clearing the endpoint's failure: %w", err)
			}
			return nil
		}
		return s.endpointFailed(ctx, tx, q, d, now)
	})
	if err != nil {
		return resultNone, err
	}
	return result, nil
}

// endpointFailed records a failed attempt against the endpoint and
// disables it once it has failed for DisableAfter.
func (s *Service) endpointFailed(ctx context.Context, tx pgx.Tx, q *store.Queries, d store.WebhooksDelivery, now time.Time) error {
	since, err := q.MarkEndpointFailing(ctx, store.MarkEndpointFailingParams{At: now, ID: d.EndpointID})
	if err != nil {
		return fmt.Errorf("app: marking the endpoint failing: %w", err)
	}
	if !since.Valid || !domain.ShouldDisable(since.Time, now, s.disableAt) {
		return nil
	}
	ep, err := q.DisableFailingEndpoint(ctx, store.DisableFailingEndpointParams{At: now, ID: d.EndpointID, Cutoff: now.Add(-s.disableAt)})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not active any more: disabled already, or by someone else
	}
	if err != nil {
		return fmt.Errorf("app: disabling the endpoint: %w", err)
	}
	dead, err := q.KillPendingDeliveries(ctx, store.KillPendingDeliveriesParams{DeadReason: text("endpoint_disabled"), At: now, EndpointID: ep.ID})
	if err != nil {
		return fmt.Errorf("app: ending the endpoint's deliveries: %w", err)
	}
	eventID, err := ids.New()
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	env, err := events.New(eventID, "webhook_endpoint.disabled", 1, ep.TenantID, now, "", map[string]any{
		"tenant_id": ep.TenantID, "endpoint_id": ep.ID, "reason": "failing",
		"failing_since": since.Time.UTC(), "disabled_at": now.UTC(), "dead_deliveries": dead,
	})
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	if err := outbox.Write(ctx, tx, "webhook_endpoint", ep.ID, env); err != nil {
		return fmt.Errorf("app: %w", err)
	}
	s.log.WarnContext(ctx, "webhooks: endpoint disabled after failing", "endpoint_id", ep.ID, "tenant_id", ep.TenantID,
		"failing_since", since.Time, "dead_deliveries", dead)
	return nil
}

func status(resp adapters.Response) pgtype.Int4 {
	if resp.Outcome != adapters.OutcomeSucceeded && resp.Outcome != adapters.OutcomeHTTPStatus {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(resp.Status), Valid: true} //nolint:gosec // G115: an HTTP status
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func timestamptz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
