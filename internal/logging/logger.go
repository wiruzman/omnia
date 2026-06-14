package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"omnia-search-tui/internal/config"
)

type PrintfLogger interface {
	Printf(format string, v ...any)
}

type DaemonLogger struct {
	Logger   PrintfLogger
	Path     string
	ToStdout bool

	closer io.Closer
}

func OpenDaemon(cfg config.Config) (*DaemonLogger, error) {
	file, err := newRotatingFile(cfg.DaemonLogFile, cfg.DaemonLogMaxBytes, cfg.DaemonLogBackups)
	if err != nil {
		return nil, fmt.Errorf("open daemon log %q: %w", cfg.DaemonLogFile, err)
	}

	output := io.Writer(file)
	toStdout := stdoutIsTerminal()
	if toStdout {
		output = io.MultiWriter(os.Stdout, file)
	}

	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: parseLevel(cfg.DaemonLogLevel)})
	withAttrs := handler.WithAttrs([]slog.Attr{
		slog.String("component", "daemon"),
		slog.Int("pid", os.Getpid()),
	})
	logger := slog.New(withAttrs)
	return &DaemonLogger{
		Logger:   &printfLogger{logger: logger},
		Path:     cfg.DaemonLogFile,
		ToStdout: toStdout,
		closer:   file,
	}, nil
}

func (l *DaemonLogger) Close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	return l.closer.Close()
}

type printfLogger struct {
	logger *slog.Logger
}

func (l *printfLogger) Printf(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	l.logger.Log(context.Background(), classifyLevel(msg), msg)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func classifyLevel(msg string) slog.Level {
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "panic"),
		strings.HasPrefix(lower, "failed"),
		strings.Contains(lower, " failed"),
		strings.Contains(lower, " failure"),
		strings.Contains(lower, " error:"),
		strings.HasPrefix(lower, "indexing error"),
		strings.HasPrefix(lower, "daemon error"),
		strings.Contains(lower, "with error"):
		return slog.LevelError
	case strings.Contains(lower, " warning"),
		strings.HasPrefix(lower, "warning"),
		strings.Contains(lower, " warn"):
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

type rotatingFile struct {
	path      string
	maxBytes  int64
	maxBackup int

	mu   sync.Mutex
	file *os.File
	size int64
}

func newRotatingFile(path string, maxBytes int64, maxBackups int) (*rotatingFile, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("log path is empty")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("log max bytes must be positive")
	}
	if maxBackups < 0 {
		return nil, fmt.Errorf("log backups cannot be negative")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rotatingFile{path: path, maxBytes: maxBytes, maxBackup: maxBackups, file: f, size: info.Size()}, nil
}

func (f *rotatingFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.file == nil {
		return 0, fmt.Errorf("log file is closed")
	}
	if f.size > 0 && f.size+int64(len(p)) > f.maxBytes {
		if err := f.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := f.file.Write(p)
	f.size += int64(n)
	return n, err
}

func (f *rotatingFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}

func (f *rotatingFile) rotateLocked() error {
	if err := f.file.Close(); err != nil {
		return err
	}
	f.file = nil

	if f.maxBackup == 0 {
		if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		for i := f.maxBackup - 1; i >= 1; i-- {
			oldPath := backupPath(f.path, i)
			newPath := backupPath(f.path, i+1)
			if err := os.Remove(newPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		firstBackup := backupPath(f.path, 1)
		if err := os.Remove(firstBackup); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(f.path, firstBackup); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	f.file = file
	f.size = 0
	return nil
}

func backupPath(path string, n int) string {
	return fmt.Sprintf("%s.%d", path, n)
}
