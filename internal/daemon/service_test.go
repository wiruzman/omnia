package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/config"
	"github.com/wiruzman/omnia/internal/daemonstate"
	"github.com/wiruzman/omnia/internal/indexer"
	"github.com/wiruzman/omnia/internal/metrics"
	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/progress"
	"github.com/wiruzman/omnia/internal/scanner"
	"github.com/wiruzman/omnia/internal/sorter"
	"github.com/wiruzman/omnia/internal/startupcache"
	"github.com/wiruzman/omnia/internal/store"
)

func TestMetricsObserveIncrementalWorkWithoutPaths(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.IncludePaths = []string{root}
	cfg.DaemonDir = filepath.Join(root, ".daemon")
	cfg.IndexDBPath = filepath.Join(cfg.DaemonDir, "index.sqlite")
	cfg.MetricsEnabled = true
	cfg.ScanThrottleMs = 0
	svc, err := New(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx := context.Background()
	path := filepath.Join(root, "confidential-project-file.txt")
	if err := os.WriteFile(path, []byte("private content"), 0o600); err != nil {
		t.Fatal(err)
	}
	flush := func() {
		t.Helper()
		if _, err := svc.flushPending(ctx, map[string]bool{path: false}); err != nil {
			t.Fatal(err)
		}
	}
	flush()
	flush() // Insert, then unchanged.
	if err := os.WriteFile(path, []byte("changed private content with different size"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Omnia indexes allocated blocks and whole-second modification times.
	modified := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	flush()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	flush()
	w := svc.metrics.Snapshot()
	if w.Flushes != 4 || w.Inserted != 1 || w.Updated != 1 || w.Deleted != 1 || w.Unchanged != 1 || w.Failures != 0 || w.FlushSeconds <= 0 {
		t.Fatalf("incorrect work counts: %+v", w)
	}
	if err := svc.publishStartupPreviewCache(ctx); err != nil {
		t.Fatal(err)
	}
	if svc.metrics.Snapshot().PreviewBuilds != 1 {
		t.Fatal("preview not measured")
	}
	stop := metrics.Start(cfg, svc.metrics, nil)
	stop()
	files, err := filepath.Glob(filepath.Join(cfg.MetricsDir(), "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatal("metrics not recorded")
	}
	for _, path := range files {
		if svc.shouldTrackPathChange(path) {
			t.Fatal("recorder files would be indexed")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"confidential", "private content", root} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatal("context leaked into metrics")
			}
		}
	}
}

func TestRunningDaemonRecordsEventsAndShutdown(t *testing.T) {
	root := t.TempDir()
	// FSEvents reports canonical /private/var paths rather than /var aliases.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.IncludePaths = []string{root}
	cfg.MetricsEnabled = true
	cfg.DaemonDir = filepath.Join(root, ".daemon")
	cfg.IndexDBPath = filepath.Join(cfg.DaemonDir, "index.sqlite")
	svc, err := New(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	seed := filepath.Join(root, "seed.txt")
	if err := os.WriteFile(seed, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := svc.scanner.EntryFromPath(seed, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.UpsertEntryIfChanged(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() { runErr = svc.Run(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	// The watcher is installed after the first status, so retry writes until one is observed.
	for svc.metrics.Snapshot().Inserted == 0 && time.Now().Before(deadline) {
		if svc.metrics.Snapshot().EventsAccepted == 0 {
			if err := os.WriteFile(filepath.Join(root, "new-private-file.txt"), []byte("test"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	w := svc.metrics.Snapshot()
	if w.Inserted == 0 || w.EventsReceived == 0 || w.EventsAccepted == 0 {
		t.Fatalf("events not observed: %+v", w)
	}
	cancel()
	select {
	case <-done:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown timeout")
	}
	files, err := filepath.Glob(filepath.Join(cfg.MetricsDir(), "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var final metrics.Record
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		err = metrics.ReadRecords(f, func(record metrics.Record) error { final = record; return nil })
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if final.Work.Inserted == 0 || final.Work.Indexed == 0 || final.Work.EventsAccepted == 0 || final.Sequence < 2 {
		t.Fatalf("shutdown snapshot incomplete: %+v", final)
	}
}

type scanCountStore struct {
	store.Backend
	calls     atomic.Int32
	failFirst bool
}

func (s *scanCountStore) Count(ctx context.Context) (int, error) {
	if s.calls.Add(1) == 1 && s.failFirst {
		return 0, errors.New("temporary count failure")
	}
	return s.Backend.Count(ctx)
}

func TestFastScanRefreshesIndexedCount(t *testing.T) {
	for _, tc := range []struct {
		name      string
		metrics   bool
		shutdown  bool
		failFirst bool
	}{
		{name: "status and metrics", metrics: true},
		{name: "metrics disabled"},
		{name: "shutdown before status tick", metrics: true, shutdown: true},
		{name: "count failure retries", metrics: true, failFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Default()
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			cfg.IncludePaths = []string{root}
			cfg.DaemonDir = filepath.Join(root, ".runtime")
			cfg.IndexDBPath = filepath.Join(cfg.DaemonDir, "index.sqlite")
			cfg.MetricsEnabled = tc.metrics
			cfg.ScanThrottleMs = 0
			if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			svc, err := New(cfg, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			// Complete the entire scan before any status tick can observe it.
			// This deterministically reproduces the missed running->idle edge.
			if err := svc.indexer.StartReindex(context.Background()); err != nil {
				t.Fatal(err)
			}
			svc.indexer.Wait()
			want, err := svc.store.Count(context.Background())
			if err != nil || want != 2 {
				t.Fatalf("fixture index: count=%d error=%v", want, err)
			}
			if err := daemonstate.Write(cfg.DaemonStatusPath(), daemonstate.Status{IndexedTotal: 123}); err != nil {
				t.Fatal(err)
			}
			counted := &scanCountStore{Backend: svc.store, failFirst: tc.failFirst}
			svc.store = counted
			ctx, cancel := context.WithCancel(context.Background())
			if tc.shutdown {
				cancel() // No periodic status tick: teardown must reconcile it.
			}
			done := make(chan struct{})
			var runErr error
			go func() { runErr = svc.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			if !tc.shutdown {
				deadline := time.Now().Add(5 * time.Second)
				for {
					st, err := daemonstate.Read(cfg.DaemonStatusPath())
					if err == nil && st.IndexedTotal == want && st.SnapshotSeq == 1 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("fast scan count not refreshed: status=%+v error=%v", st, err)
					}
					if tc.failFirst && counted.calls.Load() == 1 && err == nil && st.IndexedTotal != 123 {
						t.Fatalf("failed count replaced prior estimate: %+v", st)
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
			}
			select {
			case <-done:
				if runErr != nil {
					t.Fatal(runErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("daemon shutdown timed out")
			}
			st, err := daemonstate.Read(cfg.DaemonStatusPath())
			if err != nil || st.IndexedTotal != want || st.SnapshotSeq != 1 {
				t.Fatalf("final count: status=%+v error=%v", st, err)
			}
			wantCalls := int32(1)
			if tc.failFirst {
				wantCalls++
			}
			if got := counted.calls.Load(); got != wantCalls {
				t.Fatalf("recounted completed scan: got=%d want=%d", got, wantCalls)
			}
			if tc.metrics {
				files, err := filepath.Glob(filepath.Join(cfg.MetricsDir(), "*.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				var final metrics.Record
				for _, path := range files {
					f, err := os.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					err = metrics.ReadRecords(f, func(r metrics.Record) error { final = r; return nil })
					f.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				if final.Work.Indexed != uint64(want) || final.Work.ScanActive != 0 {
					t.Fatalf("final metrics count disagrees with index: %+v", final.Work)
				}
			}
		})
	}
}

// Hold scan teardown after cancellation so the shutdown ordering is observable
// without relying on scanner speed or an artificial multi-second throttle.
type delayedScanStore struct {
	store.Backend
	entered  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (s *delayedScanStore) BeginScan(ctx context.Context, _ int64) error {
	close(s.entered)
	<-ctx.Done()
	close(s.canceled)
	<-s.release
	return ctx.Err()
}

func TestShutdownWaitsForScanCompletionBeforeFinalMetrics(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	cfg.IncludePaths = []string{t.TempDir()}
	cfg.DaemonDir = t.TempDir()
	cfg.IndexDBPath = filepath.Join(cfg.DaemonDir, "index.sqlite")
	cfg.MetricsEnabled = true
	logger := log.New(io.Discard, "", 0)
	svc, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	blocked := &delayedScanStore{Backend: svc.store, entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(blocked.release) })
	defer unblock()
	svc.store = blocked
	svc.indexer = indexer.NewWithMetrics(cfg, svc.scanner, blocked, logger, svc.metrics)
	// Use a separate context: Run must explicitly stop and join its indexer.
	if err := svc.indexer.StartReindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("scan did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() { runErr = svc.Run(ctx); close(done) }()
	defer func() {
		cancel()
		unblock()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop")
		}
	}()
	// Wait for the initial record so cancellation cannot coalesce away the
	// distinction between startup and the final snapshot.
	deadline := time.Now().Add(3 * time.Second)
	for {
		files, err := filepath.Glob(filepath.Join(cfg.MetricsDir(), "*.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) > 0 {
			info, err := os.Stat(files[0])
			if err == nil && info.Size() > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("initial metrics record not written")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-blocked.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel the indexer")
	}
	if err := svc.indexer.StartReindex(context.Background()); err == nil {
		t.Fatal("started another scan before canceled scan teardown finished")
	}
	select {
	case <-done:
		t.Fatalf("Run returned before scan teardown: %v", runErr)
	default:
	}
	unblock()
	select {
	case <-done:
		if runErr != nil {
			t.Fatal(runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish after scan teardown")
	}
	files, err := filepath.Glob(filepath.Join(cfg.MetricsDir(), "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var final metrics.Record
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		err = metrics.ReadRecords(f, func(r metrics.Record) error { final = r; return nil })
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	want := svc.metrics.Snapshot()
	if final.Sequence < 2 || final.Work.ScanRuns != 1 || final.Work.ScanActive != 0 || final.Work.ScanSeconds <= 0 || final.Work.ScanSeconds != want.ScanSeconds || final.Work.ScanFailures != 0 {
		t.Fatalf("final record missed scan completion: final=%+v counters=%+v", final.Work, want)
	}
}

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
	indexDBPath := filepath.Clean("/Users/demo/.config/omnia-search/index.sqlite")
	logPath := filepath.Clean("/Users/demo/Library/Logs/omnia-daemon.log")
	svc := &Service{cfg: config.Config{
		DaemonDir:     daemonDir,
		IndexDBPath:   indexDBPath,
		DaemonLogFile: logPath,
	}}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{name: "daemon directory", path: daemonDir, want: true},
		{name: "daemon status file", path: filepath.Join(daemonDir, "status.json"), want: true},
		{name: "daemon subdir file", path: filepath.Join(daemonDir, "nested", "file.tmp"), want: true},
		{name: "index db", path: indexDBPath, want: true},
		{name: "index db wal", path: indexDBPath + "-wal", want: true},
		{name: "index db shm", path: indexDBPath + "-shm", want: true},
		{name: "index db similarly prefixed file", path: indexDBPath + ".backup", want: false},
		{name: "external daemon log", path: logPath, want: true},
		{name: "external daemon log backup", path: logPath + ".1", want: true},
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

func TestStatusEqualIgnoresDiskIOCounters(t *testing.T) {
	a := daemonstate.Status{
		Running: true,
		DiskIO:  daemonstate.DiskIOStats{BytesRead: 100, BytesWritten: 200, ReadBytesPerSecond: 10},
	}
	b := a
	b.DiskIO = daemonstate.DiskIOStats{BytesRead: 200, BytesWritten: 400, ReadBytesPerSecond: 20}

	if !statusEqual(a, b) {
		t.Fatal("expected disk I/O changes alone not to force status writes")
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
		scanner: scanner.NewWithOptions([]string{"node_modules"}, true),
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{name: "normal indexed path", path: filepath.Join(root, "docs", "a.txt"), want: true},
		{name: "excluded path", path: filepath.Join(root, "node_modules", "pkg", "index.js"), want: false},
		{name: "package child", path: filepath.Join(root, "Example.app", "Contents", "Info.plist"), want: false},
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

	stats, err := svc.flushPending(ctx, map[string]bool{path: false})
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
