package slogx

import (
	"log/slog"
	"strings"
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

func normalizeSensitiveKeySet(keys []string) map[string]struct{} {
	if len(keys) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(strings.ToLower(k))
		if k != "" {
			set[k] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// matchSensitiveKey performs case-insensitive lookup without heap allocation
// for keys up to 64 bytes.
func matchSensitiveKey(key string, set map[string]struct{}) bool {
	if key == "" || len(set) == 0 {
		return false
	}
	if _, ok := set[key]; ok {
		return true
	}
	hasUpper := false
	for i := 0; i < len(key); i++ {
		if key[i] >= 'A' && key[i] <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return false
	}
	if len(key) <= 64 {
		var buf [64]byte
		for i := 0; i < len(key); i++ {
			c := key[i]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			buf[i] = c
		}
		_, ok := set[string(buf[:len(key)])]
		return ok
	}
	_, ok := set[strings.ToLower(key)]
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

// buildReplaceAttr composes optional attribute rewriters. When no rewriter is
// configured, it returns nil so Go's stdlib slog uses its zero-boxing fast path.
func buildReplaceAttr(
	timeFormat string,
	sensitiveSet map[string]struct{},
	maxFieldLen int,
	userReplace func([]string, slog.Attr) slog.Attr,
	includeNativeSource bool,
) func([]string, slog.Attr) slog.Attr {
	if timeFormat == "" && len(sensitiveSet) == 0 && maxFieldLen <= 0 && userReplace == nil && !includeNativeSource {
		return nil
	}

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
				a = slog.String(slog.TimeKey, t.Format(timeFormat))
			}
			if userReplace != nil {
				return userReplace(groups, a)
			}
			return a
		}

		isBuiltinMeta := isTopLevel && (a.Key == slog.TimeKey || a.Key == slog.LevelKey)
		isMessageMeta := isTopLevel && a.Key == slog.MessageKey
		if !isBuiltinMeta && !isMessageMeta && len(sensitiveSet) > 0 && matchSensitiveKey(a.Key, sensitiveSet) {
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
