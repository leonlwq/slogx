// Package lumberjack provides an internal rolling file writer with size-based
// and time-based log rotation.
package lumberjack

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	backupTimeFormat = "2006-01-02T15-04-05.000"
	compressSuffix   = ".gz"
	defaultMaxSize   = 100

	// RotateHourly rotates the log file at the top of every hour.
	RotateHourly = time.Hour
	// RotateDaily rotates the log file at midnight (local or UTC based on LocalTime).
	RotateDaily = 24 * time.Hour
)

// ensure we always implement io.WriteCloser
var _ io.WriteCloser = (*Logger)(nil)

var gzipWriterPool = sync.Pool{
	New: func() any {
		return gzip.NewWriter(io.Discard)
	},
}

// Logger is an io.WriteCloser that writes to the specified filename.
//
// Logger opens or creates the logfile on first Write. If the file exists and
// is less than MaxSize megabytes (and still within the current RotationInterval),
// lumberjack will open and append to that file. Otherwise, the file is renamed
// by putting the current time in a timestamp in the name immediately before the
// file's extension (or the end of the filename if there's no extension). A new
// log file is then created using original filename.
type Logger struct {
	// Filename is the file to write logs to. Backup log files will be retained
	// in the same directory. It uses <processname>-lumberjack.log in
	// os.TempDir() if empty.
	Filename string `json:"filename" yaml:"filename"`

	// MaxSize is the maximum size in megabytes of the log file before it gets
	// rotated. It defaults to 100 megabytes.
	MaxSize int `json:"maxsize" yaml:"maxsize"`

	// MaxAge is the maximum number of days to retain old log files based on the
	// timestamp encoded in their filename. Note that a day is defined as 24
	// hours and may not exactly correspond to calendar days due to daylight
	// savings, leap seconds, etc. The default is not to remove old log files
	// based on age.
	MaxAge int `json:"maxage" yaml:"maxage"`

	// MaxBackups is the maximum number of old log files to retain. The default
	// is to retain all old log files (though MaxAge may still cause them to get
	// deleted.)
	MaxBackups int `json:"maxbackups" yaml:"maxbackups"`

	// LocalTime determines if the time used for formatting the timestamps in
	// backup files and aligning RotationInterval boundaries is the computer's
	// local time. The default is to use UTC time.
	LocalTime bool `json:"localtime" yaml:"localtime"`

	// Compress determines if the rotated log files should be compressed
	// using gzip. The default is not to perform compression.
	Compress bool `json:"compress" yaml:"compress"`

	// RotationInterval is the optional time interval after which the log file is
	// rotated (e.g., RotateHourly or RotateDaily). Zero disables time-based rotation.
	RotationInterval time.Duration `json:"rotationinterval" yaml:"rotationinterval"`

	// OnError is an optional callback invoked when background post-rotation
	// gzip compression or old backup removal encounters an error.
	OnError func(error) `json:"-" yaml:"-"`

	size         int64
	nextRotateAt time.Time
	file         *os.File
	mu           sync.Mutex

	millCh   chan bool
	millDone chan struct{}
}

var (
	// currentTime exists so it can be mocked out by tests.
	currentTime = time.Now

	// osStat exists so it can be mocked out by tests.
	osStat = os.Stat

	// megabyte is the conversion factor between MaxSize and bytes. It is a
	// variable so tests can mock it out and not need to write megabytes of data
	// to disk.
	megabyte = 1024 * 1024
)

// Write implements io.Writer. If a write would cause the log file to be larger
// than MaxSize or crosses RotationInterval, the file is closed, renamed to include
// a timestamp of the current time, and a new log file is created using the
// original log file name. If the length of the write is greater than MaxSize,
// an error is returned.
func (l *Logger) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	writeLen := int64(len(p))
	if writeLen > l.max() {
		return 0, fmt.Errorf(
			"write length %d exceeds maximum file size %d", writeLen, l.max(),
		)
	}

	if l.file == nil {
		if err = l.openExistingOrNew(len(p)); err != nil {
			return 0, err
		}
	}

	if l.size+writeLen > l.max() || (l.RotationInterval > 0 && !currentTime().Before(l.nextRotateAt)) {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}

	n, err = l.file.Write(p)
	l.size += int64(n)

	return n, err
}

// Sync flushes the active logfile's in-memory copy of recently written data to disk.
func (l *Logger) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	return l.file.Sync()
}

// Close implements io.Closer, closes the current logfile, and waits for any
// pending post-rotation cleanup/compression goroutine to finish.
func (l *Logger) Close() error {
	l.mu.Lock()
	err := l.close()
	ch := l.millCh
	done := l.millDone
	l.millCh = nil
	l.millDone = nil
	l.mu.Unlock()

	if ch != nil {
		close(ch)
		<-done
	}
	return err
}

// close closes the file if it is open.
func (l *Logger) close() error {
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Rotate causes Logger to close the existing log file and immediately create a
// new one. This is a helper function for applications that want to initiate
// rotations outside of the normal rotation rules, such as in response to
// SIGHUP. After rotating, this initiates compression and removal of old log
// files according to the configuration.
func (l *Logger) Rotate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rotate()
}

// rotate closes the current file, moves it aside with a timestamp in the name,
// (if it exists), opens a new file with the original filename, and then runs
// post-rotation processing and removal.
func (l *Logger) rotate() error {
	if err := l.close(); err != nil {
		return err
	}
	if err := l.openNew(); err != nil {
		return err
	}
	l.mill()
	return nil
}

// openNew opens a new log file for writing, moving any old log file out of the
// way. This method assumes the file has already been closed.
func (l *Logger) openNew() error {
	err := os.MkdirAll(l.dir(), 0755)
	if err != nil {
		return fmt.Errorf("can't make directories for new logfile: %s", err)
	}

	name := l.filename()
	mode := os.FileMode(0644)
	info, err := osStat(name)
	if err == nil {
		// Copy the mode off the old logfile.
		mode = info.Mode()
		// move the existing file
		newname := backupName(name, l.LocalTime)
		if err := os.Rename(name, newname); err != nil {
			return fmt.Errorf("can't rename log file: %s", err)
		}

		// this is a no-op anywhere but linux
		if err := chown(name, info); err != nil {
			return err
		}
	}

	// we use truncate here because this should only get called when we've moved
	// the file ourselves. if someone else creates the file in the meantime,
	// just wipe out the contents.
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("can't open new logfile: %s", err)
	}
	l.file = f
	l.size = 0
	if l.RotationInterval > 0 {
		l.nextRotateAt = nextRotateTime(currentTime(), l.RotationInterval, l.LocalTime)
	} else {
		l.nextRotateAt = time.Time{}
	}
	return nil
}

// nextRotateTime calculates the next rotation boundary after t, aligned to
// local or UTC midnight for daily/sub-daily intervals.
func nextRotateTime(t time.Time, interval time.Duration, local bool) time.Time {
	if interval <= 0 {
		return time.Time{}
	}
	if local {
		t = t.Local()
	} else {
		t = t.UTC()
	}
	loc := t.Location()
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	if interval >= 24*time.Hour && interval%(24*time.Hour) == 0 {
		days := int(interval / (24 * time.Hour))
		return dayStart.AddDate(0, 0, days)
	}
	if interval < 24*time.Hour && (24*time.Hour)%interval == 0 {
		elapsed := t.Sub(dayStart)
		slot := (elapsed / interval) + 1
		return dayStart.Add(slot * interval)
	}
	return t.Truncate(interval).Add(interval)
}

// backupName creates a new filename from the given name, inserting a timestamp
// between the filename and the extension, using the local time if requested
// (otherwise UTC).
func backupName(name string, local bool) string {
	dir := filepath.Dir(name)
	filename := filepath.Base(name)
	ext := filepath.Ext(filename)
	prefix := filename[:len(filename)-len(ext)]
	t := currentTime()
	if !local {
		t = t.UTC()
	}

	timestamp := t.Format(backupTimeFormat)
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", prefix, timestamp, ext))
}

// openExistingOrNew opens the logfile if it exists and if the current write
// would not put it over MaxSize or past RotationInterval. If there is no such
// file or rotation is due, a new file is created.
func (l *Logger) openExistingOrNew(writeLen int) error {
	l.mill()

	filename := l.filename()
	info, err := osStat(filename)
	if os.IsNotExist(err) {
		return l.openNew()
	}
	if err != nil {
		return fmt.Errorf("error getting log file info: %s", err)
	}

	if info.Size()+int64(writeLen) >= l.max() {
		return l.rotate()
	}

	var nextAt time.Time
	if l.RotationInterval > 0 {
		nextAt = nextRotateTime(info.ModTime(), l.RotationInterval, l.LocalTime)
		if !currentTime().Before(nextAt) {
			return l.rotate()
		}
	}

	file, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		// if we fail to open the old log file for some reason, just ignore
		// it and open a new log file.
		return l.openNew()
	}
	l.file = file
	l.size = info.Size()
	l.nextRotateAt = nextAt
	return nil
}

// filename generates the name of the logfile from the current time.
func (l *Logger) filename() string {
	if l.Filename != "" {
		return l.Filename
	}
	name := filepath.Base(os.Args[0]) + "-lumberjack.log"
	return filepath.Join(os.TempDir(), name)
}

// millRunOnce performs compression and removal of stale log files.
// Log files are compressed if enabled via configuration and old log
// files are removed, keeping at most l.MaxBackups files, as long as
// none of them are older than MaxAge.
func (l *Logger) millRunOnce() error {
	if l.MaxBackups == 0 && l.MaxAge == 0 && !l.Compress {
		return nil
	}

	files, err := l.oldLogFiles()
	if err != nil {
		return err
	}

	var compress, remove []logInfo

	if l.MaxBackups > 0 && l.MaxBackups < len(files) {
		preserved := make(map[string]bool)
		var remaining []logInfo
		for _, f := range files {
			// Only count the uncompressed log file or the
			// compressed log file, not both.
			fn := f.Name()
			if strings.HasSuffix(fn, compressSuffix) {
				fn = fn[:len(fn)-len(compressSuffix)]
			}
			preserved[fn] = true

			if len(preserved) > l.MaxBackups {
				remove = append(remove, f)
			} else {
				remaining = append(remaining, f)
			}
		}
		files = remaining
	}
	if l.MaxAge > 0 {
		diff := time.Duration(int64(24*time.Hour) * int64(l.MaxAge))
		cutoff := currentTime().Add(-1 * diff)

		var remaining []logInfo
		for _, f := range files {
			if f.timestamp.Before(cutoff) {
				remove = append(remove, f)
			} else {
				remaining = append(remaining, f)
			}
		}
		files = remaining
	}

	if l.Compress {
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), compressSuffix) {
				compress = append(compress, f)
			}
		}
	}

	for _, f := range remove {
		errRemove := os.Remove(filepath.Join(l.dir(), f.Name()))
		if err == nil && errRemove != nil {
			err = errRemove
		}
	}
	for _, f := range compress {
		fn := filepath.Join(l.dir(), f.Name())
		errCompress := compressLogFile(fn, fn+compressSuffix)
		if err == nil && errCompress != nil {
			err = errCompress
		}
	}

	return err
}

// millRun runs in a goroutine to manage post-rotation compression and removal
// of old log files.
func (l *Logger) millRun(ch <-chan bool, done chan<- struct{}) {
	defer close(done)
	for range ch {
		if err := l.millRunOnce(); err != nil && l.OnError != nil {
			l.OnError(err)
		}
	}
}

// mill performs post-rotation compression and removal of stale log files,
// starting the mill goroutine if necessary. Caller must hold l.mu.
func (l *Logger) mill() {
	if l.millCh == nil {
		ch := make(chan bool, 1)
		done := make(chan struct{})
		l.millCh = ch
		l.millDone = done
		go l.millRun(ch, done)
	}
	select {
	case l.millCh <- true:
	default:
	}
}

// oldLogFiles returns the list of backup log files stored in the same
// directory as the current log file, sorted by newest timestamp in name.
func (l *Logger) oldLogFiles() ([]logInfo, error) {
	entries, err := os.ReadDir(l.dir())
	if err != nil {
		return nil, fmt.Errorf("can't read log file directory: %s", err)
	}
	logFiles := make([]logInfo, 0, len(entries))

	prefix, ext := l.prefixAndExt()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if t, err := l.timeFromName(name, prefix, ext); err == nil {
			logFiles = append(logFiles, logInfo{t, entry})
			continue
		}
		if t, err := l.timeFromName(name, prefix, ext+compressSuffix); err == nil {
			logFiles = append(logFiles, logInfo{t, entry})
			continue
		}
		// error parsing means that the suffix at the end was not generated
		// by lumberjack, and therefore it's not a backup file.
	}

	slices.SortStableFunc(logFiles, func(a, b logInfo) int {
		return b.timestamp.Compare(a.timestamp)
	})

	return logFiles, nil
}

// timeFromName extracts the formatted time from the filename by stripping off
// the filename's prefix and extension. This prevents someone's filename from
// confusing time.Parse.
func (l *Logger) timeFromName(filename, prefix, ext string) (time.Time, error) {
	if !strings.HasPrefix(filename, prefix) {
		return time.Time{}, errors.New("mismatched prefix")
	}
	if !strings.HasSuffix(filename, ext) {
		return time.Time{}, errors.New("mismatched extension")
	}
	ts := filename[len(prefix) : len(filename)-len(ext)]
	if l.LocalTime {
		return time.ParseInLocation(backupTimeFormat, ts, time.Local)
	}
	return time.Parse(backupTimeFormat, ts)
}

// max returns the maximum size in bytes of log files before rolling.
func (l *Logger) max() int64 {
	if l.MaxSize == 0 {
		return int64(defaultMaxSize * megabyte)
	}
	return int64(l.MaxSize) * int64(megabyte)
}

// dir returns the directory for the current filename.
func (l *Logger) dir() string {
	return filepath.Dir(l.filename())
}

// prefixAndExt returns the filename part and extension part from the Logger's
// filename.
func (l *Logger) prefixAndExt() (prefix, ext string) {
	filename := filepath.Base(l.filename())
	ext = filepath.Ext(filename)
	prefix = filename[:len(filename)-len(ext)] + "-"
	return prefix, ext
}

// compressLogFile compresses the given log file, removing the
// uncompressed log file if successful.
func compressLogFile(src, dst string) (err error) {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open log file: %v", err)
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	fi, err := osStat(src)
	if err != nil {
		return fmt.Errorf("failed to stat log file: %v", err)
	}

	if err := chown(dst, fi); err != nil {
		return fmt.Errorf("failed to chown compressed log file: %v", err)
	}

	// If this file already exists, we presume it was created by
	// a previous attempt to compress the log file.
	gzf, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode())
	if err != nil {
		return fmt.Errorf("failed to open compressed log file: %v", err)
	}

	gz := gzipWriterPool.Get().(*gzip.Writer)
	gz.Reset(gzf)
	defer func() {
		gz.Reset(io.Discard)
		gzipWriterPool.Put(gz)
	}()

	defer func() {
		if gzf != nil {
			_ = gzf.Close()
		}
		if err != nil {
			_ = os.Remove(dst)
			err = fmt.Errorf("failed to compress log file: %v", err)
		}
	}()

	if _, err := io.Copy(gz, f); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := gzf.Close(); err != nil {
		gzf = nil
		return err
	}
	gzf = nil

	if err := f.Close(); err != nil {
		f = nil
		return err
	}
	f = nil
	if err := os.Remove(src); err != nil {
		return err
	}

	return nil
}

// logInfo is a convenience struct to return the filename and its embedded
// timestamp without calling lstat on every directory entry.
type logInfo struct {
	timestamp time.Time
	os.DirEntry
}
