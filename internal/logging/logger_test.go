package logging

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"omnia-search-tui/internal/config"
)

func TestOpenDaemonWritesJSONWithClassifiedLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	logger, err := OpenDaemon(config.Config{
		DaemonLogFile:     path,
		DaemonLogLevel:    "info",
		DaemonLogMaxBytes: 1024,
		DaemonLogBackups:  2,
	})
	if err != nil {
		t.Fatalf("open daemon logger: %v", err)
	}

	logger.Logger.Printf("watch setup failed for %s: %v", "/tmp", errors.New("boom"))
	if err := logger.Close(); err != nil {
		t.Fatalf("close daemon logger: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		t.Fatal("expected log record")
	}

	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("parse json log: %v\n%s", err, line)
	}
	if record["level"] != "ERROR" {
		t.Fatalf("expected ERROR level, got %v", record["level"])
	}
	if record["msg"] != "watch setup failed for /tmp: boom" {
		t.Fatalf("unexpected message: %v", record["msg"])
	}
	if record["component"] != "daemon" {
		t.Fatalf("expected daemon component, got %v", record["component"])
	}
	if _, ok := record["pid"].(float64); !ok {
		t.Fatalf("expected numeric pid, got %T", record["pid"])
	}
}

func TestOpenDaemonHonorsMinimumLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	logger, err := OpenDaemon(config.Config{
		DaemonLogFile:     path,
		DaemonLogLevel:    "error",
		DaemonLogMaxBytes: 1024,
		DaemonLogBackups:  2,
	})
	if err != nil {
		t.Fatalf("open daemon logger: %v", err)
	}

	logger.Logger.Printf("daemon started")
	logger.Logger.Printf("daemon stopped with error: boom")
	if err := logger.Close(); err != nil {
		t.Fatalf("close daemon logger: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected only one error log line, got %d: %q", len(lines), string(data))
	}
	if !strings.Contains(lines[0], "daemon stopped with error: boom") {
		t.Fatalf("expected error log line, got %q", lines[0])
	}
}

func TestRotatingFileRetainsConfiguredBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	writer, err := newRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatalf("open rotating file: %v", err)
	}

	for _, line := range []string{"one\n", "two\n", "three\n", "four\n"} {
		if _, err := writer.Write([]byte(line)); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close rotating file: %v", err)
	}

	active := mustReadString(t, path)
	firstBackup := mustReadString(t, path+".1")
	secondBackup := mustReadString(t, path+".2")

	if active != "four\n" {
		t.Fatalf("unexpected active log: %q", active)
	}
	if firstBackup != "three\n" {
		t.Fatalf("unexpected first backup: %q", firstBackup)
	}
	if secondBackup != "one\ntwo\n" {
		t.Fatalf("unexpected second backup: %q", secondBackup)
	}
}

func mustReadString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
