package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	vaslog "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/log"
)

func decodeLastLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	last := lines[len(lines)-1]

	var m map[string]any
	if err := json.Unmarshal(last, &m); err != nil {
		t.Fatalf("log output is not valid JSON: %v\noutput: %s", err, last)
	}
	return m
}

func TestNew_WithoutContextValues_NoExtraFields(t *testing.T) {
	var buf bytes.Buffer
	logger := vaslog.New(&buf, slog.LevelInfo)

	logger.Info("hello")

	m := decodeLastLine(t, &buf)
	for _, key := range []string{"request_id", "tenant_id", "trace_id"} {
		if _, ok := m[key]; ok {
			t.Fatalf("expected no %q field when not set, got: %v", key, m)
		}
	}
	if m["msg"] != "hello" {
		t.Fatalf("expected msg %q, got %v", "hello", m["msg"])
	}
}

func TestNew_WithRequestAndTenantID(t *testing.T) {
	var buf bytes.Buffer
	logger := vaslog.New(&buf, slog.LevelInfo)

	ctx := vaslog.WithRequestID(context.Background(), "req_123")
	ctx = vaslog.WithTenantID(ctx, "tenant_456")

	logger.InfoContext(ctx, "hello")

	m := decodeLastLine(t, &buf)
	if m["request_id"] != "req_123" {
		t.Fatalf("expected request_id %q, got %v", "req_123", m["request_id"])
	}
	if m["tenant_id"] != "tenant_456" {
		t.Fatalf("expected tenant_id %q, got %v", "tenant_456", m["tenant_id"])
	}
}

func TestNew_WithActiveSpan_IncludesTraceID(t *testing.T) {
	var buf bytes.Buffer
	logger := vaslog.New(&buf, slog.LevelInfo)

	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()

	ctx, span := tp.Tracer("test").Start(context.Background(), "test-span")
	defer span.End()

	logger.InfoContext(ctx, "hello")

	m := decodeLastLine(t, &buf)
	wantTraceID := span.SpanContext().TraceID().String()
	if m["trace_id"] != wantTraceID {
		t.Fatalf("expected trace_id %q, got %v", wantTraceID, m["trace_id"])
	}
}

func TestRequestIDAndTenantID_Accessors(t *testing.T) {
	ctx := context.Background()

	if _, ok := vaslog.RequestID(ctx); ok {
		t.Fatal("expected no request ID on a bare context")
	}
	if _, ok := vaslog.TenantID(ctx); ok {
		t.Fatal("expected no tenant ID on a bare context")
	}

	ctx = vaslog.WithRequestID(ctx, "req_123")
	ctx = vaslog.WithTenantID(ctx, "tenant_456")

	id, ok := vaslog.RequestID(ctx)
	if !ok || id != "req_123" {
		t.Fatalf("expected request ID %q, got %q (ok=%v)", "req_123", id, ok)
	}
	tenant, ok := vaslog.TenantID(ctx)
	if !ok || tenant != "tenant_456" {
		t.Fatalf("expected tenant ID %q, got %q (ok=%v)", "tenant_456", tenant, ok)
	}
}
