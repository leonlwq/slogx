package slogx

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// SampledDroppedKey is the attribute key attached when prior duplicate records
// were suppressed by SamplingHandler.
const SampledDroppedKey = "sampled_dropped"

const samplerBuckets = 4096 // must be power of two

// SamplingOptions configures log storm protection.
// Within each Tick window, the first First logs with the same (Level, Message)
// are logged in full; subsequent logs in the same window are logged once every
// Thereafter occurrences (or dropped completely if Thereafter == 0).
type SamplingOptions struct {
	Tick       time.Duration
	First      int
	Thereafter int
}

type sampleBucket struct {
	resetAt atomic.Int64
	counter atomic.Uint64
	dropped atomic.Uint64
}

type samplerState struct {
	buckets      [samplerBuckets]sampleBucket
	totalDropped atomic.Uint64
}

// SamplingHandler wraps an slog.Handler with lock-free rate-limiting per
// (Level, Message) pair to protect disk I/O during downstream failure storms.
type SamplingHandler struct {
	next       slog.Handler
	state      *samplerState
	tickNano   int64
	first      uint64
	thereafter uint64
}

// NewSamplingHandler wraps next with the specified sampling rules.
func NewSamplingHandler(next slog.Handler, opts SamplingOptions) *SamplingHandler {
	tick := opts.Tick
	if tick <= 0 {
		tick = time.Second
	}
	first := opts.First
	if first <= 0 {
		first = 100
	}
	thereafter := opts.Thereafter
	if thereafter < 0 {
		thereafter = 0
	}
	return &SamplingHandler{
		next:       next,
		state:      &samplerState{},
		tickNano:   tick.Nanoseconds(),
		first:      uint64(first),
		thereafter: uint64(thereafter),
	}
}

// Enabled delegates level checking to the wrapped handler.
func (h *SamplingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle applies lock-free bucketed sampling before delegating to next.
func (h *SamplingHandler) Handle(ctx context.Context, r slog.Record) error {
	idx := hashLevelAndMsg(r.Level, r.Message) & (samplerBuckets - 1)
	b := &h.state.buckets[idx]

	var now int64
	if !r.Time.IsZero() {
		now = r.Time.UnixNano()
	} else {
		now = time.Now().UnixNano()
	}

	reset := b.resetAt.Load()
	if now >= reset {
		if b.resetAt.CompareAndSwap(reset, now+h.tickNano) {
			b.counter.Store(1)
			if prev := b.dropped.Swap(0); prev > 0 {
				r = r.Clone()
				r.AddAttrs(slog.Uint64(SampledDroppedKey, prev))
			}
			return h.next.Handle(ctx, r)
		}
	}

	n := b.counter.Add(1)
	if n <= h.first {
		return h.next.Handle(ctx, r)
	}
	if h.thereafter > 0 && (n-h.first)%h.thereafter == 0 {
		if prev := b.dropped.Swap(0); prev > 0 {
			r = r.Clone()
			r.AddAttrs(slog.Uint64(SampledDroppedKey, prev))
		}
		return h.next.Handle(ctx, r)
	}

	b.dropped.Add(1)
	h.state.totalDropped.Add(1)
	return nil
}

// WithAttrs returns a derived SamplingHandler sharing the same rate-limit buckets.
func (h *SamplingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &SamplingHandler{
		next:       h.next.WithAttrs(attrs),
		state:      h.state,
		tickNano:   h.tickNano,
		first:      h.first,
		thereafter: h.thereafter,
	}
}

// WithGroup returns a derived SamplingHandler sharing the same rate-limit buckets.
func (h *SamplingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &SamplingHandler{
		next:       h.next.WithGroup(name),
		state:      h.state,
		tickNano:   h.tickNano,
		first:      h.first,
		thereafter: h.thereafter,
	}
}

// Dropped returns the total number of log records suppressed by this sampler.
func (h *SamplingHandler) Dropped() uint64 {
	return h.state.totalDropped.Load()
}

// hashLevelAndMsg computes an inline 64-bit FNV-1a hash with zero allocations.
func hashLevelAndMsg(level slog.Level, msg string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	lv := uint64(int64(level))
	h ^= lv & 0xff
	h *= prime64
	h ^= (lv >> 8) & 0xff
	h *= prime64

	for i := 0; i < len(msg); i++ {
		h ^= uint64(msg[i])
		h *= prime64
	}
	return h
}
