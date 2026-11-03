package slogx

import (
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultBufferSize    = 64 * 1024
	defaultFlushInterval = 200 * time.Millisecond
	defaultAsyncQueue    = 4096
	maxPooledBufCap      = 16 * 1024
	initialPooledBufCap  = 1024
)

// ErrWriterClosed is returned when writing to or rotating a closed writer or Logger.
var ErrWriterClosed = errors.New("slogx: writer is closed")

// OverflowPolicy defines how AsyncWriter behaves when its queue is full.
type OverflowPolicy int

const (
	// OverflowDrop drops incoming log records when the async queue is full so
	// business goroutines never block on disk stalls, recording a dropped count.
	OverflowDrop OverflowPolicy = iota
	// OverflowBlock blocks the caller until space becomes available in the queue
	// or the writer is closed.
	OverflowBlock
)

type closeGuardWriter struct {
	dst    io.Writer
	closed *atomic.Bool
}

func (g *closeGuardWriter) Write(p []byte) (int, error) {
	if g.closed.Load() {
		return 0, ErrWriterClosed
	}
	return g.dst.Write(p)
}

func syncUnderlyingDst(dst io.Writer) error {
	if f, ok := dst.(*os.File); ok {
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		if err := f.Sync(); err != nil && !isIgnorableSyncErr(err) {
			return err
		}
		return nil
	}
	s, ok := dst.(interface{ Sync() error })
	if !ok {
		return nil
	}
	if err := s.Sync(); err != nil && !isIgnorableSyncErr(err) {
		return err
	}
	return nil
}

// BufferedWriter wraps an io.Writer with a non-sticky in-memory byte buffer and
// optional periodic background flushing to minimize write(2) syscalls while
// automatically recovering from transient or oversized-write errors.
type BufferedWriter struct {
	mu     sync.Mutex
	dst    io.Writer
	buf    []byte
	maxCap int
	stopCh chan struct{}
	doneCh chan struct{}
	closed atomic.Bool
}

// NewBufferedWriter creates a thread-safe buffered writer around dst.
// If bufSize <= 0, 64KB is used. If flushInterval > 0, a background ticker
// periodically flushes buffered data.
func NewBufferedWriter(dst io.Writer, bufSize int, flushInterval time.Duration) *BufferedWriter {
	if bufSize <= 0 {
		bufSize = defaultBufferSize
	}
	w := &BufferedWriter{
		dst:    dst,
		buf:    make([]byte, 0, bufSize),
		maxCap: bufSize,
	}
	if flushInterval > 0 {
		w.stopCh = make(chan struct{})
		w.doneCh = make(chan struct{})
		go w.flushLoop(flushInterval)
	}
	return w
}

func (w *BufferedWriter) flushLocked() error {
	if len(w.buf) == 0 {
		return nil
	}
	n, err := w.dst.Write(w.buf)
	if n >= len(w.buf) {
		w.buf = w.buf[:0]
		return err
	}
	if n > 0 {
		copy(w.buf, w.buf[n:])
		w.buf = w.buf[:len(w.buf)-n]
	}
	if err == nil {
		err = io.ErrShortWrite
	}
	return err
}

func (w *BufferedWriter) flushLoop(interval time.Duration) {
	defer close(w.doneCh)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.mu.Lock()
			if !w.closed.Load() && len(w.buf) > 0 {
				_ = w.flushLocked()
			}
			w.mu.Unlock()
		}
	}
}

// Write writes p into the underlying buffer, flushing to dst when full.
func (w *BufferedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Load() {
		return 0, ErrWriterClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(w.buf)+len(p) <= w.maxCap {
		w.buf = append(w.buf, p...)
		return len(p), nil
	}
	if err := w.flushLocked(); err != nil {
		// Discard wedged buffer so sustained disk errors never lock up future writes.
		w.buf = w.buf[:0]
		return 0, err
	}
	if len(p) >= w.maxCap {
		return w.dst.Write(p)
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// Flush writes any buffered user-space bytes to the underlying writer without
// forcing a hardware fsync.
func (w *BufferedWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Load() {
		return nil
	}
	err := w.flushLocked()
	if err != nil {
		w.buf = w.buf[:0]
	}
	return err
}

// Sync flushes the in-memory buffer and syncs the underlying writer if supported.
func (w *BufferedWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Load() {
		return nil
	}
	err := w.flushLocked()
	if err != nil {
		w.buf = w.buf[:0]
	}
	return errors.Join(err, syncUnderlyingDst(w.dst))
}

// Close stops periodic flushing, flushes all remaining buffered bytes, syncs,
// and closes the underlying writer if it implements io.Closer.
func (w *BufferedWriter) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	if w.stopCh != nil {
		close(w.stopCh)
		<-w.doneCh
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	err := w.flushLocked()
	w.buf = w.buf[:0]
	err = errors.Join(err, syncUnderlyingDst(w.dst))
	if c, ok := w.dst.(io.Closer); ok {
		err = errors.Join(err, c.Close())
	}
	return err
}

var asyncBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, initialPooledBufCap)
		return &b
	},
}

func getAsyncBuf(size int) *[]byte {
	bp := asyncBufPool.Get().(*[]byte)
	if cap(*bp) < size {
		b := make([]byte, 0, size)
		return &b
	}
	*bp = (*bp)[:0]
	return bp
}

func putAsyncBuf(bp *[]byte) {
	if bp == nil || cap(*bp) > maxPooledBufCap {
		return
	}
	*bp = (*bp)[:0]
	asyncBufPool.Put(bp)
}

// AsyncWriter decouples log formatting from disk I/O and file rotation using a
// single background consumer goroutine with automatic micro-batching.
type AsyncWriter struct {
	dst        io.Writer
	batchBuf   []byte
	maxBuf     int
	dataCh     chan *[]byte
	syncCh     chan chan error
	stopCh     chan struct{}
	doneCh     chan struct{}
	policy     OverflowPolicy
	isJSON     bool
	closed     atomic.Bool
	dropped    atomic.Uint64
	totalDrops atomic.Uint64
}

// NewAsyncWriter creates an asynchronous writer with a single consumer goroutine.
func NewAsyncWriter(dst io.Writer, queueSize, bufSize int, policy OverflowPolicy, isJSON bool) *AsyncWriter {
	if queueSize <= 0 {
		queueSize = defaultAsyncQueue
	}
	if bufSize <= 0 {
		bufSize = defaultBufferSize
	}
	w := &AsyncWriter{
		dst:      dst,
		batchBuf: make([]byte, 0, bufSize),
		maxBuf:   bufSize,
		dataCh:   make(chan *[]byte, queueSize),
		syncCh:   make(chan chan error),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		policy:   policy,
		isJSON:   isJSON,
	}
	go w.consumeLoop()
	return w
}

// Write copies p into a pooled buffer and enqueues it without locking.
func (w *AsyncWriter) Write(p []byte) (int, error) {
	if w.closed.Load() {
		return 0, ErrWriterClosed
	}

	bp := getAsyncBuf(len(p))
	*bp = append(*bp, p...)

	select {
	case <-w.stopCh:
		putAsyncBuf(bp)
		return 0, ErrWriterClosed
	default:
	}

	if w.policy == OverflowBlock {
		select {
		case w.dataCh <- bp:
			return len(p), nil
		case <-w.stopCh:
			putAsyncBuf(bp)
			return 0, ErrWriterClosed
		}
	}

	select {
	case w.dataCh <- bp:
		return len(p), nil
	case <-w.stopCh:
		putAsyncBuf(bp)
		return 0, ErrWriterClosed
	default:
		putAsyncBuf(bp)
		w.dropped.Add(1)
		w.totalDrops.Add(1)
		return len(p), nil
	}
}

func (w *AsyncWriter) appendBatch(p []byte) {
	if len(p) == 0 {
		return
	}
	if len(w.batchBuf)+len(p) <= w.maxBuf {
		w.batchBuf = append(w.batchBuf, p...)
		return
	}
	_ = w.flushBatch()
	if len(p) >= w.maxBuf {
		_, _ = w.dst.Write(p)
		return
	}
	w.batchBuf = append(w.batchBuf, p...)
}

func (w *AsyncWriter) flushBatch() error {
	if len(w.batchBuf) == 0 {
		return nil
	}
	_, err := w.dst.Write(w.batchBuf)
	w.batchBuf = w.batchBuf[:0]
	return err
}

func (w *AsyncWriter) writeBuf(bp *[]byte) {
	w.emitDropNoticeIfAny()
	w.appendBatch(*bp)
	putAsyncBuf(bp)
}

func (w *AsyncWriter) emitDropNoticeIfAny() {
	n := w.dropped.Swap(0)
	if n == 0 {
		return
	}
	ts := time.Now().Format(time.RFC3339Nano)
	var stack [192]byte
	line := stack[:0]
	if w.isJSON {
		line = append(line, `{"time":"`...)
		line = append(line, ts...)
		line = append(line, `","level":"WARN","msg":"slogx: async queue overflow, dropped log records","dropped":`...)
		line = strconv.AppendUint(line, n, 10)
		line = append(line, "}\n"...)
	} else {
		line = append(line, "time="...)
		line = append(line, ts...)
		line = append(line, ` level=WARN msg="slogx: async queue overflow, dropped log records" dropped=`...)
		line = strconv.AppendUint(line, n, 10)
		line = append(line, '\n')
	}
	w.appendBatch(line)
}

func (w *AsyncWriter) drainPending() {
	for {
		n := len(w.dataCh)
		if n == 0 {
			break
		}
		for i := 0; i < n; i++ {
			select {
			case bp := <-w.dataCh:
				w.writeBuf(bp)
			default:
				return
			}
		}
	}
}

func (w *AsyncWriter) syncUnderlying() error {
	w.emitDropNoticeIfAny()
	err := w.flushBatch()
	return errors.Join(err, syncUnderlyingDst(w.dst))
}

func (w *AsyncWriter) consumeLoop() {
	defer close(w.doneCh)

	for {
		select {
		case bp := <-w.dataCh:
			w.writeBuf(bp)
			// Micro-batch drain: process all currently queued items before flushing.
			n := len(w.dataCh)
			for i := 0; i < n; i++ {
				w.writeBuf(<-w.dataCh)
			}
			_ = w.flushBatch()

		case reply := <-w.syncCh:
			w.drainPending()
			reply <- w.syncUnderlying()

		case <-w.stopCh:
			w.drainPending()
			_ = w.syncUnderlying()
			return
		}
	}
}

// Dropped returns the cumulative count of log records dropped due to queue overflow.
func (w *AsyncWriter) Dropped() uint64 {
	return w.totalDrops.Load()
}

// Sync drains all currently queued records and flushes them to the underlying writer.
func (w *AsyncWriter) Sync() error {
	if w.closed.Load() {
		return nil
	}
	reply := make(chan error, 1)
	select {
	case w.syncCh <- reply:
		select {
		case err := <-reply:
			return err
		case <-w.doneCh:
			return nil
		}
	case <-w.doneCh:
		return nil
	}
}

// Close stops accepting new writes, drains all queued records, flushes buffers,
// and closes the underlying writer if it implements io.Closer.
func (w *AsyncWriter) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(w.stopCh)
	<-w.doneCh

	if c, ok := w.dst.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
