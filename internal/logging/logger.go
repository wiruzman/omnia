package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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

	toStdout := stdoutIsTerminal()

	minLevel := parseLevel(cfg.DaemonLogLevel)
	handler := slog.Handler(slog.NewJSONHandler(file, &slog.HandlerOptions{Level: minLevel}))
	if toStdout {
		handler = newFanoutHandler(handler, newConsoleHandler(os.Stdout, minLevel))
	}
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
	raw := fmt.Sprintf(format, v...)
	msg, attrs := parseLogLine(raw)
	l.logger.LogAttrs(context.Background(), classifyLevel(raw), msg, attrs...)
}

type fanoutHandler struct {
	handlers []slog.Handler
}

func newFanoutHandler(handlers ...slog.Handler) *fanoutHandler {
	return &fanoutHandler{handlers: handlers}
}

func (h *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, handler := range h.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		if err := handler.Handle(ctx, record.Clone()); err != nil {
			return err
		}
	}
	return nil
}

func (h *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		handlers = append(handlers, handler.WithAttrs(attrs))
	}
	return &fanoutHandler{handlers: handlers}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		handlers = append(handlers, handler.WithGroup(name))
	}
	return &fanoutHandler{handlers: handlers}
}

type consoleHandler struct {
	out      io.Writer
	minLevel slog.Level
	attrs    []slog.Attr
	mu       *sync.Mutex
}

func newConsoleHandler(out io.Writer, minLevel slog.Level) *consoleHandler {
	return &consoleHandler{out: out, minLevel: minLevel, mu: &sync.Mutex{}}
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel
}

func (h *consoleHandler) Handle(_ context.Context, record slog.Record) error {
	var b strings.Builder
	b.WriteString(record.Time.Local().Format("2006/01/02 15:04:05"))
	b.WriteByte(' ')
	b.WriteString(consoleLevel(record.Level))
	b.WriteByte(' ')
	b.WriteString(record.Message)

	firstAttr := true
	writeAttr := func(attr slog.Attr) bool {
		attr.Value = attr.Value.Resolve()
		if shouldHideConsoleAttr(attr) {
			return true
		}
		if firstAttr {
			b.WriteString(" | ")
			firstAttr = false
		} else {
			b.WriteByte(' ')
		}
		b.WriteString(attr.Key)
		b.WriteByte('=')
		b.WriteString(consoleValue(attr.Value))
		return true
	}
	for _, attr := range h.attrs {
		writeAttr(attr)
	}
	record.Attrs(writeAttr)
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, b.String())
	return err
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &next
}

func (h *consoleHandler) WithGroup(_ string) slog.Handler {
	return h
}

func shouldHideConsoleAttr(attr slog.Attr) bool {
	return attr.Key == "" || attr.Key == "component" || attr.Key == "pid"
}

func consoleLevel(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN "
	case level <= slog.LevelDebug:
		return "DEBUG"
	default:
		return "INFO "
	}
}

func consoleValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return quoteConsoleString(v.String())
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'f', -1, 64)
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	default:
		return quoteConsoleString(v.String())
	}
}

func quoteConsoleString(s string) string {
	if s == "" {
		return ""
	}
	if strings.ContainsAny(s, " \t\r\n|") {
		return strconv.Quote(s)
	}
	return s
}

func parseLogLine(raw string) (string, []slog.Attr) {
	parts := strings.Split(raw, " | ")
	if len(parts) < 2 {
		return raw, nil
	}

	fieldsText := strings.Join(parts[1:], " ")
	attrs, ok := parseKeyValues(fieldsText)
	if !ok {
		return raw, nil
	}
	return parts[0], attrs
}

func parseKeyValues(text string) ([]slog.Attr, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil, false
	}

	attrs := make([]slog.Attr, 0, len(fields))
	unparsed := make([]string, 0)
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || !validKey(key) {
			unparsed = append(unparsed, field)
			continue
		}
		attrs = append(attrs, slog.Any(key, parseValue(value)))
	}
	if len(attrs) == 0 {
		return nil, false
	}
	if len(unparsed) > 0 {
		attrs = append(attrs, slog.String("details", strings.Join(unparsed, " ")))
	}
	return attrs, true
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func parseValue(value string) any {
	if value == "" {
		return ""
	}
	if i, err := strconv.ParseInt(value, 10, 64); err == nil {
		return i
	}
	if b, err := strconv.ParseBool(value); err == nil {
		return b
	}
	if strings.Contains(value, ".") {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	}
	return value
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
		containsWord(lower, "failure"),
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

func containsWord(s, word string) bool {
	start := 0
	for {
		idx := strings.Index(s[start:], word)
		if idx < 0 {
			return false
		}
		idx += start
		end := idx + len(word)
		if isWordBoundary(s, idx-1) && isWordBoundary(s, end) {
			return true
		}
		start = end
	}
}

func isWordBoundary(s string, idx int) bool {
	if idx < 0 || idx >= len(s) {
		return true
	}
	c := s[idx]
	return !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_')
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
