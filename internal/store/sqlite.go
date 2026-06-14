package store

/*
#cgo LDFLAGS: -lsqlite3
#include <sqlite3.h>
#include <stdlib.h>

static int omnia_sqlite_bind_text(sqlite3_stmt* stmt, int idx, const char* text) {
	return sqlite3_bind_text(stmt, idx, text, -1, SQLITE_TRANSIENT);
}
*/
import "C"

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
)

type SQLiteStore struct {
	db *sqliteDB
}

type sqliteDB struct {
	db *C.sqlite3
}

type sqliteStmt struct {
	db   *sqliteDB
	stmt *C.sqlite3_stmt
}

func OpenSQLite(path string) (*SQLiteStore, error) {
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return nil, fmt.Errorf("sqlite index path is a directory: %s", path)
	}

	db, err := openSQLiteDB(path, false)
	if err != nil {
		return nil, err
	}
	st := &SQLiteStore{db: db}
	if err := st.setup(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

func OpenSQLiteReadOnly(path string) (*SQLiteStore, error) {
	db, err := openSQLiteDB(filepath.Clean(path), true)
	if err != nil {
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) BeginScan(ctx context.Context, scanID int64) error {
	return ctx.Err()
}

func (s *SQLiteStore) UpsertBatch(ctx context.Context, scanID int64, batch []model.Entry) error {
	if len(batch) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	lookupStmt, err := s.db.Prepare("SELECT rowid FROM entries WHERE path = ?;")
	if err != nil {
		return err
	}
	defer lookupStmt.Finalize()

	insertStmt, err := s.db.Prepare(`INSERT INTO entries (
		path, path_lower, name, name_lower, parent_path, root_path, type, size, created_at, modified_at, last_seen_scan
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`)
	if err != nil {
		return err
	}
	defer insertStmt.Finalize()

	updateStmt, err := s.db.Prepare(`UPDATE entries SET
		path_lower = ?, name = ?, name_lower = ?, parent_path = ?, root_path = ?, type = ?,
		size = ?, created_at = ?, modified_at = ?, last_seen_scan = ?
		WHERE rowid = ?;`)
	if err != nil {
		return err
	}
	defer updateStmt.Finalize()

	deleteNameStmt, err := s.db.Prepare("DELETE FROM name_fts WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deleteNameStmt.Finalize()

	deletePathStmt, err := s.db.Prepare("DELETE FROM path_fts WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deletePathStmt.Finalize()

	insertNameStmt, err := s.db.Prepare("INSERT INTO name_fts(rowid, name_lower) VALUES (?, ?);")
	if err != nil {
		return err
	}
	defer insertNameStmt.Finalize()

	insertPathStmt, err := s.db.Prepare("INSERT INTO path_fts(rowid, path_lower) VALUES (?, ?);")
	if err != nil {
		return err
	}
	defer insertPathStmt.Finalize()

	return s.db.WithTx(func() error {
		for _, entry := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}

			rowID, exists, err := lookupRowID(lookupStmt, entry.Path)
			if err != nil {
				return err
			}

			if exists {
				if err := deleteFTSRows(deleteNameStmt, deletePathStmt, rowID); err != nil {
					return err
				}
				if err := bindUpdateEntry(updateStmt, entry, scanID, rowID); err != nil {
					return err
				}
				if err := updateStmt.StepDone(); err != nil {
					return err
				}
				updateStmt.Reset()
			} else {
				if err := bindInsertEntry(insertStmt, entry, scanID); err != nil {
					return err
				}
				if err := insertStmt.StepDone(); err != nil {
					return err
				}
				insertStmt.Reset()
				rowID = s.db.LastInsertRowID()
			}

			if err := insertFTSRows(insertNameStmt, insertPathStmt, rowID, entry); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLiteStore) CleanupStale(ctx context.Context, scanID int64, roots []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	selectStmt, err := s.db.Prepare("SELECT rowid FROM entries WHERE root_path = ? AND last_seen_scan != ?;")
	if err != nil {
		return err
	}
	defer selectStmt.Finalize()

	deleteNameStmt, err := s.db.Prepare("DELETE FROM name_fts WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deleteNameStmt.Finalize()

	deletePathStmt, err := s.db.Prepare("DELETE FROM path_fts WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deletePathStmt.Finalize()

	deleteEntryStmt, err := s.db.Prepare("DELETE FROM entries WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deleteEntryStmt.Finalize()

	return s.db.WithTx(func() error {
		for _, root := range roots {
			if err := ctx.Err(); err != nil {
				return err
			}
			rowIDs, err := selectStaleRowIDs(selectStmt, filepath.Clean(root), scanID)
			if err != nil {
				return err
			}
			for _, rowID := range rowIDs {
				if err := deleteRowByID(deleteNameStmt, deletePathStmt, deleteEntryStmt, rowID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *SQLiteStore) Count(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	count, err := s.countWhere(ctx, "", nil)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *SQLiteStore) CountByRoots(ctx context.Context, roots []string) (map[string]int64, error) {
	counts := make(map[string]int64, len(roots))
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cleanRoot := filepath.Clean(root)
		count, err := s.countWhere(ctx, "root_path = ?", []any{cleanRoot})
		if err != nil {
			return nil, err
		}
		counts[cleanRoot] = int64(count)
	}
	return counts, nil
}

func (s *SQLiteStore) Preview(ctx context.Context, sort sorter.SortSpec, limit int) (QueryResult, error) {
	entries, err := s.queryEntries(ctx, "entries e", "", nil, sort, limit, 0)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Entries: entries, Total: len(entries)}, nil
}

func (s *SQLiteStore) Query(ctx context.Context, query string, sortSpec sorter.SortSpec, limit, offset int) (QueryResult, error) {
	if err := ctx.Err(); err != nil {
		return QueryResult{}, err
	}
	plan := planQuery(query)
	qLower := plan.query
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	if qLower == "" {
		entries, err := s.queryEntries(ctx, "entries e", "", nil, sortSpec, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		return QueryResult{Entries: entries, Total: offset + len(entries)}, nil
	}

	entries := make([]model.Entry, 0, limit)
	seen := make(map[string]struct{}, limit)

	if !plan.pathLike {
		prefixName, err := s.queryPrefix(ctx, "name_lower", qLower, sortSpec, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		entries = appendUniqueEntries(entries, seen, prefixName, limit)
		if len(entries) >= limit {
			sortEntries(entries, sortSpec)
			return QueryResult{Entries: entries, Total: len(entries)}, nil
		}
		if err := ctx.Err(); err != nil {
			return QueryResult{}, err
		}
	}

	if plan.pathLike && plan.absolutePathLike {
		prefixPath, err := s.queryPrefix(ctx, "path_lower", qLower, sortSpec, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		entries = appendUniqueEntries(entries, seen, prefixPath, limit)
		if len(entries) >= limit {
			sortEntries(entries, sortSpec)
			return QueryResult{Entries: entries, Total: len(entries)}, nil
		}
		if err := ctx.Err(); err != nil {
			return QueryResult{}, err
		}
	}

	if plan.shouldStopAfterPrefix(len(entries), limit) {
		sortEntries(entries, sortSpec)
		return QueryResult{Entries: entries, Total: len(entries)}, nil
	}

	if plan.allowNameContains() {
		containsName, err := s.queryContains(ctx, "name", qLower, sortSpec, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		entries = appendUniqueEntries(entries, seen, containsName, limit)
		if len(entries) >= limit {
			sortEntries(entries, sortSpec)
			return QueryResult{Entries: entries, Total: len(entries)}, nil
		}
		if err := ctx.Err(); err != nil {
			return QueryResult{}, err
		}

		if plan.allowAllTermContains() {
			containsNameTerms, err := s.queryContainsAll(ctx, "name", plan.terms, sortSpec, limit, offset)
			if err != nil {
				return QueryResult{}, err
			}
			entries = appendUniqueEntries(entries, seen, containsNameTerms, limit)
			if len(entries) >= limit {
				sortEntries(entries, sortSpec)
				return QueryResult{Entries: entries, Total: len(entries)}, nil
			}
			if err := ctx.Err(); err != nil {
				return QueryResult{}, err
			}
		}
	}

	if !plan.allowPathContains(len(entries)) {
		sortEntries(entries, sortSpec)
		return QueryResult{Entries: entries, Total: len(entries)}, nil
	}

	if plan.pathLike || !plan.allowAllTermContains() {
		containsPath, err := s.queryContains(ctx, "path", qLower, sortSpec, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		entries = appendUniqueEntries(entries, seen, containsPath, limit)
	} else {
		containsPathTerms, err := s.queryContainsAll(ctx, "path", plan.terms, sortSpec, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		entries = appendUniqueEntries(entries, seen, containsPathTerms, limit)
	}
	sortEntries(entries, sortSpec)
	return QueryResult{Entries: entries, Total: len(entries)}, nil
}

func (s *SQLiteStore) UpsertEntry(ctx context.Context, e model.Entry) error {
	return s.UpsertBatch(ctx, time.Now().UnixMicro(), []model.Entry{e})
}

func (s *SQLiteStore) DeletePath(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	lookupStmt, err := s.db.Prepare("SELECT rowid FROM entries WHERE path = ?;")
	if err != nil {
		return err
	}
	defer lookupStmt.Finalize()

	deleteNameStmt, err := s.db.Prepare("DELETE FROM name_fts WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deleteNameStmt.Finalize()

	deletePathStmt, err := s.db.Prepare("DELETE FROM path_fts WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deletePathStmt.Finalize()

	deleteEntryStmt, err := s.db.Prepare("DELETE FROM entries WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deleteEntryStmt.Finalize()

	return s.db.WithTx(func() error {
		rowID, exists, err := lookupRowID(lookupStmt, filepath.Clean(path))
		if err != nil || !exists {
			return err
		}
		return deleteRowByID(deleteNameStmt, deletePathStmt, deleteEntryStmt, rowID)
	})
}

func (s *SQLiteStore) DeletePathPrefix(ctx context.Context, dirPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	prefix := filepath.Clean(dirPath)
	withSlash := prefix
	if !strings.HasSuffix(withSlash, string(os.PathSeparator)) {
		withSlash += string(os.PathSeparator)
	}
	where, args := prefixWhere("path", "path_lower", prefix, strings.ToLower(withSlash))

	selectSQL := "SELECT rowid FROM entries WHERE " + where + ";"
	selectStmt, err := s.db.Prepare(selectSQL)
	if err != nil {
		return err
	}
	defer selectStmt.Finalize()

	deleteNameStmt, err := s.db.Prepare("DELETE FROM name_fts WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deleteNameStmt.Finalize()

	deletePathStmt, err := s.db.Prepare("DELETE FROM path_fts WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deletePathStmt.Finalize()

	deleteEntryStmt, err := s.db.Prepare("DELETE FROM entries WHERE rowid = ?;")
	if err != nil {
		return err
	}
	defer deleteEntryStmt.Finalize()

	return s.db.WithTx(func() error {
		rowIDs, err := selectRowIDs(selectStmt, args)
		if err != nil {
			return err
		}
		for _, rowID := range rowIDs {
			if err := deleteRowByID(deleteNameStmt, deletePathStmt, deleteEntryStmt, rowID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLiteStore) setup() error {
	statements := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA temp_store = MEMORY;",
		"PRAGMA busy_timeout = 5000;",
		`CREATE TABLE IF NOT EXISTS entries (
			path TEXT NOT NULL UNIQUE,
			path_lower TEXT NOT NULL,
			name TEXT NOT NULL,
			name_lower TEXT NOT NULL,
			parent_path TEXT NOT NULL,
			root_path TEXT NOT NULL,
			type TEXT NOT NULL,
			size INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			modified_at INTEGER NOT NULL,
			last_seen_scan INTEGER NOT NULL
		);`,
		"CREATE INDEX IF NOT EXISTS entries_name_lower_idx ON entries(name_lower, path_lower);",
		"CREATE INDEX IF NOT EXISTS entries_path_lower_idx ON entries(path_lower);",
		"CREATE INDEX IF NOT EXISTS entries_modified_idx ON entries(modified_at, path_lower);",
		"CREATE INDEX IF NOT EXISTS entries_modified_desc_idx ON entries(modified_at DESC, path_lower ASC);",
		"CREATE INDEX IF NOT EXISTS entries_created_idx ON entries(created_at, path_lower);",
		"CREATE INDEX IF NOT EXISTS entries_created_desc_idx ON entries(created_at DESC, path_lower ASC);",
		"CREATE INDEX IF NOT EXISTS entries_size_idx ON entries(size, path_lower);",
		"CREATE INDEX IF NOT EXISTS entries_size_desc_idx ON entries(size DESC, path_lower ASC);",
		"CREATE INDEX IF NOT EXISTS entries_name_lower_desc_idx ON entries(name_lower DESC, path_lower ASC);",
		"CREATE INDEX IF NOT EXISTS entries_root_scan_idx ON entries(root_path, last_seen_scan);",
		"CREATE VIRTUAL TABLE IF NOT EXISTS name_fts USING fts5(name_lower, tokenize = 'trigram');",
		"CREATE VIRTUAL TABLE IF NOT EXISTS path_fts USING fts5(path_lower, tokenize = 'trigram');",
	}
	for _, statement := range statements {
		if err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) queryPrefix(ctx context.Context, field string, prefix string, sortSpec sorter.SortSpec, limit, offset int) ([]model.Entry, error) {
	upper := prefixUpperBound(prefix)
	if upper == "" {
		return s.queryEntries(ctx, "entries e", "e."+field+" >= ?", []any{prefix}, sortSpec, limit, offset)
	}
	return s.queryEntries(ctx, "entries e", "e."+field+" >= ? AND e."+field+" < ?", []any{prefix, upper}, sortSpec, limit, offset)
}

func (s *SQLiteStore) queryContains(ctx context.Context, field string, qLower string, sortSpec sorter.SortSpec, limit, offset int) ([]model.Entry, error) {
	if len([]rune(qLower)) < 3 {
		return s.queryEntries(ctx, "entries e", "e."+field+"_lower LIKE ? ESCAPE '\\'", []any{likeContainsPattern(qLower)}, sortSpec, limit, offset)
	}

	ftsTable := field + "_fts"
	from := ftsTable + " f JOIN entries e ON e.rowid = f.rowid"
	where := ftsTable + " MATCH ?"
	return s.queryEntries(ctx, from, where, []any{ftsLiteral(qLower)}, sortSpec, limit, offset)
}

func (s *SQLiteStore) queryContainsAll(ctx context.Context, field string, terms []string, sortSpec sorter.SortSpec, limit, offset int) ([]model.Entry, error) {
	ftsTable := field + "_fts"
	from := ftsTable + " f JOIN entries e ON e.rowid = f.rowid"
	where := ftsTable + " MATCH ?"
	return s.queryEntries(ctx, from, where, []any{ftsAllTerms(terms)}, sortSpec, limit, offset)
}

func (s *SQLiteStore) queryEntries(ctx context.Context, from string, where string, args []any, sortSpec sorter.SortSpec, limit, offset int) ([]model.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	sql := "SELECT e.path, e.name, e.parent_path, e.root_path, e.type, e.size, e.created_at, e.modified_at FROM " + from
	if where != "" {
		sql += " WHERE " + where
	}
	sql += " ORDER BY " + sqliteOrderBy(sortSpec) + " LIMIT ? OFFSET ?;"

	stmt, err := s.db.Prepare(sql)
	if err != nil {
		return nil, err
	}
	defer stmt.Finalize()
	stopInterrupt := s.db.interruptOnCancel(ctx)
	defer stopInterrupt()

	for i, arg := range args {
		if err := bindSQLiteAny(stmt, i+1, arg); err != nil {
			return nil, err
		}
	}
	if err := stmt.BindInt64(len(args)+1, int64(limit)); err != nil {
		return nil, err
	}
	if err := stmt.BindInt64(len(args)+2, int64(offset)); err != nil {
		return nil, err
	}

	entries := make([]model.Entry, 0, limit)
	for {
		row, err := stmt.Step()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}
		if !row {
			break
		}
		entries = append(entries, model.Entry{
			Path:       stmt.ColumnText(0),
			Name:       stmt.ColumnText(1),
			ParentPath: stmt.ColumnText(2),
			RootPath:   stmt.ColumnText(3),
			Type:       model.FileType(stmt.ColumnText(4)),
			Size:       stmt.ColumnInt64(5),
			CreatedAt:  time.Unix(stmt.ColumnInt64(6), 0),
			ModifiedAt: time.Unix(stmt.ColumnInt64(7), 0),
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func (s *SQLiteStore) countWhere(ctx context.Context, where string, args []any) (int, error) {
	sql := "SELECT count(*) FROM entries"
	if where != "" {
		sql += " WHERE " + where
	}
	sql += ";"

	stmt, err := s.db.Prepare(sql)
	if err != nil {
		return 0, err
	}
	defer stmt.Finalize()
	stopInterrupt := s.db.interruptOnCancel(ctx)
	defer stopInterrupt()

	for i, arg := range args {
		if err := bindSQLiteAny(stmt, i+1, arg); err != nil {
			return 0, err
		}
	}

	row, err := stmt.Step()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, err
	}
	if !row {
		return 0, nil
	}
	return int(stmt.ColumnInt64(0)), nil
}

func openSQLiteDB(path string, readOnly bool) (*sqliteDB, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	var db *C.sqlite3
	flags := C.int(C.SQLITE_OPEN_READWRITE | C.SQLITE_OPEN_CREATE | C.SQLITE_OPEN_FULLMUTEX)
	if readOnly {
		flags = C.int(C.SQLITE_OPEN_READONLY | C.SQLITE_OPEN_FULLMUTEX)
	}
	if rc := C.sqlite3_open_v2(cPath, &db, flags, nil); rc != C.SQLITE_OK {
		msg := "unknown sqlite open error"
		if db != nil {
			msg = C.GoString(C.sqlite3_errmsg(db))
			C.sqlite3_close(db)
		}
		return nil, fmt.Errorf("open sqlite %q: %s", path, msg)
	}

	wrapped := &sqliteDB{db: db}
	if err := wrapped.Exec("PRAGMA busy_timeout = 5000;"); err != nil {
		_ = wrapped.Close()
		return nil, err
	}
	return wrapped, nil
}

func (d *sqliteDB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	if rc := C.sqlite3_close(d.db); rc != C.SQLITE_OK {
		return d.errorf("close sqlite")
	}
	d.db = nil
	return nil
}

func (d *sqliteDB) Exec(sql string) error {
	cSQL := C.CString(sql)
	defer C.free(unsafe.Pointer(cSQL))

	var errMsg *C.char
	if rc := C.sqlite3_exec(d.db, cSQL, nil, nil, &errMsg); rc != C.SQLITE_OK {
		msg := d.errmsg()
		if errMsg != nil {
			msg = C.GoString(errMsg)
			C.sqlite3_free(unsafe.Pointer(errMsg))
		}
		return fmt.Errorf("sqlite exec failed: %s", msg)
	}
	return nil
}

func (d *sqliteDB) Prepare(sql string) (*sqliteStmt, error) {
	cSQL := C.CString(sql)
	defer C.free(unsafe.Pointer(cSQL))

	var stmt *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(d.db, cSQL, -1, &stmt, nil); rc != C.SQLITE_OK {
		return nil, d.errorf("prepare sqlite")
	}
	return &sqliteStmt{db: d, stmt: stmt}, nil
}

func (d *sqliteDB) WithTx(fn func() error) error {
	if err := d.Exec("BEGIN IMMEDIATE;"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = d.Exec("ROLLBACK;")
		}
	}()

	if err := fn(); err != nil {
		return err
	}
	if err := d.Exec("COMMIT;"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (d *sqliteDB) LastInsertRowID() int64 {
	return int64(C.sqlite3_last_insert_rowid(d.db))
}

func (d *sqliteDB) errmsg() string {
	if d == nil || d.db == nil {
		return "sqlite database is closed"
	}
	return C.GoString(C.sqlite3_errmsg(d.db))
}

func (d *sqliteDB) errorf(operation string) error {
	return fmt.Errorf("%s: %s", operation, d.errmsg())
}

func (d *sqliteDB) interruptOnCancel(ctx context.Context) func() {
	done := make(chan struct{})
	var stopped atomic.Bool
	go func() {
		select {
		case <-ctx.Done():
			if stopped.Load() {
				return
			}
			if d != nil && d.db != nil {
				C.sqlite3_interrupt(d.db)
			}
		case <-done:
		}
	}()
	return func() {
		stopped.Store(true)
		close(done)
	}
}

func (s *sqliteStmt) Finalize() {
	if s.stmt != nil {
		C.sqlite3_finalize(s.stmt)
		s.stmt = nil
	}
}

func (s *sqliteStmt) Reset() {
	C.sqlite3_reset(s.stmt)
	C.sqlite3_clear_bindings(s.stmt)
}

func (s *sqliteStmt) Step() (bool, error) {
	switch rc := C.sqlite3_step(s.stmt); rc {
	case C.SQLITE_ROW:
		return true, nil
	case C.SQLITE_DONE:
		return false, nil
	default:
		return false, s.db.errorf("step sqlite")
	}
}

func (s *sqliteStmt) StepDone() error {
	row, err := s.Step()
	if err != nil {
		return err
	}
	if row {
		return fmt.Errorf("sqlite statement unexpectedly returned a row")
	}
	return nil
}

func (s *sqliteStmt) BindText(index int, value string) error {
	cValue := C.CString(value)
	defer C.free(unsafe.Pointer(cValue))
	if rc := C.omnia_sqlite_bind_text(s.stmt, C.int(index), cValue); rc != C.SQLITE_OK {
		return s.db.errorf("bind sqlite text")
	}
	return nil
}

func (s *sqliteStmt) BindInt64(index int, value int64) error {
	if rc := C.sqlite3_bind_int64(s.stmt, C.int(index), C.sqlite3_int64(value)); rc != C.SQLITE_OK {
		return s.db.errorf("bind sqlite int64")
	}
	return nil
}

func (s *sqliteStmt) ColumnText(index int) string {
	text := C.sqlite3_column_text(s.stmt, C.int(index))
	if text == nil {
		return ""
	}
	length := C.sqlite3_column_bytes(s.stmt, C.int(index))
	return C.GoStringN((*C.char)(unsafe.Pointer(text)), length)
}

func (s *sqliteStmt) ColumnInt64(index int) int64 {
	return int64(C.sqlite3_column_int64(s.stmt, C.int(index)))
}

func lookupRowID(stmt *sqliteStmt, path string) (int64, bool, error) {
	defer stmt.Reset()
	if err := stmt.BindText(1, filepath.Clean(path)); err != nil {
		return 0, false, err
	}
	row, err := stmt.Step()
	if err != nil || !row {
		return 0, false, err
	}
	rowID := stmt.ColumnInt64(0)
	if extra, err := stmt.Step(); err != nil {
		return 0, false, err
	} else if extra {
		return 0, false, fmt.Errorf("duplicate path row found: %s", path)
	}
	return rowID, true, nil
}

func selectStaleRowIDs(stmt *sqliteStmt, root string, scanID int64) ([]int64, error) {
	defer stmt.Reset()
	if err := stmt.BindText(1, root); err != nil {
		return nil, err
	}
	if err := stmt.BindInt64(2, scanID); err != nil {
		return nil, err
	}
	return collectRowIDs(stmt)
}

func selectRowIDs(stmt *sqliteStmt, args []any) ([]int64, error) {
	defer stmt.Reset()
	for i, arg := range args {
		if err := bindSQLiteAny(stmt, i+1, arg); err != nil {
			return nil, err
		}
	}
	return collectRowIDs(stmt)
}

func collectRowIDs(stmt *sqliteStmt) ([]int64, error) {
	var rowIDs []int64
	for {
		row, err := stmt.Step()
		if err != nil {
			return nil, err
		}
		if !row {
			break
		}
		rowIDs = append(rowIDs, stmt.ColumnInt64(0))
	}
	return rowIDs, nil
}

func bindInsertEntry(stmt *sqliteStmt, entry model.Entry, scanID int64) error {
	if err := stmt.BindText(1, filepath.Clean(entry.Path)); err != nil {
		return err
	}
	if err := stmt.BindText(2, strings.ToLower(filepath.Clean(entry.Path))); err != nil {
		return err
	}
	if err := stmt.BindText(3, entry.Name); err != nil {
		return err
	}
	if err := stmt.BindText(4, strings.ToLower(entry.Name)); err != nil {
		return err
	}
	if err := stmt.BindText(5, filepath.Clean(entry.ParentPath)); err != nil {
		return err
	}
	if err := stmt.BindText(6, filepath.Clean(entry.RootPath)); err != nil {
		return err
	}
	if err := stmt.BindText(7, string(entry.Type)); err != nil {
		return err
	}
	if err := stmt.BindInt64(8, entry.Size); err != nil {
		return err
	}
	if err := stmt.BindInt64(9, entry.CreatedAt.Unix()); err != nil {
		return err
	}
	if err := stmt.BindInt64(10, entry.ModifiedAt.Unix()); err != nil {
		return err
	}
	return stmt.BindInt64(11, scanID)
}

func bindUpdateEntry(stmt *sqliteStmt, entry model.Entry, scanID int64, rowID int64) error {
	if err := stmt.BindText(1, strings.ToLower(filepath.Clean(entry.Path))); err != nil {
		return err
	}
	if err := stmt.BindText(2, entry.Name); err != nil {
		return err
	}
	if err := stmt.BindText(3, strings.ToLower(entry.Name)); err != nil {
		return err
	}
	if err := stmt.BindText(4, filepath.Clean(entry.ParentPath)); err != nil {
		return err
	}
	if err := stmt.BindText(5, filepath.Clean(entry.RootPath)); err != nil {
		return err
	}
	if err := stmt.BindText(6, string(entry.Type)); err != nil {
		return err
	}
	if err := stmt.BindInt64(7, entry.Size); err != nil {
		return err
	}
	if err := stmt.BindInt64(8, entry.CreatedAt.Unix()); err != nil {
		return err
	}
	if err := stmt.BindInt64(9, entry.ModifiedAt.Unix()); err != nil {
		return err
	}
	if err := stmt.BindInt64(10, scanID); err != nil {
		return err
	}
	return stmt.BindInt64(11, rowID)
}

func insertFTSRows(nameStmt, pathStmt *sqliteStmt, rowID int64, entry model.Entry) error {
	nameLower := strings.ToLower(entry.Name)
	pathLower := strings.ToLower(filepath.Clean(entry.Path))

	if err := nameStmt.BindInt64(1, rowID); err != nil {
		return err
	}
	if err := nameStmt.BindText(2, nameLower); err != nil {
		return err
	}
	if err := nameStmt.StepDone(); err != nil {
		return err
	}
	nameStmt.Reset()

	if err := pathStmt.BindInt64(1, rowID); err != nil {
		return err
	}
	if err := pathStmt.BindText(2, pathLower); err != nil {
		return err
	}
	if err := pathStmt.StepDone(); err != nil {
		return err
	}
	pathStmt.Reset()
	return nil
}

func deleteFTSRows(nameStmt, pathStmt *sqliteStmt, rowID int64) error {
	if err := nameStmt.BindInt64(1, rowID); err != nil {
		return err
	}
	if err := nameStmt.StepDone(); err != nil {
		return err
	}
	nameStmt.Reset()

	if err := pathStmt.BindInt64(1, rowID); err != nil {
		return err
	}
	if err := pathStmt.StepDone(); err != nil {
		return err
	}
	pathStmt.Reset()
	return nil
}

func deleteRowByID(nameStmt, pathStmt, entryStmt *sqliteStmt, rowID int64) error {
	if err := deleteFTSRows(nameStmt, pathStmt, rowID); err != nil {
		return err
	}
	if err := entryStmt.BindInt64(1, rowID); err != nil {
		return err
	}
	if err := entryStmt.StepDone(); err != nil {
		return err
	}
	entryStmt.Reset()
	return nil
}

func bindSQLiteAny(stmt *sqliteStmt, index int, value any) error {
	switch typed := value.(type) {
	case string:
		return stmt.BindText(index, typed)
	case int:
		return stmt.BindInt64(index, int64(typed))
	case int64:
		return stmt.BindInt64(index, typed)
	default:
		return fmt.Errorf("unsupported sqlite bind value %T", value)
	}
}

func sqliteOrderBy(spec sorter.SortSpec) string {
	direction := "ASC"
	if spec.Direction == sorter.Desc {
		direction = "DESC"
	}

	field := "name_lower"
	switch spec.Column {
	case sorter.SortName:
		field = "name_lower"
	case sorter.SortPath:
		field = "path_lower"
	case sorter.SortSize:
		field = "size"
	case sorter.SortCreated:
		field = "created_at"
	case sorter.SortModified:
		field = "modified_at"
	}
	if field == "path_lower" {
		return "e." + field + " " + direction
	}
	return "e." + field + " " + direction + ", e.path_lower ASC"
}

func prefixWhere(exactField string, lowerField string, exact string, lowerPrefix string) (string, []any) {
	upper := prefixUpperBound(lowerPrefix)
	if upper == "" {
		return exactField + " = ? OR " + lowerField + " >= ?", []any{exact, lowerPrefix}
	}
	return exactField + " = ? OR (" + lowerField + " >= ? AND " + lowerField + " < ?)", []any{exact, lowerPrefix, upper}
}

func prefixUpperBound(prefix string) string {
	if prefix == "" {
		return ""
	}
	bytes := []byte(prefix)
	for i := len(bytes) - 1; i >= 0; i-- {
		if bytes[i] < 0xff {
			bytes[i]++
			return string(bytes[:i+1])
		}
	}
	return ""
}

func ftsLiteral(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func ftsAllTerms(terms []string) string {
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		parts = append(parts, ftsLiteral(term))
	}
	return strings.Join(parts, " AND ")
}

func likeContainsPattern(value string) string {
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('%')
	for _, r := range value {
		switch r {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String()
}
