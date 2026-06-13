package app

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
)

var tableHeaders = [...]string{"Name", "Path", "Type", "Size", "Created", "Modified"}
var tableColumnMaxWidths = [...]int{40, 80, 10, 12, 19, 19}

func (a *App) renderHeader(cols []int) {
	selectedCol := clampColumnIndex(a.selectedCol)
	for p, c := range cols {
		h := tableHeaders[c]
		expansion := 0
		if p == len(cols)-1 {
			expansion = 1
		}
		cell := tview.NewTableCell(fmt.Sprintf("[::b]%s", h)).
			SetSelectable(false).
			SetBackgroundColor(tcell.ColorDefault).
			SetExpansion(expansion)
		if c == selectedCol {
			cell.SetAttributes(tcell.AttrBold | tcell.AttrUnderline)
		}
		cell.SetMaxWidth(tableColumnMaxWidths[c])
		a.table.SetCell(0, p, cell)
	}
}

func (a *App) renderTable() {
	a.selectedCol = clampColumnIndex(a.selectedCol)
	cols := a.visibleColumns()

	a.table.Clear()
	a.renderHeader(cols)
	for i, e := range a.entries {
		row := i + 1
		for p, c := range cols {
			text := a.columnText(e, c)
			expansion := 0
			if p == len(cols)-1 {
				expansion = 1
			}
			cell := tview.NewTableCell(text).
				SetBackgroundColor(tcell.ColorDefault).
				SetExpansion(expansion).
				SetMaxWidth(tableColumnMaxWidths[c])
			if c == 3 {
				cell.SetAlign(tview.AlignRight)
			}
			a.table.SetCell(row, p, cell)
		}
	}
	if len(a.entries) > 0 {
		physicalCol := physicalColumnForLogical(cols, a.selectedCol)
		a.table.Select(a.selected+1, physicalCol)
	}
}

func (a *App) moveSelectionHorizontal(delta int) {
	row, _ := a.table.GetSelection()
	if row < 1 {
		row = 1
	}
	nextCol := a.selectedCol + delta
	if nextCol < 0 {
		nextCol = 0
	}
	if nextCol >= len(tableHeaders) {
		nextCol = len(tableHeaders) - 1
	}
	a.selected = row - 1
	a.selectedCol = nextCol
	a.renderTable()
}

func (a *App) visibleColumns() []int {
	return allTableColumns()
}

func (a *App) logicalColumnForPhysical(physicalCol int) int {
	cols := a.visibleColumns()
	if physicalCol < 0 || physicalCol >= len(cols) {
		return 0
	}
	return cols[physicalCol]
}

func physicalColumnForLogical(cols []int, logicalCol int) int {
	for i, c := range cols {
		if c == logicalCol {
			return i
		}
	}
	return 0
}

func allTableColumns() []int {
	cols := make([]int, len(tableHeaders))
	for i := range cols {
		cols[i] = i
	}
	return cols
}

func clampColumnIndex(col int) int {
	if col < 0 {
		return 0
	}
	if col >= len(tableHeaders) {
		return len(tableHeaders) - 1
	}
	return col
}

func sortColumnIndex(col sorter.Column) int {
	switch col {
	case sorter.SortPath:
		return 1
	case sorter.SortSize:
		return 3
	case sorter.SortCreated:
		return 4
	case sorter.SortModified:
		return 5
	default:
		return 0
	}
}

func (a *App) columnText(e model.Entry, col int) string {
	switch col {
	case 0:
		return trimMiddle(e.Name, tableColumnMaxWidths[0])
	case 1:
		return trimMiddle(e.Path, tableColumnMaxWidths[1])
	case 2:
		return string(e.Type)
	case 3:
		return formatSize(e.Size)
	case 4:
		return e.CreatedAt.Format("2006-01-02 15:04:05")
	case 5:
		return e.ModifiedAt.Format("2006-01-02 15:04:05")
	default:
		return ""
	}
}

func formatSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func trimMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := (max - 3) / 2
	return s[:half] + "..." + s[len(s)-half:]
}
