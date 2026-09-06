package client

import (
	"bytes"
	"context"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jkenyut/nvx-go-helper/activity"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type recordedLog struct {
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

func (r *recordedLog) getInt(key string) int {
	switch v := r.Attrs[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 0
	}
}

type recordingHandler struct {
	mu      sync.Mutex
	records []recordedLog
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	attrs := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})

	h.records = append(h.records, recordedLog{
		Level:   r.Level,
		Message: r.Message,
		Attrs:   attrs,
	})
	return nil
}

func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordingHandler) getRecords() []recordedLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	copied := make([]recordedLog, len(h.records))
	copy(copied, h.records)
	return copied
}

func (h *recordingHandler) lastRecord() *recordedLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) == 0 {
		return nil
	}
	rec := h.records[len(h.records)-1]
	return &rec
}

func newTestLogger() (*slog.Logger, *recordingHandler) {
	h := &recordingHandler{}
	return slog.New(h), h
}

func TestWithAudit_BasicRequestResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"success","data":{"id":123}}`))
	}))
	defer ts.Close()

	logger, handler := newTestLogger()
	c := New(
		WithAudit(AuditConfig{
			Logger:            logger,
			ServiceName:       "order-service",
			LogRequestBodies:  true,
			LogResponseBodies: true,
		}),
	)

	reqBody := map[string]any{"item": "laptop", "quantity": float64(1)}
	resp, err := c.R().
		SetHeader("Content-Type", "application/json").
		SetBody(reqBody).
		Post(ts.URL + "/orders")

	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}
	if resp.StatusCode() != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode())
	}

	records := handler.getRecords()
	if len(records) != 1 {
		t.Fatalf("expected 1 audit log record, got %d", len(records))
	}

	rec := records[0]
	if rec.Level != slog.LevelInfo {
		t.Errorf("expected LevelInfo, got %v", rec.Level)
	}
	if rec.Message != DefaultAuditMessage {
		t.Errorf("expected message %q, got %q", DefaultAuditMessage, rec.Message)
	}

	attrs := rec.Attrs
	if attrs["service_name"] != "order-service" {
		t.Errorf("expected service_name 'order-service', got %v", attrs["service_name"])
	}
	if attrs["method"] != "POST" {
		t.Errorf("expected method 'POST', got %v", attrs["method"])
	}
	if rec.getInt("status_code") != http.StatusOK {
		t.Errorf("expected status_code 200, got %v", attrs["status_code"])
	}
	if attrs["url"] != ts.URL+"/orders" {
		t.Errorf("expected url %q, got %v", ts.URL+"/orders", attrs["url"])
	}

	// Verify request body captured
	reqMap, ok := attrs["request_body"].(map[string]any)
	if !ok {
		t.Fatalf("expected request_body to be map[string]any, got %T: %v", attrs["request_body"], attrs["request_body"])
	}
	if reqMap["item"] != "laptop" {
		t.Errorf("expected item 'laptop', got %v", reqMap["item"])
	}

	// Verify response body captured
	respMap, ok := attrs["response_body"].(map[string]any)
	if !ok {
		t.Fatalf("expected response_body to be map[string]any, got %T: %v", attrs["response_body"], attrs["response_body"])
	}
	if respMap["status"] != "success" {
		t.Errorf("expected status 'success', got %v", respMap["status"])
	}
}

func TestWithAudit_MaskingSensitiveData(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"secret_jwt_token","profile":{"pin":"123456","name":"John"}}`))
	}))
	defer ts.Close()

	logger, handler := newTestLogger()
	c := New(
		WithAudit(AuditConfig{
			Logger:            logger,
			ServiceName:       "auth-service",
			LogRequestBodies:  true,
			LogResponseBodies: true,
			MaskKeywords:      []string{"custom_secret"},
		}),
	)

	reqPayload := `{"username":"john","password":"mySecretPassword!","custom_secret":"confidential"}`
	_, err := c.R().
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", "Bearer super_secret_auth_token").
		SetHeader("X-Api-Key", "api-key-12345").
		SetBody(reqPayload).
		Post(ts.URL + "/auth/login")

	if err != nil {
		t.Fatalf("unexpected request error: %v", err)
	}

	rec := handler.lastRecord()
	if rec == nil {
		t.Fatal("expected audit record, got nil")
	}

	// 1. Verify Request Body Masking
	reqMap, ok := rec.Attrs["request_body"].(map[string]any)
	if !ok {
		t.Fatalf("expected request_body to be map, got %T", rec.Attrs["request_body"])
	}
	if reqMap["password"] == "mySecretPassword!" {
		t.Errorf("expected password to be masked, got plaintext %v", reqMap["password"])
	}
	if reqMap["custom_secret"] == "confidential" {
		t.Errorf("expected custom_secret to be masked, got plaintext %v", reqMap["custom_secret"])
	}
	if reqMap["username"] != "john" {
		t.Errorf("expected non-sensitive username to remain 'john', got %v", reqMap["username"])
	}

	// 2. Verify Response Body Masking
	respMap, ok := rec.Attrs["response_body"].(map[string]any)
	if !ok {
		t.Fatalf("expected response_body to be map, got %T", rec.Attrs["response_body"])
	}
	if respMap["token"] == "secret_jwt_token" {
		t.Errorf("expected token in response to be masked, got %v", respMap["token"])
	}
	profile, ok := respMap["profile"].(map[string]any)
	if !ok {
		t.Fatalf("expected profile map, got %T", respMap["profile"])
	}
	if profile["pin"] == "123456" {
		t.Errorf("expected pin to be masked, got %v", profile["pin"])
	}
	if profile["name"] != "John" {
		t.Errorf("expected name to be 'John', got %v", profile["name"])
	}

	// 3. Verify Header Masking
	reqHeaders, ok := rec.Attrs["request_headers"].(map[string]any)
	if !ok {
		t.Fatalf("expected request_headers map, got %T", rec.Attrs["request_headers"])
	}
	if reqHeaders["Authorization"] == "Bearer super_secret_auth_token" {
		t.Errorf("expected Authorization header to be masked, got: %v", reqHeaders["Authorization"])
	}
	if reqHeaders["X-Api-Key"] == "api-key-12345" {
		t.Errorf("expected X-Api-Key header to be masked, got: %v", reqHeaders["X-Api-Key"])
	}
}

func TestWithAudit_ExcludeMultipartAndBinary(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/download") {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("\x00\x01\x02\x03\x04\x05binarydata"))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"uploaded":true}`))
	}))
	defer ts.Close()

	logger, handler := newTestLogger()
	c := New(
		WithAudit(AuditConfig{
			Logger:            logger,
			ServiceName:       "file-service",
			LogRequestBodies:  true,
			LogResponseBodies: true,
		}),
	)

	t.Run("Multipart upload request body is excluded", func(t *testing.T) {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		fw, err := w.CreateFormFile("file", "test.txt")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte("some file content"))
		_ = w.Close()

		_, err = c.R().
			SetHeader("Content-Type", w.FormDataContentType()).
			SetBody(b.Bytes()).
			Post(ts.URL + "/upload")
		if err != nil {
			t.Fatal(err)
		}

		rec := handler.lastRecord()
		if rec.Attrs["request_body"] != nil {
			t.Errorf("expected request_body to be nil for multipart, got %v", rec.Attrs["request_body"])
		}
	})

	t.Run("Binary response body is excluded", func(t *testing.T) {
		_, err := c.R().Get(ts.URL + "/download")
		if err != nil {
			t.Fatal(err)
		}

		rec := handler.lastRecord()
		if rec.Attrs["response_body"] != nil {
			t.Errorf("expected response_body to be nil for binary octet-stream, got %v", rec.Attrs["response_body"])
		}
	})
}

func TestWithAudit_BodySizeLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("R", 200)))
	}))
	defer ts.Close()

	logger, handler := newTestLogger()
	c := New(
		WithAudit(AuditConfig{
			Logger:                   logger,
			ServiceName:              "limit-service",
			LogRequestBodies:         true,
			LogResponseBodies:        true,
			RequestBodyLogLimitSize:  20,
			ResponseBodyLogLimitSize: 30,
		}),
	)

	largeReq := strings.Repeat("A", 100)
	_, err := c.R().
		SetHeader("Content-Type", "text/plain").
		SetBody(largeReq).
		Post(ts.URL + "/limit")
	if err != nil {
		t.Fatal(err)
	}

	rec := handler.lastRecord()
	reqStr, ok := rec.Attrs["request_body"].(string)
	if !ok {
		t.Fatalf("expected string request body, got %T", rec.Attrs["request_body"])
	}
	if len(reqStr) > 20 {
		t.Errorf("expected request_body length <= 20, got %d", len(reqStr))
	}

	respStr, ok := rec.Attrs["response_body"].(string)
	if !ok {
		t.Fatalf("expected string response body, got %T", rec.Attrs["response_body"])
	}
	if len(respStr) > 30 {
		t.Errorf("expected response_body length <= 30, got %d", len(respStr))
	}
}

func TestWithAudit_ContextExtractionAndHeaderFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	t.Run("Extracts automatically from nvx-go-helper/activity context", func(t *testing.T) {
		logger, handler := newTestLogger()
		c := New(
			WithAudit(AuditConfig{
				Logger:      logger,
				ServiceName: "context-service",
			}),
		)

		ctx := context.Background()
		ctx = activity.WithTransactionID(ctx, "trx-ctx-999")
		ctx = activity.WithRequestID(ctx, "req-ctx-888")
		ctx = activity.WithUserIP(ctx, "10.0.0.1")
		ctx = activity.WithUserIPOrigin(ctx, "203.0.113.1")
		ctx = activity.WithUserID(ctx, "123456")

		_, err := c.R().SetContext(ctx).Get(ts.URL + "/ctx")
		if err != nil {
			t.Fatal(err)
		}

		rec := handler.lastRecord()
		if rec.Attrs["transaction_id"] != "trx-ctx-999" {
			t.Errorf("expected transaction_id 'trx-ctx-999', got %v", rec.Attrs["transaction_id"])
		}
		if rec.Attrs["request_id"] != "req-ctx-888" {
			t.Errorf("expected request_id 'req-ctx-888', got %v", rec.Attrs["request_id"])
		}
		if rec.Attrs["user_ip"] != "10.0.0.1" {
			t.Errorf("expected user_ip '10.0.0.1', got %v", rec.Attrs["user_ip"])
		}
		if rec.Attrs["user_ip_origin"] != "203.0.113.1" {
			t.Errorf("expected user_ip_origin '203.0.113.1', got %v", rec.Attrs["user_ip_origin"])
		}
		if rec.Attrs["user_id"] != "123456" {
			t.Errorf("expected user_id '123456', got %v", rec.Attrs["user_id"])
		}
	})

	t.Run("Extracts from custom ContextAttrs hook when provided", func(t *testing.T) {
		logger, handler := newTestLogger()
		c := New(
			WithAudit(AuditConfig{
				Logger:      logger,
				ServiceName: "context-service",
				ContextAttrs: func(ctx context.Context) []slog.Attr {
					return []slog.Attr{
						slog.String("transaction_id", "trx-custom-999"),
						slog.String("request_id", "req-custom-888"),
						slog.String("user_ip", "10.0.0.1"),
						slog.String("user_ip_origin", "203.0.113.1"),
						slog.String("user_id", "123456"),
					}
				},
			}),
		)

		_, err := c.R().Get(ts.URL + "/ctx-custom")
		if err != nil {
			t.Fatal(err)
		}

		rec := handler.lastRecord()
		if rec.Attrs["transaction_id"] != "trx-custom-999" {
			t.Errorf("expected transaction_id 'trx-custom-999', got %v", rec.Attrs["transaction_id"])
		}
		if rec.Attrs["request_id"] != "req-custom-888" {
			t.Errorf("expected request_id 'req-custom-888', got %v", rec.Attrs["request_id"])
		}
		if rec.Attrs["user_ip"] != "10.0.0.1" {
			t.Errorf("expected user_ip '10.0.0.1', got %v", rec.Attrs["user_ip"])
		}
		if rec.Attrs["user_ip_origin"] != "203.0.113.1" {
			t.Errorf("expected user_ip_origin '203.0.113.1', got %v", rec.Attrs["user_ip_origin"])
		}
		if rec.Attrs["user_id"] != "123456" {
			t.Errorf("expected user_id '123456', got %v", rec.Attrs["user_id"])
		}
	})

	t.Run("Falls back to request headers when context is empty", func(t *testing.T) {
		logger, handler := newTestLogger()
		c := New(
			WithAudit(AuditConfig{
				Logger:      logger,
				ServiceName: "header-service",
			}),
		)

		_, err := c.R().
			SetHeader("X-Transaction-Id", "trx-header-111").
			SetHeader("X-Request-Id", "req-header-222").
			SetHeader("X-Forwarded-For", "192.168.1.50").
			SetHeader("X-Ip-Origin", "198.51.100.2").
			SetHeader("X-User-Id", "789012").
			Get(ts.URL + "/headers")
		if err != nil {
			t.Fatal(err)
		}

		rec := handler.lastRecord()
		if rec.Attrs["transaction_id"] != "trx-header-111" {
			t.Errorf("expected transaction_id 'trx-header-111', got %v", rec.Attrs["transaction_id"])
		}
		if rec.Attrs["request_id"] != "req-header-222" {
			t.Errorf("expected request_id 'req-header-222', got %v", rec.Attrs["request_id"])
		}
		if rec.Attrs["user_ip"] != "192.168.1.50" {
			t.Errorf("expected user_ip '192.168.1.50', got %v", rec.Attrs["user_ip"])
		}
		if rec.Attrs["user_ip_origin"] != "198.51.100.2" {
			t.Errorf("expected user_ip_origin '198.51.100.2', got %v", rec.Attrs["user_ip_origin"])
		}
		if rec.Attrs["user_id"] != "789012" {
			t.Errorf("expected user_id '789012', got %v", rec.Attrs["user_id"])
		}
	})

	t.Run("Falls back to OpenTelemetry trace ID when transaction_id header is missing", func(t *testing.T) {
		tp := sdktrace.NewTracerProvider()
		otel.SetTracerProvider(tp)
		tracer := tp.Tracer("test-tracer")

		ctx, span := tracer.Start(context.Background(), "test-span")
		defer span.End()

		logger, handler := newTestLogger()
		c := New(
			WithAudit(AuditConfig{
				Logger: logger,
			}),
		)

		_, err := c.R().SetContext(ctx).Get(ts.URL + "/otel")
		if err != nil {
			t.Fatal(err)
		}

		rec := handler.lastRecord()
		expectedTraceID := span.SpanContext().TraceID().String()
		if rec.Attrs["transaction_id"] != expectedTraceID {
			t.Errorf("expected transaction_id to match OTel trace ID %q, got %v", expectedTraceID, rec.Attrs["transaction_id"])
		}
	})

	t.Run("Generates fallback ID when RequestID is completely missing", func(t *testing.T) {
		logger, handler := newTestLogger()
		c := New(
			WithAudit(AuditConfig{
				Logger: logger,
				IDGenerator: func() string {
					return "custom-generated-id-123"
				},
			}),
		)

		_, err := c.R().Get(ts.URL + "/blank")
		if err != nil {
			t.Fatal(err)
		}

		rec := handler.lastRecord()
		if rec.Attrs["request_id"] != "custom-generated-id-123" {
			t.Errorf("expected generated request_id 'custom-generated-id-123', got %v", rec.Attrs["request_id"])
		}
	})
}

func TestWithAudit_NetworkErrorAndStatusLevels(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/warn") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/server-error") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	logger, handler := newTestLogger()
	c := New(
		WithTimeout(100*time.Millisecond),
		WithAudit(AuditConfig{
			Logger:      logger,
			ServiceName: "err-service",
		}),
	)

	t.Run("Status 4xx logs at LevelWarn", func(t *testing.T) {
		_, err := c.R().Get(ts.URL + "/warn")
		if err != nil {
			t.Fatal(err)
		}
		rec := handler.lastRecord()
		if rec.Level != slog.LevelWarn {
			t.Errorf("expected LevelWarn for 404, got %v", rec.Level)
		}
	})

	t.Run("Status 5xx logs at LevelError", func(t *testing.T) {
		_, err := c.R().Get(ts.URL + "/server-error")
		if err != nil {
			t.Fatal(err)
		}
		rec := handler.lastRecord()
		if rec.Level != slog.LevelError {
			t.Errorf("expected LevelError for 500, got %v", rec.Level)
		}
	})

	t.Run("Transport failure logs at LevelError with error attribute", func(t *testing.T) {
		_, err := c.R().Get("http://127.0.0.1:54321/dead-port")
		if err == nil {
			t.Fatal("expected network error, got nil")
		}

		rec := handler.lastRecord()
		if rec == nil {
			t.Fatal("expected audit record to be recorded on network error, got nil")
		}
		if rec.Level != slog.LevelError {
			t.Errorf("expected LevelError on transport failure, got %v", rec.Level)
		}
		if rec.Attrs["error"] == nil {
			t.Errorf("expected error attribute to be populated on network error")
		}
		if rec.getInt("status_code") != http.StatusBadGateway {
			t.Errorf("expected status_code 502 Bad Gateway on transport failure, got %v", rec.Attrs["status_code"])
		}
	})
}

func TestWithAudit_HeaderOmission(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	t.Run("HeadersToRemove completely excludes configured headers", func(t *testing.T) {
		logger, handler := newTestLogger()
		c := New(
			WithAudit(AuditConfig{
				Logger:          logger,
				ServiceName:     "test-svc",
				HeadersToRemove: []string{"X-Api-Key", "Authorization"},
			}),
		)

		_, err := c.R().
			SetHeader("X-Api-Key", "secret-key-999").
			SetHeader("Authorization", "Bearer mytoken").
			SetHeader("X-Safe-Header", "hello").
			Get(ts.URL + "/test")
		if err != nil {
			t.Fatal(err)
		}

		rec := handler.lastRecord()
		reqH, ok := rec.Attrs["request_headers"].(map[string]any)
		if !ok {
			t.Fatalf("expected request_headers map, got %T", rec.Attrs["request_headers"])
		}
		if _, exists := reqH["X-Api-Key"]; exists {
			t.Errorf("expected X-Api-Key to be completely removed, got: %v", reqH["X-Api-Key"])
		}
		if _, exists := reqH["Authorization"]; exists {
			t.Errorf("expected Authorization to be completely removed, got: %v", reqH["Authorization"])
		}
		if reqH["X-Safe-Header"] != "hello" {
			t.Errorf("expected X-Safe-Header to be present, got: %v", reqH["X-Safe-Header"])
		}
	})

	t.Run("OmitSensitiveHeaders completely drops all sensitive headers", func(t *testing.T) {
		logger, handler := newTestLogger()
		c := New(
			WithAudit(AuditConfig{
				Logger:               logger,
				ServiceName:          "test-svc",
				OmitSensitiveHeaders: true,
			}),
		)

		_, err := c.R().
			SetHeader("X-Api-Key", "secret-key-999").
			SetHeader("Authorization", "Bearer mytoken").
			SetHeader("X-Safe-Header", "hello").
			Get(ts.URL + "/test")
		if err != nil {
			t.Fatal(err)
		}

		rec := handler.lastRecord()
		reqH, ok := rec.Attrs["request_headers"].(map[string]any)
		if !ok {
			t.Fatalf("expected request_headers map, got %T", rec.Attrs["request_headers"])
		}
		if _, exists := reqH["X-Api-Key"]; exists {
			t.Errorf("expected X-Api-Key to be completely dropped, got: %v", reqH["X-Api-Key"])
		}
		if _, exists := reqH["Authorization"]; exists {
			t.Errorf("expected Authorization to be completely dropped, got: %v", reqH["Authorization"])
		}
		if reqH["X-Safe-Header"] != "hello" {
			t.Errorf("expected X-Safe-Header to be present, got: %v", reqH["X-Safe-Header"])
		}
	})
}

func TestWithAudit_NilSafetyAndHelpers(t *testing.T) {
	// Client without audit option
	c := New()
	if c.AuditConfig() != nil {
		t.Errorf("expected nil AuditConfig on client without WithAudit")
	}

	// Apply audit defaults with nil
	if applyAuditDefaults(nil) != nil {
		t.Errorf("expected nil for applyAuditDefaults(nil)")
	}

	// WithAudit with empty config defaults to slog.Default()
	cDefaultAudit := New(WithAudit(AuditConfig{}))
	if cDefaultAudit.AuditConfig().Logger != slog.Default() {
		t.Errorf("expected slog.Default() when Logger is not specified")
	}

	// Test helper functions with edge cases
	if !isMultipart("multipart/form-data; boundary=something") {
		t.Error("expected true for multipart")
	}
	if isMultipart("application/json") {
		t.Error("expected false for application/json")
	}
	if !isBinary("image/jpeg") || !isBinary("application/octet-stream") || !isBinary("application/pdf") {
		t.Error("expected true for binary types")
	}
	if isBinary("application/json") || isBinary("text/plain") {
		t.Error("expected false for text types")
	}

	// Empty body normalization
	if normalizeBodyRaw(nil, "application/json", nil, 100) != nil {
		t.Error("expected nil for empty body raw")
	}

	// Extract request body with form data
	r := c.R()
	r.SetFormData(map[string]string{"foo": "bar"})
	b, ct := extractRequestBody(r)
	if !strings.Contains(string(b), "foo=bar") || ct != "application/x-www-form-urlencoded" {
		t.Errorf("form data extraction mismatch: %s (%s)", b, ct)
	}

	// Extract request body with reader
	readerReq := c.R()
	readerReq.SetBody(strings.NewReader("reader data"))
	bReader, _ := extractRequestBody(readerReq)
	if string(bReader) != "reader data" {
		t.Errorf("reader extraction mismatch: %s", bReader)
	}

	// normalizeHeaders with empty
	emptyH := normalizeHeaders(nil, nil, nil, false)
	if len(emptyH) != 0 {
		t.Errorf("expected empty map, got %v", emptyH)
	}

	// normalizeHeaders with multi-value header
	multiH := http.Header{"X-Tags": []string{"tag1", "tag2"}}
	multiHMap := normalizeHeaders(multiH, nil, nil, false)
	tags, ok := multiHMap["X-Tags"].([]string)
	if !ok || len(tags) != 2 {
		t.Errorf("expected multi-value slice, got: %v", multiHMap["X-Tags"])
	}

	// extractResponseBody with nil
	if body, ct := extractResponseBody(nil); body != nil || ct != "" {
		t.Errorf("expected nil/empty for nil response, got %v, %q", body, ct)
	}

	// Nil safety on recordAuditLog and setupAuditHooks
	recordAuditLog(nil, nil, nil, nil)
	recordAuditLog(&AuditConfig{Logger: nil}, nil, nil, nil)
	setupAuditHooks(nil, nil)

	// URL and UserAgent resolution edge cases
	rawReq := c.R()
	rawReq.URL = "/foo"
	if resolveFullURL(rawReq) != "/foo" {
		t.Errorf("expected '/foo', got %q", resolveFullURL(rawReq))
	}
	emptyReq := c.R()
	emptyReq.URL = ""
	if resolveFullURL(emptyReq) != "" {
		t.Errorf("expected empty string, got %q", resolveFullURL(emptyReq))
	}

	// UserAgent resolution fallback
	if resolveUserAgent(c.R()) == "" {
		t.Errorf("expected default user agent")
	}

	// extractRequestBody with struct
	structReq := c.R()
	structReq.SetBody(map[string]int{"count": 42})
	structBody, structCT := extractRequestBody(structReq)
	if !strings.Contains(string(structBody), "42") || structCT != "application/json" {
		t.Errorf("expected struct to be marshaled to json: %s (%s)", structBody, structCT)
	}

	// extractRequestBody with NoBody
	noBodyReq := c.R()
	noBodyReq.SetBody(http.NoBody)
	noBody, _ := extractRequestBody(noBodyReq)
	if noBody != nil {
		t.Errorf("expected nil for NoBody, got %v", noBody)
	}

	// UserAgent from RawRequest
	reqWithRaw := c.R()
	reqWithRaw.RawRequest = httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	reqWithRaw.RawRequest.Header.Set("User-Agent", "custom-agent-999")
	if ua := resolveUserAgent(reqWithRaw); ua != "custom-agent-999" {
		t.Errorf("expected custom-agent-999, got %q", ua)
	}

	// AuditLog struct instantiation verification
	_ = AuditLog{
		ID:          "test-id",
		Method:      "GET",
		FullURL:     "https://example.com",
		StatusCode:  200,
		ServiceName: "test",
		CreatedAt:   time.Now().UTC(),
	}
}
