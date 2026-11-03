package slogx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ansiReset   = "\x1b[0m"
	ansiDim     = "\x1b[90m"
	ansiCyan    = "\x1b[36m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiRed     = "\x1b[31m"
	ansiBoldRed = "\x1b[1;31m"
)

// PrettyOptions configures a human-readable, developer-friendly PrettyHandler
// with optional ANSI colors and indented multi-line error/stacktrace rendering.
type PrettyOptions struct {
	Level       slog.Leveler
	AddSource   bool
	TimeFormat  string // Defaults to TimeFormatDateTimeMilli ("2006-01-02 15:04:05.000")
	Color       *bool  // nil = auto-detect TTY & NO_COLOR; Bool(true) = force ANSI; Bool(false) = plain
	ReplaceAttr func(groups []string, a slog.Attr) slog.Attr
}

type groupOrAttrs struct {
	group string
	attrs []slog.Attr
}

// PrettyHandler is an slog.Handler that formats records as concise, aligned
// console lines with optional ANSI colors and clean multi-line stacktrace blocks.
type PrettyHandler struct {
	w             io.Writer
	mu            *sync.Mutex
	level         slog.Leveler
	addSource     bool
	fastCachedSrc bool
	colorize      bool
	timeFormat    string
	replaceAttr   func(groups []string, a slog.Attr) slog.Attr
	goas          []groupOrAttrs
}

// NewPrettyHandler creates a standalone PrettyHandler writing to w.
func NewPrettyHandler(w io.Writer, opts *PrettyOptions) *PrettyHandler {
	if w == nil {
		w = io.Discard
	}
	var o PrettyOptions
	if opts != nil {
		o = *opts
	}
	return newPrettyHandlerInternal(w, o, o.ReplaceAttr == nil)
}

func newPrettyHandlerInternal(w io.Writer, o PrettyOptions, fastCachedSrc bool) *PrettyHandler {
	tf := o.TimeFormat
	if tf == "" {
		tf = TimeFormatDateTimeMilli
	}
	lvl := o.Level
	if lvl == nil {
		lvl = slog.LevelInfo
	}
	return &PrettyHandler{
		w:             w,
		mu:            &sync.Mutex{},
		level:         lvl,
		addSource:     o.AddSource,
		fastCachedSrc: fastCachedSrc,
		colorize:      shouldColorize(w, o.Color),
		timeFormat:    tf,
		replaceAttr:   o.ReplaceAttr,
	}
}

func shouldColorize(w io.Writer, explicit *bool) bool {
	if explicit != nil {
		return *explicit
	}
	if os.Getenv("NO_COLOR") != "" || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

type multilineAttr struct {
	key string
	val string
}

type prettyState struct {
	buf       []byte
	attrBuf   []byte
	caller    string
	hasCaller bool
	groups    []string
	multiline []multilineAttr
}

var prettyPool = sync.Pool{
	New: func() any {
		return &prettyState{
			buf:       make([]byte, 0, 512),
			attrBuf:   make([]byte, 0, 256),
			groups:    make([]string, 0, 8),
			multiline: make([]multilineAttr, 0, 4),
		}
	},
}

func getPrettyState() *prettyState {
	ps := prettyPool.Get().(*prettyState)
	ps.buf = ps.buf[:0]
	ps.attrBuf = ps.attrBuf[:0]
	ps.caller = ""
	ps.hasCaller = false
	ps.groups = ps.groups[:0]
	ps.multiline = ps.multiline[:0]
	return ps
}

func putPrettyState(ps *prettyState) {
	if cap(ps.buf) > maxPooledBufCap || cap(ps.attrBuf) > maxPooledBufCap {
		return
	}
	prettyPool.Put(ps)
}

// Enabled reports whether the handler handles records at the given level.
func (h *PrettyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle formats r as a human-readable line with optional multi-line blocks.
func (h *PrettyHandler) Handle(_ context.Context, r slog.Record) error {
	ps := getPrettyState()
	defer putPrettyState(ps)

	// 1. Resolve native source if AddSource is enabled on PrettyHandler.
	if h.addSource && r.PC != 0 {
		h.resolveNativeCaller(ps, r.PC)
	}

	// 2. Format attributes first so any ungrouped PC-cached "source" attribute
	// injected by pipelineHandler can be hoisted into the header position.
	for _, goa := range h.goas {
		if goa.group != "" {
			ps.groups = append(ps.groups, goa.group)
		} else {
			for _, a := range goa.attrs {
				h.appendAttr(ps, a)
			}
		}
	}
	r.Attrs(func(a slog.Attr) bool {
		h.appendAttr(ps, a)
		return true
	})

	// 3. Assemble header: Time + Level + Caller + Message.
	needSpace := false
	if !r.Time.IsZero() {
		if h.appendHeaderTime(ps, r.Time) {
			needSpace = true
		}
	}
	if h.appendHeaderLevel(ps, r.Level, needSpace) {
		needSpace = true
	}
	if ps.hasCaller && ps.caller != "" {
		if needSpace {
			ps.buf = append(ps.buf, ' ')
		}
		if h.colorize {
			ps.buf = append(ps.buf, ansiDim...)
			ps.buf = append(ps.buf, ps.caller...)
			ps.buf = append(ps.buf, ansiReset...)
		} else {
			ps.buf = append(ps.buf, ps.caller...)
		}
		needSpace = true
	}
	if h.appendHeaderMessage(ps, r.Message, needSpace) {
		needSpace = true
	}

	// 4. Append inline key=value attributes and trailing newline.
	if len(ps.attrBuf) > 0 {
		if !needSpace && ps.attrBuf[0] == ' ' {
			ps.buf = append(ps.buf, ps.attrBuf[1:]...)
		} else {
			ps.buf = append(ps.buf, ps.attrBuf...)
		}
	}
	ps.buf = append(ps.buf, '\n')

	// 5. Render indented multi-line attributes (stacktraces, multi-line errors).
	for _, ml := range ps.multiline {
		ps.buf = append(ps.buf, "  "...)
		if h.colorize {
			if r.Level >= slog.LevelError {
				ps.buf = append(ps.buf, ansiRed...)
			} else if r.Level >= slog.LevelWarn {
				ps.buf = append(ps.buf, ansiYellow...)
			} else {
				ps.buf = append(ps.buf, ansiCyan...)
			}
			ps.buf = append(ps.buf, ml.key...)
			ps.buf = append(ps.buf, ':')
			ps.buf = append(ps.buf, ansiReset...)
		} else {
			ps.buf = append(ps.buf, ml.key...)
			ps.buf = append(ps.buf, ':')
		}
		ps.buf = append(ps.buf, '\n')

		rest := strings.TrimRight(ml.val, "\r\n")
		for {
			line, tail, hasMore := strings.Cut(rest, "\n")
			line = strings.TrimSuffix(line, "\r")
			ps.buf = append(ps.buf, "    "...)
			if h.colorize {
				ps.buf = append(ps.buf, ansiDim...)
				ps.buf = append(ps.buf, line...)
				ps.buf = append(ps.buf, ansiReset...)
			} else {
				ps.buf = append(ps.buf, line...)
			}
			ps.buf = append(ps.buf, '\n')
			if !hasMore {
				break
			}
			rest = tail
		}
	}

	h.mu.Lock()
	_, err := h.w.Write(ps.buf)
	h.mu.Unlock()
	return err
}

// WithAttrs returns a new PrettyHandler with the given attributes attached.
func (h *PrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	cp := *h
	cp.goas = make([]groupOrAttrs, len(h.goas)+1)
	copy(cp.goas, h.goas)
	cp.goas[len(h.goas)] = groupOrAttrs{attrs: slices.Clone(attrs)}
	return &cp
}

// WithGroup returns a new PrettyHandler that qualifies subsequent keys with name.
func (h *PrettyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	cp := *h
	cp.goas = make([]groupOrAttrs, len(h.goas)+1)
	copy(cp.goas, h.goas)
	cp.goas[len(h.goas)] = groupOrAttrs{group: name}
	return &cp
}

func (h *PrettyHandler) resolveNativeCaller(ps *prettyState, pc uintptr) {
	attr, ok := cachedCallerAttr(pc)
	if !ok {
		return
	}
	if h.replaceAttr == nil {
		ps.caller = attr.Value.String()
		ps.hasCaller = ps.caller != ""
		return
	}
	if h.fastCachedSrc {
		attr = h.replaceAttr(nil, attr)
		if attr.Equal(slog.Attr{}) {
			return
		}
		ps.caller = attr.Value.String()
		ps.hasCaller = ps.caller != ""
		return
	}
	// Standalone PrettyHandler with custom ReplaceAttr expecting *slog.Source.
	src := callerSourceFromString(attr.Value.String())
	if src == nil {
		return
	}
	ra := h.replaceAttr(nil, slog.Any(slog.SourceKey, src))
	if ra.Equal(slog.Attr{}) {
		return
	}
	if s, ok := ra.Value.Any().(*slog.Source); ok {
		if s != nil && s.File != "" {
			ps.caller = formatShortCaller(s.File, s.Line)
			ps.hasCaller = true
		}
		return
	}
	ps.caller = ra.Value.String()
	ps.hasCaller = ps.caller != ""
}

func callerSourceFromString(s string) *slog.Source {
	if s == "" {
		return nil
	}
	idx := strings.LastIndexByte(s, ':')
	if idx <= 0 {
		return &slog.Source{File: s}
	}
	line, _ := strconv.Atoi(s[idx+1:])
	return &slog.Source{File: s[:idx], Line: line}
}

func (h *PrettyHandler) appendHeaderTime(ps *prettyState, t time.Time) bool {
	if h.replaceAttr != nil {
		a := h.replaceAttr(nil, slog.Time(slog.TimeKey, t))
		if a.Equal(slog.Attr{}) {
			return false
		}
		if h.colorize {
			ps.buf = append(ps.buf, ansiDim...)
		}
		if a.Value.Kind() == slog.KindTime {
			ps.buf = a.Value.Time().AppendFormat(ps.buf, h.timeFormat)
		} else {
			ps.buf = appendInlineValue(ps.buf, a.Value, false)
		}
		if h.colorize {
			ps.buf = append(ps.buf, ansiReset...)
		}
		return true
	}
	if h.colorize {
		ps.buf = append(ps.buf, ansiDim...)
	}
	ps.buf = t.AppendFormat(ps.buf, h.timeFormat)
	if h.colorize {
		ps.buf = append(ps.buf, ansiReset...)
	}
	return true
}

func (h *PrettyHandler) appendHeaderLevel(ps *prettyState, lvl slog.Level, leadingSpace bool) bool {
	label := paddedLevelLabel(lvl)
	if h.replaceAttr != nil {
		a := h.replaceAttr(nil, slog.Any(slog.LevelKey, lvl))
		if a.Equal(slog.Attr{}) {
			return false
		}
		if a.Value.Kind() == slog.KindString {
			s := a.Value.String()
			if len(s) < 5 {
				label = s + strings.Repeat(" ", 5-len(s))
			} else {
				label = s
			}
		} else if l, ok := a.Value.Any().(slog.Level); ok {
			label = paddedLevelLabel(l)
		} else {
			label = a.Value.String()
		}
	}
	if leadingSpace {
		ps.buf = append(ps.buf, ' ')
	}
	if h.colorize {
		ps.buf = append(ps.buf, levelANSIColor(lvl)...)
		ps.buf = append(ps.buf, label...)
		ps.buf = append(ps.buf, ansiReset...)
	} else {
		ps.buf = append(ps.buf, label...)
	}
	return true
}

func paddedLevelLabel(lvl slog.Level) string {
	switch lvl {
	case slog.LevelDebug:
		return "DEBUG"
	case slog.LevelInfo:
		return "INFO "
	case slog.LevelWarn:
		return "WARN "
	case slog.LevelError:
		return "ERROR"
	default:
		s := lvl.String()
		if len(s) < 5 {
			return s + strings.Repeat(" ", 5-len(s))
		}
		return s
	}
}

func levelANSIColor(lvl slog.Level) string {
	switch {
	case lvl >= slog.LevelError:
		return ansiBoldRed
	case lvl >= slog.LevelWarn:
		return ansiYellow
	case lvl >= slog.LevelInfo:
		return ansiGreen
	default:
		return ansiCyan
	}
}

func (h *PrettyHandler) appendHeaderMessage(ps *prettyState, msg string, leadingSpace bool) bool {
	if h.replaceAttr != nil {
		a := h.replaceAttr(nil, slog.String(slog.MessageKey, msg))
		if a.Equal(slog.Attr{}) {
			return false
		}
		msg = a.Value.String()
	}
	if msg == "" {
		return false
	}
	if leadingSpace {
		ps.buf = append(ps.buf, ' ')
	}
	ps.buf = append(ps.buf, msg...)
	return true
}

func (h *PrettyHandler) appendAttr(ps *prettyState, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return
		}
		if a.Key != "" {
			ps.groups = append(ps.groups, a.Key)
			defer func() {
				ps.groups = ps.groups[:len(ps.groups)-1]
			}()
		}
		for _, sub := range attrs {
			h.appendAttr(ps, sub)
		}
		return
	}

	if h.replaceAttr != nil {
		a = h.replaceAttr(ps.groups, a)
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			for _, sub := range a.Value.Group() {
				h.appendAttr(ps, sub)
			}
			return
		}
	}
	if a.Equal(slog.Attr{}) || a.Key == "" {
		return
	}

	// Hoist PC-cached caller source ("pkg/file.go:42") into header when at top-level.
	if len(ps.groups) == 0 && !ps.hasCaller && a.Key == slog.SourceKey && a.Value.Kind() == slog.KindString {
		if s := a.Value.String(); looksLikeCaller(s) {
			ps.caller = s
			ps.hasCaller = true
			return
		}
	}

	if str, isMulti := extractMultiline(a.Value); isMulti {
		ps.multiline = append(ps.multiline, multilineAttr{
			key: joinGroupKey(ps.groups, a.Key),
			val: str,
		})
		return
	}

	ps.attrBuf = append(ps.attrBuf, ' ')
	if h.colorize {
		ps.attrBuf = append(ps.attrBuf, ansiCyan...)
	}
	for _, g := range ps.groups {
		ps.attrBuf = append(ps.attrBuf, g...)
		ps.attrBuf = append(ps.attrBuf, '.')
	}
	ps.attrBuf = append(ps.attrBuf, a.Key...)
	if h.colorize {
		ps.attrBuf = append(ps.attrBuf, ansiReset...)
	}
	ps.attrBuf = append(ps.attrBuf, '=')
	ps.attrBuf = appendInlineValue(ps.attrBuf, a.Value, true)
}

func joinGroupKey(groups []string, key string) string {
	if len(groups) == 0 {
		return key
	}
	return strings.Join(groups, ".") + "." + key
}

func looksLikeCaller(s string) bool {
	idx := strings.LastIndexByte(s, ':')
	if idx <= 3 || !strings.HasSuffix(s[:idx], ".go") {
		return false
	}
	for i := idx + 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return idx+1 < len(s)
}

func extractMultiline(v slog.Value) (string, bool) {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		return s, strings.IndexByte(s, '\n') >= 0
	case slog.KindAny:
		switch x := v.Any().(type) {
		case nil:
			return "", false
		case error:
			s := x.Error()
			return s, strings.IndexByte(s, '\n') >= 0
		case fmt.Stringer:
			s := x.String()
			return s, strings.IndexByte(s, '\n') >= 0
		}
	}
	return "", false
}

func appendInlineValue(dst []byte, v slog.Value, quoteIfNeeded bool) []byte {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if quoteIfNeeded && needsQuoting(s) {
			return strconv.AppendQuote(dst, s)
		}
		return append(dst, s...)
	case slog.KindInt64:
		return strconv.AppendInt(dst, v.Int64(), 10)
	case slog.KindUint64:
		return strconv.AppendUint(dst, v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.AppendFloat(dst, v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.AppendBool(dst, v.Bool())
	case slog.KindDuration:
		return append(dst, v.Duration().String()...)
	case slog.KindTime:
		return v.Time().AppendFormat(dst, time.RFC3339)
	default:
		anyVal := v.Any()
		if anyVal == nil {
			return append(dst, "<nil>"...)
		}
		var s string
		switch x := anyVal.(type) {
		case error:
			s = x.Error()
		case fmt.Stringer:
			s = x.String()
		case []byte:
			s = string(x)
		default:
			s = fmt.Sprint(anyVal)
		}
		if quoteIfNeeded && needsQuoting(s) {
			return strconv.AppendQuote(dst, s)
		}
		return append(dst, s...)
	}
}

func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c == '=' || c == '"' || c == '\\' || c == 0x7f {
			return true
		}
	}
	return false
}
