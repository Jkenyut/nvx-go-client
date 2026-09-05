package client

import (
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	// DefaultTimeout is the default request timeout applied to the client.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxIdleConns is the default total maximum number of idle (keep-alive) connections.
	DefaultMaxIdleConns = 100

	// DefaultMaxIdleConnsPerHost is the default maximum idle (keep-alive) connections per host.
	DefaultMaxIdleConnsPerHost = 20

	// DefaultIdleConnTimeout is the default maximum time an idle connection will remain open.
	DefaultIdleConnTimeout = 90 * time.Second
)

// Client is a wrapper around resty.Client that is automatically instrumented with OpenTelemetry.
type Client struct {
	*resty.Client
	otelOpts  []otelhttp.Option
	cbManager *CircuitBreakerManager
}

// Option is a function for configuring the client.
type Option func(*Client)

// CircuitBreakerManager returns the CircuitBreakerManager instance if circuit breaking is enabled, or nil.
func (c *Client) CircuitBreakerManager() *CircuitBreakerManager {
	return c.cbManager
}

// SetTransport sets the HTTP transport for the client and ensures it is wrapped with OpenTelemetry instrumentation
// and any configured dynamic Circuit Breaker.
func (c *Client) SetTransport(transport http.RoundTripper) *Client {
	if transport == nil {
		transport = defaultTransport()
	}

	var otelTransport http.RoundTripper
	if ot, ok := transport.(*otelhttp.Transport); ok {
		otelTransport = ot
	} else {
		otelTransport = otelhttp.NewTransport(transport, c.otelOpts...)
	}

	finalTransport := otelTransport
	if c.cbManager != nil {
		finalTransport = newCircuitBreakerTransport(otelTransport, c.cbManager)
	}

	c.Client.SetTransport(finalTransport)
	return c
}

// WithTimeout sets the default request timeout for all outgoing HTTP requests.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		c.SetTimeout(timeout)
	}
}

// WithBaseURL sets the base URL for the client.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		c.SetBaseURL(baseURL)
	}
}

// WithRetry configures the client's retry count and optional retry wait time.
func WithRetry(count int, waitTime time.Duration) Option {
	return func(c *Client) {
		c.SetRetryCount(count)
		if waitTime > 0 {
			c.SetRetryWaitTime(waitTime)
		}
	}
}

// WithTransport configures a custom HTTP RoundTripper before OpenTelemetry wrapping is applied.
func WithTransport(transport http.RoundTripper) Option {
	return func(c *Client) {
		c.Client.SetTransport(transport)
	}
}

// WithOTelOptions appends OpenTelemetry instrumentation options (e.g., custom TracerProvider, MeterProvider).
func WithOTelOptions(opts ...otelhttp.Option) Option {
	return func(c *Client) {
		c.otelOpts = append(c.otelOpts, opts...)
	}
}

// WithCircuitBreaker enables dynamic route-aware circuit breaking using sony/gobreaker/v2.
func WithCircuitBreaker(opts ...CircuitBreakerOption) Option {
	return func(c *Client) {
		c.cbManager = NewCircuitBreakerManager(opts...)
	}
}

// WithResty allows configuring the underlying resty client safely
// before OpenTelemetry instrumentation is finalized.
func WithResty(configure func(*resty.Client)) Option {
	return func(c *Client) {
		if configure != nil {
			configure(c.Client)
		}
	}
}

// New creates a new Client instance with OpenTelemetry instrumentation and production connection pooling defaults.
func New(opts ...Option) *Client {
	r := resty.New()
	r.SetTimeout(DefaultTimeout)

	c := &Client{
		Client: r,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}

	// Retrieve the transport which might have been set via opts (or default to nil).
	// Calling SetTransport guarantees it is wrapped with OpenTelemetry instrumentation.
	transport := r.GetClient().Transport
	c.SetTransport(transport)

	return c
}

func defaultTransport() *http.Transport {
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		t := dt.Clone()
		t.MaxIdleConns = DefaultMaxIdleConns
		t.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost
		t.IdleConnTimeout = DefaultIdleConnTimeout
		return t
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          DefaultMaxIdleConns,
		MaxIdleConnsPerHost:   DefaultMaxIdleConnsPerHost,
		IdleConnTimeout:       DefaultIdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
