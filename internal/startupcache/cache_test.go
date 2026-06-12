package startupcache

import (
	"path/filepath"
	"testing"
	"time"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/store"
)

func TestSaveLoadReturnsSortedCachedEntriesForMatchingSort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "startup-preview.json")
	sortSpec := sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	now := time.Now()
	result := store.QueryResult{
		Entries: []model.Entry{
			{Path: "/tmp/large.txt", Name: "large.txt", Size: 90, CreatedAt: now, ModifiedAt: now},
			{Path: "/tmp/small.txt", Name: "small.txt", Size: 10, CreatedAt: now, ModifiedAt: now},
		},
		Total: 2,
	}

	if err := Save(path, sortSpec, 400, result); err != nil {
		t.Fatalf("save startup cache: %v", err)
	}

	loaded, ok, err := Load(path, sortSpec, 400)
	if err != nil {
		t.Fatalf("load startup cache: %v", err)
	}
	if !ok {
		t.Fatal("expected matching cache to load")
	}
	if len(loaded.Entries) != 2 || loaded.Entries[0].Name != "large.txt" {
		t.Fatalf("expected cached size DESC order, got %+v", loaded.Entries)
	}
}

func TestLoadRejectsMismatchedSort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "startup-preview.json")
	sortSpec := sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	now := time.Now()
	result := store.QueryResult{
		Entries: []model.Entry{{Path: "/tmp/large.txt", Name: "large.txt", Size: 90, CreatedAt: now, ModifiedAt: now}},
		Total:   1,
	}

	if err := Save(path, sortSpec, 400, result); err != nil {
		t.Fatalf("save startup cache: %v", err)
	}

	_, ok, err := Load(path, sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 400)
	if err != nil {
		t.Fatalf("load startup cache: %v", err)
	}
	if ok {
		t.Fatal("expected mismatched sort cache to be ignored")
	}
}

func TestEffectiveLimitCapsLargeResultSets(t *testing.T) {
	if got := EffectiveLimit(5000); got != 400 {
		t.Fatalf("expected large max_results to cap at 400, got %d", got)
	}
	if got := EffectiveLimit(120); got != 120 {
		t.Fatalf("expected small max_results to stay unchanged, got %d", got)
	}
}
