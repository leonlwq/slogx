package slogx

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// LoggerNameKey is the attribute key attached to sub-loggers created via Logger.Named.
const LoggerNameKey = "logger"

const (
	allLevels            = slog.Level(math.MinInt)
	noLevelOverride      = math.MinInt64
	maxRegisteredModules = 4096
)

type moduleLevel struct {
	override atomic.Int64
	parent   slog.Leveler
}

func newModuleLevel(parent slog.Leveler) *moduleLevel {
	m := &moduleLevel{parent: parent}
	m.override.Store(noLevelOverride)
	return m
}

func (m *moduleLevel) Level() slog.Level {
	if v := m.override.Load(); v != noLevelOverride {
		return slog.Level(v)
	}
	return m.parent.Level()
}

type moduleRegistry struct {
	mu    sync.Mutex
	root  *slog.LevelVar
	nodes map[string]*moduleLevel
}

func newModuleRegistry(root *slog.LevelVar) *moduleRegistry {
	return &moduleRegistry{
		root:  root,
		nodes: make(map[string]*moduleLevel),
	}
}

func normalizeModuleName(name string) string {
	return strings.Trim(strings.TrimSpace(name), ".")
}

func (r *moduleRegistry) getOrCreate(name string) *moduleLevel {
	name = normalizeModuleName(name)
	if name == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getOrCreateLocked(name)
}

func (r *moduleRegistry) getOrCreateLocked(name string) *moduleLevel {
	if m, ok := r.nodes[name]; ok {
		return m
	}
	var parent slog.Leveler = r.root
	if idx := strings.LastIndexByte(name, '.'); idx > 0 {
		parent = r.getOrCreateLocked(name[:idx])
	}
	m := newModuleLevel(parent)
	if len(r.nodes) < maxRegisteredModules {
		r.nodes[name] = m
	}
	return m
}

func (r *moduleRegistry) effectiveLevel(name string) slog.Level {
	name = normalizeModuleName(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	for cur := name; cur != ""; {
		if m, ok := r.nodes[cur]; ok {
			if v := m.override.Load(); v != noLevelOverride {
				return slog.Level(v)
			}
		}
		idx := strings.LastIndexByte(cur, '.')
		if idx <= 0 {
			break
		}
		cur = cur[:idx]
	}
	return r.root.Level()
}

func (r *moduleRegistry) setLevel(name string, level slog.Level) {
	if m := r.getOrCreate(name); m != nil {
		m.override.Store(int64(level))
	}
}

func (r *moduleRegistry) resetLevel(name string) {
	name = normalizeModuleName(name)
	if name == "" {
		return
	}
	r.mu.Lock()
	m, ok := r.nodes[name]
	r.mu.Unlock()
	if ok {
		m.override.Store(noLevelOverride)
	}
}

func (r *moduleRegistry) snapshot() map[string]slog.Level {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out map[string]slog.Level
	for name, m := range r.nodes {
		if v := m.override.Load(); v != noLevelOverride {
			if out == nil {
				out = make(map[string]slog.Level)
			}
			out[name] = slog.Level(v)
		}
	}
	return out
}

// discardHandler is a zero-allocation no-op slog.Handler compatible with Go 1.22+.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }

// multiHandler fans out log records across multiple slog.Handlers without
// short-circuiting on errors, providing Go 1.26+ MultiHandler semantics on Go 1.22+.
type multiHandler struct {
	handlers []slog.Handler
}

func newMultiHandler(handlers ...slog.Handler) slog.Handler {
	flat := make([]slog.Handler, 0, len(handlers))
	for _, h := range handlers {
		if h == nil {
			continue
		}
		if _, ok := h.(discardHandler); ok {
			continue
		}
		if mh, ok := h.(*multiHandler); ok {
			flat = append(flat, mh.handlers...)
			continue
		}
		flat = append(flat, h)
	}
	switch len(flat) {
	case 0:
		return discardHandler{}
	case 1:
		return flat[0]
	default:
		return &multiHandler{handlers: flat}
	}
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return m
	}
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(slices.Clone(attrs))
	}
	return &multiHandler{handlers: next}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return m
	}
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: next}
}

// filterHandler applies an arbitrary LevelFilter predicate ahead of an underlying slog.Handler.
type filterHandler struct {
	next   slog.Handler
	filter LevelFilter
}

func (f *filterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if f.filter != nil && !f.filter(level) {
		return false
	}
	return f.next.Enabled(ctx, level)
}

func (f *filterHandler) Handle(ctx context.Context, r slog.Record) error {
	if f.filter != nil && !f.filter(r.Level) {
		return nil
	}
	return f.next.Handle(ctx, r)
}

func (f *filterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return f
	}
	return &filterHandler{next: f.next.WithAttrs(attrs), filter: f.filter}
}

func (f *filterHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return f
	}
	return &filterHandler{next: f.next.WithGroup(name), filter: f.filter}
}

// pipelineHandler enforces active logger/module level, enriches records with
// cached PC caller info and context attributes, and flushes buffered file writers
// immediately on >= LevelError.
type pipelineHandler struct {
	inner           slog.Handler // primary handler (AddSource: false, uses PC sync.Map cache)
	innerWithSource slog.Handler // fallback handler when WithGroup is opened so source stays top-level
	leveler         slog.Leveler // active dynamic level (root LevelVar or hierarchical moduleLevel)
	closed          *atomic.Bool
	useNativeSource bool
	addSource       bool
	sourceLevel     slog.Level
	extractors      []ContextExtractor
	errorSyncers    []func() error
	onError         ErrorHandler
}

func (h *pipelineHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if h.closed != nil && h.closed.Load() {
		return false
	}
	if h.leveler != nil && level < h.leveler.Level() {
		return false
	}
	if h.useNativeSource && h.innerWithSource != nil {
		return h.innerWithSource.Enabled(ctx, level)
	}
	return h.inner.Enabled(ctx, level)
}

func (h *pipelineHandler) Handle(ctx context.Context, r slog.Record) error {
	if h.closed != nil && h.closed.Load() {
		return ErrWriterClosed
	}
	var inlineAttrs [4]slog.Attr
	nInline := 0

	// 1. Collect built-in context correlation attributes onto stack buffer.
	if ctx != nil {
		if traceID, spanID, reqID, ok := extractBuiltinContextAttrs(ctx); ok {
			if traceID != "" {
				inlineAttrs[nInline] = slog.String(TraceIDKey, traceID)
				nInline++
			}
			if spanID != "" {
				inlineAttrs[nInline] = slog.String(SpanIDKey, spanID)
				nInline++
			}
			if reqID != "" {
				inlineAttrs[nInline] = slog.String(RequestIDKey, reqID)
				nInline++
			}
		}
	}

	// 2. Collect cached caller source ("pkg/file.go:line") onto stack buffer.
	useNative := h.useNativeSource && h.innerWithSource != nil
	if !useNative && h.addSource && r.Level >= h.sourceLevel && r.PC != 0 {
		if attr, ok := cachedCallerAttr(r.PC); ok {
			inlineAttrs[nInline] = attr
			nInline++
		}
	}

	cloned := false
	if nInline > 0 {
		r = r.Clone()
		cloned = true
		r.AddAttrs(inlineAttrs[:nInline]...)
	}

	if ctx != nil && len(h.extractors) > 0 {
		for _, ext := range h.extractors {
			if attrs := ext(ctx); len(attrs) > 0 {
				if !cloned {
					r = r.Clone()
					cloned = true
				}
				r.AddAttrs(attrs...)
			}
		}
	}

	var err error
	if useNative {
		if !h.addSource || r.Level < h.sourceLevel {
			r.PC = 0
		}
		err = h.innerWithSource.Handle(ctx, r)
	} else {
		err = h.inner.Handle(ctx, r)
	}
	if err != nil && h.onError != nil {
		h.onError(err)
	}

	// 3. Auto-flush buffered writers on Error and above so crash diagnostics aren't lost.
	if r.Level >= slog.LevelError && len(h.errorSyncers) > 0 {
		for _, syncFn := range h.errorSyncers {
			if syncErr := syncFn(); syncErr != nil && h.onError != nil {
				h.onError(syncErr)
			}
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
