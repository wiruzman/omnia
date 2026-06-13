package store

import (
	"context"
	"fmt"
	"strings"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
)

// Backend is the minimal storage/search contract used by app, daemon and indexer.
type Backend interface {
	BeginScan(ctx context.Context, scanID int64) error
	UpsertBatch(ctx context.Context, scanID int64, batch []model.Entry) error
	CleanupStale(ctx context.Context, scanID int64, roots []string) error
	Count(ctx context.Context) (int, error)
	CountByRoots(ctx context.Context, roots []string) (map[string]int64, error)
	Preview(ctx context.Context, sort sorter.SortSpec, limit int) (QueryResult, error)
	Query(ctx context.Context, query string, sort sorter.SortSpec, limit, offset int) (QueryResult, error)
	UpsertEntry(ctx context.Context, e model.Entry) error
	DeletePath(ctx context.Context, path string) error
	DeletePathPrefix(ctx context.Context, dirPath string) error
	Close() error
}

func NormalizeBackend(backend string) string {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "", "sqlite", "fts5", "sqlite_fts5", "sqlite-fts5":
		return "sqlite"
	case "bleve":
		return "bleve"
	default:
		return strings.ToLower(strings.TrimSpace(backend))
	}
}

func UsesDirectReadOnly(backend string) bool {
	return NormalizeBackend(backend) == "sqlite"
}

func OpenWithBackend(path, backend string) (Backend, error) {
	switch NormalizeBackend(backend) {
	case "sqlite":
		return OpenSQLite(path)
	case "bleve":
		return Open(path)
	default:
		return nil, fmt.Errorf("unsupported store backend: %s", backend)
	}
}

func OpenReadOnlyWithBackend(path, backend string) (Backend, error) {
	switch NormalizeBackend(backend) {
	case "sqlite":
		return OpenSQLiteReadOnly(path)
	case "bleve":
		return OpenReadOnly(path)
	default:
		return nil, fmt.Errorf("unsupported store backend: %s", backend)
	}
}
