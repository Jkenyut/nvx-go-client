package client

import (
	"cmp"

	"github.com/Jkenyut/nvx-go-helper/activity"
	"github.com/Jkenyut/nvx-go-helper/cryptoutil"
	"github.com/Jkenyut/nvx-go-telemetry/telemetry"
	"github.com/go-resty/resty/v2"
)

// WithActivityPropagation enables automatic propagation of OpenTelemetry W3C trace headers
// and nvx-go-helper/activity metadata (X-Request-Id, X-Correlation-Id, X-User-Id, X-Forwarded-For, X-Ip-Origin)
// onto outgoing Resty requests. Optional HeaderKeys can override the default header names.
func WithActivityPropagation(keys ...HeaderKeys) Option {
	return func(c *Client) {
		hk := DefaultHeaderKeys()
		if len(keys) > 0 {
			custom := keys[0]
			hk.RequestID = cmp.Or(custom.RequestID, hk.RequestID)
			hk.CorrelationID = cmp.Or(custom.CorrelationID, hk.CorrelationID)
			hk.IP = cmp.Or(custom.IP, hk.IP)
			hk.IPOrigin = cmp.Or(custom.IPOrigin, hk.IPOrigin)
			hk.UserID = cmp.Or(custom.UserID, hk.UserID)
		}
		c.propagateKeys = &hk
		setupActivityPropagation(c.Client, &hk)
	}
}

func setupActivityPropagation(r *resty.Client, keys *HeaderKeys) {
	if r == nil || keys == nil {
		return
	}
	hk := *keys
	r.OnBeforeRequest(func(_ *resty.Client, req *resty.Request) error {
		ctx := req.Context()
		if ctx == nil {
			return nil
		}

		// 1. Inject W3C trace context headers (traceparent, baggage) via OpenTelemetry.
		telemetry.InjectHTTP(ctx, req.Header)

		// 2. Ensure Request ID exists (explicit header -> activity context -> fallback UUID v7).
		if req.Header.Get(hk.RequestID) == "" {
			reqID, _ := activity.GetRequestID(ctx)
			if reqID == "" {
				reqID = cryptoutil.V7()
			}
			req.SetHeader(hk.RequestID, reqID)
		}

		// 3. Propagate Correlation ID (explicit header -> activity context -> OTel TraceID).
		if req.Header.Get(hk.CorrelationID) == "" {
			if val, ok := activity.GetCorrelationID(ctx); ok && val != "" {
				req.SetHeader(hk.CorrelationID, val)
			} else if traceID := telemetry.TraceID(ctx); traceID != "" {
				req.SetHeader(hk.CorrelationID, traceID)
			}
		}

		// 4. Propagate User ID, client IP, and IP origin when present in activity context.
		if req.Header.Get(hk.UserID) == "" {
			if val, ok := activity.GetUserID(ctx); ok && val != "" {
				req.SetHeader(hk.UserID, val)
			}
		}
		if req.Header.Get(hk.IP) == "" {
			if val, ok := activity.GetUserIP(ctx); ok && val != "" {
				req.SetHeader(hk.IP, val)
			}
		}
		if req.Header.Get(hk.IPOrigin) == "" {
			if val, ok := activity.GetUserIPOrigin(ctx); ok && val != "" {
				req.SetHeader(hk.IPOrigin, val)
			}
		}

		return nil
	})
}
