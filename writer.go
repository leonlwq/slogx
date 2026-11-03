package slogx

import (
	"errors"
	"fmt"
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

// ErrAsyncOverflow is the sentinel error matched via errors.Is when AsyncWriter
// drops log records due to queue overflow under OverflowDrop.
var ErrAsyncOverflow = errors.New("slogx: async queue overflow")

// AsyncOverflowError reports a batch of log records dropped by AsyncWriter.
type AsyncOverflowError struct {
	Dropped uint64
}

func (e *AsyncOverflowError) Error() string {
	return fmt.Sprintf("slogx: async queue overflow, dropped %d log records", e.Dropped)
}

func (e *AsyncOverflowError) Unwrap() error {
	return ErrAsyncOverflow
}

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
	mu      sync.Mutex
	dst     io.Writer
	buf     []byte
	maxCap  int
	stopCh  chan struct{}
	doneCh  chan struct{}
	onError ErrorHandler
	closed  atomic.Bool
}

// NewBufferedWriter creates a thread-safe buffered writer around dst.
// If bufSize <= 0, 64KB is used. If flushInterval > 0, a background ticker
// periodically flushes buffered data.
func NewBufferedWriter(dst io.Writer, bufSize int, flushInterval time.Duration) *BufferedWriter {
	return newBufferedWriter(dst, bufSize, flushInterval, nil)
}

func newBufferedWriter(dst io.Writer, bufSize int, flushInterval time.Duration, onError ErrorHandler) *BufferedWriter {
	if bufSize <= 0 {
		bufSize = defaultBufferSize
	}
	w := &BufferedWriter{
		dst:     dst,
		buf:     make([]byte, 0, bufSize),
		maxCap:  bufSize,
		onError: onError,
	}
	if flushInterval > 0 {
		w.stopCh = make(chan struct{})
		w.doneCh = make(chan struct{})
		go w.flushLoop(flushInterval)
	}
	return w
}

func (w *BufferedWriter) flushLocked() error {
	prevLen := len(w.buf)
	if prevLen == 0 {
		return nil
	}
	n, err := w.dst.Write(w.buf)
	// Always reset the buffer so a transient or sustained destination error
	// never wedges future log writes in memory.
	w.buf = w.buf[:0]
	if err == nil && n < prevLen {
		return io.ErrShortWrite
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
			var flushErr error
			if !w.closed.Load() && len(w.buf) > 0 {
				flushErr = w.flushLocked()
			}
			w.mu.Unlock()
			if flushErr != nil && w.onError != nil {
				w.onError(flushErr)
			}
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
	return w.flushLocked()
}

// Sync flushes the in-memory buffer and syncs the underlying writer if supported.
func (w *BufferedWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Load() {
		return nil
	}
	return errors.Join(w.flushLocked(), syncUnderlyingDst(w.dst))
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

	err := errors.Join(w.flushLocked(), syncUnderlyingDst(w.dst))
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
	onError    ErrorHandler
	closed     atomic.Bool
	dropped    atomic.Uint64
	totalDrops atomic.Uint64
}

// NewAsyncWriter creates an asynchronous writer with a single consumer goroutine.
func NewAsyncWriter(dst io.Writer, queueSize, bufSize int, policy OverflowPolicy, isJSON bool) *AsyncWriter {
	return newAsyncWriter(dst, queueSize, bufSize, policy, isJSON, nil)
}

func newAsyncWriter(dst io.Writer, queueSize, bufSize int, policy OverflowPolicy, isJSON bool, onError ErrorHandler) *AsyncWriter {
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
		onError:  onError,
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

	// Fast path: 2-way select with default compiles to runtime.selectnbsend,
	// avoiding multi-channel lock sorting and random permutation in selectgo.
	select {
	case w.dataCh <- bp:
		return len(p), nil
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

	if w.closed.Load() {
		putAsyncBuf(bp)
		return 0, ErrWriterClosed
	}
	putAsyncBuf(bp)
	w.dropped.Add(1)
	w.totalDrops.Add(1)
	return len(p), nil
}

func (w *AsyncWriter) reportErr(err error) {
	if err != nil && w.onError != nil {
		w.onError(err)
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
	w.reportErr(w.flushBatch())
	if len(p) >= w.maxBuf {
		_, err := w.dst.Write(p)
		w.reportErr(err)
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
	w.reportErr(&AsyncOverflowError{Dropped: n})
	now := time.Now()
	var stack [192]byte
	line := stack[:0]
	if w.isJSON {
		line = append(line, `{"time":"`...)
		line = now.AppendFormat(line, time.RFC3339Nano)
		line = append(line, `","level":"WARN","msg":"slogx: async queue overflow, dropped log records","dropped":`...)
		line = strconv.AppendUint(line, n, 10)
		line = append(line, "}\n"...)
	} else {
		line = append(line, "time="...)
		line = now.AppendFormat(line, time.RFC3339Nano)
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
			w.reportErr(w.flushBatch())

		case reply := <-w.syncCh:
			w.drainPending()
			err := w.syncUnderlying()
			w.reportErr(err)
			reply <- err

		case <-w.stopCh:
			w.drainPending()
			w.reportErr(w.syncUnderlying())
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

	if len(w.dataCh) > 0 {
		w.drainPending()
		w.reportErr(w.syncUnderlying())
	}

	if c, ok := w.dst.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
