package store

import (
	"context"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/wiruzman/omnia/internal/config"
	"github.com/wiruzman/omnia/internal/model"
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
		stmt, err := st.db.Prepare("SELECT count(*) FROM scan_markers WHERE last_seen_scan=1700001000000000;")
		if err != nil {
			t.Fatal(err)
		}
		_, err = stmt.Step()
		n := int(stmt.ColumnInt64(0))
		stmt.Finalize()
		if err != nil || n != len(seed) {
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

func TestStaticMarkerIndexReducesCumulativeWrites(t *testing.T) {
	ctx := context.Background()
	seed := shuffledWriteEntries(24000)
	ordered := slices.Clone(seed)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	var volumes []writeVolume
	for _, versionOne := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "index.sqlite")
		st, err := OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if versionOne {
			installVersionOneMarkerIndex(t, st)
		}
		for i := 0; i < len(seed); i += 1000 {
			if err := st.UpsertBatch(ctx, 1700000000000000, seed[i:min(i+1000, len(seed))]); err != nil {
				t.Fatal(err)
			}
		}
		volumes = append(volumes, measureWriteVolume(t, st, path, func() error {
			for i := 0; i < len(ordered); i += config.DefaultScanBatchSize {
				if err := st.UpsertBatch(ctx, 1700001000000000, ordered[i:min(i+config.DefaultScanBatchSize, len(ordered))]); err != nil {
					return err
				}
			}
			return nil
		}))
		assertMarkerIntegrity(t, st)
		if n := markerScalar(t, st, "SELECT count(*) FROM scan_markers WHERE last_seen_scan=1700001000000000;"); n != int64(len(seed)) {
			t.Fatalf("marker refresh lost rows: %d", n)
		}
	}
	baseline, static := volumes[0], volumes[1]
	t.Logf("generation vs static index; cumulative WAL bytes=%d vs %d; logical bytes including checkpoint=%d vs %d", baseline.wal, static.wal, baseline.logical, static.logical)
	if static.wal > baseline.wal*2/3 || baseline.hasIO && static.hasIO && static.logical > baseline.logical*2/3 {
		t.Fatalf("static-index write reduction regressed: %v", volumes)
	}
}

// Frozen old marker refresh: same row lookup and transaction boundaries, but
// stamps the generation in the wide entry table and its root/scan index.
func legacyRefreshMarkers(st *SQLiteStore, scanID int64, batch []model.Entry) error {
	lookup, err := st.db.Prepare("SELECT rowid,path,name,parent_path,root_path,type,size,created_at,modified_at FROM entries WHERE path=?;")
	if err != nil {
		return err
	}
	defer lookup.Finalize()
	seen, err := st.db.Prepare("UPDATE entries SET last_seen_scan=? WHERE rowid=? AND last_seen_scan!=?;")
	if err != nil {
		return err
	}
	defer seen.Finalize()
	return st.db.WithTx(context.Background(), func() error {
		for _, entry := range batch {
			id, _, _, err := lookupEntry(lookup, entry.Path)
			if err != nil {
				return err
			}
			for i, value := range []int64{scanID, id, scanID} {
				if err := seen.BindInt64(i+1, value); err != nil {
					return err
				}
			}
			if err := seen.StepDone(); err != nil {
				return err
			}
			seen.Reset()
		}
		return nil
	})
}

func TestCompactScanMarkersReduceCumulativeWrites(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, layout := range []string{"scattered", "ordered"} {
		t.Run(layout, func(t *testing.T) {
			seed := shuffledWriteEntries(24000) // Cross the production batch cap.
			ordered := slices.Clone(seed)
			sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
			if layout == "ordered" {
				seed = ordered
			}
			var volumes []writeVolume
			for _, legacy := range []bool{true, false} {
				path := filepath.Join(t.TempDir(), "index.sqlite")
				var st *SQLiteStore
				if legacy {
					st = legacyMarkerStore(t, path)
				} else {
					st, err = OpenSQLite(path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { st.Close() })
				}
				for i := 0; i < len(seed); i += 1000 {
					batch := seed[i:min(i+1000, len(seed))]
					if legacy {
						err = legacyInsertBatch(st, 1700000000000000, batch)
					} else {
						err = st.UpsertBatch(ctx, 1700000000000000, batch)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				volume := measureWriteVolume(t, st, path, func() error {
					for i := 0; i < len(ordered); i += cfg.ScanBatchSize {
						batch := ordered[i:min(i+cfg.ScanBatchSize, len(ordered))]
						if legacy {
							err = legacyRefreshMarkers(st, 1700001000000000, batch)
						} else {
							err = st.UpsertBatch(ctx, 1700001000000000, batch)
						}
						if err != nil {
							return err
						}
					}
					return nil
				})
				table := "entries"
				if !legacy {
					table = "scan_markers"
					assertMarkerIntegrity(t, st)
				}
				if n := markerScalar(t, st, "SELECT count(*) FROM "+table+" WHERE last_seen_scan=1700001000000000;"); n != int64(len(seed)) {
					t.Fatalf("lost scan markers: %d", n)
				}
				volumes = append(volumes, volume)
			}
			old, compact := volumes[0], volumes[1]
			t.Logf("cumulative WAL bytes: legacy=%d compact=%d; logical bytes including final checkpoint: legacy=%d compact=%d", old.wal, compact.wal, old.logical, compact.logical)
			if compact.wal > old.wal/2 || old.hasIO && compact.hasIO && compact.logical > old.logical/2 {
				t.Fatalf("compact-marker write reduction regressed: %v", volumes)
			}
		})
	}
}
