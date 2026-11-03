// Package slogx provides a zero-dependency, high-performance cloud-native logging
// library built on Go's standard log/slog and an enhanced rolling file writer.
//
// Key capabilities include:
//   - Buffered and lock-free asynchronous micro-batched file I/O (BufferedWriter / AsyncWriter)
//   - Size-based, time-based (RotateHourly / RotateDaily), and SIGHUP-triggered log rotation
//   - Multi-sink routing across console, rolling application logs, and isolated error logs
//   - Atomic dynamic log level control with a ready-to-mount net/http management handler
//   - Built-in TraceID / SpanID / RequestID propagation and custom ContextExtractor hooks
//   - Lazy PII masking (Secret, MaskPhone, MaskEmail, MaskString), key-based redaction, and UTF-8 field truncation
//   - Lock-free bucketed SamplingHandler for log storm protection and zero-alloc PC caller caching
package slogx

