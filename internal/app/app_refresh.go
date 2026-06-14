package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wiruzman/omnia/internal/model"
	"github.com/wiruzman/omnia/internal/sorter"
	"github.com/wiruzman/omnia/internal/startupcache"
	"github.com/wiruzman/omnia/internal/store"
)

const (
	searchStateIdle uint32 = iota
	searchStatePending
	searchStateRunning
	searchStateDone
)

const (
	refreshReasonRefresh uint32 = iota
	refreshReasonSearch
	refreshReasonSort
)

const (
	liveSearchLimitCap = 1200
	liveSearchTimeout  = 900 * time.Millisecond
)

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

func (a *App) activityText() string {
	state := a.searchState.Load()
	reason := a.refreshReason.Load()

	switch state {
	case searchStatePending:
		switch reason {
		case refreshReasonSort:
			return "sorting queued"
		case refreshReasonSearch:
			return "search queued"
		default:
			return "refresh queued"
		}
	case searchStateRunning:
		switch reason {
		case refreshReasonSort:
			return "sorting"
		case refreshReasonSearch:
			return "searching"
		default:
			return "refreshing"
		}
	case searchStateDone:
		switch reason {
		case refreshReasonSort:
			return "sort applied"
		case refreshReasonSearch:
			return "search finished"
		default:
			return "idle"
		}
	default:
		return "idle"
	}
}

func refreshReasonForQuery(query string) uint32 {
	if strings.TrimSpace(query) != "" {
		return refreshReasonSearch
	}
	return refreshReasonRefresh
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
	a.refreshReason.Store(refreshReasonForQuery(query))
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
	a.refreshReason.Store(refreshReasonRefresh)
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
	a.requestRefreshAsyncWithReason(query, sortSpec, refreshReasonForQuery(query))
}

func (a *App) requestRefreshAsyncWithReason(query string, sortSpec sorter.SortSpec, reason uint32) {
	a.refreshReason.Store(reason)
	a.setSearchState(searchStatePending)
	refreshID := atomic.AddUint64(&a.refreshID, 1)
	go a.refreshDataAsync(context.Background(), refreshID, query, sortSpec)
}

func (a *App) applySortSpec(sortSpec sorter.SortSpec) {
	a.sortSpec = sortSpec
	a.selectedCol = sortColumnIndex(a.sortSpec.Column)
	a.persistSortSpec()

	query := a.query
	a.invalidatePendingRefreshes()
	a.refreshReason.Store(refreshReasonSort)
	if strings.TrimSpace(query) == "" {
		if a.restoreEmptyQueryResults(sortSpec) {
			return
		}
	}

	a.requestRefreshAsyncWithReason(query, sortSpec, refreshReasonSort)
	a.updateStatus()
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
	a.rememberEmptyQueryResults(query, sortSpec, entries, total)
	a.cacheWarmStartResults(query, sortSpec, entries, total)
	a.tui.QueueUpdateDraw(func() {
		if refreshID != atomic.LoadUint64(&a.refreshID) {
			return
		}
		a.applyResults(entries, total)
	})
}

func (a *App) refreshData(ctx context.Context) {
	a.refreshReason.Store(refreshReasonForQuery(a.query))
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
	a.rememberEmptyQueryResults(a.query, a.sortSpec, entries, total)
	a.cacheWarmStartResults(a.query, a.sortSpec, entries, total)
	a.applyResults(entries, total)
}

func (a *App) queryEntries(ctx context.Context, query string, sortSpec sorter.SortSpec) ([]model.Entry, int, error) {
	qLower := strings.ToLower(strings.TrimSpace(query))
	queryLimit := a.cfg.MaxResults
	if qLower != "" {
		queryLimit = effectiveLiveSearchLimit(queryLimit)
	}

	queryCtx := ctx
	cancel := func() {}
	if qLower != "" {
		// Keep live search responsive while users type; empty-query refreshes may need
		// more time on very large indexes to compute sorted top-N correctly.
		queryCtx, cancel = context.WithTimeout(ctx, liveSearchTimeout)
	}
	defer cancel()

	res, err := a.storeQuery(queryCtx, query, sortSpec, queryLimit, 0)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && strings.TrimSpace(query) != "" {
			// Always update UI state on timeout instead of keeping stale rows.
			return []model.Entry{}, 0, nil
		}
		return nil, 0, err
	}
	return res.Entries, res.Total, nil
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

func (a *App) rememberEmptyQueryResults(query string, sortSpec sorter.SortSpec, entries []model.Entry, total int) {
	if strings.TrimSpace(query) != "" {
		return
	}
	result := store.QueryResult{
		Entries: append([]model.Entry(nil), entries...),
		Total:   total,
	}

	a.emptyQueryMu.Lock()
	if a.emptyQueryResults == nil {
		a.emptyQueryResults = make(map[sorter.SortSpec]store.QueryResult)
	}
	a.emptyQueryResults[sortSpec] = result
	a.emptyQueryMu.Unlock()
}

func (a *App) restoreEmptyQueryResults(sortSpec sorter.SortSpec) bool {
	a.emptyQueryMu.RLock()
	result, ok := a.emptyQueryResults[sortSpec]
	a.emptyQueryMu.RUnlock()
	if !ok {
		return false
	}
	a.applyResults(append([]model.Entry(nil), result.Entries...), result.Total)
	return true
}

func (a *App) forgetEmptyQueryResults() {
	a.emptyQueryMu.Lock()
	a.emptyQueryResults = nil
	a.emptyQueryMu.Unlock()
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
