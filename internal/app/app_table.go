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

type tableColumnLayout struct {
	col       int
	maxWidth  int
	expansion int
}

func (a *App) renderHeader(layouts []tableColumnLayout) {
	selectedCol := clampColumnIndex(a.selectedCol)
	for p, layout := range layouts {
		c := layout.col
		h := tableHeaders[c]
		cell := tview.NewTableCell(fmt.Sprintf("[::b]%s", h)).
			SetSelectable(false).
			SetBackgroundColor(tcell.ColorDefault).
			SetExpansion(0)
		if align := tableColumnAlign(c); align != tview.AlignLeft {
			cell.SetAlign(align)
		}
		if c == selectedCol {
			cell.SetAttributes(tcell.AttrBold | tcell.AttrUnderline)
		}
		cell.SetMaxWidth(layout.maxWidth)
		a.table.SetCell(0, p, cell)
	}
}

func (a *App) renderTable() {
	a.selectedCol = clampColumnIndex(a.selectedCol)
	a.horizontalScrollCol = a.clampHorizontalScrollCol(a.horizontalScrollCol)
	layouts := a.visibleColumnLayouts()
	rowOffset, _ := a.table.GetOffset()

	a.table.Clear()
	a.table.SetOffset(rowOffset, 0)
	a.renderHeader(layouts)
	for i, e := range a.entries {
		row := i + 1
		for p, layout := range layouts {
			c := layout.col
			text := a.columnText(e, c)
			cell := tview.NewTableCell(text).
				SetBackgroundColor(tcell.ColorDefault).
				SetExpansion(layout.expansion).
				SetMaxWidth(layout.maxWidth)
			cell.SetAlign(tableColumnAlign(c))
			a.table.SetCell(row, p, cell)
		}
	}
	if len(a.entries) > 0 {
		a.table.Select(a.selected+1, 0)
	}
}

func (a *App) moveSelectionHorizontal(delta int) {
	row, _ := a.table.GetSelection()
	if row < 1 {
		row = 1
	}
	nextCol := (a.selectedCol + delta) % len(tableHeaders)
	if nextCol < 0 {
		nextCol += len(tableHeaders)
	}
	a.selected = row - 1
	a.selectedCol = nextCol
	a.renderTable()
}

func (a *App) scrollColumnsHorizontal(delta int) {
	nextCol := a.horizontalScrollCol + delta
	nextCol = a.clampHorizontalScrollCol(nextCol)
	if nextCol == a.horizontalScrollCol {
		return
	}
	a.horizontalScrollCol = nextCol
	a.renderTable()
}

func (a *App) visibleColumns() []int {
	layouts := a.visibleColumnLayouts()
	cols := make([]int, len(layouts))
	for i, layout := range layouts {
		cols[i] = layout.col
	}
	return cols
}

func (a *App) visibleColumnLayouts() []tableColumnLayout {
	cols := tableColumnsFrom(a.horizontalScrollCol)
	if a.horizontalScrollCol > 0 && a.horizontalScrollCol == a.maxHorizontalScrollCol() {
		return a.rightmostColumnLayouts(cols)
	}
	return makeTableColumnLayouts(cols, len(cols)-1)
}

func (a *App) rightmostColumnLayouts(cols []int) []tableColumnLayout {
	layouts := makeTableColumnLayouts(cols, -1)
	_, _, width, _ := a.table.GetInnerRect()
	if width <= 0 {
		return layouts
	}

	remaining := width - a.columnsDisplayWidth(cols)
	for col := a.horizontalScrollCol - 1; col >= 0 && remaining > 1; col-- {
		colWidth := a.tableColumnWidth(col)
		maxWidth := remaining - 1
		if colWidth < maxWidth {
			maxWidth = colWidth
		}
		if maxWidth <= 0 {
			break
		}
		layouts = append([]tableColumnLayout{{
			col:      col,
			maxWidth: maxWidth,
		}}, layouts...)
		remaining -= maxWidth + 1
		if maxWidth < colWidth {
			break
		}
	}
	return layouts
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
	return tableColumnsFrom(0)
}

func tableColumnsFrom(startCol int) []int {
	startCol = clampColumnIndex(startCol)
	cols := make([]int, len(tableHeaders)-startCol)
	for i := range cols {
		cols[i] = startCol + i
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

func (a *App) clampHorizontalScrollCol(col int) int {
	if col < 0 {
		return 0
	}
	maxCol := a.maxHorizontalScrollCol()
	if col > maxCol {
		return maxCol
	}
	return col
}

func (a *App) maxHorizontalScrollCol() int {
	lastCol := len(tableHeaders) - 1
	if lastCol <= 0 {
		return 0
	}

	_, _, width, _ := a.table.GetInnerRect()
	if width <= 0 {
		return lastCol
	}
	for startCol := 0; startCol <= lastCol; startCol++ {
		if a.columnsFitThroughLast(startCol, width) {
			return startCol
		}
	}
	return lastCol
}

func (a *App) columnsFitThroughLast(startCol, width int) bool {
	return a.columnsDisplayWidth(tableColumnsFrom(startCol)) <= width
}

func (a *App) columnsDisplayWidth(cols []int) int {
	if len(cols) == 0 {
		return 0
	}
	used := len(cols) - 1
	for _, col := range cols {
		used += a.tableColumnWidth(col)
	}
	return used
}

func (a *App) tableColumnWidth(col int) int {
	col = clampColumnIndex(col)
	width := tview.TaggedStringWidth(tableHeaders[col])
	for _, entry := range a.entries {
		cellWidth := tview.TaggedStringWidth(a.columnText(entry, col))
		if maxWidth := tableColumnMaxWidths[col]; maxWidth > 0 && cellWidth > maxWidth {
			cellWidth = maxWidth
		}
		if cellWidth > width {
			width = cellWidth
		}
	}
	return width
}

func makeTableColumnLayouts(cols []int, expandingPosition int) []tableColumnLayout {
	layouts := make([]tableColumnLayout, len(cols))
	for p, col := range cols {
		expansion := 0
		if p == expandingPosition {
			expansion = 1
		}
		layouts[p] = tableColumnLayout{
			col:       col,
			maxWidth:  tableColumnMaxWidths[col],
			expansion: expansion,
		}
	}
	return layouts
}

func tableColumnAlign(col int) int {
	switch col {
	case 3, len(tableHeaders) - 1:
		return tview.AlignRight
	default:
		return tview.AlignLeft
	}
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
