package slogx_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/leonlwq/slogx"
)

func omitTimeAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

func ExampleNew() {
	log, err := slogx.New(
		slogx.WithConsoleWriter(os.Stdout),
		slogx.WithFormat(slogx.FormatJSON),
		slogx.WithSensitiveKeys(),
		slogx.WithReplaceAttr(omitTimeAttr),
	)
	if err != nil {
		panic(err)
	}
	defer log.Close()

	ctx := slogx.WithTraceID(context.Background(), "4bf92f3577b34da6")
	log.InfoContext(ctx, "user login",
		slog.Any("phone", slogx.MaskPhone("13812345678")),
		slog.String("password", "s3cr3t"),
	)
	// Output:
	// {"level":"INFO","msg":"user login","phone":"138****5678","password":"***","trace_id":"4bf92f3577b34da6"}
}

func ExampleLogger_LevelHandler() {
	log := slogx.MustNew(slogx.WithConsole(false))
	defer log.Close()

	req := httptest.NewRequest(http.MethodPut, "/debug/loglevel", strings.NewReader(`{"level":"debug"}`))
	rec := httptest.NewRecorder()
	log.LevelHandler().ServeHTTP(rec, req)

	fmt.Print(rec.Body.String())
	// Output:
	// {"level":"DEBUG"}
}

