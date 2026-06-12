package app

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"omnia-search-tui/internal/model"
	"omnia-search-tui/internal/sorter"
)

func (a *App) renderHeader(startCol int) {
	headers := []string{"Name", "Path", "Type", "Size", "Created", "Modified"}
	widths := []int{40, 80, 10, 12, 19, 19}
	for p := 0; p+startCol < len(headers); p++ {
		c := startCol + p
		h := headers[c]
		expansion := 0
		if c == 5 {
			expansion = 1
		}
		cell := tview.NewTableCell(fmt.Sprintf("[::b]%s", h)).
			SetSelectable(false).
			SetBackgroundColor(tcell.ColorDefault).
			SetExpansion(expansion)
		cell.SetMaxWidth(widths[c])
		a.table.SetCell(0, p, cell)
	}
}

func (a *App) renderTable() {
	startCol := a.visibleStartForSelection()
	a.visibleStartCol = startCol
	widths := []int{40, 80, 10, 12, 19, 19}

	a.table.Clear()
	a.renderHeader(startCol)
	for i, e := range a.entries {
		row := i + 1
		for p := 0; p+startCol <= 5; p++ {
			c := startCol + p
			text := a.columnText(e, c)
			expansion := 0
			if c == 5 {
				expansion = 1
			}
			cell := tview.NewTableCell(text).
				SetBackgroundColor(tcell.ColorDefault).
				SetExpansion(expansion).
				SetMaxWidth(widths[c])
			if c == 3 {
				cell.SetAlign(tview.AlignRight)
			}
			a.table.SetCell(row, p, cell)
		}
	}
	if len(a.entries) > 0 {
		if a.selectedCol < 0 {
			a.selectedCol = 0
		}
		if a.selectedCol > 5 {
			a.selectedCol = 5
		}
		physicalCol := a.selectedCol - startCol
		if physicalCol < 0 {
			physicalCol = 0
		}
		maxPhysicalCol := 5 - startCol
		if physicalCol > maxPhysicalCol {
			physicalCol = maxPhysicalCol
		}
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
	if nextCol > 5 {
		nextCol = 5
	}
	a.selected = row - 1
	a.selectedCol = nextCol
	a.renderTable()
}

func (a *App) visibleStartForSelection() int {
	if a.selectedCol <= 0 {
		return 0
	}
	if a.selectedCol <= 3 {
		return 1
	}
	if a.selectedCol <= 4 {
		return 2
	}
	return 3
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
		return trimMiddle(e.Name, 40)
	case 1:
		return trimMiddle(e.Path, 80)
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
