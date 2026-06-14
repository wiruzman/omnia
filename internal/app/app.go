package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/daemonstate"
	"omnia-search-tui/internal/indexer"
	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/scanner"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/startupcache"
	"omnia-search-tui/internal/store"
)

type App struct {
	cfg           config.Config
	store         store.Backend
	storeMu       sync.RWMutex
	indexer       *indexer.Indexer
	tui           *tview.Application
	layout        *tview.Flex
	searchBox     *tview.Flex
	input         *tview.InputField
	shortcutHelp  *tview.TextView
	table         *tview.Table
	status        *tview.TextView
	details       *tview.TextView
	progressTable *tview.Table
	pages         *tview.Pages

	entries                     []model.Entry
	selected                    int
	selectedCol                 int
	horizontalScrollCol         int
	tableRenderWidth            int
	query                       string
	shortcutHelpVisible         bool
	sortSpec                    sorter.SortSpec
	resetSelectionOnNextResults bool
	total                       int
	visible                     int
	searchMu                    sync.Mutex
	searchDue                   *time.Timer
	searchCancel                context.CancelFunc
	searchRunID                 uint64
	searchState                 atomic.Uint32
	refreshReason               atomic.Uint32
	emptyQueryMu                sync.RWMutex
	emptyQueryResults           map[sorter.SortSpec]store.QueryResult
	deleteState                 atomic.Uint32
	deleteMu                    sync.Mutex
	deletingPath                string
	logger                      *log.Logger
	refreshID                   uint64
	system                      SystemAdapter

	lastDaemonStatus           daemonstate.Status
	hasDaemonStatus            bool
	daemonSnapshotSeq          int64
	snapshotRefreshAttemptSeq  int64
	lastSnapshotRefreshAttempt time.Time
}

func New() (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	st, err := store.OpenSQLiteReadOnly(cfg.IndexDBPath)
	if err != nil {
		// First run may not have an index yet; create it lazily.
		st, err = store.OpenSQLite(cfg.IndexDBPath)
	}
	if err != nil {
		return nil, err
	}

	logPath := filepath.Join(filepath.Dir(cfg.IndexDBPath), "omnia.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	logger := log.New(logFile, "", log.LstdFlags)

	scan := scanner.New(cfg.ExcludeGlobs)
	idx := indexer.New(cfg, scan, st, logger)

	a := &App{
		cfg:         cfg,
		store:       st,
		indexer:     idx,
		tui:         tview.NewApplication(),
		sortSpec:    sorter.SortSpec{Column: sorter.Column(cfg.SortColumn), Direction: sorter.Direction(cfg.SortDirection)},
		selectedCol: 0,
		logger:      logger,
		system:      NewMacOSAdapter(),
	}
	a.buildUI()
	return a, nil
}

func (a *App) Close() error {
	a.indexer.Stop()
	if a.searchDue != nil {
		a.searchDue.Stop()
	}
	a.cancelInFlightSearch()
	a.storeMu.Lock()
	defer a.storeMu.Unlock()
	if a.store == nil {
		return nil
	}
	err := a.store.Close()
	a.store = nil
	return err
}

func (a *App) Run(ctx context.Context) error {
	a.tui.SetRoot(a.pages, true)
	a.tui.SetFocus(a.input)

	var startOnce sync.Once
	a.tui.SetAfterDrawFunc(func(_ tcell.Screen) {
		startOnce.Do(func() {
			go a.startInitialRefresh(ctx)
		})
	})

	a.startStatusLoop(ctx)
	return a.tui.Run()
}

func (a *App) startInitialRefresh(ctx context.Context) {
	warmStarted := a.queueCachedWarmStart(ctx)
	if ctx.Err() != nil {
		return
	}
	if strings.TrimSpace(a.query) != "" {
		return
	}
	if warmStarted {
		return
	}
	a.requestRefreshAsync(a.query, a.sortSpec)
}

func (a *App) queueCachedWarmStart(ctx context.Context) bool {
	sortSpec := a.sortSpec
	res, ok, err := a.loadWarmStartCache()
	if err != nil {
		a.logger.Printf("load startup cache failed: %v", err)
	}
	if err != nil || !ok {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	a.tui.QueueUpdateDraw(func() {
		if strings.TrimSpace(a.query) != "" || len(a.entries) > 0 || a.sortSpec != sortSpec {
			return
		}
		a.rememberEmptyQueryResults("", sortSpec, res.Entries, res.Total)
		a.applyResults(res.Entries, res.Total)
	})
	return true
}

func (a *App) loadWarmStartCache() (store.QueryResult, bool, error) {
	if strings.TrimSpace(a.query) != "" {
		return store.QueryResult{}, false, nil
	}
	limit := startupcache.EffectiveLimit(a.cfg.MaxResults)
	return startupcache.Load(startupcache.Path(a.cfg), a.sortSpec, limit)
}

func (a *App) storeCount(ctx context.Context) (int, error) {
	a.storeMu.RLock()
	defer a.storeMu.RUnlock()
	if a.store == nil {
		return 0, fmt.Errorf("store is closed")
	}
	return a.store.Count(ctx)
}

func (a *App) storeQuery(ctx context.Context, query string, sortSpec sorter.SortSpec, limit, offset int) (store.QueryResult, error) {
	a.storeMu.RLock()
	defer a.storeMu.RUnlock()
	if a.store == nil {
		return store.QueryResult{}, fmt.Errorf("store is closed")
	}
	return a.store.Query(ctx, query, sortSpec, limit, offset)
}

func (a *App) storeDeletePathPrefix(ctx context.Context, path string) error {
	a.storeMu.RLock()
	defer a.storeMu.RUnlock()
	if a.store == nil {
		return fmt.Errorf("store is closed")
	}
	return a.store.DeletePathPrefix(ctx, path)
}

func (a *App) refreshStoreConnection() error {
	newStore, err := store.OpenSQLiteReadOnly(a.cfg.IndexDBPath)
	if err != nil {
		return err
	}

	a.storeMu.Lock()
	oldStore := a.store
	a.store = newStore
	a.storeMu.Unlock()

	if oldStore != nil {
		_ = oldStore.Close()
	}
	a.forgetEmptyQueryResults()
	return nil
}
