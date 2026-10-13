// Package otel wires the OpenTelemetry SDK into a process: one trace
// provider and one meter provider, both exporting over OTLP/gRPC to a
// collector (locally, grafana/otel-lgtm from deploy/compose).
package otel

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Config holds what's needed to set up tracing and metrics for one service.
type Config struct {
	// ServiceName identifies this process in the resulting traces/metrics.
	ServiceName string

	// OTLPEndpoint is the collector's gRPC address (host:port). The
	// connection is plaintext (no TLS) — fine for local dev and anywhere
	// the collector is reached over a trusted network; a later phase
	// revisits this once OD-02 (hosting) is decided.
	OTLPEndpoint string
}

// Setup builds and registers the global trace and meter providers, and
// returns a shutdown func that flushes and closes both. Exporter creation
// doesn't dial eagerly, so Setup succeeds even if OTLPEndpoint is
// unreachable; a dead endpoint only surfaces an error from shutdown (or
// is dropped silently by the SDK's own retry/backoff, depending on how
// long shutdown's context allows it to try).
func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(cfg.ServiceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("otel: building resource: %w", err)
	}

	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("otel: creating trace exporter: %w", err)
	}

	metricExporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("otel: creating metric exporter: %w", err)
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter),
	)

	meterProvider := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(metricExporter)),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	shutdown = func(ctx context.Context) error {
		return errors.Join(
			tracerProvider.Shutdown(ctx),
			meterProvider.Shutdown(ctx),
		)
	}
	return shutdown, nil
}
