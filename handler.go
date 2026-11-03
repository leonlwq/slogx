package slogx

import (
	"context"
	"log/slog"
)

// minLeveler enforces a floor level on top of a dynamic Leveler (used for
// error.log so switching the main logger to DEBUG never pollutes error.log).
type minLeveler struct {
	dynamic slog.Leveler
	floor   slog.Level
}

func (m minLeveler) Level() slog.Level {
	if m.dynamic != nil {
		if dl := m.dynamic.Level(); dl > m.floor {
			return dl
		}
	}
	return m.floor
}

// pipelineHandler enriches records with cached PC caller info and context
// attributes, and flushes buffered file writers immediately on >= LevelError.
type pipelineHandler struct {
	inner           slog.Handler // primary handler (AddSource: false, uses PC sync.Map cache)
	innerWithSource slog.Handler // fallback handler when WithGroup is opened so source stays top-level
	useNativeSource bool
	addSource       bool
	sourceLevel     slog.Level
	extractors      []ContextExtractor
	errorSyncers    []func() error
}

func (h *pipelineHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.useNativeSource && h.innerWithSource != nil {
		return h.innerWithSource.Enabled(ctx, level)
	}
	return h.inner.Enabled(ctx, level)
}

func (h *pipelineHandler) Handle(ctx context.Context, r slog.Record) error {
	cloned := false
	ensureClone := func() {
		if !cloned {
			r = r.Clone()
			cloned = true
		}
	}

	// 1. Inject built-in & custom context correlation attributes.
	if ctx != nil {
		if traceID, spanID, reqID, ok := extractBuiltinContextAttrs(ctx); ok {
			ensureClone()
			if traceID != "" {
				r.AddAttrs(slog.String(TraceIDKey, traceID))
			}
			if spanID != "" {
				r.AddAttrs(slog.String(SpanIDKey, spanID))
			}
			if reqID != "" {
				r.AddAttrs(slog.String(RequestIDKey, reqID))
			}
		}
		for _, ext := range h.extractors {
			if attrs := ext(ctx); len(attrs) > 0 {
				ensureClone()
				r.AddAttrs(attrs...)
			}
		}
	}

	// 2. Inject cached caller source ("pkg/file.go:line") or delegate to native grouped handler.
	var err error
	if h.useNativeSource && h.innerWithSource != nil {
		if !h.addSource || r.Level < h.sourceLevel {
			r.PC = 0
		}
		err = h.innerWithSource.Handle(ctx, r)
	} else {
		if h.addSource && r.Level >= h.sourceLevel && r.PC != 0 {
			if attr, ok := cachedCallerAttr(r.PC); ok {
				ensureClone()
				r.AddAttrs(attr)
			}
		}
		err = h.inner.Handle(ctx, r)
	}

	// 3. Auto-sync buffered writers on Error and above so crash diagnostics aren't lost.
	if r.Level >= slog.LevelError && len(h.errorSyncers) > 0 {
		for _, syncFn := range h.errorSyncers {
			_ = syncFn()
		}
	}
	return err
}

func (h *pipelineHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	cp := *h
	if cp.useNativeSource && cp.innerWithSource != nil {
		cp.innerWithSource = cp.innerWithSource.WithAttrs(attrs)
		return &cp
	}
	cp.inner = cp.inner.WithAttrs(attrs)
	if cp.innerWithSource != nil {
		cp.innerWithSource = cp.innerWithSource.WithAttrs(attrs)
	}
	return &cp
}

func (h *pipelineHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	cp := *h
	if cp.addSource && cp.innerWithSource != nil {
		cp.useNativeSource = true
		cp.innerWithSource = cp.innerWithSource.WithGroup(name)
		return &cp
	}
	cp.inner = cp.inner.WithGroup(name)
	return &cp
}
