package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/wiruzman/omnia/internal/model"
)

// entryWriter reuses statements for one transaction, for both full scans and
// incremental changes. It owns no connection, queue, or background worker.
type entryWriter struct {
	lookup                 *sqliteStmt
	insert, update         *sqliteStmt
	seen                   *sqliteStmt
	deleteName, deletePath *sqliteStmt
	insertName, insertPath *sqliteStmt
	prefixes, deleteEntry  *sqliteStmt
	statements             []*sqliteStmt
}

func newEntryWriter(db *sqliteDB) (*entryWriter, error) {
	w := &entryWriter{}
	for _, statement := range []struct {
		target **sqliteStmt
		sql    string
	}{
		{&w.lookup, "SELECT rowid, path, name, parent_path, root_path, type, size, created_at, modified_at FROM entries WHERE path = ?;"},
		{&w.insert, `INSERT INTO entries (path, path_lower, name, name_lower, parent_path, root_path, type, size, created_at, modified_at, last_seen_scan) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`},
		{&w.update, `UPDATE entries SET path_lower = ?, name = ?, name_lower = ?, parent_path = ?, root_path = ?, type = ?, size = ?, created_at = ?, modified_at = ?, last_seen_scan = ? WHERE rowid = ?;`},
		{&w.seen, "UPDATE entries SET last_seen_scan = ? WHERE rowid = ? AND last_seen_scan != ?;"},
		{&w.deleteName, "DELETE FROM name_fts WHERE rowid = ?;"},
		{&w.deletePath, "DELETE FROM path_fts WHERE rowid = ?;"},
		{&w.insertName, "INSERT INTO name_fts(rowid, name_lower) VALUES (?, ?);"},
		{&w.insertPath, "INSERT INTO path_fts(rowid, path_lower) VALUES (?, ?);"},
		{&w.prefixes, "SELECT rowid FROM entries WHERE path = ? OR (path_lower >= ? AND path_lower < ?) LIMIT ?;"},
		{&w.deleteEntry, "DELETE FROM entries WHERE rowid = ?;"},
	} {
		stmt, err := db.Prepare(statement.sql)
		if err != nil {
			w.close()
			return nil, err
		}
		*statement.target = stmt
		w.statements = append(w.statements, stmt)
	}
	return w, nil
}

func (w *entryWriter) close() {
	for _, stmt := range w.statements {
		stmt.Finalize()
	}
}

func (w *entryWriter) upsert(entry model.Entry, scanID int64, skipUnchanged bool) (UpsertEntryResult, error) {
	rowID, existing, exists, err := lookupEntry(w.lookup, entry.Path)
	if err != nil {
		return UpsertEntryResult{}, err
	}
	if exists && sameIndexedEntry(existing, entry) {
		if skipUnchanged {
			return UpsertEntryResult{}, nil
		}
		return UpsertEntryResult{}, w.markSeen(rowID, scanID)
	}
	refreshFTS := !exists || entryNeedsFTSRefresh(existing, entry)
	if exists {
		if refreshFTS {
			if err := deleteFTSRows(w.deleteName, w.deletePath, rowID); err != nil {
				return UpsertEntryResult{}, err
			}
		}
		if err := bindUpdateEntry(w.update, entry, scanID, rowID); err != nil {
			return UpsertEntryResult{}, err
		}
		if err := w.update.StepDone(); err != nil {
			return UpsertEntryResult{}, err
		}
		w.update.Reset()
	} else {
		if err := bindInsertEntry(w.insert, entry, scanID); err != nil {
			return UpsertEntryResult{}, err
		}
		if err := w.insert.StepDone(); err != nil {
			return UpsertEntryResult{}, err
		}
		w.insert.Reset()
		rowID = w.insert.db.LastInsertRowID()
	}
	if refreshFTS {
		if err := insertFTSRows(w.insertName, w.insertPath, rowID, entry); err != nil {
			return UpsertEntryResult{}, err
		}
	}
	return UpsertEntryResult{Changed: true, Inserted: !exists}, nil
}

func (w *entryWriter) markSeen(rowID, scanID int64) error {
	defer w.seen.Reset()
	if err := w.seen.BindInt64(1, scanID); err != nil {
		return err
	}
	if err := w.seen.BindInt64(2, rowID); err != nil {
		return err
	}
	if err := w.seen.BindInt64(3, scanID); err != nil {
		return err
	}
	return w.seen.StepDone()
}

func (w *entryWriter) remove(path string, limit int) (ChangeResult, error) {
	path = filepath.Clean(path)
	prefix := strings.ToLower(path)
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	ids, err := selectRowIDs(w.prefixes, []any{path, prefix, prefixUpperBound(prefix), limit + 1})
	if err != nil {
		return ChangeResult{}, err
	}
	result := ChangeResult{Complete: len(ids) <= limit}
	for _, id := range ids[:min(len(ids), limit)] {
		if err := deleteRowByID(w.deleteName, w.deletePath, w.deleteEntry, id); err != nil {
			return ChangeResult{}, err
		}
		result.Deleted++
	}
	return result, nil
}

// ApplyChanges commits a bounded amount of work atomically, including prefix
// deletions. An error returns no results; callers retain all paths for retry.
func (s *SQLiteStore) ApplyChanges(ctx context.Context, changes []Change, limit int) ([]ChangeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("change limit must be positive")
	}
	if len(changes) == 0 {
		return nil, nil
	}
	w, err := newEntryWriter(s.db)
	if err != nil {
		return nil, err
	}
	defer w.close()
	var results []ChangeResult
	scanID := time.Now().UnixMicro()
	err = s.db.WithTx(ctx, func() error {
		for _, change := range changes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if limit == 0 {
				break
			}
			result := ChangeResult{Complete: true}
			var err error
			if change.Entry == nil {
				result, err = w.remove(change.Path, limit)
			} else {
				result.UpsertEntryResult, err = w.upsert(*change.Entry, scanID, true)
			}
			if err != nil {
				return err
			}
			results = append(results, result)
			limit -= max(1, result.Deleted)
			if !result.Complete {
				break
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}
