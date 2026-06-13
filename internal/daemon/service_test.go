package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/daemonstate"
	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/progress"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/startupcache"
	"omnia-search-tui/internal/store"
)

func TestRootForPathPrefersMostSpecificRoot(t *testing.T) {
	roots := []string{"/Users/demo", "/Users/demo/Projects"}
	path := "/Users/demo/Projects/omnia/internal/daemon/service.go"
	got := rootForPath(roots, path)
	if got != "/Users/demo/Projects" {
		t.Fatalf("expected most specific root, got %q", got)
	}
}

func TestIsDaemonManagedPath(t *testing.T) {
	daemonDir := filepath.Clean("/Users/demo/.config/omnia-search/daemon")
	svc := &Service{cfg: config.Config{DaemonDir: daemonDir}}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{name: "daemon directory", path: daemonDir, want: true},
		{name: "daemon status file", path: filepath.Join(daemonDir, "status.json"), want: true},
		{name: "daemon subdir file", path: filepath.Join(daemonDir, "nested", "file.tmp"), want: true},
		{name: "outside daemon directory", path: "/Users/demo/Documents/file.txt", want: false},
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

func TestReadonlyIndexPathUsesDirectSQLitePath(t *testing.T) {
	sqliteSvc := &Service{cfg: config.Config{IndexDBPath: "/tmp/index.sqlite", StoreBackend: "sqlite"}}
	if got := sqliteSvc.readonlyIndexPath(); got != "/tmp/index.sqlite" {
		t.Fatalf("expected sqlite readonly path to be direct DB path, got %q", got)
	}

	bleveSvc := &Service{cfg: config.Config{IndexDBPath: "/tmp/index.bleve", StoreBackend: "bleve"}}
	if got := bleveSvc.readonlyIndexPath(); got != "/tmp/index.bleve.readonly" {
		t.Fatalf("expected bleve readonly path to be snapshot path, got %q", got)
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

func TestPublishStartupPreviewCacheUsesConfiguredSort(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.bleve"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := config.Config{
		DaemonDir:     t.TempDir(),
		MaxResults:    400,
		SortColumn:    string(sorter.SortSize),
		SortDirection: string(sorter.Desc),
	}
	svc := &Service{
		cfg:    cfg,
		store:  st,
		logger: log.New(io.Discard, "", 0),
	}

	now := time.Now()
	if err := st.UpsertBatch(ctx, now.UnixMicro(), []model.Entry{
		{Path: "/tmp/small.txt", Name: "small.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/large.txt", Name: "large.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 90, CreatedAt: now, ModifiedAt: now},
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	if err := svc.publishStartupPreviewCache(ctx); err != nil {
		t.Fatalf("publish startup cache: %v", err)
	}

	sortSpec := sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	res, ok, err := startupcache.Load(startupcache.Path(cfg), sortSpec, startupcache.EffectiveLimit(cfg.MaxResults))
	if err != nil {
		t.Fatalf("load startup cache: %v", err)
	}
	if !ok {
		t.Fatal("expected startup cache to load")
	}
	if len(res.Entries) != 2 || res.Entries[0].Name != "large.txt" {
		t.Fatalf("expected size DESC cache, got %+v", res.Entries)
	}
}
