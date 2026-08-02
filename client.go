package client

import (
	"net/http"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Client is a wrapper around resty.Client that is automatically instrumented with OpenTelemetry.
type Client struct {
	*resty.Client
}

// Option is a function for configuring the client.
type Option func(*Client)

// WithResty allows configuring the underlying resty client safely
// before OpenTelemetry instrumentation is applied.
func WithResty(configure func(*resty.Client)) Option {
	return func(c *Client) {
		configure(c.Client)
	}
}

// NewClient creates a new instance of Client with OpenTelemetry instrumentation.
func NewClient(opts ...Option) *Client {
	r := resty.New()

	c := &Client{
		Client: r,
	}

	for _, opt := range opts {
		opt(c)
	}

	// Retrieve the transport which might have been modified by opts.
	// If nil (no custom transport), fallback to http.DefaultTransport.
	transport := r.GetClient().Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	// Wrap the final transport with OpenTelemetry to ensure instrumentation
	// is always applied, regardless of any custom transport set via opts.
	// This automatically handles tracing, metric generation, and context propagation.
	r.SetTransport(otelhttp.NewTransport(transport))

	return c
}
