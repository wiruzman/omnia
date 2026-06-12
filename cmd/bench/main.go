package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/store"
)

func main() {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(".", "bench-index.bleve"))
	if err != nil {
		panic(err)
	}
	defer st.Close()

	scanID := time.Now().UnixMicro()
	_ = st.BeginScan(ctx, scanID)

	now := time.Now()
	batch := make([]model.Entry, 0, 200000)
	for i := 0; i < 200000; i++ {
		path := fmt.Sprintf("/Users/demo/project/dir_%d/file_%d.txt", i%500, i)
		batch = append(batch, model.Entry{
			Path:       path,
			Name:       fmt.Sprintf("file_%d.txt", i),
			ParentPath: filepath.Dir(path),
			RootPath:   "/Users/demo",
			Type:       model.TypeFile,
			Size:       int64(i % 4096),
			CreatedAt:  now,
			ModifiedAt: now,
		})
		if len(batch) == 2000 {
			if err := st.UpsertBatch(ctx, scanID, batch); err != nil {
				panic(err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := st.UpsertBatch(ctx, scanID, batch); err != nil {
			panic(err)
		}
	}

	start := time.Now()
	res, err := st.Query(ctx, "file_1999", sorter.SortSpec{Column: sorter.SortPath, Direction: sorter.Asc}, 100, 0)
	if err != nil {
		panic(err)
	}
	fmt.Printf("query returned %d rows (total %d) in %s\n", len(res.Entries), res.Total, time.Since(start))
}
