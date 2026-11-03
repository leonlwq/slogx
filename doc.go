// Package slogx provides a zero-dependency, high-performance cloud-native logging
// library built on Go's standard log/slog (compatible with Go 1.22+) and an
// enhanced rolling file writer.
//
// Key capabilities include:
//   - Declarative JSON/YAML/TOML Config schema (NewFromConfig) and 12-Factor environment overrides (WithEnv)
//   - Buffered and lock-free asynchronous micro-batched file I/O (BufferedWriter / AsyncWriter)
//   - Developer-friendly FormatPretty console output with TTY/NO_COLOR auto-detection and multi-line stacktraces (Stack)
//   - Size-based, time-based (RotateHourly / RotateDaily), and SIGHUP-triggered log rotation
//   - Generalized multi-sink routing (WithSink), external slog.Handler plugins (WithHandler), and HandlerMiddleware chains
//   - Atomic global & hierarchical per-module dynamic levels (Logger.Named) with an HTTP management endpoint
//   - Built-in TraceID / SpanID / RequestID propagation, ContextExtractor hooks, and ErrorHandler observability
//   - Lazy PII masking (Secret, MaskPhone, MaskEmail, MaskString), key-based redaction, and UTF-8 field truncation
//   - Lock-free cache-line-isolated SamplingHandler for log storm protection and zero-alloc PC caller caching
package slogx
