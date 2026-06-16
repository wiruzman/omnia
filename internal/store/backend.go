package store

import (
	"context"

	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/sorter"
)

// Backend is the minimal storage/search contract used by app, daemon and indexer.
type Backend interface {
	BeginScan(ctx context.Context, scanID int64) error
	UpsertBatch(ctx context.Context, scanID int64, batch []model.Entry) error
	CleanupStale(ctx context.Context, scanID int64, roots []string) error
	Count(ctx context.Context) (int, error)
	CountByRoots(ctx context.Context, roots []string) (map[string]int64, error)
	HasEntries(ctx context.Context) (bool, error)
	Preview(ctx context.Context, sort sorter.SortSpec, limit int) (QueryResult, error)
	Query(ctx context.Context, query string, sort sorter.SortSpec, limit, offset int) (QueryResult, error)
	UpsertEntry(ctx context.Context, e model.Entry) error
	UpsertEntryIfChanged(ctx context.Context, e model.Entry) (UpsertEntryResult, error)
	DeletePath(ctx context.Context, path string) error
	DeletePathPrefix(ctx context.Context, dirPath string) error
	DeletePathPrefixCount(ctx context.Context, dirPath string) (int, error)
	Close() error
}

type UpsertEntryResult struct {
	Changed  bool
	Inserted bool
}
