package store

import (
	"context"
	"path/filepath"
	"strings"
)

// PathsUnder returns only a bounded prefix-index page, excluding path itself.
// Callers request one extra row to detect work that needs full-scan recovery.
func (s *SQLiteStore) PathsUnder(ctx context.Context, path string, limit int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	prefix := strings.TrimSuffix(strings.ToLower(filepath.Clean(path)), string(filepath.Separator)) + string(filepath.Separator)
	stmt, err := s.db.Prepare("SELECT path FROM entries WHERE path_lower >= ? AND path_lower < ? AND path != ? ORDER BY path_lower LIMIT ?;")
	if err != nil {
		return nil, err
	}
	defer stmt.Finalize()
	stopInterrupt := s.db.interruptOnCancel(ctx)
	defer stopInterrupt()
	for i, value := range []any{prefix, prefixUpperBound(prefix), filepath.Clean(path), limit} {
		if err := bindSQLiteAny(stmt, i+1, value); err != nil {
			return nil, err
		}
	}
	var paths []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, err := stmt.Step()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
		if !row {
			return paths, nil
		}
		paths = append(paths, stmt.ColumnText(0))
	}
}
