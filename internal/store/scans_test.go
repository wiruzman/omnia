package store

import (
	"context"
	"errors"
	"fmt"
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
	// Unchanged entries must not rewrite any part of the wide metadata row.
	if err := st.db.Exec(`CREATE TRIGGER reject_metadata_update BEFORE UPDATE ON entries BEGIN SELECT RAISE(ABORT,'metadata rewritten'); END;`); err != nil {
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
	path := filepath.Join(t.TempDir(), "index.sqlite")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
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
	stmt, err := st.db.Prepare("SELECT count(*) FROM scan_markers WHERE last_seen_scan=2;")
	if err != nil {
		t.Fatal(err)
	}
	_, err = stmt.Step()
	seen := int(stmt.ColumnInt64(0))
	stmt.Finalize()
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
	// Each interleaved old generation spans more than one batch. Mixing an
	// entry-ID cursor with the former generation ordering would skip rows.
	entries := changeEntries(scanCleanupBatchSize*3 + 9)
	foreign := entries[0]
	foreign.Path, foreign.RootPath = "/other/keep.txt", "/other"
	if err := st.UpsertBatch(ctx, 1, append(entries, foreign)); err != nil {
		t.Fatal(err)
	}
	if err := st.db.Exec("UPDATE scan_markers SET last_seen_scan = entry_id % 3;"); err != nil {
		t.Fatal(err)
	}
	if err := st.CleanupStale(ctx, 2, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
	want := (len(entries)+1)/3 + 1
	if n, err := st.Count(ctx); err != nil || n != want {
		t.Fatalf("cursor/root filtering: count=%d want=%d err=%v", n, want, err)
	}
	assertMarkerIntegrity(t, st)
}

func TestCleanupEntryIDCursorRestartsForEachRoot(t *testing.T) {
	st := changeStore(t)
	entries := changeEntries(4)
	entries[0].Path, entries[0].RootPath = "/other/remove.dat", "/other"
	entries[3].Path, entries[3].RootPath = "/excluded/keep.dat", "/excluded"
	if err := st.UpsertBatch(context.Background(), 1, entries); err != nil {
		t.Fatal(err)
	}
	// The second root's stale entry has a lower ID than all of the first
	// root's entries; its selection must start from the initial cursor.
	if err := st.CleanupStale(context.Background(), 2, []string{"/fixture", "/other"}); err != nil {
		t.Fatal(err)
	}
	if n := markerScalar(t, st, "SELECT count(*) FROM entries WHERE root_path!='/excluded';"); n != 0 {
		t.Fatalf("cursor carried across roots: %d rows remain", n)
	}
	if n, err := st.Count(context.Background()); err != nil || n != 1 {
		t.Fatalf("excluded root lost: count=%d err=%v", n, err)
	}
	assertMarkerIntegrity(t, st)
}
