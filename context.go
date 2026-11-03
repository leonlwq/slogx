package slogx

import (
	"context"
	"log/slog"
	"sync/atomic"
)

type corrKeyType struct{}

var ctxKeyCorrelation = corrKeyType{}

type corrIDs struct {
	traceID string
	spanID  string
	reqID   string
}

// builtinCtxUsed is flipped once when any built-in correlation setter is used,
// allowing InfoContext/LogAttrs to skip ctx.Value traversal completely in apps
// that rely solely on custom ContextExtractors or plain contexts.
var builtinCtxUsed atomic.Bool

// Default attribute keys for context correlation fields.
const (
	TraceIDKey   = "trace_id"
	SpanIDKey    = "span_id"
	RequestIDKey = "request_id"
)

// ContextExtractor extracts structured attributes from a context.Context on each
// log call (for example, bridging OpenTelemetry SpanContext or gateway metadata).
type ContextExtractor func(ctx context.Context) []slog.Attr

func withCorrelation(ctx context.Context, update func(*corrIDs)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	c := new(corrIDs)
	if prev, ok := ctx.Value(ctxKeyCorrelation).(*corrIDs); ok && prev != nil {
		*c = *prev
	}
	update(c)
	if !builtinCtxUsed.Load() {
		builtinCtxUsed.Store(true)
	}
	return context.WithValue(ctx, ctxKeyCorrelation, c)
}

func getCorrelation(ctx context.Context) (*corrIDs, bool) {
	if ctx == nil || !builtinCtxUsed.Load() {
		return nil, false
	}
	c, ok := ctx.Value(ctxKeyCorrelation).(*corrIDs)
	return c, ok && c != nil
}

// WithTraceID returns a derived context carrying traceID.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return withCorrelation(ctx, func(c *corrIDs) {
		c.traceID = traceID
	})
}

// TraceID retrieves the trace ID stored by WithTraceID, or "" if absent.
func TraceID(ctx context.Context) string {
	if c, ok := getCorrelation(ctx); ok {
		return c.traceID
	}
	return ""
}

// WithSpanID returns a derived context carrying spanID.
func WithSpanID(ctx context.Context, spanID string) context.Context {
	return withCorrelation(ctx, func(c *corrIDs) {
		c.spanID = spanID
	})
}

// SpanID retrieves the span ID stored by WithSpanID, or "" if absent.
func SpanID(ctx context.Context) string {
	if c, ok := getCorrelation(ctx); ok {
		return c.spanID
	}
	return ""
}

// WithRequestID returns a derived context carrying requestID.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return withCorrelation(ctx, func(c *corrIDs) {
		c.reqID = requestID
	})
}

// RequestID retrieves the request ID stored by WithRequestID, or "" if absent.
func RequestID(ctx context.Context) string {
	if c, ok := getCorrelation(ctx); ok {
		return c.reqID
	}
	return ""
}

// extractBuiltinContextAttrs extracts non-empty built-in correlation IDs in a
// single ctx.Value lookup without allocating intermediate slices.
func extractBuiltinContextAttrs(ctx context.Context) (traceID, spanID, reqID string, ok bool) {
	c, found := getCorrelation(ctx)
	if !found {
		return "", "", "", false
	}
	return c.traceID, c.spanID, c.reqID, c.traceID != "" || c.spanID != "" || c.reqID != ""
}
