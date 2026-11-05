package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetentionWindowsAndForeignFiles(t *testing.T) {
	for _, days := range []int{30, 90, 180} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			h := history{dir: t.TempDir(), days: days, maxBytes: 1 << 20}
			for _, age := range []int{days + 2, days + 1, days, days - 1} {
				day := now.AddDate(0, 0, -age).Format("2006-01-02")
				if err := os.WriteFile(filepath.Join(h.dir, "metrics-v1-"+day+"-0000.jsonl"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			foreign := filepath.Join(h.dir, "user-notes.jsonl")
			if err := os.WriteFile(foreign, []byte("do not remove"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := h.append(now, []byte("{}\n")); err != nil {
				t.Fatal(err)
			}
			files, err := h.files()
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 3 {
				t.Fatalf("wrong retained count: %d", len(files))
			}
			if _, err := os.Stat(foreign); err != nil {
				t.Fatal("foreign file deleted")
			}
		})
	}
}

func TestSizeCapAndTailRepair(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	h := history{dir: t.TempDir(), days: 180, maxBytes: 20}
	if err := h.append(now.AddDate(0, 0, -1), []byte("123456789\n")); err != nil {
		t.Fatal(err)
	}
	if err := h.append(now, []byte("123456789\n")); err != nil {
		t.Fatal(err)
	}
	if err := h.append(now, []byte("123456789\n")); err != nil {
		t.Fatal(err)
	}
	files, err := h.files()
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, f := range files {
		size += f.size
	}
	if size > 20 || len(files) != 1 {
		t.Fatalf("cap not enforced: %d %+v", size, files)
	}
	if h.append(now, []byte(strings.Repeat("x", 21))) == nil {
		t.Fatal("oversized record accepted")
	}
	// Simulate an interrupted append, then reopen as a fresh daemon would.
	h.maxBytes = 1 << 20
	path := filepath.Join(h.dir, files[0].name)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("partial-without-newline"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	h.prepared = ""
	if err := h.append(now, []byte("good\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "partial") || !strings.HasSuffix(string(data), "good\n") {
		t.Fatalf("tail not repaired: %q", data)
	}
}

func TestHistoryRefusesSymlinks(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := history{dir: t.TempDir(), days: 180, maxBytes: 1000}
	if err := os.Symlink(outside, filepath.Join(h.dir, "metrics-v1-2026-10-29-0000.jsonl")); err != nil {
		t.Fatal(err)
	}
	if h.append(now, []byte("{}\n")) == nil {
		t.Fatal("followed metrics symlink")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "untouched" {
		t.Fatal("outside file changed")
	}
	link := filepath.Join(t.TempDir(), "metrics-link")
	if err := os.Symlink(h.dir, link); err != nil {
		t.Fatal(err)
	}
	h.dir = link
	if h.append(now, []byte("{}\n")) == nil {
		t.Fatal("followed directory symlink")
	}
}

func TestRotationUsesUTCAndSurvivesBackwardDay(t *testing.T) {
	zone := time.FixedZone("local", -7*3600)
	now := time.Date(2026, 10, 29, 20, 0, 0, 0, zone)
	h := history{dir: t.TempDir(), days: 180, maxBytes: 1000}
	if err := h.append(now, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "metrics-v1-2026-10-30-0000.jsonl")); err != nil {
		t.Fatal("rotation used local time")
	}
	if err := h.append(now.Add(-24*time.Hour), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	files, err := h.files()
	if err != nil || len(files) != 2 {
		t.Fatalf("backward rotation: %+v %v", files, err)
	}
}

func TestLargeDayRotatesParts(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	h := history{dir: t.TempDir(), days: 180, maxBytes: 32 << 20}
	path := filepath.Join(h.dir, "metrics-v1-2026-10-29-0000.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse fixture exercises the logical shard limit without writing 8 MiB.
	if err := f.Truncate(8 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := h.append(now, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "metrics-v1-2026-10-29-0001.jsonl")); err != nil {
		t.Fatal("large day did not rotate")
	}
}
