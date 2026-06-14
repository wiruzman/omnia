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
	renderWidths := a.columnRenderWidths(layouts)
	_, _, renderWidth, _ := a.table.GetInnerRect()
	a.tableRenderWidth = renderWidth
	rowOffset, _ := a.table.GetOffset()

	a.table.Clear()
	a.table.SetOffset(rowOffset, 0)
	a.renderHeader(layouts)
	for i, e := range a.entries {
		row := i + 1
		for p, layout := range layouts {
			c := layout.col
			text := a.columnText(e, c, renderWidths[p])
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
	return makeTableColumnLayouts(cols, expandingColumnPosition(cols))
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
		cellWidth := tview.TaggedStringWidth(a.columnText(entry, col, tableColumnMaxWidths[col]))
		if maxWidth := tableColumnMaxWidths[col]; maxWidth > 0 && cellWidth > maxWidth {
			cellWidth = maxWidth
		}
		if cellWidth > width {
			width = cellWidth
		}
	}
	return width
}

func (a *App) columnRenderWidths(layouts []tableColumnLayout) []int {
	widths := make([]int, len(layouts))
	_, _, netWidth, _ := a.table.GetInnerRect()
	if netWidth <= 0 {
		for p, layout := range layouts {
			widths[p] = a.tableColumnLayoutWidth(layout)
		}
		return widths
	}

	var tableWidth, expansionTotal int
	for p, layout := range layouts {
		baseWidth := a.tableColumnLayoutWidth(layout)
		if tableWidth >= netWidth {
			baseWidth = 0
		} else if tableWidth+baseWidth > netWidth {
			baseWidth = netWidth - tableWidth
		}
		widths[p] = baseWidth
		tableWidth += baseWidth + 1
		expansionTotal += layout.expansion
	}

	if tableWidth < netWidth && expansionTotal > 0 {
		toDistribute := netWidth - tableWidth
		for p, layout := range layouts {
			if expansionTotal <= 0 {
				break
			}
			extraWidth := toDistribute * layout.expansion / expansionTotal
			widths[p] += extraWidth
			toDistribute -= extraWidth
			expansionTotal -= layout.expansion
		}
	}
	return widths
}

func (a *App) tableColumnLayoutWidth(layout tableColumnLayout) int {
	width := a.tableColumnWidth(layout.col)
	if layout.maxWidth > 0 && width > layout.maxWidth {
		return layout.maxWidth
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

func expandingColumnPosition(cols []int) int {
	pathCol := sortColumnIndex(sorter.SortPath)
	for p, col := range cols {
		if col == pathCol {
			return p
		}
	}
	for p, col := range cols {
		if tableColumnAlign(col) != tview.AlignRight {
			return p
		}
	}
	return len(cols) - 1
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

func (a *App) columnText(e model.Entry, col, width int) string {
	switch col {
	case 0:
		return trimMiddle(e.Name, width)
	case 1:
		return trimMiddle(e.Path, width)
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
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	half := (max - 3) / 2
	tail := max - 3 - half
	return s[:half] + "..." + s[len(s)-tail:]
}
