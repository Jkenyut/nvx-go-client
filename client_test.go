package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// setupTracer sets up an isolated TracerProvider and propagator for testing.
func setupTracer(t *testing.T) {
	t.Helper()
	prevPropagator := otel.GetTextMapPropagator()
	prevTracerProvider := otel.GetTracerProvider()

	otel.SetTextMapPropagator(propagation.TraceContext{})
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)

	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTextMapPropagator(prevPropagator)
		otel.SetTracerProvider(prevTracerProvider)
	})
}

func TestClient_OpenTelemetryInstrumentation(t *testing.T) {
	setupTracer(t)
	tracer := otel.Tracer("test-tracer")

	var receivedTraceparent string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	c := New()

	ctx, span := tracer.Start(context.Background(), "test-request")
	defer span.End()

	resp, err := c.R().SetContext(ctx).Get(ts.URL)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if resp.StatusCode() != http.StatusOK {
		t.Errorf("Expected status 200, got %v", resp.StatusCode())
	}

	if receivedTraceparent == "" {
		t.Error("Expected 'traceparent' header to be injected by OpenTelemetry transport, but it was empty")
	}
}

func TestNew_Defaults(t *testing.T) {
	c := New()
	if c == nil {
		t.Fatal("Expected non-nil Client")
	}
	if c.Client == nil {
		t.Fatal("Expected non-nil resty.Client")
	}
	// Verify default timeout is 30s
	if c.GetClient().Timeout != DefaultTimeout {
		t.Errorf("Expected timeout %v, got %v", DefaultTimeout, c.GetClient().Timeout)
	}
	// Verify transport is wrapped with otelhttp
	transport := c.GetClient().Transport
	if _, ok := transport.(*otelhttp.Transport); !ok {
		t.Errorf("Expected transport to be *otelhttp.Transport, got %T", transport)
	}
}

func TestClient_NilOptionsSafety(t *testing.T) {
	// Should not panic on nil options
	c := New(nil, nil)
	if c == nil {
		t.Fatal("Expected non-nil Client with nil options")
	}

	// Should not panic on nil WithResty closure
	c2 := New(WithResty(nil))
	if c2 == nil {
		t.Fatal("Expected non-nil Client with nil WithResty")
	}
}

func TestClient_Options(t *testing.T) {
	t.Run("WithTimeout", func(t *testing.T) {
		timeout := 5 * time.Second
		c := New(WithTimeout(timeout))
		if c.GetClient().Timeout != timeout {
			t.Errorf("Expected timeout %v, got %v", timeout, c.GetClient().Timeout)
		}
	})

	t.Run("WithBaseURL", func(t *testing.T) {
		baseURL := "https://api.example.com"
		c := New(WithBaseURL(baseURL))
		if c.HostURL != baseURL {
			t.Errorf("Expected BaseURL %v, got %v", baseURL, c.HostURL)
		}
	})

	t.Run("WithRetry", func(t *testing.T) {
		retryCount := 3
		retryWait := 100 * time.Millisecond
		c := New(WithRetry(retryCount, retryWait))
		if c.RetryCount != retryCount {
			t.Errorf("Expected RetryCount %d, got %d", retryCount, c.RetryCount)
		}
		if c.RetryWaitTime != retryWait {
			t.Errorf("Expected RetryWaitTime %v, got %v", retryWait, c.RetryWaitTime)
		}
	})

	t.Run("WithTransport", func(t *testing.T) {
		customTransport := &http.Transport{
			MaxIdleConns: 50,
		}
		c := New(WithTransport(customTransport))
		transport, ok := c.GetClient().Transport.(*otelhttp.Transport)
		if !ok {
			t.Fatalf("Expected transport to be wrapped in *otelhttp.Transport, got %T", c.GetClient().Transport)
		}
		if transport == nil {
			t.Fatal("Expected non-nil wrapped transport")
		}
	})

	t.Run("WithOTelOptions", func(t *testing.T) {
		formatter := func(operation string, r *http.Request) string {
			return "custom-span-" + operation
		}
		c := New(WithOTelOptions(otelhttp.WithSpanNameFormatter(formatter)))
		if len(c.otelOpts) != 1 {
			t.Errorf("Expected 1 otelOpt, got %d", len(c.otelOpts))
		}
	})

	t.Run("WithResty", func(t *testing.T) {
		var invoked bool
		c := New(WithResty(func(r *resty.Client) {
			invoked = true
			r.SetHeader("X-Custom-Header", "test-val")
		}))
		if !invoked {
			t.Error("Expected WithResty configuration function to be invoked")
		}
		if c.Header.Get("X-Custom-Header") != "test-val" {
			t.Errorf("Expected header X-Custom-Header to be 'test-val', got %q", c.Header.Get("X-Custom-Header"))
		}
	})
}

type testCustomRoundTripper struct {
	called atomic.Int64
}

func (rt *testCustomRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.called.Add(1)
	return http.DefaultTransport.RoundTrip(req)
}

func TestClient_SetTransport_GuaranteesInstrumentation(t *testing.T) {
	setupTracer(t)
	tracer := otel.Tracer("test-tracer")

	var receivedTraceparent string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ts.Close()

	customRT := &testCustomRoundTripper{}

	c := New()
	// Call SetTransport after New() to simulate caller overriding transport
	c.SetTransport(customRT)

	ctx, span := tracer.Start(context.Background(), "test-overridden-transport")
	defer span.End()

	resp, err := c.R().SetContext(ctx).Get(ts.URL)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if resp.StatusCode() != http.StatusOK {
		t.Errorf("Expected status 200, got: %v", resp.StatusCode())
	}

	// Verify custom transport was called
	if customRT.called.Load() == 0 {
		t.Error("Expected custom RoundTripper to be executed")
	}

	// Verify traceparent is still injected through OpenTelemetry wrapper
	if receivedTraceparent == "" {
		t.Error("Expected 'traceparent' header to be injected even after SetTransport call")
	}

	t.Run("SetTransport nil resets safely", func(t *testing.T) {
		c.SetTransport(nil)
		if _, ok := c.GetClient().Transport.(*otelhttp.Transport); !ok {
			t.Errorf("Expected *otelhttp.Transport after SetTransport(nil), got %T", c.GetClient().Transport)
		}
	})

	t.Run("SetTransport already wrapped avoids double wrapping", func(t *testing.T) {
		ot := otelhttp.NewTransport(http.DefaultTransport)
		c.SetTransport(ot)
		if c.GetClient().Transport != ot {
			t.Errorf("Expected transport to match existing otelhttp.Transport directly")
		}
	})
}

func BenchmarkNew(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = New(WithTimeout(10*time.Second), WithBaseURL("https://example.com"))
	}
}

func BenchmarkClient_Request(b *testing.B) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(WithBaseURL(ts.URL))
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = c.R().SetContext(ctx).Get("/")
	}
}
