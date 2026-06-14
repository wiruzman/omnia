package startupcache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/sorter"
	"github.com/wiruzman/omnia/internal/store"
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

func TestLoadAcceptsSortedPreviousVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "startup-preview.json")
	sortSpec := sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	now := time.Now()
	cache := fileCache{
		Version:       1,
		SortColumn:    sorter.SortSize,
		SortDirection: sorter.Desc,
		Limit:         400,
		Total:         2,
		SavedAt:       now,
		Entries: []model.Entry{
			{Path: "/tmp/large.txt", Name: "large.txt", Size: 90, CreatedAt: now, ModifiedAt: now},
			{Path: "/tmp/small.txt", Name: "small.txt", Size: 10, CreatedAt: now, ModifiedAt: now},
		},
	}
	b, err := json.Marshal(cache)
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	loaded, ok, err := Load(path, sortSpec, 400)
	if err != nil {
		t.Fatalf("load startup cache: %v", err)
	}
	if !ok {
		t.Fatal("expected sorted previous-version cache to load")
	}
	if len(loaded.Entries) != 2 || loaded.Entries[0].Name != "large.txt" {
		t.Fatalf("expected previous-version cache in size DESC order, got %+v", loaded.Entries)
	}
}

func TestLoadRejectsUnsortedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "startup-preview.json")
	sortSpec := sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	now := time.Now()
	result := store.QueryResult{
		Entries: []model.Entry{
			{Path: "/tmp/small.txt", Name: "small.txt", Size: 10, CreatedAt: now, ModifiedAt: now},
			{Path: "/tmp/large.txt", Name: "large.txt", Size: 90, CreatedAt: now, ModifiedAt: now},
		},
		Total: 2,
	}

	if err := Save(path, sortSpec, 400, result); err != nil {
		t.Fatalf("save startup cache: %v", err)
	}

	_, ok, err := Load(path, sortSpec, 400)
	if err != nil {
		t.Fatalf("load startup cache: %v", err)
	}
	if ok {
		t.Fatal("expected unsorted cache to be ignored")
	}
}

func TestTopKeepsSortedBestEntriesForConfiguredSort(t *testing.T) {
	sortSpec := sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	top := NewTop(sortSpec, 3)
	now := time.Now()

	top.AddBatch([]model.Entry{
		{Path: "/tmp/10.txt", Name: "10.txt", Size: 10, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/90.txt", Name: "90.txt", Size: 90, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/30.txt", Name: "30.txt", Size: 30, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/70.txt", Name: "70.txt", Size: 70, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/20.txt", Name: "20.txt", Size: 20, CreatedAt: now, ModifiedAt: now},
	})

	result := top.Result(5)
	if got := names(result.Entries); len(got) != 3 || got[0] != "90.txt" || got[1] != "70.txt" || got[2] != "30.txt" {
		t.Fatalf("expected top size DESC entries, got %v", got)
	}
	if !IsSorted(result.Entries, sortSpec) {
		t.Fatalf("expected result to be sorted, got %+v", result.Entries)
	}
	if result.Total != 5 {
		t.Fatalf("expected total to be preserved, got %d", result.Total)
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

func names(entries []model.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name)
	}
	return out
}
