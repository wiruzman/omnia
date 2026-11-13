package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/wiruzman/omnia/internal/model"
)

func measureCleanupWrites(t *testing.T, entries []model.Entry, singleTransaction bool) writeVolume {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.sqlite")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const oldScan = 1700000000000000
	const currentScan = 1700001000000000
	for i := 0; i < len(entries); i += 1000 {
		if err := st.UpsertBatch(ctx, oldScan, entries[i:min(i+1000, len(entries))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.db.Exec(fmt.Sprintf("UPDATE entries SET last_seen_scan=%d WHERE rowid%%2=0;", currentScan)); err != nil {
		t.Fatal(err)
	}
	writes := measureWriteVolume(t, st, path, func() error {
		if !singleTransaction {
			return st.CleanupStale(ctx, currentScan, []string{"/synthetic"})
		}
		// The previous single-transaction cleanup is only a test control, not
		// a production fallback. Use the same deletion order and FTS updates.
		w, err := newEntryWriter(st.db)
		if err != nil {
			return err
		}
		defer w.close()
		stmt, err := st.db.Prepare("SELECT rowid FROM entries WHERE root_path=? AND last_seen_scan!=? ORDER BY last_seen_scan,rowid;")
		if err != nil {
			return err
		}
		defer stmt.Finalize()
		return st.db.WithTx(context.Background(), func() error {
			ids, err := selectRowIDs(stmt, []any{"/synthetic", int64(currentScan)})
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := deleteRowByID(w.deleteName, w.deletePath, w.deleteEntry, id); err != nil {
					return err
				}
			}
			return nil
		})
	})
	if n, err := st.Count(ctx); err != nil || n != len(entries)/2 {
		t.Fatalf("remaining=%d err=%v", n, err)
	}
	return writes
}

func TestCleanupCumulativeWrites(t *testing.T) {
	// Include a workload crossing the transaction cap. Shuffling insertion
	// order and varying indexed metadata expose repeated secondary-index work.
	for _, stale := range []int{6000, 18000} {
		t.Run(fmt.Sprintf("stale_%d", stale), func(t *testing.T) {
			entries := shuffledWriteEntries(stale * 2)
			baseline := measureCleanupWrites(t, entries, true)
			bounded := measureCleanupWrites(t, entries, false)
			t.Logf("stale=%d cumulative WAL bytes: single=%d bounded=%d; logical bytes: single=%d bounded=%d", stale, baseline.wal, bounded.wal, baseline.logical, bounded.logical)
			// Small jobs should stay close to the single-transaction control;
			// crossing the cap permits bounded overhead, not the former 9x spike.
			numerator, denominator := int64(2), int64(1)
			if stale == 6000 {
				numerator, denominator = 5, 4
			}
			if bounded.wal > baseline.wal*numerator/denominator {
				t.Fatalf("cumulative cleanup WAL amplification: single=%d bounded=%d", baseline.wal, bounded.wal)
			}
			if baseline.hasIO && bounded.hasIO && bounded.logical > baseline.logical*uint64(numerator)/uint64(denominator) {
				t.Fatalf("cumulative logical-write amplification: single=%d bounded=%d", baseline.logical, bounded.logical)
			}
		})
	}
}
