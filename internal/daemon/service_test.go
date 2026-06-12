package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/daemonstate"
	"omnia-search-tui/internal/progress"
)

func TestRootForPathPrefersMostSpecificRoot(t *testing.T) {
	roots := []string{"/Users/mehmet", "/Users/mehmet/Projects"}
	path := "/Users/mehmet/Projects/omnia/internal/daemon/service.go"
	got := rootForPath(roots, path)
	if got != "/Users/mehmet/Projects" {
		t.Fatalf("expected most specific root, got %q", got)
	}
}

func TestIsDaemonManagedPath(t *testing.T) {
	daemonDir := filepath.Clean("/Users/mehmet/.config/omnia-search/daemon")
	svc := &Service{cfg: config.Config{DaemonDir: daemonDir}}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{name: "daemon directory", path: daemonDir, want: true},
		{name: "daemon status file", path: filepath.Join(daemonDir, "status.json"), want: true},
		{name: "daemon subdir file", path: filepath.Join(daemonDir, "nested", "file.tmp"), want: true},
		{name: "outside daemon directory", path: "/Users/mehmet/Documents/file.txt", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := svc.isDaemonManagedPath(tc.path)
			if got != tc.want {
				t.Fatalf("isDaemonManagedPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestStatusEqualIgnoresUpdatedAt(t *testing.T) {
	baseTime := time.Unix(1714000000, 0)
	a := daemonstate.Status{
		Running:      true,
		Indexing:     false,
		Scanned:      123,
		CurrentPath:  "/tmp/a",
		LastScanAt:   baseTime,
		LastError:    "",
		IndexedTotal: 42,
		UpdatedAt:    baseTime,
	}
	b := a
	b.PathProgress = append([]progress.PathProgress(nil), a.PathProgress...)
	b.UpdatedAt = baseTime.Add(10 * time.Second)

	if !statusEqual(a, b) {
		t.Fatalf("expected statuses with only UpdatedAt difference to be equal")
	}
}

func TestStatusEqualDetectsSignalChanges(t *testing.T) {
	baseTime := time.Unix(1714000000, 0)
	a := daemonstate.Status{
		Running:      true,
		Indexing:     false,
		Scanned:      1,
		CurrentPath:  "/tmp/a",
		LastScanAt:   baseTime,
		LastError:    "",
		IndexedTotal: 10,
		SnapshotSeq:  1,
	}
	b := a
	b.IndexedTotal = 11

	if statusEqual(a, b) {
		t.Fatalf("expected statuses with different indexed total to be different")
	}

	b = a
	b.PathProgress = append([]progress.PathProgress(nil), a.PathProgress...)
	b.SnapshotSeq = 2
	if statusEqual(a, b) {
		t.Fatalf("expected statuses with different snapshot sequence to be different")
	}
}

func TestStatusEqualDetectsPathProgressChanges(t *testing.T) {
	a := daemonstate.Status{
		Running:  true,
		Indexing: true,
		PathProgress: []progress.PathProgress{{
			Root:           "/tmp/a",
			Scanned:        10,
			EstimatedTotal: 100,
			Percent:        10,
			CurrentPath:    "/tmp/a/file.txt",
		}},
	}
	b := a
	b.PathProgress = append([]progress.PathProgress(nil), a.PathProgress...)

	if !statusEqual(a, b) {
		t.Fatal("expected equal statuses when path progress is identical")
	}

	b.PathProgress[0].Scanned = 11
	if statusEqual(a, b) {
		t.Fatal("expected different statuses when path progress scanned changes")
	}
}

func TestIsRetryableSnapshotError(t *testing.T) {
	if !isRetryableSnapshotError(os.ErrNotExist) {
		t.Fatal("expected os.ErrNotExist to be retryable")
	}
	if !isRetryableSnapshotError(errors.New("open /tmp/x: no such file or directory")) {
		t.Fatal("expected missing file message to be retryable")
	}
	if isRetryableSnapshotError(errors.New("permission denied")) {
		t.Fatal("expected non-missing-file error to be non-retryable")
	}
}

func TestShouldRefreshIndexedTotal(t *testing.T) {
	now := time.Unix(1714000000, 0)
	ready := now.Add(-6 * time.Second)
	notReady := now.Add(-2 * time.Second)

	cases := []struct {
		name           string
		indexing       bool
		needsRecount   bool
		lastCountAt    time.Time
		wantShouldTick bool
	}{
		{name: "indexing running and interval elapsed", indexing: true, needsRecount: false, lastCountAt: ready, wantShouldTick: true},
		{name: "needs recount and interval elapsed", indexing: false, needsRecount: true, lastCountAt: ready, wantShouldTick: true},
		{name: "neither indexing nor recount needed", indexing: false, needsRecount: false, lastCountAt: ready, wantShouldTick: false},
		{name: "interval not elapsed", indexing: true, needsRecount: true, lastCountAt: notReady, wantShouldTick: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRefreshIndexedTotal(tc.indexing, tc.needsRecount, tc.lastCountAt, now)
			if got != tc.wantShouldTick {
				t.Fatalf("shouldRefreshIndexedTotal(indexing=%v needsRecount=%v) = %v, want %v", tc.indexing, tc.needsRecount, got, tc.wantShouldTick)
			}
		})
	}
}
