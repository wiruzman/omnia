package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/sorter"
	"github.com/wiruzman/omnia/internal/store"
)

type benchConfig struct {
	count     int
	batchSize int
	runs      int
	limit     int
	dir       string
	keep      bool
}

type queryCase struct {
	name  string
	query string
	sort  sorter.SortSpec
}

type queryBenchResult struct {
	name   string
	query  string
	rows   int
	total  int
	first  time.Duration
	median time.Duration
	p95    time.Duration
}

type engineBenchResult struct {
	name      string
	indexTime time.Duration
	indexSize int64
	queries   []queryBenchResult
}

func main() {
	cfg := parseFlags()
	ctx := context.Background()

	workDir, cleanup, err := prepareWorkDir(cfg)
	if err != nil {
		panic(err)
	}
	defer cleanup()

	entries := syntheticEntries(cfg.count)
	cases := benchmarkQueries()

	fmt.Printf("dataset: %d entries, batch=%d, runs=%d, limit=%d\n", cfg.count, cfg.batchSize, cfg.runs, cfg.limit)
	if cfg.keep {
		fmt.Printf("index dir: %s\n\n", workDir)
	} else {
		fmt.Printf("index dir: %s (temporary)\n\n", workDir)
	}

	sqliteResult, err := runSQLiteBenchmark(ctx, filepath.Join(workDir, "index.sqlite"), entries, cases, cfg)
	if err != nil {
		panic(err)
	}
	printEngineResult(sqliteResult)
}

func parseFlags() benchConfig {
	cfg := benchConfig{}
	flag.IntVar(&cfg.count, "n", 200_000, "number of synthetic filesystem entries")
	flag.IntVar(&cfg.batchSize, "batch", 2_000, "entries per indexing batch")
	flag.IntVar(&cfg.runs, "runs", 20, "query runs per case")
	flag.IntVar(&cfg.limit, "limit", 100, "result limit per query")
	flag.StringVar(&cfg.dir, "dir", "", "directory for benchmark indexes; defaults to a temporary directory")
	flag.BoolVar(&cfg.keep, "keep", false, "keep benchmark indexes after the run")
	flag.Parse()

	if cfg.count <= 0 {
		cfg.count = 1
	}
	if cfg.batchSize <= 0 {
		cfg.batchSize = 1
	}
	if cfg.runs <= 0 {
		cfg.runs = 1
	}
	if cfg.limit <= 0 {
		cfg.limit = 1
	}
	return cfg
}

func prepareWorkDir(cfg benchConfig) (string, func(), error) {
	if cfg.dir != "" {
		if err := os.MkdirAll(cfg.dir, 0o755); err != nil {
			return "", nil, err
		}
		return cfg.dir, func() {}, nil
	}

	dir, err := os.MkdirTemp("", "omnia-search-bench-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		if !cfg.keep {
			_ = os.RemoveAll(dir)
		}
	}
	return dir, cleanup, nil
}

func syntheticEntries(count int) []model.Entry {
	now := time.Unix(1_700_000_000, 0)
	entries := make([]model.Entry, 0, count)
	extensions := []string{"txt", "md", "go", "pdf", "png", "json"}
	kinds := []string{"report", "source", "image", "archive", "note", "invoice"}

	for i := 0; i < count; i++ {
		ext := extensions[i%len(extensions)]
		kind := kinds[i%len(kinds)]
		name := fmt.Sprintf("%s_file_%06d.%s", kind, i, ext)
		if i%997 == 0 {
			name = fmt.Sprintf("Install Logi Options %06d.app", i)
		}
		if i%1231 == 0 {
			name = fmt.Sprintf("Alpha-Needle-Plan-%06d.md", i)
		}

		path := fmt.Sprintf("/Users/demo/project/dir_%04d/sub_%02d/%s", i%1000, i%37, name)
		entryType := model.TypeFile
		if strings.HasSuffix(name, ".app") {
			entryType = model.TypeDirectory
		}

		entries = append(entries, model.Entry{
			Path:       path,
			Name:       name,
			ParentPath: filepath.Dir(path),
			RootPath:   "/Users/demo",
			Type:       entryType,
			Size:       int64((i * 7919) % 8_388_608),
			CreatedAt:  now.Add(-time.Duration(i%10_000) * time.Minute),
			ModifiedAt: now.Add(-time.Duration((i*17)%20_000) * time.Minute),
		})
	}
	return entries
}

func benchmarkQueries() []queryCase {
	return []queryCase{
		{
			name:  "empty preview by modified",
			query: "",
			sort:  sorter.SortSpec{Column: sorter.SortModified, Direction: sorter.Desc},
		},
		{
			name:  "name prefix",
			query: "source_file_001",
			sort:  sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc},
		},
		{
			name:  "name contains",
			query: "needle",
			sort:  sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc},
		},
		{
			name:  "path prefix",
			query: "/users/demo/project/dir_0042",
			sort:  sorter.SortSpec{Column: sorter.SortPath, Direction: sorter.Asc},
		},
		{
			name:  "path contains",
			query: "sub_17",
			sort:  sorter.SortSpec{Column: sorter.SortPath, Direction: sorter.Asc},
		},
		{
			name:  "miss",
			query: "not-present-in-index",
			sort:  sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc},
		},
	}
}

func runSQLiteBenchmark(ctx context.Context, path string, entries []model.Entry, cases []queryCase, cfg benchConfig) (engineBenchResult, error) {
	if err := removeSQLiteFiles(path); err != nil {
		return engineBenchResult{}, err
	}

	start := time.Now()
	st, err := store.OpenSQLite(path)
	if err != nil {
		return engineBenchResult{}, err
	}
	scanID := time.Now().UnixMicro()
	if err := st.BeginScan(ctx, scanID); err != nil {
		_ = st.Close()
		return engineBenchResult{}, err
	}
	for batchStart := 0; batchStart < len(entries); batchStart += cfg.batchSize {
		batchEnd := minInt(batchStart+cfg.batchSize, len(entries))
		if err := st.UpsertBatch(ctx, scanID, entries[batchStart:batchEnd]); err != nil {
			_ = st.Close()
			return engineBenchResult{}, err
		}
	}
	if err := st.Close(); err != nil {
		return engineBenchResult{}, err
	}
	indexTime := time.Since(start)

	indexSize, err := sqlitePathSize(path)
	if err != nil {
		return engineBenchResult{}, err
	}

	readStore, err := store.OpenSQLiteReadOnly(path)
	if err != nil {
		return engineBenchResult{}, err
	}
	defer readStore.Close()

	queryResults, err := measureQueries(ctx, cases, cfg, readStore.Query)
	if err != nil {
		return engineBenchResult{}, err
	}

	return engineBenchResult{
		name:      "SQLite FTS5 trigram",
		indexTime: indexTime,
		indexSize: indexSize,
		queries:   queryResults,
	}, nil
}

func measureQueries(
	ctx context.Context,
	cases []queryCase,
	cfg benchConfig,
	query func(context.Context, string, sorter.SortSpec, int, int) (store.QueryResult, error),
) ([]queryBenchResult, error) {
	results := make([]queryBenchResult, 0, len(cases))
	for _, c := range cases {
		durations := make([]time.Duration, 0, cfg.runs)
		var rows int
		var total int
		var first time.Duration

		for run := 0; run < cfg.runs; run++ {
			start := time.Now()
			res, err := query(ctx, c.query, c.sort, cfg.limit, 0)
			elapsed := time.Since(start)
			if err != nil {
				return nil, fmt.Errorf("%s query %q failed: %w", c.name, c.query, err)
			}
			if run == 0 {
				rows = len(res.Entries)
				total = res.Total
				first = elapsed
			}
			durations = append(durations, elapsed)
		}

		sortedDurations := append([]time.Duration(nil), durations...)
		sort.Slice(sortedDurations, func(i, j int) bool {
			return sortedDurations[i] < sortedDurations[j]
		})

		results = append(results, queryBenchResult{
			name:   c.name,
			query:  c.query,
			rows:   rows,
			total:  total,
			first:  first,
			median: percentile(sortedDurations, 50),
			p95:    percentile(sortedDurations, 95),
		})
	}
	return results, nil
}

func percentile(sortedDurations []time.Duration, pct int) time.Duration {
	if len(sortedDurations) == 0 {
		return 0
	}
	index := (len(sortedDurations)*pct + 99) / 100
	if index <= 0 {
		index = 1
	}
	if index > len(sortedDurations) {
		index = len(sortedDurations)
	}
	return sortedDurations[index-1]
}

func sqlitePathSize(path string) (int64, error) {
	var total int64
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func removeSQLiteFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func printEngineResult(result engineBenchResult) {
	fmt.Printf("%s\n", result.name)
	fmt.Printf("  index: %s, size=%s\n", result.indexTime.Round(time.Millisecond), formatBytes(result.indexSize))
	fmt.Printf("  queries:\n")
	for _, query := range result.queries {
		fmt.Printf("    %-25s rows=%3d total=%3d first=%9s median=%9s p95=%9s query=%q\n",
			query.name,
			query.rows,
			query.total,
			query.first.Round(time.Microsecond),
			query.median.Round(time.Microsecond),
			query.p95.Round(time.Microsecond),
			query.query,
		)
	}
	fmt.Println()
}

func formatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KiB", "MiB", "GiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TiB", value/unit)
}
