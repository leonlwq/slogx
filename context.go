package slogx

import (
	"context"
	"log/slog"
)

type ctxKey uint8

const (
	ctxKeyTraceID ctxKey = iota + 1
	ctxKeySpanID
	ctxKeyRequestID
)

// Default attribute keys for context correlation fields.
const (
	TraceIDKey   = "trace_id"
	SpanIDKey    = "span_id"
	RequestIDKey = "request_id"
)

// ContextExtractor extracts structured attributes from a context.Context on each
// log call (for example, bridging OpenTelemetry SpanContext or gateway metadata).
type ContextExtractor func(ctx context.Context) []slog.Attr

func withCtxStr(ctx context.Context, key ctxKey, val string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, key, val)
}

func getCtxStr(ctx context.Context, key ctxKey) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(key).(string)
	return v
}

// WithTraceID returns a derived context carrying traceID.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return withCtxStr(ctx, ctxKeyTraceID, traceID)
}

// TraceID retrieves the trace ID stored by WithTraceID, or "" if absent.
func TraceID(ctx context.Context) string {
	return getCtxStr(ctx, ctxKeyTraceID)
}

// WithSpanID returns a derived context carrying spanID.
func WithSpanID(ctx context.Context, spanID string) context.Context {
	return withCtxStr(ctx, ctxKeySpanID, spanID)
}

// SpanID retrieves the span ID stored by WithSpanID, or "" if absent.
func SpanID(ctx context.Context) string {
	return getCtxStr(ctx, ctxKeySpanID)
}

// WithRequestID returns a derived context carrying requestID.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return withCtxStr(ctx, ctxKeyRequestID, requestID)
}

// RequestID retrieves the request ID stored by WithRequestID, or "" if absent.
func RequestID(ctx context.Context) string {
	return getCtxStr(ctx, ctxKeyRequestID)
}

// extractBuiltinContextAttrs extracts non-empty built-in correlation IDs in a
// single pass without allocating intermediate slices.
func extractBuiltinContextAttrs(ctx context.Context) (traceID, spanID, reqID string, ok bool) {
	traceID, _ = ctx.Value(ctxKeyTraceID).(string)
	spanID, _ = ctx.Value(ctxKeySpanID).(string)
	reqID, _ = ctx.Value(ctxKeyRequestID).(string)
	return traceID, spanID, reqID, traceID != "" || spanID != "" || reqID != ""
}
