package startupcache

import (
	"container/heap"
	"sort"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/store"
)

type Top struct {
	sortSpec sorter.SortSpec
	limit    int
	entries  entryHeap
}

func NewTop(sortSpec sorter.SortSpec, limit int) *Top {
	t := &Top{
		sortSpec: sortSpec,
		limit:    limit,
		entries:  entryHeap{sortSpec: sortSpec},
	}
	heap.Init(&t.entries)
	return t
}

func (t *Top) Add(entry model.Entry) {
	if t == nil || t.limit <= 0 {
		return
	}
	if t.entries.Len() < t.limit {
		heap.Push(&t.entries, entry)
		return
	}
	if CompareEntries(entry, t.entries.items[0], t.sortSpec) >= 0 {
		return
	}
	t.entries.items[0] = entry
	heap.Fix(&t.entries, 0)
}

func (t *Top) AddBatch(entries []model.Entry) {
	for _, entry := range entries {
		t.Add(entry)
	}
}

func (t *Top) Len() int {
	if t == nil {
		return 0
	}
	return t.entries.Len()
}

func (t *Top) Result(total int) store.QueryResult {
	if t == nil || t.entries.Len() == 0 {
		return store.QueryResult{}
	}
	entries := append([]model.Entry(nil), t.entries.items...)
	sort.Slice(entries, func(i, j int) bool {
		return CompareEntries(entries[i], entries[j], t.sortSpec) < 0
	})
	if total < len(entries) {
		total = len(entries)
	}
	return store.QueryResult{Entries: entries, Total: total}
}

type entryHeap struct {
	sortSpec sorter.SortSpec
	items    []model.Entry
}

func (h entryHeap) Len() int {
	return len(h.items)
}

func (h entryHeap) Less(i, j int) bool {
	return CompareEntries(h.items[i], h.items[j], h.sortSpec) > 0
}

func (h entryHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
}

func (h *entryHeap) Push(x any) {
	h.items = append(h.items, x.(model.Entry))
}

func (h *entryHeap) Pop() any {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}
