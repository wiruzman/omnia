package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/config"
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

	logger.Logger.Printf("incremental flush | total=%d upserts=%d deletes=%d skipped=%d failures=%d", 2, 1, 1, 0, 0)
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
	if record["level"] != "INFO" {
		t.Fatalf("expected INFO level, got %v", record["level"])
	}
	if record["msg"] != "incremental flush" {
		t.Fatalf("unexpected message: %v", record["msg"])
	}
	if record["total"] != float64(2) || record["upserts"] != float64(1) || record["deletes"] != float64(1) || record["skipped"] != float64(0) || record["failures"] != float64(0) {
		t.Fatalf("expected parsed numeric flush fields, got %+v", record)
	}
	if record["component"] != "daemon" {
		t.Fatalf("expected daemon component, got %v", record["component"])
	}
	if _, ok := record["pid"].(float64); !ok {
		t.Fatalf("expected numeric pid, got %T", record["pid"])
	}
}

func TestConsoleHandlerWritesHumanReadableLine(t *testing.T) {
	var buf bytes.Buffer
	handler := newConsoleHandler(&buf, slog.LevelInfo).WithAttrs([]slog.Attr{
		slog.String("component", "daemon"),
		slog.Int("pid", 123),
	})

	record := slog.NewRecord(time.Date(2026, 6, 14, 19, 49, 48, 0, time.Local), slog.LevelInfo, "incremental flush", 0)
	record.AddAttrs(
		slog.Int("total", 2),
		slog.Int("upserts", 1),
		slog.Int("deletes", 1),
		slog.Int("skipped", 0),
		slog.Int("failures", 0),
	)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("write console log: %v", err)
	}

	want := "2026/06/14 19:49:48 INFO  incremental flush | total=2 upserts=1 deletes=1 skipped=0 failures=0\n"
	if buf.String() != want {
		t.Fatalf("unexpected console output:\nwant %q\n got %q", want, buf.String())
	}
}

func TestConsoleHandlerWritesErrorLevel(t *testing.T) {
	var buf bytes.Buffer
	handler := newConsoleHandler(&buf, slog.LevelInfo)

	record := slog.NewRecord(time.Date(2026, 6, 14, 19, 50, 10, 0, time.Local), slog.LevelError, "watch setup failed", 0)
	record.AddAttrs(slog.String("root", "/tmp"), slog.String("err", "boom"))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatalf("write console log: %v", err)
	}

	want := "2026/06/14 19:50:10 ERROR watch setup failed | root=/tmp err=boom\n"
	if buf.String() != want {
		t.Fatalf("unexpected console output:\nwant %q\n got %q", want, buf.String())
	}
}

func TestParseLogLineExtractsFields(t *testing.T) {
	msg, attrs := parseLogLine("daemon logging initialized | path=/tmp/daemon.log level=info max_bytes=10485760 backups=5 stdout=false")

	if msg != "daemon logging initialized" {
		t.Fatalf("unexpected message: %q", msg)
	}
	if len(attrs) != 5 {
		t.Fatalf("expected 5 attrs, got %d: %+v", len(attrs), attrs)
	}
	if attrs[0].Key != "path" || attrs[0].Value.String() != "/tmp/daemon.log" {
		t.Fatalf("unexpected path attr: %+v", attrs[0])
	}
	if attrs[2].Key != "max_bytes" || attrs[2].Value.Int64() != 10485760 {
		t.Fatalf("unexpected max_bytes attr: %+v", attrs[2])
	}
	if attrs[4].Key != "stdout" || attrs[4].Value.Bool() {
		t.Fatalf("unexpected stdout attr: %+v", attrs[4])
	}
}

func TestClassifyLevelKeepsFailureCountersAtInfo(t *testing.T) {
	got := classifyLevel("incremental flush | total=2 upserts=1 deletes=1 skipped=0 failures=0")
	if got != slog.LevelInfo {
		t.Fatalf("expected failures counter to stay info, got %v", got)
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
