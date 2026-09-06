package client

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Jkenyut/nvx-go-helper/activity"
	"github.com/Jkenyut/nvx-go-helper/cryptoutil"
	"github.com/Jkenyut/nvx-go-helper/format"
	"github.com/bytedance/sonic"
	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/otel/trace"
)

// Default limits and values for request and response body logging.
const (
	// DefaultRequestBodyLogLimitSize is 3 MB (3,145,728 bytes).
	DefaultRequestBodyLogLimitSize int64 = 3 * 1024 * 1024
	// DefaultResponseBodyLogLimitSize is 5 MB (5,242,880 bytes).
	DefaultResponseBodyLogLimitSize int64 = 5 * 1024 * 1024
	// DefaultAuditMessage is the standard message string for audit log records.
	DefaultAuditMessage = "outbound HTTP request"
)

// defaultMaskKeywords defines common sensitive fields that are always masked in headers and bodies.
var defaultMaskKeywords = []string{
	// Authentication & Base Secrets
	"password", "password_cbo", "passphrase", "secret", "client_secret", "client_secret_encrypted",
	"token", "access_token", "refresh_token", "id_token", "jwt",
	"apikey", "api_key", "x-api-key", "client_id", "authorization",
	"cookie", "set-cookie",

	// Session & OTP
	"session_id", "session_token", "auth_code", "verification_code", "otp",

	// PIN & Pass Numbers
	"pin", "mpin", "transaction_pin", "encrypted_pin_number",
	"pass_number", "pass_number_cbo", "encrypted_pass_number", "encrypted_pass_number_cbo",
	"pass_number_of_account", "encrypted_pass_number_of_account",

	// Signatures & Hashes
	"hash", "checksum", "signature", "signature_hash", "private_key", "tls_key", "certificate_key",

	// Accounts & Cards
	"account_number_encrypted", "account_number_cbo", "account_number_encrypted_cbo",
	"card_number", "card_number_cbo", "encrypted_card_number", "encrypted_card_number_cbo",
	"credit_card_number", "credit_card_number_cbo", "encrypted_credit_card_number", "encrypted_credit_card_number_cbo",
	"cvv", "cvc", "cvv2", "cvc2",

	// PII & Identification
	"nik", "ktp", "ssn", "national_id", "id_card_number", "npwp", "tax_id",
	"mother_maiden_name", "dob", "date_of_birth",
	"phone", "phone_number", "mobile_number", "email", "email_address",

	// General Encrypted Data
	"encrypted_data",
}

// HeaderKeys defines header names extracted into the audit log when context lacks them.
type HeaderKeys struct {
	RequestID     string
	TransactionID string
	IP            string
	IPOrigin      string
	UserID        string
}

// DefaultHeaderKeys returns the standard header names.
func DefaultHeaderKeys() HeaderKeys {
	return HeaderKeys{
		RequestID:     "X-Request-Id",
		TransactionID: "X-Transaction-Id",
		IP:            "X-Forwarded-For",
		IPOrigin:      "X-Ip-Origin",
		UserID:        "X-User-Id",
	}
}

// AuditLog represents a structured outbound HTTP audit log entry.
// It can be used by custom slog.Handler implementations, formatters, or downstream services.
type AuditLog struct {
	ID              string         `json:"id,omitempty"`
	Method          string         `json:"method"`
	FullURL         string         `json:"full_url"`
	StatusCode      int            `json:"status_code"`
	LatencyMS       int64          `json:"latency_ms"`
	IP              string         `json:"ip,omitempty"`
	IPOrigin        string         `json:"ip_origin,omitempty"`
	RequestID       string         `json:"request_id,omitempty"`
	TransactionID   string         `json:"transaction_id,omitempty"`
	RequestHeaders  map[string]any `json:"request_headers,omitempty"`
	ResponseHeaders map[string]any `json:"response_headers,omitempty"`
	RequestBody     any            `json:"request_body,omitempty"`
	ResponseBody    any            `json:"response_body,omitempty"`
	CreatedBy       string         `json:"created_by,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	Protocol        string         `json:"protocol"`
	ServiceName     string         `json:"service_name,omitempty"`
	UserAgent       string         `json:"user_agent"`
	ErrorMessage    string         `json:"error_message,omitempty"`
}

// AuditConfig holds configuration for outbound request and response audit logging via log/slog.
type AuditConfig struct {
	// Logger is the slog.Logger used to emit audit records.
	// If nil, slog.Default() is used.
	Logger *slog.Logger

	// Level specifies the slog.Level for successful requests (< 400).
	// Default: slog.LevelInfo.
	Level slog.Level

	// ErrorLevel specifies the slog.Level for failed requests (>= 400 or network errors).
	// Default: slog.LevelError.
	ErrorLevel slog.Level

	// Message specifies the log message string.
	// Default: "outbound HTTP request".
	Message string

	// ServiceName identifies the microservice emitting this log.
	ServiceName string

	// LogRequestBodies determines whether request bodies are logged. Default: false.
	LogRequestBodies bool

	// LogResponseBodies determines whether response bodies are logged. Default: false.
	LogResponseBodies bool

	// RequestBodyLogLimitSize is the maximum size (in bytes) of the request body to log. Default: 3MB.
	RequestBodyLogLimitSize int64

	// ResponseBodyLogLimitSize is the maximum size (in bytes) of the response body to log. Default: 5MB.
	ResponseBodyLogLimitSize int64

	// MaskKeywords specifies additional keywords to mask in headers and JSON/text bodies.
	MaskKeywords []string

	// Headers defines custom header keys for fallback extraction.
	Headers HeaderKeys

	// HeadersToRemove specifies header names that must be completely omitted from audit log headers.
	// Headers matching this list (case-insensitive) will NOT be printed at all in request_headers or response_headers.
	HeadersToRemove []string

	// OmitSensitiveHeaders, when true, completely drops sensitive headers (like X-Api-Key, Authorization)
	// from the audit log headers rather than masking their values with "******". Default: false (masking).
	OmitSensitiveHeaders bool

	// ContextAttrs is an optional hook to extract custom slog.Attr attributes from context.Context.
	// If nil, context attributes are automatically extracted via activity.ToSlogAttrs(ctx)
	// with fallback to request headers and OpenTelemetry SpanContext.
	ContextAttrs func(ctx context.Context) []slog.Attr

	// IDGenerator generates unique identifiers (e.g. for request ID fallback).
	// If nil, defaults to UUID v7.
	IDGenerator func() string
}

func applyAuditDefaults(cfg *AuditConfig) *AuditConfig {
	if cfg == nil {
		return nil
	}
	cloned := *cfg

	if cloned.Logger == nil {
		cloned.Logger = slog.Default()
	}
	if cloned.Message == "" {
		cloned.Message = DefaultAuditMessage
	}
	if cloned.ErrorLevel == 0 && cloned.Level >= 0 {
		cloned.ErrorLevel = slog.LevelError
	}
	if cloned.IDGenerator == nil {
		cloned.IDGenerator = cryptoutil.V7
	}

	if cloned.RequestBodyLogLimitSize <= 0 {
		cloned.RequestBodyLogLimitSize = DefaultRequestBodyLogLimitSize
	}
	if cloned.ResponseBodyLogLimitSize <= 0 {
		cloned.ResponseBodyLogLimitSize = DefaultResponseBodyLogLimitSize
	}

	// Merge default keywords with custom keywords
	mergedKeywords := slices.Clone(defaultMaskKeywords)
	for _, k := range cloned.MaskKeywords {
		kTrim := strings.TrimSpace(k)
		if kTrim != "" && !slices.Contains(mergedKeywords, strings.ToLower(kTrim)) {
			mergedKeywords = append(mergedKeywords, strings.ToLower(kTrim))
		}
	}
	cloned.MaskKeywords = mergedKeywords

	// Fill default header keys if not specified
	defHeaders := DefaultHeaderKeys()
	if cloned.Headers.RequestID == "" {
		cloned.Headers.RequestID = defHeaders.RequestID
	}
	if cloned.Headers.TransactionID == "" {
		cloned.Headers.TransactionID = defHeaders.TransactionID
	}
	if cloned.Headers.IP == "" {
		cloned.Headers.IP = defHeaders.IP
	}
	if cloned.Headers.IPOrigin == "" {
		cloned.Headers.IPOrigin = defHeaders.IPOrigin
	}
	if cloned.Headers.UserID == "" {
		cloned.Headers.UserID = defHeaders.UserID
	}

	return &cloned
}

type (
	auditStartTimeKey struct{}
	auditRecordedKey  struct{}
)

// setupAuditHooks registers Resty hooks to construct and record audit log entries via slog.
func setupAuditHooks(r *resty.Client, cfg *AuditConfig) {
	if cfg == nil || cfg.Logger == nil {
		return
	}

	// 1. Record start time before request dispatch
	r.OnBeforeRequest(func(_ *resty.Client, req *resty.Request) error {
		ctx := req.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		var once sync.Once
		ctx = context.WithValue(ctx, auditRecordedKey{}, &once)
		ctx = context.WithValue(ctx, auditStartTimeKey{}, time.Now())
		req.SetContext(ctx)
		return nil
	})

	// 2. Handle successful response receipt (including 4xx/5xx status codes)
	r.OnAfterResponse(func(_ *resty.Client, resp *resty.Response) error {
		recordAuditLog(cfg, resp.Request, resp, nil)
		return nil
	})

	// 3. Handle transport / network errors where OnAfterResponse is not invoked
	r.OnError(func(req *resty.Request, err error) {
		recordAuditLog(cfg, req, nil, err)
	})
}

// recordAuditLog constructs and logs structured slog attributes safely.
func recordAuditLog(cfg *AuditConfig, req *resty.Request, resp *resty.Response, reqErr error) {
	if cfg == nil || cfg.Logger == nil || req == nil {
		return
	}

	ctx := req.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// Ensure each request is only audited once (prevents duplicate if OnError and OnAfterResponse overlap)
	if once, ok := ctx.Value(auditRecordedKey{}).(*sync.Once); ok && once != nil {
		executed := false
		once.Do(func() {
			executed = true
		})
		if !executed {
			return
		}
	}

	startTime := time.Now()
	if t, ok := ctx.Value(auditStartTimeKey{}).(time.Time); ok {
		startTime = t
	}

	var reqHeader http.Header
	if req.Header != nil {
		reqHeader = req.Header
	} else if req.RawRequest != nil {
		reqHeader = req.RawRequest.Header
	}

	var (
		statusCode   int
		latencyMS    int64
		protocol     = "HTTP/1.1"
		errorMessage string
	)

	if resp != nil {
		statusCode = resp.StatusCode()
		latencyMS = resp.Time().Milliseconds()
		if resp.RawResponse != nil && resp.RawResponse.Proto != "" {
			protocol = resp.RawResponse.Proto
		}
	} else {
		latencyMS = time.Since(startTime).Milliseconds()
		if reqErr != nil {
			errorMessage = reqErr.Error()
			statusCode = http.StatusBadGateway
		}
	}

	// Determine log level
	level := cfg.Level
	if reqErr != nil || statusCode >= 500 {
		level = cfg.ErrorLevel
	} else if statusCode >= 400 {
		level = slog.LevelWarn
	}

	// Fast path: if logger is disabled for this level, exit immediately before heavy body/header processing
	if !cfg.Logger.Enabled(ctx, level) {
		return
	}

	var respHeaders map[string]any
	var respBodyRaw any
	if resp != nil {
		respHeaders = normalizeHeaders(resp.Header(), cfg.MaskKeywords, cfg.HeadersToRemove, cfg.OmitSensitiveHeaders)
		if cfg.LogResponseBodies {
			rawBody, ct := extractResponseBody(resp)
			respBodyRaw = normalizeBodyRaw(rawBody, ct, cfg.MaskKeywords, cfg.ResponseBodyLogLimitSize)
		}
	} else {
		respHeaders = make(map[string]any)
	}

	var reqBodyRaw any
	if cfg.LogRequestBodies {
		rawBody, ct := extractRequestBody(req)
		reqBodyRaw = normalizeBodyRaw(rawBody, ct, cfg.MaskKeywords, cfg.RequestBodyLogLimitSize)
	}

	// Build slog attributes
	attrs := make([]slog.Attr, 0, 16)
	if cfg.ServiceName != "" {
		attrs = append(attrs, slog.String("service_name", cfg.ServiceName))
	}
	attrs = append(attrs,
		slog.String("method", req.Method),
		slog.String("url", resolveFullURL(req)),
		slog.Int("status_code", statusCode),
		slog.Int64("latency_ms", latencyMS),
		slog.String("protocol", protocol),
		slog.String("user_agent", resolveUserAgent(req)),
	)

	// Context and tracing attributes (nvx-go-helper/activity with header and OTel fallback)
	contextAttrs := extractContextAttrs(ctx, reqHeader, &cfg.Headers, cfg.ContextAttrs, cfg.IDGenerator)
	attrs = append(attrs, contextAttrs...)

	// Headers
	attrs = append(attrs,
		slog.Any("request_headers", normalizeHeaders(reqHeader, cfg.MaskKeywords, cfg.HeadersToRemove, cfg.OmitSensitiveHeaders)),
		slog.Any("response_headers", respHeaders),
	)

	// Bodies (if enabled)
	if cfg.LogRequestBodies && reqBodyRaw != nil {
		attrs = append(attrs, slog.Any("request_body", reqBodyRaw))
	}
	if cfg.LogResponseBodies && respBodyRaw != nil {
		attrs = append(attrs, slog.Any("response_body", respBodyRaw))
	}

	if errorMessage != "" {
		attrs = append(attrs, slog.String("error", errorMessage))
	}

	cfg.Logger.LogAttrs(ctx, level, cfg.Message, attrs...)
}

func extractContextAttrs(ctx context.Context, header http.Header, keys *HeaderKeys, customExtractor func(ctx context.Context) []slog.Attr, idGen func() string) []slog.Attr {
	if customExtractor != nil {
		if attrs := customExtractor(ctx); len(attrs) > 0 {
			return attrs
		}
	}

	// 1. Extract standard attributes using nvx-go-helper/activity
	actAttrs := activity.ToSlogAttrs(ctx)

	hasTrx := false
	hasReq := false
	hasIP := false
	hasIPOrigin := false
	hasUser := false

	for _, a := range actAttrs {
		switch a.Key {
		case "transaction_id":
			hasTrx = true
		case "request_id":
			hasReq = true
		case "user_ip":
			hasIP = true
		case "user_ip_origin":
			hasIPOrigin = true
		case "user_id":
			hasUser = true
		}
	}

	attrs := make([]slog.Attr, len(actAttrs), len(actAttrs)+5)
	copy(attrs, actAttrs)

	if keys == nil {
		def := DefaultHeaderKeys()
		keys = &def
	}

	// 2. Fall back to headers and OTel SpanContext if missing
	if !hasTrx {
		var trxID string
		if header != nil {
			trxID = header.Get(keys.TransactionID)
		}
		if trxID == "" {
			span := trace.SpanFromContext(ctx)
			if span != nil && span.SpanContext().IsValid() {
				trxID = span.SpanContext().TraceID().String()
			}
		}
		if trxID != "" {
			attrs = append(attrs, slog.String("transaction_id", trxID))
		}
	}

	if !hasReq {
		var reqID string
		if header != nil {
			reqID = header.Get(keys.RequestID)
		}
		if reqID == "" && idGen != nil {
			reqID = idGen()
		}
		if reqID != "" {
			attrs = append(attrs, slog.String("request_id", reqID))
		}
	}

	if header != nil {
		if !hasIP {
			if ip := header.Get(keys.IP); ip != "" {
				attrs = append(attrs, slog.String("user_ip", ip))
			}
		}
		if !hasIPOrigin {
			if ipOrigin := header.Get(keys.IPOrigin); ipOrigin != "" {
				attrs = append(attrs, slog.String("user_ip_origin", ipOrigin))
			}
		}
		if !hasUser {
			if uidStr := header.Get(keys.UserID); uidStr != "" {
				attrs = append(attrs, slog.String("user_id", uidStr))
			}
		}
	}

	return attrs
}

func isMultipart(contentType string) bool {
	ct := strings.TrimSpace(contentType)
	return len(ct) >= 10 && strings.EqualFold(ct[:10], "multipart/")
}

func isBinary(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(ct, "application/octet-stream") ||
		strings.HasPrefix(ct, "image/") ||
		strings.HasPrefix(ct, "audio/") ||
		strings.HasPrefix(ct, "video/") ||
		strings.HasPrefix(ct, "application/pdf") ||
		strings.HasPrefix(ct, "application/zip") ||
		strings.HasPrefix(ct, "application/gzip")
}

func extractRequestBody(req *resty.Request) (body []byte, contentType string) {
	ct := req.Header.Get("Content-Type")
	if ct == "" && req.RawRequest != nil && req.RawRequest.Header != nil {
		ct = req.RawRequest.Header.Get("Content-Type")
	}

	if isMultipart(ct) {
		return nil, "multipart/form-data"
	}

	if req.Body == nil || req.Body == http.NoBody {
		if len(req.FormData) > 0 {
			return []byte(req.FormData.Encode()), "application/x-www-form-urlencoded"
		}
		return nil, ct
	}

	switch b := req.Body.(type) {
	case []byte:
		return b, ct
	case string:
		return []byte(b), ct
	case io.Reader:
		if rs, ok := b.(io.ReadSeeker); ok {
			data, err := io.ReadAll(rs)
			if err == nil {
				_, _ = rs.Seek(0, io.SeekStart)
				return data, ct
			}
		}
		return nil, ct
	default:
		data, err := sonic.ConfigDefault.Marshal(req.Body)
		if err == nil {
			if ct == "" {
				ct = "application/json"
			}
			return data, ct
		}
		return nil, ct
	}
}

func extractResponseBody(resp *resty.Response) (body []byte, contentType string) {
	if resp == nil {
		return nil, ""
	}
	ct := resp.Header().Get("Content-Type")
	return resp.Body(), ct
}

func normalizeBodyRaw(raw []byte, contentType string, keywordList []string, limit int64) any {
	if len(raw) == 0 {
		return nil
	}
	if isMultipart(contentType) || isBinary(contentType) {
		return nil
	}
	if limit > 0 && int64(len(raw)) > limit {
		raw = raw[:limit]
	}

	str := string(raw)
	if len(keywordList) > 0 {
		str = format.MaskAfterKeywords(str, keywordList, "*")
	}

	var v any
	if err := sonic.ConfigDefault.UnmarshalFromString(str, &v); err == nil {
		return v
	}
	return str
}

func normalizeHeaders(h http.Header, keywordList, headersToRemove []string, omitSensitive bool) map[string]any {
	if len(h) == 0 {
		return make(map[string]any)
	}
	out := make(map[string]any, len(h))
	for k, v := range h {
		// 1. Completely omit headers specified in HeadersToRemove
		if slices.ContainsFunc(headersToRemove, func(target string) bool {
			return strings.EqualFold(target, k)
		}) {
			continue
		}

		// 2. Handle sensitive headers
		if isSensitiveHeader(k, keywordList) {
			if omitSensitive {
				continue
			}
			out[k] = "******"
			continue
		}

		if len(v) == 1 {
			out[k] = v[0]
		} else {
			out[k] = v
		}
	}
	return out
}

func isSensitiveHeader(name string, keywords []string) bool {
	if len(keywords) == 0 {
		keywords = defaultMaskKeywords
	}
	lower := strings.ToLower(name)
	for _, kw := range keywords {
		kwClean := strings.ToLower(strings.TrimSpace(kw))
		if kwClean != "" && strings.Contains(lower, kwClean) {
			return true
		}
	}
	return false
}

func resolveFullURL(req *resty.Request) string {
	if req.RawRequest != nil && req.RawRequest.URL != nil {
		if u := req.RawRequest.URL.String(); u != "" {
			return u
		}
	}
	if req.URL != "" {
		return req.URL
	}
	return ""
}

func resolveUserAgent(req *resty.Request) string {
	if req.Header != nil {
		if ua := req.Header.Get("User-Agent"); ua != "" {
			return ua
		}
	}
	if req.RawRequest != nil && req.RawRequest.Header != nil {
		if ua := req.RawRequest.Header.Get("User-Agent"); ua != "" {
			return ua
		}
	}
	return "nvx-go-client"
}
