package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/sorter"
)

func changeEntries(n int) []model.Entry {
	entries := make([]model.Entry, n)
	for i := range entries {
		path := fmt.Sprintf("/fixture/directory/file-%06d.dat", i)
		entries[i] = model.Entry{Path: path, Name: filepath.Base(path), ParentPath: filepath.Dir(path), RootPath: "/fixture", Type: model.TypeFile, Size: 4096, CreatedAt: time.Unix(100, 0), ModifiedAt: time.Unix(100, 0)}
	}
	return entries
}

func changeStore(t *testing.T) *SQLiteStore {
	t.Helper()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestApplyChangesBoundsDeletesAndPreservesAdjacentPrefixes(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	entries := changeEntries(40)
	sibling := entries[0]
	sibling.Path = "/fixture/directory2/keep.dat"
	entries = append(entries, sibling)
	if err := st.UpsertBatch(ctx, 1, entries); err != nil {
		t.Fatal(err)
	}
	deleted := 0
	for pass := 0; ; pass++ {
		if pass > 10 {
			t.Fatal("prefix deletion did not finish")
		}
		results, err := st.ApplyChanges(ctx, []Change{{Path: "/fixture/directory"}}, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].Deleted > 7 {
			t.Fatalf("unbounded delete: %+v", results)
		}
		deleted += results[0].Deleted
		if results[0].Complete {
			break
		}
	}
	count, err := st.Count(ctx)
	if err != nil || count != 1 || deleted != 40 {
		t.Fatalf("deleted=%d remaining=%d err=%v", deleted, count, err)
	}
	r, err := st.Query(ctx, "keep", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil || len(r.Entries) != 1 {
		t.Fatalf("adjacent prefix damaged: %+v %v", r, err)
	}
}

func TestApplyChangesChangeDetectionLimitsAndScanRefresh(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	entries := changeEntries(5)
	changes := make([]Change, len(entries))
	for i := range entries {
		changes[i] = Change{Path: entries[i].Path, Entry: &entries[i]}
	}
	results, err := st.ApplyChanges(ctx, changes, 2)
	if err != nil || len(results) != 2 || !results[0].Inserted || !results[1].Complete {
		t.Fatalf("bounded insert: %+v %v", results, err)
	}
	results, err = st.ApplyChanges(ctx, changes[:2], 2)
	if err != nil || results[0].Changed || results[1].Changed {
		t.Fatalf("unchanged rows rewritten: %+v %v", results, err)
	}
	entries[0].Size++
	results, err = st.ApplyChanges(ctx, changes[:1], 2)
	if err != nil || !results[0].Changed || results[0].Inserted {
		t.Fatalf("metadata update: %+v %v", results, err)
	}
	// A full scan must refresh last_seen_scan even for unchanged metadata.
	if err := st.UpsertBatch(ctx, 42, entries[:2]); err != nil {
		t.Fatal(err)
	}
	if err := st.CleanupStale(ctx, 42, []string{"/fixture"}); err != nil {
		t.Fatal(err)
	}
	count, err := st.Count(ctx)
	if err != nil || count != 2 {
		t.Fatalf("scan lost unchanged entries: %d %v", count, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.ApplyChanges(canceled, changes, 2); err == nil {
		t.Fatal("canceled changes committed")
	}
}

func TestApplyChangesRollbackReturnsNoPartialResults(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	entries := changeEntries(2)
	if err := st.db.Exec(`CREATE TRIGGER reject_fixture BEFORE INSERT ON entries WHEN NEW.name = 'file-000001.dat' BEGIN SELECT RAISE(ABORT,'fixture failure'); END;`); err != nil {
		t.Fatal(err)
	}
	changes := []Change{{Path: entries[0].Path, Entry: &entries[0]}, {Path: entries[1].Path, Entry: &entries[1]}}
	results, err := st.ApplyChanges(ctx, changes, 128)
	if err == nil || len(results) != 0 {
		t.Fatalf("partial success leaked: %+v %v", results, err)
	}
	count, err := st.Count(ctx)
	if err != nil || count != 0 {
		t.Fatalf("transaction not rolled back: %d %v", count, err)
	}
	if err := st.db.Exec("DROP TRIGGER reject_fixture;"); err != nil {
		t.Fatal(err)
	}
	results, err = st.ApplyChanges(ctx, changes, 128)
	if err != nil || len(results) != 2 || !results[0].Inserted {
		t.Fatalf("retry failed: %+v %v", results, err)
	}
}

func TestApplyChangesBatchesWALWrites(t *testing.T) {
	entries := changeEntries(256)
	var sizes []int64
	for _, size := range []int{1, 128} {
		path := filepath.Join(t.TempDir(), "index.sqlite")
		st, err := OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.db.Exec("PRAGMA wal_autocheckpoint=0; PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(entries); i += size {
			var changes []Change
			for j := i; j < min(i+size, len(entries)); j++ {
				changes = append(changes, Change{Path: entries[j].Path, Entry: &entries[j]})
			}
			if _, err := st.ApplyChanges(context.Background(), changes, size); err != nil {
				t.Fatal(err)
			}
		}
		info, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, info.Size())
		st.Close()
	}
	t.Logf("WAL bytes for same 256 inserts: per-file=%d batch-128=%d", sizes[0], sizes[1])
	if sizes[1] >= sizes[0]/2 {
		t.Fatalf("batching lost write reduction: %v", sizes)
	}
}

func TestApplyChangesFTSMatchesIncrementalSemantics(t *testing.T) {
	st := changeStore(t)
	ctx := context.Background()
	entry := changeEntries(1)[0]
	entry.Name = "oldneedle.dat"
	entry.Path = "/fixture/stable.dat"
	if _, err := st.ApplyChanges(ctx, []Change{{Path: entry.Path, Entry: &entry}}, 1); err != nil {
		t.Fatal(err)
	}
	entry.Name = "newneedle.dat"
	if _, err := st.ApplyChanges(ctx, []Change{{Path: entry.Path, Entry: &entry}}, 1); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"newneedle", "oldneedle"} {
		r, err := st.Query(ctx, query, sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if strings.Contains(query, "newneedle") {
			want = 1
		}
		if len(r.Entries) != want {
			t.Fatalf("query=%s result=%+v", query, r)
		}
	}
}
