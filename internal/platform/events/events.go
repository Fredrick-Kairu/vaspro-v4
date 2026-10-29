// Package events defines the event envelope every service publishes
// through platform/outbox (data model §1).
package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Envelope is the shape of every event on the bus.
type Envelope struct {
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	Version    int             `json:"version"`
	TenantID   uuid.UUID       `json:"tenant_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	TraceID    string          `json:"trace_id,omitempty"`
	Data       json.RawMessage `json:"data"`
}

// New marshals data and builds an Envelope around it. It takes id and
// occurredAt as explicit arguments rather than generating them itself —
// the caller already has an ids/clock instance wired in, and a pure
// constructor over explicit values is simpler to test.
func New(id uuid.UUID, eventType string, version int, tenantID uuid.UUID, occurredAt time.Time, traceID string, data any) (Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, fmt.Errorf("events: marshaling data: %w", err)
	}
	return Envelope{
		ID:         id,
		Type:       eventType,
		Version:    version,
		TenantID:   tenantID,
		OccurredAt: occurredAt,
		TraceID:    traceID,
		Data:       raw,
	}, nil
}
