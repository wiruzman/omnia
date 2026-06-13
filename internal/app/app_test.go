package app

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"omnia-search-tui/internal/config"
	"omnia-search-tui/internal/daemonstate"
	"omnia-search-tui/internal/indexer"
	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/progress"
	"omnia-search-tui/internal/scanner"
	"omnia-search-tui/internal/sorter"
	"omnia-search-tui/internal/startupcache"
	"omnia-search-tui/internal/store"
)

type mockSystemAdapter struct {
	openCalls   []string
	revealCalls []string
	copyCalls   []string
	trashCalls  []string
	trashErr    error
}

func (m *mockSystemAdapter) OpenPath(path string) error {
	m.openCalls = append(m.openCalls, path)
	return nil
}

func (m *mockSystemAdapter) RevealInFinder(path string) error {
	m.revealCalls = append(m.revealCalls, path)
	return nil
}

func (m *mockSystemAdapter) CopyToClipboard(text string) error {
	m.copyCalls = append(m.copyCalls, text)
	return nil
}

func (m *mockSystemAdapter) MoveToTrash(path string) error {
	m.trashCalls = append(m.trashCalls, path)
	return m.trashErr
}

func newTestApp(t *testing.T, sys *mockSystemAdapter) *App {
	t.Helper()

	// Keep config persistence inside test temp dirs; some UI paths call persistSortSpec().
	t.Setenv("HOME", t.TempDir())

	cfg := config.Config{
		IndexDBPath:   filepath.Join(t.TempDir(), "index.bleve"),
		MaxResults:    5000,
		DebounceMs:    5,
		ScanBatchSize: 100,
		DaemonDir:     filepath.Join(t.TempDir(), "daemon"),
		SortColumn:    "name",
		SortDirection: "ASC",
	}

	st, err := store.Open(cfg.IndexDBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	idx := indexer.New(cfg, scanner.New(nil), st, log.New(io.Discard, "", 0))
	a := &App{
		cfg:      cfg,
		store:    st,
		indexer:  idx,
		tui:      tview.NewApplication(),
		sortSpec: sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc},
		logger:   log.New(io.Discard, "", 0),
		system:   sys,
	}
	a.buildUI()
	return a
}

func seedEntries(t *testing.T, a *App, entries []model.Entry) {
	t.Helper()
	if err := a.store.UpsertBatch(context.Background(), time.Now().UnixNano(), entries); err != nil {
		t.Fatalf("seed entries: %v", err)
	}
	a.entries = append([]model.Entry(nil), entries...)
	a.applyResults(a.entries, len(a.entries))
}

func TestOpenRevealCopyUseSystemAdapter(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	e := model.Entry{Path: "/tmp/a.txt", Name: "a.txt", Type: model.TypeFile}
	a.entries = []model.Entry{e}
	a.selected = 0

	a.openSelected()
	a.revealSelected()
	a.copySelectedPath()

	if len(sys.openCalls) != 1 || sys.openCalls[0] != e.Path {
		t.Fatalf("open call mismatch: %+v", sys.openCalls)
	}
	if len(sys.revealCalls) != 1 || sys.revealCalls[0] != e.Path {
		t.Fatalf("reveal call mismatch: %+v", sys.revealCalls)
	}
	if len(sys.copyCalls) != 1 || sys.copyCalls[0] != e.Path {
		t.Fatalf("copy call mismatch: %+v", sys.copyCalls)
	}
}

func TestNewStartsWhenDaemonRunningUsingReadonlySnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgPath, err := config.ConfigPath()
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := config.Config{
		IncludePaths:  []string{home},
		ExcludeGlobs:  []string{".git"},
		IndexDBPath:   "index.bleve",
		StoreBackend:  "bleve",
		MaxResults:    100,
		DebounceMs:    50,
		ScanBatchSize: 100,
		DaemonDir:     "daemon",
		SortColumn:    "name",
		SortDirection: "ASC",
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	readonlyPath := loaded.IndexDBPath + ".readonly"
	roStore, err := store.Open(readonlyPath)
	if err != nil {
		t.Fatalf("open readonly snapshot store: %v", err)
	}
	now := time.Now()
	if err := roStore.UpsertBatch(context.Background(), now.UnixNano(), []model.Entry{{
		Path:       "/tmp/snapshot.txt",
		Name:       "snapshot.txt",
		ParentPath: "/tmp",
		RootPath:   "/tmp",
		Type:       model.TypeFile,
		Size:       1,
		CreatedAt:  now,
		ModifiedAt: now,
	}}); err != nil {
		_ = roStore.Close()
		t.Fatalf("seed readonly snapshot store: %v", err)
	}
	if err := roStore.Close(); err != nil {
		t.Fatalf("close readonly snapshot store: %v", err)
	}

	if err := daemonstate.Write(loaded.DaemonStatusPath(), daemonstate.Status{Running: true, Indexing: true}); err != nil {
		t.Fatalf("write daemon status: %v", err)
	}

	a, err := New()
	if err != nil {
		t.Fatalf("expected New to succeed with daemon running, got %v", err)
	}
	defer func() { _ = a.Close() }()

	count, err := a.store.Count(context.Background())
	if err != nil {
		t.Fatalf("count readonly snapshot entries: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 entry in readonly snapshot, got %d", count)
	}
}

func TestNewUsesSQLiteDirectReadOnlyStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgPath, err := config.ConfigPath()
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := config.Config{
		IncludePaths:  []string{home},
		ExcludeGlobs:  []string{".git"},
		IndexDBPath:   "index.sqlite",
		StoreBackend:  "sqlite",
		MaxResults:    100,
		DebounceMs:    50,
		ScanBatchSize: 100,
		DaemonDir:     "daemon",
		SortColumn:    "name",
		SortDirection: "ASC",
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sqliteStore, err := store.OpenWithBackend(loaded.IndexDBPath, loaded.StoreBackend)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	now := time.Now()
	if err := sqliteStore.UpsertBatch(context.Background(), now.UnixNano(), []model.Entry{{
		Path:       "/tmp/sqlite.txt",
		Name:       "sqlite.txt",
		ParentPath: "/tmp",
		RootPath:   "/tmp",
		Type:       model.TypeFile,
		Size:       1,
		CreatedAt:  now,
		ModifiedAt: now,
	}}); err != nil {
		_ = sqliteStore.Close()
		t.Fatalf("seed sqlite store: %v", err)
	}
	if err := sqliteStore.Close(); err != nil {
		t.Fatalf("close sqlite store: %v", err)
	}

	if err := daemonstate.Write(loaded.DaemonStatusPath(), daemonstate.Status{Running: true, Indexing: false}); err != nil {
		t.Fatalf("write daemon status: %v", err)
	}

	a, err := New()
	if err != nil {
		t.Fatalf("expected New to open direct sqlite store, got %v", err)
	}
	defer func() { _ = a.Close() }()

	count, err := a.store.Count(context.Background())
	if err != nil {
		t.Fatalf("count sqlite entries: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 sqlite entry, got %d", count)
	}
}

func TestDeletePathCallsTrashAndRemovesFromIndex(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	now := time.Now()
	entries := []model.Entry{
		{Path: "/tmp/a.txt", Name: "a.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 1, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/b.txt", Name: "b.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 1, CreatedAt: now, ModifiedAt: now},
	}
	seedEntries(t, a, entries)

	a.deletePath("/tmp/a.txt")

	if len(sys.trashCalls) != 1 || sys.trashCalls[0] != "/tmp/a.txt" {
		t.Fatalf("trash call mismatch: %+v", sys.trashCalls)
	}

	res, err := a.store.Query(context.Background(), "a.txt", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatalf("query deleted path: %v", err)
	}
	if res.Total != 0 {
		t.Fatalf("expected deleted path to be gone, total=%d", res.Total)
	}
}

func TestDeletePathStopsIfTrashFails(t *testing.T) {
	sys := &mockSystemAdapter{trashErr: errors.New("trash failed")}
	a := newTestApp(t, sys)
	now := time.Now()
	entries := []model.Entry{
		{Path: "/tmp/a.txt", Name: "a.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 1, CreatedAt: now, ModifiedAt: now},
	}
	seedEntries(t, a, entries)

	a.deletePath("/tmp/a.txt")

	res, err := a.store.Query(context.Background(), "a.txt", sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}, 10, 0)
	if err != nil {
		t.Fatalf("query path after failed trash: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("expected path to remain when trash fails, total=%d", res.Total)
	}
}

func TestFilterFallbackEntriesPrioritizesRelevantContains(t *testing.T) {
	entries := []model.Entry{
		{Path: "/tmp/haos_generic-aarch64-13.2.qcow2", Name: "haos_generic-aarch64-13.2.qcow2"},
		{Path: "/tmp/LogiMgr.pkg", Name: "LogiMgr.pkg"},
		{Path: "/tmp/logioptionsplus_installer_offline.zip", Name: "logioptionsplus_installer_offline.zip"},
		{Path: "/opt/vendor/pkg.bin", Name: "pkg.bin"},
	}

	filtered := filterFallbackEntries("logi", entries, 10)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 relevant entries, got %d", len(filtered))
	}
	got := map[string]bool{
		filtered[0].Name: true,
		filtered[1].Name: true,
	}
	if !got["logioptionsplus_installer_offline.zip"] || !got["LogiMgr.pkg"] {
		t.Fatalf("expected only logi-related name hits, got %#v", got)
	}
}

func TestEffectiveLiveSearchLimit(t *testing.T) {
	if got := effectiveLiveSearchLimit(5000); got != 1200 {
		t.Fatalf("expected live search cap of 1200, got %d", got)
	}
	if got := effectiveLiveSearchLimit(300); got != 300 {
		t.Fatalf("expected smaller configured max to remain unchanged, got %d", got)
	}
}

func TestClearingQueryKeepsCurrentSort(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	base := sorter.SortSpec{Column: sorter.SortModified, Direction: sorter.Desc}
	a.sortSpec = base

	a.input.SetText("log")

	// Simulate changing sort while query is active.
	active := sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}
	a.sortSpec = active

	a.input.SetText("")
	if a.sortSpec != active {
		t.Fatalf("expected sort to stay unchanged after clearing query, got %+v want %+v", a.sortSpec, active)
	}
}

func TestEscFromTableClearsQueryAndKeepsCurrentSort(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	base := sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	a.sortSpec = base

	a.input.SetText("log")

	// Simulate sort change while query is active.
	active := sorter.SortSpec{Column: sorter.SortName, Direction: sorter.Asc}
	a.sortSpec = active

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone))

	if got := a.input.GetText(); got != "" {
		t.Fatalf("expected query input to be cleared, got %q", got)
	}
	if a.sortSpec != active {
		t.Fatalf("expected ESC clear to keep current sort, got %+v want %+v", a.sortSpec, active)
	}
	if !a.resetSelectionOnNextResults {
		t.Fatalf("expected clear flow to request next-results selection reset")
	}
}

func TestApplyResultsResetsSelectionAfterClear(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	now := time.Now()
	entries := []model.Entry{
		{Path: "/tmp/a", Name: "a", Type: model.TypeFile, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/b", Name: "b", Type: model.TypeFile, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/c", Name: "c", Type: model.TypeFile, CreatedAt: now, ModifiedAt: now},
	}

	a.selected = 2
	a.selectedCol = 1
	a.table.Select(3, 1)
	a.resetSelectionOnNextResults = true

	a.applyResults(entries, len(entries))

	if a.selected != 0 {
		t.Fatalf("expected selected row to reset to 0, got %d", a.selected)
	}
	if a.selectedCol != 0 {
		t.Fatalf("expected selected col to reset to 0, got %d", a.selectedCol)
	}
	if a.resetSelectionOnNextResults {
		t.Fatalf("expected reset flag to be consumed")
	}
}

func TestApplyFirstResultsStartsAtTopOnNameAndKeepsColumnOrder(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.sortSpec = sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}
	a.selectedCol = 0

	// tview marks a short, header-only table as tracking the end. The first
	// real result set must not inherit that bottom offset or its synthetic row.
	a.table.SetOffset(365, 0)
	a.table.Select(1, 0)

	now := time.Now()
	entries := []model.Entry{
		{Path: "/tmp/large", Name: "large", Type: model.TypeFile, Size: 90, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/small", Name: "small", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
	}

	a.applyResults(entries, len(entries))

	if a.selected != 0 {
		t.Fatalf("expected first result to be selected, got %d", a.selected)
	}
	if a.selectedCol != 0 {
		t.Fatalf("expected cursor to start on name column, got %d", a.selectedCol)
	}
	row, col := a.table.GetSelection()
	if row != 1 || col != 0 {
		t.Fatalf("expected table cursor at first name cell, got row=%d col=%d", row, col)
	}
	rowOffset, colOffset := a.table.GetOffset()
	if rowOffset != 0 || colOffset != 0 {
		t.Fatalf("expected first results to start at top offset, got (%d,%d)", rowOffset, colOffset)
	}
	if got := a.table.GetCell(0, 1).Text; !strings.Contains(got, "Path") {
		t.Fatalf("expected path to remain second column, got %q", got)
	}
	if got := a.table.GetCell(0, 2).Text; !strings.Contains(got, "Type") {
		t.Fatalf("expected type to remain third column, got %q", got)
	}
	if got := a.table.GetCell(0, 3).Text; !strings.Contains(got, "Size") {
		t.Fatalf("expected size to remain fourth column, got %q", got)
	}
	if got := a.table.GetCell(1, 3).Text; got != "90 B" {
		t.Fatalf("expected size value in fourth column, got %q", got)
	}
}

func TestNewStartsOnNameColumnWithConfiguredSizeSort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgPath, err := config.ConfigPath()
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := config.Config{
		IncludePaths:  []string{home},
		ExcludeGlobs:  []string{".git"},
		IndexDBPath:   "index.bleve",
		StoreBackend:  "bleve",
		MaxResults:    100,
		DebounceMs:    50,
		ScanBatchSize: 100,
		DaemonDir:     "daemon",
		SortColumn:    "size",
		SortDirection: "DESC",
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	readonlyPath := loaded.IndexDBPath + ".readonly"
	roStore, err := store.Open(readonlyPath)
	if err != nil {
		t.Fatalf("open readonly snapshot store: %v", err)
	}
	if err := roStore.Close(); err != nil {
		t.Fatalf("close readonly snapshot store: %v", err)
	}

	a, err := New()
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer func() {
		if err := a.Close(); err != nil {
			t.Fatalf("close app: %v", err)
		}
	}()

	if a.sortSpec.Column != sorter.SortSize {
		t.Fatalf("expected configured size sort, got %s", a.sortSpec.Column)
	}
	if a.selectedCol != 0 {
		t.Fatalf("expected startup cursor on name column, got %d", a.selectedCol)
	}
	cols := a.visibleColumns()
	want := []int{0, 1, 2, 3, 4, 5}
	if len(cols) != len(want) {
		t.Fatalf("expected canonical column order %+v, got %+v", want, cols)
	}
	for i := range want {
		if cols[i] != want[i] {
			t.Fatalf("expected canonical column order %+v, got %+v", want, cols)
		}
	}
}

func TestSortKeyMovesSelectedColumnToSortColumn(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, 's', tcell.ModNone))
	if a.sortSpec.Column != sorter.SortPath {
		t.Fatalf("expected sort column path after first sort key, got %s", a.sortSpec.Column)
	}
	if a.selectedCol != sortColumnIndex(sorter.SortPath) {
		t.Fatalf("expected selected column to follow path sort, got %d", a.selectedCol)
	}

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, 's', tcell.ModNone))
	if a.sortSpec.Column != sorter.SortSize {
		t.Fatalf("expected sort column size after second sort key, got %s", a.sortSpec.Column)
	}
	if a.selectedCol != sortColumnIndex(sorter.SortSize) {
		t.Fatalf("expected selected column to follow size sort, got %d", a.selectedCol)
	}
}

func TestEndKeyKeepsHighlightedColumn(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	now := time.Now()
	entries := []model.Entry{
		{Path: "/tmp/a.txt", Name: "a.txt", Type: model.TypeFile, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/b.txt", Name: "b.txt", Type: model.TypeFile, CreatedAt: now, ModifiedAt: now},
		{Path: "/tmp/c.txt", Name: "c.txt", Type: model.TypeFile, CreatedAt: now, ModifiedAt: now},
	}
	seedEntries(t, a, entries)

	a.selectedCol = 2
	a.renderTable()
	a.table.Select(1, 0)

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone))

	row, col := a.table.GetSelection()
	if row != len(entries) {
		t.Fatalf("expected END to move to last row %d, got %d", len(entries), row)
	}
	if col != 0 {
		t.Fatalf("expected END to keep row selection at column 0, got %d", col)
	}
	if a.selected != len(entries)-1 {
		t.Fatalf("expected selected index %d, got %d", len(entries)-1, a.selected)
	}
	if a.selectedCol != 2 {
		t.Fatalf("expected logical selected column 2, got %d", a.selectedCol)
	}
}

func TestArrowRightUsesNativeHorizontalTableScroll(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.table.SetRect(0, 0, 50, 5)

	now := time.Now()
	seedEntries(t, a, []model.Entry{{
		Path:       "/tmp/" + strings.Repeat("p", 90),
		Name:       strings.Repeat("n", 40),
		ParentPath: "/tmp",
		RootPath:   "/tmp",
		Type:       model.TypeFile,
		Size:       90,
		CreatedAt:  now,
		ModifiedAt: now,
	}})

	handler := a.table.InputHandler()
	handler(tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone), func(tview.Primitive) {})

	if a.selectedCol != 0 {
		t.Fatalf("expected plain right arrow to leave selected header column unchanged, got %d", a.selectedCol)
	}
	if a.horizontalScrollCol != 1 {
		t.Fatalf("expected plain right arrow to scroll rendered columns to path, got %d", a.horizontalScrollCol)
	}
	if got := a.table.GetColumnCount(); got != len(tableHeaders)-1 {
		t.Fatalf("expected leading name column to be hidden after right scroll, got %d columns", got)
	}
	if got := a.table.GetCell(0, 0).Text; !strings.Contains(got, "Path") {
		t.Fatalf("expected path header to be first visible column, got %q", got)
	}
	row, col := a.table.GetSelection()
	if row != 1 || col != 0 {
		t.Fatalf("expected row selection to stay on column 0, got row=%d col=%d", row, col)
	}
	_, colOffset := a.table.GetOffset()
	if colOffset != 0 {
		t.Fatalf("expected internal tview column offset to remain 0, got %d", colOffset)
	}
}

func TestArrowRightStaysOnRightmostColumn(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.table.SetRect(0, 0, 50, 5)

	now := time.Now()
	seedEntries(t, a, []model.Entry{{
		Path:       "/tmp/" + strings.Repeat("p", 90),
		Name:       strings.Repeat("n", 40),
		ParentPath: "/tmp",
		RootPath:   "/tmp",
		Type:       model.TypeFile,
		Size:       90,
		CreatedAt:  now,
		ModifiedAt: now,
	}})

	handler := a.table.InputHandler()
	for range len(tableHeaders) + 2 {
		handler(tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone), func(tview.Primitive) {})
	}
	a.renderTable()

	if a.horizontalScrollCol != len(tableHeaders)-1 {
		t.Fatalf("expected rightmost scroll column %d, got %d", len(tableHeaders)-1, a.horizontalScrollCol)
	}
	if got := a.table.GetColumnCount(); got != 1 {
		t.Fatalf("expected only the rightmost column to render, got %d columns", got)
	}
	if got := a.table.GetCell(0, 0).Text; !strings.Contains(got, "Modified") {
		t.Fatalf("expected modified header to stay visible, got %q", got)
	}

	screen := tcell.NewSimulationScreen("UTF-8")
	if err := screen.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer screen.Fini()
	screen.SetSize(50, 5)
	a.table.Draw(screen)
	_, colAtRightEdge := a.table.CellAt(49, 1)
	if colAtRightEdge != 0 {
		t.Fatalf("expected rightmost column to fill right edge, got column %d", colAtRightEdge)
	}
}

func TestShiftRightSelectsHeaderColumnWithoutScrolling(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.table.SetRect(0, 0, 50, 5)

	now := time.Now()
	seedEntries(t, a, []model.Entry{{
		Path:       "/tmp/" + strings.Repeat("p", 90),
		Name:       strings.Repeat("n", 40),
		ParentPath: "/tmp",
		RootPath:   "/tmp",
		Type:       model.TypeFile,
		Size:       90,
		CreatedAt:  now,
		ModifiedAt: now,
	}})

	handler := a.table.InputHandler()
	handler(tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModShift), func(tview.Primitive) {})

	if a.selectedCol != 1 {
		t.Fatalf("expected shift-right to select path header column, got %d", a.selectedCol)
	}
	if a.horizontalScrollCol != 0 {
		t.Fatalf("expected shift-right to leave rendered column scroll unchanged, got %d", a.horizontalScrollCol)
	}
	if got := a.table.GetColumnCount(); got != len(tableHeaders) {
		t.Fatalf("expected all table columns to remain rendered, got %d", got)
	}
	if got := a.table.GetCell(0, 1).Text; !strings.Contains(got, "Path") {
		t.Fatalf("expected path header to remain at canonical column 1, got %q", got)
	}
	row, col := a.table.GetSelection()
	if row != 1 || col != 0 {
		t.Fatalf("expected row selection to stay on column 0, got row=%d col=%d", row, col)
	}
	_, colOffset := a.table.GetOffset()
	if colOffset != 0 {
		t.Fatalf("expected shift-right to leave viewport offset unchanged, got %d", colOffset)
	}
}

func TestShiftLeftWrapsSelectedHeaderColumn(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	seedEntries(t, a, []model.Entry{{Path: "/tmp/a", Name: "a", Type: model.TypeFile}})

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModShift))

	if a.selectedCol != len(tableHeaders)-1 {
		t.Fatalf("expected shift-left from first column to wrap to %d, got %d", len(tableHeaders)-1, a.selectedCol)
	}
}

func TestTypingRuneOnTableDoesNotAutoFocusSearch(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	a.tui.SetFocus(a.table)
	a.input.SetText("")

	ret := a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModNone))

	if ret == nil {
		t.Fatalf("expected unhandled table rune to bubble through")
	}
	if got := a.input.GetText(); got != "" {
		t.Fatalf("expected query input to remain empty, got %q", got)
	}
	if a.tui.GetFocus() != a.table {
		t.Fatalf("expected table to keep focus")
	}
}

func TestShiftColonFocusesSearchInput(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	a.tui.SetFocus(a.table)

	ret := a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, ':', tcell.ModShift))

	if ret != nil {
		t.Fatalf("expected Shift+: to be handled")
	}
	if a.tui.GetFocus() != a.input {
		t.Fatalf("expected search input to receive focus")
	}
}

func TestRKeyRequestsStopWhenIndexingIsRunning(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.hasDaemonStatus = true
	a.lastDaemonStatus = daemonstate.Status{Indexing: true}

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, 'r', tcell.ModNone))

	if _, err := os.Stat(a.cfg.DaemonStopPath()); err != nil {
		t.Fatalf("expected daemon stop file to be created, got: %v", err)
	}
}

func TestRKeyRequestsResumeWhenIndexingIsStopped(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.hasDaemonStatus = true
	a.lastDaemonStatus = daemonstate.Status{Indexing: false}

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, 'r', tcell.ModNone))

	if _, err := os.Stat(a.cfg.DaemonTriggerPath()); err != nil {
		t.Fatalf("expected daemon trigger file to be created, got: %v", err)
	}
}

func TestShiftRKeyRequestsFreshReindex(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.hasDaemonStatus = true
	a.lastDaemonStatus = daemonstate.Status{Indexing: false}

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, 'R', tcell.ModShift))

	if _, err := os.Stat(a.cfg.DaemonFreshStartPath()); err != nil {
		t.Fatalf("expected daemon fresh-start file to be created, got: %v", err)
	}
}

func TestShiftRKeyAlsoRequestsStopWhenRunning(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.hasDaemonStatus = true
	a.lastDaemonStatus = daemonstate.Status{Indexing: true}

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, 'R', tcell.ModShift))

	if _, err := os.Stat(a.cfg.DaemonFreshStartPath()); err != nil {
		t.Fatalf("expected daemon fresh-start file to be created, got: %v", err)
	}
	if _, err := os.Stat(a.cfg.DaemonStopPath()); err != nil {
		t.Fatalf("expected daemon stop file to be created, got: %v", err)
	}
}

func TestRKeyClearsFreshAndTriggerWhenStopping(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.hasDaemonStatus = true
	a.lastDaemonStatus = daemonstate.Status{Indexing: true}

	if err := daemonstate.TriggerReindex(a.cfg.DaemonTriggerPath()); err != nil {
		t.Fatalf("seed trigger: %v", err)
	}
	if err := daemonstate.RequestFreshReindex(a.cfg.DaemonFreshStartPath()); err != nil {
		t.Fatalf("seed fresh request: %v", err)
	}

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, 'r', tcell.ModNone))

	if _, err := os.Stat(a.cfg.DaemonTriggerPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected trigger to be cleared, err=%v", err)
	}
	if _, err := os.Stat(a.cfg.DaemonFreshStartPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected fresh request to be cleared, err=%v", err)
	}
}

func TestLoadWarmStartCacheReturnsEntriesForConfiguredSort(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.sortSpec = sorter.SortSpec{Column: sorter.SortSize, Direction: sorter.Desc}

	now := time.Now()
	res := store.QueryResult{
		Entries: []model.Entry{
			{Path: "/tmp/large.txt", Name: "large.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 90, CreatedAt: now, ModifiedAt: now},
			{Path: "/tmp/small.txt", Name: "small.txt", ParentPath: "/tmp", RootPath: "/tmp", Type: model.TypeFile, Size: 10, CreatedAt: now, ModifiedAt: now},
		},
		Total: 2,
	}

	limit := startupcache.EffectiveLimit(a.cfg.MaxResults)
	if err := startupcache.Save(startupcache.Path(a.cfg), a.sortSpec, limit, res); err != nil {
		t.Fatalf("save startup cache: %v", err)
	}

	loaded, ok, err := a.loadWarmStartCache()
	if err != nil {
		t.Fatalf("load warm start cache: %v", err)
	}
	if !ok {
		t.Fatal("expected startup cache to load")
	}
	if loaded.Total != 2 || len(loaded.Entries) != 2 {
		t.Fatalf("expected cached entries, got total=%d len=%d", loaded.Total, len(loaded.Entries))
	}
	if loaded.Entries[0].Name != "large.txt" {
		t.Fatalf("expected cached size DESC order, first=%q", loaded.Entries[0].Name)
	}
}

func TestSortColumnIndex(t *testing.T) {
	cases := []struct {
		column sorter.Column
		want   int
	}{
		{column: sorter.SortName, want: 0},
		{column: sorter.SortPath, want: 1},
		{column: sorter.SortSize, want: 3},
		{column: sorter.SortCreated, want: 4},
		{column: sorter.SortModified, want: 5},
	}
	for _, tc := range cases {
		if got := sortColumnIndex(tc.column); got != tc.want {
			t.Fatalf("sortColumnIndex(%s) = %d, want %d", tc.column, got, tc.want)
		}
	}
}

func TestRenderTableKeepsCanonicalColumnOrderWhenSizeSelected(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.selectedCol = sortColumnIndex(sorter.SortSize)

	now := time.Now()
	a.applyResults([]model.Entry{{
		Path:       "/tmp/large.txt",
		Name:       "large.txt",
		ParentPath: "/tmp",
		RootPath:   "/tmp",
		Type:       model.TypeFile,
		Size:       90,
		CreatedAt:  now,
		ModifiedAt: now,
	}}, 1)

	if got := a.table.GetCell(0, 0).Text; !strings.Contains(got, "Name") {
		t.Fatalf("expected first visible column to be Name, got %q", got)
	}
	if got := a.table.GetCell(0, 1).Text; !strings.Contains(got, "Path") {
		t.Fatalf("expected second visible column to be Path, got %q", got)
	}
	if got := a.table.GetCell(0, 2).Text; !strings.Contains(got, "Type") {
		t.Fatalf("expected third visible column to be Type, got %q", got)
	}
	if got := a.table.GetCell(0, 3).Text; !strings.Contains(got, "Size") {
		t.Fatalf("expected fourth visible column to be Size, got %q", got)
	}
	if got := a.table.GetCell(1, 3).Text; got != "90 B" {
		t.Fatalf("expected size value in fourth visible column, got %q", got)
	}
}

func TestSearchStateTextIdleWhenQueryIsEmpty(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	a.query = ""
	a.setSearchState(searchStateRunning)
	if got := a.searchStateText(); got != "idle" {
		t.Fatalf("expected empty-query search state to be idle, got %q", got)
	}

	a.query = "abc"
	a.setSearchState(searchStateRunning)
	if got := a.searchStateText(); got != "running" {
		t.Fatalf("expected non-empty query to report running, got %q", got)
	}
}

func TestUpdateStatusUsesDaemonIndexedTotalWhenQueryEmpty(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	a.hasDaemonStatus = true
	a.lastDaemonStatus = daemonstate.Status{IndexedTotal: 321}
	a.total = 12
	a.visible = 12
	a.query = ""
	a.input.SetText("")

	a.updateStatus()
	text := a.status.GetText(true)
	if !strings.Contains(text, "indexed: 321") {
		t.Fatalf("expected status to include daemon indexed total, got %q", text)
	}
}

func TestShouldAttemptSnapshotRefresh(t *testing.T) {
	now := time.Unix(1714000000, 0)

	if !shouldAttemptSnapshotRefresh(2, 1, now, now) {
		t.Fatal("expected retry when target sequence changed")
	}
	if shouldAttemptSnapshotRefresh(2, 2, now, now.Add(2*time.Second)) {
		t.Fatal("expected retry throttling for same sequence")
	}
	if !shouldAttemptSnapshotRefresh(2, 2, now, now.Add(snapshotRefreshRetryInterval+time.Second)) {
		t.Fatal("expected retry allowed after throttle interval")
	}
}

func TestShouldQueueLiveRefresh(t *testing.T) {
	now := time.Unix(1714000000, 0)

	if !shouldQueueLiveRefresh(true, "", now.Add(-3*time.Second), now, searchStateDone) {
		t.Fatal("expected live refresh when indexing, query empty, cooldown elapsed, and no in-flight refresh")
	}
	if shouldQueueLiveRefresh(true, "", now.Add(-3*time.Second), now, searchStateRunning) {
		t.Fatal("expected live refresh to be suppressed while refresh is running")
	}
	if shouldQueueLiveRefresh(true, "needle", now.Add(-3*time.Second), now, searchStateDone) {
		t.Fatal("expected live refresh to be suppressed for non-empty query")
	}
	if shouldQueueLiveRefresh(false, "", now.Add(-3*time.Second), now, searchStateDone) {
		t.Fatal("expected live refresh to be suppressed when indexing is stopped")
	}
	if shouldQueueLiveRefresh(true, "", now.Add(-time.Second), now, searchStateDone) {
		t.Fatal("expected live refresh to respect cooldown")
	}
}

func TestCurrentIndexerStatusUsesDaemonPathProgress(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	a.hasDaemonStatus = true
	a.lastDaemonStatus = daemonstate.Status{
		Indexing: true,
		PathProgress: []progress.PathProgress{{
			Root:           "/tmp",
			Scanned:        25,
			EstimatedTotal: 100,
			Percent:        25,
			CurrentPath:    "/tmp/file.txt",
		}},
	}

	st := a.currentIndexerStatus()
	if len(st.PathProgress) != 1 {
		t.Fatalf("expected 1 path progress row, got %d", len(st.PathProgress))
	}
	if st.PathProgress[0].Root != "/tmp" || st.PathProgress[0].Scanned != 25 {
		t.Fatalf("unexpected path progress row: %+v", st.PathProgress[0])
	}
}

func TestCaptureTableKeysTogglesProgressView(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, 'p', tcell.ModNone))
	front, _ := a.pages.GetFrontPage()
	if front != "progress" {
		t.Fatalf("expected progress page front, got %q", front)
	}

	a.captureTableKeys(tcell.NewEventKey(tcell.KeyRune, 'p', tcell.ModNone))
	front, _ = a.pages.GetFrontPage()
	if front != "main" {
		t.Fatalf("expected main page front after toggle, got %q", front)
	}
}

func TestCurrentPathProgressUsesDaemonStatus(t *testing.T) {
	sys := &mockSystemAdapter{}
	a := newTestApp(t, sys)
	a.hasDaemonStatus = true
	a.lastDaemonStatus = daemonstate.Status{
		PathProgress: []progress.PathProgress{{
			Root:           "/tmp",
			Scanned:        2,
			EstimatedTotal: 10,
			Percent:        20,
			CurrentPath:    "/tmp/a.txt",
		}},
	}

	rows := a.currentPathProgress()
	if len(rows) != 1 {
		t.Fatalf("expected 1 daemon path progress row, got %d", len(rows))
	}
	if rows[0].Root != "/tmp" {
		t.Fatalf("expected root /tmp, got %q", rows[0].Root)
	}
	if rows[0].Scanned != 2 {
		t.Fatalf("expected scanned > 0, got row %+v", rows[0])
	}
}
