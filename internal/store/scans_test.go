package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/sorter"
)

func TestFullScanUnchangedUpdatesOnlyScanMarker(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	entry := changeEntries(1)[0]
	if err := st.UpsertBatch(ctx, 1, []model.Entry{entry}); err != nil {
		t.Fatal(err)
	}
	// Mentioning unchanged metadata columns in UPDATE still maintains their
	// secondary indexes. Fail if a full scan touches any of those columns.
	if err := st.db.Exec(`CREATE TRIGGER reject_metadata_update BEFORE UPDATE OF path_lower,name,name_lower,parent_path,root_path,type,size,created_at,modified_at ON entries BEGIN SELECT RAISE(ABORT,'metadata rewritten'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBatch(ctx, 2, []model.Entry{entry}); err != nil {
		t.Fatal(err)
	}
	if err := st.CleanupStale(ctx, 2, []string{entry.RootPath}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.Count(ctx); err != nil || n != 1 {
		t.Fatalf("unchanged entry lost: count=%d err=%v", n, err)
	}
	// A resumed pass using the same scan ID should not rewrite even its marker.
	if err := st.db.Exec("PRAGMA wal_autocheckpoint=0; PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBatch(ctx, 2, []model.Entry{entry}); err != nil {
		t.Fatal(err)
	}
	stmt, err := st.db.Prepare("PRAGMA wal_checkpoint(PASSIVE);")
	if err != nil {
		t.Fatal(err)
	}
	row, err := stmt.Step()
	if err != nil || !row || stmt.ColumnInt64(1) != 0 {
		t.Fatalf("resumed entry rewrote WAL: row=%t err=%v", row, err)
	}
	stmt.Finalize()
	if err := st.db.Exec("DROP TRIGGER reject_metadata_update;"); err != nil {
		t.Fatal(err)
	}
	entry.Name = "changedneedle.dat"
	entry.Size++
	if err := st.UpsertBatch(ctx, 3, []model.Entry{entry}); err != nil {
		t.Fatal(err)
	}
	r, err := st.Query(ctx, "changedneedle", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil || len(r.Entries) != 1 || r.Entries[0].Size != entry.Size {
		t.Fatalf("changed metadata/FTS lost: %+v %v", r, err)
	}
}

func TestCleanupStaleCommitsBoundedBatchesAndRetries(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	n := scanCleanupBatchSize + 7
	entries := changeEntries(n)
	if err := st.UpsertBatch(ctx, 1, entries); err != nil {
		t.Fatal(err)
	}
	// Failure in batch two must roll back that batch, not the first batch.
	if err := st.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_cleanup BEFORE DELETE ON entries WHEN OLD.rowid=%d BEGIN SELECT RAISE(ABORT,'fixture failure'); END;`, scanCleanupBatchSize+3)); err != nil {
		t.Fatal(err)
	}
	if err := st.CleanupStale(ctx, 2, []string{"/fixture"}); err == nil {
		t.Fatal("expected forced cleanup failure")
	}
	if count, err := st.Count(ctx); err != nil || count != n-scanCleanupBatchSize {
		t.Fatalf("batch boundary/rollback: count=%d err=%v", count, err)
	}
	if err := st.db.Exec("DROP TRIGGER fail_cleanup;"); err != nil {
		t.Fatal(err)
	}
	if err := st.CleanupStale(ctx, 2, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
	if count, err := st.Count(ctx); err != nil || count != 0 {
		t.Fatalf("retry: count=%d err=%v", count, err)
	}
	r, err := st.Query(ctx, "file", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil || len(r.Entries) != 0 {
		t.Fatalf("stale FTS rows: %+v %v", r, err)
	}
}

type cancelAfterChecks struct {
	context.Context
	checks int
	cancel context.CancelFunc
}

func (c *cancelAfterChecks) Err() error {
	c.checks--
	if c.checks <= 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCleanupStaleCancellationRollsBackCurrentBatch(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	n := scanCleanupBatchSize + 64
	if err := st.UpsertBatch(ctx, 1, changeEntries(n)); err != nil {
		t.Fatal(err)
	}
	// Err checks cover selection, deletion and the commit boundary. Cancel
	// during batch two: batch one stays committed, batch two stays atomic.
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	canceled := &cancelAfterChecks{Context: child, checks: scanCleanupBatchSize*2 + 20, cancel: cancel}
	if err := st.CleanupStale(canceled, 2, []string{"/fixture"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if count, err := st.Count(ctx); err != nil || count != n-scanCleanupBatchSize {
		t.Fatalf("canceled batch leaked: count=%d err=%v", count, err)
	}
	if err := st.CleanupStale(ctx, 2, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
}

func TestMarkSubtreeSeenPreservesOnlySubtreeAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.sqlite")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	entries := changeEntries(scanProtectionBatchSize * 3)
	dir := entries[0]
	dir.Path = "/fixture/directory"
	dir.Name = "directory"
	dir.Type = model.TypeDirectory
	other := entries[0]
	other.Path = "/fixture/directory2/remove.txt"
	foreign := entries[0]
	foreign.Path = "/other/keep.txt"
	foreign.RootPath = "/other"
	entries = append(entries, dir, other, foreign)
	if err := st.UpsertBatch(ctx, 1, entries); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkSubtreeSeen(ctx, 2, dir.Path); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CleanupStale(ctx, 2, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
	if count, err := st.Count(ctx); err != nil || count != len(entries)-1 {
		t.Fatalf("protection scope: count=%d err=%v", count, err)
	}
	// Accessible on the following scan: normal stale cleanup resumes.
	if err := st.CleanupStale(ctx, 3, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
	if count, err := st.Count(ctx); err != nil || count != 1 {
		t.Fatalf("protection leaked across scans: count=%d err=%v", count, err)
	}
}

func TestFullScanMarkerUpdateReducesWAL(t *testing.T) {
	ctx := context.Background()
	entries := changeEntries(4000)
	var sizes []int64
	for _, legacy := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "index.sqlite")
		st, err := OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		if err := st.UpsertBatch(ctx, 1700000000000000, entries); err != nil {
			t.Fatal(err)
		}
		if err := st.db.Exec("PRAGMA wal_autocheckpoint=0; PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
			t.Fatal(err)
		}
		for start := 0; start < len(entries); start += 1000 {
			batch := entries[start:min(start+1000, len(entries))]
			if !legacy {
				if err := st.UpsertBatch(ctx, 1700001000000000, batch); err != nil {
					t.Fatal(err)
				}
				continue
			}
			w, err := newEntryWriter(st.db)
			if err != nil {
				t.Fatal(err)
			}
			err = st.db.WithTx(context.Background(), func() error {
				for _, e := range batch {
					id, _, _, err := lookupEntry(w.lookup, e.Path)
					if err != nil {
						return err
					}
					if err := bindUpdateEntry(w.update, e, 1700001000000000, id); err != nil {
						return err
					}
					if err := w.update.StepDone(); err != nil {
						return err
					}
					w.update.Reset()
				}
				return nil
			})
			w.close()
			if err != nil {
				t.Fatal(err)
			}
		}
		info, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, info.Size())
	}
	t.Logf("WAL bytes: legacy=%d marker_only=%d", sizes[0], sizes[1])
	if sizes[1] >= sizes[0]*3/4 {
		t.Fatalf("write reduction regressed: %v", sizes)
	}
}

func TestMarkSubtreeSeenCancellationIsBoundedAndRetryable(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	n := scanProtectionBatchSize * 3
	if err := st.UpsertBatch(ctx, 1, changeEntries(n)); err != nil {
		t.Fatal(err)
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	canceled := &cancelAfterChecks{Context: child, checks: scanProtectionBatchSize*3 + 20, cancel: cancel}
	if err := st.MarkSubtreeSeen(canceled, 2, "/fixture/directory"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	seen, err := st.countWhere(ctx, "last_seen_scan = ?", []any{int64(2)})
	if err != nil || seen != scanProtectionBatchSize {
		t.Fatalf("partial protection batch: seen=%d err=%v", seen, err)
	}
	if err := st.MarkSubtreeSeen(ctx, 2, "/fixture/directory"); err != nil {
		t.Fatal(err)
	}
	if err := st.CleanupStale(ctx, 2, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
	if count, err := st.Count(ctx); err != nil || count != n {
		t.Fatalf("protection retry lost entries: count=%d err=%v", count, err)
	}
}

func TestCleanupStaleCursorAcrossScanIDsAndRoots(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	entries := changeEntries(scanCleanupBatchSize*2 + 3)
	foreign := entries[0]
	foreign.Path, foreign.RootPath = "/other/keep.txt", "/other"
	if err := st.UpsertBatch(ctx, 1, append(entries, foreign)); err != nil {
		t.Fatal(err)
	}
	if err := st.db.Exec("UPDATE entries SET last_seen_scan = rowid % 3;"); err != nil {
		t.Fatal(err)
	}
	if err := st.CleanupStale(ctx, 2, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
	want := (len(entries)+1)/3 + 1
	if n, err := st.Count(ctx); err != nil || n != want {
		t.Fatalf("cursor/root filtering: count=%d want=%d err=%v", n, want, err)
	}
}
