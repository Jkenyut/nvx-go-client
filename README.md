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
  - [Custom OpenTelemetry Options](#custom-opentelemetry-options)
  - [Advanced Resty Configuration](#advanced-resty-configuration)
  - [Custom Transport & mTLS](#custom-transport--mtls)
- [Available Options](#available-options)
- [Under the Hood & Architecture](#under-the-hood--architecture)
  - [Connection Pooling Defaults](#connection-pooling-defaults)
  - [Foolproof OpenTelemetry Invariant](#foolproof-opentelemetry-invariant)
- [Testing & Quality Gates](#testing--quality-gates)
- [Contributing](#contributing)
- [License](#license)

---

## Features

- **Built on Resty**: Inherits the complete feature set of `resty.Client` (automatic retries, timeouts, JSON/XML unmarshaling, middleware, and request chaining).
- **Automatic OpenTelemetry Instrumentation**: Transparently injects W3C `traceparent` headers and records client spans and metrics for all outgoing HTTP requests.
- **Foolproof Instrumentation Guarantee**: Even if a custom `http.RoundTripper` is configured via options or updated post-instantiation via `c.SetTransport()`, OpenTelemetry wrapping is always preserved.
- **Enterprise Connection Pooling**: Clones `http.DefaultTransport` with high-throughput defaults (`MaxIdleConns: 100`, `MaxIdleConnsPerHost: 20`, `IdleConnTimeout: 90s`), eliminating the notorious Go standard library 2-connection bottleneck.
- **Ergonomic Functional Options**: Clean, direct options like `WithTimeout`, `WithBaseURL`, `WithRetry`, `WithTransport`, and `WithOTelOptions` eliminate closure boilerplate.
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
| `client.WithTransport(rt)` | Configures a custom `http.RoundTripper` before OpenTelemetry wrapping. | Cloned `http.DefaultTransport` |
| `client.WithOTelOptions(opts...)` | Passes custom OpenTelemetry `otelhttp.Option` configurations. | None |
| `client.WithResty(fn)` | Arbitrary configuration callback on the underlying `*resty.Client`. | `nil` |

---

## Under the Hood & Architecture

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

---

## Contributing

Contributions are welcome! Please read our [CONTRIBUTING.md](CONTRIBUTING.md) for branch organization, commit conventions, and submission guidelines.

---

## License

Distributed under the [Apache 2.0 License](LICENSE).