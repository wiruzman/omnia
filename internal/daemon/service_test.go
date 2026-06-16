package daemon

import (
	"context"
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
	"omnia-search-tui/internal/scanner"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/startupcache"
	"omnia-search-tui/internal/store"
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

func TestLastIndexedTotalRestoresDaemonStatusTotal(t *testing.T) {
	cfg := config.Config{DaemonDir: t.TempDir()}
	if err := daemonstate.Write(cfg.DaemonStatusPath(), daemonstate.Status{IndexedTotal: 123}); err != nil {
		t.Fatalf("write status: %v", err)
	}
	svc := &Service{cfg: cfg}
	if got := svc.lastIndexedTotal(); got != 123 {
		t.Fatalf("expected restored indexed total 123, got %d", got)
	}
}

func TestApplyIndexDeltaClampsAtZero(t *testing.T) {
	if got := applyIndexDelta(10, 5); got != 15 {
		t.Fatalf("expected incremented total 15, got %d", got)
	}
	if got := applyIndexDelta(10, -3); got != 7 {
		t.Fatalf("expected decremented total 7, got %d", got)
	}
	if got := applyIndexDelta(2, -10); got != 0 {
		t.Fatalf("expected total to clamp at zero, got %d", got)
	}
}

func TestShouldTrackPathChangeSkipsExcludedAndDaemonPaths(t *testing.T) {
	root := filepath.Clean("/tmp/root")
	daemonDir := filepath.Join(root, ".daemon")
	svc := &Service{
		cfg:     config.Config{IncludePaths: []string{root}, DaemonDir: daemonDir},
		scanner: scanner.New([]string{"node_modules"}),
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{name: "normal indexed path", path: filepath.Join(root, "docs", "a.txt"), want: true},
		{name: "excluded path", path: filepath.Join(root, "node_modules", "pkg", "index.js"), want: false},
		{name: "daemon path", path: filepath.Join(daemonDir, "status.json"), want: false},
		{name: "outside root", path: "/tmp/outside/a.txt", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := svc.shouldTrackPathChange(tc.path); got != tc.want {
				t.Fatalf("shouldTrackPathChange(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestStartupPreviewPublishPolicyThrottlesDirtyPreview(t *testing.T) {
	now := time.Unix(1714000000, 0)
	svc := &Service{
		cfg: config.Config{MaxResults: 400, SortColumn: string(sorter.SortSize), SortDirection: string(sorter.Desc)},
		startupPreview: startupPreviewState{
			sortSpec:    sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc},
			limit:       startupcache.EffectiveLimit(400),
			publishedAt: now,
		},
	}

	if svc.shouldPublishStartupPreview(false, false, now.Add(2*time.Hour)) {
		t.Fatal("did not expect clean preview to publish")
	}
	if svc.shouldPublishStartupPreview(true, false, now.Add(10*time.Second)) {
		t.Fatal("did not expect dirty preview to publish before throttle interval")
	}
	if !svc.shouldPublishStartupPreview(true, false, now.Add(startupPreviewMinInterval+time.Second)) {
		t.Fatal("expected dirty preview to publish after throttle interval")
	}
	if !svc.shouldPublishStartupPreview(true, true, now.Add(10*time.Second)) {
		t.Fatal("expected forced preview to publish immediately")
	}
}

func TestStartupPreviewRelevanceUsesTopBoundary(t *testing.T) {
	now := time.Unix(1714000000, 0)
	sortSpec := sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	svc := &Service{
		cfg: config.Config{MaxResults: 2, SortColumn: string(sorter.SortSize), SortDirection: string(sorter.Desc)},
		now: func() time.Time {
			return now
		},
	}
	svc.setStartupPreviewState(sortSpec, 2, store.QueryResult{Entries: []model.Entry{
		{Path: "/root/large.bin", Name: "large.bin", Size: 100},
		{Path: "/root/medium.bin", Name: "medium.bin", Size: 50},
	}})

	if svc.entryMayAffectStartupPreview(model.Entry{Path: "/root/small.bin", Name: "small.bin", Size: 1}) {
		t.Fatal("did not expect small outside-preview entry to affect full size-desc preview")
	}
	if !svc.entryMayAffectStartupPreview(model.Entry{Path: "/root/huge.bin", Name: "huge.bin", Size: 500}) {
		t.Fatal("expected large outside-preview entry to affect full size-desc preview")
	}
	if !svc.entryMayAffectStartupPreview(model.Entry{Path: "/root/medium.bin", Name: "medium.bin", Size: 1}) {
		t.Fatal("expected changed preview member to affect preview")
	}
	if svc.deletedPathMayAffectStartupPreview("/root/untracked.bin") {
		t.Fatal("did not expect outside-preview delete to affect preview")
	}
	if !svc.deletedPathMayAffectStartupPreview("/root/large.bin") {
		t.Fatal("expected preview member delete to affect preview")
	}
}

func TestFlushPendingSkipsUnchangedEntry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "same.txt")
	if err := os.WriteFile(path, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	scan := scanner.New(nil)
	entry, err := scan.EntryFromPath(path, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}

	svc := &Service{
		cfg:     config.Config{IncludePaths: []string{root}, DaemonDir: filepath.Join(root, ".daemon")},
		store:   st,
		scanner: scan,
		logger:  log.New(io.Discard, "", 0),
	}

	stats, err := svc.flushPending(ctx, map[string]struct{}{path: {}})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Upserts != 0 || stats.Deletes != 0 || stats.Skipped != 1 || stats.PreviewRelevant {
		t.Fatalf("expected unchanged path to be skipped without preview relevance, got %+v", stats)
	}
}

func TestPublishStartupPreviewCacheUsesConfiguredSort(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
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
