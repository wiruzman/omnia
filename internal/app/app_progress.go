package app

import (
	"fmt"
	"sort"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"omnia-search-tui/internal/progress"
)

func (a *App) buildProgressPage() tview.Primitive {
	a.progressTable = tview.NewTable().SetBorders(false).SetSelectable(false, false)
	a.progressTable.SetBackgroundColor(tcell.ColorDefault)
	a.progressTable.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc || (event.Key() == tcell.KeyRune && event.Rune() == 'p') {
			a.showMainView()
			return nil
		}
		return event
	})

	progressView := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(a.progressTable, 0, 1, true)
	progressView.SetBorder(true)
	progressView.SetTitle(" Include Paths Progress (Esc or p to close) ")
	progressView.SetBackgroundColor(tcell.ColorDefault)
	return progressView
}

func (a *App) toggleProgressView() {
	if name, _ := a.pages.GetFrontPage(); name == "progress" {
		a.showMainView()
		return
	}
	a.pages.ShowPage("progress")
	a.pages.HidePage("details")
	a.renderProgressTable()
	a.tui.SetFocus(a.progressTable)
}

func (a *App) showMainView() {
	a.pages.HidePage("progress")
	a.pages.HidePage("details")
	a.tui.SetFocus(a.table)
}

func (a *App) currentPathProgress() []progress.PathProgress {
	if !a.hasDaemonStatus {
		return nil
	}
	rows := append([]progress.PathProgress(nil), a.lastDaemonStatus.PathProgress...)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Root < rows[j].Root
	})
	return rows
}

func (a *App) renderProgressTable() {
	if a.progressTable == nil {
		return
	}
	rows := a.currentPathProgress()
	a.progressTable.Clear()

	headers := []string{"Include Path", "Scanned", "Estimated", "Progress", "Current Path"}
	for col, h := range headers {
		a.progressTable.SetCell(0, col, tview.NewTableCell(fmt.Sprintf("[::b]%s", h)).
			SetSelectable(false).
			SetBackgroundColor(tcell.ColorDefault))
	}

	if len(rows) == 0 {
		a.progressTable.SetCell(1, 0, tview.NewTableCell("No scan progress available yet").
			SetSelectable(false).
			SetBackgroundColor(tcell.ColorDefault).
			SetExpansion(1))
		return
	}

	for i, row := range rows {
		r := i + 1
		a.progressTable.SetCell(r, 0, tview.NewTableCell(trimMiddle(row.Root, 60)).SetBackgroundColor(tcell.ColorDefault))
		a.progressTable.SetCell(r, 1, tview.NewTableCell(fmt.Sprintf("%d", row.Scanned)).SetAlign(tview.AlignRight).SetBackgroundColor(tcell.ColorDefault))
		a.progressTable.SetCell(r, 2, tview.NewTableCell(fmt.Sprintf("%d", row.EstimatedTotal)).SetAlign(tview.AlignRight).SetBackgroundColor(tcell.ColorDefault))
		a.progressTable.SetCell(r, 3, tview.NewTableCell(fmt.Sprintf("%.1f%%*", row.Percent)).SetAlign(tview.AlignRight).SetBackgroundColor(tcell.ColorDefault))
		a.progressTable.SetCell(r, 4, tview.NewTableCell(trimMiddle(row.CurrentPath, 80)).SetBackgroundColor(tcell.ColorDefault).SetExpansion(1))
	}
}
