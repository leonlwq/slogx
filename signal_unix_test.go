//go:build unix

package slogx

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSIGHUPFlushesBufferedWriterBeforeRotate(t *testing.T) {
	dir := t.TempDir()
	appLog := filepath.Join(dir, "app.log")
	errLog := filepath.Join(dir, "error.log")

	// Use a 1-hour flush interval so buffered logs stay exclusively in user-space RAM
	// until SIGHUP triggers coordinated flush + rotation.
	l := MustNew(
		WithConsole(false),
		WithBuffer(64*1024, time.Hour),
		WithFile(appLog, FileOptions{MaxSizeMB: 10, LocalTime: true}),
		WithErrorFile(errLog, FileOptions{MaxSizeMB: 10, LocalTime: true}),
		WithEnableSIGHUP(true),
	)
	defer l.Close()

	l.Info("before-sighup-buffered")

	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := p.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("Signal SIGHUP: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	var rotatedBackup string
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != "app.log" && e.Name() != "error.log" && filepath.Ext(e.Name()) == ".log" {
				if len(e.Name()) > 4 && e.Name()[:4] == "app-" {
					rotatedBackup = filepath.Join(dir, e.Name())
					break
				}
			}
		}
		if rotatedBackup != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rotatedBackup == "" {
		t.Fatalf("timed out waiting for SIGHUP rotated app backup file")
	}

	backupRaw, err := os.ReadFile(rotatedBackup)
	if err != nil {
		t.Fatalf("ReadFile rotated backup: %v", err)
	}
	backupRecords := parseJSONLines(t, backupRaw)
	if len(backupRecords) != 1 || backupRecords[0]["msg"] != "before-sighup-buffered" {
		t.Fatalf("expected pre-SIGHUP buffered record inside rotated backup, got %#v", backupRecords)
	}

	activeRaw, err := os.ReadFile(appLog)
	if err != nil {
		t.Fatalf("ReadFile active app.log: %v", err)
	}
	if len(activeRaw) != 0 {
		t.Fatalf("expected active app.log to be empty immediately after SIGHUP rotation, got %q", string(activeRaw))
	}
}
