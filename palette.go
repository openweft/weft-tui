package main

// palette.go — k9s-style command palette. The operator presses `:`
// to open the prompt, types a resource id (`networks`, `volumes`,
// `tenants`), then Enter switches the active view to the matching
// ResourceListModel. Esc cancels.
//
// Completion : partial input matches against resource IDs and the
// best prefix match is shown beside the cursor as a ghost hint.
// Tab accepts the hint.

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// paletteModel owns the prompt state. open=false means the palette
// isn't visible ; otherwise the chrome renders a one-line input
// fixed to the bottom of the screen.
type paletteModel struct {
	open  bool
	input string
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

// completion returns the best prefix-match suggestion for the
// current input. Empty input → no suggestion. Empty return = no
// match.
func (p *paletteModel) completion() string {
	if p.input == "" {
		return ""
	}
	for _, id := range resourceIDs() {
		if strings.HasPrefix(id, p.input) {
			return id
		}
	}
	return ""
}

// matched reports whether the current input matches a known
// resource exactly. Used by Enter handling.
func (p *paletteModel) matched() (ResourceConfig, bool) {
	return resourceByID(p.input)
}

// handleKey processes a keypress while the palette is open. Returns
// (newCmd, switchToResource) — switchToResource is non-empty when
// the operator Enter'd a valid resource id, signalling app.go to
// switch the active view.
func (p *paletteModel) handleKey(msg tea.KeyMsg) (cmd tea.Cmd, switchTo string) {
	switch msg.String() {
	case "esc":
		p.open = false
		p.input = ""
	case "enter":
		if _, ok := p.matched(); ok {
			switchTo = p.input
		}
		p.open = false
		p.input = ""
	case "tab":
		if c := p.completion(); c != "" {
			p.input = c
		}
	case "backspace":
		if len(p.input) > 0 {
			p.input = p.input[:len(p.input)-1]
		}
	default:
		s := msg.String()
		// Accept single printable chars + hyphen (for "dns-zones" etc.)
		if len(s) == 1 && (s == "-" || (s >= "a" && s <= "z") || (s >= "0" && s <= "9")) {
			p.input += s
		}
	}
	return nil, switchTo
}

// View renders the prompt line. Called by app.go's render path when
// open=true.
func (p *paletteModel) View(theme Theme, width int) string {
	c := p.completion()
	ghost := ""
	if c != "" && c != p.input {
		ghost = theme.Faint.Render(strings.TrimPrefix(c, p.input))
	}
	return theme.BadgeOK.Render(" : ") + p.input + ghost + theme.Title.Render("_")
}
