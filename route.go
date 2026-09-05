package client

import (
	"context"
	"net/http"
	"strings"
)

type contextKey struct {
	name string
}

var routePatternKey = &contextKey{name: "nvx-route-pattern"}

// WithRoutePattern attaches an explicit route pattern (e.g. "/api/v1/users/{id}")
// to the context, allowing callers to bypass heuristic route matching with 100% precision.
func WithRoutePattern(ctx context.Context, pattern string) context.Context {
	return context.WithValue(ctx, routePatternKey, pattern)
}

// RoutePatternFromContext retrieves an explicit route pattern from the context if present.
func RoutePatternFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	val := ctx.Value(routePatternKey)
	if val == nil {
		return "", false
	}
	pattern, ok := val.(string)
	return pattern, ok && pattern != ""
}

// NormalizePath scans an HTTP URL path and replaces dynamic path parameters
// (numbers, UUIDs, Mongo ObjectIDs, hex hashes, ULIDs) with "{param}".
// It preserves static resource names (e.g. "users", "orders", "active", "status", "v1").
func NormalizePath(path string) string {
	// Strip query string or fragments if present
	if idx := strings.IndexAny(path, "?#"); idx != -1 {
		path = path[:idx]
	}

	if path == "" || path == "/" {
		return "/"
	}

	var b strings.Builder
	// Allocate space to minimize reallocations
	b.Grow(len(path) + 8)

	for i := 0; i < len(path); {
		for i < len(path) && path[i] == '/' {
			i++
		}
		if i >= len(path) {
			break
		}
		start := i
		for i < len(path) && path[i] != '/' {
			i++
		}
		seg := path[start:i]

		b.WriteByte('/')
		if isDynamicParam(seg) {
			b.WriteString("{param}")
		} else {
			b.WriteString(seg)
		}
	}

	if b.Len() == 0 {
		return "/"
	}

	return b.String()
}

// DefaultRouteKey generates a canonical route key in the form:
//
//	"METHOD host/normalized_path"
//
// Example: "GET api.service.internal/v1/users/{param}/orders/{param}"
func DefaultRouteKey(req *http.Request) string {
	if req == nil {
		return "UNKNOWN"
	}

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}

	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}

	// Layer 1: Check explicit context pattern
	if pattern, ok := RoutePatternFromContext(req.Context()); ok {
		if !strings.HasPrefix(pattern, "/") {
			pattern = "/" + pattern
		}
		if host != "" {
			return method + " " + host + pattern
		}
		return method + " " + pattern
	}

	// Layer 2: Heuristic path normalization
	path := ""
	if req.URL != nil {
		path = req.URL.Path
	}
	normalized := NormalizePath(path)

	if host != "" {
		return method + " " + host + normalized
	}
	return method + " " + normalized
}

// isDynamicParam checks whether a path segment is a dynamic parameter.
func isDynamicParam(seg string) bool {
	// Check already templated segments: {id}, {userId}, :id
	if (strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")) || strings.HasPrefix(seg, ":") {
		return true
	}

	// Check numeric ID: e.g. "12345"
	if isNumeric(seg) {
		return true
	}

	// Check UUID: 36 chars with dashes
	if isUUID(seg) {
		return true
	}

	// Check Mongo ObjectID (24 hex) or SHA hashes (32, 40, 64 hex)
	if isHexID(seg) {
		return true
	}

	// Check ULID (26 chars) or KSUID (27 chars)
	if isULIDOrKSUID(seg) {
		return true
	}

	return false
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !isHexByte(s[i]) {
			return false
		}
	}
	return true
}

func isHexID(s string) bool {
	// Mongo ObjectId is 24 hex chars; MD5 is 32; SHA1 is 40; SHA256 is 64
	l := len(s)
	if l != 24 && l != 32 && l != 40 && l != 64 {
		return false
	}
	for i := 0; i < l; i++ {
		if !isHexByte(s[i]) {
			return false
		}
	}
	return true
}

func isULIDOrKSUID(s string) bool {
	// ULID is 26 chars; KSUID is 27 chars
	l := len(s)
	if l != 26 && l != 27 {
		return false
	}
	for i := 0; i < l; i++ {
		c := s[i]
		isUpperAlnum := (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		if !isUpperAlnum {
			return false
		}
	}
	return true
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
