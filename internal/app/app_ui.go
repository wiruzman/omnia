package app

import (
	"context"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"omnia-search-tui/internal/daemonstate"
)

func (a *App) requestReindexControl(freshStart bool) {
	running := a.currentIndexerStatus().Running

	if freshStart {
		if err := daemonstate.RequestFreshReindex(a.cfg.DaemonFreshStartPath()); err != nil {
			a.logger.Printf("daemon fresh reindex request failed: %v", err)
		}
		if running {
			if err := daemonstate.RequestReindexStop(a.cfg.DaemonStopPath()); err != nil {
				a.logger.Printf("daemon stop request for fresh reindex failed: %v", err)
			}
		}
		return
	}

	if running {
		if err := daemonstate.RequestReindexStop(a.cfg.DaemonStopPath()); err != nil {
			a.logger.Printf("daemon reindex stop request failed: %v", err)
		}
		if err := daemonstate.ClearReindexTrigger(a.cfg.DaemonTriggerPath()); err != nil {
			a.logger.Printf("clear daemon reindex trigger failed: %v", err)
		}
		if err := daemonstate.ClearFreshReindex(a.cfg.DaemonFreshStartPath()); err != nil {
			a.logger.Printf("clear daemon fresh reindex request failed: %v", err)
		}
		return
	}

	if err := daemonstate.ClearReindexStop(a.cfg.DaemonStopPath()); err != nil {
		a.logger.Printf("clear daemon reindex stop request failed: %v", err)
	}
	if err := daemonstate.TriggerReindex(a.cfg.DaemonTriggerPath()); err != nil {
		a.logger.Printf("daemon reindex trigger failed: %v", err)
	}
}

func (a *App) buildUI() {
	tview.Styles.PrimitiveBackgroundColor = tcell.ColorDefault
	tview.Styles.ContrastBackgroundColor = tcell.ColorDefault
	tview.Styles.MoreContrastBackgroundColor = tcell.ColorDefault

	a.input = tview.NewInputField().
		SetLabel("Search: ").
		SetFieldWidth(0)
	a.input.SetFieldBackgroundColor(tcell.ColorDefault)
	a.input.SetFieldTextColor(tcell.ColorDefault)
	a.input.SetLabelColor(tcell.ColorDefault)
	a.input.SetChangedFunc(a.handleQueryChanged)
	a.input.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyDown:
			row, col := a.table.GetSelection()
			a.table.Select(row+1, col)
			a.tui.SetFocus(a.table)
			return nil
		case tcell.KeyUp:
			row, col := a.table.GetSelection()
			if row > 1 {
				a.table.Select(row-1, col)
			}
			a.tui.SetFocus(a.table)
			return nil
		case tcell.KeyRight:
			if event.Modifiers()&tcell.ModShift != 0 {
				a.moveSelectionHorizontal(1)
				a.tui.SetFocus(a.table)
				return nil
			}
		case tcell.KeyLeft:
			if event.Modifiers()&tcell.ModShift != 0 {
				a.moveSelectionHorizontal(-1)
				a.tui.SetFocus(a.table)
				return nil
			}
		}
		return event
	})
	a.input.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEsc {
			a.clearSearch()
			a.tui.SetFocus(a.input)
		}
	})

	a.table = tview.NewTable().SetBorders(false).SetSelectable(true, false)
	a.table.SetBackgroundColor(tcell.ColorDefault)
	a.table.SetSelectedStyle(tcell.StyleDefault.Reverse(true))
	a.table.SetFixed(1, 0)
	a.table.SetSelectedFunc(func(row, _ int) {
		if row <= 0 || row-1 >= len(a.entries) {
			return
		}
		a.selected = row - 1
		a.openSelected()
	})
	a.table.SetSelectionChangedFunc(func(row, _ int) {
		if row <= 0 || row-1 >= len(a.entries) {
			return
		}
		a.selected = row - 1
	})
	a.table.SetInputCapture(a.captureTableKeys)

	a.status = tview.NewTextView().SetDynamicColors(true)
	a.status.SetBackgroundColor(tcell.ColorDefault)
	a.details = tview.NewTextView().SetDynamicColors(true)
	a.details.SetBackgroundColor(tcell.ColorDefault)
	a.details.SetBorder(true)
	a.details.SetTitle(" Details ")
	a.details.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc || (event.Key() == tcell.KeyRune && event.Rune() == ' ') {
			a.pages.HidePage("details")
			a.tui.SetFocus(a.table)
			return nil
		}
		return event
	})

	searchBox := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.input, 1, 0, false)
	searchBox.SetBorder(true)
	searchBox.SetTitle(" Search ")
	searchBox.SetBackgroundColor(tcell.ColorDefault)

	resultsBox := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.table, 0, 1, true)
	resultsBox.SetBorder(true)
	resultsBox.SetTitle(" Results ")
	resultsBox.SetBackgroundColor(tcell.ColorDefault)

	a.layout = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(searchBox, 3, 0, false).
		AddItem(resultsBox, 0, 1, true).
		AddItem(a.status, 1, 0, false)
	a.layout.SetBackgroundColor(tcell.ColorDefault)

	a.pages = tview.NewPages().
		AddPage("main", a.layout, true, true).
		AddPage("details", a.details, true, false).
		AddPage("progress", a.buildProgressPage(), true, false)
	a.pages.SetBackgroundColor(tcell.ColorDefault)

	a.renderHeader(a.visibleColumns())
	a.updateStatus()
	a.renderProgressTable()
}

func (a *App) captureTableKeys(event *tcell.EventKey) *tcell.EventKey {
	if event.Modifiers()&tcell.ModShift != 0 {
		if event.Key() == tcell.KeyRight {
			a.moveSelectionHorizontal(1)
			return nil
		}
		if event.Key() == tcell.KeyLeft {
			a.moveSelectionHorizontal(-1)
			return nil
		}
	}
	if event.Key() == tcell.KeyEnd {
		if len(a.entries) == 0 {
			return nil
		}
		a.table.Select(len(a.entries), 0)
		return nil
	}

	if event.Key() == tcell.KeyRune {
		r := event.Rune()
		switch r {
		case 'q':
			a.tui.Stop()
			return nil
		case 'p':
			a.toggleProgressView()
			return nil
		case 'r':
			a.requestReindexControl(false)
			return nil
		case 'R':
			a.requestReindexControl(true)
			return nil
		case 's':
			a.sortSpec = a.sortSpec.NextColumn()
			a.selectedCol = sortColumnIndex(a.sortSpec.Column)
			a.persistSortSpec()
			a.refreshData(context.Background())
			return nil
		case 'S':
			a.sortSpec = a.sortSpec.ToggleDirection()
			a.selectedCol = sortColumnIndex(a.sortSpec.Column)
			a.persistSortSpec()
			a.refreshData(context.Background())
			return nil
		case 'f':
			a.revealSelected()
			return nil
		case 'd':
			a.confirmDeleteSelected()
			return nil
		case 'y':
			a.copySelectedPath()
			return nil
		case ':':
			a.tui.SetFocus(a.input)
			return nil
		case 'j':
			row, col := a.table.GetSelection()
			a.table.Select(row+1, col)
			return nil
		case 'k':
			row, col := a.table.GetSelection()
			if row > 1 {
				a.table.Select(row-1, col)
			}
			return nil
		case ' ':
			a.toggleDetails()
			return nil
		}
	}

	switch event.Key() {
	case tcell.KeyEsc:
		if strings.TrimSpace(a.query) != "" || strings.TrimSpace(a.input.GetText()) != "" {
			a.clearSearch()
			a.tui.SetFocus(a.input)
			return nil
		}
		return event
	case tcell.KeyEnter:
		a.openSelected()
		return nil
	}
	return event
}

func (a *App) clearSearch() {
	a.input.SetText("")
	a.tui.SetFocus(a.input)

	// Some tview versions may not invoke ChangedFunc on programmatic SetText.
	// Ensure clear/reset logic still runs exactly once.
	if strings.TrimSpace(a.query) != "" {
		a.handleQueryChanged("")
	}
}

func (a *App) handleQueryChanged(text string) {
	previousHadQuery := strings.TrimSpace(a.query) != ""
	nextHasQuery := strings.TrimSpace(text) != ""
	if previousHadQuery && !nextHasQuery {
		a.resetSelectionOnNextResults = true
	}

	a.query = text
	// While searching, keep the table on leading columns so result changes are visible.
	a.selectedCol = 0
	if !nextHasQuery {
		// Clearing query should restore full-list results immediately.
		a.invalidatePendingRefreshes()
		a.requestRefreshAsync(a.query, a.sortSpec)
	} else {
		a.debounceRefresh()
	}
	a.updateStatus()
}
