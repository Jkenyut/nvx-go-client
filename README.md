# nvx-go-client

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Tests](https://img.shields.io/badge/coverage-98.0%25-brightgreen.svg)]()

`nvx-go-client` is an enterprise-grade Go HTTP client built on top of [go-resty/resty](https://github.com/go-resty/resty), automatically instrumented with [OpenTelemetry](https://opentelemetry.io/) (`otelhttp`).

It delivers seamless distributed tracing, metric generation, and W3C context propagation out of the box, backed by production-tuned connection pooling and resilient timeout defaults.

---

## Table of Contents

- [Features](#features)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Advanced Usage](#advanced-usage)
  - [Dynamic Route-Based Circuit Breaker (`sony/gobreaker/v2`)](#dynamic-route-based-circuit-breaker-sonygobreakerv2)
  - [Custom OpenTelemetry Options](#custom-opentelemetry-options)
  - [Advanced Resty Configuration](#advanced-resty-configuration)
  - [Custom Transport & mTLS](#custom-transport--mtls)
- [Available Options](#available-options)
- [Under the Hood & Architecture](#under-the-hood--architecture)
  - [Dynamic Route Normalization & Cardinality Protection](#dynamic-route-normalization--cardinality-protection)
  - [Enterprise Circuit Breaker Calibration](#enterprise-circuit-breaker-calibration)
  - [Connection Pooling Defaults](#connection-pooling-defaults)
  - [Foolproof OpenTelemetry Invariant](#foolproof-opentelemetry-invariant)
- [Testing & Quality Gates](#testing--quality-gates)
- [Contributing](#contributing)
- [License](#license)

---

## Features

- **Built on Resty**: Inherits the complete feature set of `resty.Client` (automatic retries, timeouts, JSON/XML unmarshaling, middleware, and request chaining).
- **Automatic OpenTelemetry Instrumentation**: Transparently injects W3C `traceparent` headers and records client spans and metrics for all outgoing HTTP requests.
- **Dynamic Route-Aware Circuit Breaker**: Powered by `sony/gobreaker/v2`. Automatically isolates circuit breakers per endpoint, collapses dynamic path parameters (numeric IDs, UUIDs, ObjectIDs, ULIDs), strips query strings, and fails fast on downstream outages without cardinality explosion.
- **Foolproof Instrumentation Guarantee**: Even if a custom `http.RoundTripper` is configured via options or updated post-instantiation via `c.SetTransport()`, OpenTelemetry wrapping is always preserved.
- **Enterprise Connection Pooling**: Clones `http.DefaultTransport` with high-throughput defaults (`MaxIdleConns: 100`, `MaxIdleConnsPerHost: 20`, `IdleConnTimeout: 90s`), eliminating the notorious Go standard library 2-connection bottleneck.
- **Ergonomic Functional Options**: Clean, direct options like `WithTimeout`, `WithBaseURL`, `WithRetry`, `WithCircuitBreaker`, and `WithOTelOptions` eliminate closure boilerplate.
- **Resilient Defaults**: Enforces a 30-second default request timeout to prevent uncancelled network calls from hanging indefinitely.

---

## Installation

```bash
go get github/nvx-go-client
```

---

## Quick Start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github/nvx-go-client"
)

func main() {
	// 1. Initialize client with functional options
	c := client.New(
		client.WithBaseURL("https://jsonplaceholder.typicode.com"),
		client.WithTimeout(10 * time.Second),
		client.WithRetry(3, 100 * time.Millisecond),
	)

	// 2. Make an outgoing request
	// IMPORTANT: Always pass a context via SetContext() so OpenTelemetry
	// can inject distributed trace headers into the outgoing HTTP request.
	ctx := context.Background() // In a web handler, use r.Context()

	resp, err := c.R().
		SetContext(ctx).
		Get("/todos/1")

	if err != nil {
		log.Fatalf("Request failed: %v", err)
	}

	fmt.Printf("Status: %s\n", resp.Status())
	fmt.Printf("Body: %s\n", resp.String())
}
```

---

## Advanced Usage

### Dynamic Route-Based Circuit Breaker (`sony/gobreaker/v2`)

Enable automatic route-aware circuit breaking to protect your services from cascading outages and thundering-herd effects:

```go
package main

import (
	"context"
	"errors"
	"time"

	"github/nvx-go-client"
)

func main() {
	c := client.New(
		client.WithBaseURL("https://api.payment.internal"),
		client.WithCircuitBreaker(
			client.WithCBTimeout(30 * time.Second), // Sleep duration in Open state
			client.WithCBMaxRequests(3),            // Trial requests in Half-Open state
		),
	)

	ctx := context.Background()

	// Dynamic URLs (/orders/12345, /orders/67890) and queries (?page=1)
	// are automatically collapsed into the canonical route:
	// "GET api.payment.internal/orders/{param}"
	resp, err := c.R().
		SetContext(ctx).
		Get("/orders/12345?notify=true")

	if errors.Is(err, client.ErrCircuitOpen) {
		// Fail-fast: The downstream service is degraded.
		// The request was aborted immediately without any network I/O.
		return
	}
}
```

#### Explicit Route Pattern Override
If your endpoint contains dynamic string slugs that heuristics cannot safely detect (e.g., usernames `/users/johndoe`), bind an explicit route pattern to the request context:

```go
ctx := client.WithRoutePattern(context.Background(), "/users/{username}")

resp, err := c.R().
	SetContext(ctx).
	Get("/users/johndoe")
```

### Outbound Audit Logging (`log/slog` Agnostic Integration)

Seamlessly emit structured enterprise outbound audit logs via Go standard library `log/slog` with automatic context extraction (from `nvx-go-helper/activity` or custom hooks), text payload masking, body size limits, and multipart/binary exclusion:

```go
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/Jkenyut/nvx-go-client"
)

func main() {
	// Any standard slog.Logger (JSON, text, or third-party handler)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	c := client.New(
		client.WithBaseURL("https://api.partner.com"),
		client.WithAudit(client.AuditConfig{
			Logger:                   logger, // Defaults to slog.Default() if nil
			ServiceName:              "checkout-service",
			LogRequestBodies:         true,  // Log text request bodies (JSON/Form)
			LogResponseBodies:        true,  // Log text response bodies
			RequestBodyLogLimitSize:  3 * 1024 * 1024, // 3MB limit
			ResponseBodyLogLimitSize: 5 * 1024 * 1024, // 5MB limit
			MaskKeywords:             []string{"custom_token", "tax_number"},
			// Context attributes (transaction_id, request_id, user_id, user_ip, user_ip_origin)
			// are automatically extracted from nvx-go-helper/activity or HTTP headers.
			// You can also supply a custom ContextAttrs hook:
			// ContextAttrs: func(ctx context.Context) []slog.Attr { ... },
		}),
	)

	// Context metadata injected by activity package or HTTP headers
	// is automatically captured into structured slog attributes!
	_, _ = c.R().SetContext(ctx).SetBody(payload).Post("/v1/charge")
}
```

### Custom OpenTelemetry Options

You can pass custom `otelhttp.Option` values directly via `WithOTelOptions` (e.g., custom span name formatters, custom `TracerProvider`, or client trace filters):

```go
import (
	"net/http"

	"github/nvx-go-client"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

c := client.New(
	client.WithBaseURL("https://api.internal.service"),
	client.WithOTelOptions(
		otelhttp.WithSpanNameFormatter(func(operation string, r *http.Request) string {
			return "HTTP " + r.Method + " " + r.URL.Path
		}),
	),
)
```

### Advanced Resty Configuration

For advanced settings (e.g., global auth tokens, default headers, cookies), use `WithResty`:

```go
c := client.New(
	client.WithBaseURL("https://api.internal.service"),
	client.WithResty(func(r *resty.Client) {
		r.SetAuthToken("my-secret-token").
			SetHeader("Accept", "application/json").
			SetHeader("X-Client-Version", "1.0.0")
	}),
)
```

### Custom Transport & mTLS

If your service requires Mutual TLS (mTLS) or a custom proxy, pass the `http.RoundTripper` via `WithTransport`:

```go
tlsConfig := &tls.Config{
	MinVersion: tls.VersionTLS13,
}

customTransport := &http.Transport{
	TLSClientConfig: tlsConfig,
}

c := client.New(
	client.WithTransport(customTransport),
)
```

Even with custom transports, OpenTelemetry tracing is **automatically applied and preserved**.

---

## Available Options

| Option | Description | Default |
| :--- | :--- | :--- |
| `client.WithTimeout(d)` | Sets the default request timeout for all requests. | `30s` |
| `client.WithBaseURL(url)` | Sets the target service base URL. | `""` |
| `client.WithRetry(count, wait)` | Configures automatic retry count and backoff wait time. | `0`, `0s` |
| `client.WithCircuitBreaker(opts...)` | Enables dynamic route-aware circuit breaking (`sony/gobreaker/v2`). | Disabled |
| `client.WithCBIsFailure(fn)` | Customizes error and HTTP status classification for circuit breaking. | `5xx` & network errors |
| `client.WithRoutePattern(ctx, pat)` | Binds an explicit route pattern (e.g. `/users/{username}`) to context. | None |
| `client.WithTransport(rt)` | Configures a custom `http.RoundTripper` before OpenTelemetry wrapping. | Cloned `http.DefaultTransport` |
| `client.WithOTelOptions(opts...)` | Passes custom OpenTelemetry `otelhttp.Option` configurations. | None |
| `client.WithResty(fn)` | Arbitrary configuration callback on the underlying `*resty.Client`. | `nil` |

---

## Under the Hood & Architecture

### Dynamic Route Normalization & Cardinality Protection

Dynamic circuit breaking groups requests by canonical route key (`METHOD Host/normalized_path`) instead of raw URLs:

1. **Query Stripping**: URLs like `/api/v1/users?page=1&sort=desc` are truncated to `/api/v1/users`.
2. **Path Parameter Detection**: Individual path segments are analyzed and dynamic identifiers are collapsed to `{param}` with zero regex overhead and a single allocation:
   - **Numeric IDs** (`/users/12345/orders/6789`) $\rightarrow$ `/users/{param}/orders/{param}`
   - **UUIDs** (`/items/550e8400-e29b-41d4-a716-446655440000`) $\rightarrow$ `/items/{param}`
   - **Mongo ObjectIDs & Hex Hashes** (`/docs/507f1f77bcf86cd799439011`) $\rightarrow$ `/docs/{param}`
   - **ULIDs / KSUIDs** (`/events/01ARZ3NDEKTSV4RRFFQ69G5FAV`) $\rightarrow$ `/events/{param}`
3. **Hardened Bounded Registry Guard**: In-memory circuit breakers are capped (`MaxBreakers: 1000` by default). When capacity is reached, healthy `StateClosed` breakers are pruned first, ensuring that `StateOpen` and `StateHalfOpen` breakers actively protecting failing backends are never prematurely wiped (preventing reset attacks).

### Enterprise Circuit Breaker Calibration

- **`ReadyToTrip` Gate**: Requires at least 10 requests (`Requests >= 10`) within the rolling 30s window to avoid tripping during cold start or low-traffic anomalies.
- **Fail-Fast**: Trips to `Open` when failure rate $\ge 50\%$ or 5 consecutive failures occur.
- **Error Classification**:
  - **`5xx` & Network Timeouts**: Counted as **Failures**.
  - **`4xx` (e.g. `400`, `401`, `404`)**: Counted as **Successes** from the breaker's perspective, because the upstream server is healthy and responding. This prevents malformed client requests from tripping circuit breakers for all other users.
  - **`context.Canceled`**: Excluded from counts (client disconnects do not penalize upstream health).
  - **Customizable Classification**: Supply `WithCBIsFailure(fn)` to easily customize classification (e.g., treating `429 Too Many Requests` or specific errors as failures).

### Connection Pooling Defaults

By default, Go's `http.DefaultTransport` limits idle connections per host to `2` (`DefaultMaxIdleConnsPerHost = 2`). In high-throughput microservices, concurrent requests to a single upstream service rapidly exceed 2, causing excess connections to be closed immediately and forcing expensive TCP + TLS renegotiations on every request.

`nvx-go-client` solves this by provisioning an isolated transport with tuned production defaults:

- **`MaxIdleConns: 100`**: Allows up to 100 total idle connections across all hosts.
- **`MaxIdleConnsPerHost: 20`**: Permits up to 20 concurrent idle connections to the same host.
- **`IdleConnTimeout: 90s`**: Keeps idle connections warm for up to 90 seconds.

### Foolproof OpenTelemetry Invariant

In standard wrappers, if a caller invokes `SetTransport()` on an HTTP client, the OpenTelemetry round-tripper is silently overwritten and tracing stops functioning without warning.

`nvx-go-client` overrides `SetTransport(roundTripper)` on the `*Client` struct:

```go
func (c *Client) SetTransport(transport http.RoundTripper) *Client {
	if transport == nil {
		transport = defaultTransport()
	}
	if ot, ok := transport.(*otelhttp.Transport); ok {
		c.Client.SetTransport(ot)
		return c
	}
	c.Client.SetTransport(otelhttp.NewTransport(transport, c.otelOpts...))
	return c
}
```

This guarantees that tracing, metrics, and context propagation remain active under all circumstances.

---

## Testing & Quality Gates

Run the automated test suite, race detector, and coverage check:

```bash
# Run unit tests with race detection and coverage
go test -v -race -cover ./...

# Run benchmarks
go test -run=^$ -bench=. -benchmem ./...

# Run linting and static analysis
golangci-lint run ./...
govulncheck ./...
```

### Benchmark Results (Apple M4)

| Benchmark | Latency | Memory | Heap Allocs | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `BenchmarkNormalizePath` | **140 ns/op** | 96 B/op | **1 alloc/op** | Zero regex, single allocation byte scanning |
| `BenchmarkCircuitBreaker_AllowedRequest` | **33.8 µs/op** | 13.3 KB/op | 124 allocs/op | Full roundtrip + OTel spans + Circuit Breaker |
| `BenchmarkClient_Request` | **31.9 µs/op** | 13.2 KB/op | 121 allocs/op | Baseline HTTP roundtrip + OTel spans |
| **Circuit Breaker Net Overhead** | **+1.9 µs/op** | **+180 B/op** | **+3 allocs/op** | Negligible impact on real network throughput |

---

## Contributing

Contributions are welcome! Please read our [CONTRIBUTING.md](CONTRIBUTING.md) for branch organization, commit conventions, and submission guidelines.

---

## License

Distributed under the [Apache 2.0 License](LICENSE).