package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/sorter"
	"github.com/wiruzman/omnia/internal/store/sqlitetest"
)

// Frozen pre-migration entry schema. No application startup or live index is used.
const legacyEntrySchema = `CREATE TABLE entries (
	path TEXT NOT NULL UNIQUE, path_lower TEXT NOT NULL,
	name TEXT NOT NULL, name_lower TEXT NOT NULL,
	parent_path TEXT NOT NULL, root_path TEXT NOT NULL,
	type TEXT NOT NULL, size INTEGER NOT NULL,
	created_at INTEGER NOT NULL, modified_at INTEGER NOT NULL,
	last_seen_scan INTEGER NOT NULL);
	CREATE INDEX entries_name_lower_idx ON entries(name_lower,path_lower);
	CREATE INDEX entries_path_lower_idx ON entries(path_lower);
	CREATE INDEX entries_modified_idx ON entries(modified_at,path_lower);
	CREATE INDEX entries_modified_desc_idx ON entries(modified_at DESC,path_lower ASC);
	CREATE INDEX entries_created_idx ON entries(created_at,path_lower);
	CREATE INDEX entries_created_desc_idx ON entries(created_at DESC,path_lower ASC);
	CREATE INDEX entries_size_idx ON entries(size,path_lower);
	CREATE INDEX entries_size_desc_idx ON entries(size DESC,path_lower ASC);
	CREATE INDEX entries_name_lower_desc_idx ON entries(name_lower DESC,path_lower ASC);
	CREATE INDEX entries_root_scan_idx ON entries(root_path,last_seen_scan);
	CREATE VIRTUAL TABLE name_fts USING fts5(name_lower,content='',contentless_delete=1,tokenize='trigram');
	CREATE VIRTUAL TABLE path_fts USING fts5(path_lower,content='',contentless_delete=1,tokenize='trigram');`

func legacyMarkerStore(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	db, err := openSQLiteDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	st := &SQLiteStore{db: db}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;" + legacyEntrySchema); err != nil {
		t.Fatal(err)
	}
	return st
}

func legacyInsertBatch(st *SQLiteStore, scanID int64, entries []model.Entry) error {
	insert, err := st.db.Prepare(`INSERT INTO entries(path,path_lower,name,name_lower,parent_path,root_path,type,size,created_at,modified_at,last_seen_scan) VALUES(?,?,?,?,?,?,?,?,?,?,?);`)
	if err != nil {
		return err
	}
	defer insert.Finalize()
	name, err := st.db.Prepare("INSERT INTO name_fts(rowid,name_lower) VALUES(?,?);")
	if err != nil {
		return err
	}
	defer name.Finalize()
	path, err := st.db.Prepare("INSERT INTO path_fts(rowid,path_lower) VALUES(?,?);")
	if err != nil {
		return err
	}
	defer path.Finalize()
	return st.db.WithTx(context.Background(), func() error {
		for _, entry := range entries {
			if err := bindInsertEntry(insert, entry); err != nil {
				return err
			}
			if err := insert.BindInt64(11, scanID); err != nil {
				return err
			}
			if err := insert.StepDone(); err != nil {
				return err
			}
			insert.Reset()
			if err := insertFTSRows(name, path, st.db.LastInsertRowID(), entry); err != nil {
				return err
			}
		}
		return nil
	})
}

func markerScalar(t *testing.T, st *SQLiteStore, sql string) int64 {
	t.Helper()
	stmt, err := st.db.Prepare(sql)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Finalize()
	row, err := stmt.Step()
	if err != nil || !row {
		t.Fatalf("query %q: row=%v err=%v", sql, row, err)
	}
	return stmt.ColumnInt64(0)
}

func assertMarkerIntegrity(t *testing.T, st *SQLiteStore) {
	t.Helper()
	if n := markerScalar(t, st, `SELECT count(*) FROM entries e LEFT JOIN scan_markers m ON m.entry_id=e.rowid WHERE m.entry_id IS NULL OR m.root_path!=e.root_path;`); n != 0 {
		t.Fatalf("missing/mismatched markers: %d", n)
	}
	if n := markerScalar(t, st, `SELECT count(*) FROM scan_markers m LEFT JOIN entries e ON e.rowid=m.entry_id WHERE e.rowid IS NULL;`); n != 0 {
		t.Fatalf("orphan markers: %d", n)
	}
}

func TestScanMarkerMigrationPreservesRowsSearchAndGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	old := legacyMarkerStore(t, path)
	entries := changeEntries(3)
	if err := legacyInsertBatch(old, 42, entries); err != nil {
		t.Fatal(err)
	}
	if err := old.db.Exec("DELETE FROM name_fts WHERE rowid=2; DELETE FROM path_fts WHERE rowid=2; DELETE FROM entries WHERE rowid=2;"); err != nil {
		t.Fatal(err)
	}
	ftsPage := markerScalar(t, old, "SELECT rootpage FROM sqlite_master WHERE name='path_fts_data';")
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	// The read-only CLI must not need to perform the writer's migration.
	legacyReader, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := legacyReader.Count(context.Background()); err != nil || n != 2 {
		t.Fatalf("legacy read-only count: %d %v", n, err)
	}
	if err := legacyReader.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assertMarkerIntegrity(t, st)
	if markerScalar(t, st, "PRAGMA user_version;") != scanMarkerSchemaVersion {
		t.Fatal("schema version not advanced")
	}
	if n := markerScalar(t, st, "SELECT count(*) FROM pragma_table_info('entries') WHERE name='last_seen_scan';"); n != 0 {
		t.Fatal("legacy column remains")
	}
	if n := markerScalar(t, st, "SELECT count(*) FROM sqlite_master WHERE name='entries_root_scan_idx';"); n != 0 {
		t.Fatal("legacy index remains")
	}
	if markerScalar(t, st, "SELECT count(*) FROM scan_markers WHERE entry_id IN(1,3) AND last_seen_scan=42;") != 2 {
		t.Fatal("rowids or generations changed")
	}
	if markerScalar(t, st, "SELECT rootpage FROM sqlite_master WHERE name='path_fts_data';") != ftsPage {
		t.Fatal("FTS was rebuilt")
	}
	result, err := st.Query(context.Background(), "000002", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil || len(result.Entries) != 1 || result.Entries[0].Path != entries[2].Path {
		t.Fatalf("migrated search: %+v %v", result, err)
	}
	if err := st.CleanupStale(context.Background(), 42, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := st.db.Exec("PRAGMA wal_autocheckpoint=0; PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		t.Fatal(err)
	}
	if err := st.setup(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() != 0 {
		t.Fatalf("repeated setup wrote WAL: info=%v err=%v", info, err)
	}
	if markerScalar(t, st, "SELECT count(*) FROM scan_markers;") != 2 {
		t.Fatal("reopen recopied markers")
	}
	reader, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if n, err := reader.Count(context.Background()); err != nil || n != 2 {
		t.Fatalf("read-only count: %d %v", n, err)
	}
}

func TestScanMarkerMigrationFailureRollsBackAndRetries(t *testing.T) {
	st := legacyMarkerStore(t, filepath.Join(t.TempDir(), "index.sqlite"))
	if err := legacyInsertBatch(st, 7, changeEntries(3)); err != nil {
		t.Fatal(err)
	}
	allow := sqlitetest.DenyAlterTable(unsafe.Pointer(st.db.db))
	err := st.setup()
	allow()
	if err == nil {
		t.Fatal("migration should fail at ALTER after copying markers")
	}
	if markerScalar(t, st, "PRAGMA user_version;") != 0 || markerScalar(t, st, "SELECT count(*) FROM entries WHERE last_seen_scan=7;") != 3 {
		t.Fatal("failed migration changed legacy data")
	}
	if markerScalar(t, st, "SELECT count(*) FROM sqlite_master WHERE name='scan_markers';") != 0 || markerScalar(t, st, "SELECT count(*) FROM sqlite_master WHERE name='entries_root_scan_idx';") != 1 {
		t.Fatal("partial migration escaped rollback")
	}
	if err := st.setup(); err != nil {
		t.Fatal(err)
	}
	assertMarkerIntegrity(t, st)
}

func TestScanMarkerMigrationAlsoSupportsOlderFTSSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	old := legacyMarkerStore(t, path)
	if err := legacyInsertBatch(old, 7, changeEntries(2)); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"name", "path"} {
		if err := old.db.Exec(fmt.Sprintf(`DROP TABLE %s_fts;
			CREATE VIRTUAL TABLE %s_fts USING fts5(%s_lower, tokenize='trigram');
			INSERT INTO %s_fts(rowid,%s_lower) SELECT rowid,%s_lower FROM entries;`, column, column, column, column, column, column)); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assertMarkerIntegrity(t, st)
	for _, table := range []string{"name_fts", "path_fts"} {
		if !strings.Contains(sqliteCreateSQL(t, st, table), "contentless_delete") {
			t.Fatal("older FTS migration was skipped")
		}
	}
	r, err := st.Query(context.Background(), "000001", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil || len(r.Entries) != 1 {
		t.Fatalf("older FTS search after migration: %+v %v", r, err)
	}
}

func TestMigratedMarkersPreserveProtectionAcrossResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite")
	old := legacyMarkerStore(t, path)
	entries := changeEntries(3)
	if err := legacyInsertBatch(old, 2, entries[:2]); err != nil {
		t.Fatal(err)
	} // Ordinary and unreadable entries already protected before interruption.
	if err := legacyInsertBatch(old, 1, entries[2:]); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBatch(context.Background(), 2, entries[:1]); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CleanupStale(context.Background(), 2, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.Count(context.Background()); err != nil || n != 2 {
		t.Fatalf("resume lost protected entries: %d %v", n, err)
	}
	assertMarkerIntegrity(t, st)
	if err := st.CleanupStale(context.Background(), 3, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
	if n, err := st.Count(context.Background()); err != nil || n != 0 {
		t.Fatalf("protection leaked into later scan: %d %v", n, err)
	}
}

func TestScanMarkerWritesAreAtomic(t *testing.T) {
	for _, operation := range []string{"insert", "rename", "root_change", "unchanged", "protect"} {
		t.Run(operation, func(t *testing.T) {
			st := changeStore(t)
			ctx := context.Background()
			entry := changeEntries(1)[0]
			if err := st.UpsertBatch(ctx, 1, []model.Entry{entry}); err != nil {
				t.Fatal(err)
			}
			if err := st.db.Exec(`CREATE TRIGGER reject_marker_insert BEFORE INSERT ON scan_markers BEGIN SELECT RAISE(ABORT,'marker failure'); END;
				CREATE TRIGGER reject_marker_update BEFORE UPDATE ON scan_markers BEGIN SELECT RAISE(ABORT,'marker failure'); END;`); err != nil {
				t.Fatal(err)
			}
			changed := entry
			switch operation {
			case "insert":
				changed.Path += "-new"
				changed.Name += "-new"
			case "rename":
				changed.Name = "newsearchneedle"
			case "root_change":
				changed.RootPath = "/other"
			}
			var err error
			if operation == "protect" {
				err = st.MarkSubtreeSeen(ctx, 2, entry.ParentPath)
			} else {
				err = st.UpsertBatch(ctx, 2, []model.Entry{changed})
			}
			if err == nil || !strings.Contains(err.Error(), "marker failure") {
				t.Fatalf("expected injected error: %v", err)
			}
			assertMarkerIntegrity(t, st)
			if markerScalar(t, st, "SELECT count(*) FROM entries;") != 1 || markerScalar(t, st, "SELECT last_seen_scan FROM scan_markers;") != 1 {
				t.Fatal("partial entry/marker write")
			}
			id, stored, exists, err := lookupEntryForTest(st, entry.Path)
			if err != nil || !exists || id != 1 || !sameIndexedEntry(stored, entry) {
				t.Fatalf("metadata changed after rollback: %+v %v", stored, err)
			}
			res, err := st.Query(ctx, "newsearchneedle", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
			if err != nil || len(res.Entries) != 0 {
				t.Fatalf("FTS escaped rollback: %+v %v", res, err)
			}
		})
	}
}

func lookupEntryForTest(st *SQLiteStore, path string) (int64, model.Entry, bool, error) {
	stmt, err := st.db.Prepare("SELECT rowid,path,name,parent_path,root_path,type,size,created_at,modified_at FROM entries WHERE path=?;")
	if err != nil {
		return 0, model.Entry{}, false, err
	}
	defer stmt.Finalize()
	return lookupEntry(stmt, path)
}

func TestMarkerRootMovesAndAllDeletePaths(t *testing.T) {
	for _, operation := range []string{"path", "prefix", "changes", "stale"} {
		t.Run(operation, func(t *testing.T) {
			st := changeStore(t)
			ctx := context.Background()
			entries := changeEntries(3)
			if err := st.UpsertBatch(ctx, 1, entries); err != nil {
				t.Fatal(err)
			}
			entries[0].RootPath = "/other"
			if err := st.UpsertBatch(ctx, 2, entries[:1]); err != nil {
				t.Fatal(err)
			}
			assertMarkerIntegrity(t, st)
			if markerScalar(t, st, "SELECT count(*) FROM scan_markers WHERE root_path='/other' AND last_seen_scan=2;") != 1 {
				t.Fatal("marker root not moved")
			}
			var err error
			switch operation {
			case "path":
				err = st.DeletePath(ctx, entries[0].Path)
			case "prefix":
				err = st.DeletePathPrefix(ctx, entries[0].ParentPath)
			case "changes":
				_, err = st.ApplyChanges(ctx, []Change{{Path: entries[0].ParentPath}}, 10)
			case "stale":
				err = st.CleanupStale(ctx, 3, []string{"/fixture", "/other"})
			}
			if err != nil {
				t.Fatal(err)
			}
			assertMarkerIntegrity(t, st)
			if n := markerScalar(t, st, "SELECT count(*) FROM scan_markers;"); operation == "path" && n != 2 || operation != "path" && n != 0 {
				t.Fatal(n)
			}
		})
	}
}

func TestMarkerDeleteFailureRollsBackEntryAndFTS(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	if err := st.UpsertBatch(ctx, 1, changeEntries(2)); err != nil {
		t.Fatal(err)
	}
	if err := st.db.Exec(`CREATE TRIGGER reject_marker_delete BEFORE DELETE ON scan_markers BEGIN SELECT RAISE(ABORT,'marker deletion failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePathPrefix(ctx, "/fixture"); err == nil {
		t.Fatal("expected delete failure")
	}
	assertMarkerIntegrity(t, st)
	if markerScalar(t, st, "SELECT count(*) FROM entries;") != 2 {
		t.Fatal("entry deletion escaped rollback")
	}
	r, err := st.Query(ctx, "000001", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil || len(r.Entries) != 1 {
		t.Fatalf("FTS deletion escaped rollback: %+v %v", r, err)
	}
}

func TestNewerIndexSchemaIsRejectedWithoutMigration(t *testing.T) {
	st := changeStore(t)
	if err := st.db.Exec(fmt.Sprintf("PRAGMA user_version=%d;", scanMarkerSchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := st.setup(); err == nil {
		t.Fatal("newer schema accepted")
	}
	if markerScalar(t, st, "PRAGMA user_version;") != scanMarkerSchemaVersion+1 {
		t.Fatal("schema version overwritten")
	}
}

func TestScanMarkerRowIDReuseDoesNotInheritProtection(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	entry := changeEntries(1)[0]
	if err := st.UpsertBatch(ctx, 7, []model.Entry{entry}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePath(ctx, entry.Path); err != nil {
		t.Fatal(err)
	}
	entry.Path += "-replacement"
	if err := st.UpsertBatch(ctx, 6, []model.Entry{entry}); err != nil {
		t.Fatal(err)
	}
	if markerScalar(t, st, "SELECT entry_id FROM scan_markers;") != 1 {
		t.Fatal("fixture did not reuse the deleted rowid")
	}
	assertMarkerIntegrity(t, st)
	if err := st.CleanupStale(ctx, 7, []string{entry.RootPath}); err != nil {
		t.Fatal(err)
	}
	if markerScalar(t, st, "SELECT count(*) FROM entries;") != 0 {
		t.Fatal("replacement inherited deleted entry's scan generation")
	}
}

func TestScanMarkerIndexesKeepCleanupAndRootCountsCovered(t *testing.T) {
	st := changeStore(t)
	for _, query := range []struct {
		sql, index string
	}{
		{`SELECT entry_id,last_seen_scan FROM scan_markers
			WHERE root_path='/fixture' AND last_seen_scan!=2 AND (last_seen_scan,entry_id)>(-1,-1)
			ORDER BY last_seen_scan,entry_id LIMIT 16384`, "scan_markers_root_scan_idx"},
		{`SELECT count(*) FROM entries WHERE root_path='/fixture'`, "entries_root_idx"},
	} {
		details := strings.Join(sqliteQueryPlanDetails(t, st, "EXPLAIN QUERY PLAN "+query.sql), " ")
		if !strings.Contains(details, "COVERING INDEX "+query.index) || strings.Contains(details, "TEMP B-TREE") {
			t.Fatalf("lost covering index: %s", details)
		}
	}
}
