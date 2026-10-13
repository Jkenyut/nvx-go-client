package client

import (
	"context"
	"errors"
	"log/slog"
)

// LogStore defines the interface for persisting outbound audit log entries.
// Implement this to store logs in a database, message queue, or any backend.
type LogStore interface {
	// Save persists an audit log entry. ctx carries request-scoped values.
	Save(ctx context.Context, entry *AuditLog) error
}

// ConsoleStore is the default LogStore implementation that writes structured
// log entries to the configured slog.Logger.
type ConsoleStore struct {
	logger *slog.Logger
}

// NewConsoleStore creates a new ConsoleStore with the specified logger.
// If logger is nil, slog.Default() is used.
func NewConsoleStore(logger *slog.Logger) *ConsoleStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &ConsoleStore{logger: logger}
}

// Save writes the audit log entry as structured log attributes via slog.
func (c *ConsoleStore) Save(ctx context.Context, entry *AuditLog) error {
	if entry == nil {
		return nil
	}
	c.logger.InfoContext(ctx, "outbound audit log",
		slog.String("id", entry.ID),
		slog.String("service_name", entry.ServiceName),
		slog.String("method", entry.Method),
		slog.String("url", entry.FullURL),
		slog.Int("status_code", entry.StatusCode),
		slog.Int64("latency_ms", entry.LatencyMS),
		slog.String("client_ip", entry.IP),
		slog.String("ip_origin", entry.IPOrigin),
		slog.String("correlation_id", entry.CorrelationID),
		slog.String("created_by", entry.CreatedBy),
		slog.Time("created_at", entry.CreatedAt),
		slog.Any("request_headers", entry.RequestHeaders),
		slog.Any("response_headers", entry.ResponseHeaders),
		slog.Any("request_body", entry.RequestBody),
		slog.Any("response_body", entry.ResponseBody),
		slog.String("protocol", entry.Protocol),
		slog.String("user_agent", entry.UserAgent),
		slog.String("error_message", entry.ErrorMessage),
		slog.String("request_id", entry.RequestID),
	)
	return nil
}

// multiLogStore wraps multiple LogStore implementations into one.
type multiLogStore struct {
	stores []LogStore
}

// MultiLogStore combines multiple LogStore instances into a single LogStore.
// It filters out any nil stores, flattens nested MultiLogStore instances,
// and returns the store directly if only one valid store is provided.
func MultiLogStore(stores ...LogStore) LogStore {
	valid := make([]LogStore, 0, len(stores))
	for _, s := range stores {
		if s == nil {
			continue
		}
		if multi, ok := s.(*multiLogStore); ok {
			valid = append(valid, multi.stores...)
		} else {
			valid = append(valid, s)
		}
	}
	if len(valid) == 0 {
		return &multiLogStore{}
	}
	if len(valid) == 1 {
		return valid[0]
	}
	return &multiLogStore{stores: valid}
}

// Save broadcasts the audit log entry to all configured stores sequentially.
func (m *multiLogStore) Save(ctx context.Context, entry *AuditLog) error {
	if m == nil || len(m.stores) == 0 || entry == nil {
		return nil
	}
	var errs []error
	for _, s := range m.stores {
		if err := s.Save(ctx, entry); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
