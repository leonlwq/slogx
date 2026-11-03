package lumberjack

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// !!!NOTE!!!
//
// Running these tests in parallel will almost certainly cause sporadic (or even
// regular) failures, because they're all messing with the same global variable
// that controls the logic's mocked time.Now.  So... don't do that.

// Since all the tests uses the time to determine filenames etc, we need to
// control the wall clock as much as possible, which means having a wall clock
// that doesn't change unless we want it to.
var (
	fakeTimeMu      sync.RWMutex
	fakeCurrentTime = time.Now()
)

func fakeTime() time.Time {
	fakeTimeMu.RLock()
	defer fakeTimeMu.RUnlock()
	return fakeCurrentTime
}

func setFakeTime(t time.Time) {
	fakeTimeMu.Lock()
	defer fakeTimeMu.Unlock()
	fakeCurrentTime = t
}

func TestNewFile(t *testing.T) {
	currentTime = fakeTime

	dir := makeTempDir("TestNewFile", t)
	defer os.RemoveAll(dir)
	l := &Logger{
		Filename: logFile(dir),
	}
	defer l.Close()
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)
	existsWithContent(logFile(dir), b, t)
	fileCount(dir, 1, t)
}

func TestOpenExisting(t *testing.T) {
	currentTime = fakeTime
	dir := makeTempDir("TestOpenExisting", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	data := []byte("foo!")
	err := os.WriteFile(filename, data, 0644)
	isNil(err, t)
	existsWithContent(filename, data, t)

	l := &Logger{
		Filename: filename,
	}
	defer l.Close()
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	// make sure the file got appended
	existsWithContent(filename, append(data, b...), t)

	// make sure no other files were created
	fileCount(dir, 1, t)
}

func TestWriteTooLong(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	t.Cleanup(func() { megabyte = 1024 * 1024 })
	dir := makeTempDir("TestWriteTooLong", t)
	defer os.RemoveAll(dir)
	l := &Logger{
		Filename: logFile(dir),
		MaxSize:  5,
	}
	defer l.Close()
	b := []byte("booooooooooooooo!")
	n, err := l.Write(b)
	notNil(err, t)
	equals(0, n, t)
	equals(err.Error(),
		fmt.Sprintf("write length %d exceeds maximum file size %d", len(b), l.MaxSize), t)
	_, err = os.Stat(logFile(dir))
	assert(os.IsNotExist(err), t, "File exists, but should not have been created")
}

func TestMakeLogDir(t *testing.T) {
	currentTime = fakeTime
	dir := time.Now().Format("TestMakeLogDir" + backupTimeFormat)
	dir = filepath.Join(os.TempDir(), dir)
	defer os.RemoveAll(dir)
	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
	}
	defer l.Close()
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)
	existsWithContent(logFile(dir), b, t)
	fileCount(dir, 1, t)
}

func TestDefaultFilename(t *testing.T) {
	currentTime = fakeTime
	dir := os.TempDir()
	filename := filepath.Join(dir, filepath.Base(os.Args[0])+"-lumberjack.log")
	defer os.Remove(filename)
	l := &Logger{}
	defer l.Close()
	b := []byte("boo!")
	n, err := l.Write(b)

	isNil(err, t)
	equals(len(b), n, t)
	existsWithContent(filename, b, t)
}

func TestAutoRotate(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	t.Cleanup(func() { megabyte = 1024 * 1024 })

	dir := makeTempDir("TestAutoRotate", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
	}
	defer l.Close()
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)

	newFakeTime()

	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	// the old logfile should be moved aside and the main logfile should have
	// only the last write in it.
	existsWithContent(filename, b2, t)

	// the backup file will use the current fake time and have the old contents.
	existsWithContent(backupFile(dir), b, t)

	fileCount(dir, 2, t)
}

func TestFirstWriteRotate(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	t.Cleanup(func() { megabyte = 1024 * 1024 })
	dir := makeTempDir("TestFirstWriteRotate", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
	}
	defer l.Close()

	start := []byte("boooooo!")
	err := os.WriteFile(filename, start, 0600)
	isNil(err, t)

	newFakeTime()

	// this would make us rotate
	b := []byte("fooo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	existsWithContent(backupFile(dir), start, t)

	fileCount(dir, 2, t)
}

func TestMaxBackups(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	t.Cleanup(func() { megabyte = 1024 * 1024 })
	dir := makeTempDir("TestMaxBackups", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename:   filename,
		MaxSize:    10,
		MaxBackups: 1,
	}
	defer l.Close()
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)

	newFakeTime()

	// this will put us over the max
	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	// this will use the new fake time
	secondFilename := backupFile(dir)
	existsWithContent(secondFilename, b, t)

	// make sure the old file still exists with the same content.
	existsWithContent(filename, b2, t)

	fileCount(dir, 2, t)

	newFakeTime()

	// this will make us rotate again
	b3 := []byte("baaaaaar!")
	n, err = l.Write(b3)
	isNil(err, t)
	equals(len(b3), n, t)

	// this will use the new fake time
	thirdFilename := backupFile(dir)
	existsWithContent(thirdFilename, b2, t)

	existsWithContent(filename, b3, t)

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(time.Millisecond * 10)

	// should only have two files in the dir still
	fileCount(dir, 2, t)

	// second file name should still exist
	existsWithContent(thirdFilename, b2, t)

	// should have deleted the first backup
	notExist(secondFilename, t)

	// now test that we don't delete directories or non-logfile files

	newFakeTime()

	// create a file that is close to but different from the logfile name.
	// It shouldn't get caught by our deletion filters.
	notlogfile := logFile(dir) + ".foo"
	err = os.WriteFile(notlogfile, []byte("data"), 0644)
	isNil(err, t)

	// Make a directory that exactly matches our log file filters... it still
	// shouldn't get caught by the deletion filter since it's a directory.
	notlogfiledir := backupFile(dir)
	err = os.Mkdir(notlogfiledir, 0700)
	isNil(err, t)

	newFakeTime()

	// this will use the new fake time
	fourthFilename := backupFile(dir)

	// Create a log file that is/was being compressed - this should
	// not be counted since both the compressed and the uncompressed
	// log files still exist.
	compLogFile := fourthFilename + compressSuffix
	err = os.WriteFile(compLogFile, []byte("compress"), 0644)
	isNil(err, t)

	// this will make us rotate again
	b4 := []byte("baaaaaaz!")
	n, err = l.Write(b4)
	isNil(err, t)
	equals(len(b4), n, t)

	existsWithContent(fourthFilename, b3, t)
	existsWithContent(fourthFilename+compressSuffix, []byte("compress"), t)

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(time.Millisecond * 10)

	// We should have four things in the directory now - the 2 log files, the
	// not log file, and the directory
	fileCount(dir, 5, t)

	// third file name should still exist
	existsWithContent(filename, b4, t)

	existsWithContent(fourthFilename, b3, t)

	// should have deleted the first filename
	notExist(thirdFilename, t)

	// the not-a-logfile should still exist
	exists(notlogfile, t)

	// the directory
	exists(notlogfiledir, t)
}

func TestCleanupExistingBackups(t *testing.T) {
	// test that if we start with more backup files than we're supposed to have
	// in total, that extra ones get cleaned up when we rotate.

	currentTime = fakeTime
	megabyte = 1
	t.Cleanup(func() { megabyte = 1024 * 1024 })

	dir := makeTempDir("TestCleanupExistingBackups", t)
	defer os.RemoveAll(dir)

	// make 3 backup files

	data := []byte("data")
	backup := backupFile(dir)
	err := os.WriteFile(backup, data, 0644)
	isNil(err, t)

	newFakeTime()

	backup = backupFile(dir)
	err = os.WriteFile(backup+compressSuffix, data, 0644)
	isNil(err, t)

	newFakeTime()

	backup = backupFile(dir)
	err = os.WriteFile(backup, data, 0644)
	isNil(err, t)

	// now create a primary log file with some data
	filename := logFile(dir)
	err = os.WriteFile(filename, data, 0644)
	isNil(err, t)

	l := &Logger{
		Filename:   filename,
		MaxSize:    10,
		MaxBackups: 1,
	}
	defer l.Close()

	newFakeTime()

	b2 := []byte("foooooo!")
	n, err := l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(time.Millisecond * 10)

	// now we should only have 2 files left - the primary and one backup
	fileCount(dir, 2, t)
}

func TestMaxAge(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	t.Cleanup(func() { megabyte = 1024 * 1024 })

	dir := makeTempDir("TestMaxAge", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Filename: filename,
		MaxSize:  10,
		MaxAge:   1,
	}
	defer l.Close()
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)

	// two days later
	newFakeTime()

	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)
	existsWithContent(backupFile(dir), b, t)

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(10 * time.Millisecond)

	// We should still have 2 log files, since the most recent backup was just
	// created.
	fileCount(dir, 2, t)

	existsWithContent(filename, b2, t)

	// we should have deleted the old file due to being too old
	existsWithContent(backupFile(dir), b, t)

	// two days later
	newFakeTime()

	b3 := []byte("baaaaar!")
	n, err = l.Write(b3)
	isNil(err, t)
	equals(len(b3), n, t)
	existsWithContent(backupFile(dir), b2, t)

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(10 * time.Millisecond)

	// We should have 2 log files - the main log file, and the most recent
	// backup.  The earlier backup is past the cutoff and should be gone.
	fileCount(dir, 2, t)

	existsWithContent(filename, b3, t)

	// we should have deleted the old file due to being too old
	existsWithContent(backupFile(dir), b2, t)
}

func TestOldLogFiles(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	t.Cleanup(func() { megabyte = 1024 * 1024 })

	dir := makeTempDir("TestOldLogFiles", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	data := []byte("data")
	err := os.WriteFile(filename, data, 07)
	isNil(err, t)

	// This gives us a time with the same precision as the time we get from the
	// timestamp in the name.
	t1, err := time.Parse(backupTimeFormat, fakeTime().UTC().Format(backupTimeFormat))
	isNil(err, t)

	backup := backupFile(dir)
	err = os.WriteFile(backup, data, 07)
	isNil(err, t)

	newFakeTime()

	t2, err := time.Parse(backupTimeFormat, fakeTime().UTC().Format(backupTimeFormat))
	isNil(err, t)

	backup2 := backupFile(dir)
	err = os.WriteFile(backup2, data, 07)
	isNil(err, t)

	l := &Logger{Filename: filename}
	files, err := l.oldLogFiles()
	isNil(err, t)
	equals(2, len(files), t)

	// should be sorted by newest file first, which would be t2
	equals(t2, files[0].timestamp, t)
	equals(t1, files[1].timestamp, t)
}

func TestTimeFromName(t *testing.T) {
	l := &Logger{Filename: "/var/log/myfoo/foo.log"}
	prefix, ext := l.prefixAndExt()

	tests := []struct {
		filename string
		want     time.Time
		wantErr  bool
	}{
		{"foo-2014-05-04T14-44-33.555.log", time.Date(2014, 5, 4, 14, 44, 33, 555000000, time.UTC), false},
		{"foo-2014-05-04T14-44-33.555", time.Time{}, true},
		{"2014-05-04T14-44-33.555.log", time.Time{}, true},
		{"foo.log", time.Time{}, true},
	}

	for _, test := range tests {
		got, err := l.timeFromName(test.filename, prefix, ext)
		equals(got, test.want, t)
		equals(err != nil, test.wantErr, t)
	}
}

func TestLocalTime(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	t.Cleanup(func() { megabyte = 1024 * 1024 })

	dir := makeTempDir("TestLocalTime", t)
	defer os.RemoveAll(dir)

	l := &Logger{
		Filename:  logFile(dir),
		MaxSize:   10,
		LocalTime: true,
	}
	defer l.Close()
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	b2 := []byte("fooooooo!")
	n2, err := l.Write(b2)
	isNil(err, t)
	equals(len(b2), n2, t)

	existsWithContent(logFile(dir), b2, t)
	existsWithContent(backupFileLocal(dir), b, t)
}

func TestRotate(t *testing.T) {
	currentTime = fakeTime
	dir := makeTempDir("TestRotate", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)

	l := &Logger{
		Filename:   filename,
		MaxBackups: 1,
		MaxSize:    100, // megabytes
	}
	defer l.Close()
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)

	newFakeTime()

	err = l.Rotate()
	isNil(err, t)

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(10 * time.Millisecond)

	filename2 := backupFile(dir)
	existsWithContent(filename2, b, t)
	existsWithContent(filename, []byte{}, t)
	fileCount(dir, 2, t)
	newFakeTime()

	err = l.Rotate()
	isNil(err, t)

	// we need to wait a little bit since the files get deleted on a different
	// goroutine.
	<-time.After(10 * time.Millisecond)

	filename3 := backupFile(dir)
	existsWithContent(filename3, []byte{}, t)
	existsWithContent(filename, []byte{}, t)
	fileCount(dir, 2, t)

	b2 := []byte("foooooo!")
	n, err = l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)

	// this will use the new fake time
	existsWithContent(filename, b2, t)
}

func TestCompressOnRotate(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	t.Cleanup(func() { megabyte = 1024 * 1024 })

	dir := makeTempDir("TestCompressOnRotate", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Compress: true,
		Filename: filename,
		MaxSize:  10,
	}
	defer l.Close()
	b := []byte("boo!")
	n, err := l.Write(b)
	isNil(err, t)
	equals(len(b), n, t)

	existsWithContent(filename, b, t)
	fileCount(dir, 1, t)

	newFakeTime()

	err = l.Rotate()
	isNil(err, t)

	// the old logfile should be moved aside and the main logfile should have
	// nothing in it.
	existsWithContent(filename, []byte{}, t)

	// we need to wait a little bit since the files get compressed on a different
	// goroutine.
	<-time.After(300 * time.Millisecond)

	// a compressed version of the log file should now exist and the original
	// should have been removed.
	bc := new(bytes.Buffer)
	gz := gzip.NewWriter(bc)
	_, err = gz.Write(b)
	isNil(err, t)
	err = gz.Close()
	isNil(err, t)
	existsWithContent(backupFile(dir)+compressSuffix, bc.Bytes(), t)
	notExist(backupFile(dir), t)

	fileCount(dir, 2, t)
}

func TestCompressOnResume(t *testing.T) {
	currentTime = fakeTime
	megabyte = 1
	t.Cleanup(func() { megabyte = 1024 * 1024 })

	dir := makeTempDir("TestCompressOnResume", t)
	defer os.RemoveAll(dir)

	filename := logFile(dir)
	l := &Logger{
		Compress: true,
		Filename: filename,
		MaxSize:  10,
	}
	defer l.Close()

	// Create a backup file and empty "compressed" file.
	filename2 := backupFile(dir)
	b := []byte("foo!")
	err := os.WriteFile(filename2, b, 0644)
	isNil(err, t)
	err = os.WriteFile(filename2+compressSuffix, []byte{}, 0644)
	isNil(err, t)

	newFakeTime()

	b2 := []byte("boo!")
	n, err := l.Write(b2)
	isNil(err, t)
	equals(len(b2), n, t)
	existsWithContent(filename, b2, t)

	// we need to wait a little bit since the files get compressed on a different
	// goroutine.
	<-time.After(300 * time.Millisecond)

	// The write should have started the compression - a compressed version of
	// the log file should now exist and the original should have been removed.
	bc := new(bytes.Buffer)
	gz := gzip.NewWriter(bc)
	_, err = gz.Write(b)
	isNil(err, t)
	err = gz.Close()
	isNil(err, t)
	existsWithContent(filename2+compressSuffix, bc.Bytes(), t)
	notExist(filename2, t)

	fileCount(dir, 2, t)
}

func TestJson(t *testing.T) {
	data := []byte(`
{
	"filename": "foo",
	"maxsize": 5,
	"maxage": 10,
	"maxbackups": 3,
	"localtime": true,
	"compress": true
}`[1:])

	l := Logger{}
	err := json.Unmarshal(data, &l)
	isNil(err, t)
	equals("foo", l.Filename, t)
	equals(5, l.MaxSize, t)
	equals(10, l.MaxAge, t)
	equals(3, l.MaxBackups, t)
	equals(true, l.LocalTime, t)
	equals(true, l.Compress, t)
}

// makeTempDir creates a file with a semi-unique name in the OS temp directory.
// It should be based on the name of the test, to keep parallel tests from
// colliding, and must be cleaned up after the test is finished.
func makeTempDir(name string, t testing.TB) string {
	t.Helper()
	dir := time.Now().Format(name + backupTimeFormat)
	dir = filepath.Join(os.TempDir(), dir)
	isNil(os.Mkdir(dir, 0700), t)
	return dir
}

// existsWithContent checks that the given file exists and has the correct content.
func existsWithContent(path string, content []byte, t testing.TB) {
	t.Helper()
	info, err := os.Stat(path)
	isNil(err, t)
	equals(int64(len(content)), info.Size(), t)

	b, err := os.ReadFile(path)
	isNil(err, t)
	equals(content, b, t)
}

// logFile returns the log file name in the given directory for the current fake
// time.
func logFile(dir string) string {
	return filepath.Join(dir, "foobar.log")
}

func backupFile(dir string) string {
	return filepath.Join(dir, "foobar-"+fakeTime().UTC().Format(backupTimeFormat)+".log")
}

func backupFileLocal(dir string) string {
	return filepath.Join(dir, "foobar-"+fakeTime().Format(backupTimeFormat)+".log")
}

// fileCount checks that the number of files in the directory is exp.
func fileCount(dir string, exp int, t testing.TB) {
	t.Helper()
	files, err := os.ReadDir(dir)
	isNil(err, t)
	// Make sure no other files were created.
	equals(exp, len(files), t)
}

// newFakeTime sets the fake "current time" to two days later.
func newFakeTime() {
	fakeTimeMu.Lock()
	defer fakeTimeMu.Unlock()
	fakeCurrentTime = fakeCurrentTime.Add(time.Hour * 24 * 2)
}

func notExist(path string, t testing.TB) {
	t.Helper()
	_, err := os.Stat(path)
	assert(os.IsNotExist(err), t, "expected to get os.IsNotExist, but instead got %v", err)
}

func exists(path string, t testing.TB) {
	t.Helper()
	_, err := os.Stat(path)
	assert(err == nil, t, "expected file to exist, but got error from os.Stat: %v", err)
}

func TestTimeRotationDailyLocal(t *testing.T) {
	megabyte = 1024 * 1024
	dir := makeTempDir("TestTimeRotationDailyLocal", t)
	defer os.RemoveAll(dir)

	loc := time.Local
	setFakeTime(time.Date(2026, 11, 3, 23, 50, 0, 0, loc))
	currentTime = fakeTime

	filename := logFile(dir)
	l := &Logger{
		Filename:         filename,
		MaxSize:          100,
		LocalTime:        true,
		RotationInterval: RotateDaily,
	}
	defer l.Close()

	day1Part1 := []byte("day1-part1\n")
	_, err := l.Write(day1Part1)
	isNil(err, t)

	// Still before midnight -> should append without rotating
	setFakeTime(time.Date(2026, 11, 3, 23, 59, 59, 0, loc))
	day1Part2 := []byte("day1-part2\n")
	_, err = l.Write(day1Part2)
	isNil(err, t)
	fileCount(dir, 1, t)
	existsWithContent(filename, append(day1Part1, day1Part2...), t)

	// Cross local midnight -> must rotate immediately
	setFakeTime(time.Date(2026, 11, 4, 0, 0, 0, 0, loc))
	day2 := []byte("day2-start\n")
	_, err = l.Write(day2)
	isNil(err, t)

	fileCount(dir, 2, t)
	existsWithContent(filename, day2, t)
	existsWithContent(backupFileLocal(dir), append(day1Part1, day1Part2...), t)
}

func TestTimeRotationHourly(t *testing.T) {
	megabyte = 1024 * 1024
	dir := makeTempDir("TestTimeRotationHourly", t)
	defer os.RemoveAll(dir)

	setFakeTime(time.Date(2026, 11, 3, 10, 15, 0, 0, time.UTC))
	currentTime = fakeTime

	filename := logFile(dir)
	l := &Logger{
		Filename:         filename,
		MaxSize:          100,
		LocalTime:        false,
		RotationInterval: RotateHourly,
	}
	defer l.Close()

	_, err := l.Write([]byte("hour10\n"))
	isNil(err, t)
	fileCount(dir, 1, t)

	// Advance within hour 10
	setFakeTime(time.Date(2026, 11, 3, 10, 59, 59, 0, time.UTC))
	_, err = l.Write([]byte("still-hour10\n"))
	isNil(err, t)
	fileCount(dir, 1, t)

	// Advance to top of hour 11
	setFakeTime(time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC))
	_, err = l.Write([]byte("hour11\n"))
	isNil(err, t)
	fileCount(dir, 2, t)
	existsWithContent(filename, []byte("hour11\n"), t)
}

func TestCloseReopenAndGoroutineCleanup(t *testing.T) {
	megabyte = 1024 * 1024
	currentTime = fakeTime
	dir := makeTempDir("TestCloseReopenAndGoroutineCleanup", t)
	defer os.RemoveAll(dir)

	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baseGoroutines := runtime.NumGoroutine()

	filename := logFile(dir)
	for i := 0; i < 10; i++ {
		l := &Logger{
			Filename:   filename,
			MaxBackups: 3,
			Compress:   true,
		}
		_, err := l.Write([]byte("hello\n"))
		isNil(err, t)
		isNil(l.Sync(), t)
		isNil(l.Close(), t)

		// Reopen after Close must also work cleanly without panic
		_, err = l.Write([]byte("reopened\n"))
		isNil(err, t)
		isNil(l.Close(), t)
	}

	time.Sleep(30 * time.Millisecond)
	afterGoroutines := runtime.NumGoroutine()
	assert(afterGoroutines <= baseGoroutines+2, t,
		"goroutine leak detected: before=%d after=%d", baseGoroutines, afterGoroutines)
}

func TestMaxAgeLocalTime(t *testing.T) {
	origLocal := time.Local
	// Simulate UTC-8 where parsing local timestamps as UTC previously caused 8h premature deletion.
	loc := time.FixedZone("UTC-8", -8*3600)
	time.Local = loc
	t.Cleanup(func() { time.Local = origLocal })

	megabyte = 1024 * 1024
	dir := makeTempDir("TestMaxAgeLocalTime", t)
	defer os.RemoveAll(dir)

	base := time.Date(2026, 11, 2, 10, 0, 0, 0, loc)
	setFakeTime(base)
	currentTime = fakeTime

	filename := logFile(dir)
	l := &Logger{
		Filename:  filename,
		MaxSize:   100,
		MaxAge:    1, // 24 hours
		LocalTime: true,
	}
	defer l.Close()

	_, err := l.Write([]byte("day1\n"))
	isNil(err, t)
	isNil(l.Rotate(), t)
	firstBackup := backupFileLocal(dir)

	// 18 hours later (< 24h MaxAge): firstBackup MUST still be retained!
	setFakeTime(base.Add(18 * time.Hour))
	_, err = l.Write([]byte("18h-later\n"))
	isNil(err, t)
	isNil(l.Rotate(), t)
	time.Sleep(20 * time.Millisecond)
	exists(firstBackup, t)

	// 25 hours after base (> 24h MaxAge): firstBackup MUST now be removed!
	setFakeTime(base.Add(25 * time.Hour))
	_, err = l.Write([]byte("25h-later\n"))
	isNil(err, t)
	isNil(l.Rotate(), t)
	time.Sleep(20 * time.Millisecond)
	notExist(firstBackup, t)
}
