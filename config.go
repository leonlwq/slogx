package slogx

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Bool returns a pointer to v for populating optional boolean fields in Config,
// FileConfig, and SamplingConfig.
func Bool(v bool) *bool {
	return &v
}

// FileConfig is the declarative schema for a rolling log file in Config.
type FileConfig struct {
	Filename         string `json:"filename" yaml:"filename" toml:"filename"`
	MaxSizeMB        int    `json:"max_size_mb" yaml:"max_size_mb" toml:"max_size_mb"`
	MaxBackups       int    `json:"max_backups" yaml:"max_backups" toml:"max_backups"`
	MaxAgeDays       int    `json:"max_age_days" yaml:"max_age_days" toml:"max_age_days"`
	LocalTime        *bool  `json:"local_time" yaml:"local_time" toml:"local_time"`
	Compress         *bool  `json:"compress" yaml:"compress" toml:"compress"`
	RotationInterval string `json:"rotation_interval" yaml:"rotation_interval" toml:"rotation_interval"` // e.g. "daily", "hourly", "24h", "1h"
	Format           Format `json:"format" yaml:"format" toml:"format"`
	Level            string `json:"level" yaml:"level" toml:"level"` // Optional minimum level for this file (e.g. "warn", "error")
}

// AsyncConfig is the declarative schema for asynchronous non-blocking file I/O.
type AsyncConfig struct {
	Enabled    bool   `json:"enabled" yaml:"enabled" toml:"enabled"`
	QueueSize  int    `json:"queue_size" yaml:"queue_size" toml:"queue_size"`
	BufferSize int    `json:"buffer_size" yaml:"buffer_size" toml:"buffer_size"`
	Policy     string `json:"policy" yaml:"policy" toml:"policy"` // "drop" (default) or "block"
}

// SamplingConfig is the declarative schema for log-storm protection.
type SamplingConfig struct {
	Enabled    *bool  `json:"enabled" yaml:"enabled" toml:"enabled"` // Defaults to true when Sampling block is non-nil
	Tick       string `json:"tick" yaml:"tick" toml:"tick"`          // e.g. "1s", "100ms"
	First      int    `json:"first" yaml:"first" toml:"first"`
	Thereafter int    `json:"thereafter" yaml:"thereafter" toml:"thereafter"`
}

// Config provides a zero-dependency declarative configuration schema directly
// unmarshalable from JSON, YAML, TOML, Viper, koanf, Nacos, or Apollo.
type Config struct {
	Level        string            `json:"level" yaml:"level" toml:"level"`
	ModuleLevels map[string]string `json:"module_levels" yaml:"module_levels" toml:"module_levels"`
	Format       Format            `json:"format" yaml:"format" toml:"format"`
	TimeFormat   string            `json:"time_format" yaml:"time_format" toml:"time_format"`

	Console       *bool  `json:"console" yaml:"console" toml:"console"`
	ConsoleFormat Format `json:"console_format" yaml:"console_format" toml:"console_format"`
	ConsoleColor  *bool  `json:"console_color" yaml:"console_color" toml:"console_color"`

	File      *FileConfig  `json:"file" yaml:"file" toml:"file"`
	ErrorFile *FileConfig  `json:"error_file" yaml:"error_file" toml:"error_file"`
	Files     []FileConfig `json:"files" yaml:"files" toml:"files"`

	DisableBuffer bool   `json:"disable_buffer" yaml:"disable_buffer" toml:"disable_buffer"`
	BufferSize    int    `json:"buffer_size" yaml:"buffer_size" toml:"buffer_size"`
	FlushInterval string `json:"flush_interval" yaml:"flush_interval" toml:"flush_interval"`

	Async *AsyncConfig `json:"async" yaml:"async" toml:"async"`

	AddSource   bool   `json:"add_source" yaml:"add_source" toml:"add_source"`
	SourceLevel string `json:"source_level" yaml:"source_level" toml:"source_level"`

	Service *ServiceInfo      `json:"service" yaml:"service" toml:"service"`
	Attrs   map[string]string `json:"attrs" yaml:"attrs" toml:"attrs"`

	EnableDefaultSensitive bool     `json:"enable_default_sensitive" yaml:"enable_default_sensitive" toml:"enable_default_sensitive"`
	SensitiveKeys          []string `json:"sensitive_keys" yaml:"sensitive_keys" toml:"sensitive_keys"`
	MaxFieldLen            int      `json:"max_field_len" yaml:"max_field_len" toml:"max_field_len"`

	Sampling     *SamplingConfig `json:"sampling" yaml:"sampling" toml:"sampling"`
	EnableSIGHUP bool            `json:"enable_sighup" yaml:"enable_sighup" toml:"enable_sighup"`
	SetDefault   bool            `json:"set_default" yaml:"set_default" toml:"set_default"`
}

// NewFromConfig constructs a Logger from declarative Config and optional
// programmatic Options (such as WithContextExtractor, WithHandler, or WithErrorHandler).
func NewFromConfig(cfg Config, opts ...Option) (*Logger, error) {
	all := make([]Option, 0, len(opts)+1)
	all = append(all, WithConfig(cfg))
	all = append(all, opts...)
	return New(all...)
}

// MustNewFromConfig is like NewFromConfig but panics if configuration fails.
func MustNewFromConfig(cfg Config, opts ...Option) *Logger {
	l, err := NewFromConfig(cfg, opts...)
	if err != nil {
		panic(err)
	}
	return l
}

// WithConfig applies a declarative Config struct onto the Logger configuration.
func WithConfig(cfg Config) Option {
	return func(c *config) {
		if strings.TrimSpace(cfg.Level) != "" {
			WithLevelString(cfg.Level)(c)
		}
		if len(cfg.ModuleLevels) > 0 {
			for _, mod := range sortedMapKeys(cfg.ModuleLevels) {
				WithModuleLevelString(mod, cfg.ModuleLevels[mod])(c)
			}
		}
		if cfg.Format != "" {
			WithFormat(cfg.Format)(c)
		}
		if cfg.TimeFormat != "" {
			WithTimeFormat(cfg.TimeFormat)(c)
		}
		if cfg.Console != nil {
			WithConsole(*cfg.Console)(c)
		}
		if cfg.ConsoleFormat != "" {
			WithConsoleFormat(cfg.ConsoleFormat)(c)
		}
		if cfg.ConsoleColor != nil {
			WithConsoleColor(*cfg.ConsoleColor)(c)
		}

		if cfg.File != nil {
			fo, lvl, hasLvl, err := convertFileConfig(*cfg.File)
			if err != nil {
				c.setErr(err)
				return
			}
			if hasLvl {
				WithSink(SinkOptions{File: &fo, Level: lvl})(c)
			} else {
				WithFile(fo.Filename, fo)(c)
			}
		}

		if cfg.ErrorFile != nil {
			fo, lvl, hasLvl, err := convertFileConfig(*cfg.ErrorFile)
			if err != nil {
				c.setErr(err)
				return
			}
			if hasLvl {
				WithSink(SinkOptions{File: &fo, Level: lvl})(c)
			} else {
				WithErrorFile(fo.Filename, fo)(c)
			}
		}

		for _, fc := range cfg.Files {
			fo, lvl, hasLvl, err := convertFileConfig(fc)
			if err != nil {
				c.setErr(err)
				return
			}
			var leveler slog.Leveler
			if hasLvl {
				leveler = lvl
			}
			WithSink(SinkOptions{File: &fo, Level: leveler})(c)
		}

		if cfg.DisableBuffer {
			WithDisableBuffer()(c)
		} else if cfg.BufferSize > 0 || strings.TrimSpace(cfg.FlushInterval) != "" {
			dur := c.flushInterval
			if raw := strings.TrimSpace(cfg.FlushInterval); raw != "" {
				parsed, err := parseOptionalDuration(raw, "flush_interval")
				if err != nil {
					c.setErr(err)
					return
				}
				dur = parsed
			}
			sz := c.bufSize
			if cfg.BufferSize > 0 {
				sz = cfg.BufferSize
			}
			WithBuffer(sz, dur)(c)
		}

		if cfg.Async != nil && cfg.Async.Enabled {
			pol, err := parseAsyncPolicy(cfg.Async.Policy)
			if err != nil {
				c.setErr(err)
				return
			}
			WithAsync(AsyncOptions{
				QueueSize:  cfg.Async.QueueSize,
				BufferSize: cfg.Async.BufferSize,
				Policy:     pol,
			})(c)
		}

		if strings.TrimSpace(cfg.SourceLevel) != "" {
			lvl, err := parseLevelString(cfg.SourceLevel)
			if err != nil {
				c.setErr(fmt.Errorf("slogx: invalid source_level %q: %w", cfg.SourceLevel, err))
				return
			}
			WithSourceLevel(lvl)(c)
		} else if cfg.AddSource {
			WithSource(true)(c)
		}

		if cfg.Service != nil {
			WithServiceInfo(*cfg.Service)(c)
		}
		if len(cfg.Attrs) > 0 {
			keys := sortedMapKeys(cfg.Attrs)
			attrs := make([]slog.Attr, 0, len(keys))
			for _, k := range keys {
				attrs = append(attrs, slog.String(k, cfg.Attrs[k]))
			}
			WithAttrs(attrs...)(c)
		}

		if cfg.EnableDefaultSensitive {
			WithSensitiveKeys(DefaultSensitiveKeys...)(c)
		}
		if len(cfg.SensitiveKeys) > 0 {
			WithSensitiveKeys(cfg.SensitiveKeys...)(c)
		}
		if cfg.MaxFieldLen > 0 {
			WithMaxFieldLength(cfg.MaxFieldLen)(c)
		}

		if cfg.Sampling != nil && (cfg.Sampling.Enabled == nil || *cfg.Sampling.Enabled) {
			var tick time.Duration
			if raw := strings.TrimSpace(cfg.Sampling.Tick); raw != "" {
				d, err := time.ParseDuration(raw)
				if err != nil || d <= 0 {
					c.setErr(fmt.Errorf("slogx: invalid sampling tick %q", cfg.Sampling.Tick))
					return
				}
				tick = d
			}
			WithSampling(SamplingOptions{
				Tick:       tick,
				First:      cfg.Sampling.First,
				Thereafter: cfg.Sampling.Thereafter,
			})(c)
		}

		if cfg.EnableSIGHUP {
			WithEnableSIGHUP(true)(c)
		}
		if cfg.SetDefault {
			WithSetDefault(true)(c)
		}
	}
}

// WithEnv overrides Logger settings from standard 12-Factor environment variables:
//   - LOG_LEVEL, LOG_MODULE_LEVELS ("db=debug,grpc=warn"), LOG_FORMAT, LOG_TIME_FORMAT
//   - LOG_CONSOLE ("true"/"false"), LOG_CONSOLE_FORMAT
//   - LOG_FILE, LOG_ERROR_FILE
//   - LOG_ADD_SOURCE ("true"/"false"), LOG_SOURCE_LEVEL
func WithEnv() Option {
	return WithEnvPrefix("")
}

// WithEnvPrefix is like WithEnv but prepends prefix (for example, "MYAPP_" reads
// MYAPP_LOG_LEVEL, MYAPP_LOG_FORMAT, etc.).
func WithEnvPrefix(prefix string) Option {
	return func(c *config) {
		get := func(key string) string {
			return strings.TrimSpace(os.Getenv(prefix + key))
		}

		if v := get("LOG_LEVEL"); v != "" {
			WithLevelString(v)(c)
		}
		if v := get("LOG_MODULE_LEVELS"); v != "" {
			for _, pair := range strings.Split(v, ",") {
				pair = strings.TrimSpace(pair)
				if pair == "" {
					continue
				}
				mod, lvlStr, ok := strings.Cut(pair, "=")
				mod = normalizeModuleName(mod)
				lvlStr = strings.TrimSpace(lvlStr)
				if !ok || mod == "" || lvlStr == "" {
					c.setErr(fmt.Errorf("slogx: invalid %sLOG_MODULE_LEVELS entry %q (expected module=level)", prefix, pair))
					return
				}
				WithModuleLevelString(mod, lvlStr)(c)
			}
		}
		if v := get("LOG_FORMAT"); v != "" {
			WithFormat(Format(v))(c)
		}
		if v := get("LOG_TIME_FORMAT"); v != "" {
			WithTimeFormat(v)(c)
		}
		if v := get("LOG_CONSOLE"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				c.setErr(fmt.Errorf("slogx: invalid %sLOG_CONSOLE %q: %w", prefix, v, err))
				return
			}
			WithConsole(b)(c)
		}
		if v := get("LOG_CONSOLE_FORMAT"); v != "" {
			WithConsoleFormat(Format(v))(c)
		}
		if v := get("LOG_CONSOLE_COLOR"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				c.setErr(fmt.Errorf("slogx: invalid %sLOG_CONSOLE_COLOR %q: %w", prefix, v, err))
				return
			}
			WithConsoleColor(b)(c)
		}
		if v := get("LOG_FILE"); v != "" {
			if c.appFile != nil {
				c.appFile.Filename = v
			} else {
				WithFile(v)(c)
			}
		}
		if v := get("LOG_ERROR_FILE"); v != "" {
			if c.errFile != nil {
				c.errFile.Filename = v
			} else {
				WithErrorFile(v)(c)
			}
		}
		if v := get("LOG_SOURCE_LEVEL"); v != "" {
			lvl, err := parseLevelString(v)
			if err != nil {
				c.setErr(fmt.Errorf("slogx: invalid %sLOG_SOURCE_LEVEL %q: %w", prefix, v, err))
				return
			}
			WithSourceLevel(lvl)(c)
		} else if v := get("LOG_ADD_SOURCE"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				c.setErr(fmt.Errorf("slogx: invalid %sLOG_ADD_SOURCE %q: %w", prefix, v, err))
				return
			}
			WithSource(b)(c)
		}
	}
}

func convertFileConfig(fc FileConfig) (FileOptions, slog.Level, bool, error) {
	rot, err := parseRotationInterval(fc.RotationInterval)
	if err != nil {
		return FileOptions{}, 0, false, err
	}
	maxSize := fc.MaxSizeMB
	if maxSize <= 0 {
		maxSize = 100
	}
	localTime := true
	if fc.LocalTime != nil {
		localTime = *fc.LocalTime
	}
	compress := true
	if fc.Compress != nil {
		compress = *fc.Compress
	}
	fo := FileOptions{
		Filename:         fc.Filename,
		MaxSizeMB:        maxSize,
		MaxBackups:       fc.MaxBackups,
		MaxAgeDays:       fc.MaxAgeDays,
		LocalTime:        localTime,
		Compress:         compress,
		RotationInterval: rot,
		Format:           fc.Format,
	}

	if raw := strings.TrimSpace(fc.Level); raw != "" {
		lvl, err := parseLevelString(raw)
		if err != nil {
			return FileOptions{}, 0, false, fmt.Errorf("slogx: invalid file level %q for %q: %w", fc.Level, fc.Filename, err)
		}
		return fo, lvl, true, nil
	}
	return fo, 0, false, nil
}

func parseLevelString(raw string) (slog.Level, error) {
	var lv slog.LevelVar
	if err := lv.UnmarshalText([]byte(strings.TrimSpace(raw))); err != nil {
		return 0, err
	}
	return lv.Level(), nil
}

func parseRotationInterval(raw string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "hourly":
		return RotateHourly, nil
	case "daily":
		return RotateDaily, nil
	default:
		return parseOptionalDuration(raw, "rotation_interval")
	}
}

func parseOptionalDuration(raw, field string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "none", "0", "0s":
		return 0, nil
	default:
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil || d < 0 {
			return 0, fmt.Errorf("slogx: invalid %s %q", field, raw)
		}
		return d, nil
	}
}

func parseAsyncPolicy(raw string) (OverflowPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "drop":
		return OverflowDrop, nil
	case "block":
		return OverflowBlock, nil
	default:
		return OverflowDrop, fmt.Errorf("slogx: invalid async policy %q (expected \"drop\" or \"block\")", raw)
	}
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
