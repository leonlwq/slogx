package slogx

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkFileWrite_Unbuffered(b *testing.B) {
	dir := b.TempDir()
	l := MustNew(
		WithConsole(false),
		WithDisableBuffer(),
		WithFile(filepath.Join(dir, "unbuf.log"), FileOptions{MaxSizeMB: 500, LocalTime: true}),
	)
	defer l.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.LogAttrs(context.Background(), slog.LevelInfo, "http request handled",
			slog.String("method", "POST"),
			slog.String("path", "/api/v1/orders"),
			slog.Int("status", 200),
		)
	}
	b.StopTimer()
	_ = l.Sync()
}

func BenchmarkFileWrite_Buffered(b *testing.B) {
	dir := b.TempDir()
	l := MustNew(
		WithConsole(false),
		WithFile(filepath.Join(dir, "buf.log"), FileOptions{MaxSizeMB: 500, LocalTime: true}),
	)
	defer l.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.LogAttrs(context.Background(), slog.LevelInfo, "http request handled",
			slog.String("method", "POST"),
			slog.String("path", "/api/v1/orders"),
			slog.Int("status", 200),
		)
	}
	b.StopTimer()
	_ = l.Sync()
}

func BenchmarkFileWrite_Async(b *testing.B) {
	dir := b.TempDir()
	l := MustNew(
		WithConsole(false),
		WithAsync(AsyncOptions{QueueSize: 65536, BufferSize: 64 * 1024, Policy: OverflowBlock}),
		WithFile(filepath.Join(dir, "async.log"), FileOptions{MaxSizeMB: 500, LocalTime: true}),
	)
	defer l.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.LogAttrs(context.Background(), slog.LevelInfo, "http request handled",
			slog.String("method", "POST"),
			slog.String("path", "/api/v1/orders"),
			slog.Int("status", 200),
		)
	}
	b.StopTimer()
	_ = l.Sync()
}

func BenchmarkParallelFileWrite_Unbuffered(b *testing.B) {
	dir := b.TempDir()
	l := MustNew(
		WithConsole(false),
		WithDisableBuffer(),
		WithFile(filepath.Join(dir, "p_unbuf.log"), FileOptions{MaxSizeMB: 500, LocalTime: true}),
	)
	defer l.Close()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.LogAttrs(context.Background(), slog.LevelInfo, "http request handled",
				slog.String("method", "POST"),
				slog.String("path", "/api/v1/orders"),
				slog.Int("status", 200),
			)
		}
	})
	b.StopTimer()
	_ = l.Sync()
}

func BenchmarkParallelFileWrite_Buffered(b *testing.B) {
	dir := b.TempDir()
	l := MustNew(
		WithConsole(false),
		WithFile(filepath.Join(dir, "p_buf.log"), FileOptions{MaxSizeMB: 500, LocalTime: true}),
	)
	defer l.Close()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.LogAttrs(context.Background(), slog.LevelInfo, "http request handled",
				slog.String("method", "POST"),
				slog.String("path", "/api/v1/orders"),
				slog.Int("status", 200),
			)
		}
	})
	b.StopTimer()
	_ = l.Sync()
}

func BenchmarkParallelFileWrite_Async(b *testing.B) {
	dir := b.TempDir()
	l := MustNew(
		WithConsole(false),
		WithAsync(AsyncOptions{QueueSize: 65536, BufferSize: 64 * 1024, Policy: OverflowDrop}),
		WithFile(filepath.Join(dir, "p_async.log"), FileOptions{MaxSizeMB: 500, LocalTime: true}),
	)
	defer l.Close()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.LogAttrs(context.Background(), slog.LevelInfo, "http request handled",
				slog.String("method", "POST"),
				slog.String("path", "/api/v1/orders"),
				slog.Int("status", 200),
			)
		}
	})
	b.StopTimer()
	_ = l.Sync()
}

func BenchmarkCaller_StdlibAddSourceAndReplaceAttr(b *testing.B) {
	h := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{
		AddSource: true,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.SourceKey {
				if src, ok := a.Value.Any().(*slog.Source); ok {
					src.File = filepath.Base(src.File)
				}
			}
			return a
		},
	})
	l := slog.New(h)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.LogAttrs(context.Background(), slog.LevelInfo, "caller bench", slog.Int("id", i))
	}
}

func BenchmarkCaller_SlogxPCCache(b *testing.B) {
	l := MustNew(
		WithWriter(io.Discard),
		WithFormat(FormatJSON),
		WithSource(true),
	)
	defer l.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.LogAttrs(context.Background(), slog.LevelInfo, "caller bench", slog.Int("id", i))
	}
}

func BenchmarkCaller_SlogxSourceLevelWarn_OnInfo(b *testing.B) {
	l := MustNew(
		WithWriter(io.Discard),
		WithFormat(FormatJSON),
		WithSourceLevel(slog.LevelWarn),
	)
	defer l.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.LogAttrs(context.Background(), slog.LevelInfo, "caller bench", slog.Int("id", i))
	}
}

func BenchmarkLogStorm_NoSampling(b *testing.B) {
	l := MustNew(
		WithWriter(io.Discard),
		WithFormat(FormatJSON),
	)
	defer l.Close()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.LogAttrs(context.Background(), slog.LevelError, "downstream rpc timeout", slog.String("peer", "10.0.1.9:8080"))
		}
	})
}

func BenchmarkLogStorm_WithSampling(b *testing.B) {
	l := MustNew(
		WithWriter(io.Discard),
		WithFormat(FormatJSON),
		WithSampling(SamplingOptions{Tick: time.Second, First: 100, Thereafter: 100}),
	)
	defer l.Close()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.LogAttrs(context.Background(), slog.LevelError, "downstream rpc timeout", slog.String("peer", "10.0.1.9:8080"))
		}
	})
}
