package slogx

import (
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// pcAttrCache caches resolved slog.Attr ("source": "pkg/file.go:line") keyed by
// program counter (uintptr). Since call sites in a compiled binary are finite,
// each call site pays runtime.CallersFrames only once across process lifetime.
var pcAttrCache sync.Map

func cachedCallerAttr(pc uintptr) (slog.Attr, bool) {
	if pc == 0 {
		return slog.Attr{}, false
	}
	if cached, ok := pcAttrCache.Load(pc); ok {
		return cached.(slog.Attr), true
	}

	fs := runtime.CallersFrames([]uintptr{pc})
	f, _ := fs.Next()
	if f.File == "" {
		return slog.Attr{}, false
	}

	formatted := formatShortCaller(f.File, f.Line)
	attr := slog.String(slog.SourceKey, formatted)
	pcAttrCache.Store(pc, attr)
	return attr, true
}

// formatShortCaller formats "/a/b/pkg/file.go" + 42 into "pkg/file.go:42",
// preserving the parent directory name to disambiguate identically named files.
func formatShortCaller(file string, line int) string {
	short := shortFilePath(file)
	if line <= 0 {
		return short
	}
	var buf [128]byte
	b := buf[:0]
	b = append(b, short...)
	b = append(b, ':')
	b = strconv.AppendInt(b, int64(line), 10)
	return string(b)
}

func shortFilePath(file string) string {
	// Normalize Windows backslashes if present so path trimming is uniform.
	norm := file
	if strings.IndexByte(norm, '\\') >= 0 {
		norm = strings.ReplaceAll(norm, "\\", "/")
	}
	idx := strings.LastIndexByte(norm, '/')
	if idx < 0 {
		return norm
	}
	parentIdx := strings.LastIndexByte(norm[:idx], '/')
	if parentIdx < 0 {
		return norm
	}
	return norm[parentIdx+1:]
}
