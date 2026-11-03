package slogx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/slogtest"
	"time"
	"unicode/utf8"
)

func parseJSONLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	out := make([]map[string]any, 0, len(lines))
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d invalid JSON %q: %v", i, line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestSlogtestCompliance(t *testing.T) {
	for _, addSource := range []bool{false, true} {
		name := "source_off"
		if addSource {
			name = "source_on"
		}
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			slogtest.Run(t, func(t *testing.T) slog.Handler {
				buf.Reset()
				l, err := New(
					WithWriter(&buf),
					WithFormat(FormatJSON),
					WithLevel(slog.LevelDebug),
					WithSource(addSource),
				)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Close() })
				return l.Handler()
			}, func(t *testing.T) map[string]any {
				var m map[string]any
				if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
					t.Fatalf("json unmarshal %q: %v", buf.String(), err)
				}
				return m
			})
		})
	}
}

// Regression test: logging a custom user field named "source" must never panic
// even when caller source reporting and sensitive key rewriting are enabled.
func TestUserSourceAttrNoPanic(t *testing.T) {
	var buf bytes.Buffer
	l := MustNew(
		WithWriter(&buf),
		WithFormat(FormatJSON),
		WithSource(true),
		WithSensitiveKeys(),
	)
	defer l.Close()

	l.Info("order created", "source", "wechat_mini_program")
	l.WithGroup("utm").Info("campaign click", "source", "search_engine")

	records := parseJSONLines(t, buf.Bytes())
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	utm, ok := records[1]["utm"].(map[string]any)
	if !ok || utm["source"] != "search_engine" {
		t.Fatalf("expected nested utm.source=search_engine, got %#v", records[1])
	}
	topSrc, _ := records[1]["source"].(string)
	if !strings.Contains(topSrc, "logger_test.go:") {
		t.Fatalf("expected top-level caller source containing logger_test.go, got %q", topSrc)
	}
}

func TestContextExtraction(t *testing.T) {
	type tenantKey struct{}
	var buf bytes.Buffer

	l := MustNew(
		WithWriter(&buf),
		WithFormat(FormatJSON),
		WithContextExtractor(func(ctx context.Context) []slog.Attr {
			if tenant, ok := ctx.Value(tenantKey{}).(string); ok && tenant != "" {
				return []slog.Attr{slog.String("tenant_id", tenant)}
			}
			return nil
		}),
	)
	defer l.Close()

	ctx := context.Background()
	ctx = WithTraceID(ctx, "4bf92f3577b34da6a3ce929d0e0e4736")
	ctx = WithSpanID(ctx, "00f067aa0ba902b7")
	ctx = WithRequestID(ctx, "req-9981")
	ctx = context.WithValue(ctx, tenantKey{}, "acme-corp")

	if got := TraceID(ctx); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("unexpected TraceID: %q", got)
	}
	if got := SpanID(ctx); got != "00f067aa0ba902b7" {
		t.Fatalf("unexpected SpanID: %q", got)
	}
	if got := RequestID(ctx); got != "req-9981" {
		t.Fatalf("unexpected RequestID: %q", got)
	}

	l.InfoContext(ctx, "payment processed", slog.Int("amount", 199))

	records := parseJSONLines(t, buf.Bytes())
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	rec := records[0]
	if rec["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" ||
		rec["span_id"] != "00f067aa0ba902b7" ||
		rec["request_id"] != "req-9981" ||
		rec["tenant_id"] != "acme-corp" {
		t.Fatalf("missing context attributes in record: %#v", rec)
	}
}

func TestDynamicLevelAndHTTPHandler(t *testing.T) {
	var buf bytes.Buffer
	l := MustNew(
		WithWriter(&buf),
		WithFormat(FormatJSON),
		WithLevelString("info"),
	)
	defer l.Close()

	l.Debug("debug-hidden")
	if buf.Len() != 0 {
		t.Fatalf("expected debug log to be suppressed at INFO level, got %s", buf.String())
	}

	handler := l.LevelHandler()

	// 1. GET current level
	req := httptest.NewRequest(http.MethodGet, "/debug/level", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"INFO"`) {
		t.Fatalf("GET /debug/level got %d %s", rec.Code, rec.Body.String())
	}

	// 2. PUT JSON body {"level":"debug"}
	req = httptest.NewRequest(http.MethodPut, "/debug/level", strings.NewReader(`{"level":"debug"}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || l.Level() != slog.LevelDebug {
		t.Fatalf("PUT debug failed: status=%d body=%s level=%v", rec.Code, rec.Body.String(), l.Level())
	}

	l.Debug("debug-visible")
	if !strings.Contains(buf.String(), "debug-visible") {
		t.Fatalf("expected debug-visible after dynamic level switch, got %s", buf.String())
	}

	// 3. POST query parameter ?level=error
	req = httptest.NewRequest(http.MethodPost, "/debug/level?level=error", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || l.Level() != slog.LevelError {
		t.Fatalf("POST ?level=error failed: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 4. Invalid level -> 400
	req = httptest.NewRequest(http.MethodPut, "/debug/level", strings.NewReader(`{"level":"not-a-level"}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid level, got %d", rec.Code)
	}

	// 5. Unsupported method -> 405
	req = httptest.NewRequest(http.MethodDelete, "/debug/level", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for DELETE, got %d", rec.Code)
	}
}

func TestMultiOutputAndErrorFileIsolation(t *testing.T) {
	dir := t.TempDir()
	appLog := filepath.Join(dir, "app.log")
	errLog := filepath.Join(dir, "error.log")

	var consoleBuf bytes.Buffer
	l := MustNew(
		WithWriter(&consoleBuf),
		WithFormat(FormatJSON),
		WithLevel(slog.LevelDebug), // Even at DEBUG, error.log must only receive ERROR+!
		WithFile(appLog, FileOptions{MaxSizeMB: 10, LocalTime: true}),
		WithErrorFile(errLog, FileOptions{MaxSizeMB: 10, LocalTime: true}),
		WithService("checkout-svc", "v1.4.2", "staging"),
	)

	l.Debug("step-debug")
	l.Info("step-info")
	l.Warn("step-warn")
	l.Error("step-error", slog.String("code", "DB_TIMEOUT"))

	// Notice: Error log triggers immediate auto-Sync even before Close()!
	errBytesBeforeClose, err := os.ReadFile(errLog)
	if err != nil {
		t.Fatalf("failed reading error.log before Close: %v", err)
	}
	errRecords := parseJSONLines(t, errBytesBeforeClose)
	if len(errRecords) != 1 || errRecords[0]["msg"] != "step-error" {
		t.Fatalf("error.log must contain exactly 1 ERROR record immediately, got %#v", errRecords)
	}
	if errRecords[0]["service"] != "checkout-svc" || errRecords[0]["version"] != "v1.4.2" {
		t.Fatalf("missing preformatted service metadata: %#v", errRecords[0])
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	appBytes, err := os.ReadFile(appLog)
	if err != nil {
		t.Fatalf("failed reading app.log: %v", err)
	}
	appRecords := parseJSONLines(t, appBytes)
	if len(appRecords) != 4 {
		t.Fatalf("expected 4 records in app.log, got %d", len(appRecords))
	}
	consoleRecords := parseJSONLines(t, consoleBuf.Bytes())
	if len(consoleRecords) != 4 {
		t.Fatalf("expected 4 records in console writer, got %d", len(consoleRecords))
	}
}

func TestSourceLevelGatingAndShortPath(t *testing.T) {
	var buf bytes.Buffer
	l := MustNew(
		WithWriter(&buf),
		WithFormat(FormatJSON),
		WithSourceLevel(slog.LevelWarn),
	)
	defer l.Close()

	l.Info("normal-op")
	l.Warn("slow-query")

	records := parseJSONLines(t, buf.Bytes())
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if _, exists := records[0]["source"]; exists {
		t.Fatalf("expected Info record to omit source when SourceLevel=Warn, got %#v", records[0])
	}
	src, _ := records[1]["source"].(string)
	if !strings.Contains(src, "logger_test.go:") {
		t.Fatalf("expected Warn record to include short caller path, got %q", src)
	}
}

func TestRedactionAndUTF8Truncation(t *testing.T) {
	var buf bytes.Buffer
	l := MustNew(
		WithWriter(&buf),
		WithFormat(FormatJSON),
		WithSensitiveKeys(),    // uses DefaultSensitiveKeys
		WithMaxFieldLength(20), // 20 bytes cuts mid-rune on the 7th 3-byte Chinese char -> backs up cleanly to 18 bytes (6 chars)
		WithTimeFormat(TimeFormatUnixMillis),
	)
	defer l.Close()

	l.Info("user signup",
		"Password", "s3cr3t!",
		"Authorization", "Bearer eyJhbGci...",
		"api_token", Secret("tok_live_12345"),
		"phone", MaskPhone("13812345678"),
		"email", MaskEmail("alice@example.com"),
		"short_mask", MaskString("ab", 2, 2),
		"payload", "你好世界这是一段超长中文报文",
	)

	records := parseJSONLines(t, buf.Bytes())
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	r := records[0]

	if _, ok := r["time"].(float64); !ok {
		t.Fatalf("expected numeric unix_ms timestamp, got %T (%v)", r["time"], r["time"])
	}
	for _, key := range []string{"Password", "Authorization", "api_token", "short_mask"} {
		if r[key] != "***" {
			t.Fatalf("expected %s to be masked as '***', got %v", key, r[key])
		}
	}
	if r["phone"] != "138****5678" {
		t.Fatalf("unexpected phone mask: %v", r["phone"])
	}
	if r["email"] != "a****@example.com" {
		t.Fatalf("unexpected email mask: %v", r["email"])
	}
	payload, _ := r["payload"].(string)
	if !utf8.ValidString(payload) || payload != "你好世界这是...[truncated]" {
		t.Fatalf("expected UTF-8 safe truncated payload '你好世界这是...[truncated]', got %q", payload)
	}
}

func TestSamplingHandlerStormProtection(t *testing.T) {
	var buf bytes.Buffer
	l := MustNew(
		WithWriter(&buf),
		WithFormat(FormatJSON),
		WithSampling(SamplingOptions{
			Tick:       time.Second,
			First:      5,
			Thereafter: 10,
		}),
	)
	defer l.Close()

	// Emit 35 identical messages within the same window:
	// #1..#5 allowed (5), #15 allowed (+1, dropped=9), #25 allowed (+1, dropped=9), #35 allowed (+1, dropped=9) -> total 8 allowed, 27 dropped!
	fixedNow := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 35; i++ {
		rec := slog.NewRecord(fixedNow, slog.LevelError, "redis connection refused", 0)
		_ = l.Handler().Handle(context.Background(), rec)
	}

	records := parseJSONLines(t, buf.Bytes())
	if len(records) != 8 {
		t.Fatalf("expected 8 sampled records out of 35, got %d", len(records))
	}
	if got := l.SampledDropped(); got != 27 {
		t.Fatalf("expected SampledDropped=27, got %d", got)
	}
	if records[5][SampledDroppedKey] != float64(9) {
		t.Fatalf("expected 6th emitted record (#15) to report sampled_dropped=9, got %#v", records[5])
	}
}

type slowBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	delay time.Duration
}

func (s *slowBuffer) Write(p []byte) (int, error) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *slowBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, s.buf.Len())
	copy(cp, s.buf.Bytes())
	return cp
}

func TestAsyncWriterConcurrencyAndGracefulClose(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "async.log")

	runtime.GC()
	baseGoroutines := runtime.NumGoroutine()

	l := MustNew(
		WithConsole(false),
		WithFile(logPath, FileOptions{MaxSizeMB: 50, LocalTime: true}),
		WithAsync(AsyncOptions{
			QueueSize:  2048,
			BufferSize: 32 * 1024,
			Policy:     OverflowBlock,
		}),
	)

	const workers = 20
	const perWorker = 200
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				l.Info("async-work", slog.Int("worker", id), slog.Int("seq", i))
			}
		}(w)
	}
	wg.Wait()

	if err := l.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	records := parseJSONLines(t, raw)
	if len(records) != workers*perWorker {
		t.Fatalf("expected all %d records with OverflowBlock, got %d", workers*perWorker, len(records))
	}

	time.Sleep(20 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > baseGoroutines+2 {
		t.Fatalf("goroutine leak after AsyncWriter Close: before=%d after=%d", baseGoroutines, after)
	}
}

func TestAsyncWriterDropOnFullNotice(t *testing.T) {
	sb := &slowBuffer{delay: 5 * time.Millisecond}
	aw := NewAsyncWriter(sb, 2, 1024, OverflowDrop, true)

	for i := 0; i < 50; i++ {
		_, err := aw.Write([]byte(`{"time":"2026-11-03T00:00:00Z","level":"INFO","msg":"burst"}` + "\n"))
		if err != nil {
			t.Fatalf("unexpected write error: %v", err)
		}
	}
	if aw.Dropped() == 0 {
		t.Fatalf("expected non-zero dropped count under slow sink + tiny queue")
	}
	if err := aw.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Every line including the overflow warning must be valid JSON!
	records := parseJSONLines(t, sb.Bytes())
	foundDropNotice := false
	for _, r := range records {
		if _, ok := r["dropped"]; ok {
			foundDropNotice = true
			break
		}
	}
	if !foundDropNotice {
		t.Fatalf("expected JSON overflow drop notice in output, got %d lines", len(records))
	}
}

func TestRotateAndStdLogBridge(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "bridge.log")

	l := MustNew(
		WithConsole(false),
		WithFile(logPath, FileOptions{MaxSizeMB: 10, LocalTime: true}),
	)
	stdLog := l.StdLogLogger(slog.LevelWarn)
	stdLog.Print("legacy http server error")

	if err := l.Rotate(); err != nil {
		t.Fatalf("Rotate failed: %v", err)
	}
	l.Info("after-rotate")
	if err := l.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected 2 files (active + 1 rotated backup), got %d (err=%v)", len(entries), err)
	}
}

type flakyWriter struct {
	mu   sync.Mutex
	fail atomic.Bool
	buf  bytes.Buffer
}

func (f *flakyWriter) Write(p []byte) (int, error) {
	if f.fail.Load() {
		return 0, errors.New("simulated ENOSPC disk full")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}

func (f *flakyWriter) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

func TestBufferedAndAsyncWriterErrorRecovery(t *testing.T) {
	// 1. BufferedWriter must recover cleanly after transient underlying write errors
	fw := &flakyWriter{}
	bw := NewBufferedWriter(fw, 64, 0)

	fw.fail.Store(true)
	if _, err := bw.Write([]byte(strings.Repeat("x", 100))); err == nil {
		t.Fatalf("expected error while underlying writer is failing")
	}

	fw.fail.Store(false)
	if _, err := bw.Write([]byte("recovered-line\n")); err != nil {
		t.Fatalf("expected BufferedWriter to recover after transient error, got %v", err)
	}
	if err := bw.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if fw.String() != "recovered-line\n" {
		t.Fatalf("unexpected BufferedWriter recovered output: %q", fw.String())
	}

	// 2. AsyncWriter must also recover cleanly after transient underlying write errors
	fw2 := &flakyWriter{}
	aw := NewAsyncWriter(fw2, 16, 64, OverflowBlock, true)
	fw2.fail.Store(true)
	_, _ = aw.Write([]byte(`{"msg":"lost-during-enospc"}` + "\n"))
	_ = aw.Sync()

	fw2.fail.Store(false)
	_, _ = aw.Write([]byte(`{"msg":"recovered-async"}` + "\n"))
	if err := aw.Close(); err != nil {
		t.Fatalf("AsyncWriter Close failed: %v", err)
	}
	if !strings.Contains(fw2.String(), "recovered-async") {
		t.Fatalf("expected AsyncWriter to recover after transient error, got %q", fw2.String())
	}
}

func TestClosePreventsRotateAndUnbufferedReopen(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "unbuf.log")

	l := MustNew(
		WithConsole(false),
		WithDisableBuffer(),
		WithFile(logPath, FileOptions{MaxSizeMB: 10, LocalTime: true, Compress: true}),
	)
	l.Info("before-close")
	if err := l.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if err := l.Rotate(); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("expected ErrWriterClosed on Rotate after Close, got %v", err)
	}
	if err := l.Sync(); err != nil {
		t.Fatalf("expected nil on Sync after Close, got %v", err)
	}

	// Writing after Close on unbuffered logger must NOT reopen file or write new records.
	l.Info("after-close-ignored")
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	records := parseJSONLines(t, raw)
	if len(records) != 1 || records[0]["msg"] != "before-close" {
		t.Fatalf("expected only before-close record, got %#v", records)
	}
}

func TestStringerRedactionAndZeroAllocSecretAttr(t *testing.T) {
	if got := fmt.Sprint(Secret("my-secret")); got != "***" {
		t.Fatalf("Secret String() leaked or wrong: %q", got)
	}
	if got := fmt.Sprint(Secret("")); got != "" {
		t.Fatalf("empty Secret String() expected empty, got %q", got)
	}
	if got := fmt.Sprint(MaskPhone("13812345678")); got != "138****5678" {
		t.Fatalf("MaskPhone String() leaked or wrong: %q", got)
	}
	if got := fmt.Sprint(MaskEmail("alice@example.com")); got != "a****@example.com" {
		t.Fatalf("MaskEmail String() leaked or wrong: %q", got)
	}
	if got := fmt.Sprint(MaskString("abcdef", 1, 1)); got != "a****f" {
		t.Fatalf("MaskString String() leaked or wrong: %q", got)
	}

	allocs := testing.AllocsPerRun(100, func() {
		a := SecretAttr("access_token", "eyJhbGciOiJIUzI1NiIs...")
		if a.Value.String() != "***" {
			t.Fatal("unexpected value")
		}
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs for SecretAttr, got %v", allocs)
	}
}

func TestConsoleWriterAndSourceOverride(t *testing.T) {
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer rPipe.Close()
	defer wPipe.Close()

	// Drain pipe asynchronously
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := rPipe.Read(buf); err != nil {
				return
			}
		}
	}()

	var consoleBuf bytes.Buffer
	l := MustNew(
		WithFormat(FormatJSON),
		WithConsoleWriter(&consoleBuf),
		WithConsoleFormat(FormatText),
		WithWriter(wPipe), // Real *os.File pipe: Sync() must ignore EINVAL cleanly
		WithSourceLevel(slog.LevelError),
		WithSource(true), // Must override previous WithSourceLevel and report source on Info too
	)
	defer l.Close()

	l.Info("hello-console")
	if err := l.Sync(); err != nil {
		t.Fatalf("Sync with pipe writer failed: %v", err)
	}
	out := consoleBuf.String()
	if !strings.Contains(out, "msg=hello-console") || !strings.Contains(out, "source=") {
		t.Fatalf("expected text console output with source on Info, got %q", out)
	}
}
