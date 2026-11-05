package metrics

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

var ownedName = regexp.MustCompile(`^metrics-v1-(\d{4}-\d{2}-\d{2})-(\d{4})\.jsonl$`)

type historyFile struct {
	name string
	size int64
	day  time.Time
	part int
}

// history is used by one sink worker. No index connection is opened.
type history struct {
	dir      string
	days     int
	maxBytes int64
	prepared string
	lastFile string
}

func (h *history) files() ([]historyFile, error) {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return nil, err
	}
	var files []historyFile
	for _, entry := range entries {
		match := ownedName.FindStringSubmatch(entry.Name())
		if match == nil || !entry.Type().IsRegular() {
			continue
		}
		day, err := time.Parse("2006-01-02", match[1])
		if err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		part, _ := strconv.Atoi(match[2])
		files = append(files, historyFile{entry.Name(), info.Size(), day, part})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

func (h *history) append(now time.Time, data []byte) error {
	if int64(len(data)) > h.maxBytes {
		return errors.New("metrics record exceeds storage cap")
	}
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(h.dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid metrics directory")
	}
	files, err := h.files()
	if err != nil {
		return err
	}
	cutoff := now.UTC().AddDate(0, 0, -h.days)
	kept := files[:0]
	var total int64
	for _, f := range files {
		// Expire whole UTC-day files only when every record in the day is old.
		if !f.day.AddDate(0, 0, 1).After(cutoff) {
			if err := os.Remove(filepath.Join(h.dir, f.name)); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, f)
		total += f.size
	}
	files = kept
	for len(files) > 0 && total+int64(len(data)) > h.maxBytes {
		f := files[0]
		if err := os.Remove(filepath.Join(h.dir, f.name)); err != nil {
			return err
		}
		total -= f.size
		files = files[1:]
		if h.prepared == f.name {
			h.prepared = ""
		}
	}
	day := now.UTC().Format("2006-01-02")
	part := 0
	for _, f := range files {
		if f.day.Format("2006-01-02") == day {
			part = max(part, f.part)
			if f.size+int64(len(data)) > min(8<<20, h.maxBytes) {
				part = max(part, f.part+1)
			}
		}
	}
	if part > 9999 {
		return errors.New("metrics daily part limit exceeded")
	}
	name := fmt.Sprintf("metrics-v1-%s-%04d.jsonl", day, part)
	// OpenRoot prevents a symlink from directing writes outside this directory.
	root, err := os.OpenRoot(h.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if info, err := root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return errors.New("metrics target is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if h.prepared != name {
		if err := repairTail(f); err != nil {
			return err
		}
		h.prepared = name
	}
	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err != nil || n != len(data) {
		// A failed append must not poison the next line in this process.
		h.prepared = ""
		_ = f.Truncate(offset)
		if err != nil {
			return err
		}
		return io.ErrShortWrite
	}
	h.lastFile = name
	return nil
}

// Remove only an incomplete final line, scanning backwards in bounded memory.
func repairTail(f *os.File) error {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for end := size; end > 0; {
		start := max(int64(0), end-int64(len(buf)))
		n, err := f.ReadAt(buf[:end-start], start)
		if err != nil && err != io.EOF {
			return err
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				return f.Truncate(start + int64(i) + 1)
			}
		}
		end = start
	}
	return f.Truncate(0)
}

func (h *history) sync() error {
	if h.lastFile == "" {
		return nil
	}
	root, err := os.OpenRoot(h.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(h.lastFile, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// ReadRecords streams complete records; a crash-truncated final line is ignored.
// Invalid complete lines and unknown schema versions are reported to the caller.
func ReadRecords(r io.Reader, visit func(Record) error) error {
	scanner := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := scanner.ReadSlice('\n')
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if len(line) > 64<<10 {
			return errors.New("metrics record too large")
		}
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			return err
		}
		if record.SchemaVersion != 1 {
			return errors.New("unsupported metrics schema")
		}
		if err := visit(record); err != nil {
			return err
		}
	}
}
