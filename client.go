package client

import (
	"net"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/metric/noop"
)

const (
	// DefaultTimeout is the default request timeout applied to the client.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxIdleConns is the default total maximum number of idle (keep-alive) connections.
	DefaultMaxIdleConns = 100

	// DefaultMaxIdleConnsPerHost is the default maximum idle (keep-alive) connections per host.
	DefaultMaxIdleConnsPerHost = 20

	// DefaultMaxConnsPerHost is the default maximum total (active + idle) connections per host.
	DefaultMaxConnsPerHost = 100

	// DefaultIdleConnTimeout is the default maximum time an idle connection will remain open.
	DefaultIdleConnTimeout = 90 * time.Second
)

// Client is a wrapper around resty.Client that is automatically instrumented with OpenTelemetry.
type Client struct {
	*resty.Client
	otelOpts         []otelhttp.Option
	cbManager        *CircuitBreakerManager
	auditCfg         *AuditConfig
	baseTransport    http.RoundTripper
	wrappedTransport http.RoundTripper
}

// Option is a function for configuring the client.
type Option func(*Client)

// CircuitBreakerManager returns the CircuitBreakerManager instance if circuit breaking is enabled, or nil.
func (c *Client) CircuitBreakerManager() *CircuitBreakerManager {
	return c.cbManager
}

// AuditConfig returns the configured AuditConfig, or nil if audit logging is not enabled.
func (c *Client) AuditConfig() *AuditConfig {
	return c.auditCfg
}

// CloseIdleConnections closes all idle connections in the underlying HTTP transport pool.
func (c *Client) CloseIdleConnections() {
	if c == nil || c.Client == nil {
		return
	}
	if cl := c.GetClient(); cl != nil {
		cl.CloseIdleConnections()
		if c.baseTransport != nil && c.baseTransport != cl.Transport {
			if _, ok := cl.Transport.(interface{ CloseIdleConnections() }); !ok {
				if ci, ok := c.baseTransport.(interface{ CloseIdleConnections() }); ok {
					ci.CloseIdleConnections()
				}
			}
		}
	}
}

// SetTransport sets the HTTP transport for the client and ensures it is wrapped with OpenTelemetry instrumentation
// and any configured dynamic Circuit Breaker without double-wrapping existing layers.
func (c *Client) SetTransport(transport http.RoundTripper) *Client {
	if transport == nil {
		transport = defaultTransport()
	}

	if c.wrappedTransport != nil && transport == c.wrappedTransport && c.baseTransport != nil {
		transport = c.baseTransport
	}

	for {
		if mt, ok := transport.(*metricsTransport); ok {
			transport = mt.base
			continue
		}
		if cbt, ok := transport.(*circuitBreakerTransport); ok {
			transport = cbt.next
			continue
		}
		break
	}

	c.baseTransport = transport

	if ot, ok := transport.(*otelhttp.Transport); ok {
		var final http.RoundTripper = ot
		if c.cbManager != nil {
			final = newCircuitBreakerTransport(ot, c.cbManager)
		}
		c.wrappedTransport = final
		c.Client.SetTransport(final)
		return c
	}

	current := transport
	if c.cbManager != nil {
		current = newCircuitBreakerTransport(current, c.cbManager)
	}

	metricsWrapped := newMetricsTransport(current)
	otelOpts := make([]otelhttp.Option, 0, 1+len(c.otelOpts))
	otelOpts = append(otelOpts, otelhttp.WithMeterProvider(noop.NewMeterProvider()))
	otelOpts = append(otelOpts, c.otelOpts...)
	otelTransport := otelhttp.NewTransport(metricsWrapped, otelOpts...)

	c.wrappedTransport = otelTransport
	c.Client.SetTransport(otelTransport)
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

// WithAudit enables outbound audit logging, emitting structured request and response records via log/slog.
//
//nolint:gocritic // hugeParam: intentional ergonomic by-value option pattern
func WithAudit(cfg AuditConfig) Option {
	return func(c *Client) {
		c.auditCfg = applyAuditDefaults(&cfg)
	}
}

// WithAuditConfig enables outbound audit logging configured via functional AuditOptions.
func WithAuditConfig(opts ...AuditOption) Option {
	return func(c *Client) {
		var cfg AuditConfig
		if c.auditCfg != nil {
			cfg = *c.auditCfg
		}
		for _, opt := range opts {
			if opt != nil {
				opt(&cfg)
			}
		}
		c.auditCfg = applyAuditDefaults(&cfg)
	}
}

// WithLogStore configures a custom LogStore for outbound audit logging.
func WithLogStore(store LogStore) Option {
	return WithAuditConfig(WithAuditLogStore(store))
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

	if c.auditCfg != nil {
		setupAuditHooks(c.Client, c.auditCfg)
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
		t.MaxConnsPerHost = DefaultMaxConnsPerHost
		t.IdleConnTimeout = DefaultIdleConnTimeout
		return t
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          DefaultMaxIdleConns,
		MaxIdleConnsPerHost:   DefaultMaxIdleConnsPerHost,
		MaxConnsPerHost:       DefaultMaxConnsPerHost,
		IdleConnTimeout:       DefaultIdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
