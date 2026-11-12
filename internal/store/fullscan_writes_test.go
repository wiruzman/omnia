package store

import (
	"context"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/wiruzman/omnia/internal/config"
)

func TestFullScanDefaultBatchReducesCumulativeWrites(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	seed := shuffledWriteEntries(24000) // More than one default batch.
	ordered := slices.Clone(seed)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	var volumes []writeVolume
	for _, batchSize := range []int{1000, cfg.ScanBatchSize} {
		path := filepath.Join(t.TempDir(), "index.sqlite")
		st, err := OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		for i := 0; i < len(seed); i += 1000 {
			if err := st.UpsertBatch(ctx, 1700000000000000, seed[i:min(i+1000, len(seed))]); err != nil {
				t.Fatal(err)
			}
		}
		volume := measureWriteVolume(t, st, path, func() error {
			for i := 0; i < len(ordered); i += batchSize {
				if err := st.UpsertBatch(ctx, 1700001000000000, ordered[i:min(i+batchSize, len(ordered))]); err != nil {
					return err
				}
			}
			return nil
		})
		if n, err := st.countWhere(ctx, "last_seen_scan = ?", []any{int64(1700001000000000)}); err != nil || n != len(seed) {
			t.Fatalf("marker refresh lost entries: count=%d err=%v", n, err)
		}
		volumes = append(volumes, volume)
	}
	baseline, optimized := volumes[0], volumes[1]
	t.Logf("batch=1000 vs batch=%d; cumulative WAL bytes=%d vs %d; logical bytes=%d vs %d", cfg.ScanBatchSize, baseline.wal, optimized.wal, baseline.logical, optimized.logical)
	if optimized.wal > baseline.wal*2/3 {
		t.Fatalf("full-scan batching write reduction regressed: %v", volumes)
	}
	if baseline.hasIO && optimized.hasIO && optimized.logical > baseline.logical*2/3 {
		t.Fatalf("full-scan logical-write reduction regressed: %v", volumes)
	}
}
