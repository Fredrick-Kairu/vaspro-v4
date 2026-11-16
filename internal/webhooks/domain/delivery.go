package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
)

// DefaultSchedule is the wait before each retry (OQ-23): one attempt at
// once, then one after each wait, about 71 hours in all.
var DefaultSchedule = []time.Duration{
	30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour,
	3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 24 * time.Hour,
}

// DefaultDisableAfter is how long an endpoint fails, with no success,
// before it is disabled (OQ-23).
const DefaultDisableAfter = 72 * time.Hour

// MaxAttempts is how many attempts a delivery gets under schedule: the
// first, and one after each wait.
func MaxAttempts(schedule []time.Duration) int { return len(schedule) + 1 }

// ParseSchedule reads a comma-separated list of positive Go durations,
// e.g. "30s,2m,10m".
func ParseSchedule(s string) ([]time.Duration, error) {
	parts := strings.Split(s, ",")
	out := make([]time.Duration, 0, len(parts))
	for _, p := range parts {
		d, err := time.ParseDuration(strings.TrimSpace(p))
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("webhooks: retry schedule %q: each wait must be a positive duration", s)
		}
		out = append(out, d)
	}
	return out, nil
}

// NextAttempt is when a delivery that has had made attempts, the last
// failing at now, is tried again, or false once every wait is spent.
// The wait varies by up to ±jitter of itself, drawn from rnd (a uniform
// [0,1); math/rand when nil), so failed deliveries don't retry in step.
func NextAttempt(schedule []time.Duration, made int, now time.Time, jitter float64, rnd func() float64) (time.Time, bool) {
	if made < 1 || made > len(schedule) {
		return time.Time{}, false
	}
	wait := schedule[made-1]
	if jitter > 0 {
		if rnd == nil {
			rnd = rand.Float64
		}
		wait = time.Duration(float64(wait) * (1 + jitter*(2*rnd()-1)))
	}
	return now.Add(wait), true
}

// ShouldDisable reports whether an endpoint failing since failingSince
// has failed long enough, at now, to be disabled.
func ShouldDisable(failingSince, now time.Time, after time.Duration) bool {
	return !now.Before(failingSince.Add(after))
}

// body is what a webhook carries (data model §8): the event's envelope
// without its trace ID.
type body struct {
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	Version    int             `json:"version"`
	TenantID   uuid.UUID       `json:"tenant_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

// DeliveryBody is the bytes every attempt and replay of env sends.
func DeliveryBody(env events.Envelope) ([]byte, error) {
	b, err := json.Marshal(body{
		ID: env.ID, Type: env.Type, Version: env.Version, TenantID: env.TenantID,
		OccurredAt: env.OccurredAt.UTC(), Data: env.Data,
	})
	if err != nil {
		return nil, fmt.Errorf("webhooks: encoding the delivery body: %w", err)
	}
	return b, nil
}

// errNoRecipient is why an event goes to no client.
var errNoRecipient = errors.New("webhooks: no client to send this event to")

// Recipient is the client env is sent to: only an event type sent to
// clients, whose envelope names the client and whose data's tenant_id
// (required for these events) is the same client. Anything else goes
// nowhere, so a mislabelled event never reaches the wrong client.
func Recipient(env events.Envelope) (uuid.UUID, bool) {
	tenant, err := recipient(env)
	return tenant, err == nil
}

func recipient(env events.Envelope) (uuid.UUID, error) {
	if !IsClientEventType(env.Type) || env.TenantID == uuid.Nil {
		return uuid.Nil, errNoRecipient
	}
	var data struct {
		TenantID string `json:"tenant_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return uuid.Nil, errNoRecipient
	}
	named, err := uuid.Parse(data.TenantID)
	if err != nil || named != env.TenantID {
		return uuid.Nil, errNoRecipient
	}
	return env.TenantID, nil
}
