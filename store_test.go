package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

type recordingStore struct {
	entries []*AuditLog
	err     error
}

func (r *recordingStore) Save(_ context.Context, entry *AuditLog) error {
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, entry)
	return nil
}

func TestMultiLogStore_Broadcast(t *testing.T) {
	s1 := &recordingStore{}
	s2 := &recordingStore{}

	multi := MultiLogStore(nil, s1, nil, s2)
	entry := &AuditLog{ID: "client-test-id", Method: "POST", StatusCode: 200}

	if err := multi.Save(context.Background(), entry); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(s1.entries) != 1 || s1.entries[0].ID != "client-test-id" {
		t.Errorf("s1 did not receive entry: %+v", s1.entries)
	}
	if len(s2.entries) != 1 || s2.entries[0].ID != "client-test-id" {
		t.Errorf("s2 did not receive entry: %+v", s2.entries)
	}
}

func TestMultiLogStore_ErrorAggregation(t *testing.T) {
	err1 := errors.New("client store 1 failed")
	err2 := errors.New("client store 2 failed")

	s1 := &recordingStore{err: err1}
	s2 := &recordingStore{err: err2}
	s3 := &recordingStore{}

	multi := MultiLogStore(s1, s2, s3)
	entry := &AuditLog{ID: "err-client-id"}

	err := multi.Save(context.Background(), entry)
	if err == nil {
		t.Fatal("expected aggregated error, got nil")
	}

	if !errors.Is(err, err1) || !errors.Is(err, err2) {
		t.Errorf("expected errors.Join containing err1 and err2, got: %v", err)
	}
	if len(s3.entries) != 1 {
		t.Errorf("s3 should still receive entry even if preceding stores fail")
	}
}

func TestConsoleStore_Save(t *testing.T) {
	cs := NewConsoleStore(nil)
	entry := &AuditLog{
		ID:         "test-id",
		Method:     "GET",
		FullURL:    "https://example.com/api",
		StatusCode: 200,
		LatencyMS:  10,
		CreatedAt:  time.Now(),
	}

	if err := cs.Save(context.Background(), entry); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if err := cs.Save(context.Background(), nil); err != nil {
		t.Fatalf("expected nil for nil entry, got %v", err)
	}

	var zeroCS ConsoleStore
	if err := zeroCS.Save(context.Background(), entry); err != nil {
		t.Fatalf("expected nil on zero-value ConsoleStore, got %v", err)
	}
}

func TestClient_WithLogStore_Integration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	store1 := &recordingStore{}
	store2 := &recordingStore{}

	c := New(
		WithBaseURL(server.URL),
		WithAuditConfig(
			WithAuditServiceName("test-service"),
			WithAuditLogStore(MultiLogStore(store1, store2)),
		),
	)

	resp, err := c.R().Get("/hello")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode() != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode())
	}

	if len(store1.entries) != 1 {
		t.Fatalf("expected store1 to receive 1 entry, got %d", len(store1.entries))
	}
	if len(store2.entries) != 1 {
		t.Fatalf("expected store2 to receive 1 entry, got %d", len(store2.entries))
	}

	entry := store1.entries[0]
	if entry.Method != "GET" {
		t.Errorf("expected Method GET, got %q", entry.Method)
	}
	if entry.StatusCode != http.StatusOK {
		t.Errorf("expected StatusCode 200, got %d", entry.StatusCode)
	}
	if entry.ServiceName != "test-service" {
		t.Errorf("expected ServiceName 'test-service', got %q", entry.ServiceName)
	}
	if entry.ID == "" {
		t.Error("expected non-empty ID")
	}
}

func TestClient_WithLogStore_DirectOption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	store := &recordingStore{}
	c := New(
		WithBaseURL(server.URL),
		WithLogStore(store),
	)

	_, err := c.R().Post("/create")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	if len(store.entries) != 1 {
		t.Fatalf("expected store to receive 1 entry, got %d", len(store.entries))
	}
	if store.entries[0].StatusCode != http.StatusCreated {
		t.Errorf("expected 201 Created, got %d", store.entries[0].StatusCode)
	}
}

func TestConsoleStore_TraceAndSpanID(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("invalid trace ID: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("invalid span ID: %v", err)
	}

	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	tracedCtx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	t.Run("logs trace_id and span_id from context", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))
		store := NewConsoleStore(logger)

		entry := &AuditLog{ID: "trace-1", Method: "GET", FullURL: "https://example.com/health"}
		if saveErr := store.Save(tracedCtx, entry); saveErr != nil {
			t.Fatalf("unexpected error: %v", saveErr)
		}

		var logged map[string]any
		if unmarshalErr := json.Unmarshal(buf.Bytes(), &logged); unmarshalErr != nil {
			t.Fatalf("failed to unmarshal log output: %v (raw: %s)", unmarshalErr, buf.String())
		}
		if got := logged["trace_id"]; got != traceID.String() {
			t.Errorf("expected trace_id %q, got %v", traceID.String(), got)
		}
		if got := logged["span_id"]; got != spanID.String() {
			t.Errorf("expected span_id %q, got %v", spanID.String(), got)
		}
	})

	t.Run("omits trace_id and span_id when absent", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, nil))
		store := NewConsoleStore(logger)

		entry := &AuditLog{ID: "trace-2", Method: "GET", FullURL: "https://example.com/health"}
		if saveErr := store.Save(context.Background(), entry); saveErr != nil {
			t.Fatalf("unexpected error: %v", saveErr)
		}

		var logged map[string]any
		if unmarshalErr := json.Unmarshal(buf.Bytes(), &logged); unmarshalErr != nil {
			t.Fatalf("failed to unmarshal log output: %v (raw: %s)", unmarshalErr, buf.String())
		}
		if _, exists := logged["trace_id"]; exists {
			t.Errorf("expected trace_id to be omitted, got %v", logged["trace_id"])
		}
		if _, exists := logged["span_id"]; exists {
			t.Errorf("expected span_id to be omitted, got %v", logged["span_id"])
		}
	})

	t.Run("recordAuditLog populates TraceID and SpanID on entry", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		store := &recordingStore{}
		c := New(
			WithBaseURL(ts.URL),
			WithLogStore(store),
		)

		if _, reqErr := c.R().SetContext(tracedCtx).Get("/trace"); reqErr != nil {
			t.Fatalf("request failed: %v", reqErr)
		}
		if len(store.entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(store.entries))
		}
		if store.entries[0].TraceID != traceID.String() {
			t.Errorf("expected entry.TraceID %q, got %q", traceID.String(), store.entries[0].TraceID)
		}
		if store.entries[0].SpanID != spanID.String() {
			t.Errorf("expected entry.SpanID %q, got %q", spanID.String(), store.entries[0].SpanID)
		}
	})
}
