package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/startupcache"
	"omnia-search-tui/internal/store"
)

const (
	searchStateIdle uint32 = iota
	searchStatePending
	searchStateRunning
	searchStateDone
)

const liveSearchLimitCap = 1200

func (a *App) setSearchState(state uint32) {
	a.searchState.Store(state)
}

func (a *App) searchStateText() string {
	if strings.TrimSpace(a.query) == "" {
		return "idle"
	}
	switch a.searchState.Load() {
	case searchStatePending:
		return "queued"
	case searchStateRunning:
		return "running"
	case searchStateDone:
		return "finished"
	default:
		return "idle"
	}
}

func (a *App) debounceRefresh() {
	a.searchMu.Lock()
	defer a.searchMu.Unlock()
	if a.searchDue != nil {
		a.searchDue.Stop()
	}
	if a.searchCancel != nil {
		a.searchCancel()
		a.searchCancel = nil
	}
	a.setSearchState(searchStatePending)
	refreshID := atomic.AddUint64(&a.refreshID, 1)
	query := a.query
	sortSpec := a.sortSpec
	a.searchDue = time.AfterFunc(time.Duration(a.cfg.DebounceMs)*time.Millisecond, func() {
		go a.refreshDataAsync(context.Background(), refreshID, query, sortSpec)
	})
}

func (a *App) invalidatePendingRefreshes() {
	a.searchMu.Lock()
	defer a.searchMu.Unlock()
	if a.searchDue != nil {
		a.searchDue.Stop()
		a.searchDue = nil
	}
	if a.searchCancel != nil {
		a.searchCancel()
		a.searchCancel = nil
	}
	a.setSearchState(searchStateIdle)
	atomic.AddUint64(&a.refreshID, 1)
}

func (a *App) cancelInFlightSearch() {
	a.searchMu.Lock()
	defer a.searchMu.Unlock()
	if a.searchCancel != nil {
		a.searchCancel()
		a.searchCancel = nil
	}
}

func (a *App) newSearchContext(parent context.Context) (context.Context, func()) {
	a.searchMu.Lock()
	if a.searchCancel != nil {
		a.searchCancel()
		a.searchCancel = nil
	}
	a.searchRunID++
	runID := a.searchRunID
	ctx, cancel := context.WithCancel(parent)
	a.searchCancel = cancel
	a.searchMu.Unlock()

	cleanup := func() {
		a.searchMu.Lock()
		if a.searchRunID == runID {
			a.searchCancel = nil
		}
		a.searchMu.Unlock()
		cancel()
	}
	return ctx, cleanup
}

func (a *App) requestRefreshAsync(query string, sortSpec sorter.SortSpec) {
	a.setSearchState(searchStatePending)
	refreshID := atomic.AddUint64(&a.refreshID, 1)
	go a.refreshDataAsync(context.Background(), refreshID, query, sortSpec)
}

func (a *App) refreshDataAsync(ctx context.Context, refreshID uint64, query string, sortSpec sorter.SortSpec) {
	a.setSearchState(searchStateRunning)
	a.tui.QueueUpdateDraw(func() {
		if refreshID != atomic.LoadUint64(&a.refreshID) {
			return
		}
		a.updateStatus()
	})

	searchCtx, cleanup := a.newSearchContext(ctx)
	defer cleanup()

	entries, total, err := a.queryEntries(searchCtx, query, sortSpec)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		if refreshID == atomic.LoadUint64(&a.refreshID) {
			a.setSearchState(searchStateDone)
			a.tui.QueueUpdateDraw(func() {
				if refreshID != atomic.LoadUint64(&a.refreshID) {
					return
				}
				a.updateStatus()
			})
		}
		a.logger.Printf("query failed: %v", err)
		return
	}
	if refreshID != atomic.LoadUint64(&a.refreshID) {
		return
	}
	a.cacheWarmStartResults(query, sortSpec, entries, total)
	a.tui.QueueUpdateDraw(func() {
		if refreshID != atomic.LoadUint64(&a.refreshID) {
			return
		}
		a.applyResults(entries, total)
	})
}

func (a *App) refreshData(ctx context.Context) {
	a.setSearchState(searchStateRunning)
	searchCtx, cleanup := a.newSearchContext(ctx)
	defer cleanup()

	entries, total, err := a.queryEntries(searchCtx, a.query, a.sortSpec)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		a.setSearchState(searchStateDone)
		a.logger.Printf("query failed: %v", err)
		return
	}
	a.cacheWarmStartResults(a.query, a.sortSpec, entries, total)
	a.applyResults(entries, total)
}

func (a *App) queryEntries(ctx context.Context, query string, sortSpec sorter.SortSpec) ([]model.Entry, int, error) {
	qLower := strings.ToLower(strings.TrimSpace(query))
	allowBroadFallback := len(qLower) >= 5 || strings.Contains(qLower, "/")
	queryLimit := a.cfg.MaxResults
	if qLower != "" {
		queryLimit = effectiveLiveSearchLimit(queryLimit)
	}

	queryCtx := ctx
	cancel := func() {}
	if qLower != "" {
		// Keep live search responsive while users type; empty-query refreshes may need
		// more time on very large indexes to compute sorted top-N correctly.
		queryCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
	}
	defer cancel()

	res, err := a.storeQuery(queryCtx, query, sortSpec, queryLimit, 0)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && strings.TrimSpace(query) != "" {
			fallbackLimit := queryLimit
			if fallbackLimit < 200 {
				fallbackLimit = 200
			}

			fallbackCtx, fallbackCancel := context.WithTimeout(ctx, 1200*time.Millisecond)
			defer fallbackCancel()
			fallback, ferr := a.storeQuery(fallbackCtx, "", sortSpec, fallbackLimit*2, 0)
			if ferr == nil {
				filtered := filterFallbackEntries(query, fallback.Entries, queryLimit)
				return filtered, len(filtered), nil
			}

			// Always update UI state on timeout instead of keeping stale rows.
			return []model.Entry{}, 0, nil
		}
		return nil, 0, err
	}
	entries := res.Entries

	if qLower != "" && len(entries) == 0 && !a.isIndexing() && allowBroadFallback {
		fallbackLimit := queryLimit
		if fallbackLimit < 200 {
			fallbackLimit = 200
		}

		fallbackCtx, fallbackCancel := context.WithTimeout(ctx, 700*time.Millisecond)
		defer fallbackCancel()

		fallback, err := a.storeQuery(fallbackCtx, "", sortSpec, fallbackLimit*2, 0)
		if err == nil {
			filtered := filterFallbackEntries(query, fallback.Entries, queryLimit)
			entries = filtered
			res.Total = len(filtered)
		}
	}
	return entries, res.Total, nil
}

func effectiveLiveSearchLimit(maxResults int) int {
	if maxResults <= 0 {
		return liveSearchLimitCap
	}
	if maxResults > liveSearchLimitCap {
		return liveSearchLimitCap
	}
	return maxResults
}

func filterFallbackEntries(query string, source []model.Entry, limit int) []model.Entry {
	qLower := strings.ToLower(strings.TrimSpace(query))
	if qLower == "" || limit <= 0 {
		return nil
	}
	allowPathContains := len(qLower) >= 5 || strings.Contains(qLower, "/")

	namePrefix := make([]model.Entry, 0, limit)
	nameContains := make([]model.Entry, 0, limit)
	pathContains := make([]model.Entry, 0, limit)
	seen := make(map[string]struct{}, limit)

	for _, e := range source {
		nameLower := strings.ToLower(e.Name)
		pathLower := strings.ToLower(e.Path)

		if strings.HasPrefix(nameLower, qLower) {
			if _, ok := seen[e.Path]; !ok {
				namePrefix = append(namePrefix, e)
				seen[e.Path] = struct{}{}
			}
			continue
		}
		if strings.Contains(nameLower, qLower) {
			if _, ok := seen[e.Path]; !ok {
				nameContains = append(nameContains, e)
				seen[e.Path] = struct{}{}
			}
			continue
		}
		if allowPathContains && strings.Contains(pathLower, qLower) {
			if _, ok := seen[e.Path]; !ok {
				pathContains = append(pathContains, e)
				seen[e.Path] = struct{}{}
			}
		}
	}

	out := make([]model.Entry, 0, limit)
	for _, bucket := range [][]model.Entry{namePrefix, nameContains, pathContains} {
		for _, e := range bucket {
			out = append(out, e)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func (a *App) applyResults(entries []model.Entry, total int) {
	hadEntries := len(a.entries) > 0
	if a.resetSelectionOnNextResults {
		a.selected = 0
		a.selectedCol = 0
		a.horizontalScrollCol = 0
		a.table.SetOffset(0, 0)
		a.resetSelectionOnNextResults = false
	} else if row, _ := a.table.GetSelection(); row > 0 && row-1 < len(a.entries) {
		a.selected = row - 1
	} else if !hadEntries && len(entries) > 0 {
		a.selected = 0
		a.horizontalScrollCol = 0
		a.table.SetOffset(0, 0)
	}

	a.entries = entries
	a.total = total
	a.visible = len(entries)
	if a.selected >= len(a.entries) {
		a.selected = len(a.entries) - 1
	}
	if a.selected < 0 {
		a.selected = 0
	}
	a.renderTable()
	a.setSearchState(searchStateDone)
	a.updateStatus()
}

func (a *App) cacheWarmStartResults(query string, sortSpec sorter.SortSpec, entries []model.Entry, total int) {
	if strings.TrimSpace(query) != "" || len(entries) == 0 {
		return
	}
	limit := startupcache.EffectiveLimit(a.cfg.MaxResults)
	if limit <= 0 {
		return
	}
	result := store.QueryResult{
		Entries: append([]model.Entry(nil), entries...),
		Total:   total,
	}
	go func() {
		if err := startupcache.Save(startupcache.Path(a.cfg), sortSpec, limit, result); err != nil {
			a.logger.Printf("save startup cache failed: %v", err)
		}
	}()
}

func (a *App) applyWarmStartCache() bool {
	res, ok, err := a.loadWarmStartCache()
	if err != nil {
		a.logger.Printf("load startup cache failed: %v", err)
		return false
	}
	if !ok {
		return false
	}
	a.applyResults(res.Entries, res.Total)
	return true
}
