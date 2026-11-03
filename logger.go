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
	"sync"
	"sync/atomic"

	"github.com/leonlwq/slogx/internal/lumberjack"
)

type loggerCore struct {
	levelVar     *slog.LevelVar
	modules      *moduleRegistry
	syncers      []func() error
	rotators     []func() error
	closers      []func() error
	asyncWriters []*AsyncWriter
	sampler      *SamplingHandler
	onError      ErrorHandler
	buildHandler func(leveler slog.Leveler, extraAttrs []slog.Attr) slog.Handler
	closed       atomic.Bool
}

func (c *loggerCore) reportErr(err error) {
	if err != nil && c.onError != nil {
		c.onError(err)
	}
}

// Logger wraps *slog.Logger with dynamic global & per-module level control,
// multi-sink routing, buffered/async I/O flushing, rolling file rotation, and
// graceful resource cleanup.
type Logger struct {
	*slog.Logger
	core        *loggerCore
	moduleName  string
	activeLevel slog.Leveler
}

// New constructs a new Logger with the given options.
func New(opts ...Option) (*Logger, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.cfgErr != nil {
		return nil, cfg.cfgErr
	}

	mainFmt, err := parseFormat(cfg.format)
	if err != nil {
		return nil, err
	}

	lvlVar := &slog.LevelVar{}
	lvlVar.Set(cfg.level)

	modules := newModuleRegistry(lvlVar)
	for _, ms := range cfg.moduleLevels {
		modules.setLevel(ms.module, ms.level)
	}

	core := &loggerCore{
		levelVar: lvlVar,
		modules:  modules,
		onError:  cfg.onError,
	}
	l := &Logger{
		core:        core,
		activeLevel: lvlVar,
	}

	sb := &sinkBuilder{
		cfg:     &cfg,
		core:    core,
		mainFmt: mainFmt,
	}
	if err := sb.setupAll(); err != nil {
		_ = l.Close()
		return nil, err
	}

	if cfg.enableSIGHUP && len(core.rotators) > 0 {
		stopSig := watchSIGHUP(func() {
			if err := l.Rotate(); err != nil {
				core.reportErr(err)
			}
		})
		core.closers = append([]func() error{func() error {
			stopSig()
			return nil
		}}, core.closers...)
	}

	sensitive := newSensitiveMatcher(cfg.sensitiveKeys)
	fastReplace := buildReplaceAttr(cfg.timeFormat, sensitive, cfg.maxFieldLen, cfg.replaceAttr, false)
	nativeSrcReplace := buildReplaceAttr(cfg.timeFormat, sensitive, cfg.maxFieldLen, cfg.replaceAttr, true)
	inner, innerWithSrc := sb.buildHandlers(fastReplace, nativeSrcReplace)

	srcLevel := slog.LevelDebug - 100
	if cfg.hasSourceLevel {
		srcLevel = cfg.sourceLevel
	}
	preAttrs := buildServiceAttrs(cfg.serviceInfo, cfg.staticAttrs)
	if cfg.sampling != nil {
		core.sampler = NewSamplingHandler(discardHandler{}, *cfg.sampling)
	}

	errorSyncers := sb.errorSyncers
	core.buildHandler = func(leveler slog.Leveler, extraAttrs []slog.Attr) slog.Handler {
		var h slog.Handler = &pipelineHandler{
			inner:           inner,
			innerWithSource: innerWithSrc,
			leveler:         leveler,
			closed:          &core.closed,
			addSource:       cfg.addSource,
			sourceLevel:     srcLevel,
			extractors:      cfg.extractors,
			errorSyncers:    errorSyncers,
			onError:         cfg.onError,
		}
		if len(preAttrs) > 0 {
			h = h.WithAttrs(preAttrs)
		}
		if len(extraAttrs) > 0 {
			h = h.WithAttrs(extraAttrs)
		}
		if core.sampler != nil {
			h = newSamplingHandlerWithState(h, core.sampler)
		}
		for i := len(cfg.middlewares) - 1; i >= 0; i-- {
			h = cfg.middlewares[i](h)
		}
		return h
	}

	l.Logger = slog.New(core.buildHandler(lvlVar, nil))
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

// Named returns a child Logger scoped to the given module name (hierarchical
// when chained, e.g. l.Named("http").Named("auth") -> "http.auth").
// The child Logger pre-formats a "logger" attribute, shares all underlying
// sinks and lifecycle with l, and supports independent dynamic level overrides
// via SetModuleLevel, SetLevel, or LevelHandler.
func (l *Logger) Named(name string) *Logger {
	sub := normalizeModuleName(name)
	if sub == "" {
		return l
	}
	full := sub
	if l.moduleName != "" {
		full = l.moduleName + "." + sub
	}
	modLevel := l.core.modules.getOrCreate(full)
	h := l.core.buildHandler(modLevel, []slog.Attr{slog.String(LoggerNameKey, full)})
	return &Logger{
		Logger:      slog.New(h),
		core:        l.core,
		moduleName:  full,
		activeLevel: modLevel,
	}
}

// Name returns the hierarchical module name of a Named sub-logger, or "" for the root Logger.
func (l *Logger) Name() string {
	return l.moduleName
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
// On a root Logger it updates the global level; on a Named sub-logger it sets
// that module's level override.
func (l *Logger) SetLevel(level slog.Level) {
	if l.moduleName != "" {
		l.core.modules.setLevel(l.moduleName, level)
		return
	}
	l.core.levelVar.Set(level)
}

// SetLevelString atomically updates the minimum log level from a string
// such as "debug", "info", "warn", or "error" (case-insensitive).
func (l *Logger) SetLevelString(level string) error {
	if l.moduleName != "" {
		return l.SetModuleLevelString(l.moduleName, level)
	}
	return l.core.levelVar.UnmarshalText([]byte(strings.TrimSpace(level)))
}

// ResetLevel clears any module-specific level override on a Named sub-logger so
// it resumes inheriting from its parent/root Logger. No-op on the root Logger.
func (l *Logger) ResetLevel() {
	if l.moduleName != "" {
		l.core.modules.resetLevel(l.moduleName)
	}
}

// Level returns the effective minimum log level of this Logger (accounting for
// any module-level override on Named sub-loggers).
func (l *Logger) Level() slog.Level {
	return l.activeLevel.Level()
}

// LevelVar returns the root *slog.LevelVar for direct stdlib integration.
func (l *Logger) LevelVar() *slog.LevelVar {
	return l.core.levelVar
}

// SetModuleLevel atomically sets an independent runtime log level override for
// the named module (e.g. "db" or "http.auth").
func (l *Logger) SetModuleLevel(module string, level slog.Level) {
	l.core.modules.setLevel(module, level)
}

// SetModuleLevelString parses level and sets an independent runtime override
// for the named module.
func (l *Logger) SetModuleLevelString(module, level string) error {
	mod := normalizeModuleName(module)
	if mod == "" {
		return errors.New("slogx: module name cannot be empty")
	}
	parsed, err := parseLevelString(level)
	if err != nil {
		return err
	}
	l.core.modules.setLevel(mod, parsed)
	return nil
}

// ResetModuleLevel clears any runtime level override for module so it inherits
// from its parent module or root Logger level.
func (l *Logger) ResetModuleLevel(module string) {
	l.core.modules.resetLevel(module)
}

// ModuleLevels returns a snapshot of all currently active module level overrides.
func (l *Logger) ModuleLevels() map[string]slog.Level {
	return l.core.modules.snapshot()
}

// LevelHandler returns a zero-dependency net/http.Handler for inspecting and
// dynamically updating global and per-module log levels at runtime:
//   - GET: returns {"level":"INFO"} (plus "modules" if overrides exist), or ?module=db for a module's effective level
//   - PUT / POST: accepts ?level=debug, ?module=db&level=debug, ?module=db&level=reset,
//     JSON {"level":"debug","module":"db"}, or plain text body "debug"
func (l *Logger) LevelHandler() http.Handler {
	type levelPayload struct {
		Level   string            `json:"level"`
		Module  string            `json:"module,omitempty"`
		Modules map[string]string `json:"modules,omitempty"`
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
			qMod := normalizeModuleName(r.URL.Query().Get("module"))
			if qMod != "" {
				eff := l.core.modules.effectiveLevel(qMod)
				writeJSON(w, http.StatusOK, levelPayload{
					Level:  eff.String(),
					Module: qMod,
				})
				return
			}
			snap := l.core.modules.snapshot()
			var mods map[string]string
			if len(snap) > 0 {
				mods = make(map[string]string, len(snap))
				for k, v := range snap {
					mods[k] = v.String()
				}
			}
			writeJSON(w, http.StatusOK, levelPayload{
				Level:   l.core.levelVar.Level().String(),
				Modules: mods,
			})

		case http.MethodPut, http.MethodPost:
			rawLevel := strings.TrimSpace(r.URL.Query().Get("level"))
			rawModule := normalizeModuleName(r.URL.Query().Get("module"))

			if (rawLevel == "" || rawModule == "") && r.Body != nil {
				body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
				if err != nil {
					writeJSON(w, http.StatusBadRequest, errorPayload{Error: "failed to read request body"})
					return
				}
				trimmed := strings.TrimSpace(string(body))
				if trimmed != "" {
					if strings.HasPrefix(trimmed, "{") {
						var p levelPayload
						if err := json.Unmarshal([]byte(trimmed), &p); err != nil {
							writeJSON(w, http.StatusBadRequest, errorPayload{Error: "invalid JSON payload"})
							return
						}
						if rawLevel == "" {
							rawLevel = strings.TrimSpace(p.Level)
						}
						if rawModule == "" {
							rawModule = normalizeModuleName(p.Module)
						}
					} else if rawLevel == "" {
						rawLevel = trimmed
					}
				}
			}
			if rawLevel == "" {
				writeJSON(w, http.StatusBadRequest, errorPayload{Error: "missing level parameter"})
				return
			}

			if rawModule != "" {
				switch strings.ToLower(rawLevel) {
				case "reset", "default", "inherit":
					l.ResetModuleLevel(rawModule)
				default:
					if err := l.SetModuleLevelString(rawModule, rawLevel); err != nil {
						writeJSON(w, http.StatusBadRequest, errorPayload{Error: err.Error()})
						return
					}
				}
				eff := l.core.modules.effectiveLevel(rawModule)
				writeJSON(w, http.StatusOK, levelPayload{
					Level:  eff.String(),
					Module: rawModule,
				})
				return
			}

			if err := l.core.levelVar.UnmarshalText([]byte(rawLevel)); err != nil {
				writeJSON(w, http.StatusBadRequest, errorPayload{Error: err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, levelPayload{Level: l.core.levelVar.Level().String()})

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
	for _, aw := range l.core.asyncWriters {
		total += aw.Dropped()
	}
	return total
}

// SampledDropped returns the total number of duplicate log records suppressed
// by SamplingHandler across root and Named sub-loggers, or 0 if disabled.
func (l *Logger) SampledDropped() uint64 {
	if l.core.sampler == nil {
		return 0
	}
	return l.core.sampler.Dropped()
}

// Sync flushes all buffered and asynchronous writers to their underlying files.
func (l *Logger) Sync() error {
	if l.core.closed.Load() {
		return nil
	}
	var errs []error
	for _, fn := range l.core.syncers {
		if err := fn(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Rotate flushes buffered data and immediately rotates all configured rolling files.
func (l *Logger) Rotate() error {
	if l.core.closed.Load() {
		return ErrWriterClosed
	}
	var errs []error
	for _, fn := range l.core.rotators {
		if err := fn(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close flushes pending logs, stops background flushers/consumers/signal watchers,
// and closes all open log files. Safe to call multiple times.
func (l *Logger) Close() error {
	if !l.core.closed.CompareAndSwap(false, true) {
		return nil
	}
	var errs []error
	for _, fn := range l.core.closers {
		if err := fn(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type sinkEntry struct {
	format   Format
	colorize bool
	leveler  slog.Leveler
	filter   LevelFilter
	writers  []io.Writer
}

type sinkBuilder struct {
	cfg          *config
	core         *loggerCore
	mainFmt      Format
	entries      []sinkEntry
	errorSyncers []func() error
}

func (sb *sinkBuilder) setupAll() error {
	if err := sb.addConsole(); err != nil {
		return err
	}
	if sb.cfg.appFile != nil {
		if err := sb.addFile(sb.cfg.appFile, nil, nil, nil, false, true); err != nil {
			return err
		}
	}
	if sb.cfg.errFile != nil {
		if err := sb.addFile(sb.cfg.errFile, nil, slog.LevelError, nil, false, true); err != nil {
			return err
		}
	}
	for _, so := range sb.cfg.sinks {
		if err := sb.addSinkOption(so); err != nil {
			return err
		}
	}
	return nil
}

func (sb *sinkBuilder) addConsole() error {
	if !sb.cfg.consoleEnabled {
		return nil
	}
	cw := sb.cfg.consoleWriter
	if cw == nil {
		cw = os.Stdout
	}
	cf := sb.mainFmt
	if sb.cfg.consoleFormat != "" {
		var err error
		cf, err = parseFormat(sb.cfg.consoleFormat)
		if err != nil {
			return err
		}
	}
	sb.addEntry(cw, cf, sb.cfg.consoleColor, nil, nil)
	return nil
}

func (sb *sinkBuilder) addFile(fo *FileOptions, colorOpt *bool, leveler slog.Leveler, filter LevelFilter, forceUnbuffered, syncOnError bool) error {
	if strings.TrimSpace(fo.Filename) == "" {
		return errors.New("slogx: file output requires a non-empty Filename")
	}
	ff := sb.mainFmt
	if fo.Format != "" {
		var err error
		ff, err = parseFormat(fo.Format)
		if err != nil {
			return err
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
		OnError:          sb.cfg.onError,
	}

	var sinkWriter io.Writer
	var syncFn func() error
	var errFlushFn func() error
	var closeFn func() error

	if forceUnbuffered || (!sb.cfg.asyncEnabled && sb.cfg.disableBuffer) {
		sinkWriter = &closeGuardWriter{dst: lj, closed: &sb.core.closed}
		syncFn = func() error { return syncUnderlyingDst(lj) }
		closeFn = lj.Close
	} else if sb.cfg.asyncEnabled {
		aw := newAsyncWriter(
			lj,
			sb.cfg.asyncOpts.QueueSize,
			clampFileBufferSize(sb.cfg.asyncOpts.BufferSize, fo.MaxSizeMB),
			sb.cfg.asyncOpts.Policy,
			ff == FormatJSON,
			sb.cfg.onError,
		)
		sinkWriter = aw
		syncFn = aw.Sync
		closeFn = aw.Close
		sb.core.asyncWriters = append(sb.core.asyncWriters, aw)
	} else {
		bw := newBufferedWriter(
			lj,
			clampFileBufferSize(sb.cfg.bufSize, fo.MaxSizeMB),
			sb.cfg.flushInterval,
			sb.cfg.onError,
		)
		sinkWriter = bw
		syncFn = bw.Sync
		errFlushFn = bw.Flush
		closeFn = bw.Close
	}

	sb.core.syncers = append(sb.core.syncers, syncFn)
	sb.core.closers = append(sb.core.closers, closeFn)
	sb.core.rotators = append(sb.core.rotators, func() error {
		return errors.Join(syncFn(), lj.Rotate())
	})
	if syncOnError && errFlushFn != nil {
		sb.errorSyncers = append(sb.errorSyncers, errFlushFn)
	}

	sb.addEntry(sinkWriter, ff, colorOpt, leveler, filter)
	return nil
}

func (sb *sinkBuilder) addSinkOption(so SinkOptions) error {
	if (so.Writer == nil && so.File == nil) || (so.Writer != nil && so.File != nil) {
		return errors.New("slogx: SinkOptions requires exactly one of Writer or File")
	}
	if so.File != nil {
		fo := *so.File
		if fo.Format == "" && so.Format != "" {
			fo.Format = so.Format
		}
		if err := sb.addFile(&fo, so.Color, so.Level, so.LevelFilter, so.DisableBuffer, true); err != nil {
			return err
		}
		if so.SyncFn != nil {
			sb.core.syncers = append(sb.core.syncers, so.SyncFn)
		}
		if so.CloseFn != nil {
			sb.core.closers = append(sb.core.closers, so.CloseFn)
		}
		return nil
	}

	sf := sb.mainFmt
	if so.Format != "" {
		var err error
		sf, err = parseFormat(so.Format)
		if err != nil {
			return err
		}
	}
	w := so.Writer
	if so.CloseFn != nil {
		w = &closeGuardWriter{dst: w, closed: &sb.core.closed}
		sb.core.closers = append(sb.core.closers, so.CloseFn)
	}
	sb.registerWriterSyncer(so.Writer, so.SyncFn)
	sb.addEntry(w, sf, so.Color, so.Level, so.LevelFilter)
	return nil
}

func (sb *sinkBuilder) addEntry(w io.Writer, f Format, colorOpt *bool, leveler slog.Leveler, filter LevelFilter) {
	colorize := f == FormatPretty && shouldColorize(w, colorOpt)
	if filter == nil && isShareableLeveler(leveler) {
		for i := range sb.entries {
			if sb.entries[i].filter == nil &&
				sb.entries[i].format == f &&
				sb.entries[i].colorize == colorize &&
				sameLeveler(sb.entries[i].leveler, leveler) {
				sb.entries[i].writers = append(sb.entries[i].writers, w)
				return
			}
		}
	}
	sb.entries = append(sb.entries, sinkEntry{
		format:   f,
		colorize: colorize,
		leveler:  leveler,
		filter:   filter,
		writers:  []io.Writer{w},
	})
}

func (sb *sinkBuilder) registerWriterSyncer(w io.Writer, explicitSync func() error) {
	if explicitSync != nil {
		sb.core.syncers = append(sb.core.syncers, explicitSync)
		return
	}
	if w != os.Stdout && w != os.Stderr {
		if _, ok := w.(interface{ Sync() error }); ok {
			dst := w
			sb.core.syncers = append(sb.core.syncers, func() error {
				return syncUnderlyingDst(dst)
			})
		}
	}
}

func (sb *sinkBuilder) buildHandlers(
	fastReplace func([]string, slog.Attr) slog.Attr,
	nativeSrcReplace func([]string, slog.Attr) slog.Attr,
) (slog.Handler, slog.Handler) {
	totalHandlers := len(sb.entries) + len(sb.cfg.externalHandlers)
	primaryHandlers := make([]slog.Handler, 0, totalHandlers)
	var nativeSrcHandlers []slog.Handler
	if sb.cfg.addSource {
		nativeSrcHandlers = make([]slog.Handler, 0, totalHandlers)
	}

	for _, se := range sb.entries {
		w := combineWriters(se.writers)
		if sb.cfg.addSource && !isConcurrentSafeWriter(w) {
			w = &syncWriter{w: w}
		}
		stdLevel := se.leveler
		if stdLevel == nil {
			stdLevel = allLevels
		}
		hPrimary := newSinkHandler(w, se.format, se.colorize, &slog.HandlerOptions{
			AddSource:   false,
			Level:       stdLevel,
			ReplaceAttr: fastReplace,
		})
		if se.filter != nil {
			hPrimary = &filterHandler{next: hPrimary, filter: se.filter}
		}
		primaryHandlers = append(primaryHandlers, hPrimary)

		if sb.cfg.addSource {
			hNative := newSinkHandler(w, se.format, se.colorize, &slog.HandlerOptions{
				AddSource:   true,
				Level:       stdLevel,
				ReplaceAttr: nativeSrcReplace,
			})
			if se.filter != nil {
				hNative = &filterHandler{next: hNative, filter: se.filter}
			}
			nativeSrcHandlers = append(nativeSrcHandlers, hNative)
		}
	}

	for _, extH := range sb.cfg.externalHandlers {
		primaryHandlers = append(primaryHandlers, extH)
		if sb.cfg.addSource {
			nativeSrcHandlers = append(nativeSrcHandlers, extH)
		}
	}

	inner := newMultiHandler(primaryHandlers...)
	var innerWithSrc slog.Handler
	if sb.cfg.addSource {
		innerWithSrc = newMultiHandler(nativeSrcHandlers...)
	}
	return inner, innerWithSrc
}

func clampFileBufferSize(sz, maxSizeMB int) int {
	if sz <= 0 {
		sz = defaultBufferSize
	}
	if maxSizeMB <= 0 {
		maxSizeMB = 100
	}
	maxBytes := maxSizeMB * 1024 * 1024
	if sz > maxBytes {
		return maxBytes
	}
	return sz
}

func isShareableLeveler(lv slog.Leveler) bool {
	if lv == nil {
		return true
	}
	switch lv.(type) {
	case slog.Level, *slog.LevelVar:
		return true
	default:
		return false
	}
}

func sameLeveler(a, b slog.Leveler) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if la, ok := a.(slog.Level); ok {
		lb, ok2 := b.(slog.Level)
		return ok2 && la == lb
	}
	if va, ok := a.(*slog.LevelVar); ok {
		vb, ok2 := b.(*slog.LevelVar)
		return ok2 && va == vb
	}
	return false
}

func parseFormat(f Format) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(string(f))) {
	case "", string(FormatJSON):
		return FormatJSON, nil
	case string(FormatText):
		return FormatText, nil
	case string(FormatPretty):
		return FormatPretty, nil
	default:
		return "", fmt.Errorf("slogx: unsupported format %q (expected %q, %q, or %q)", f, FormatJSON, FormatText, FormatPretty)
	}
}

func newSinkHandler(w io.Writer, f Format, colorize bool, opts *slog.HandlerOptions) slog.Handler {
	switch f {
	case FormatText:
		return slog.NewTextHandler(w, opts)
	case FormatPretty:
		return newPrettyHandlerInternal(w, PrettyOptions{
			Level:       opts.Level,
			AddSource:   opts.AddSource,
			Color:       Bool(colorize),
			ReplaceAttr: opts.ReplaceAttr,
		}, true)
	default:
		return slog.NewJSONHandler(w, opts)
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *syncWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

func isConcurrentSafeWriter(w io.Writer) bool {
	if w == io.Discard {
		return true
	}
	switch fw := w.(type) {
	case *BufferedWriter, *AsyncWriter, *closeGuardWriter:
		return true
	case fanoutWriter:
		for _, sub := range fw {
			if !isConcurrentSafeWriter(sub) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

type fanoutWriter []io.Writer

func (fw fanoutWriter) Write(p []byte) (int, error) {
	var errs []error
	for _, w := range fw {
		if _, err := w.Write(p); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return 0, errors.Join(errs...)
	}
	return len(p), nil
}

func combineWriters(writers []io.Writer) io.Writer {
	if len(writers) == 1 {
		return writers[0]
	}
	return fanoutWriter(writers)
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
