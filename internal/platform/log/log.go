// Package log provides a JSON slog.Logger that automatically carries
// request_id, tenant_id and trace_id from context into every log line.
package log

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	tenantIDKey
)

// WithRequestID returns a context carrying the given request ID, for the
// request-ID middleware to set once per incoming request.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// WithTenantID returns a context carrying the given tenant ID.
func WithTenantID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, tenantIDKey, id)
}

// RequestID returns the request ID stored in ctx, if any.
func RequestID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}

// TenantID returns the tenant ID stored in ctx, if any.
func TenantID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(tenantIDKey).(string)
	return id, ok
}

// New returns a JSON slog.Logger. Every call made with *Context (e.g.
// InfoContext) picks up request_id and tenant_id from ctx (if set via
// WithRequestID/WithTenantID) and trace_id from the active OpenTelemetry
// span (if any) — callers never pass these fields themselves.
func New(w io.Writer, level slog.Level) *slog.Logger {
	handler := &contextHandler{
		Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}),
	}
	return slog.New(handler)
}

type contextHandler struct {
	slog.Handler
}

func (h *contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if id, ok := RequestID(ctx); ok {
		record.AddAttrs(slog.String("request_id", id))
	}
	if id, ok := TenantID(ctx); ok {
		record.AddAttrs(slog.String("tenant_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		record.AddAttrs(slog.String("trace_id", sc.TraceID().String()))
	}
	return h.Handler.Handle(ctx, record)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
