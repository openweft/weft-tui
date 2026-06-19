package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// helpBinding is one row of the help overlay : a key (or chord) plus
// a one-line description of what pressing it does.
type helpBinding struct {
	Key  string
	Desc string
}

// globalBindings are always active, regardless of which tab is in
// focus. Tab-specific bindings (cordon, remove, etc.) are listed in
// tabBindings keyed by tab index.
var globalBindings = []helpBinding{
	{"1..4", "switch tab (Hosts / VMs / Projects / Events)"},
	{"r", "refresh current tab"},
	{"?", "toggle this help overlay"},
	{"q / Ctrl+C", "quit"},
}

var hostsBindings = []helpBinding{
	{"↑/↓ or j/k", "move selection"},
	{"c", "cordon selected host"},
	{"u", "uncordon selected host"},
	{"d", "set state → down (drain prep)"},
	{"x", "remove host (asks for confirmation)"},
	{"y / n", "confirm / cancel remove (when prompted)"},
}

// helpView renders the overlay shown when the user toggles `?`. It is
// drawn centred on top of the current view in app.View. Width is the
// terminal width so we can centre it.
func (t Theme) helpView(width int) string {
	var b strings.Builder
	b.WriteString(t.Title.Render("weft TUI — keybindings"))
	b.WriteString("\n\n")
	b.WriteString(t.StatusKey.Render("Global"))
	b.WriteString("\n")
	for _, kb := range globalBindings {
		b.WriteString("  ")
		b.WriteString(t.StatusKey.Render(padKey(kb.Key)))
		b.WriteString("  ")
		b.WriteString(kb.Desc)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(t.StatusKey.Render("Hosts tab"))
	b.WriteString("\n")
	for _, kb := range hostsBindings {
		b.WriteString("  ")
		b.WriteString(t.StatusKey.Render(padKey(kb.Key)))
		b.WriteString("  ")
		b.WriteString(kb.Desc)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(t.Faint.Render("press ? again to close"))

	box := t.HelpBox.Render(b.String())
	return lipgloss.Place(width, lipgloss.Height(box), lipgloss.Center, lipgloss.Top, box)
}

// padKey right-pads a key label so the descriptions line up. 14 chars
// is enough for the longest current binding ("↑/↓ or j/k").
func padKey(k string) string {
	const width = 14
	if len(k) >= width {
		return k
	}
	return k + strings.Repeat(" ", width-len(k))
}
