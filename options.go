package slogx

import (
	"io"
	"log/slog"
	"time"
)

// Format specifies the serialized output format of log records.
type Format string

const (
	// FormatJSON outputs newline-delimited JSON objects.
	FormatJSON Format = "json"
	// FormatText outputs slog key=value text lines.
	FormatText Format = "text"

	// RotateHourly rotates rolling log files at the top of every hour.
	RotateHourly = time.Hour
	// RotateDaily rotates rolling log files at midnight (local or UTC based on LocalTime).
	RotateDaily = 24 * time.Hour
)

// FileOptions configures rolling log files backed by lumberjack.
type FileOptions struct {
	Filename         string
	MaxSizeMB        int
	MaxBackups       int
	MaxAgeDays       int
	LocalTime        bool
	Compress         bool
	RotationInterval time.Duration
	Format           Format // Optional override; defaults to Logger's global format.
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
	QueueSize  int
	BufferSize int
	Policy     OverflowPolicy
}

// ServiceInfo holds static cloud-native service metadata pre-formatted onto every log.
type ServiceInfo struct {
	Name        string
	Version     string
	Env         string
	Instance    string
	IncludeHost bool
}

type config struct {
	format          Format
	consoleEnabled  bool
	consoleExplicit bool
	consoleWriter   io.Writer
	consoleFormat   Format

	customWriters []io.Writer

	appFile *FileOptions
	errFile *FileOptions

	level       slog.Level
	levelStr    string
	hasLevelStr bool

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
	enableSIGHUP bool
	setDefault   bool
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
		c.hasLevelStr = false
	}
}

// WithLevelString sets the initial minimum log level from a string
// (e.g. "debug", "info", "warn", "error").
func WithLevelString(level string) Option {
	return func(c *config) {
		c.levelStr = level
		c.hasLevelStr = true
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
// (useful for FormatText in console alongside FormatJSON in rolling files).
func WithConsoleFormat(format Format) Option {
	return func(c *config) {
		c.consoleFormat = format
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
	return func(c *config) {
		if w != nil {
			c.customWriters = append(c.customWriters, w)
			if !c.consoleExplicit {
				c.consoleEnabled = false
			}
		}
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
