package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
)

func TestSQLiteQueryShortPlainTermSkipsPathContains(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	assertShortPlainTermSkipsPathContains(t, ctx, st)
}

func TestSQLiteQueryMultiTermMatchesSeparatedNameTokens(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	assertMultiTermMatchesSeparatedNameTokens(t, ctx, st)
}

func TestSQLiteQueryLongPlainTermMatchesPathContains(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	assertLongPlainTermMatchesPathContains(t, ctx, st)
}

func TestSQLiteQueryPlainTermReturnsNameMatchesBeforePathContains(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	assertPlainTermReturnsNameMatchesBeforePathContains(t, ctx, st)
}

func TestSQLiteQueryPathTermSearchesPathContains(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	assertPathTermSearchesPathContains(t, ctx, st)
}

func assertShortPlainTermSkipsPathContains(t *testing.T, ctx context.Context, st Backend) {
	t.Helper()

	now := time.Now()
	batch := []model.Entry{
		{
			Path:       "/fixture/work/cph-agenda.pdf",
			Name:       "cph-agenda.pdf",
			ParentPath: "/fixture/work",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
		{
			Path:       "/fixture/work/flight-cph-ticket.pdf",
			Name:       "flight-cph-ticket.pdf",
			ParentPath: "/fixture/work",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
		{
			Path:       "/fixture/cph-archive/report.txt",
			Name:       "report.txt",
			ParentPath: "/fixture/cph-archive",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
	}
	if err := st.UpsertBatch(ctx, now.UnixMicro(), batch); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "cph", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}

	got := make(map[string]bool, len(res.Entries))
	for _, entry := range res.Entries {
		got[entry.Name] = true
	}
	if !got["cph-agenda.pdf"] || !got["flight-cph-ticket.pdf"] {
		t.Fatalf("expected 3-character query to keep name matches, got %+v", res.Entries)
	}
	if got["report.txt"] {
		t.Fatalf("did not expect 3-character query to include path-only contains match, got %+v", res.Entries)
	}
}

func assertMultiTermMatchesSeparatedNameTokens(t *testing.T, ctx context.Context, st Backend) {
	t.Helper()

	now := time.Now()
	batch := []model.Entry{
		{
			Path:       "/fixture/work/Alpha-Needle-Plan.md",
			Name:       "Alpha-Needle-Plan.md",
			ParentPath: "/fixture/work",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
		{
			Path:       "/fixture/work/Alpha-Report.md",
			Name:       "Alpha-Report.md",
			ParentPath: "/fixture/work",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
	}
	if err := st.UpsertBatch(ctx, now.UnixMicro(), batch); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "alpha plan", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Name != "Alpha-Needle-Plan.md" {
		t.Fatalf("expected all-term name match for Alpha-Needle-Plan.md, got %+v", res.Entries)
	}
}

func assertLongPlainTermMatchesPathContains(t *testing.T, ctx context.Context, st Backend) {
	t.Helper()

	now := time.Now()
	batch := []model.Entry{
		{
			Path:       "/fixture/copenhagen/report.txt",
			Name:       "report.txt",
			ParentPath: "/fixture/copenhagen",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
	}
	if err := st.UpsertBatch(ctx, now.UnixMicro(), batch); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "copenhagen", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Path != "/fixture/copenhagen/report.txt" {
		t.Fatalf("expected long plain query to include path contains match, got %+v", res.Entries)
	}
}

func assertPlainTermReturnsNameMatchesBeforePathContains(t *testing.T, ctx context.Context, st Backend) {
	t.Helper()

	now := time.Now()
	batch := []model.Entry{
		{
			Path:       "/Applications/Install Logi Options+.app",
			Name:       "Install Logi Options+.app",
			ParentPath: "/Applications",
			RootPath:   "/Applications",
			Type:       model.TypeDirectory,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
		{
			Path:       "/Applications/LogiTune.app",
			Name:       "LogiTune.app",
			ParentPath: "/Applications",
			RootPath:   "/Applications",
			Type:       model.TypeDirectory,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
		{
			Path:       "/fixture/logi/archive/report.txt",
			Name:       "report.txt",
			ParentPath: "/fixture/logi/archive",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
	}
	if err := st.UpsertBatch(ctx, now.UnixMicro(), batch); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "logi", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}

	got := make(map[string]bool, len(res.Entries))
	for _, entry := range res.Entries {
		got[entry.Name] = true
	}
	if !got["Install Logi Options+.app"] || !got["LogiTune.app"] {
		t.Fatalf("expected logi filename matches, got %+v", res.Entries)
	}
	if got["report.txt"] {
		t.Fatalf("did not expect plain term with filename hits to wait for path-only contains match, got %+v", res.Entries)
	}
}

func assertPathTermSearchesPathContains(t *testing.T, ctx context.Context, st Backend) {
	t.Helper()

	now := time.Now()
	batch := []model.Entry{
		{
			Path:       "/fixture/cph/report.txt",
			Name:       "report.txt",
			ParentPath: "/fixture/cph",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
	}
	if err := st.UpsertBatch(ctx, now.UnixMicro(), batch); err != nil {
		t.Fatal(err)
	}

	res, err := st.Query(ctx, "cph/report", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Path != "/fixture/cph/report.txt" {
		t.Fatalf("expected slash query to include path contains match, got %+v", res.Entries)
	}
}
