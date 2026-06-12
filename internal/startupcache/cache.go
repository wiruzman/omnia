package startupcache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/store"
)

const (
	version  = 1
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
	if cache.Version != version ||
		cache.SortColumn != sortSpec.Column ||
		cache.SortDirection != sortSpec.Direction ||
		cache.Limit != limit ||
		len(cache.Entries) == 0 {
		return store.QueryResult{}, false, nil
	}

	entries := append([]model.Entry(nil), cache.Entries...)
	if len(entries) > limit {
		entries = entries[:limit]
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
