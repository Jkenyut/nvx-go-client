package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel/trace"
)

// ErrCircuitOpen is returned when a request is blocked because the circuit breaker is in the Open state.
var ErrCircuitOpen = gobreaker.ErrOpenState

// CircuitBreakerConfig defines configuration settings for the route-aware dynamic circuit breaker.
type CircuitBreakerConfig struct {
	// MaxRequests is the maximum number of requests allowed through when half-open. Default: 3.
	MaxRequests uint32

	// Interval is the cyclic period of the closed state to clear internal counts. Default: 30s.
	Interval time.Duration

	// Timeout is the duration the circuit breaker remains in open state before transitioning to half-open. Default: 30s.
	Timeout time.Duration

	// ReadyToTrip customizes the trip condition.
	// Default: Requests >= 10 && (FailureRate >= 50% || ConsecutiveFailures >= 5).
	ReadyToTrip func(counts gobreaker.Counts) bool

	// OnStateChange is an optional hook invoked whenever a circuit breaker transitions state.
	OnStateChange func(name string, from, to gobreaker.State)

	// IsFailure customizes error/status classification.
	// If nil, defaults to: err != nil && !errors.Is(err, context.Canceled) || resp.StatusCode >= 500.
	IsFailure func(resp *http.Response, err error) bool

	// RouteKeyFunc extracts the canonical route key from an HTTP request.
	// Defaults to DefaultRouteKey.
	RouteKeyFunc func(req *http.Request) string

	// MaxBreakers is the maximum number of circuit breakers retained in memory before eviction. Default: 1000.
	MaxBreakers int
}

// CircuitBreakerOption is a functional option for customizing CircuitBreakerConfig.
type CircuitBreakerOption func(*CircuitBreakerConfig)

// WithCBMaxRequests sets the maximum number of requests allowed through when the breaker is half-open.
func WithCBMaxRequests(maxRequests uint32) CircuitBreakerOption {
	return func(cfg *CircuitBreakerConfig) {
		cfg.MaxRequests = maxRequests
	}
}

// WithCBInterval sets the cyclic duration of the closed state to clear internal counts.
func WithCBInterval(interval time.Duration) CircuitBreakerOption {
	return func(cfg *CircuitBreakerConfig) {
		cfg.Interval = interval
	}
}

// WithCBTimeout sets the cooldown duration in open state before transitioning to half-open.
func WithCBTimeout(timeout time.Duration) CircuitBreakerOption {
	return func(cfg *CircuitBreakerConfig) {
		cfg.Timeout = timeout
	}
}

// WithCBReadyToTrip specifies the predicate function deciding when to trip into open state.
func WithCBReadyToTrip(fn func(counts gobreaker.Counts) bool) CircuitBreakerOption {
	return func(cfg *CircuitBreakerConfig) {
		cfg.ReadyToTrip = fn
	}
}

// WithCBOnStateChange specifies a callback for state transition notifications.
func WithCBOnStateChange(fn func(name string, from, to gobreaker.State)) CircuitBreakerOption {
	return func(cfg *CircuitBreakerConfig) {
		cfg.OnStateChange = fn
	}
}

// WithCBIsFailure sets a custom predicate to determine whether an HTTP response or error is counted as a failure.
func WithCBIsFailure(fn func(resp *http.Response, err error) bool) CircuitBreakerOption {
	return func(cfg *CircuitBreakerConfig) {
		cfg.IsFailure = fn
	}
}

// WithCBRouteKeyFunc customizes the route key generation function.
func WithCBRouteKeyFunc(fn func(req *http.Request) string) CircuitBreakerOption {
	return func(cfg *CircuitBreakerConfig) {
		cfg.RouteKeyFunc = fn
	}
}

// WithCBMaxBreakers sets the maximum number of circuit breakers stored in memory.
func WithCBMaxBreakers(maxBreakers int) CircuitBreakerOption {
	return func(cfg *CircuitBreakerConfig) {
		cfg.MaxBreakers = maxBreakers
	}
}

func defaultCBConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		MaxRequests: 3,
		Interval:    30 * time.Second,
		Timeout:     30 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// Require minimum sample size to avoid false positive trips on low traffic
			if counts.Requests < 10 {
				return false
			}
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return failureRatio >= 0.50 || counts.ConsecutiveFailures >= 5
		},
		RouteKeyFunc: DefaultRouteKey,
		MaxBreakers:  1000,
	}
}

// CircuitBreakerManager manages a thread-safe registry of route-keyed circuit breakers.
type CircuitBreakerManager struct {
	mu       sync.RWMutex
	breakers map[string]*gobreaker.TwoStepCircuitBreaker[struct{}]
	cfg      CircuitBreakerConfig
}

// NewCircuitBreakerManager initializes a new CircuitBreakerManager with the given configuration options.
func NewCircuitBreakerManager(opts ...CircuitBreakerOption) *CircuitBreakerManager {
	cfg := defaultCBConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &CircuitBreakerManager{
		breakers: make(map[string]*gobreaker.TwoStepCircuitBreaker[struct{}]),
		cfg:      cfg,
	}
}

// GetOrCreate retrieves an existing circuit breaker for routeKey, or initializes a new one.
func (m *CircuitBreakerManager) GetOrCreate(routeKey string) *gobreaker.TwoStepCircuitBreaker[struct{}] {
	m.mu.RLock()
	cb, exists := m.breakers[routeKey]
	m.mu.RUnlock()
	if exists {
		return cb
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double check under write lock
	if cb, exists = m.breakers[routeKey]; exists {
		return cb
	}

	// Safety guard against unbounded cardinality: evict if capacity exceeded
	if m.cfg.MaxBreakers > 0 && len(m.breakers) >= m.cfg.MaxBreakers {
		// Pass 1: Prune closed (healthy) breakers to preserve open/half-open tripping states
		for k, b := range m.breakers {
			if b.State() == gobreaker.StateClosed {
				delete(m.breakers, k)
				if len(m.breakers) <= m.cfg.MaxBreakers*3/4 {
					break
				}
			}
		}
		// Pass 2: If still at limit (all breakers open/half-open), prune oldest entries to admit new
		if len(m.breakers) >= m.cfg.MaxBreakers {
			for k := range m.breakers {
				delete(m.breakers, k)
				if len(m.breakers) <= m.cfg.MaxBreakers*3/4 {
					break
				}
			}
		}
	}

	st := gobreaker.Settings{
		Name:          routeKey,
		MaxRequests:   m.cfg.MaxRequests,
		Interval:      m.cfg.Interval,
		Timeout:       m.cfg.Timeout,
		ReadyToTrip:   m.cfg.ReadyToTrip,
		OnStateChange: m.cfg.OnStateChange,
	}

	cb = gobreaker.NewTwoStepCircuitBreaker[struct{}](st)
	m.breakers[routeKey] = cb
	return cb
}

// Count returns the number of active route circuit breakers registered in memory.
func (m *CircuitBreakerManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.breakers)
}

// circuitBreakerTransport is an http.RoundTripper middleware that enforces circuit breaking per route.
type circuitBreakerTransport struct {
	next http.RoundTripper
	cbm  *CircuitBreakerManager
}

// newCircuitBreakerTransport wraps next with circuit breaking protection.
func newCircuitBreakerTransport(next http.RoundTripper, cbm *CircuitBreakerManager) *circuitBreakerTransport {
	return &circuitBreakerTransport{
		next: next,
		cbm:  cbm,
	}
}

// RoundTrip executes the HTTP request under the protection of the route-keyed circuit breaker.
func (t *circuitBreakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.cbm == nil {
		return t.next.RoundTrip(req)
	}

	routeKey := t.cbm.cfg.RouteKeyFunc(req)
	cb := t.cbm.GetOrCreate(routeKey)

	done, err := cb.Allow()
	if err != nil {
		// Circuit is open or request rejected
		// Record error in OpenTelemetry span if available
		span := trace.SpanFromContext(req.Context())
		if span.IsRecording() {
			span.RecordError(err)
		}
		return nil, fmt.Errorf("circuit breaker %q rejected request: %w", routeKey, err)
	}

	resp, roundTripErr := t.next.RoundTrip(req)
	if t.cbm.cfg.IsFailure != nil {
		if t.cbm.cfg.IsFailure(resp, roundTripErr) {
			if roundTripErr != nil {
				done(roundTripErr)
			} else {
				done(fmt.Errorf("circuit breaker failure: HTTP %d", resp.StatusCode))
			}
		} else {
			done(nil)
		}
		return resp, roundTripErr
	}

	if roundTripErr != nil {
		// Check if error is client cancellation (should not penalize upstream health)
		if errors.Is(roundTripErr, context.Canceled) {
			done(nil)
		} else {
			done(roundTripErr)
		}
		return nil, roundTripErr
	}

	// Evaluate HTTP status code: 5xx is server failure; 4xx and 2xx/3xx are considered success
	if resp.StatusCode >= http.StatusInternalServerError {
		done(fmt.Errorf("upstream server error: %d", resp.StatusCode))
	} else {
		done(nil)
	}

	return resp, nil
}
