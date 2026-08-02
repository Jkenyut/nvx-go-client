# nvx-go-client

`nvx-go-client` is a robust Go HTTP client built on top of the popular [go-resty/resty](https://github.com/go-resty/resty) library, automatically instrumented with [OpenTelemetry](https://opentelemetry.io/). 

It provides seamless integration for distributed tracing, metric generation, and context propagation out of the box, making it ideal for enterprise microservices and cloud-native applications.

## Features

- **Built on Resty**: Inherits all the powerful features of `resty.Client` (retries, timeouts, easy marshaling/unmarshaling, etc.).
- **Automatic OpenTelemetry Instrumentation**: Automatically injects trace headers and records metrics for all outgoing HTTP requests.
- **Flexible Configuration**: Safe functional options pattern (`ClientOption`) ensures that custom transports and configurations do not overwrite the tracing instrumentation.

## Installation

```bash
go get github/nvx-go-client
```

## Usage

Here is a basic example of how to use `nvx-go-client`:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github/nvx-go-client"
)

func main() {
	// 1. Initialize the client
	// You can pass optional ClientOptions here.
	c := client.NewClient()

	// 2. Make a request
	// IMPORTANT: Always pass a context to your request using SetContext()
	// so that OpenTelemetry can properly propagate the trace IDs.
	ctx := context.Background() // In a real app, use the context from your incoming HTTP handler

	resp, err := c.R().
		SetContext(ctx).
		Get("https://jsonplaceholder.typicode.com/todos/1")

	if err != nil {
		log.Fatalf("Request failed: %v", err)
	}

	fmt.Printf("Response Status: %v\n", resp.Status())
	fmt.Printf("Response Body: %v\n", resp.String())
}
```

## Under the Hood

When you call `NewClient()`, it initializes a new `resty.Client`. After applying any custom `ClientOption` functions you provide, it safely wraps the final `http.Transport` with `otelhttp.NewTransport`. This ensures that all outgoing requests are automatically instrumented for observability, regardless of any custom connection pooling or timeout settings you may have configured.