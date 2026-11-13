package store

import (
	"context"
	"path/filepath"
	"strings"
)

// Bulk cleanup commits flush FTS/index work. Measured sparse-delete fixtures
// favor a 16K-row cap over tiny transactions; cumulative-write tests guard it.
const scanCleanupBatchSize = 16 * 1024
const scanProtectionBatchSize = 128
const firstScanCursor int64 = -1 << 63

// CleanupStale walks the static root index in entry-ID order. Generations are
// filtered from the narrow marker rows without maintaining a changing index.
// Each transaction holds at most one batch, not every stale row in a root.
// Earlier batches remain committed if a later batch fails or is canceled.
func (s *SQLiteStore) CleanupStale(ctx context.Context, scanID int64, roots []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w, err := newEntryWriter(s.db)
	if err != nil {
		return err
	}
	defer w.close()
	stmt, err := s.db.Prepare(`SELECT entry_id FROM scan_markers
		WHERE root_path = ? AND last_seen_scan != ? AND entry_id > ?
		ORDER BY entry_id LIMIT ?;`)
	if err != nil {
		return err
	}
	defer stmt.Finalize()
	for _, root := range roots {
		cursorID := firstScanCursor
		for {
			var ids []int64
			err := s.db.WithTx(ctx, func() error {
				defer stmt.Reset()
				if err := ctx.Err(); err != nil {
					return err
				}
				for i, value := range []any{filepath.Clean(root), scanID, cursorID, scanCleanupBatchSize} {
					if err := bindSQLiteAny(stmt, i+1, value); err != nil {
						return err
					}
				}
				for {
					if err := ctx.Err(); err != nil {
						return err
					}
					row, err := stmt.Step()
					if err != nil {
						return err
					}
					if !row {
						break
					}
					cursorID = stmt.ColumnInt64(0)
					ids = append(ids, cursorID)
				}
				stmt.Reset()
				for _, id := range ids {
					if err := ctx.Err(); err != nil {
						return err
					}
					if err := deleteRowByID(w.deleteName, w.deletePath, w.deleteEntry, id); err != nil {
						return err
					}
				}
				return ctx.Err()
			})
			if err != nil {
				return err
			}
			if len(ids) < scanCleanupBatchSize {
				break
			}
		}
	}
	return nil
}

// MarkSubtreeSeen protects existing rows when the scanner cannot inspect a path.
// Updating their existing scan markers also preserves protection across resume.
func (s *SQLiteStore) MarkSubtreeSeen(ctx context.Context, scanID int64, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w, err := newEntryWriter(s.db)
	if err != nil {
		return err
	}
	defer w.close()
	err = s.db.WithTx(ctx, func() error {
		id, _, exists, err := lookupEntry(w.lookup, path)
		if err != nil || !exists {
			return err
		}
		if err := w.markSeen(id, scanID); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	prefix := strings.TrimSuffix(strings.ToLower(filepath.Clean(path)), string(filepath.Separator)) + string(filepath.Separator)
	stmt, err := s.db.Prepare(`SELECT rowid, path_lower FROM entries
		WHERE path_lower >= ? AND path_lower < ? AND (path_lower, rowid) > (?, ?)
		ORDER BY path_lower, rowid LIMIT ?;`)
	if err != nil {
		return err
	}
	defer stmt.Finalize()
	cursorPath, cursorID := prefix, firstScanCursor
	for {
		var ids []int64
		err := s.db.WithTx(ctx, func() error {
			defer stmt.Reset()
			for i, value := range []any{prefix, prefixUpperBound(prefix), cursorPath, cursorID, scanProtectionBatchSize} {
				if err := bindSQLiteAny(stmt, i+1, value); err != nil {
					return err
				}
			}
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				row, err := stmt.Step()
				if err != nil {
					return err
				}
				if !row {
					break
				}
				cursorID, cursorPath = stmt.ColumnInt64(0), stmt.ColumnText(1)
				ids = append(ids, cursorID)
			}
			stmt.Reset()
			for _, id := range ids {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := w.markSeen(id, scanID); err != nil {
					return err
				}
			}
			return ctx.Err()
		})
		if err != nil {
			return err
		}
		if len(ids) < scanProtectionBatchSize {
			return nil
		}
	}
}
