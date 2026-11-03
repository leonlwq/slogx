package slogx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/slogtest"
	"time"
	"unicode/utf8"
	"unsafe"
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

func TestSamplingHandlerWindowRolloverAndPadding(t *testing.T) {
	if sz := unsafe.Sizeof(sampleBucket{}); sz != cacheLinePadSize {
		t.Fatalf("expected sampleBucket size %d bytes, got %d", cacheLinePadSize, sz)
	}

	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, nil)
	sh := NewSamplingHandler(base, SamplingOptions{
		Tick:       100 * time.Millisecond,
		First:      3,
		Thereafter: 5,
	})

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	send := func(ts time.Time) {
		r := slog.NewRecord(ts, slog.LevelError, "storm-event", 0)
		if err := sh.Handle(context.Background(), r); err != nil {
			t.Fatalf("Handle failed: %v", err)
		}
	}

	// Window 1: 10 records -> #1, #2, #3, #8 (sampled_dropped=4) emitted; #4..7 and #9..10 suppressed (total 6 dropped, 2 tail drops).
	for i := 0; i < 10; i++ {
		send(t0.Add(time.Duration(i) * time.Millisecond))
	}
	if got := sh.Dropped(); got != 6 {
		t.Fatalf("window 1: expected 6 dropped, got %d", got)
	}
	recs := parseJSONLines(t, buf.Bytes())
	if len(recs) != 4 {
		t.Fatalf("window 1: expected 4 emitted records, got %d", len(recs))
	}
	if recs[3][SampledDroppedKey] != float64(4) {
		t.Fatalf("window 1: expected 4th emitted record sampled_dropped=4, got %#v", recs[3])
	}

	// Window 2: 1st record rolls over window and reports the 2 unreported tail drops from Window 1.
	send(t0.Add(150 * time.Millisecond))
	if got := sh.Dropped(); got != 6 {
		t.Fatalf("window 2 rollover: expected 6 dropped, got %d", got)
	}
	recs = parseJSONLines(t, buf.Bytes())
	if len(recs) != 5 || recs[4][SampledDroppedKey] != float64(2) {
		t.Fatalf("window 2 rollover: expected 5th record sampled_dropped=2, got %#v", recs)
	}

	// Send 4 more in Window 2 (total 5 in Window 2 -> 3 emitted, 2 suppressed -> cumulative 8 dropped).
	for i := 1; i <= 4; i++ {
		send(t0.Add(150*time.Millisecond + time.Duration(i)*time.Millisecond))
	}
	if got := sh.Dropped(); got != 8 {
		t.Fatalf("window 2 end: expected 8 cumulative dropped, got %d", got)
	}

	// Window 3: Thereafter == 0 mode check
	var bufZero bytes.Buffer
	shZero := NewSamplingHandler(slog.NewJSONHandler(&bufZero, nil), SamplingOptions{
		Tick:       100 * time.Millisecond,
		First:      2,
		Thereafter: 0,
	})
	for i := 0; i < 7; i++ {
		r := slog.NewRecord(t0.Add(time.Duration(i)*time.Millisecond), slog.LevelWarn, "drop-all-excess", 0)
		_ = shZero.Handle(context.Background(), r)
	}
	if got := shZero.Dropped(); got != 5 {
		t.Fatalf("Thereafter=0 window 1: expected 5 dropped, got %d", got)
	}
	rNext := slog.NewRecord(t0.Add(200*time.Millisecond), slog.LevelWarn, "drop-all-excess", 0)
	_ = shZero.Handle(context.Background(), rNext)
	zeroRecs := parseJSONLines(t, bufZero.Bytes())
	if len(zeroRecs) != 3 || zeroRecs[2][SampledDroppedKey] != float64(5) {
		t.Fatalf("Thereafter=0 rollover: expected sampled_dropped=5, got %#v", zeroRecs)
	}
}

func TestMultiSinkSharedSerializationAndErrorIsolation(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	failing := &flakyWriter{}
	failing.fail.Store(true)

	var replaceCount atomic.Int64
	l := MustNew(
		WithFormat(FormatJSON),
		WithConsoleWriter(&buf1),
		WithWriter(failing), // Fails on every Write; must NOT abort buf2!
		WithWriter(&buf2),
		WithReplaceAttr(func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == "order_id" {
				replaceCount.Add(1)
			}
			return a
		}),
	)
	defer l.Close()

	l.Info("checkout", slog.String("order_id", "ord-99"))

	// Single-pass serialization: ReplaceAttr for "order_id" runs once despite 3 JSON sinks.
	if got := replaceCount.Load(); got != 1 {
		t.Fatalf("expected single ReplaceAttr pass across shared JSON sinks, got %d", got)
	}
	recs1 := parseJSONLines(t, buf1.Bytes())
	recs2 := parseJSONLines(t, buf2.Bytes())
	if len(recs1) != 1 || len(recs2) != 1 {
		t.Fatalf("expected both healthy sinks to receive log despite failing sink, got %d and %d", len(recs1), len(recs2))
	}
	if recs1[0]["order_id"] != "ord-99" || recs2[0]["order_id"] != "ord-99" {
		t.Fatalf("unexpected sink payloads: %#v / %#v", recs1[0], recs2[0])
	}
}

func TestConcurrentGroupedAndUngroupedWithSourceRace(t *testing.T) {
	var buf bytes.Buffer // Intentionally unsynchronized writer
	l := MustNew(
		WithFormat(FormatJSON),
		WithConsoleWriter(&buf),
		WithSource(true),
	)
	defer l.Close()

	grouped := l.WithGroup("req")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				l.Info("ungrouped", slog.Int("g", id), slog.Int("i", i))
			}
		}(g)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				grouped.Info("grouped", slog.Int("g", id), slog.Int("i", i))
			}
		}(g)
	}
	wg.Wait()

	recs := parseJSONLines(t, buf.Bytes())
	if len(recs) != 800 {
		t.Fatalf("expected 800 serialized JSON lines without race/interleaving, got %d", len(recs))
	}
}

func isRaceEnabled() bool {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, s := range bi.Settings {
		if s.Key == "-race" && s.Value == "true" {
			return true
		}
	}
	return false
}

func TestZeroAllocWithSensitiveKeysAndSource(t *testing.T) {
	if isRaceEnabled() {
		t.Skip("skipping exact sync.Pool zero-alloc assertion under -race (sync.Pool drops items randomly under race detector)")
	}
	l := MustNew(
		WithConsole(false),
		WithWriter(io.Discard),
		WithFormat(FormatJSON),
		WithSensitiveKeys(DefaultSensitiveKeys...),
		WithMaxFieldLength(4096),
		WithSource(true),
	)
	defer l.Close()

	ctx := context.Background()
	// Warm up PC cache once
	l.LogAttrs(ctx, slog.LevelInfo, "warmup", slog.String("method", "GET"), slog.String("password", "s3cr3t"))

	allocs := testing.AllocsPerRun(200, func() {
		l.LogAttrs(ctx, slog.LevelInfo, "steady-state", slog.String("method", "GET"), slog.String("password", "s3cr3t"))
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs/op on steady-state JSON log with sensitive redaction + PC cache, got %v", allocs)
	}
}

func TestDeclarativeConfigAndEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	appPath := filepath.Join(dir, "app.log")
	warnPath := filepath.Join(dir, "warn.log")
	errPath := filepath.Join(dir, "error.log")

	rawJSON := fmt.Sprintf(`{
		"level": "warn",
		"module_levels": {"rpc": "error"},
		"format": "json",
		"console": false,
		"file": {"filename": %q, "max_size_mb": 10},
		"error_file": {"filename": %q, "max_size_mb": 10},
		"files": [
			{"filename": %q, "level": "warn", "rotation_interval": "daily"}
		],
		"service": {"name": "billing", "version": "v2.0.0", "env": "prod"},
		"attrs": {"region": "ap-east-1"},
		"enable_default_sensitive": true,
		"sensitive_keys": ["card_no"],
		"max_field_len": 64,
		"sampling": {"tick": "1s", "first": 100, "thereafter": 100}
	}`, appPath, errPath, warnPath)

	var cfg Config
	if err := json.Unmarshal([]byte(rawJSON), &cfg); err != nil {
		t.Fatalf("Unmarshal Config failed: %v", err)
	}

	t.Setenv("BILLING_LOG_LEVEL", "info")
	t.Setenv("BILLING_LOG_MODULE_LEVELS", "db=debug, rpc=warn")

	l, err := NewFromConfig(cfg, WithEnvPrefix("BILLING_"))
	if err != nil {
		t.Fatalf("NewFromConfig failed: %v", err)
	}

	if l.Level() != slog.LevelInfo {
		t.Fatalf("expected env override level INFO, got %v", l.Level())
	}

	dbLog := l.Named("db")
	rpcLog := l.Named("rpc")

	l.Debug("root-debug-dropped")
	dbLog.Debug("db-debug-kept", slog.String("card_no", "4111222233334444"))
	rpcLog.Info("rpc-info-dropped")
	rpcLog.Warn("rpc-warn-kept")
	l.Error("root-err-kept")

	if err := l.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	appRecs := parseJSONLines(t, mustReadFile(t, appPath))
	warnRecs := parseJSONLines(t, mustReadFile(t, warnPath))
	errRecs := parseJSONLines(t, mustReadFile(t, errPath))

	if len(appRecs) != 3 {
		t.Fatalf("expected 3 records in app.log (db debug, rpc warn, root err), got %d: %#v", len(appRecs), appRecs)
	}
	if appRecs[0]["card_no"] != "***" || appRecs[0]["region"] != "ap-east-1" || appRecs[0]["service"] != "billing" {
		t.Fatalf("unexpected attributes in app.log[0]: %#v", appRecs[0])
	}
	if len(warnRecs) != 2 {
		t.Fatalf("expected 2 records in warn.log (rpc warn, root err), got %d", len(warnRecs))
	}
	if len(errRecs) != 1 || errRecs[0]["msg"] != "root-err-kept" {
		t.Fatalf("expected 1 record in error.log, got %#v", errRecs)
	}

	// Verify config validation errors
	if _, err := NewFromConfig(Config{Async: &AsyncConfig{Enabled: true, Policy: "invalid"}}); err == nil {
		t.Fatal("expected error for invalid async policy")
	}
	if _, err := NewFromConfig(Config{File: &FileConfig{Filename: appPath, RotationInterval: "bad"}}); err == nil {
		t.Fatal("expected error for invalid rotation_interval")
	}
}

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", p, err)
	}
	return b
}

func TestGeneralizedSinksAndLevelFilter(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.log")
	auditOpts := DefaultFileOptions(auditPath)

	var infoOnlyBuf, customBuf bytes.Buffer
	var synced, closed atomic.Int32

	l := MustNew(
		WithLevel(slog.LevelInfo),
		// Sink 1: exact bounded range [INFO, WARN) -> excludes WARN and ERROR!
		WithSink(SinkOptions{
			Writer: &infoOnlyBuf,
			Format: FormatJSON,
			LevelFilter: func(lvl slog.Level) bool {
				return lvl < slog.LevelWarn
			},
		}),
		// Sink 2: extra rolling file at WARN+ in Text format
		WithSink(SinkOptions{
			File:   &auditOpts,
			Format: FormatText,
			Level:  slog.LevelWarn,
		}),
		// Sink 3: custom managed writer with SyncFn & CloseFn lifecycle hooks
		WithSink(SinkOptions{
			Writer:  &customBuf,
			Format:  FormatJSON,
			SyncFn:  func() error { synced.Add(1); return nil },
			CloseFn: func() error { closed.Add(1); return nil },
		}),
	)

	l.Info("info-msg")
	l.Warn("warn-msg")
	l.Error("err-msg")

	if err := l.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if synced.Load() == 0 {
		t.Fatal("expected SinkOptions.SyncFn to be invoked on Logger.Sync()")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if closed.Load() != 1 {
		t.Fatalf("expected SinkOptions.CloseFn to be invoked once, got %d", closed.Load())
	}

	// Writes after Close must not hit customBuf
	l.Info("after-close")

	infoRecs := parseJSONLines(t, infoOnlyBuf.Bytes())
	if len(infoRecs) != 1 || infoRecs[0]["msg"] != "info-msg" {
		t.Fatalf("expected infoOnlyBuf to contain only info-msg, got %#v", infoRecs)
	}
	customRecs := parseJSONLines(t, customBuf.Bytes())
	if len(customRecs) != 3 {
		t.Fatalf("expected customBuf to contain 3 pre-close records, got %d", len(customRecs))
	}
	auditText := string(mustReadFile(t, auditPath))
	if strings.Contains(auditText, "info-msg") || !strings.Contains(auditText, "warn-msg") || !strings.Contains(auditText, "err-msg") {
		t.Fatalf("unexpected audit.log text content: %q", auditText)
	}

	// Invalid SinkOptions check
	if _, err := New(WithSink(SinkOptions{})); err == nil {
		t.Fatal("expected error for empty SinkOptions")
	}
}

type OrderMiddleware struct {
	slog.Handler
	tag   string
	order *[]string
}

func (m *OrderMiddleware) Handle(ctx context.Context, r slog.Record) error {
	*m.order = append(*m.order, m.tag)
	return m.Handler.Handle(ctx, r)
}

func (m *OrderMiddleware) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &OrderMiddleware{Handler: m.Handler.WithAttrs(attrs), tag: m.tag, order: m.order}
}

func (m *OrderMiddleware) WithGroup(name string) slog.Handler {
	return &OrderMiddleware{Handler: m.Handler.WithGroup(name), tag: m.tag, order: m.order}
}

func TestExternalHandlerAndMiddlewareChain(t *testing.T) {
	var extBuf, sinkBuf bytes.Buffer
	extHandler := slog.NewJSONHandler(&extBuf, nil)

	var order []string
	mw1 := func(next slog.Handler) slog.Handler {
		return &OrderMiddleware{Handler: next, tag: "mw1", order: &order}
	}
	mw2 := func(next slog.Handler) slog.Handler {
		return &OrderMiddleware{Handler: next, tag: "mw2", order: &order}
	}

	l := MustNew(
		WithWriter(&sinkBuf),
		WithHandler(extHandler),
		WithService("auth-svc", "v1.0.0", "dev"),
		WithSource(true),
		WithHandlerMiddleware(mw1, mw2),
	)
	defer l.Close()

	ctx := WithTraceID(context.Background(), "trace-abc")
	l.WithGroup("req").InfoContext(ctx, "token issued", slog.String("user", "bob"))

	if strings.Join(order, "->") != "mw1->mw2" {
		t.Fatalf("expected middleware order mw1->mw2, got %v", order)
	}

	extRecs := parseJSONLines(t, extBuf.Bytes())
	sinkRecs := parseJSONLines(t, sinkBuf.Bytes())
	if len(extRecs) != 1 || len(sinkRecs) != 1 {
		t.Fatalf("expected 1 record in both external handler and writer sink, got %d / %d", len(extRecs), len(sinkRecs))
	}
	if extRecs[0]["service"] != "auth-svc" {
		t.Fatalf("expected pre-formatted service attr on external handler, got %#v", extRecs[0])
	}
	reqGrp, _ := extRecs[0]["req"].(map[string]any)
	if reqGrp["user"] != "bob" || reqGrp["trace_id"] != "trace-abc" {
		t.Fatalf("expected grouped attrs and trace_id in external handler, got %#v", extRecs[0])
	}
}

type blockingSlowWriter struct {
	unblock chan struct{}
	buf     bytes.Buffer
	mu      sync.Mutex
}

func (b *blockingSlowWriter) Write(p []byte) (int, error) {
	<-b.unblock
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func TestErrorHandlerHookAndAsyncOverflowError(t *testing.T) {
	// 1. Synchronous write failure reported to ErrorHandler
	failing := &flakyWriter{}
	failing.fail.Store(true)

	var syncErrCount atomic.Int32
	lSync := MustNew(
		WithWriter(failing),
		WithErrorHandler(func(err error) {
			if err != nil {
				syncErrCount.Add(1)
			}
		}),
	)
	lSync.Info("triggers-write-err")
	_ = lSync.Close()
	if syncErrCount.Load() != 1 {
		t.Fatalf("expected ErrorHandler to receive 1 write error, got %d", syncErrCount.Load())
	}

	// 2. AsyncWriter queue overflow reported via AsyncOverflowError (errors.Is / errors.As)
	slow := &blockingSlowWriter{unblock: make(chan struct{})}
	var overflowReported atomic.Uint64
	aw := newAsyncWriter(slow, 2, 1024, OverflowDrop, true, func(err error) {
		if errors.Is(err, ErrAsyncOverflow) {
			var oe *AsyncOverflowError
			if errors.As(err, &oe) {
				overflowReported.Add(oe.Dropped)
			}
		}
	})

	for i := 0; i < 30; i++ {
		_, _ = aw.Write([]byte(`{"msg":"flood"}` + "\n"))
	}
	close(slow.unblock)
	_ = aw.Close()

	if aw.Dropped() == 0 || overflowReported.Load() != aw.Dropped() {
		t.Fatalf("expected AsyncOverflowError count (%d) to equal aw.Dropped() (%d)", overflowReported.Load(), aw.Dropped())
	}
}

func TestNamedModuleLevelsAndHTTPManagement(t *testing.T) {
	var buf bytes.Buffer
	l := MustNew(
		WithWriter(&buf),
		WithFormat(FormatJSON),
		WithLevel(slog.LevelInfo),
	)
	defer l.Close()

	httpLog := l.Named("http")
	authLog := httpLog.Named("auth")
	if authLog.Name() != "http.auth" || l.Name() != "" {
		t.Fatalf("unexpected logger names: auth=%q root=%q", authLog.Name(), l.Name())
	}

	// Initially root=INFO -> DEBUG suppressed on authLog
	authLog.Debug("hidden-1")
	if buf.Len() != 0 {
		t.Fatalf("expected debug suppressed before module override, got %s", buf.String())
	}

	h := l.LevelHandler()

	// Enable DEBUG on parent module "http" via HTTP PUT -> "http.auth" inherits DEBUG!
	req := httptest.NewRequest(http.MethodPut, "/debug/loglevel?module=http&level=debug", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || authLog.Level() != slog.LevelDebug || l.Level() != slog.LevelInfo {
		t.Fatalf("module override failed: code=%d authLevel=%v rootLevel=%v", rec.Code, authLog.Level(), l.Level())
	}

	l.Debug("root-still-hidden")
	authLog.Debug("auth-debug-visible")

	// Narrow child "http.auth" to WARN via JSON body while "http" stays DEBUG
	req = httptest.NewRequest(http.MethodPut, "/debug/loglevel", strings.NewReader(`{"module":"http.auth","level":"warn"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || authLog.Level() != slog.LevelWarn || httpLog.Level() != slog.LevelDebug {
		t.Fatalf("child override failed: code=%d auth=%v http=%v", rec.Code, authLog.Level(), httpLog.Level())
	}

	// GET snapshot includes modules map, and GET ?module=http.auth returns WARN
	req = httptest.NewRequest(http.MethodGet, "/debug/loglevel", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"http.auth":"WARN"`) {
		t.Fatalf("expected modules snapshot in GET, got %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/debug/loglevel?module=http.auth", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"level":"WARN"`) {
		t.Fatalf("expected module WARN on GET ?module=http.auth, got %s", rec.Body.String())
	}

	// Reset both overrides -> authLog returns to INFO
	req = httptest.NewRequest(http.MethodPut, "/debug/loglevel?module=http.auth&level=reset", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	l.ResetModuleLevel("http")
	if authLog.Level() != slog.LevelInfo || len(l.ModuleLevels()) != 0 {
		t.Fatalf("expected reset back to INFO with empty overrides, got level=%v map=%v", authLog.Level(), l.ModuleLevels())
	}

	// Ensure nested Named("http").Named("auth") emitted a single "logger":"http.auth" key without duplicate "logger" keys!
	rawLine := strings.TrimSpace(buf.String())
	if strings.Count(rawLine, `"logger":`) != 1 {
		t.Fatalf("expected exactly one logger key in JSON output, got %s", rawLine)
	}
	recs := parseJSONLines(t, buf.Bytes())
	if len(recs) != 1 || recs[0]["logger"] != "http.auth" || recs[0]["msg"] != "auth-debug-visible" {
		t.Fatalf("unexpected records: %#v", recs)
	}
}

func TestPrettyHandlerColorMultilineAndStack(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "app.json")

	var prettyPlainBuf, prettyColorBuf bytes.Buffer
	l := MustNew(
		WithFormat(FormatJSON),
		WithConsoleWriter(&prettyPlainBuf),
		WithConsoleFormat(FormatPretty), // Auto color on bytes.Buffer -> plain (no ANSI)
		WithSink(SinkOptions{
			Writer: &prettyColorBuf,
			Format: FormatPretty,
			Color:  Bool(true), // Explicit ANSI color enabled
		}),
		WithFile(jsonPath, FileOptions{MaxSizeMB: 10, LocalTime: true}),
		WithSource(true),
		WithSensitiveKeys(),
	)

	multiErr := errors.New("rpc failed:\n  upstream reset by peer\n  attempts=3")
	l.WithGroup("order").Info("created", slog.String("id", "ord-7"), slog.String("secret", "top-secret"))
	l.WithGroup("empty_grp").Info("no-group-attrs")
	l.Error("checkout panicked", slog.Any("err", multiErr), Stack())

	if err := l.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	plainOut := prettyPlainBuf.String()
	colorOut := prettyColorBuf.String()

	if strings.Contains(plainOut, "\x1b[") {
		t.Fatalf("expected auto-detected bytes.Buffer pretty output to contain no ANSI codes, got %q", plainOut)
	}
	if !strings.Contains(colorOut, "\x1b[32mINFO ") || !strings.Contains(colorOut, "\x1b[1;31mERROR") {
		t.Fatalf("expected forced-color pretty output to contain ANSI level badges, got %q", colorOut)
	}
	if !strings.Contains(plainOut, "order.id=ord-7") || !strings.Contains(plainOut, "order.secret=***") {
		t.Fatalf("expected grouped inline attrs and redaction in pretty output, got:\n%s", plainOut)
	}
	if strings.Contains(plainOut, "empty_grp") {
		t.Fatalf("expected empty WithGroup to be omitted in pretty output, got:\n%s", plainOut)
	}
	if !strings.Contains(plainOut, "\n  err:\n    rpc failed:\n      upstream reset by peer") {
		t.Fatalf("expected indented multi-line error block in pretty output, got:\n%s", plainOut)
	}
	if !strings.Contains(plainOut, "\n  stack:\n    ") || !strings.Contains(plainOut, "logger_test.go:") {
		t.Fatalf("expected indented stacktrace block in pretty output, got:\n%s", plainOut)
	}

	// Meanwhile app.json must remain valid newline-delimited JSON!
	jsonRecs := parseJSONLines(t, mustReadFile(t, jsonPath))
	if len(jsonRecs) != 3 {
		t.Fatalf("expected 3 valid JSON lines in app.json alongside pretty console, got %d", len(jsonRecs))
	}
	stackStr, _ := jsonRecs[2][StackKey].(string)
	if !strings.Contains(stackStr, "TestPrettyHandlerColorMultilineAndStack") {
		t.Fatalf("expected JSON stack field to contain test function name, got %q", stackStr)
	}

	// Verify NO_COLOR suppresses color even on a character device (os.DevNull on Unix).
	if devNull, err := os.Open(os.DevNull); err == nil {
		defer devNull.Close()
		t.Setenv("NO_COLOR", "1")
		if shouldColorize(devNull, nil) {
			t.Fatal("expected NO_COLOR=1 to disable auto colorization on char device")
		}
	}

	// Verify 0 allocs/op on steady-state PrettyHandler
	if !isRaceEnabled() {
		ph := NewPrettyHandler(io.Discard, &PrettyOptions{Color: Bool(true)})
		pl := slog.New(ph)
		ctx := context.Background()
		pl.LogAttrs(ctx, slog.LevelInfo, "warmup", slog.String("k", "v"), slog.Int("n", 1))
		allocs := testing.AllocsPerRun(200, func() {
			pl.LogAttrs(ctx, slog.LevelInfo, "steady", slog.String("k", "v"), slog.Int("n", 1))
		})
		if allocs != 0 {
			t.Fatalf("expected 0 allocs/op for PrettyHandler steady-state, got %v", allocs)
		}
	}
}

func TestMultiHandlerAndDiscardHandler(t *testing.T) {
	// Empty combine -> discardHandler
	dh := newMultiHandler(nil, discardHandler{})
	if dh.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("expected discardHandler.Enabled to be false")
	}
	if err := dh.WithAttrs([]slog.Attr{slog.Int("a", 1)}).WithGroup("g").Handle(context.Background(), slog.Record{}); err != nil {
		t.Fatalf("unexpected discardHandler error: %v", err)
	}

	var b1, b2 bytes.Buffer
	failing := &flakyWriter{}
	failing.fail.Store(true)

	h1 := slog.NewJSONHandler(&b1, nil)
	hFail := slog.NewJSONHandler(failing, nil)
	h2 := slog.NewJSONHandler(&b2, nil)

	// Nested multiHandler flattening + error non-short-circuiting
	nested := newMultiHandler(h1, newMultiHandler(hFail, h2))
	sl := slog.New(nested.WithAttrs([]slog.Attr{slog.String("env", "test")}).WithGroup("g"))
	sl.Info("fanout", slog.Int("x", 1))

	r1 := parseJSONLines(t, b1.Bytes())
	r2 := parseJSONLines(t, b2.Bytes())
	if len(r1) != 1 || len(r2) != 1 {
		t.Fatalf("expected both healthy handlers in multiHandler to receive record, got %d / %d", len(r1), len(r2))
	}
}

func TestContextCorrelationChainingAndModuleRegistryCap(t *testing.T) {
	// 1. Nil & chained correlation context preservation + partial child override
	if TraceID(nil) != "" || SpanID(nil) != "" || RequestID(nil) != "" {
		t.Fatal("expected empty correlation IDs for nil context")
	}
	parentCtx := WithTraceID(nil, "trace-1")
	parentCtx = WithSpanID(parentCtx, "span-parent")
	parentCtx = WithRequestID(parentCtx, "req-1")

	childCtx := WithSpanID(parentCtx, "span-child")
	if TraceID(childCtx) != "trace-1" || SpanID(childCtx) != "span-child" || RequestID(childCtx) != "req-1" {
		t.Fatalf("unexpected childCtx correlation: trace=%q span=%q req=%q", TraceID(childCtx), SpanID(childCtx), RequestID(childCtx))
	}
	// Parent context must remain immutable!
	if SpanID(parentCtx) != "span-parent" {
		t.Fatalf("parentCtx mutated by child WithSpanID: got %q", SpanID(parentCtx))
	}

	// 2. Option validation errors on invalid level / empty module
	if _, err := New(WithLevelString("bogus-level")); err == nil {
		t.Fatal("expected error for invalid WithLevelString")
	}
	if _, err := New(WithModuleLevel(" . ", slog.LevelDebug)); err == nil {
		t.Fatal("expected error for empty WithModuleLevel")
	}
	if _, err := New(WithModuleLevelString("db", "bogus-level")); err == nil {
		t.Fatal("expected error for invalid WithModuleLevelString")
	}

	// 3. Read-only HTTP GET ?module=... must not insert into moduleRegistry, and
	// unbounded dynamic Named(...) calls must stay capped at maxRegisteredModules.
	l := MustNew(WithWriter(io.Discard), WithLevel(slog.LevelInfo))
	defer l.Close()

	h := l.LevelHandler()
	req := httptest.NewRequest(http.MethodGet, "/debug/level?module=ephemeral.probe", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	l.core.modules.mu.Lock()
	afterGetLen := len(l.core.modules.nodes)
	l.core.modules.mu.Unlock()
	if afterGetLen != 0 {
		t.Fatalf("expected GET ?module=... to allocate 0 moduleRegistry nodes, got %d", afterGetLen)
	}

	l.SetModuleLevel("tenant", slog.LevelDebug)
	for i := 0; i < maxRegisteredModules+1000; i++ {
		_ = l.Named(fmt.Sprintf("tenant.u_%d", i))
	}
	l.core.modules.mu.Lock()
	cappedLen := len(l.core.modules.nodes)
	l.core.modules.mu.Unlock()
	if cappedLen > maxRegisteredModules {
		t.Fatalf("expected moduleRegistry nodes <= %d, got %d", maxRegisteredModules, cappedLen)
	}

	// Even an uncached sub-logger created after reaching the cap must inherit parent ("tenant") DEBUG level!
	overflowSub := l.Named("tenant.u_overflow_999999")
	if overflowSub.Level() != slog.LevelDebug {
		t.Fatalf("expected uncached child module to inherit parent DEBUG level, got %v", overflowSub.Level())
	}
}
