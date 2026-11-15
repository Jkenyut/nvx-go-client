package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/Jkenyut/nvx-go-telemetry/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type metricsTransport struct {
	base         http.RoundTripper
	durationHist metric.Float64Histogram
	reqCounter   metric.Int64Counter
}

func newMetricsTransport(base http.RoundTripper) http.RoundTripper {
	mm := telemetry.NewMetricManager("github.com/Jkenyut/nvx-go-client")

	durationHist, _ := mm.RegisterHistogram(
		"http.client.request.duration",
		"Duration of outbound HTTP requests in seconds",
		"s",
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)

	reqCounter, _ := mm.RegisterCounter(
		"http.client.requests.total",
		"Total number of outbound HTTP requests initiated",
	)

	return &metricsTransport{
		base:         base,
		durationHist: durationHist,
		reqCounter:   reqCounter,
	}
}

func (m *metricsTransport) CloseIdleConnections() {
	if ci, ok := m.base.(interface{ CloseIdleConnections() }); ok {
		ci.CloseIdleConnections()
	}
}

func (m *metricsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	start := time.Now()

	resp, err := m.base.RoundTrip(req)
	duration := time.Since(start).Seconds()

	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}

	serverAddr := extractServerHost(req)
	routeTemplate := extractRouteTemplate(req)

	attrs := make([]attribute.KeyValue, 0, 5)
	attrs = append(attrs,
		attribute.String("http.request.method", req.Method),
		attribute.String("server.address", serverAddr),
		attribute.String("http.route", routeTemplate),
		attribute.Int("http.response.status_code", statusCode),
	)
	if err != nil {
		attrs = append(attrs, attribute.String("error.type", classifyError(err)))
	}

	if m.durationHist != nil {
		m.durationHist.Record(ctx, duration, metric.WithAttributes(attrs...))
	}
	if m.reqCounter != nil {
		m.reqCounter.Add(ctx, 1, metric.WithAttributes(attrs...))
	}

	return resp, err
}

func classifyError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		if netErr, ok := errors.AsType[net.Error](err); ok {
			if netErr.Timeout() {
				return "network_timeout"
			}
			return "network_error"
		}
		return "transport_error"
	}
}
