package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/ids"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/identity/store"
)

// SecurityEvent is one identity.security_events row as shown to its
// account — system design §3's "logged and shown to the tenant admin"
// (this phase's placeholder: the account's own owner, same reasoning
// every other Phase 1 list uses — real tenant admins don't exist
// until Phase 2).
type SecurityEvent struct {
	ID         uuid.UUID
	Type       string
	IP         *string
	Detail     map[string]any
	OccurredAt time.Time
}

func securityEventSummary(row store.IdentitySecurityEvent) (SecurityEvent, error) {
	ev := SecurityEvent{ID: row.ID, Type: row.Type, OccurredAt: row.OccurredAt}
	if row.Ip != nil {
		ip := row.Ip.String()
		ev.IP = &ip
	}
	if len(row.Detail) > 0 {
		if err := json.Unmarshal(row.Detail, &ev.Detail); err != nil {
			return SecurityEvent{}, fmt.Errorf("app: decoding security event detail: %w", err)
		}
	}
	return ev, nil
}

// ListSecurityEvents returns userID's own security events, newest
// first (bounded — see ListSecurityEventsByUser's own comment).
func (s *Service) ListSecurityEvents(ctx context.Context, userID uuid.UUID) ([]SecurityEvent, error) {
	rows, err := store.New(s.pool).ListSecurityEventsByUser(ctx, pgtype.UUID{Bytes: userID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("app: listing security events: %w", err)
	}
	events := make([]SecurityEvent, len(rows))
	for i, row := range rows {
		ev, err := securityEventSummary(row)
		if err != nil {
			return nil, err
		}
		events[i] = ev
	}
	return events, nil
}

// recordSecurityEvent inserts one identity.security_events row — the
// shared helper every credential/sign-in action in this service calls
// (system design §3: "every credential event ... is logged"). detail
// is marshaled to JSON; a nil map becomes "{}", the column's own
// default shape.
func recordSecurityEvent(ctx context.Context, q *store.Queries, userID uuid.UUID, eventType string, ip net.IP, detail map[string]any) error {
	id, err := ids.New()
	if err != nil {
		return fmt.Errorf("app: generating security event id: %w", err)
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("app: encoding security event detail: %w", err)
	}
	_, err = q.CreateSecurityEvent(ctx, store.CreateSecurityEventParams{
		ID:     id,
		UserID: pgtype.UUID{Bytes: userID, Valid: true},
		Type:   eventType,
		Ip:     toNetipAddr(ip),
		Detail: detailJSON,
	})
	if err != nil {
		return fmt.Errorf("app: recording security event %q: %w", eventType, err)
	}
	return nil
}

// recordSignInEvent records a sign_in security event for userID and
// reports whether SendLoginAlert should follow once the enclosing
// transaction commits — true only when ip is known, this isn't the
// account's very first sign-in ever, and ip has never appeared on a
// prior sign_in event for this user ("used from a new IP", system
// design §3's audit bullet; a brand-new account's very first sign-in
// is always "new" by that measure, but alerting on it would just be
// noise right after sign-up, so it's deliberately excluded). Called
// from the one shared chokepoint every sign-in path (password, TOTP,
// passkey) already funnels through (Service.createSession).
func (s *Service) recordSignInEvent(ctx context.Context, q *store.Queries, userID uuid.UUID, ip net.IP) (bool, error) {
	newIP := false
	shouldAlert := false

	if ip != nil {
		pgUserID := pgtype.UUID{Bytes: userID, Valid: true}
		priorTotal, err := q.CountSignInEventsByUser(ctx, pgUserID)
		if err != nil {
			return false, fmt.Errorf("app: counting prior sign-ins: %w", err)
		}
		priorSameIP, err := q.CountSignInEventsByUserAndIP(ctx, store.CountSignInEventsByUserAndIPParams{
			UserID: pgUserID,
			Ip:     toNetipAddr(ip),
		})
		if err != nil {
			return false, fmt.Errorf("app: counting prior sign-ins from this ip: %w", err)
		}
		newIP = priorSameIP == 0
		shouldAlert = newIP && priorTotal > 0
	}

	if err := recordSecurityEvent(ctx, q, userID, "sign_in", ip, map[string]any{"new_ip": newIP}); err != nil {
		return false, err
	}
	return shouldAlert, nil
}

// sendLoginAlert sends a best-effort "new sign-in" notice — unlike
// every other EmailSender call in this service (signup/password-
// reset/step-up codes), a failure here is deliberately swallowed, not
// propagated: those three are essential to their own flow (no code
// delivered means the flow is actually stuck), while this is a
// notification about a sign-in that has already succeeded. Failing
// the sign-in itself over a transient SMTP hiccup would be worse than
// losing one notification.
func (s *Service) sendLoginAlert(ctx context.Context, to, fullName string, ip net.IP) {
	if ip == nil {
		return
	}
	_ = s.email.SendLoginAlert(ctx, to, fullName, ip.String(), s.clk.Now())
}
