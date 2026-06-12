package indexer

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/scanner"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/startupcache"
	"omnia-search-tui/internal/store"
)

func TestStartReindexWritesSortedStartupCacheFromScan(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, filepath.Join(root, "small.bin"), 1)
	writeSizedFile(t, filepath.Join(root, "large.bin"), 1024*1024)

	cfg := config.Config{
		IncludePaths:  []string{root},
		IndexDBPath:   filepath.Join(t.TempDir(), "index.bleve"),
		MaxResults:    400,
		ScanBatchSize: 1,
		DaemonDir:     t.TempDir(),
		SortColumn:    string(sorter.SortSize),
		SortDirection: string(sorter.Desc),
	}
	st, err := store.Open(cfg.IndexDBPath)
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
