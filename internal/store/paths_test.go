package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wiruzman/omnia/internal/model"
)

func TestPathsUnderIsBoundedAndPreservesScope(t *testing.T) {
	st := changeStore(t)
	entries := changeEntries(8)
	for _, path := range []string{"/", "/fixture/directory", "/fixture/directory2/adjacent", "/elsewhere/outside"} {
		entries = append(entries, model.Entry{Path: path, RootPath: "/", Type: model.TypeFile})
	}
	ctx := context.Background()
	if err := st.UpsertBatch(ctx, 1, entries); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, 1, 4, 20} {
		got, err := st.PathsUnder(ctx, "/fixture/directory", limit)
		if err != nil || len(got) != min(limit, 8) {
			t.Fatalf("limit %d: %v err=%v", limit, got, err)
		}
		for i, path := range got {
			if path != entries[i].Path {
				t.Fatalf("out of scope or order: %v", got)
			}
		}
	}
	all, err := st.PathsUnder(ctx, "/", 20)
	if err != nil || len(all) != len(entries)-1 {
		t.Fatalf("filesystem root: %v %v", all, err)
	}
	for _, p := range all {
		if p == "/" {
			t.Fatal("returned path itself")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.PathsUnder(cancelled, "/fixture", 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
