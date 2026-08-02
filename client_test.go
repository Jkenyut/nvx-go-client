package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestClient_OpenTelemetryInstrumentation(t *testing.T) {
	// Setup global propagator for testing (so otelhttp injects trace context into headers)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Setup tracer provider so that span contexts are generated properly
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	tracer := otel.Tracer("test-tracer")

	var receivedTraceparent string

	// Setup mock server to capture the incoming headers
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// otelhttp should automatically inject the traceparent header
		receivedTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	c := NewClient()

	// Start a trace in our test environment
	ctx, span := tracer.Start(context.Background(), "test-request")
	defer span.End()

	// Make a request, passing the context so the transport can inject its traceparent.
	resp, err := c.R().SetContext(ctx).Get(ts.URL)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if resp.StatusCode() != http.StatusOK {
		t.Errorf("Expected status 200, got %v", resp.StatusCode())
	}

	// Verify that OpenTelemetry successfully injected the traceparent header
	if receivedTraceparent == "" {
		t.Error("Expected 'traceparent' header to be injected by OpenTelemetry transport, but it was empty")
	} else {
		t.Logf("Successfully propagated traceparent: %s", receivedTraceparent)
	}
}
