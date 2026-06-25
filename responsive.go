package main

// responsive.go — helpers that make every table widget respect the
// terminal's current width. The bubbles/table widget doesn't
// auto-resize : we have to recompute the per-column width on every
// tea.WindowSizeMsg and call SetColumns with the new layout.
//
// The catalogue ships each column with a DECLARED width (the v1
// fixed value). Those are now treated as WEIGHTS : the total of all
// declared widths sums to "T", the available width "A" gets
// distributed proportionally — `newWidth = round(declared * A / T)`.
// A tail correction nudges the last column up/down by ≤ N so the
// rounded widths exactly hit A (no off-by-one stripe to the right of
// the last column on most terminals).
//
// We also clamp each column to a min of 4 chars so a very narrow
// terminal (e.g. 60 cols, 8 columns) still renders without a column
// collapsing to width 0 (which the widget renders as a blank, eating
// the header).

import (
	"github.com/charmbracelet/bubbles/table"
)

const (
	// columnMinWidth is the floor a column never goes below.
	// 4 chars holds 3 letters + a separator — enough to render
	// "N/A" or a short truncation.
	columnMinWidth = 4
	// tableSidePadding accounts for the table widget's outer chrome
	// (cursor column + border edges). Empirically ~4 cols.
	tableSidePadding = 4
	// tableCellPaddingCols is the per-column overhead added by the
	// Header / Cell styles. The TUI sets those to Padding(0, 0) (no
	// horizontal padding) in newHostsModel / newVMsModel / etc., so
	// the renderable cell width == col.Width and the rendered header
	// row sums to exactly sum(col.Width). If a future refactor adds
	// horizontal padding back, raise this to 2.
	tableCellPaddingCols = 0
)

// rescaleColumns returns a fresh slice of columns whose widths sum
// (approximately) to availableWidth - tableSidePadding, preserving
// the proportions of the input. The Title and any other table.Column
// fields are copied through unchanged.
//
// availableWidth ≤ 0 → returns the input unchanged (defensive : the
// terminal hasn't reported its size yet). When the proportional
// scaling would push every column below columnMinWidth (extremely
// narrow terminal), the function clamps to the min — the table will
// be slightly wider than the terminal but still renders something
// usable rather than a stripe of single-character cells.
func rescaleColumns(orig []table.Column, availableWidth int) []table.Column {
	if availableWidth <= 0 || len(orig) == 0 {
		return orig
	}
	// Total chrome = outer table chrome + per-column padding from the
	// Header/Cell style. Without subtracting the per-column padding,
	// the rendered header line (which is N cells of width + padding)
	// overflows the body box.
	usable := availableWidth - tableSidePadding - len(orig)*tableCellPaddingCols
	if usable < len(orig)*columnMinWidth {
		usable = len(orig) * columnMinWidth
	}
	var total int
	for _, c := range orig {
		total += c.Width
	}
	if total <= 0 {
		// Degenerate input : every column with declared width 0.
		// Distribute the usable width evenly.
		each := usable / len(orig)
		if each < columnMinWidth {
			each = columnMinWidth
		}
		out := make([]table.Column, len(orig))
		for i, c := range orig {
			out[i] = table.Column{Title: c.Title, Width: each}
		}
		return out
	}
	out := make([]table.Column, len(orig))
	consumed := 0
	for i, c := range orig {
		w := c.Width * usable / total
		if w < columnMinWidth {
			w = columnMinWidth
		}
		if i == len(orig)-1 {
			// Tail : the last column absorbs any rounding leftover
			// so the total lands on `usable` exactly. Still
			// respects the min.
			w = usable - consumed
			if w < columnMinWidth {
				w = columnMinWidth
			}
		}
		out[i] = table.Column{Title: c.Title, Width: w}
		consumed += w
	}
	return out
}

// applyResize is the call site every tab + resource model uses to
// react to a tea.WindowSizeMsg. Passing 0 for either dimension
// leaves that axis untouched (so a caller that only knows one of
// them — e.g. height from a parent layout — doesn't have to
// fabricate the other).
func applyResize(t *table.Model, origCols []table.Column, width, height int) {
	if width > 0 {
		t.SetColumns(rescaleColumns(origCols, width))
		t.SetWidth(width)
	}
	if height > 0 {
		t.SetHeight(height)
	}
}
