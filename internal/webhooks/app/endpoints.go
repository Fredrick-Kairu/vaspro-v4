package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	vascrypto "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/crypto"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/adapters"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/domain"
	"github.com/Fredrick-Kairu/vaspro-v4/internal/webhooks/store"
)

// MaxDescriptionLength is the longest endpoint description.
const MaxDescriptionLength = 255

// NewEndpoint is what a client gives to register an endpoint.
type NewEndpoint struct {
	URL         string
	Description string
	Events      []string // client event types, or "*"
}

// EndpointChanges are the fields an update changes; nil leaves one as
// it is.
type EndpointChanges struct {
	URL         *string
	Description *string // "" clears it
	Events      []string
}

// Endpoint is an endpoint as a client sees it: never its secret.
type Endpoint struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	URL            string
	Description    string
	Status         string
	VerifiedAt     *time.Time
	FailingSince   *time.Time
	DisabledAt     *time.Time
	DisabledReason string
	Events         []string
	CreatedBy      uuid.UUID
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CreatedEndpoint is a new endpoint and its secret, shown this once.
type CreatedEndpoint struct {
	Endpoint
	Secret string
}

// CreateEndpoint registers an endpoint (task 5.2, OQ-23):
// credentials:manage and a fresh step-up, an allowed URL, and event
// types sent to clients. It starts pending_verification, receiving
// nothing until VerifyEndpoint's challenge is answered, which the client
// can only do once it has configured the secret returned here: the one
// time it is shown. A client has at most Deps.MaxEndpoints undeleted
// endpoints.
func (s *Service) CreateEndpoint(ctx context.Context, user, session, tenant uuid.UUID, in NewEndpoint) (CreatedEndpoint, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsManage); err != nil {
		return CreatedEndpoint{}, err
	}
	if err := s.requireStepUp(ctx, user, session); err != nil {
		return CreatedEndpoint{}, err
	}
	if err := domain.CheckURL(in.URL, s.insecure); err != nil {
		return CreatedEndpoint{}, err
	}
	events, err := checkEvents(in.Events)
	if err != nil {
		return CreatedEndpoint{}, err
	}
	desc, err := description(in.Description)
	if err != nil {
		return CreatedEndpoint{}, err
	}
	id, err := ids.New()
	if err != nil {
		return CreatedEndpoint{}, fmt.Errorf("app: %w", err)
	}
	secret, sealed, err := s.newSecret(ctx, tenant, id, user)
	if err != nil {
		return CreatedEndpoint{}, err
	}

	var out Endpoint
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockTenantEndpoints(ctx, tenant); err != nil {
			return fmt.Errorf("app: locking the account's endpoints: %w", err)
		}
		n, err := q.CountEndpoints(ctx)
		if err != nil {
			return fmt.Errorf("app: counting endpoints: %w", err)
		}
		if n >= int64(s.maxEnd) {
			return fmt.Errorf("%w: an account has at most %d webhook endpoints; delete one first", ErrValidation, s.maxEnd)
		}
		row, err := q.CreateEndpoint(ctx, store.CreateEndpointParams{
			ID: id, TenantID: tenant, Url: in.URL, Description: desc, CreatedBy: user,
		})
		if err != nil {
			return fmt.Errorf("app: creating the endpoint: %w", err)
		}
		if err := replaceSubscriptions(ctx, q, tenant, id, events, false); err != nil {
			return err
		}
		if _, err := q.CreateEndpointSecret(ctx, sealed); err != nil {
			return fmt.Errorf("app: storing the endpoint's secret: %w", err)
		}
		out = toEndpoint(row, events)
		return nil
	})
	if err != nil {
		return CreatedEndpoint{}, err
	}
	s.log.InfoContext(ctx, "webhooks: endpoint created", "endpoint_id", id, "tenant_id", tenant, "events", len(events))
	return CreatedEndpoint{Endpoint: out, Secret: secret}, nil
}

// GetEndpoint reads one endpoint (credentials:read).
func (s *Service) GetEndpoint(ctx context.Context, user, tenant, id uuid.UUID) (Endpoint, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsRead); err != nil {
		return Endpoint{}, err
	}
	var out Endpoint
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = loadEndpoint(ctx, store.New(tx), id)
		return err
	})
	return out, err
}

// ListEndpoints lists the account's undeleted endpoints, oldest first
// (credentials:read). There are at most Deps.MaxEndpoints, so no
// cursor.
func (s *Service) ListEndpoints(ctx context.Context, user, tenant uuid.UUID) ([]Endpoint, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsRead); err != nil {
		return nil, err
	}
	var out []Endpoint
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		rows, err := q.ListEndpoints(ctx)
		if err != nil {
			return fmt.Errorf("app: listing endpoints: %w", err)
		}
		out = make([]Endpoint, 0, len(rows))
		for _, row := range rows {
			events, err := subscriptionsOf(ctx, q, row.ID)
			if err != nil {
				return err
			}
			out = append(out, toEndpoint(row, events))
		}
		return nil
	})
	return out, err
}

// UpdateEndpoint changes an endpoint (credentials:manage). A new URL
// needs a fresh step-up (system design §3: changing callback URLs) and
// sends the endpoint back to pending_verification, so it receives
// nothing until it answers the challenge. The description and the
// event types need no step-up; the types given replace the old ones.
func (s *Service) UpdateEndpoint(ctx context.Context, user, session, tenant, id uuid.UUID, ch EndpointChanges) (Endpoint, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsManage); err != nil {
		return Endpoint{}, err
	}
	if ch.URL != nil {
		if err := s.requireStepUp(ctx, user, session); err != nil {
			return Endpoint{}, err
		}
		if err := domain.CheckURL(*ch.URL, s.insecure); err != nil {
			return Endpoint{}, err
		}
	}
	var events []string
	if ch.Events != nil {
		var err error
		if events, err = checkEvents(ch.Events); err != nil {
			return Endpoint{}, err
		}
	}
	var desc pgtype.Text
	if ch.Description != nil {
		var err error
		if desc, err = description(*ch.Description); err != nil {
			return Endpoint{}, err
		}
	}

	var out Endpoint
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		row, err := q.GetEndpointForUpdate(ctx, id)
		if err != nil {
			return notFound(err, id)
		}
		now := s.clk.Now()
		if ch.URL != nil && *ch.URL != row.Url {
			if _, err := q.UpdateEndpointURL(ctx, store.UpdateEndpointURLParams{Url: *ch.URL, At: now, ID: id}); err != nil {
				return fmt.Errorf("app: changing the endpoint's URL: %w", err)
			}
		}
		if ch.Description != nil {
			if _, err := q.UpdateEndpointDescription(ctx, store.UpdateEndpointDescriptionParams{Description: desc, At: now, ID: id}); err != nil {
				return fmt.Errorf("app: changing the endpoint's description: %w", err)
			}
		}
		if ch.Events != nil {
			if err := replaceSubscriptions(ctx, q, tenant, id, events, true); err != nil {
				return err
			}
		}
		out, err = loadEndpoint(ctx, q, id)
		return err
	})
	if err != nil {
		return Endpoint{}, err
	}
	s.log.InfoContext(ctx, "webhooks: endpoint updated", "endpoint_id", id, "tenant_id", tenant, "url_changed", ch.URL != nil)
	return out, nil
}

// VerifyEndpoint sends the endpoint a signed webhook.verification
// request (credentials:manage; it changes no credential, so no
// step-up). An answer within the Sender's timeout, 2xx, echoing the
// challenge makes the endpoint active: from pending, from disabled
// (re-enabling), or again when already active. Anything else is a
// *VerificationFailure and changes nothing; a refused address is
// ErrURLNotAllowed. The request goes out with no transaction open, and
// the endpoint is marked verified only if it wasn't changed meanwhile.
func (s *Service) VerifyEndpoint(ctx context.Context, user, tenant, id uuid.UUID) (Endpoint, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsManage); err != nil {
		return Endpoint{}, err
	}
	var (
		row     store.WebhooksEndpoint
		secrets []string
	)
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if row, err = q.GetEndpoint(ctx, id); err != nil {
			return notFound(err, id)
		}
		secrets, err = s.signingSecrets(ctx, q, id)
		return err
	})
	if err != nil {
		return Endpoint{}, err
	}

	if err := s.challenge(ctx, tenant, row.Url, secrets); err != nil {
		s.log.InfoContext(ctx, "webhooks: endpoint verification failed", "endpoint_id", id, "tenant_id", tenant, "error", err)
		return Endpoint{}, err
	}

	var out Endpoint
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.MarkEndpointVerified(ctx, store.MarkEndpointVerifiedParams{At: s.clk.Now(), ID: id, Version: row.Version})
		if err != nil {
			return fmt.Errorf("app: marking the endpoint verified: %w", err)
		}
		if n == 0 {
			return &VerificationFailure{Reason: "the endpoint changed while it was being verified; verify it again"}
		}
		out, err = loadEndpoint(ctx, q, id)
		return err
	})
	if err != nil {
		return Endpoint{}, err
	}
	s.log.InfoContext(ctx, "webhooks: endpoint verified", "endpoint_id", id, "tenant_id", tenant)
	return out, nil
}

// challenge sends one signed verification request to url.
func (s *Service) challenge(ctx context.Context, tenant uuid.UUID, url string, secrets []string) error {
	if s.sender == nil {
		return errors.New("app: no sender for endpoints")
	}
	value, err := domain.NewChallenge(s.rand)
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	eventID, err := ids.New()
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	now := s.clk.Now()
	body, err := domain.VerificationBody(eventID, tenant, now, value)
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	sig, err := domain.Sign(secrets, now, body)
	if err != nil {
		return fmt.Errorf("app: signing the challenge: %w", err)
	}
	resp := s.sender.Send(ctx, adapters.Request{URL: url, Body: body, Headers: map[string]string{
		domain.HeaderID: eventID.String(), domain.HeaderTimestamp: sig.Timestamp, domain.HeaderSignature: sig.Value,
	}})
	switch {
	case resp.Outcome == adapters.OutcomeRefusedURL:
		return fmt.Errorf("%w: %s", ErrURLNotAllowed, resp.Error)
	case resp.Outcome == adapters.OutcomeHTTPStatus:
		return &VerificationFailure{Outcome: resp.Outcome, Status: resp.Status, Reason: fmt.Sprintf("the endpoint answered %d, not 2xx", resp.Status)}
	case resp.Outcome != adapters.OutcomeSucceeded:
		return &VerificationFailure{Outcome: resp.Outcome, Reason: fmt.Sprintf("no answer (%s): %s", resp.Outcome, resp.Error)}
	case resp.Truncated || !domain.ChallengeEchoed(resp.Body, value):
		return &VerificationFailure{Status: resp.Status, Reason: `the answer wasn't {"challenge": "<the value sent>"}`}
	}
	return nil
}

// Rotation is a new endpoint secret, shown once, and until when the
// previous one still signs alongside it.
type Rotation struct {
	Secret             string
	PreviousValidUntil time.Time
}

// RotateEndpointSecret gives the endpoint a new secret, returned this
// once (credentials:manage and a fresh step-up). The previous one keeps
// signing for domain.SecretOverlap, after the new one; a secret still
// in an earlier overlap stops at once, so no more than two are valid.
func (s *Service) RotateEndpointSecret(ctx context.Context, user, session, tenant, id uuid.UUID) (Rotation, error) {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsManage); err != nil {
		return Rotation{}, err
	}
	if err := s.requireStepUp(ctx, user, session); err != nil {
		return Rotation{}, err
	}
	secret, sealed, err := s.newSecret(ctx, tenant, id, user)
	if err != nil {
		return Rotation{}, err
	}
	var until time.Time
	err = s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.GetEndpointForUpdate(ctx, id); err != nil {
			return notFound(err, id)
		}
		now := s.clk.Now()
		until = now.Add(domain.SecretOverlap)
		if _, err := q.ExpireOverlappingSecrets(ctx, store.ExpireOverlappingSecretsParams{EndpointID: id, At: now}); err != nil {
			return fmt.Errorf("app: ending the previous overlap: %w", err)
		}
		if _, err := q.ExpireCurrentEndpointSecret(ctx, store.ExpireCurrentEndpointSecretParams{EndpointID: id, ExpiresAt: until}); err != nil {
			return fmt.Errorf("app: starting the overlap: %w", err)
		}
		if _, err := q.CreateEndpointSecret(ctx, sealed); err != nil {
			return fmt.Errorf("app: storing the new secret: %w", err)
		}
		return nil
	})
	if err != nil {
		return Rotation{}, err
	}
	s.log.InfoContext(ctx, "webhooks: endpoint secret rotated", "endpoint_id", id, "tenant_id", tenant)
	return Rotation{Secret: secret, PreviousValidUntil: until}, nil
}

// DeleteEndpoint deletes an endpoint (credentials:manage). The row
// stays, marked deleted, so the delivery log still names it; the
// delivery worker (task 5.4) ends its pending deliveries.
func (s *Service) DeleteEndpoint(ctx context.Context, user, tenant, id uuid.UUID) error {
	if err := s.requireTenant(ctx, user, tenant, PermCredentialsManage); err != nil {
		return err
	}
	err := s.pool.WithTenantTx(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		n, err := store.New(tx).DeleteEndpoint(ctx, store.DeleteEndpointParams{At: s.clk.Now(), ID: id})
		if err != nil {
			return fmt.Errorf("app: deleting the endpoint: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: endpoint %s", ErrNotFound, id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "webhooks: endpoint deleted", "endpoint_id", id, "tenant_id", tenant)
	return nil
}

// newSecret makes a secret for an endpoint and seals it, ready to
// insert.
func (s *Service) newSecret(ctx context.Context, tenant, endpoint, user uuid.UUID) (string, store.CreateEndpointSecretParams, error) {
	if s.envelope == nil || len(s.fpKey) == 0 {
		return "", store.CreateEndpointSecretParams{}, errors.New("app: no KMS or fingerprint key for endpoint secrets")
	}
	secret, err := domain.GenerateSecret(s.rand)
	if err != nil {
		return "", store.CreateEndpointSecretParams{}, fmt.Errorf("app: %w", err)
	}
	ciphertext, err := s.envelope.Seal(ctx, SecretsKMSKeyID, []byte(secret))
	if err != nil {
		return "", store.CreateEndpointSecretParams{}, fmt.Errorf("app: sealing the secret: %w", err)
	}
	id, err := ids.New()
	if err != nil {
		return "", store.CreateEndpointSecretParams{}, fmt.Errorf("app: %w", err)
	}
	return secret, store.CreateEndpointSecretParams{
		ID: id, TenantID: tenant, EndpointID: endpoint, Ciphertext: ciphertext, KmsKeyID: SecretsKMSKeyID,
		Fingerprint: vascrypto.HMACSign(s.fpKey, []byte(secret)), CreatedBy: user,
	}, nil
}

// signingSecrets opens the endpoint's valid secrets, the current one
// first: what every request to it is signed with.
func (s *Service) signingSecrets(ctx context.Context, q *store.Queries, endpoint uuid.UUID) ([]string, error) {
	if s.envelope == nil {
		return nil, errors.New("app: no KMS for endpoint secrets")
	}
	rows, err := q.ListValidEndpointSecrets(ctx, store.ListValidEndpointSecretsParams{EndpointID: endpoint, At: s.clk.Now()})
	if err != nil {
		return nil, fmt.Errorf("app: reading the endpoint's secrets: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("app: endpoint %s has no valid secret", endpoint)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		plain, err := s.envelope.Open(ctx, r.KmsKeyID, r.Ciphertext)
		if err != nil {
			return nil, fmt.Errorf("app: opening an endpoint secret: %w", err)
		}
		out = append(out, string(plain))
	}
	return out, nil
}

func checkEvents(in []string) ([]string, error) {
	events, err := domain.CheckSubscriptions(in)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrValidation, err)
	}
	return events, nil
}

func description(d string) (pgtype.Text, error) {
	if len(d) > MaxDescriptionLength {
		return pgtype.Text{}, fmt.Errorf("%w: a description is at most %d characters", ErrValidation, MaxDescriptionLength)
	}
	return pgtype.Text{String: d, Valid: d != ""}, nil
}

func replaceSubscriptions(ctx context.Context, q *store.Queries, tenant, endpoint uuid.UUID, events []string, clear bool) error {
	if clear {
		if err := q.DeleteSubscriptions(ctx, endpoint); err != nil {
			return fmt.Errorf("app: clearing subscriptions: %w", err)
		}
	}
	for _, e := range events {
		if err := q.AddSubscription(ctx, store.AddSubscriptionParams{TenantID: tenant, EndpointID: endpoint, EventType: e}); err != nil {
			return fmt.Errorf("app: subscribing to %s: %w", e, err)
		}
	}
	return nil
}

func subscriptionsOf(ctx context.Context, q *store.Queries, endpoint uuid.UUID) ([]string, error) {
	rows, err := q.ListSubscriptions(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("app: reading subscriptions: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.EventType)
	}
	return out, nil
}

func loadEndpoint(ctx context.Context, q *store.Queries, id uuid.UUID) (Endpoint, error) {
	row, err := q.GetEndpoint(ctx, id)
	if err != nil {
		return Endpoint{}, notFound(err, id)
	}
	events, err := subscriptionsOf(ctx, q, id)
	if err != nil {
		return Endpoint{}, err
	}
	return toEndpoint(row, events), nil
}

func notFound(err error, id uuid.UUID) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: endpoint %s", ErrNotFound, id)
	}
	return fmt.Errorf("app: reading endpoint %s: %w", id, err)
}

func toEndpoint(r store.WebhooksEndpoint, events []string) Endpoint {
	return Endpoint{
		ID: r.ID, TenantID: r.TenantID, URL: r.Url, Description: r.Description.String, Status: r.Status,
		VerifiedAt: timePtr(r.VerifiedAt), FailingSince: timePtr(r.FailingSince), DisabledAt: timePtr(r.DisabledAt),
		DisabledReason: r.DisabledReason.String, Events: events, CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
