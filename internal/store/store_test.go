package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
)

func TestStoreUpsertQueryAndCleanup(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	scanID := time.Now().UnixNano()
	if err := st.BeginScan(ctx, scanID); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	batch := []model.Entry{
		{Path: "/tmp/a.txt", Name: "a.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/b.txt", Name: "b.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 20, CreatedAt: now, ModifiedAt: now},
	}
	if err := st.UpsertBatch(ctx, scanID, batch); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "a.txt", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 {
		t.Fatalf("expected total 1 got %d", res.Total)
	}

	res, err = st.Query(ctx, "tmp/a", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Path != "/tmp/a.txt" {
		t.Fatalf("expected substring path match for /tmp/a.txt, got %+v", res.Entries)
	}

	if err := st.CleanupStale(ctx, scanID+1, []string{"/tmp"}); err != nil {
		t.Fatal(err)
	}
	count, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected count 0 after cleanup, got %d", count)
	}
}

func TestStoreCleanupKeepsEntriesFromCurrentScan(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	scanID := time.Now().UnixMicro()
	now := time.Now()
	if err := st.UpsertBatch(ctx, scanID, []model.Entry{
		{Path: "/tmp/current.txt", Name: "current.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.CleanupStale(ctx, scanID, []string{"/tmp"}); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "current", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Name != "current.txt" {
		t.Fatalf("expected current scan entry to survive cleanup, got %+v", res.Entries)
	}
}

func TestStoreQueryContainsForMediumShortTerms(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	batch := []model.Entry{
		{Path: "/Applications/Install Logi Options+.app", Name: "Install Logi Options+.app", ParentPath: "/Applications", RootPath: "/Applications", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
		{Path: "/Applications/LogiTune.app", Name: "LogiTune.app", ParentPath: "/Applications", RootPath: "/Applications", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
	}
	if err := st.UpsertBatch(ctx, time.Now().UnixNano(), batch); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "logi", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("expected 2 matches for logi, got %d", len(res.Entries))
	}
}

func TestStoreQueryIsCaseInsensitiveForDocker(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	batch := []model.Entry{
		{Path: "/Applications/Docker.app", Name: "Docker.app", ParentPath: "/Applications", RootPath: "/Applications", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
		{Path: "/Users/mehmet/Library/Containers/com.docker.docker", Name: "com.docker.docker", ParentPath: "/Users/mehmet/Library/Containers", RootPath: "/Users/mehmet", Type: model.TypeDirectory, Size: 10, CreatedAt: now, ModifiedAt: now},
	}
	if err := st.UpsertBatch(ctx, time.Now().UnixNano(), batch); err != nil {
		t.Fatal(err)
	}

	upper, err := st.Query(ctx, "Docker", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	lower, err := st.Query(ctx, "docker", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(upper.Entries) == 0 || len(lower.Entries) == 0 {
		t.Fatalf("expected Docker/docker queries to return matches, got upper=%d lower=%d", len(upper.Entries), len(lower.Entries))
	}
}

func TestStoreCountByRoots(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	batch := []model.Entry{
		{Path: "/r1/a.txt", Name: "a.txt", ParentPath: "/r1", RootPath: "/r1", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
		{Path: "/r1/b.txt", Name: "b.txt", ParentPath: "/r1", RootPath: "/r1", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
		{Path: "/r2/c.txt", Name: "c.txt", ParentPath: "/r2", RootPath: "/r2", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
	}
	if err := st.UpsertBatch(ctx, now.UnixNano(), batch); err != nil {
		t.Fatal(err)
	}

	counts, err := st.CountByRoots(ctx, []string{"/r1", "/r2", "/missing"})
	if err != nil {
		t.Fatal(err)
	}
	if counts["/r1"] != 2 {
		t.Fatalf("expected /r1 count 2, got %d", counts["/r1"])
	}
	if counts["/r2"] != 1 {
		t.Fatalf("expected /r2 count 1, got %d", counts["/r2"])
	}
	if counts["/missing"] != 0 {
		t.Fatalf("expected /missing count 0, got %d", counts["/missing"])
	}
}

func TestStoreQueryHonorsCanceledContext(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	if err := st.UpsertBatch(ctx, now.UnixNano(), []model.Entry{
		{Path: "/tmp/a.txt", Name: "a.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 1, CreatedAt: now, ModifiedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = st.Query(canceledCtx, "a", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err == nil {
		t.Fatal("expected canceled context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled error, got %v", err)
	}
}

func TestStoreQueryShortPrefixWindowSkipsBroadContains(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	batch := make([]model.Entry, 0, 260)
	for i := 0; i < 220; i++ {
		name := "docprefix_" + time.Unix(int64(i), 0).Format("150405") + ".txt"
		batch = append(batch, model.Entry{
			Path:       "/tmp/" + name,
			Name:       name,
			ParentPath: "/tmp",
			RootPath:   "/tmp",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		})
	}
	for i := 0; i < 30; i++ {
		name := "xdoccontains_" + time.Unix(int64(i+300), 0).Format("150405") + ".txt"
		batch = append(batch, model.Entry{
			Path:       "/tmp/" + name,
			Name:       name,
			ParentPath: "/tmp",
			RootPath:   "/tmp",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		})
	}

	if err := st.UpsertBatch(ctx, now.UnixNano(), batch); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "doc", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) < 200 {
		t.Fatalf("expected prefix window to be populated, got %d entries", len(res.Entries))
	}
	for _, e := range res.Entries {
		if len(e.Name) > 0 && e.Name[0] == 'x' {
			t.Fatalf("did not expect contains-only match %q in short-query optimized results", e.Name)
		}
	}
}
