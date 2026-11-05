package indexer

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/config"
	"github.com/wiruzman/omnia/internal/metrics"
	"github.com/wiruzman/omnia/internal/scanner"
	"github.com/wiruzman/omnia/internal/sorter"
	"github.com/wiruzman/omnia/internal/startupcache"
	"github.com/wiruzman/omnia/internal/store"
)

func TestReindexMetricsAndRecorderExclusion(t *testing.T) {
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
	cfg.ScanBatchSize = 1
	writeSizedFile(t, filepath.Join(root, "private-file.bin"), 10)
	st, err := store.OpenSQLite(cfg.IndexDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	counters := &metrics.Counters{}
	stop := metrics.Start(cfg, counters, nil)
	defer stop()
	idx := NewWithMetrics(cfg, scanner.New(nil), st, log.New(io.Discard, "", 0), counters)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatal(err)
	}
	waitForReindex(t, ctx, idx)
	for counters.Snapshot().ScanActive > 0 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	w := counters.Snapshot()
	if w.ScanRuns != 1 || w.ScanActive != 0 || w.Scanned == 0 || w.ScanSeconds <= 0 || w.PreviewBuilds == 0 || w.ScanFailures != 0 {
		t.Fatalf("missing scan metrics: %+v", w)
	}
	result, err := st.Query(ctx, "metrics-v1", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 0 {
		t.Fatal("metrics recorder files were indexed")
	}
}

func TestStartReindexWritesSortedStartupCacheFromScan(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, filepath.Join(root, "small.bin"), 1)
	writeSizedFile(t, filepath.Join(root, "large.bin"), 1024*1024)

	cfg := config.Config{
		IncludePaths:  []string{root},
		IndexDBPath:   filepath.Join(t.TempDir(), "index.sqlite"),
		MaxResults:    400,
		ScanBatchSize: 1,
		DaemonDir:     t.TempDir(),
		SortColumn:    string(sorter.SortSize),
		SortDirection: string(sorter.Desc),
	}
	st, err := store.OpenSQLite(cfg.IndexDBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	idx := New(cfg, scanner.New(nil), st, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := idx.StartReindex(ctx); err != nil {
		t.Fatalf("start reindex: %v", err)
	}

	waitForReindex(t, ctx, idx)

	sortSpec := sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	res, ok, err := startupcache.Load(startupcache.Path(cfg), sortSpec, startupcache.EffectiveLimit(cfg.MaxResults))
	if err != nil {
		t.Fatalf("load startup cache: %v", err)
	}
	if !ok {
		t.Fatal("expected startup cache to load")
	}
	if len(res.Entries) == 0 || res.Entries[0].Name != "large.bin" {
		t.Fatalf("expected largest file first in startup cache, got %+v", res.Entries)
	}
	if !startupcache.IsSorted(res.Entries, sortSpec) {
		t.Fatalf("expected startup cache to be sorted, got %+v", res.Entries)
	}
}

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func waitForReindex(t *testing.T, ctx context.Context, idx *Indexer) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for reindex: %v", ctx.Err())
		case <-ticker.C:
			st := idx.CurrentStatus()
			if st.Running || st.StartedAt.IsZero() {
				continue
			}
			if st.LastError != "" {
				t.Fatalf("reindex failed: %s", st.LastError)
			}
			return
		}
	}
}
