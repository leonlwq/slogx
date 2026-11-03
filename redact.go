package slogx

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// Common time format constants for WithTimeFormat.
const (
	TimeFormatRFC3339Millis = "2006-01-02T15:04:05.000Z07:00"
	TimeFormatDateTimeMilli = "2006-01-02 15:04:05.000"
	TimeFormatUnixMillis    = "unix_ms"
	TimeFormatUnixNano      = "unix_ns"

	redactedPlaceholder = "***"
	truncatedSuffix     = "...[truncated]"
)

// DefaultSensitiveKeys provides a curated list of common credential/secret keys.
var DefaultSensitiveKeys = []string{
	"password",
	"passwd",
	"pwd",
	"secret",
	"token",
	"access_token",
	"refresh_token",
	"api_key",
	"apikey",
	"authorization",
	"private_key",
}

type secretValue string

func (s secretValue) LogValue() slog.Value {
	if s == "" {
		return slog.StringValue("")
	}
	return slog.StringValue(redactedPlaceholder)
}

func (s secretValue) String() string {
	if s == "" {
		return ""
	}
	return redactedPlaceholder
}

// Secret wraps a sensitive string so it is always logged or formatted as "***"
// (or "" if empty) without retaining the underlying plaintext in memory.
func Secret(val string) slog.LogValuer {
	if val == "" {
		return secretValue("")
	}
	return secretValue(redactedPlaceholder)
}

// SecretAttr constructs a zero-allocation slog.Attr whose value is masked as
// "***" (or "" if empty).
func SecretAttr(key, val string) slog.Attr {
	if val == "" {
		return slog.String(key, "")
	}
	return slog.String(key, redactedPlaceholder)
}

type maskedString struct {
	raw        string
	keepPrefix int
	keepSuffix int
}

func (m maskedString) LogValue() slog.Value {
	return slog.StringValue(applyMask(m.raw, m.keepPrefix, m.keepSuffix))
}

func (m maskedString) String() string {
	return applyMask(m.raw, m.keepPrefix, m.keepSuffix)
}

func applyMask(s string, keepPrefix, keepSuffix int) string {
	if s == "" {
		return ""
	}
	if keepPrefix < 0 {
		keepPrefix = 0
	}
	if keepSuffix < 0 {
		keepSuffix = 0
	}
	runes := []rune(s)
	if keepPrefix+keepSuffix >= len(runes) {
		return redactedPlaceholder
	}
	var b strings.Builder
	b.Grow(keepPrefix + 4 + keepSuffix)
	for i := 0; i < keepPrefix; i++ {
		b.WriteRune(runes[i])
	}
	b.WriteString("****")
	for i := len(runes) - keepSuffix; i < len(runes); i++ {
		b.WriteRune(runes[i])
	}
	return b.String()
}

// MaskString lazily masks the middle of val, preserving keepPrefix leading runes
// and keepSuffix trailing runes (or "***" if val is too short).
func MaskString(val string, keepPrefix, keepSuffix int) slog.LogValuer {
	return maskedString{raw: val, keepPrefix: keepPrefix, keepSuffix: keepSuffix}
}

// MaskPhone lazily masks a phone number (e.g. "13812345678" -> "138****5678").
func MaskPhone(phone string) slog.LogValuer {
	return maskedString{raw: phone, keepPrefix: 3, keepSuffix: 4}
}

type maskedEmail string

func (e maskedEmail) mask() string {
	s := string(e)
	if s == "" {
		return ""
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 {
		return redactedPlaceholder
	}
	local := []rune(s[:at])
	domain := s[at:]
	if len(local) <= 1 {
		return "*" + domain
	}
	return string(local[0]) + "****" + domain
}

func (e maskedEmail) LogValue() slog.Value {
	return slog.StringValue(e.mask())
}

func (e maskedEmail) String() string {
	return e.mask()
}

// MaskEmail lazily masks an email address (e.g. "alice@example.com" -> "a****@example.com").
func MaskEmail(email string) slog.LogValuer {
	return maskedEmail(email)
}

type sensitiveMatcher struct {
	set    map[string]struct{}
	minLen int
	maxLen int
}

func newSensitiveMatcher(keys []string) sensitiveMatcher {
	if len(keys) == 0 {
		return sensitiveMatcher{}
	}
	set := make(map[string]struct{}, len(keys))
	minLen := int(^uint(0) >> 1)
	maxLen := 0
	for _, k := range keys {
		k = strings.TrimSpace(strings.ToLower(k))
		if k == "" {
			continue
		}
		set[k] = struct{}{}
		if len(k) < minLen {
			minLen = len(k)
		}
		if len(k) > maxLen {
			maxLen = len(k)
		}
	}
	if len(set) == 0 {
		return sensitiveMatcher{}
	}
	return sensitiveMatcher{set: set, minLen: minLen, maxLen: maxLen}
}

// match performs length-prefiltered, case-insensitive lookup without heap
// allocation for keys up to 64 bytes.
func (m sensitiveMatcher) match(key string) bool {
	n := len(key)
	if n == 0 || len(m.set) == 0 || n < m.minLen || n > m.maxLen {
		return false
	}
	if _, ok := m.set[key]; ok {
		return true
	}
	hasUpper := false
	for i := 0; i < n; i++ {
		if key[i] >= 'A' && key[i] <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return false
	}
	if n <= 64 {
		var buf [64]byte
		for i := 0; i < n; i++ {
			c := key[i]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			buf[i] = c
		}
		_, ok := m.set[string(buf[:n])]
		return ok
	}
	_, ok := m.set[strings.ToLower(key)]
	return ok
}

// truncateUTF8 truncates s to at most maxBytes on a valid UTF-8 rune boundary
// and appends "...[truncated]".
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedSuffix
}

type cachedTimeEntry struct {
	unixMilli int64
	offset    int
	zone      string
	attr      slog.Attr
}

// isMillisecondCacheableLayout reports whether layout has at most millisecond
// fractional precision (<= 3 digits after '.' or ','), making its output a pure
// function of (t.UnixMilli(), t.Zone()).
func isMillisecondCacheableLayout(layout string) bool {
	if layout == "" || layout == TimeFormatUnixMillis || layout == TimeFormatUnixNano {
		return false
	}
	for i := 0; i < len(layout); i++ {
		if layout[i] == '.' || layout[i] == ',' {
			j := i + 1
			if j < len(layout) && (layout[j] == '0' || layout[j] == '9') {
				ch := layout[j]
				for j < len(layout) && layout[j] == ch {
					j++
				}
				if j-(i+1) > 3 {
					return false
				}
			}
		}
	}
	return true
}

// buildReplaceAttr composes optional attribute rewriters. When no rewriter is
// configured, it returns nil so Go's stdlib slog uses its zero-boxing fast path.
func buildReplaceAttr(
	timeFormat string,
	sensitive sensitiveMatcher,
	maxFieldLen int,
	userReplace func([]string, slog.Attr) slog.Attr,
	includeNativeSource bool,
) func([]string, slog.Attr) slog.Attr {
	hasSensitive := len(sensitive.set) > 0
	if timeFormat == "" && !hasSensitive && maxFieldLen <= 0 && userReplace == nil && !includeNativeSource {
		return nil
	}

	cacheMillis := isMillisecondCacheableLayout(timeFormat)
	var timeCache atomic.Pointer[cachedTimeEntry]

	return func(groups []string, a slog.Attr) slog.Attr {
		isTopLevel := len(groups) == 0

		if includeNativeSource && isTopLevel && a.Key == slog.SourceKey {
			if src, ok := a.Value.Any().(*slog.Source); ok {
				if src == nil || src.File == "" {
					return slog.Attr{}
				}
				a = slog.String(slog.SourceKey, formatShortCaller(src.File, src.Line))
				if userReplace != nil {
					return userReplace(groups, a)
				}
				return a
			}
		}

		if timeFormat != "" && isTopLevel && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
			t := a.Value.Time()
			switch timeFormat {
			case TimeFormatUnixMillis:
				a = slog.Int64(slog.TimeKey, t.UnixMilli())
			case TimeFormatUnixNano:
				a = slog.Int64(slog.TimeKey, t.UnixNano())
			default:
				if cacheMillis {
					ms := t.UnixMilli()
					zone, offset := t.Zone()
					if ce := timeCache.Load(); ce != nil && ce.unixMilli == ms && ce.offset == offset && ce.zone == zone {
						a = ce.attr
					} else {
						var buf [64]byte
						a = slog.String(slog.TimeKey, string(t.AppendFormat(buf[:0], timeFormat)))
						timeCache.Store(&cachedTimeEntry{
							unixMilli: ms,
							offset:    offset,
							zone:      zone,
							attr:      a,
						})
					}
				} else {
					var buf [64]byte
					a = slog.String(slog.TimeKey, string(t.AppendFormat(buf[:0], timeFormat)))
				}
			}
			if userReplace != nil {
				return userReplace(groups, a)
			}
			return a
		}

		if isTopLevel && a.Key == slog.LevelKey {
			if userReplace != nil {
				a = userReplace(groups, a)
			}
			if a.Value.Kind() == slog.KindAny {
				if lvl, ok := a.Value.Any().(slog.Level); ok {
					a.Value = slog.StringValue(lvl.String())
				}
			}
			return a
		}

		isBuiltinMeta := isTopLevel && a.Key == slog.TimeKey
		isMessageMeta := isTopLevel && a.Key == slog.MessageKey
		if !isBuiltinMeta && !isMessageMeta && hasSensitive && sensitive.match(a.Key) {
			a.Value = slog.StringValue(redactedPlaceholder)
		} else if !isBuiltinMeta && maxFieldLen > 0 && a.Value.Kind() == slog.KindString {
			if s := a.Value.String(); len(s) > maxFieldLen {
				a.Value = slog.StringValue(truncateUTF8(s, maxFieldLen))
			}
		}

		if userReplace != nil {
			a = userReplace(groups, a)
		}
		return a
	}
}
