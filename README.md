# slogx

基于 Go 标准库 `log/slog`（兼容 **Go 1.22+**）与增强型滚动文件写入器打造的生产级、**零外部第三方依赖（Zero-Dependency）** 云原生后端日志库。

## 核心特性

- **零外部依赖 & 广泛兼容**：`go.mod` 兼容 **Go 1.22+** 且无任何第三方库，100% 基于标准库构建，彻底杜绝供应链传递依赖风险。
- **双模高性能 I/O**：
  - **默认 `BufferedWriter`**：`64KB` 内存缓冲 + `200ms` 定时刷盘 + `Error` 及以上级别立即落盘，大幅减少 `write(2)` 系统调用。
  - **可选 `AsyncWriter`**：无锁 `sync.Pool` 内存池 + 有界通道 + 单消费者微批聚合（Batch-Drain），将文件写入与轮转完全隔离至后台协程，业务请求尾延迟零磁盘抖动。
- **全维日志轮转**：支持**按大小轮转** + **按时间周期轮转**（按小时 `RotateHourly` / 按本地自然日零点对齐 `RotateDaily`）+ **Unix `SIGHUP` 信号即时轮转** + 池化 `gzip` 压缩。
- **云原生链路与元信息**：
  - 内置 `TraceID` / `SpanID` / `RequestID` 上下文存取器，并支持自定义 `ContextExtractor` 一行对接 OpenTelemetry 或网关上下文。
  - 初始化时预格式化绑定 `service`、`version`、`env`、`instance`（自动探测 K8s `POD_NAME` / `HOSTNAME`），单条日志零重复序列化开销。
- **多路异构分流与插件扩展**：支持 `Stdout`（含本地研发友好的 **`FormatPretty`** ANSI 彩色对齐与多行错误/堆栈缩进渲染，自动识别 TTY 与 `NO_COLOR`）+ 多路任意级别/区间滚动文件（`WithSink`）+ 外部原生 `slog.Handler` 插件（`WithHandler`，如 `otelslog` / Sentry）+ `HandlerMiddleware` 拦截链，同格式同级别 Sink 自动共享单次序列化扇出。
- **全局与子模块分层动态调级**：内置 `Logger.Named("http.auth")` 树形子模块命名空间与无锁动态级别继承，通过 HTTP 管理端点可在线单独开启指定模块的 `DEBUG` 日志而不放大全站日志量。
- **声明式配置与 12-Factor 环境覆盖**：原生导出带 `json` / `yaml` / `toml` 标签的 `Config` 结构体（`NewFromConfig`）与 `WithEnv()` / `WithEnvPrefix()`，零样板代码直连 Viper / Nacos / K8s ConfigMap。
- **安全合规、自监控与防雪崩治理**：
  - 惰性零开销 `Secret`、`MaskPhone`、`MaskEmail`、`MaskString`（实现 `slog.LogValuer`）；
  - 敏感字段名（如 `password`、`token`、`authorization`）大小写不敏感自动打码；
  - 大字段按 UTF-8 字符边界安全截断；
  - `WithErrorHandler` 底层写盘异常与异步队列溢出（`ErrAsyncOverflow`）可观测回调；
  - `128B` 缓存行对齐无锁分桶 `SamplingHandler` 限流采样，防止日志风暴打满磁盘 IOPS。
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
		slogx.WithConsoleFormat(slogx.FormatPretty), // 控制台输出可读彩色对齐与多行堆栈（自动识别 TTY），文件输出 JSON
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

### 3. 声明式配置（JSON / YAML / TOML）与环境变量覆盖

业务微服务可直接将配置文件反序列化至 `slogx.Config`，并叠加 `WithEnv()` 与代码级扩展钩子：

```go
var cfg slogx.Config
_ = json.Unmarshal(rawConfigBytes, &cfg) // 同样兼容 yaml.v3 / toml / Viper

log, err := slogx.NewFromConfig(cfg,
	slogx.WithEnv(), // 支持 LOG_LEVEL / LOG_MODULE_LEVELS="db=debug" 等环境变量覆盖
	slogx.WithErrorHandler(func(err error) {
		if errors.Is(err, slogx.ErrAsyncOverflow) {
			// 上报异步丢弃告警指标
		}
	}),
)
```

### 4. 子模块分层动态调级（`Named`）与通用扩展口（`WithSink` / `WithHandler`）

```go
dbLog := log.Named("db") // 自动附加 "logger":"db"，支持树形继承 log.Named("http").Named("auth")

// 运行时仅开启 db 模块的 DEBUG 日志（亦可通过 PUT /debug/loglevel?module=db&level=debug 在线热更新）：
log.SetModuleLevel("db", slog.LevelDebug)
```

### 5. 接管标准库 `http.Server.ErrorLog`

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
| `BenchmarkFileWrite_Unbuffered-14`（旧版无缓冲直写） | `1973 ns/op` | `0 B/op` | `0 allocs/op` | 基准 (`1.0x`) |
| `BenchmarkFileWrite_Buffered-14`（默认 `64KB` 缓冲写） | **`426.9 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`4.62x` 吞吐提升** |
| `BenchmarkFileWrite_Async-14`（异步微批写） | **`495.6 ns/op`** | `4 B/op` | **`0 allocs/op`** | **`3.98x` + 零磁盘阻塞** |
| `BenchmarkParallelFileWrite_Unbuffered-14`（14 核并发直写） | `3313 ns/op` | `0 B/op` | `0 allocs/op` | 基准 (`1.0x`) |
| `BenchmarkParallelFileWrite_Buffered-14`（14 核并发缓冲写） | **`509.6 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`6.50x` 并发吞吐提升** |
| `BenchmarkParallelFileWrite_Async-14`（14 核并发异步写） | **`745.5 ns/op`** | `1 B/op` | **`0 allocs/op`** | **`4.44x` + 轮转零毛刺** |
| `BenchmarkCaller_StdlibAddSourceAndReplaceAttr-14`（旧版调用栈） | `900.5 ns/op` | `601 B/op` | `8 allocs/op` | 基准 (`1.0x`) |
| `BenchmarkCaller_SlogxPCCache-14`（`PC` 缓存 + 快路径） | **`391.5 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`2.30x` 提速 & 消除全部 8 次分配** |
| `BenchmarkCaller_SlogxSourceLevelWarn_OnInfo-14`（按级别开启） | **`311.7 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`2.89x` 提速 & 0 分配** |
| `BenchmarkLogStorm_NoSampling-14`（并发风暴无采样） | `211.3 ns/op` | `0 B/op` | `0 allocs/op` | 基准 (`1.0x`) |
| `BenchmarkLogStorm_WithSampling-14`（14 核单 Key 极限风暴采样） | **`74.59 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`2.83x` 提速（真实落盘场景 `>40x`）** |
| `BenchmarkLogStorm_MultiKeyWithSampling-14`（14 核多 Key 128B 隔离分桶采样） | **`18.94 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **`11.16x` 提速（`>5200 万 ops/s`）** |
| `BenchmarkMultiSink_SharedJSON-14`（3 路 JSON 共享序列化 + 脱敏 + 时间格式化） | **`631.9 ns/op`** | `32 B/op` | **`1 allocs/op`** | **单次序列化扇出 + 消除 `slog.Level` 反射** |
| `BenchmarkConsole_PrettyHandler-14`（ANSI 彩色对齐 `FormatPretty` + `PC` 缓存） | **`369.9 ns/op`** | **`0 B/op`** | **`0 allocs/op`** | **池化零分配彩色渲染** |
