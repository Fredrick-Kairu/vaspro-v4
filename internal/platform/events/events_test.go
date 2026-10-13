package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Fredrick-Kairu/vaspro-v4/internal/platform/events"
)

type tokensPurchased struct {
	WalletID string `json:"wallet_id"`
	Tokens   int    `json:"tokens"`
}

func TestNew_MarshalsDataAndBuildsEnvelope(t *testing.T) {
	id := uuid.New()
	tenantID := uuid.New()
	occurredAt := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	env, err := events.New(id, "tokens.purchased", 1, tenantID, occurredAt, "trace-123",
		tokensPurchased{WalletID: "wallet_1", Tokens: 1000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if env.ID != id {
		t.Errorf("ID = %v, want %v", env.ID, id)
	}
	if env.Type != "tokens.purchased" {
		t.Errorf("Type = %q, want %q", env.Type, "tokens.purchased")
	}
	if env.Version != 1 {
		t.Errorf("Version = %d, want 1", env.Version)
	}
	if env.TenantID != tenantID {
		t.Errorf("TenantID = %v, want %v", env.TenantID, tenantID)
	}
	if !env.OccurredAt.Equal(occurredAt) {
		t.Errorf("OccurredAt = %v, want %v", env.OccurredAt, occurredAt)
	}
	if env.TraceID != "trace-123" {
		t.Errorf("TraceID = %q, want %q", env.TraceID, "trace-123")
	}

	var data tokensPurchased
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("Data did not unmarshal: %v", err)
	}
	if data != (tokensPurchased{WalletID: "wallet_1", Tokens: 1000}) {
		t.Fatalf("Data round-tripped as %+v", data)
	}
}

func TestNew_JSONFieldNamesMatchTheEnvelopeShape(t *testing.T) {
	env, err := events.New(uuid.New(), "wallet.credited", 1, uuid.New(), time.Now().UTC(), "", map[string]int{"amount": 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, field := range []string{"id", "type", "version", "tenant_id", "occurred_at", "data"} {
		if _, ok := m[field]; !ok {
			t.Errorf("marshaled envelope is missing field %q: %s", field, raw)
		}
	}
	if _, ok := m["trace_id"]; ok {
		t.Error("expected trace_id to be omitted when empty")
	}
}

func TestNew_UnmarshalableDataReturnsError(t *testing.T) {
	_, err := events.New(uuid.New(), "x", 1, uuid.New(), time.Now().UTC(), "", make(chan int))
	if err == nil {
		t.Fatal("expected an error for data that can't be marshaled to JSON")
	}
}
