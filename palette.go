package main

// palette.go — k9s-style command palette. The operator presses `:`
// to open the prompt, then EITHER types a resource id to filter
// down OR uses the arrow keys to browse the visible list, OR Tab
// to accept the current top match. Enter switches the active view
// to the highlighted resource. Esc cancels.
//
// v2 (2026-06-21) : the palette now SHOWS the catalogue right
// underneath the input line, so the operator no longer has to
// guess resource ids. Empty input lists everything ; typing
// filters by substring match (more forgiving than the v1
// prefix-only match — "vol" surfaces "volumes" + "volume-
// properties" + "volume-snapshots" + "volume-backups").

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// paletteMaxVisible bounds the list height so the palette doesn't
// dominate the viewport on small terminals. 12 entries comfortably
// fits the current 21-resource catalogue with a few scrolled — and
// keeps the bottom of the screen readable.
const paletteMaxVisible = 12

// paletteModel owns the prompt state. open=false means the palette
// isn't visible ; otherwise the chrome renders the input + a list
// of matches anchored to the bottom of the screen.
type paletteModel struct {
	open     bool
	input    string
	selected int // index into the filtered list ; clamped on every keystroke
}

// resourceIDs returns the sorted list of resource IDs the catalogue
// exposes. Used for completion + the View() footer hint when the
// palette is empty.
func resourceIDs() []string {
	ids := make([]string, 0, len(resourceCatalogue))
	for _, r := range resourceCatalogue {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// matches returns the resource ids that match the current input,
// in catalogue (alphabetical) order. Empty input → every id.
// Match is substring (case-insensitive) so partial / mid-word
// queries land : "snap" surfaces every `*-snapshots` resource.
func (p *paletteModel) matches() []string {
	ids := resourceIDs()
	if p.input == "" {
		return ids
	}
	q := strings.ToLower(p.input)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.Contains(strings.ToLower(id), q) {
			out = append(out, id)
		}
	}
	return out
}

// completion returns the top match — i.e. what Tab would accept.
// Empty when there's no match.
func (p *paletteModel) completion() string {
	m := p.matches()
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// matched reports whether the highlighted entry resolves to a real
// resource (it always should, but defensive).
func (p *paletteModel) selectedID() (string, bool) {
	m := p.matches()
	if len(m) == 0 {
		return "", false
	}
	if p.selected < 0 || p.selected >= len(m) {
		return "", false
	}
	return m[p.selected], true
}

// clampSelected keeps p.selected in range for the current filtered
// list. Called after every keystroke that might change the list
// shape (typing, backspace).
func (p *paletteModel) clampSelected() {
	m := p.matches()
	if len(m) == 0 {
		p.selected = 0
		return
	}
	if p.selected < 0 {
		p.selected = 0
	}
	if p.selected >= len(m) {
		p.selected = len(m) - 1
	}
}

// handleKey processes a keypress while the palette is open. Returns
// (newCmd, switchToResource) — switchToResource is non-empty when
// the operator Enter'd a valid resource id, signalling app.go to
// switch the active view.
func (p *paletteModel) handleKey(msg tea.KeyMsg) (cmd tea.Cmd, switchTo string) {
	switch msg.String() {
	case "esc", "ctrl+c":
		// Ctrl+C dismisses too — without it the palette
		// silently swallows the quit shortcut. Audit 2026-06-25.
		p.open = false
		p.input = ""
		p.selected = 0
	case "enter":
		if id, ok := p.selectedID(); ok {
			switchTo = id
		}
		p.open = false
		p.input = ""
		p.selected = 0
	case "tab":
		// Tab accepts the top match's literal text into the input
		// box. Useful when the operator wants to refine further
		// (e.g. tabs to "volumes" then types "-snap" to narrow).
		if c := p.completion(); c != "" {
			p.input = c
			p.selected = 0
		}
	case "up", "ctrl+p":
		p.selected--
		p.clampSelected()
	case "down", "ctrl+n":
		p.selected++
		p.clampSelected()
	case "backspace":
		if len(p.input) > 0 {
			p.input = p.input[:len(p.input)-1]
		}
		p.selected = 0
	default:
		s := msg.String()
		// Accept single printable chars + hyphen (for "dns-zones" etc.)
		if len(s) == 1 && (s == "-" || (s >= "a" && s <= "z") || (s >= "0" && s <= "9")) {
			p.input += s
			p.selected = 0
		}
	}
	return nil, switchTo
}

// View renders the prompt line + the visible slice of the filtered
// catalogue. Called by app.go's render path when open=true.
//
// Layout (anchored bottom of viewport, app.go positions it) :
//
//   : <input>_                              ← prompt line
//   ▸ networks          Network             ← matched entries, one
//     subnets           Network             per line. Selected one
//     volumes           Storage             is marked with ▸ and
//     ...                                   rendered in BadgeOK
//                                           (green).
//
// The Section column on the right helps disambiguate when two
// resources share a prefix (Network vs Storage vs Identity, etc).
func (p *paletteModel) View(theme Theme, width int) string {
	var b strings.Builder

	// Input line.
	c := p.completion()
	ghost := ""
	if c != "" && c != p.input && p.input != "" {
		// Ghost suggestion only when the user has typed something
		// AND the top match extends it — confirms that Tab will
		// land on the entry highlighted below.
		ghost = theme.Faint.Render(strings.TrimPrefix(strings.ToLower(c), strings.ToLower(p.input)))
	}
	b.WriteString(theme.BadgeOK.Render(" : "))
	b.WriteString(p.input)
	b.WriteString(ghost)
	b.WriteString(theme.Title.Render("_"))
	b.WriteString("\n")

	// Catalogue list.
	m := p.matches()
	if len(m) == 0 {
		b.WriteString(theme.StatusErr.Render("  (no match — press Esc)"))
		return b.String()
	}

	// Compute the visible window around p.selected. Simple
	// scrolling : keep the selection within the centre half of
	// the window when possible.
	start := 0
	if len(m) > paletteMaxVisible {
		// Centre the selection.
		start = p.selected - paletteMaxVisible/2
		if start < 0 {
			start = 0
		}
		if start+paletteMaxVisible > len(m) {
			start = len(m) - paletteMaxVisible
		}
	}
	end := start + paletteMaxVisible
	if end > len(m) {
		end = len(m)
	}

	// Column widths : pad the id column to the longest id in the
	// VISIBLE window so the section column lines up cleanly.
	idWidth := 0
	for i := start; i < end; i++ {
		if l := lipgloss.Width(m[i]); l > idWidth {
			idWidth = l
		}
	}

	for i := start; i < end; i++ {
		id := m[i]
		section := sectionForID(id)
		marker := "  "
		idStr := id
		secStr := section
		if i == p.selected {
			marker = theme.BadgeOK.Render("▸ ")
			idStr = theme.BadgeOK.Render(id)
			secStr = theme.StatusKey.Render(section)
		} else {
			idStr = lipgloss.NewStyle().Render(id)
			secStr = theme.Faint.Render(section)
		}
		// Pad id manually since lipgloss styles don't compose
		// width on rendered output reliably for selected rows.
		padding := strings.Repeat(" ", maxInt(0, idWidth-lipgloss.Width(id)+2))
		b.WriteString(marker)
		b.WriteString(idStr)
		b.WriteString(padding)
		b.WriteString(secStr)
		b.WriteString("\n")
	}

	if len(m) > paletteMaxVisible {
		b.WriteString(theme.Faint.Render("  (")) //
		b.WriteString(theme.Faint.Render(strings.TrimSpace(stringifyInt(p.selected+1)) + "/" + stringifyInt(len(m))))
		b.WriteString(theme.Faint.Render(" — ↑/↓ to browse, Enter to open)"))
	}
	return b.String()
}

// sectionForID looks up the Section field for a given resource id.
// Returns "" when not found — caller renders empty section anyway.
func sectionForID(id string) string {
	if cfg, ok := resourceByID(id); ok {
		return cfg.Section
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func stringifyInt(n int) string {
	// Avoid importing strconv just for this — palette is a hot UI
	// path called every frame.
	if n == 0 {
		return "0"
	}
	negative := false
	if n < 0 {
		negative = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
