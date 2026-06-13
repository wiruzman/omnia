package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
)

func TestOpenWithBackendDefaultsToSQLite(t *testing.T) {
	st, err := OpenWithBackend(filepath.Join(t.TempDir(), "index.sqlite"), "")
	if err != nil {
		t.Fatalf("open default backend: %v", err)
	}
	defer st.Close()

	if _, ok := st.(*SQLiteStore); !ok {
		t.Fatalf("expected default backend to be SQLite, got %T", st)
	}
}

func TestSQLiteInterruptOnCancelStopsRunningStatement(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	stmt, err := st.db.Prepare(`
		WITH RECURSIVE cnt(x) AS (
			VALUES(0)
			UNION ALL
			SELECT x + 1 FROM cnt WHERE x < 100000000
		)
		SELECT sum(x) FROM cnt;
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Finalize()

	ctx, cancel := context.WithCancel(context.Background())
	stopInterrupt := st.db.interruptOnCancel(ctx)
	defer stopInterrupt()

	timer := time.AfterFunc(10*time.Millisecond, cancel)
	defer timer.Stop()

	start := time.Now()
	_, err = stmt.Step()
	if err == nil {
		t.Fatal("expected canceled statement to return an error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("expected cancellation to interrupt running statement promptly, took %s", elapsed)
	}
}

func TestSQLiteStoreUpsertQueryCleanupAndReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.sqlite")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	scanID := time.Now().UnixMicro()
	if err := st.BeginScan(ctx, scanID); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	batch := []model.Entry{
		{Path: "/tmp/a.txt", Name: "a.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/b.txt", Name: "b.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 20, CreatedAt: now, ModifiedAt: now},
	}
	if err := st.UpsertBatch(ctx, scanID, batch); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "tmp/a", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Path != "/tmp/a.txt" {
		t.Fatalf("expected substring path match for /tmp/a.txt, got %+v", res.Entries)
	}

	readOnly, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only sqlite store: %v", err)
	}
	count, err := readOnly.Count(ctx)
	if closeErr := readOnly.Close(); closeErr != nil {
		t.Fatalf("close read-only sqlite store: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("count read-only sqlite store: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected read-only store to see 2 committed rows, got %d", count)
	}

	if err := st.CleanupStale(ctx, scanID+1, []string{"/tmp"}); err != nil {
		t.Fatal(err)
	}
	count, err = st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected count 0 after cleanup, got %d", count)
	}
}

func TestSQLiteStoreUpsertRefreshesFTSAndDeletePathPrefix(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	entry := model.Entry{
		Path:       "/tmp/dir/file.txt",
		Name:       "oldneedle.txt",
		ParentPath: "/tmp/dir",
		RootPath:   "/tmp",
		Type:       model.TypeFile,
		Size:       1,
		CreatedAt:  now,
		ModifiedAt: now,
	}
	if err := st.UpsertBatch(ctx, now.UnixMicro(), []model.Entry{entry}); err != nil {
		t.Fatal(err)
	}

	entry.Name = "newneedle.txt"
	entry.Size = 2
	if err := st.UpsertBatch(ctx, now.UnixMicro()+1, []model.Entry{entry}); err != nil {
		t.Fatal(err)
	}

	oldRes, err := st.Query(ctx, "oldneedle", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldRes.Entries) != 0 {
		t.Fatalf("expected old FTS term to be removed, got %+v", oldRes.Entries)
	}

	newRes, err := st.Query(ctx, "newneedle", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(newRes.Entries) != 1 || newRes.Entries[0].Size != 2 {
		t.Fatalf("expected updated FTS entry, got %+v", newRes.Entries)
	}

	if err := st.DeletePathPrefix(ctx, "/tmp/dir"); err != nil {
		t.Fatal(err)
	}
	count, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected delete prefix to remove entry, got %d", count)
	}
}
