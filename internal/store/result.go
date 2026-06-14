package store

import (
	gosort "sort"
	"strings"

	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/sorter"
)

type QueryResult struct {
	Entries []model.Entry
	Total   int
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
