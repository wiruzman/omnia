package startupcache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/store"
)

const (
	version  = 2
	limitCap = 400
)

type fileCache struct {
	Version       int              `json:"version"`
	SortColumn    sorter.Column    `json:"sort_column"`
	SortDirection sorter.Direction `json:"sort_direction"`
	Limit         int              `json:"limit"`
	Total         int              `json:"total"`
	SavedAt       time.Time        `json:"saved_at"`
	Entries       []model.Entry    `json:"entries"`
}

func Path(cfg config.Config) string {
	return filepath.Join(cfg.DaemonDir, "startup-preview.json")
}

func EffectiveLimit(maxResults int) int {
	if maxResults <= 0 || maxResults > limitCap {
		return limitCap
	}
	return maxResults
}

func Load(path string, sortSpec sorter.SortSpec, limit int) (store.QueryResult, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store.QueryResult{}, false, nil
		}
		return store.QueryResult{}, false, err
	}

	var cache fileCache
	if err := json.Unmarshal(b, &cache); err != nil {
		return store.QueryResult{}, false, err
	}
	entries := append([]model.Entry(nil), cache.Entries...)
	if len(entries) > limit {
		entries = entries[:limit]
	}
	if cache.Version != version ||
		cache.SortColumn != sortSpec.Column ||
		cache.SortDirection != sortSpec.Direction ||
		cache.Limit != limit ||
		len(entries) == 0 ||
		!isSorted(entries, sortSpec) {
		return store.QueryResult{}, false, nil
	}

	total := cache.Total
	if total < len(entries) {
		total = len(entries)
	}
	return store.QueryResult{Entries: entries, Total: total}, true, nil
}

func Save(path string, sortSpec sorter.SortSpec, limit int, result store.QueryResult) error {
	if limit <= 0 || len(result.Entries) == 0 {
		return nil
	}
	entries := append([]model.Entry(nil), result.Entries...)
	if len(entries) > limit {
		entries = entries[:limit]
	}
	total := result.Total
	if total < len(entries) {
		total = len(entries)
	}

	cache := fileCache{
		Version:       version,
		SortColumn:    sortSpec.Column,
		SortDirection: sortSpec.Direction,
		Limit:         limit,
		Total:         total,
		SavedAt:       time.Now(),
		Entries:       entries,
	}
	b, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isSorted(entries []model.Entry, sortSpec sorter.SortSpec) bool {
	for i := 1; i < len(entries); i++ {
		if compareEntries(entries[i-1], entries[i], sortSpec) > 0 {
			return false
		}
	}
	return true
}

func compareEntries(a, b model.Entry, sortSpec sorter.SortSpec) int {
	cmp := 0
	switch sortSpec.Column {
	case sorter.SortPath:
		cmp = strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
	case sorter.SortSize:
		if a.Size < b.Size {
			cmp = -1
		} else if a.Size > b.Size {
			cmp = 1
		}
	case sorter.SortCreated:
		if a.CreatedAt.Before(b.CreatedAt) {
			cmp = -1
		} else if a.CreatedAt.After(b.CreatedAt) {
			cmp = 1
		}
	case sorter.SortModified:
		if a.ModifiedAt.Before(b.ModifiedAt) {
			cmp = -1
		} else if a.ModifiedAt.After(b.ModifiedAt) {
			cmp = 1
		}
	default:
		cmp = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	}
	if sortSpec.Direction == sorter.Desc {
		cmp = -cmp
	}
	if cmp != 0 {
		return cmp
	}
	return strings.Compare(strings.ToLower(a.Path), strings.ToLower(b.Path))
}
