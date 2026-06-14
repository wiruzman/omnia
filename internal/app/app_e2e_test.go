package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/indexer"
	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/scanner"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/startupcache"
	"omnia-search-tui/internal/store"
)

type meteredBackend struct {
	store.Backend

	mu      sync.Mutex
	batches []int
}

type blockingStartupBackend struct {
	store.Backend

	queryStarted     chan struct{}
	releaseQuery     chan struct{}
	queryStartedOnce sync.Once
}

func (b *blockingStartupBackend) Query(ctx context.Context, query string, sort sorter.SortSpec, limit, offset int) (store.QueryResult, error) {
	if strings.TrimSpace(query) == "" {
		b.queryStartedOnce.Do(func() {
			close(b.queryStarted)
		})
		select {
		case <-b.releaseQuery:
		case <-ctx.Done():
			return store.QueryResult{}, ctx.Err()
		}
	}
	return b.Backend.Query(ctx, query, sort, limit, offset)
}

func (m *meteredBackend) UpsertBatch(ctx context.Context, scanID int64, batch []model.Entry) error {
	m.mu.Lock()
	m.batches = append(m.batches, len(batch))
	m.mu.Unlock()
	return m.Backend.UpsertBatch(ctx, scanID, batch)
}

func (m *meteredBackend) batchSizes() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.batches...)
}

func newE2EApp(t *testing.T, root string, batchSize int) (*App, *meteredBackend) {
	t.Helper()

	t.Setenv("HOME", t.TempDir())

	cfg := config.Config{
		IncludePaths: []string{root},
		ExcludeGlobs: []string{
			".git",
			"node_modules",
			"Library/Caches",
			".Trash",
			"Trash",
		},
		IndexDBPath:   filepath.Join(t.TempDir(), "index.bleve"),
		StoreBackend:  "bleve",
		MaxResults:    5000,
		DebounceMs:    5,
		ScanBatchSize: batchSize,
		DaemonDir:     filepath.Join(t.TempDir(), "daemon"),
		SortColumn:    "name",
		SortDirection: "ASC",
	}

	st, err := store.Open(cfg.IndexDBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	metered := &meteredBackend{Backend: st}

	idx := indexer.New(cfg, scanner.New(cfg.ExcludeGlobs), metered, log.New(io.Discard, "", 0))
	a := &App{
		cfg:      cfg,
		store:    metered,
		indexer:  idx,
		tui:      tview.NewApplication(),
		sortSpec: sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc},
		logger:   log.New(io.Discard, "", 0),
		system:   &mockSystemAdapter{},
	}
	a.buildUI()

	t.Cleanup(func() {
		_ = a.Close()
	})
	return a, metered
}

func runReindexAndWait(t *testing.T, a *App) indexer.Status {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := a.indexer.StartReindex(ctx); err != nil {
		t.Fatalf("start reindex: %v", err)
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for reindex: %v", ctx.Err())
		case <-ticker.C:
			st := a.indexer.CurrentStatus()
			if st.Running {
				continue
			}
			if st.StartedAt.IsZero() {
				continue
			}
			if st.LastError != "" {
				t.Fatalf("reindex failed: %s", st.LastError)
			}
			return st
		}
	}
}

func createSearchFixture(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "Documents", "Reports"))
	mustMkdir(t, filepath.Join(root, "Projects", "Omnia", "Quarterly"))
	mustMkdir(t, filepath.Join(root, "Media"))
	mustMkdir(t, filepath.Join(root, "node_modules", "high-churn-package"))
	mustMkdir(t, filepath.Join(root, "Library", "Caches", "omnia"))
	mustMkdir(t, filepath.Join(root, ".git", "objects"))

	for i := 0; i < 180; i++ {
		writeFile(t, filepath.Join(root, "Documents", "Reports", fmt.Sprintf("team-search-target-%03d.txt", i)), "indexed")
	}
	for i := 0; i < 90; i++ {
		writeFile(t, filepath.Join(root, "Media", fmt.Sprintf("photo-%03d.raw", i)), "indexed")
	}
	writeFile(t, filepath.Join(root, "Projects", "Omnia", "Quarterly", "Alpha-Needle-Plan.md"), "indexed")

	for i := 0; i < 650; i++ {
		writeFile(t, filepath.Join(root, "node_modules", "high-churn-package", fmt.Sprintf("excluded-target-%03d.js", i)), "ignored")
	}
	for i := 0; i < 120; i++ {
		writeFile(t, filepath.Join(root, "Library", "Caches", "omnia", fmt.Sprintf("cached-target-%03d.bin", i)), "ignored")
	}
	writeFile(t, filepath.Join(root, ".git", "objects", "excluded-commit-object"), "ignored")

	return root
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func queryThroughTUI(t *testing.T, a *App, query string) []model.Entry {
	t.Helper()

	a.query = query
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	a.refreshData(ctx)
	if err := ctx.Err(); err != nil {
		t.Fatalf("TUI query %q exceeded timeout after %s: %v", query, time.Since(start), err)
	}
	return append([]model.Entry(nil), a.entries...)
}

func TestE2EIndexerBuildsBatchedSearchableIndexForTUI(t *testing.T) {
	root := createSearchFixture(t)
	const batchSize = 64
	a, metered := newE2EApp(t, root, batchSize)

	st := runReindexAndWait(t, a)
	if st.Scanned == 0 {
		t.Fatal("expected reindex to scan entries")
	}

	batches := metered.batchSizes()
	if len(batches) < 4 {
		t.Fatalf("expected multiple index batches, got %v", batches)
	}
	for _, size := range batches {
		if size <= 0 || size > batchSize {
			t.Fatalf("batch size %d outside expected range 1..%d; all batches=%v", size, batchSize, batches)
		}
	}

	needle := queryThroughTUI(t, a, "alpha-needle")
	if len(needle) != 1 || needle[0].Name != "Alpha-Needle-Plan.md" {
		t.Fatalf("expected Alpha-Needle-Plan.md through TUI search, got %+v; diagnostics=%+v", namesOf(needle), queryDiagnostics(t, a, "", "alpha", "needle", "plan", "team", "Alpha-Needle-Plan.md"))
	}

	targets := queryThroughTUI(t, a, "team-search-target")
	if len(targets) != 180 {
		t.Fatalf("expected 180 indexed target files through TUI search, got %d", len(targets))
	}

	for _, query := range []string{"excluded-target", "cached-target", "excluded-commit-object"} {
		if hits := queryThroughTUI(t, a, query); len(hits) != 0 {
			t.Fatalf("expected excluded query %q to return no hits, got %+v", query, namesOf(hits))
		}
	}
}

func TestE2EReindexCleansDeletedFilesAndFindsNewFilesThroughTUI(t *testing.T) {
	root := createSearchFixture(t)
	a, _ := newE2EApp(t, root, 80)
	runReindexAndWait(t, a)

	oldPath := filepath.Join(root, "Projects", "Omnia", "Quarterly", "Alpha-Needle-Plan.md")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old indexed file: %v", err)
	}
	writeFile(t, filepath.Join(root, "Projects", "Omnia", "Quarterly", "Beta-Needle-Plan.md"), "new indexed file")

	runReindexAndWait(t, a)

	oldHits := queryThroughTUI(t, a, "alpha-needle")
	if len(oldHits) != 0 {
		t.Fatalf("expected deleted file to be removed after reindex, got %+v", namesOf(oldHits))
	}

	newHits := queryThroughTUI(t, a, "beta-needle")
	if len(newHits) != 1 || newHits[0].Name != "Beta-Needle-Plan.md" {
		t.Fatalf("expected new file after reindex, got %+v; diagnostics=%+v", namesOf(newHits), queryDiagnostics(t, a, "", "beta", "needle", "plan", "Beta-Needle-Plan.md"))
	}
}

func TestE2ELiveTUISearchIsCappedAndResponsiveOnLargeIndex(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	now := time.Now()
	scanID := now.UnixMicro()
	batch := make([]model.Entry, 0, 500)
	flush := func() {
		t.Helper()
		if len(batch) == 0 {
			return
		}
		if err := a.store.UpsertBatch(context.Background(), scanID, batch); err != nil {
			t.Fatalf("seed large index: %v", err)
		}
		batch = batch[:0]
	}

	for i := 0; i < 1600; i++ {
		path := fmt.Sprintf("/fixture/work/needle-result-%04d.txt", i)
		batch = append(batch, model.Entry{
			Path:       path,
			Name:       filepath.Base(path),
			ParentPath: filepath.Dir(path),
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       int64(i),
			CreatedAt:  now,
			ModifiedAt: now,
		})
		if len(batch) == cap(batch) {
			flush()
		}
	}
	for i := 0; i < 2400; i++ {
		path := fmt.Sprintf("/fixture/archive/report-%04d.txt", i)
		batch = append(batch, model.Entry{
			Path:       path,
			Name:       filepath.Base(path),
			ParentPath: filepath.Dir(path),
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       int64(i),
			CreatedAt:  now,
			ModifiedAt: now,
		})
		if len(batch) == cap(batch) {
			flush()
		}
	}
	flush()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	a.query = "needle"
	start := time.Now()
	a.refreshData(ctx)
	elapsed := time.Since(start)
	if err := ctx.Err(); err != nil {
		t.Fatalf("live TUI search exceeded timeout after %s: %v", elapsed, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected live TUI search to finish inside timeout, took %s", elapsed)
	}
	if len(a.entries) != liveSearchLimitCap {
		t.Fatalf("expected live search to cap visible rows at %d, got %d", liveSearchLimitCap, len(a.entries))
	}
	if a.visible != len(a.entries) {
		t.Fatalf("expected visible count to match entries, visible=%d entries=%d", a.visible, len(a.entries))
	}
	for _, e := range a.entries {
		if !strings.HasPrefix(e.Name, "needle-result-") {
			t.Fatalf("expected capped live results to stay on cheap name-prefix hits, got %q", e.Name)
		}
	}
	if got := a.searchStateText(); got != "finished" {
		t.Fatalf("expected finished search state, got %q", got)
	}
}

func TestE2EStartupPreviewDoesNotBlockFirstSearch(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.sortSpec = sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	a.selectedCol = 0

	now := time.Now()
	cacheResult := store.QueryResult{
		Entries: []model.Entry{{
			Path:       "/fixture/quick-preview.txt",
			Name:       "quick-preview.txt",
			ParentPath: "/fixture",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       90,
			CreatedAt:  now,
			ModifiedAt: now,
		}},
		Total: 1,
	}
	cacheLimit := startupcache.EffectiveLimit(a.cfg.MaxResults)
	if err := startupcache.Save(startupcache.Path(a.cfg), a.sortSpec, cacheLimit, cacheResult); err != nil {
		t.Fatalf("seed startup cache: %v", err)
	}

	sqlEntry := model.Entry{
		Path:       "/fixture/sql-guide.txt",
		Name:       "sql-guide.txt",
		ParentPath: "/fixture",
		RootPath:   "/fixture",
		Type:       model.TypeFile,
		Size:       10,
		CreatedAt:  now,
		ModifiedAt: now,
	}
	if err := a.store.UpsertBatch(context.Background(), now.UnixMicro(), []model.Entry{sqlEntry}); err != nil {
		t.Fatalf("seed sql search entry: %v", err)
	}

	backend := &blockingStartupBackend{
		Backend:      a.store,
		queryStarted: make(chan struct{}),
		releaseQuery: make(chan struct{}),
	}
	a.store = backend

	var releaseOnce sync.Once
	defer releaseOnce.Do(func() {
		close(backend.releaseQuery)
	})

	screen := startSimulatedTUI(t, a, 120, 25)
	waitForScreenText(t, screen, "Size", 2*time.Second)
	waitForScreenText(t, screen, "90 B", 2*time.Second)
	waitForScreenText(t, screen, "quick-preview.txt", 2*time.Second)

	select {
	case <-backend.queryStarted:
		t.Fatal("expected cached startup preview not to start a blocking empty refresh")
	case <-time.After(200 * time.Millisecond):
	}

	if !screen.InjectKeyBytes([]byte("sql")) {
		t.Fatal("failed to inject first search query")
	}
	waitForScreenText(t, screen, "sql-guide.txt", 2*time.Second)
	waitForScreenText(t, screen, "query: sql", 2*time.Second)

	releaseOnce.Do(func() {
		close(backend.releaseQuery)
	})
}

func TestE2ESortChangeDoesNotBlockUIWhileResultsRefresh(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	now := time.Now()
	preview := store.QueryResult{
		Entries: []model.Entry{{
			Path:       "/fixture/current.txt",
			Name:       "current.txt",
			ParentPath: "/fixture",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			CreatedAt:  now,
			ModifiedAt: now,
		}},
		Total: 1,
	}
	cacheLimit := startupcache.EffectiveLimit(a.cfg.MaxResults)
	if err := startupcache.Save(startupcache.Path(a.cfg), a.sortSpec, cacheLimit, preview); err != nil {
		t.Fatalf("seed startup cache: %v", err)
	}

	backend := &blockingStartupBackend{
		Backend:      a.store,
		queryStarted: make(chan struct{}),
		releaseQuery: make(chan struct{}),
	}
	a.store = backend

	var releaseOnce sync.Once
	defer releaseOnce.Do(func() {
		close(backend.releaseQuery)
	})

	screen := startSimulatedTUI(t, a, 120, 25)
	waitForScreenText(t, screen, "current.txt", 2*time.Second)

	a.tui.QueueUpdate(func() {
		a.tui.SetFocus(a.table)
	})
	if !screen.InjectKeyBytes([]byte("s")) {
		t.Fatal("failed to inject sort key")
	}

	select {
	case <-backend.queryStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected sort change to start a background refresh")
	}
	waitForScreenText(t, screen, "activity: sorting", 500*time.Millisecond)
	waitForScreenText(t, screen, "sort: path ASC", 500*time.Millisecond)

	if !screen.InjectKeyBytes([]byte(":x")) {
		t.Fatal("failed to inject input after sort key")
	}
	waitForScreenText(t, screen, "query: x", 500*time.Millisecond)

	releaseOnce.Do(func() {
		close(backend.releaseQuery)
	})
}

func TestE2ESimulatedTerminalTUISearchRendersAndOpensResult(t *testing.T) {
	root := createSearchFixture(t)
	a, _ := newE2EApp(t, root, 64)
	runReindexAndWait(t, a)

	screen := startSimulatedTUI(t, a, 140, 35)
	if !screen.InjectKeyBytes([]byte("alpha-needle")) {
		t.Fatal("failed to inject query bytes")
	}

	waitForScreenText(t, screen, "Alpha-Needle-Plan.md", 5*time.Second)
	waitForScreenText(t, screen, "query: alpha-needle", 5*time.Second)

	screen.InjectKey(tcell.KeyDown, 0, tcell.ModNone)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)

	waitForCondition(t, 2*time.Second, func() bool {
		opened := false
		a.tui.QueueUpdate(func() {
			sys := a.system.(*mockSystemAdapter)
			opened = len(sys.openCalls) == 1 &&
				filepath.Base(sys.openCalls[0]) == "Alpha-Needle-Plan.md"
		})
		return opened
	}, "expected Enter in the rendered TUI table to open Alpha-Needle-Plan.md")
}

func TestE2ESearchAfterCachedClearFindsShortTerm(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.cfg.DebounceMs = 5

	now := time.Now()
	storeEntries := []model.Entry{
		{
			Path:       "/fixture/cache/alpha-initial.txt",
			Name:       "alpha-initial.txt",
			ParentPath: "/fixture/cache",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       1,
			CreatedAt:  now,
			ModifiedAt: now,
		},
		{
			Path:       "/fixture/tools/copilot-language-server",
			Name:       "copilot-language-server",
			ParentPath: "/fixture/tools",
			RootPath:   "/fixture",
			Type:       model.TypeFile,
			Size:       2,
			CreatedAt:  now,
			ModifiedAt: now,
		},
	}
	if err := a.store.UpsertBatch(context.Background(), now.UnixMicro(), storeEntries); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	a.rememberEmptyQueryResults("", a.sortSpec, storeEntries[:1], 1)

	screen := startSimulatedTUI(t, a, 140, 35)
	if !screen.InjectKeyBytes([]byte("alpha")) {
		t.Fatal("failed to inject initial query")
	}
	waitForScreenText(t, screen, "alpha-initial.txt", 2*time.Second)

	screen.InjectKey(tcell.KeyEsc, 0, tcell.ModNone)
	waitForScreenText(t, screen, "alpha-initial.txt", 2*time.Second)

	if !screen.InjectKeyBytes([]byte("cop")) {
		t.Fatal("failed to inject short query")
	}
	waitForScreenText(t, screen, "copilot-language-server", 2*time.Second)
	waitForScreenText(t, screen, "query: cop", 2*time.Second)
}

func namesOf(entries []model.Entry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

func queryDiagnostics(t *testing.T, a *App, queries ...string) map[string][]string {
	t.Helper()

	out := make(map[string][]string, len(queries))
	for _, query := range queries {
		res, err := a.store.Query(context.Background(), query, a.sortSpec, 5, 0)
		if err != nil {
			out[query] = []string{"error: " + err.Error()}
			continue
		}
		out[query] = namesOf(res.Entries)
	}
	return out
}

func startSimulatedTUI(t *testing.T, a *App, width, height int) tcell.SimulationScreen {
	t.Helper()

	screen := tcell.NewSimulationScreen("UTF-8")
	a.tui.SetScreen(screen)
	screen.SetSize(width, height)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.Run(ctx)
	}()

	t.Cleanup(func() {
		cancel()
		a.tui.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("simulated TUI run failed: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out stopping simulated TUI")
		}
	})

	waitForScreenText(t, screen, "Search:", 2*time.Second)
	return screen
}

func waitForScreenText(t *testing.T, screen tcell.SimulationScreen, want string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	text := simulatedScreenText(screen)
	for time.Now().Before(deadline) {
		text = simulatedScreenText(screen)
		if strings.Contains(text, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	text = simulatedScreenText(screen)
	if strings.Contains(text, want) {
		return
	}
	t.Fatalf("expected screen to contain %q; screen:\n%s", want, text)
}

func waitForCondition(t *testing.T, timeout time.Duration, ok func() bool, failure string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ok() {
		return
	}
	t.Fatal(failure)
}

func simulatedScreenText(screen tcell.SimulationScreen) string {
	cells, width, height := screen.GetContents()
	var b strings.Builder
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			cell := cells[y*width+x]
			if len(cell.Runes) == 0 {
				b.WriteByte(' ')
				continue
			}
			for _, r := range cell.Runes {
				if r == 0 {
					b.WriteByte(' ')
					continue
				}
				b.WriteRune(r)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
