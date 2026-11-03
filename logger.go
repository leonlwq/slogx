package slogx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"

	"github.com/leonlwq/slogx/internal/lumberjack"
)

// Logger wraps *slog.Logger with dynamic level control, buffered/async I/O
// flushing, rolling file rotation, and graceful resource cleanup.
type Logger struct {
	*slog.Logger
	levelVar     *slog.LevelVar
	syncers      []func() error
	rotators     []func() error
	closers      []func() error
	asyncWriters []*AsyncWriter
	sampler      *SamplingHandler
	closed       atomic.Bool
}

// New constructs a new Logger with the given options.
func New(opts ...Option) (*Logger, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	mainFmt, err := parseFormat(cfg.format)
	if err != nil {
		return nil, err
	}

	lvlVar := &slog.LevelVar{}
	if cfg.hasLevelStr {
		if err := lvlVar.UnmarshalText([]byte(strings.TrimSpace(cfg.levelStr))); err != nil {
			return nil, fmt.Errorf("slogx: invalid log level %q: %w", cfg.levelStr, err)
		}
	} else {
		lvlVar.Set(cfg.level)
	}

	sensitiveSet := normalizeSensitiveKeySet(cfg.sensitiveKeys)
	fastReplace := buildReplaceAttr(cfg.timeFormat, sensitiveSet, cfg.maxFieldLen, cfg.replaceAttr, false)
	nativeSrcReplace := buildReplaceAttr(cfg.timeFormat, sensitiveSet, cfg.maxFieldLen, cfg.replaceAttr, true)

	l := &Logger{
		levelVar: lvlVar,
	}

	var primaryHandlers []slog.Handler
	var nativeSrcHandlers []slog.Handler
	var errorSyncers []func() error

	addSink := func(w io.Writer, f Format, leveler slog.Leveler) {
		primaryHandlers = append(primaryHandlers, newStdHandler(w, f, &slog.HandlerOptions{
			AddSource:   false,
			Level:       leveler,
			ReplaceAttr: fastReplace,
		}))
		if cfg.addSource {
			nativeSrcHandlers = append(nativeSrcHandlers, newStdHandler(w, f, &slog.HandlerOptions{
				AddSource:   true,
				Level:       leveler,
				ReplaceAttr: nativeSrcReplace,
			}))
		}
	}

	// 1. Console sink (os.Stdout by default)
	if cfg.consoleEnabled {
		cw := cfg.consoleWriter
		if cw == nil {
			cw = os.Stdout
		}
		cf := mainFmt
		if cfg.consoleFormat != "" {
			cf, err = parseFormat(cfg.consoleFormat)
			if err != nil {
				return nil, err
			}
		}
		addSink(cw, cf, lvlVar)
	}

	// 2. Custom io.Writer sinks
	for _, cw := range cfg.customWriters {
		addSink(cw, mainFmt, lvlVar)
		if cw != os.Stdout && cw != os.Stderr {
			if _, ok := cw.(interface{ Sync() error }); ok {
				w := cw
				l.syncers = append(l.syncers, func() error {
					return syncUnderlyingDst(w)
				})
			}
		}
	}

	// Helper to wire rolling file + BufferedWriter/AsyncWriter
	setupFileSink := func(fo *FileOptions, leveler slog.Leveler, syncOnError bool) error {
		if strings.TrimSpace(fo.Filename) == "" {
			return errors.New("slogx: file output requires a non-empty Filename")
		}
		ff := mainFmt
		if fo.Format != "" {
			var ferr error
			ff, ferr = parseFormat(fo.Format)
			if ferr != nil {
				return ferr
			}
		}

		lj := &lumberjack.Logger{
			Filename:         fo.Filename,
			MaxSize:          fo.MaxSizeMB,
			MaxBackups:       fo.MaxBackups,
			MaxAge:           fo.MaxAgeDays,
			LocalTime:        fo.LocalTime,
			Compress:         fo.Compress,
			RotationInterval: fo.RotationInterval,
		}

		clampBufSize := func(sz int) int {
			if sz <= 0 {
				sz = defaultBufferSize
			}
			maxMB := fo.MaxSizeMB
			if maxMB <= 0 {
				maxMB = 100
			}
			maxBytes := maxMB * 1024 * 1024
			if sz > maxBytes {
				sz = maxBytes
			}
			return sz
		}

		var sinkWriter io.Writer
		var syncFn func() error
		var errFlushFn func() error
		var closeFn func() error

		if cfg.asyncEnabled {
			aw := NewAsyncWriter(
				lj,
				cfg.asyncOpts.QueueSize,
				clampBufSize(cfg.asyncOpts.BufferSize),
				cfg.asyncOpts.Policy,
				ff == FormatJSON,
			)
			sinkWriter = aw
			syncFn = aw.Sync
			closeFn = aw.Close
			l.asyncWriters = append(l.asyncWriters, aw)
		} else if cfg.disableBuffer {
			sinkWriter = &closeGuardWriter{dst: lj, closed: &l.closed}
			syncFn = func() error { return syncUnderlyingDst(lj) }
			closeFn = lj.Close
		} else {
			bw := NewBufferedWriter(lj, clampBufSize(cfg.bufSize), cfg.flushInterval)
			sinkWriter = bw
			syncFn = bw.Sync
			errFlushFn = bw.Flush
			closeFn = bw.Close
		}

		l.syncers = append(l.syncers, syncFn)
		l.closers = append(l.closers, closeFn)
		l.rotators = append(l.rotators, func() error {
			_ = syncFn()
			return lj.Rotate()
		})
		if syncOnError && errFlushFn != nil {
			errorSyncers = append(errorSyncers, errFlushFn)
		}

		addSink(sinkWriter, ff, leveler)
		return nil
	}

	// 3. Application rolling file sink
	if cfg.appFile != nil {
		if err := setupFileSink(cfg.appFile, lvlVar, true); err != nil {
			_ = l.Close()
			return nil, err
		}
	}

	// 4. Dedicated Error rolling file sink (>= slog.LevelError floor)
	if cfg.errFile != nil {
		errLeveler := minLeveler{dynamic: lvlVar, floor: slog.LevelError}
		if err := setupFileSink(cfg.errFile, errLeveler, true); err != nil {
			_ = l.Close()
			return nil, err
		}
	}

	if cfg.enableSIGHUP && len(l.rotators) > 0 {
		stopSig := watchSIGHUP(func() {
			_ = l.Rotate()
		})
		l.closers = append([]func() error{func() error {
			stopSig()
			return nil
		}}, l.closers...)
	}

	// Combine sinks using Go 1.27's native MultiHandler (or DiscardHandler if all sinks disabled)
	inner := combineHandlers(primaryHandlers)
	var innerWithSrc slog.Handler
	if cfg.addSource {
		innerWithSrc = combineHandlers(nativeSrcHandlers)
	}

	srcLevel := slog.LevelDebug - 100 // all levels when addSource is enabled without explicit floor
	if cfg.hasSourceLevel {
		srcLevel = cfg.sourceLevel
	}

	var rootHandler slog.Handler = &pipelineHandler{
		inner:           inner,
		innerWithSource: innerWithSrc,
		addSource:       cfg.addSource,
		sourceLevel:     srcLevel,
		extractors:      cfg.extractors,
		errorSyncers:    errorSyncers,
	}

	// Pre-format static service metadata and root attributes once at initialization
	preAttrs := buildServiceAttrs(cfg.serviceInfo, cfg.staticAttrs)
	if len(preAttrs) > 0 {
		rootHandler = rootHandler.WithAttrs(preAttrs)
	}

	// Optional outer SamplingHandler so suppressed storm logs skip PC lookup, Context extraction, and formatting
	if cfg.sampling != nil {
		sh := NewSamplingHandler(rootHandler, *cfg.sampling)
		l.sampler = sh
		rootHandler = sh
	}

	l.Logger = slog.New(rootHandler)
	if cfg.setDefault {
		slog.SetDefault(l.Logger)
	}
	return l, nil
}

// MustNew is like New but panics if configuration fails.
func MustNew(opts ...Option) *Logger {
	l, err := New(opts...)
	if err != nil {
		panic(err)
	}
	return l
}

// SetDefault sets this logger as the global default slog.Logger and returns l.
func (l *Logger) SetDefault() *Logger {
	slog.SetDefault(l.Logger)
	return l
}

// Slog returns the embedded *slog.Logger for APIs expecting the standard type.
func (l *Logger) Slog() *slog.Logger {
	return l.Logger
}

// StdLogLogger returns a standard library *log.Logger bridged to this logger
// at the specified level (ideal for http.Server.ErrorLog).
func (l *Logger) StdLogLogger(level slog.Level) *log.Logger {
	return slog.NewLogLogger(l.Logger.Handler(), level)
}

// SetLevel atomically updates the minimum log level at runtime.
func (l *Logger) SetLevel(level slog.Level) {
	l.levelVar.Set(level)
}

// SetLevelString atomically updates the minimum log level from a string
// such as "debug", "info", "warn", or "error" (case-insensitive).
func (l *Logger) SetLevelString(level string) error {
	return l.levelVar.UnmarshalText([]byte(strings.TrimSpace(level)))
}

// Level returns the current minimum log level.
func (l *Logger) Level() slog.Level {
	return l.levelVar.Level()
}

// LevelVar returns the underlying *slog.LevelVar for direct integration.
func (l *Logger) LevelVar() *slog.LevelVar {
	return l.levelVar
}

// LevelHandler returns a zero-dependency net/http.Handler for inspecting and
// dynamically updating the logger's minimum level at runtime:
//   - GET: returns {"level":"INFO"}
//   - PUT / POST: accepts ?level=debug, JSON {"level":"debug"}, or plain text body "debug"
func (l *Logger) LevelHandler() http.Handler {
	type levelPayload struct {
		Level string `json:"level"`
	}
	type errorPayload struct {
		Error string `json:"error"`
	}

	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, levelPayload{Level: l.Level().String()})

		case http.MethodPut, http.MethodPost:
			rawLevel := strings.TrimSpace(r.URL.Query().Get("level"))
			if rawLevel == "" && r.Body != nil {
				body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
				if err != nil {
					writeJSON(w, http.StatusBadRequest, errorPayload{Error: "failed to read request body"})
					return
				}
				trimmed := strings.TrimSpace(string(body))
				if strings.HasPrefix(trimmed, "{") {
					var p levelPayload
					if err := json.Unmarshal([]byte(trimmed), &p); err != nil {
						writeJSON(w, http.StatusBadRequest, errorPayload{Error: "invalid JSON payload"})
						return
					}
					rawLevel = strings.TrimSpace(p.Level)
				} else {
					rawLevel = trimmed
				}
			}
			if rawLevel == "" {
				writeJSON(w, http.StatusBadRequest, errorPayload{Error: "missing level parameter"})
				return
			}
			if err := l.SetLevelString(rawLevel); err != nil {
				writeJSON(w, http.StatusBadRequest, errorPayload{Error: err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, levelPayload{Level: l.Level().String()})

		default:
			w.Header().Set("Allow", "GET, PUT, POST")
			writeJSON(w, http.StatusMethodNotAllowed, errorPayload{Error: "method not allowed"})
		}
	})
}

// AsyncDropped returns the total number of log records dropped across all
// AsyncWriters due to queue overflow.
func (l *Logger) AsyncDropped() uint64 {
	var total uint64
	for _, aw := range l.asyncWriters {
		total += aw.Dropped()
	}
	return total
}

// SampledDropped returns the total number of duplicate log records suppressed
// by SamplingHandler, or 0 if sampling is disabled.
func (l *Logger) SampledDropped() uint64 {
	if l.sampler == nil {
		return 0
	}
	return l.sampler.Dropped()
}

// Sync flushes all buffered and asynchronous writers to their underlying files.
func (l *Logger) Sync() error {
	if l.closed.Load() {
		return nil
	}
	var errs []error
	for _, fn := range l.syncers {
		if err := fn(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Rotate flushes buffered data and immediately rotates all configured rolling files.
func (l *Logger) Rotate() error {
	if l.closed.Load() {
		return ErrWriterClosed
	}
	var errs []error
	for _, fn := range l.rotators {
		if err := fn(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close flushes pending logs, stops background flushers/consumers/signal watchers,
// and closes all open log files. Safe to call multiple times.
func (l *Logger) Close() error {
	if !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	var errs []error
	for _, fn := range l.closers {
		if err := fn(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func parseFormat(f Format) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(string(f))) {
	case "", string(FormatJSON):
		return FormatJSON, nil
	case string(FormatText):
		return FormatText, nil
	default:
		return "", fmt.Errorf("slogx: unsupported format %q (expected %q or %q)", f, FormatJSON, FormatText)
	}
}

func newStdHandler(w io.Writer, f Format, opts *slog.HandlerOptions) slog.Handler {
	if f == FormatText {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

func combineHandlers(handlers []slog.Handler) slog.Handler {
	switch len(handlers) {
	case 0:
		return slog.DiscardHandler
	case 1:
		return handlers[0]
	default:
		return slog.NewMultiHandler(handlers...)
	}
}

func buildServiceAttrs(info *ServiceInfo, extra []slog.Attr) []slog.Attr {
	var attrs []slog.Attr
	if info != nil {
		if info.Name != "" {
			attrs = append(attrs, slog.String("service", info.Name))
		}
		if info.Version != "" {
			attrs = append(attrs, slog.String("version", info.Version))
		}
		if info.Env != "" {
			attrs = append(attrs, slog.String("env", info.Env))
		}
		inst := info.Instance
		if inst == "" && info.IncludeHost {
			inst = detectInstanceName()
		}
		if inst != "" {
			attrs = append(attrs, slog.String("instance", inst))
		}
	}
	attrs = append(attrs, extra...)
	return attrs
}

func detectInstanceName() string {
	for _, key := range []string{"POD_NAME", "HOSTNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}
