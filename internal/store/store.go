package store

import (
	"context"
	"os"
	"path/filepath"
	gosort "sort"
	"strings"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	querypkg "github.com/blevesearch/bleve/v2/search/query"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
)

type Store struct {
	index bleve.Index
}

type QueryResult struct {
	Entries []model.Entry
	Total   int
}

type indexedEntry struct {
	Path         string `json:"path"`
	PathLower    string `json:"path_lower"`
	Name         string `json:"name"`
	NameLower    string `json:"name_lower"`
	ParentPath   string `json:"parent_path"`
	RootPath     string `json:"root_path"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	CreatedAt    int64  `json:"created_at"`
	ModifiedAt   int64  `json:"modified_at"`
	LastSeenScan int64  `json:"last_seen_scan"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	idx, err := bleve.Open(path)
	if err != nil {
		idx, err = bleve.New(path, newIndexMapping())
		if err != nil {
			return nil, err
		}
	}
	return &Store{index: idx}, nil
}

func OpenReadOnly(path string) (*Store, error) {
	idx, err := bleve.OpenUsing(path, map[string]interface{}{"read_only": true})
	if err != nil {
		return nil, err
	}
	return &Store{index: idx}, nil
}

func (s *Store) Close() error {
	return s.index.Close()
}

func (s *Store) BeginScan(ctx context.Context, scanID int64) error {
	return nil
}

func (s *Store) UpsertBatch(ctx context.Context, scanID int64, batch []model.Entry) error {
	if len(batch) == 0 {
		return nil
	}
	b := s.index.NewBatch()
	for _, e := range batch {
		if err := b.Index(e.Path, entryToIndexed(e, scanID)); err != nil {
			return err
		}
	}
	return s.index.Batch(b)
}

func (s *Store) CleanupStale(ctx context.Context, scanID int64, roots []string) error {
	b := s.index.NewBatch()
	deletes := 0
	for _, root := range roots {
		root = filepath.Clean(root)
		q := bleve.NewTermQuery(root)
		q.SetField("root_path")
		req := bleve.NewSearchRequestOptions(q, 1_000_000, 0, false)
		req.Fields = []string{"last_seen_scan"}
		res, err := s.index.Search(req)
		if err != nil {
			return err
		}
		for _, hit := range res.Hits {
			if fieldInt64(hit.Fields["last_seen_scan"]) != scanID {
				b.Delete(hit.ID)
				deletes++
			}
		}
	}
	if deletes == 0 {
		return nil
	}
	return s.index.Batch(b)
}

func (s *Store) Count(ctx context.Context) (int, error) {
	res, err := s.index.Search(bleve.NewSearchRequestOptions(bleve.NewMatchAllQuery(), 0, 0, false))
	if err != nil {
		return 0, err
	}
	return int(res.Total), nil
}

func (s *Store) CountByRoots(ctx context.Context, roots []string) (map[string]int64, error) {
	counts := make(map[string]int64, len(roots))
	for _, root := range roots {
		cleanRoot := filepath.Clean(root)
		q := bleve.NewTermQuery(cleanRoot)
		q.SetField("root_path")
		res, err := s.index.Search(bleve.NewSearchRequestOptions(q, 0, 0, false))
		if err != nil {
			return nil, err
		}
		counts[cleanRoot] = int64(res.Total)
	}
	return counts, nil
}

func (s *Store) Preview(ctx context.Context, sort sorter.SortSpec, limit int) (QueryResult, error) {
	entries, total, err := s.searchEntries(ctx, bleve.NewMatchAllQuery(), sortToBleveFields(sort), limit, 0)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Entries: entries, Total: total}, nil
}

func (s *Store) Query(ctx context.Context, query string, sort sorter.SortSpec, limit, offset int) (QueryResult, error) {
	if err := ctx.Err(); err != nil {
		return QueryResult{}, err
	}
	plan := planQuery(query)
	qLower := plan.query
	searchSort := sortToBleveFields(sort)
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	if qLower == "" {
		entries, total, err := s.searchEntries(ctx, bleve.NewMatchAllQuery(), sortToBleveFields(sort), limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		return QueryResult{Entries: entries, Total: total}, nil
	}

	// Stage search from cheapest to most expensive to keep interactive typing responsive.
	entries := make([]model.Entry, 0, limit)
	seen := make(map[string]struct{}, limit)

	if !plan.pathLike {
		prefixName := bleve.NewPrefixQuery(qLower)
		prefixName.SetField("name_lower")
		prefixNameEntries, _, err := s.searchEntries(ctx, prefixName, searchSort, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		entries = appendUniqueEntries(entries, seen, prefixNameEntries, limit)
		if len(entries) >= limit {
			sortEntries(entries, sort)
			return QueryResult{Entries: entries, Total: len(entries)}, nil
		}
		if err := ctx.Err(); err != nil {
			return QueryResult{}, err
		}
	}

	if plan.pathLike && plan.absolutePathLike {
		prefixPath := bleve.NewPrefixQuery(qLower)
		prefixPath.SetField("path_lower")
		prefixPathEntries, _, err := s.searchEntries(ctx, prefixPath, searchSort, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		entries = appendUniqueEntries(entries, seen, prefixPathEntries, limit)
		if len(entries) >= limit {
			sortEntries(entries, sort)
			return QueryResult{Entries: entries, Total: len(entries)}, nil
		}
		if err := ctx.Err(); err != nil {
			return QueryResult{}, err
		}
	}

	if plan.shouldStopAfterPrefix(len(entries), limit) {
		sortEntries(entries, sort)
		return QueryResult{Entries: entries, Total: len(entries)}, nil
	}

	if plan.allowNameContains() {
		containsName := bleveContainsQuery("name_lower", qLower)
		containsNameEntries, _, err := s.searchEntries(ctx, containsName, searchSort, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		entries = appendUniqueEntries(entries, seen, containsNameEntries, limit)
		if len(entries) >= limit {
			sortEntries(entries, sort)
			return QueryResult{Entries: entries, Total: len(entries)}, nil
		}
		if err := ctx.Err(); err != nil {
			return QueryResult{}, err
		}

		if plan.allowAllTermContains() {
			containsNameTerms := bleveContainsAllQuery("name_lower", plan.terms)
			containsNameTermEntries, _, err := s.searchEntries(ctx, containsNameTerms, searchSort, limit, offset)
			if err != nil {
				return QueryResult{}, err
			}
			entries = appendUniqueEntries(entries, seen, containsNameTermEntries, limit)
			if len(entries) >= limit {
				sortEntries(entries, sort)
				return QueryResult{Entries: entries, Total: len(entries)}, nil
			}
			if err := ctx.Err(); err != nil {
				return QueryResult{}, err
			}
		}
	}

	if !plan.allowPathContains(len(entries)) {
		sortEntries(entries, sort)
		return QueryResult{Entries: entries, Total: len(entries)}, nil
	}

	containsPath := bleveContainsQuery("path_lower", qLower)
	if plan.pathLike || !plan.allowAllTermContains() {
		containsPathEntries, _, err := s.searchEntries(ctx, containsPath, searchSort, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		entries = appendUniqueEntries(entries, seen, containsPathEntries, limit)
	} else {
		containsPathTerms := bleveContainsAllQuery("path_lower", plan.terms)
		containsPathTermEntries, _, err := s.searchEntries(ctx, containsPathTerms, searchSort, limit, offset)
		if err != nil {
			return QueryResult{}, err
		}
		entries = appendUniqueEntries(entries, seen, containsPathTermEntries, limit)
	}
	sortEntries(entries, sort)

	// Avoid expensive full COUNT during live search; report visible match count.
	return QueryResult{Entries: entries, Total: len(entries)}, nil
}

func appendUniqueEntries(dst []model.Entry, seen map[string]struct{}, src []model.Entry, limit int) []model.Entry {
	for _, e := range src {
		if _, ok := seen[e.Path]; ok {
			continue
		}
		dst = append(dst, e)
		seen[e.Path] = struct{}{}
		if len(dst) >= limit {
			return dst
		}
	}
	return dst
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Store) searchEntries(ctx context.Context, q querypkg.Query, sortFields []string, limit, offset int) ([]model.Entry, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	req := bleve.NewSearchRequestOptions(q, limit, offset, false)
	req.Fields = []string{"path", "name", "parent_path", "root_path", "type", "size", "created_at", "modified_at", "last_seen_scan"}
	if len(sortFields) > 0 {
		req.SortBy(sortFields)
	}
	res, err := s.index.Search(req)
	if err != nil {
		return nil, 0, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	entries := make([]model.Entry, 0, len(res.Hits))
	for _, hit := range res.Hits {
		entries = append(entries, hitToEntry(hit))
	}
	return entries, int(res.Total), nil
}

func (s *Store) UpsertEntry(ctx context.Context, e model.Entry) error {
	return s.UpsertBatch(ctx, time.Now().UnixMicro(), []model.Entry{e})
}

func (s *Store) DeletePath(ctx context.Context, path string) error {
	b := s.index.NewBatch()
	b.Delete(filepath.Clean(path))
	return s.index.Batch(b)
}

func (s *Store) DeletePathPrefix(ctx context.Context, dirPath string) error {
	prefix := filepath.Clean(dirPath)
	withSlash := prefix
	if !strings.HasSuffix(withSlash, string(os.PathSeparator)) {
		withSlash += string(os.PathSeparator)
	}
	qExact := bleve.NewTermQuery(prefix)
	qExact.SetField("path")
	qPrefix := bleve.NewPrefixQuery(strings.ToLower(withSlash))
	qPrefix.SetField("path_lower")
	q := bleve.NewDisjunctionQuery(qExact, qPrefix)

	entries, _, err := s.searchEntries(ctx, q, nil, 1_000_000, 0)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	b := s.index.NewBatch()
	for _, e := range entries {
		b.Delete(e.Path)
	}
	return s.index.Batch(b)
}

func newIndexMapping() *mapping.IndexMappingImpl {
	mapping := bleve.NewIndexMapping()
	doc := bleve.NewDocumentMapping()

	text := bleve.NewTextFieldMapping()
	text.Analyzer = "keyword"
	text.Store = true

	numeric := bleve.NewNumericFieldMapping()
	numeric.Store = true

	for _, field := range []string{"path", "path_lower", "name", "name_lower", "parent_path", "root_path", "type"} {
		doc.AddFieldMappingsAt(field, text)
	}
	for _, field := range []string{"size", "created_at", "modified_at", "last_seen_scan"} {
		doc.AddFieldMappingsAt(field, numeric)
	}

	mapping.DefaultAnalyzer = "keyword"
	mapping.DefaultMapping = doc
	return mapping
}

func entryToIndexed(e model.Entry, scanID int64) indexedEntry {
	return indexedEntry{
		Path:         e.Path,
		PathLower:    strings.ToLower(e.Path),
		Name:         e.Name,
		NameLower:    strings.ToLower(e.Name),
		ParentPath:   e.ParentPath,
		RootPath:     e.RootPath,
		Type:         string(e.Type),
		Size:         e.Size,
		CreatedAt:    e.CreatedAt.Unix(),
		ModifiedAt:   e.ModifiedAt.Unix(),
		LastSeenScan: scanID,
	}
}

func hitToEntry(hit *search.DocumentMatch) model.Entry {
	fields := hit.Fields
	created := fieldInt64(fields["created_at"])
	modified := fieldInt64(fields["modified_at"])
	return model.Entry{
		Path:       fieldString(fields["path"]),
		Name:       fieldString(fields["name"]),
		ParentPath: fieldString(fields["parent_path"]),
		RootPath:   fieldString(fields["root_path"]),
		Type:       model.FileType(fieldString(fields["type"])),
		Size:       fieldInt64(fields["size"]),
		CreatedAt:  time.Unix(created, 0),
		ModifiedAt: time.Unix(modified, 0),
	}
}

func sortToBleveFields(spec sorter.SortSpec) []string {
	prefix := ""
	if spec.Direction == sorter.Desc {
		prefix = "-"
	}
	field := "name_lower"
	switch spec.Column {
	case sorter.SortName:
		field = "name_lower"
	case sorter.SortPath:
		field = "path_lower"
	case sorter.SortSize:
		field = "size"
	case sorter.SortCreated:
		field = "created_at"
	case sorter.SortModified:
		field = "modified_at"
	}
	if field == "path_lower" {
		return []string{prefix + field}
	}
	return []string{prefix + field, "path_lower"}
}

func fieldString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}

func fieldInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case float32:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case uint64:
		return int64(t)
	default:
		return 0
	}
}

func sortEntries(entries []model.Entry, spec sorter.SortSpec) {
	gosort.SliceStable(entries, func(i, j int) bool {
		a := entries[i]
		b := entries[j]
		cmp := 0
		switch spec.Column {
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

		if spec.Direction == sorter.Desc {
			cmp = -cmp
		}
		if cmp != 0 {
			return cmp < 0
		}
		return strings.ToLower(a.Path) < strings.ToLower(b.Path)
	})
}
