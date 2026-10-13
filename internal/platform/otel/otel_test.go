package otel_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"

	vaspotel "github.com/Fredrick-Kairu/vaspro-v4/internal/platform/otel"
)

// fakeCollector is a minimal in-process OTLP collector that records every
// export request it receives, so tests can assert data was actually sent
// over the wire rather than just that no error was returned. Both gRPC
// services define their RPC as "Export" with different signatures, so each
// needs its own receiver type; both share this struct for storage.
type fakeCollector struct {
	mu             sync.Mutex
	traceRequests  []*collectortrace.ExportTraceServiceRequest
	metricRequests []*collectormetrics.ExportMetricsServiceRequest
}

type fakeTraceServer struct {
	collectortrace.UnimplementedTraceServiceServer
	c *fakeCollector
}

func (s *fakeTraceServer) Export(_ context.Context, req *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.traceRequests = append(s.c.traceRequests, req)
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

type fakeMetricsServer struct {
	collectormetrics.UnimplementedMetricsServiceServer
	c *fakeCollector
}

func (s *fakeMetricsServer) Export(_ context.Context, req *collectormetrics.ExportMetricsServiceRequest) (*collectormetrics.ExportMetricsServiceResponse, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.metricRequests = append(s.c.metricRequests, req)
	return &collectormetrics.ExportMetricsServiceResponse{}, nil
}

func (f *fakeCollector) serviceNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, req := range f.traceRequests {
		for _, rs := range req.GetResourceSpans() {
			for _, attr := range rs.GetResource().GetAttributes() {
				if attr.GetKey() == "service.name" {
					names = append(names, attr.GetValue().GetStringValue())
				}
			}
		}
	}
	return names
}

func startFakeCollector(t *testing.T) (addr string, collector *fakeCollector) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	collector = &fakeCollector{}
	server := grpc.NewServer()
	collectortrace.RegisterTraceServiceServer(server, &fakeTraceServer{c: collector})
	collectormetrics.RegisterMetricsServiceServer(server, &fakeMetricsServer{c: collector})

	go func() {
		_ = server.Serve(lis)
	}()
	t.Cleanup(server.Stop)

	return lis.Addr().String(), collector
}

func TestSetup_ExportsTracesAndMetricsToCollector(t *testing.T) {
	addr, collector := startFakeCollector(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shutdown, err := vaspotel.Setup(ctx, vaspotel.Config{
		ServiceName:  "test-service",
		OTLPEndpoint: addr,
	})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	_, span := otel.Tracer("test").Start(ctx, "test-span")
	span.End()

	counter, err := otel.Meter("test").Int64Counter("test_counter")
	if err != nil {
		t.Fatalf("failed to create counter: %v", err)
	}
	counter.Add(ctx, 1)

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}

	if len(collector.traceRequests) == 0 {
		t.Fatal("expected the fake collector to receive at least one trace export request")
	}
	if len(collector.metricRequests) == 0 {
		t.Fatal("expected the fake collector to receive at least one metrics export request")
	}

	names := collector.serviceNames()
	if len(names) == 0 || names[0] != "test-service" {
		t.Fatalf("expected resource service.name %q, got %v", "test-service", names)
	}
}

func TestSetup_UnreachableEndpointDoesNotErrorOrHang(t *testing.T) {
	// Reserve a port, then close it immediately so nothing is listening.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve a port: %v", err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("failed to close the reserved listener: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	shutdown, err := vaspotel.Setup(ctx, vaspotel.Config{
		ServiceName:  "test-service",
		OTLPEndpoint: addr,
	})
	if err != nil {
		t.Fatalf("Setup should not error for an unreachable endpoint (export errors surface at flush time): %v", err)
	}

	// Bounded by the context timeout above; we only care that this returns,
	// not whether the flush against a dead endpoint succeeds.
	_ = shutdown(ctx)
}
