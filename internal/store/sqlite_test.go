package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/sorter"
)

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

func TestSQLiteStoreMetadataOnlyUpsertKeepsFTSRows(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	entry := model.Entry{
		Path:       "/tmp/dir/stableneedle.txt",
		Name:       "stableneedle.txt",
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

	entry.Size = 42
	entry.ModifiedAt = now.Add(time.Minute)
	if err := st.UpsertBatch(ctx, now.UnixMicro()+1, []model.Entry{entry}); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "stableneedle", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Size != 42 {
		t.Fatalf("expected metadata-only upsert to keep FTS searchability and update size, got %+v", res.Entries)
	}
}

func TestSQLiteStoreUpsertEntryIfChangedSkipsIdenticalEntry(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Unix(1714000000, 0)
	entry := model.Entry{
		Path:       "/tmp/dir/file.txt",
		Name:       "same.txt",
		ParentPath: "/tmp/dir",
		RootPath:   "/tmp",
		Type:       model.TypeFile,
		Size:       10,
		CreatedAt:  now,
		ModifiedAt: now,
	}

	result, err := st.UpsertEntryIfChanged(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.Inserted {
		t.Fatal("expected first upsert to insert entry")
	}

	result, err = st.UpsertEntryIfChanged(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || result.Inserted {
		t.Fatal("expected identical upsert to be skipped")
	}

	entry.Name = "changed.txt"
	result, err = st.UpsertEntryIfChanged(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Inserted {
		t.Fatal("expected changed entry to be updated")
	}

	oldRes, err := st.Query(ctx, "same", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldRes.Entries) != 0 {
		t.Fatalf("expected old FTS row to be removed, got %+v", oldRes.Entries)
	}
	newRes, err := st.Query(ctx, "changed", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(newRes.Entries) != 1 {
		t.Fatalf("expected updated FTS row, got %+v", newRes.Entries)
	}
}

func TestSQLiteStoreHasEntriesUsesLimitedProbe(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hasEntries, err := st.HasEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hasEntries {
		t.Fatal("expected empty store to report no entries")
	}

	now := time.Now()
	if err := st.UpsertEntry(ctx, model.Entry{
		Path:       "/tmp/a.txt",
		Name:       "a.txt",
		ParentPath: "/tmp",
		RootPath:   "/tmp",
		Type:       model.TypeFile,
		Size:       1,
		CreatedAt:  now,
		ModifiedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	hasEntries, err = st.HasEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEntries {
		t.Fatal("expected non-empty store to report entries")
	}
}

func TestSQLiteStoreDeletePathPrefixCountReportsActualDeletes(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	if err := st.UpsertBatch(ctx, now.UnixMicro(), []model.Entry{
		{Path: "/tmp/dir/a.txt", Name: "a.txt", ParentPath: "/tmp/dir", RootPath: "/tmp", Type: model.TypeFile, Size: 1, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/dir/nested/b.txt", Name: "b.txt", ParentPath: "/tmp/dir/nested", RootPath: "/tmp", Type: model.TypeFile, Size: 1, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/other.txt", Name: "other.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 1, CreatedAt: now, ModifiedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := st.DeletePathPrefixCount(ctx, "/tmp/dir")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 deleted rows, got %d", deleted)
	}

	deleted, err = st.DeletePathPrefixCount(ctx, "/tmp/dir")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("expected no-op delete to report 0 rows, got %d", deleted)
	}
}

func TestSQLiteEmptyQueryReturnsVisibleTotalWithoutFullCount(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	entries := []model.Entry{
		{Path: "/tmp/a.txt", Name: "a.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 1, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/b.txt", Name: "b.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 2, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/c.txt", Name: "c.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 3, CreatedAt: now, ModifiedAt: now},
	}
	if err := st.UpsertBatch(ctx, now.UnixMicro(), entries); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("expected limited empty-query entries, got %d", len(res.Entries))
	}
	if res.Total != len(res.Entries) {
		t.Fatalf("expected empty-query total to report visible rows without full count, got total=%d visible=%d", res.Total, len(res.Entries))
	}

	preview, err := st.Preview(ctx, sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Total != len(preview.Entries) {
		t.Fatalf("expected preview total to report visible rows without full count, got total=%d visible=%d", preview.Total, len(preview.Entries))
	}
}

func TestSQLiteEmptyQuerySortsUseIndexesWithoutTempOrderBy(t *testing.T) {
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cases := []sorter.SortSpec{
		{Column: sorter.SortName, Direction: sorter.Asc},
		{Column: sorter.SortName, Direction: sorter.Desc},
		{Column: sorter.SortPath, Direction: sorter.Asc},
		{Column: sorter.SortPath, Direction: sorter.Desc},
		{Column: sorter.SortSize, Direction: sorter.Asc},
		{Column: sorter.SortSize, Direction: sorter.Desc},
		{Column: sorter.SortCreated, Direction: sorter.Asc},
		{Column: sorter.SortCreated, Direction: sorter.Desc},
		{Column: sorter.SortModified, Direction: sorter.Asc},
		{Column: sorter.SortModified, Direction: sorter.Desc},
	}

	for _, spec := range cases {
		sql := `EXPLAIN QUERY PLAN
			SELECT e.path, e.name, e.parent_path, e.root_path, e.type, e.size, e.created_at, e.modified_at
			FROM entries e
			ORDER BY ` + sqliteOrderBy(spec) + `
			LIMIT 100 OFFSET 0;`
		details := sqliteQueryPlanDetails(t, st, sql)
		for _, detail := range details {
			if strings.Contains(detail, "USE TEMP B-TREE FOR ORDER BY") {
				t.Fatalf("expected indexed sort for %+v, plan used temp order by: %s", spec, strings.Join(details, "\n"))
			}
		}
	}
}

func sqliteQueryPlanDetails(t *testing.T, st *SQLiteStore, sql string) []string {
	t.Helper()

	stmt, err := st.db.Prepare(sql)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Finalize()

	var details []string
	for {
		row, err := stmt.Step()
		if err != nil {
			t.Fatal(err)
		}
		if !row {
			break
		}
		details = append(details, stmt.ColumnText(3))
	}
	return details
}
