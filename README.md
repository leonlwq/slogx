# slogx

[![Go Reference](https://pkg.go.dev/badge/github.com/leonlwq/slogx.svg)](https://pkg.go.dev/github.com/leonlwq/slogx)
[![Go Version](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go)](go.mod)
[![Zero Dependencies](https://img.shields.io/badge/dependencies-0-brightgreen)](go.mod)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**`slogx`** is a production-grade, **zero-dependency**, high-performance cloud-native logging library for Go (`1.22+`), built natively on top of the standard library's `log/slog` with a built-in rolling file engine.

It bridges the gap between `log/slog`'s standard API and real-world backend operational needs—delivering **zero-allocation (`0 allocs/op`)** steady-state logging, buffered & asynchronous micro-batched file I/O, size/time/`SIGHUP` log rotation, developer-friendly `FormatPretty` console output, hierarchical per-module dynamic log levels, PII masking, and lock-free log-storm protection without pulling in a single third-party dependency.

---

## Features

- **Zero External Dependencies & Full `log/slog` Compatibility**
  - Requires **Go 1.22+** with **zero** third-party modules in `go.mod`, eliminating transitive supply-chain risks.
  - Embeds `*slog.Logger` directly and passes Go's official `testing/slogtest` compliance suite.
- **Dual High-Throughput I/O Modes**
  - **`BufferedWriter` (Default)**: `64KB` user-space buffer with periodic `200ms` background flushing and immediate auto-flush on `ERROR` and above, cutting `write(2)` syscalls dramatically.
  - **`AsyncWriter` (Optional)**: Lock-free `sync.Pool` buffers + bounded channel + single-consumer micro-batch draining (`OverflowDrop` or `OverflowBlock`), isolating request tail latency from disk stalls and file rotations.
- **Built-in Multi-Dimensional Log Rotation**
  - Supports **size-based** (`MaxSizeMB`), **calendar-aligned time-based** (`RotateHourly`, `RotateDaily`, or custom intervals), and **Unix `SIGHUP`** signal-triggered rotation with pooled `gzip` compression and age/count retention.
- **Heterogeneous Multi-Sink Routing & Single-Pass Fan-Out**
  - Route logs simultaneously to `Stdout` (`FormatPretty` with automatic TTY & `NO_COLOR` detection and indented multi-line error/stacktrace blocks), rolling `JSON`/`Text` files, bounded-level sinks (`WithSink` + `LevelFilter`), external `slog.Handler` plugins (`WithHandler`), and AOP-style `HandlerMiddleware` chains.
  - Sinks sharing the same format and leveler automatically share a single serialization pass.
- **Dynamic Global & Hierarchical Module Levels**
  - Create scoped sub-loggers via `log.Named("http").Named("auth")` (`"http.auth"`) with lock-free atomic level inheritance, controllable in-process or remotely via the built-in zero-dependency `net/http` `LevelHandler`.
- **Declarative Configuration & 12-Factor Environment Overrides**
  - Ready-to-unmarshal `Config` schema (`json` / `yaml` / `toml` struct tags) via `NewFromConfig`, composable with `WithEnv()` / `WithEnvPrefix()` and programmatic hooks.
- **Cloud-Native Correlation, Security & Reliability**
  - Built-in `TraceID` / `SpanID` / `RequestID` context helpers (`1 × O(depth)` lookup, `0 ns` bypass when unused) plus `ContextExtractor` hooks for OpenTelemetry.
  - Lazy zero-overhead PII masking (`Secret`, `SecretAttr`, `MaskPhone`, `MaskEmail`, `MaskString`), case-insensitive sensitive key redaction, and UTF-8-safe field length truncation.
  - `128B` cache-line-isolated lock-free `SamplingHandler` (`>50M ops/s` across cores) to prevent failure storms from exhausting disk IOPS.
  - Operational self-observability via `WithErrorHandler` (`ErrAsyncOverflow` / `AsyncOverflowError`, write/flush/rotation/compression errors).
- **Zero-Allocation Hot Path Optimizations**
  - Process-wide `PC -> slog.Attr` (`"pkg/file.go:line"`) caller cache (`sync.Map`), lazy `Stack()` capture, and lock-free atomic millisecond timestamp caching.

---

## Installation

```bash
go get github.com/leonlwq/slogx@latest
```

---

## Quick Start

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/leonlwq/slogx"
)

func main() {
	log := slogx.MustNew(
		slogx.WithFormat(slogx.FormatJSON),
		slogx.WithLevel(slog.LevelInfo),
		slogx.WithConsole(true),
		slogx.WithConsoleFormat(slogx.FormatPretty), // Human-readable console (auto TTY/NO_COLOR) + JSON files
		slogx.WithFile("logs/app.log", slogx.FileOptions{
			MaxSizeMB:        100,
			MaxBackups:       30,
			MaxAgeDays:       30,
			LocalTime:        true,
			Compress:         true,
			RotationInterval: slogx.RotateDaily, // Rotate at midnight and/or 100MB
		}),
		slogx.WithErrorFile("logs/error.log"),            // Dedicated rolling file for >= ERROR
		slogx.WithService("order-api", "v1.2.0", "prod"), // Pre-formatted service & K8s pod/host metadata
		slogx.WithSourceLevel(slog.LevelWarn),            // Attach caller file:line only on WARN+
		slogx.WithSensitiveKeys(),                        // Auto-redact password, token, authorization, etc.
		slogx.WithMaxFieldLength(4096),                   // Truncate oversized strings on UTF-8 boundaries
		slogx.WithSampling(slogx.SamplingOptions{         // Log-storm protection per (Level, Message)
			Tick:       time.Second,
			First:      100,
			Thereafter: 100,
		}),
		slogx.WithEnableSIGHUP(true),
		slogx.WithSetDefault(true),
	)
	defer log.Close() // Flush buffers, finish pending compressions, and close file descriptors

	// Propagate correlation IDs via context.Context
	ctx := slogx.WithTraceID(context.Background(), "4bf92f3577b34da6a3ce929d0e0e4736")
	ctx = slogx.WithRequestID(ctx, "req-10086")

	log.InfoContext(ctx, "user authenticated",
		slog.Any("phone", slogx.MaskPhone("13812345678")),
		slogx.SecretAttr("access_token", "eyJhbGciOiJIUzI1NiIs..."),
	)

	log.ErrorContext(ctx, "payment upstream timeout",
		slog.Any("err", errors.New("rpc deadline exceeded")),
		slogx.Stack(), // Lazy goroutine stacktrace
	)

	// Expose runtime HTTP level management endpoint:
	//   GET  http://127.0.0.1:9090/debug/loglevel
	//   PUT  http://127.0.0.1:9090/debug/loglevel -d '{"level":"debug"}'
	//   PUT  http://127.0.0.1:9090/debug/loglevel?module=db&level=debug
	mux := http.NewServeMux()
	mux.Handle("/debug/loglevel", log.LevelHandler())
}
```

---

## Advanced Usage

### 1. Non-Blocking Asynchronous File I/O (`AsyncWriter`)

For latency-critical gateways and trading engines, enable `WithAsync` to decouple log I/O and file rotation from business goroutines:

```go
log := slogx.MustNew(
	slogx.WithConsole(false),
	slogx.WithFile("logs/gateway.log"),
	slogx.WithAsync(slogx.AsyncOptions{
		QueueSize:  8192,
		BufferSize: 64 * 1024,
		Policy:     slogx.OverflowDrop, // Never block callers; or use OverflowBlock
	}),
	slogx.WithErrorHandler(func(err error) {
		if errors.Is(err, slogx.ErrAsyncOverflow) {
			// Export overflow drop metrics or alerts
		}
	}),
)
defer log.Close()
```

### 2. Declarative Config (`JSON` / `YAML` / `TOML`) & 12-Factor Env Overrides

Unmarshal configuration directly from JSON, YAML, TOML, Viper, koanf, or remote config centers into `slogx.Config`, then layer environment overrides and code hooks:

```go
var cfg slogx.Config
if err := json.Unmarshal(rawConfigBytes, &cfg); err != nil {
	panic(err)
}

log, err := slogx.NewFromConfig(cfg,
	slogx.WithEnv(), // Or slogx.WithEnvPrefix("MYAPP_")
	slogx.WithErrorHandler(onLogError),
)
```

### 3. Hierarchical Module Levels (`Named`)

Enable fine-grained `DEBUG` logging for a single subsystem in production without flooding global logs:

```go
httpLog := log.Named("http")
authLog := httpLog.Named("auth") // Emits "logger":"http.auth", inherits from "http" -> root

// Dynamically adjust a specific module in code or via HTTP PUT /debug/loglevel?module=http.auth&level=debug
log.SetModuleLevel("http.auth", slog.LevelDebug)

// Reset module override so it inherits from its parent again
log.ResetModuleLevel("http.auth")
```

### 4. Generalized Sinks, External `slog.Handler` Plugins & Middlewares

Integrate custom writers, bounded level ranges, third-party `slog.Handler` implementations (such as `otelslog` or Sentry), and `HandlerMiddleware` decorators:

```go
auditFile := slogx.DefaultFileOptions("logs/audit.log")

log := slogx.MustNew(
	slogx.WithSink(slogx.SinkOptions{
		File:   &auditFile,
		Format: slogx.FormatJSON,
		LevelFilter: func(lvl slog.Level) bool {
			return lvl >= slog.LevelInfo && lvl < slog.LevelError
		},
	}),
	slogx.WithHandler(otelSlogHandler),
	slogx.WithHandlerMiddleware(metricsMiddleware),
)
```

### 5. OpenTelemetry SpanContext & `http.Server.ErrorLog` Integration

Bridge OpenTelemetry `trace.SpanContext` in three lines without coupling `slogx` to OTel dependencies, and route standard library `*log.Logger` output into structured logs:

```go
log := slogx.MustNew(
	slogx.WithContextExtractor(func(ctx context.Context) []slog.Attr {
		sc := trace.SpanContextFromContext(ctx)
		if !sc.IsValid() {
			return nil
		}
		return []slog.Attr{
			slog.String(slogx.TraceIDKey, sc.TraceID().String()),
			slog.String(slogx.SpanIDKey, sc.SpanID().String()),
		}
	}),
)

srv := &http.Server{
	Addr:     ":8080",
	ErrorLog: log.StdLogLogger(slog.LevelWarn),
}
```

---

## Environment Variables (`WithEnv` / `WithEnvPrefix`)

| Variable | Example | Description |
| :--- | :--- | :--- |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` | Global minimum log level |
| `LOG_MODULE_LEVELS` | `db=debug,http.auth=warn` | Comma-separated per-module level overrides |
| `LOG_FORMAT` | `json`, `text`, `pretty` | Primary output format |
| `LOG_TIME_FORMAT` | `2006-01-02T15:04:05.000Z07:00`, `unix_ms` | Timestamp layout or Unix epoch unit |
| `LOG_CONSOLE` | `true`, `false` | Enable or disable console output |
| `LOG_CONSOLE_FORMAT` | `pretty`, `json`, `text` | Console-specific format override |
| `LOG_CONSOLE_COLOR` | `true`, `false` | Force-enable or disable ANSI colors in `pretty` mode |
| `LOG_FILE` | `/var/log/app/app.log` | Primary rolling log file path |
| `LOG_ERROR_FILE` | `/var/log/app/error.log` | Dedicated `>= ERROR` rolling log file path |
| `LOG_ADD_SOURCE` | `true`, `false` | Enable caller `pkg/file.go:line` reporting |
| `LOG_SOURCE_LEVEL` | `warn`, `error` | Minimum level required to attach caller source |

---

## Benchmarks

Measured on `darwin/arm64` (**Apple M4 Pro**, 14 cores) with `go test -bench=. -benchmem -run=^$ .`:

| Benchmark | Latency (`ns/op`) | Bytes (`B/op`) | Allocs (`allocs/op`) | Notes |
| :--- | ---: | ---: | ---: | :--- |
| `BenchmarkFileWrite_Unbuffered-14` | `1898 ns/op` | `0 B/op` | `0 allocs/op` | Direct unbuffered file write (`1.0x` baseline) |
| `BenchmarkFileWrite_Buffered-14` | **`424.2 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`4.47x` faster** (`64KB` `BufferedWriter`) |
| `BenchmarkFileWrite_Async-14` | **`474.7 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`4.00x` faster** + non-blocking micro-batched I/O |
| `BenchmarkParallelFileWrite_Unbuffered-14` | `3203 ns/op` | `0 B/op` | `0 allocs/op` | 14-core parallel direct write (`1.0x` baseline) |
| `BenchmarkParallelFileWrite_Buffered-14` | **`505.7 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`6.33x` higher** parallel throughput |
| `BenchmarkParallelFileWrite_Async-14` | **`755.3 ns/op`** | `4 B/op` | **`0 allocs/op`** | **`4.24x` faster** + zero rotation tail-latency spikes |
| `BenchmarkCaller_StdlibAddSourceAndReplaceAttr-14` | `848.1 ns/op` | `601 B/op` | `8 allocs/op` | Stdlib `AddSource` + `ReplaceAttr` baseline |
| `BenchmarkCaller_SlogxPCCache-14` | **`379.7 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`2.23x` faster** & eliminates all 8 heap allocs |
| `BenchmarkCaller_SlogxSourceLevelWarn_OnInfo-14` | **`306.6 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`2.77x` faster** (`WithSourceLevel(LevelWarn)` on `INFO`) |
| `BenchmarkLogStorm_NoSampling-14` | `211.0 ns/op` | `0 B/op` | `0 allocs/op` | 14-core log storm without sampling |
| `BenchmarkLogStorm_WithSampling-14` | **`75.36 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | 14-core single-key storm (`>40x` faster on real disk) |
| `BenchmarkLogStorm_MultiKeyWithSampling-14` | **`19.32 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`>51M ops/s`** (128B cache-line-isolated buckets) |
| `BenchmarkMultiSink_SharedJSON-14` | **`524.3 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | 3 JSON sinks + redaction + millisecond time cache |
| `BenchmarkConsole_PrettyHandler-14` | **`375.7 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | ANSI `FormatPretty` + caller cache |
| `BenchmarkContextCorrelation_DeepChain-14` | **`461.4 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | 8-layer `context.Context` chain + 3 correlation IDs |

---

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
