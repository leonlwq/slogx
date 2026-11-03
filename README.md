# slogx

基于 Go 1.27 官方 `log/slog` 与增强型 `lumberjack` 打造的生产级、**零外部第三方依赖（Zero-Dependency）** 云原生后端日志库。

## 核心特性

- **零外部依赖**：`go.mod` 无任何第三方库，100% 基于 Go 标准库构建，彻底杜绝供应链传递依赖风险。
- **双模高性能 I/O**：
  - **默认 `BufferedWriter`**：`64KB` 内存缓冲 + `200ms` 定时刷盘 + `Error` 及以上级别立即落盘，大幅减少 `write(2)` 系统调用。
  - **可选 `AsyncWriter`**：无锁 `sync.Pool` 内存池 + 有界通道 + 单消费者微批聚合（Batch-Drain），将文件写入与轮转完全隔离至后台协程，业务请求尾延迟零磁盘抖动。
- **全维日志轮转**：支持**按大小轮转** + **按时间周期轮转**（按小时 `RotateHourly` / 按本地自然日零点对齐 `RotateDaily`）+ **Unix `SIGHUP` 信号即时轮转** + 池化 `gzip` 压缩。
- **云原生链路与元信息**：
  - 内置 `TraceID` / `SpanID` / `RequestID` 上下文存取器，并支持自定义 `ContextExtractor` 一行对接 OpenTelemetry 或网关上下文。
  - 初始化时预格式化绑定 `service`、`version`、`env`、`instance`（自动探测 K8s `POD_NAME` / `HOSTNAME`），单条日志零重复序列化开销。
- **多路异构分流**：基于 Go 1.27 原生 `slog.NewMultiHandler`，同时支持 `Stdout`（如本地可读 `Text`）+ `app.log`（`JSON`）+ `error.log`（独立归集 `>= ERROR` 告警日志，不受动态调级为 `DEBUG` 干扰）。
- **原子动态调级 + 内置 HTTP 端点**：封装 `slog.LevelVar`，直接提供标准 `net/http.Handler`（`GET` 查询、`PUT/POST` 热更新日志级别），线上排障无需重启容器。
- **安全合规与防雪崩治理**：
  - 惰性零开销 `Secret`、`MaskPhone`、`MaskEmail`、`MaskString`（实现 `slog.LogValuer`）；
  - 敏感字段名（如 `password`、`token`、`authorization`）大小写不敏感自动打码；
  - 大字段按 UTF-8 字符边界安全截断（防止巨型报文打爆单行上限）；
  - 无锁分桶 `SamplingHandler` 限流采样，防止下游雪崩引发的日志风暴打满磁盘 IOPS。
- **极致调用栈性能**：`PC -> slog.Attr`（`"pkg/file.go:line"`）`sync.Map` 进程级缓存，每个日志调用点全生命周期仅解析一次栈帧，后续命中 **0 堆内存分配**，并支持按级别开启（如仅 `Warn+` 记录行号）。

---

## 快速开始

```go
package main

import (
	"context"
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
		slogx.WithConsoleFormat(slogx.FormatText), // 控制台输出可读文本，文件输出 JSON
		slogx.WithFile("logs/app.log", slogx.FileOptions{
			MaxSizeMB:        100,
			MaxBackups:       30,
			MaxAgeDays:       30,
			LocalTime:        true,
			Compress:         true,
			RotationInterval: slogx.RotateDaily, // 按天零点 + 100MB 混合轮转
		}),
		slogx.WithErrorFile("logs/error.log"),        // >= ERROR 独立落盘
		slogx.WithService("order-api", "v1.2.0", "prod"),
		slogx.WithSourceLevel(slog.LevelWarn),        // 仅 Warn/Error 附加调用行号
		slogx.WithSensitiveKeys(),                    // 自动掩码 password/token/authorization 等字段
		slogx.WithMaxFieldLength(4096),               // 超长字符串 UTF-8 安全截断
		slogx.WithSampling(slogx.SamplingOptions{     // 防日志风暴：每秒同模板前 100 条全放行，之后每 100 条采样 1 条
			Tick:       time.Second,
			First:      100,
			Thereafter: 100,
		}),
		slogx.WithEnableSIGHUP(true),
		slogx.WithSetDefault(true), // 可选：设为全局 slog.Default()
	)
	defer log.Close() // 优雅停机：刷空缓冲区、等待压缩完成并关闭文件句柄

	// 1. 携带链路追踪 ID 打印日志
	ctx := slogx.WithTraceID(context.Background(), "4bf92f3577b34da6a3ce929d0e0e4736")
	ctx = slogx.WithRequestID(ctx, "req-10086")

	log.InfoContext(ctx, "user login",
		slog.String("phone", "13812345678"), // 若需脱敏可用 slog.Any("phone", slogx.MaskPhone("13812345678"))
		slog.Any("masked_phone", slogx.MaskPhone("13812345678")),
		slogx.SecretAttr("access_token", "eyJhbGciOiJIUzI1NiIs..."),
	)

	// 2. 挂载动态日志级别 HTTP 管理端点
	// 查询: curl http://127.0.0.1:9090/debug/loglevel
	// 调级: curl -X PUT http://127.0.0.1:9090/debug/loglevel -d '{"level":"debug"}'
	mux := http.NewServeMux()
	mux.Handle("/debug/loglevel", log.LevelHandler())
}
```

---

## 进阶用法

### 1. 开启异步零阻塞写入（`AsyncWriter`）

在对 P999 尾延迟极度敏感的核心网关或交易服务中，可开启异步微批写入：

```go
log := slogx.MustNew(
	slogx.WithConsole(false),
	slogx.WithFile("logs/gateway.log"),
	slogx.WithAsync(slogx.AsyncOptions{
		QueueSize:  8192,
		BufferSize: 64 * 1024,
		Policy:     slogx.OverflowDrop, // 满载丢弃并记录计数，绝不阻塞业务协程（亦可选 OverflowBlock）
	}),
)
defer log.Close()
```

### 2. 零依赖对接 OpenTelemetry Context

无需在本库引入任何 OpenTelemetry 依赖，业务项目只需在初始化时注册三行提取闭包：

```go
slogx.WithContextExtractor(func(ctx context.Context) []slog.Attr {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []slog.Attr{
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	}
})
```

### 3. 接管标准库 `http.Server.ErrorLog`

```go
srv := &http.Server{
	Addr:     ":8080",
	ErrorLog: log.StdLogLogger(slog.LevelWarn),
}
```

---

## 性能基准测试（Benchmark）

测试环境：`Go 1.27.3 darwin/arm64 (Apple M4 Pro)`，执行命令 `go test -bench=. -benchmem -run=^$ ./...`：

| 基准测试项 | 耗时 (`ns/op`) | 内存分配 (`B/op`) | 分配次数 (`allocs/op`) | 对比提升 |
| :--- | ---: | ---: | ---: | :--- |
| `BenchmarkFileWrite_Unbuffered-14`（旧版无缓冲直写） | `1927 ns/op` | `0 B/op` | `0 allocs/op` | 基准 (`1.0x`) |
| `BenchmarkFileWrite_Buffered-14`（默认 `64KB` 缓冲写） | **`449.2 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`4.29x` 吞吐提升** |
| `BenchmarkFileWrite_Async-14`（异步微批写） | **`507.8 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`3.79x` + 零磁盘阻塞** |
| `BenchmarkParallelFileWrite_Unbuffered-14`（14 核并发直写） | `3238 ns/op` | `0 B/op` | `0 allocs/op` | 基准 (`1.0x`) |
| `BenchmarkParallelFileWrite_Buffered-14`（14 核并发缓冲写） | **`510.1 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`6.35x` 并发吞吐提升** |
| `BenchmarkParallelFileWrite_Async-14`（14 核并发异步写） | **`757.8 ns/op`** | `1 B/op` | **`0 allocs/op`** | **`4.27x` + 轮转零毛刺** |
| `BenchmarkCaller_StdlibAddSourceAndReplaceAttr-14`（旧版调用栈） | `872.4 ns/op` | `601 B/op` | `8 allocs/op` | 基准 (`1.0x`) |
| `BenchmarkCaller_SlogxPCCache-14`（`PC` 缓存 + 快路径） | **`393.0 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`2.22x` 提速 & 消除全部 8 次分配** |
| `BenchmarkCaller_SlogxSourceLevelWarn_OnInfo-14`（按级别开启） | **`308.3 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`2.83x` 提速 & 0 分配** |
| `BenchmarkLogStorm_NoSampling-14`（并发风暴无采样） | `215.8 ns/op` | `0 B/op` | `0 allocs/op` | 基准 |
| `BenchmarkLogStorm_WithSampling-14`（并发风暴开启采样） | **`130.9 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **短路径直接拦截（真实落盘场景 `~25x`）** |
