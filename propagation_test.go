package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jkenyut/nvx-go-helper/activity"
)

func TestWithActivityPropagation(t *testing.T) {
	t.Run("propagates activity metadata and generates fallback request id", func(t *testing.T) {
		var gotHeaders http.Header
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		c := New(WithBaseURL(ts.URL), WithActivityPropagation())

		ctx := activity.WithActivity(context.Background(), activity.Activity{
			CorrelationID: "corr-123",
			UserID:        "user-456",
			UserIP:        "10.0.0.2",
			UserIPOrigin:  "203.0.113.9",
		})

		resp, err := c.R().SetContext(ctx).Get("/test")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.StatusCode() != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode())
		}

		if gotHeaders.Get("X-Request-Id") == "" {
			t.Error("expected generated X-Request-Id header")
		}
		if gotHeaders.Get("X-Correlation-Id") != "corr-123" {
			t.Errorf("expected X-Correlation-Id corr-123, got %q", gotHeaders.Get("X-Correlation-Id"))
		}
		if gotHeaders.Get("X-User-Id") != "user-456" {
			t.Errorf("expected X-User-Id user-456, got %q", gotHeaders.Get("X-User-Id"))
		}
		if gotHeaders.Get("X-Forwarded-For") != "10.0.0.2" {
			t.Errorf("expected X-Forwarded-For 10.0.0.2, got %q", gotHeaders.Get("X-Forwarded-For"))
		}
		if gotHeaders.Get("X-Ip-Origin") != "203.0.113.9" {
			t.Errorf("expected X-Ip-Origin 203.0.113.9, got %q", gotHeaders.Get("X-Ip-Origin"))
		}
	})

	t.Run("does not overwrite explicit request headers", func(t *testing.T) {
		var gotHeaders http.Header
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		c := New(WithBaseURL(ts.URL), WithActivityPropagation())

		ctx := activity.WithActivity(context.Background(), activity.Activity{
			RequestID:     "ctx-req",
			CorrelationID: "ctx-corr",
			UserID:        "ctx-user",
		})

		_, err := c.R().
			SetContext(ctx).
			SetHeader("X-Request-Id", "explicit-req").
			SetHeader("X-User-Id", "explicit-user").
			Get("/test")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if gotHeaders.Get("X-Request-Id") != "explicit-req" {
			t.Errorf("expected explicit-req, got %q", gotHeaders.Get("X-Request-Id"))
		}
		if gotHeaders.Get("X-User-Id") != "explicit-user" {
			t.Errorf("expected explicit-user, got %q", gotHeaders.Get("X-User-Id"))
		}
		if gotHeaders.Get("X-Correlation-Id") != "ctx-corr" {
			t.Errorf("expected ctx-corr, got %q", gotHeaders.Get("X-Correlation-Id"))
		}
	})
}
