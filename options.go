package slogx

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/leonlwq/slogx/internal/lumberjack"
)

// Format specifies the serialized output format of log records.
type Format string

const (
	// FormatJSON outputs newline-delimited JSON objects.
	FormatJSON Format = "json"
	// FormatText outputs slog key=value text lines.
	FormatText Format = "text"
	// FormatPretty outputs human-readable aligned console lines with optional ANSI
	// colors and indented multi-line error/stacktrace blocks.
	FormatPretty Format = "pretty"

	// RotateHourly rotates rolling log files at the top of every hour.
	RotateHourly = lumberjack.RotateHourly
	// RotateDaily rotates rolling log files at midnight (local or UTC based on LocalTime).
	RotateDaily = lumberjack.RotateDaily
)

// FileOptions configures rolling log files backed by lumberjack.
// For JSON/YAML/TOML config-file unmarshaling with string intervals ("daily")
// and true-by-default booleans, prefer FileConfig inside Config.
type FileOptions struct {
	Filename         string        `json:"filename" yaml:"filename" toml:"filename"`
	MaxSizeMB        int           `json:"max_size_mb" yaml:"max_size_mb" toml:"max_size_mb"`
	MaxBackups       int           `json:"max_backups" yaml:"max_backups" toml:"max_backups"`
	MaxAgeDays       int           `json:"max_age_days" yaml:"max_age_days" toml:"max_age_days"`
	LocalTime        bool          `json:"local_time" yaml:"local_time" toml:"local_time"`
	Compress         bool          `json:"compress" yaml:"compress" toml:"compress"`
	RotationInterval time.Duration `json:"rotation_interval" yaml:"rotation_interval" toml:"rotation_interval"`
	Format           Format        `json:"format" yaml:"format" toml:"format"` // Optional override; defaults to Logger's global format.
}

// DefaultFileOptions returns production-ready rolling file defaults for filename
// (100MB max size, 30 backups, 30 days max age, local time, gzip compression enabled).
func DefaultFileOptions(filename string) FileOptions {
	return FileOptions{
		Filename:   filename,
		MaxSizeMB:  100,
		MaxBackups: 30,
		MaxAgeDays: 30,
		LocalTime:  true,
		Compress:   true,
	}
}

// AsyncOptions configures AsyncWriter parameters when async mode is enabled.
type AsyncOptions struct {
	QueueSize  int            `json:"queue_size" yaml:"queue_size" toml:"queue_size"`
	BufferSize int            `json:"buffer_size" yaml:"buffer_size" toml:"buffer_size"`
	Policy     OverflowPolicy `json:"policy" yaml:"policy" toml:"policy"`
}

// ServiceInfo holds static cloud-native service metadata pre-formatted onto every log.
type ServiceInfo struct {
	Name        string `json:"name" yaml:"name" toml:"name"`
	Version     string `json:"version" yaml:"version" toml:"version"`
	Env         string `json:"env" yaml:"env" toml:"env"`
	Instance    string `json:"instance" yaml:"instance" toml:"instance"`
	IncludeHost bool   `json:"include_host" yaml:"include_host" toml:"include_host"`
}

// LevelFilter decides whether a log record at the given level should be routed to a sink.
type LevelFilter func(slog.Level) bool

// HandlerMiddleware wraps an slog.Handler to observe, filter, or enrich log records.
// Middlewares registered via WithHandlerMiddleware execute in registration order.
type HandlerMiddleware func(slog.Handler) slog.Handler

// ErrorHandler receives internal operational errors from background/synchronous I/O,
// file rotation, or async queue overflow (matched via errors.Is(err, ErrAsyncOverflow)
// or errors.As(err, &AsyncOverflowError{})).
// Do not log back into the same Logger inside ErrorHandler to avoid recursion.
type ErrorHandler func(err error)

// SinkOptions configures an extensible output destination (either a custom
// io.Writer or a rolling FileOptions) with independent format, level filtering,
// buffering behavior, and optional Sync/Close lifecycle hooks.
type SinkOptions struct {
	// Destination: specify either Writer or File (mutually exclusive).
	Writer io.Writer
	File   *FileOptions

	// Format overrides the Logger's global format for this sink ("json", "text", or "pretty").
	Format Format

	// Color controls ANSI coloring when Format is FormatPretty (nil = auto-detect TTY & NO_COLOR).
	Color *bool

	// Level sets an optional minimum level (e.g. slog.LevelWarn or a *slog.LevelVar)
	// for this sink. When nil, the sink follows the Logger's dynamic level.
	Level slog.Leveler

	// LevelFilter sets an optional level predicate (for example, bounded range
	// INFO..WARN or exact-level routing). Both Level and LevelFilter apply if set.
	LevelFilter LevelFilter

	// DisableBuffer bypasses BufferedWriter/AsyncWriter for this File sink.
	DisableBuffer bool

	// Optional lifecycle hooks invoked on Logger.Sync() and Logger.Close().
	SyncFn  func() error
	CloseFn func() error
}

type moduleLevelSpec struct {
	module string
	level  slog.Level
}

type config struct {
	cfgErr error

	format          Format
	consoleEnabled  bool
	consoleExplicit bool
	consoleWriter   io.Writer
	consoleFormat   Format
	consoleColor    *bool

	sinks            []SinkOptions
	externalHandlers []slog.Handler
	middlewares      []HandlerMiddleware

	appFile *FileOptions
	errFile *FileOptions

	level        slog.Level
	moduleLevels []moduleLevelSpec

	disableBuffer bool
	bufSize       int
	flushInterval time.Duration

	asyncEnabled bool
	asyncOpts    AsyncOptions

	addSource      bool
	sourceLevel    slog.Level
	hasSourceLevel bool

	serviceInfo *ServiceInfo
	staticAttrs []slog.Attr
	extractors  []ContextExtractor

	sensitiveKeys []string
	maxFieldLen   int
	timeFormat    string
	replaceAttr   func(groups []string, a slog.Attr) slog.Attr

	sampling     *SamplingOptions
	onError      ErrorHandler
	enableSIGHUP bool
	setDefault   bool
}

func (c *config) setErr(err error) {
	if err != nil && c.cfgErr == nil {
		c.cfgErr = err
	}
}

func defaultConfig() config {
	return config{
		format:         FormatJSON,
		consoleEnabled: true,
		level:          slog.LevelInfo,
		bufSize:        defaultBufferSize,
		flushInterval:  defaultFlushInterval,
		asyncOpts: AsyncOptions{
			QueueSize:  defaultAsyncQueue,
			BufferSize: defaultBufferSize,
			Policy:     OverflowDrop,
		},
	}
}

// Option configures a Logger in New or MustNew.
type Option func(*config)

// WithFormat sets the primary output format (FormatJSON or FormatText).
func WithFormat(format Format) Option {
	return func(c *config) {
		c.format = format
	}
}

// WithLevel sets the initial minimum log level.
func WithLevel(level slog.Level) Option {
	return func(c *config) {
		c.level = level
	}
}

// WithLevelString sets the initial minimum log level from a string
// (e.g. "debug", "info", "warn", "error").
func WithLevelString(level string) Option {
	return func(c *config) {
		parsed, err := parseLevelString(level)
		if err != nil {
			c.setErr(fmt.Errorf("slogx: invalid log level %q: %w", level, err))
			return
		}
		c.level = parsed
	}
}

// WithModuleLevel sets an initial dynamic level override for a named sub-logger
// module (created via Logger.Named, e.g. "db" or "http.auth").
func WithModuleLevel(module string, level slog.Level) Option {
	return func(c *config) {
		mod := normalizeModuleName(module)
		if mod == "" {
			c.setErr(errors.New("slogx: module name cannot be empty"))
			return
		}
		c.moduleLevels = append(c.moduleLevels, moduleLevelSpec{
			module: mod,
			level:  level,
		})
	}
}

// WithModuleLevelString sets an initial dynamic level override from a string
// for a named sub-logger module.
func WithModuleLevelString(module, level string) Option {
	return func(c *config) {
		mod := normalizeModuleName(module)
		if mod == "" {
			c.setErr(errors.New("slogx: module name cannot be empty"))
			return
		}
		parsed, err := parseLevelString(level)
		if err != nil {
			c.setErr(fmt.Errorf("slogx: invalid level %q for module %q: %w", level, mod, err))
			return
		}
		c.moduleLevels = append(c.moduleLevels, moduleLevelSpec{
			module: mod,
			level:  parsed,
		})
	}
}

// WithConsole enables or disables logging to os.Stdout (enabled by default).
func WithConsole(enabled bool) Option {
	return func(c *config) {
		c.consoleEnabled = enabled
		c.consoleExplicit = true
	}
}

// WithConsoleFormat overrides the output format specifically for console output
// (useful for FormatPretty or FormatText in console alongside FormatJSON in rolling files).
func WithConsoleFormat(format Format) Option {
	return func(c *config) {
		c.consoleFormat = format
	}
}

// WithConsoleColor explicitly enables or disables ANSI color output when the
// console uses FormatPretty. By default (when unset), colors are auto-enabled
// on interactive TTY character devices when NO_COLOR is not set.
func WithConsoleColor(enabled bool) Option {
	return func(c *config) {
		c.consoleColor = &enabled
	}
}

// WithConsoleWriter overrides the destination io.Writer for console output
// (defaults to os.Stdout) and explicitly enables console output.
func WithConsoleWriter(w io.Writer) Option {
	return func(c *config) {
		if w != nil {
			c.consoleWriter = w
			c.consoleEnabled = true
			c.consoleExplicit = true
		}
	}
}

// WithWriter adds a custom io.Writer sink. Unless WithConsole(true) was explicitly
// specified, default os.Stdout output is disabled so logs go exclusively to the
// configured writer(s).
func WithWriter(w io.Writer) Option {
	if w == nil {
		return func(*config) {}
	}
	return WithSink(SinkOptions{Writer: w})
}

// WithSink adds a generalized output sink (an extra rolling file with custom
// level/format, or a custom io.Writer with independent format, level filter, and
// Sync/Close hooks). When SinkOptions.Writer is set without File, implicit
// default os.Stdout is disabled unless WithConsole(true) was specified.
func WithSink(sink SinkOptions) Option {
	return func(c *config) {
		c.sinks = append(c.sinks, sink)
		if sink.Writer != nil && sink.File == nil && !c.consoleExplicit {
			c.consoleEnabled = false
		}
	}
}

// WithHandler attaches one or more external slog.Handler implementations
// (such as OpenTelemetry, Sentry, or custom collectors) into the slogx pipeline.
// Unless WithConsole(true) was explicitly specified, implicit default os.Stdout
// output is disabled.
func WithHandler(handlers ...slog.Handler) Option {
	return func(c *config) {
		for _, h := range handlers {
			if h != nil {
				c.externalHandlers = append(c.externalHandlers, h)
				if !c.consoleExplicit {
					c.consoleEnabled = false
				}
			}
		}
	}
}

// WithHandlerMiddleware registers one or more slog.Handler middlewares around
// the constructed pipeline in registration order (outermost first).
func WithHandlerMiddleware(middlewares ...HandlerMiddleware) Option {
	return func(c *config) {
		for _, mw := range middlewares {
			if mw != nil {
				c.middlewares = append(c.middlewares, mw)
			}
		}
	}
}

// WithErrorHandler registers a callback invoked when underlying writers,
// background flushers, file rotators, or async overflow queues encounter errors.
func WithErrorHandler(fn ErrorHandler) Option {
	return func(c *config) {
		c.onError = fn
	}
}

func resolveFileOptions(filename string, opts []FileOptions) *FileOptions {
	var fo FileOptions
	if len(opts) > 0 {
		fo = opts[0]
		if fo.Filename == "" {
			fo.Filename = filename
		}
	} else {
		fo = DefaultFileOptions(filename)
	}
	return &fo
}

// WithFile enables rolling file output for logs at or above the configured level.
// If opts is omitted, DefaultFileOptions(filename) is used.
func WithFile(filename string, opts ...FileOptions) Option {
	return func(c *config) {
		c.appFile = resolveFileOptions(filename, opts)
	}
}

// WithErrorFile enables a dedicated rolling file that receives only logs at or
// above slog.LevelError, regardless of dynamic debug/info level adjustments.
func WithErrorFile(filename string, opts ...FileOptions) Option {
	return func(c *config) {
		c.errFile = resolveFileOptions(filename, opts)
	}
}

// WithBuffer customizes the BufferedWriter size and background flush interval
// used for rolling file outputs.
func WithBuffer(sizeBytes int, flushInterval time.Duration) Option {
	return func(c *config) {
		c.disableBuffer = false
		c.bufSize = sizeBytes
		c.flushInterval = flushInterval
	}
}

// WithDisableBuffer disables BufferedWriter so file writes go directly to lumberjack.
func WithDisableBuffer() Option {
	return func(c *config) {
		c.disableBuffer = true
	}
}

// WithAsync enables AsyncWriter for rolling file outputs to isolate business
// goroutines from disk latency and file rotation pauses.
func WithAsync(opts ...AsyncOptions) Option {
	return func(c *config) {
		c.asyncEnabled = true
		if len(opts) > 0 {
			ao := opts[0]
			if ao.QueueSize <= 0 {
				ao.QueueSize = defaultAsyncQueue
			}
			if ao.BufferSize <= 0 {
				ao.BufferSize = defaultBufferSize
			}
			c.asyncOpts = ao
		}
	}
}

// WithSource enables or disables caller file:line reporting (cached by PC).
func WithSource(enabled bool) Option {
	return func(c *config) {
		c.addSource = enabled
		c.hasSourceLevel = false
	}
}

// WithSourceLevel enables caller file:line reporting only for records at or
// above minLevel (for example, slog.LevelWarn to skip caller overhead on Info).
func WithSourceLevel(minLevel slog.Level) Option {
	return func(c *config) {
		c.addSource = true
		c.sourceLevel = minLevel
		c.hasSourceLevel = true
	}
}

// WithService attaches pre-formatted service, version, env, and pod/host attributes.
func WithService(name, version, env string) Option {
	return func(c *config) {
		c.serviceInfo = &ServiceInfo{
			Name:        name,
			Version:     version,
			Env:         env,
			IncludeHost: true,
		}
	}
}

// WithServiceInfo attaches custom ServiceInfo metadata.
func WithServiceInfo(info ServiceInfo) Option {
	return func(c *config) {
		c.serviceInfo = &info
	}
}

// WithAttrs pre-formats static root-level attributes onto every record.
func WithAttrs(attrs ...slog.Attr) Option {
	return func(c *config) {
		c.staticAttrs = append(c.staticAttrs, attrs...)
	}
}

// WithContextExtractor registers a callback to extract correlation attributes
// (e.g., OpenTelemetry trace/span IDs or tenant info) from context.Context.
func WithContextExtractor(extractor ContextExtractor) Option {
	return func(c *config) {
		if extractor != nil {
			c.extractors = append(c.extractors, extractor)
		}
	}
}

// WithSensitiveKeys masks values of matching attribute keys (case-insensitive) as "***".
// If no keys are passed, DefaultSensitiveKeys is used.
func WithSensitiveKeys(keys ...string) Option {
	return func(c *config) {
		if len(keys) == 0 {
			c.sensitiveKeys = append(c.sensitiveKeys, DefaultSensitiveKeys...)
		} else {
			c.sensitiveKeys = append(c.sensitiveKeys, keys...)
		}
	}
}

// WithMaxFieldLength truncates string attributes exceeding maxBytes on a valid
// UTF-8 boundary and appends "...[truncated]".
func WithMaxFieldLength(maxBytes int) Option {
	return func(c *config) {
		c.maxFieldLen = maxBytes
	}
}

// WithTimeFormat customizes how the built-in time attribute is formatted
// (supports Go time layouts as well as TimeFormatUnixMillis and TimeFormatUnixNano).
func WithTimeFormat(layout string) Option {
	return func(c *config) {
		c.timeFormat = layout
	}
}

// WithReplaceAttr registers a custom slog attribute transformation function.
func WithReplaceAttr(fn func(groups []string, a slog.Attr) slog.Attr) Option {
	return func(c *config) {
		c.replaceAttr = fn
	}
}

// WithSampling enables lock-free rate-limiting per (Level, Message) window to
// prevent log storms from exhausting disk IOPS.
func WithSampling(opts SamplingOptions) Option {
	return func(c *config) {
		c.sampling = &opts
	}
}

// WithEnableSIGHUP registers a SIGHUP signal watcher on rolling files (Unix)
// to trigger immediate log rotation (compatible with logrotate).
func WithEnableSIGHUP(enabled bool) Option {
	return func(c *config) {
		c.enableSIGHUP = enabled
	}
}

// WithSetDefault sets the newly constructed logger as the process-wide slog.Default().
func WithSetDefault(enabled bool) Option {
	return func(c *config) {
		c.setDefault = enabled
	}
}
