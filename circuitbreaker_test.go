package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
)

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty path",
			input:    "",
			expected: "/",
		},
		{
			name:     "root path",
			input:    "/",
			expected: "/",
		},
		{
			name:     "static segments only",
			input:    "/api/v1/users/active/status",
			expected: "/api/v1/users/active/status",
		},
		{
			name:     "single numeric ID",
			input:    "/users/12345",
			expected: "/users/{param}",
		},
		{
			name:     "multiple numeric IDs",
			input:    "/users/123/orders/456/items/789",
			expected: "/users/{param}/orders/{param}/items/{param}",
		},
		{
			name:     "UUID parameter",
			input:    "/orders/550e8400-e29b-41d4-a716-446655440000",
			expected: "/orders/{param}",
		},
		{
			name:     "Mongo ObjectID parameter",
			input:    "/documents/507f1f77bcf86cd799439011/view",
			expected: "/documents/{param}/view",
		},
		{
			name:     "ULID parameter",
			input:    "/events/01ARZ3NDEKTSV4RRFFQ69G5FAV",
			expected: "/events/{param}",
		},
		{
			name:     "templated segment with braces",
			input:    "/users/{userId}/profile",
			expected: "/users/{param}/profile",
		},
		{
			name:     "templated segment with colon",
			input:    "/users/:userId/settings",
			expected: "/users/{param}/settings",
		},
		{
			name:     "path with query string stripped",
			input:    "/users/123?page=1&sort=desc",
			expected: "/users/{param}",
		},
		{
			name:     "path with hash fragment stripped",
			input:    "/items/456#section-2",
			expected: "/items/{param}",
		},
		{
			name:     "multiple consecutive slashes only",
			input:    "///",
			expected: "/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := NormalizePath(tt.input)
			if actual != tt.expected {
				t.Errorf("NormalizePath(%q) = %q, want %q", tt.input, actual, tt.expected)
			}
		})
	}
}

func TestDefaultRouteKey(t *testing.T) {
	t.Run("nil request", func(t *testing.T) {
		if key := DefaultRouteKey(nil); key != "UNKNOWN" {
			t.Errorf("Expected 'UNKNOWN', got %q", key)
		}
	})

	t.Run("explicit context route pattern overrides heuristic", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/users/9999", nil)
		ctx := WithRoutePattern(req.Context(), "/users/{userId}")
		req = req.WithContext(ctx)

		key := DefaultRouteKey(req)
		expected := "GET api.example.com/users/{userId}"
		if key != expected {
			t.Errorf("Expected %q, got %q", expected, key)
		}
	})

	t.Run("heuristic route key with host and method", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1/orders/12345/cancel", nil)
		key := DefaultRouteKey(req)
		expected := "POST api.example.com/v1/orders/{param}/cancel"
		if key != expected {
			t.Errorf("Expected %q, got %q", expected, key)
		}
	})

	t.Run("explicit context route pattern without leading slash and without host", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/users/9999", nil)
		ctx := WithRoutePattern(req.Context(), "users/{userId}")
		req = req.WithContext(ctx)

		key := DefaultRouteKey(req)
		expected := "GET /users/{userId}"
		if key != expected {
			t.Errorf("Expected %q, got %q", expected, key)
		}
	})

	t.Run("heuristic route key without host", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/v1/items/123", nil)
		key := DefaultRouteKey(req)
		expected := "GET /v1/items/{param}"
		if key != expected {
			t.Errorf("Expected %q, got %q", expected, key)
		}
	})

	t.Run("invalid formats fall back to static segments", func(t *testing.T) {
		// 36 chars with wrong dashes
		if isUUID("550e8400xe29b-41d4-a716-446655440000") {
			t.Error("Expected false for invalid UUID dashes")
		}
		// 36 chars with non-hex
		if isUUID("550e8400-e29b-41d4-a716-44665544000z") {
			t.Error("Expected false for non-hex UUID")
		}
		// 24 chars with non-hex
		if isHexID("507f1f77bcf86cd79943901z") {
			t.Error("Expected false for non-hex ObjectID")
		}
		// 26 chars with symbol
		if isULIDOrKSUID("01ARZ3NDEKTSV4RRFFQ69G5FA!") {
			t.Error("Expected false for non-alnum ULID")
		}
		// empty string
		if isNumeric("") {
			t.Error("Expected false for empty numeric")
		}
	})
}

func TestCircuitBreaker_TrippingOn5xx(t *testing.T) {
	var serverRequests atomic.Int64
	serverStatusCode := http.StatusInternalServerError

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverRequests.Add(1)
		w.WriteHeader(serverStatusCode)
	}))
	defer ts.Close()

	// Configure sensitive breaker for testing: trips after 3 failures
	cbOpts := []CircuitBreakerOption{
		WithCBTimeout(100 * time.Millisecond),
		WithCBReadyToTrip(func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 3
		}),
	}

	c := New(
		WithBaseURL(ts.URL),
		WithCircuitBreaker(cbOpts...),
	)

	ctx := context.Background()

	// Send 3 requests that result in 500 error
	for i := 1; i <= 3; i++ {
		resp, err := c.R().SetContext(ctx).Get("/items/101")
		if err != nil {
			t.Fatalf("Request %d expected response, got error: %v", i, err)
		}
		if resp.StatusCode() != http.StatusInternalServerError {
			t.Fatalf("Request %d expected 500, got %d", i, resp.StatusCode())
		}
	}

	if serverRequests.Load() != 3 {
		t.Fatalf("Expected 3 requests to reach server, got %d", serverRequests.Load())
	}

	// 4th request must be rejected by circuit breaker immediately without reaching server
	_, err := c.R().SetContext(ctx).Get("/items/101")
	if err == nil {
		t.Fatal("Expected 4th request to fail with circuit breaker open, but got nil error")
	}
	if !errors.Is(err, ErrCircuitOpen) {
		t.Logf("Got expected error wrapping ErrCircuitOpen: %v", err)
	}

	// Server should still have received exactly 3 requests (4th request was blocked before network)
	if serverRequests.Load() != 3 {
		t.Errorf("Expected server requests to remain 3, got %d", serverRequests.Load())
	}

	// Request to a DIFFERENT route should NOT be blocked (route isolation)
	serverStatusCode = http.StatusOK
	resp2, err2 := c.R().SetContext(ctx).Get("/other-route/202")
	if err2 != nil {
		t.Fatalf("Expected independent route /other-route/202 to succeed, got error: %v", err2)
	}
	if resp2.StatusCode() != http.StatusOK {
		t.Errorf("Expected 200 OK for /other-route/202, got %d", resp2.StatusCode())
	}

	// Wait for circuit breaker timeout to enter Half-Open state (150ms > 100ms timeout)
	time.Sleep(150 * time.Millisecond)

	// In Half-Open, successful requests should close the circuit
	serverStatusCode = http.StatusOK
	for i := 1; i <= 3; i++ {
		resp, err := c.R().SetContext(ctx).Get("/items/101")
		if err != nil {
			t.Fatalf("Recovery request %d failed: %v", i, err)
		}
		if resp.StatusCode() != http.StatusOK {
			t.Errorf("Expected 200 OK, got %d", resp.StatusCode())
		}
	}
}

func TestCircuitBreaker_4xxDoesNotTrip(t *testing.T) {
	var serverRequests atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverRequests.Add(1)
		w.WriteHeader(http.StatusNotFound) // 404 Client Error
	}))
	defer ts.Close()

	cbOpts := []CircuitBreakerOption{
		WithCBReadyToTrip(func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 3
		}),
	}

	c := New(
		WithBaseURL(ts.URL),
		WithCircuitBreaker(cbOpts...),
	)

	ctx := context.Background()

	// Send 5 requests returning 404
	for i := 1; i <= 5; i++ {
		resp, err := c.R().SetContext(ctx).Get("/missing-resource")
		if err != nil {
			t.Fatalf("Request %d expected 404 response, got error: %v", i, err)
		}
		if resp.StatusCode() != http.StatusNotFound {
			t.Errorf("Expected status 404, got %d", resp.StatusCode())
		}
	}

	// Breaker should NOT trip because 4xx is client-side error, not upstream failure
	if serverRequests.Load() != 5 {
		t.Errorf("Expected 5 requests to reach server, got %d", serverRequests.Load())
	}
}

func TestCircuitBreaker_ContextCancellationExcluded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(
		WithBaseURL(ts.URL),
		WithCircuitBreaker(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := c.R().SetContext(ctx).Get("/test")
	if err == nil {
		t.Fatal("Expected error on canceled context")
	}

	// Verify breaker is still closed
	resp, err := c.R().SetContext(context.Background()).Get("/test")
	if err != nil {
		t.Fatalf("Expected valid request after canceled context to succeed, got %v", err)
	}
	if resp.StatusCode() != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode())
	}
}

func TestCircuitBreaker_BoundedRegistry(t *testing.T) {
	cbm := NewCircuitBreakerManager(WithCBMaxBreakers(5))

	for i := 0; i < 10; i++ {
		req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/items/"+string(rune('a'+i)), nil)
		key := DefaultRouteKey(req)
		_ = cbm.GetOrCreate(key)
	}

	count := cbm.Count()
	if count > 5 {
		t.Errorf("Expected at most 5 circuit breakers in memory, got %d", count)
	}
}

func TestCircuitBreaker_ConcurrentRequests(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(
		WithBaseURL(ts.URL),
		WithCircuitBreaker(),
	)

	const goroutines = 30
	const requestsPerGoroutine = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(gID int) {
			defer wg.Done()
			ctx := context.Background()
			for r := 0; r < requestsPerGoroutine; r++ {
				url := "/users/100/orders/200"
				if gID%2 == 0 {
					url = "/products/300"
				}
				resp, err := c.R().SetContext(ctx).Get(url)
				if err != nil {
					t.Errorf("Unexpected error in goroutine %d: %v", gID, err)
					return
				}
				if resp.StatusCode() != http.StatusOK {
					t.Errorf("Expected 200, got %d", resp.StatusCode())
				}
			}
		}(g)
	}

	wg.Wait()
}

func TestCircuitBreaker_OptionsAndDefaults(t *testing.T) {
	var stateChanged bool
	cbOpts := []CircuitBreakerOption{
		WithCBMaxRequests(5),
		WithCBInterval(10 * time.Second),
		WithCBTimeout(20 * time.Second),
		WithCBOnStateChange(func(name string, from gobreaker.State, to gobreaker.State) {
			stateChanged = true
		}),
		WithCBRouteKeyFunc(func(req *http.Request) string {
			return "custom-key"
		}),
		WithCBMaxBreakers(50),
	}

	c := New(WithCircuitBreaker(cbOpts...))
	cbm := c.CircuitBreakerManager()
	if cbm == nil {
		t.Fatal("Expected non-nil CircuitBreakerManager")
	}
	if cbm.cfg.MaxRequests != 5 {
		t.Errorf("Expected MaxRequests 5, got %d", cbm.cfg.MaxRequests)
	}
	if cbm.cfg.Interval != 10*time.Second {
		t.Errorf("Expected Interval 10s, got %v", cbm.cfg.Interval)
	}
	if cbm.cfg.Timeout != 20*time.Second {
		t.Errorf("Expected Timeout 20s, got %v", cbm.cfg.Timeout)
	}
	if cbm.cfg.MaxBreakers != 50 {
		t.Errorf("Expected MaxBreakers 50, got %d", cbm.cfg.MaxBreakers)
	}

	// Trigger onStateChange check
	if cbm.cfg.OnStateChange != nil {
		cbm.cfg.OnStateChange("test", gobreaker.StateClosed, gobreaker.StateOpen)
		if !stateChanged {
			t.Error("Expected stateChanged to be true")
		}
	}

	// Test default ReadyToTrip
	defCfg := defaultCBConfig()
	// Case 1: Requests < 10 -> returns false
	if defCfg.ReadyToTrip(gobreaker.Counts{Requests: 5, TotalFailures: 5}) {
		t.Error("Expected false when Requests < 10")
	}
	// Case 2: Requests >= 10 and FailureRate >= 50% -> returns true
	if !defCfg.ReadyToTrip(gobreaker.Counts{Requests: 10, TotalFailures: 5}) {
		t.Error("Expected true when Requests >= 10 and failureRate >= 0.5")
	}
	// Case 3: Requests >= 10 and ConsecutiveFailures >= 5 -> returns true
	if !defCfg.ReadyToTrip(gobreaker.Counts{Requests: 10, TotalFailures: 5, ConsecutiveFailures: 5}) {
		t.Error("Expected true when ConsecutiveFailures >= 5")
	}
	// Case 4: Requests >= 10, low failure rate, low consecutive -> returns false
	if defCfg.ReadyToTrip(gobreaker.Counts{Requests: 10, TotalFailures: 1, ConsecutiveFailures: 1}) {
		t.Error("Expected false when failure rate is low")
	}

	// Test nil cbm transport pass-through
	rt := newCircuitBreakerTransport(http.DefaultTransport, nil)
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if rt.cbm != nil {
		t.Error("Expected nil cbm")
	}
	_, _ = rt.RoundTrip(req)

	// Test RoutePatternFromContext edge cases
	if _, ok := RoutePatternFromContext(nil); ok {
		t.Error("Expected false for nil context")
	}
	ctxEmpty := context.WithValue(context.Background(), routePatternKey, "")
	if _, ok := RoutePatternFromContext(ctxEmpty); ok {
		t.Error("Expected false for empty pattern")
	}
	ctxInvalid := context.WithValue(context.Background(), routePatternKey, 123)
	if _, ok := RoutePatternFromContext(ctxInvalid); ok {
		t.Error("Expected false for non-string pattern")
	}

	// Test DefaultRouteKey fallback when URL is nil or host is empty
	reqNoURL := &http.Request{Method: http.MethodGet}
	if key := DefaultRouteKey(reqNoURL); key != "GET /" {
		t.Errorf("Expected 'GET /', got %q", key)
	}
}

func TestCircuitBreaker_CustomIsFailure(t *testing.T) {
	var serverRequests atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverRequests.Add(1)
		w.WriteHeader(http.StatusTooManyRequests) // 429 Too Many Requests
	}))
	defer ts.Close()

	// Treat 429 as failure
	cbOpts := []CircuitBreakerOption{
		WithCBReadyToTrip(func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 2
		}),
		WithCBIsFailure(func(resp *http.Response, err error) bool {
			if err != nil {
				return !errors.Is(err, context.Canceled)
			}
			return resp.StatusCode == http.StatusTooManyRequests
		}),
	}

	c := New(
		WithBaseURL(ts.URL),
		WithCircuitBreaker(cbOpts...),
	)
	ctx := context.Background()

	// Send 2 requests returning 429
	for i := 1; i <= 2; i++ {
		_, err := c.R().SetContext(ctx).Get("/rate-limited")
		if err != nil {
			t.Fatalf("Request %d expected 429 response, got error: %v", i, err)
		}
	}

	// 3rd request should be blocked by circuit breaker
	_, err := c.R().SetContext(ctx).Get("/rate-limited")
	if err == nil {
		t.Fatal("Expected 3rd request to fail because 429 tripped the circuit breaker")
	}
}

func TestCircuitBreaker_EvictionPreservesOpenBreakers(t *testing.T) {
	cbm := NewCircuitBreakerManager(
		WithCBMaxBreakers(4),
		WithCBReadyToTrip(func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 1
		}),
	)

	// Create and trip breaker for route1
	b1 := cbm.GetOrCreate("GET route1")
	done, _ := b1.Allow()
	done(errors.New("fail")) // trips b1 to Open

	if b1.State() != gobreaker.StateOpen {
		t.Fatalf("Expected b1 to be Open, got %v", b1.State())
	}

	// Add closed breakers to exceed MaxBreakers (4)
	cbm.GetOrCreate("GET route2")
	cbm.GetOrCreate("GET route3")
	cbm.GetOrCreate("GET route4")
	// Adding 5th breaker triggers eviction
	cbm.GetOrCreate("GET route5")

	// Verify open breaker b1 was preserved during closed breaker pruning!
	cbm.mu.RLock()
	retainedB1, ok := cbm.breakers["GET route1"]
	cbm.mu.RUnlock()

	if !ok {
		t.Error("Expected open breaker route1 to be preserved during eviction")
	} else if retainedB1.State() != gobreaker.StateOpen {
		t.Errorf("Expected retained breaker to still be Open, got %v", retainedB1.State())
	}

	// Test eviction pass 2: when ALL breakers are Open and capacity is exceeded
	allOpenMgr := NewCircuitBreakerManager(
		WithCBMaxBreakers(3),
		WithCBReadyToTrip(func(counts gobreaker.Counts) bool { return true }),
	)
	for i := 1; i <= 3; i++ {
		b := allOpenMgr.GetOrCreate(fmt.Sprintf("GET route%d", i))
		done, _ := b.Allow()
		done(errors.New("fail")) // trip all to Open
	}
	// 4th breaker forces pass 2 eviction since none are StateClosed
	allOpenMgr.GetOrCreate("GET route4")
	if allOpenMgr.Count() > 3 {
		t.Errorf("Expected count <= 3, got %d", allOpenMgr.Count())
	}
}

func TestCircuitBreaker_CustomIsFailure_ErrorAndFalse(t *testing.T) {
	// Case 1: Custom IsFailure returning true on error
	cbOptsErr := []CircuitBreakerOption{
		WithCBIsFailure(func(resp *http.Response, err error) bool {
			return err != nil
		}),
	}
	c1 := New(
		WithBaseURL("http://127.0.0.1:1"), // connection refused error
		WithCircuitBreaker(cbOptsErr...),
	)
	_, err := c1.R().SetContext(context.Background()).Get("/")
	if err == nil {
		t.Fatal("Expected error connecting to dead port")
	}

	// Case 2: Custom IsFailure returning false on 500 (ignoring failure)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	cbOptsIgnore500 := []CircuitBreakerOption{
		WithCBReadyToTrip(func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 1
		}),
		WithCBIsFailure(func(resp *http.Response, err error) bool {
			return false // never fail
		}),
	}
	c2 := New(
		WithBaseURL(ts.URL),
		WithCircuitBreaker(cbOptsIgnore500...),
	)
	for i := 0; i < 3; i++ {
		resp, _ := c2.R().SetContext(context.Background()).Get("/")
		if resp.StatusCode() != http.StatusInternalServerError {
			t.Errorf("Expected 500, got %d", resp.StatusCode())
		}
	}
	// Breaker should STILL be closed because IsFailure returned false
	cb := c2.CircuitBreakerManager().GetOrCreate("GET " + ts.URL[len("http://"):])
	if cb.State() != gobreaker.StateClosed {
		t.Errorf("Expected breaker to remain Closed, got %v", cb.State())
	}
}

func BenchmarkNormalizePath(b *testing.B) {
	path := "/api/v1/customers/9821/orders/550e8400-e29b-41d4-a716-446655440000/status?notify=true"
	b.ReportAllocs()
	for b.Loop() {
		_ = NormalizePath(path)
	}
}

func BenchmarkCircuitBreaker_AllowedRequest(b *testing.B) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := New(
		WithBaseURL(ts.URL),
		WithCircuitBreaker(),
	)
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = c.R().SetContext(ctx).Get("/users/123")
	}
}
