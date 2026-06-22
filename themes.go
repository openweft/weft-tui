package main

// themes.go — the catalogue of preset colour themes the operator
// can cycle through with the `T` key. Persists the choice in
// ~/.weft/tui-theme so a fresh run on the same machine remembers
// the operator's preference.
//
// One Theme is the bundle Lip Gloss consumes for every view
// (styles.go). Each ThemePreset here just supplies the SIX hue
// values (primary + 5 accent / state colours) ; NewThemeWith
// expands them into the full Theme struct using the same shape
// styles.go's NewTheme uses.

import (
	"os"
	"path/filepath"

	"github.com/charmbracelet/lipgloss"
)

// ThemePreset bundles the hue values that distinguish one theme
// from the next. Light + Dark variants for every value so the
// theme reads well on either terminal background — same convention
// styles.go has always followed.
type ThemePreset struct {
	Name      string
	Primary   lipgloss.AdaptiveColor // titles, active tab, prompt prefix
	Muted     lipgloss.AdaptiveColor // tabs, faint, status bar
	OK        lipgloss.AdaptiveColor // success badges + matching messages
	Warn      lipgloss.AdaptiveColor // warning badges
	Bad       lipgloss.AdaptiveColor // error badges, confirm-box border
	Border    lipgloss.AdaptiveColor // box borders, status-bar separator
}

// themePresets is the catalogue. Order = cycle order on `T`.
// Green ships first — 2026-06-21 operator preference.
var themePresets = []ThemePreset{
	{
		Name:    "green",
		Primary: lipgloss.AdaptiveColor{Light: "#16A34A", Dark: "#4ADE80"},
		Muted:   lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"},
		OK:      lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#86EFAC"},
		Warn:    lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FCD34D"},
		Bad:     lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"},
		Border:  lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"},
	},
	{
		Name:    "blue",
		Primary: lipgloss.AdaptiveColor{Light: "#1D4ED8", Dark: "#60A5FA"},
		Muted:   lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"},
		OK:      lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#86EFAC"},
		Warn:    lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FCD34D"},
		Bad:     lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"},
		Border:  lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"},
	},
	{
		Name:    "amber",
		Primary: lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FCD34D"},
		Muted:   lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"},
		OK:      lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#86EFAC"},
		Warn:    lipgloss.AdaptiveColor{Light: "#92400E", Dark: "#F59E0B"},
		Bad:     lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"},
		Border:  lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"},
	},
	{
		Name:    "violet",
		// Original pre-v0.3.3 hues, kept as an opt-in for operators
		// who liked the look.
		Primary: lipgloss.AdaptiveColor{Light: "#5B21B6", Dark: "#A78BFA"},
		Muted:   lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"},
		OK:      lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#86EFAC"},
		Warn:    lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FCD34D"},
		Bad:     lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"},
		Border:  lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"},
	},
	{
		Name:    "mono",
		// Monochrome — for screenshots, low-contrast terminals,
		// accessibility. Primary distinguished only by weight.
		Primary: lipgloss.AdaptiveColor{Light: "#111827", Dark: "#F3F4F6"},
		Muted:   lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"},
		OK:      lipgloss.AdaptiveColor{Light: "#111827", Dark: "#F3F4F6"},
		Warn:    lipgloss.AdaptiveColor{Light: "#111827", Dark: "#F3F4F6"},
		Bad:     lipgloss.AdaptiveColor{Light: "#7F1D1D", Dark: "#FCA5A5"},
		Border:  lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"},
	},
}

// NewThemeWith builds a Theme from a ThemePreset. Same shape as
// NewTheme() in styles.go — that one stays as the zero-config
// entry point + delegates to the first preset to keep behaviour
// identical when the operator hasn't picked anything.
func NewThemeWith(p ThemePreset) Theme {
	return Theme{
		Title: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.Primary).
			Padding(0, 1),
		Tab: lipgloss.NewStyle().
			Foreground(p.Muted).
			Padding(0, 2),
		ActiveTab: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.Primary).
			Underline(true).
			Padding(0, 2),
		StatusBar: lipgloss.NewStyle().
			Foreground(p.Muted).
			BorderTop(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(p.Border).
			Padding(0, 1),
		StatusKey: lipgloss.NewStyle().Bold(true).Foreground(p.Primary),
		StatusVal: lipgloss.NewStyle().Foreground(p.Muted),
		StatusMsg: lipgloss.NewStyle().Foreground(p.OK),
		StatusErr: lipgloss.NewStyle().Foreground(p.Bad),
		HelpBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.Border).
			Padding(1, 2),
		ConfirmBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.Bad).
			Padding(1, 2),
		BadgeOK:   lipgloss.NewStyle().Foreground(p.OK).Bold(true),
		BadgeWarn: lipgloss.NewStyle().Foreground(p.Warn).Bold(true),
		BadgeBad:  lipgloss.NewStyle().Foreground(p.Bad).Bold(true),
		Faint:     lipgloss.NewStyle().Foreground(p.Muted),
		// Selected row : primary as background, dark foreground for
		// contrast. Same colour the title bar / active tab use so the
		// whole UI reads as a coherent palette.
		SelectedRow: lipgloss.NewStyle().
			Foreground(lipgloss.Color("0")).
			Background(p.Primary).
			Bold(true),
		SidebarBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.Border).
			Padding(1, 1),
		SidebarItem:       lipgloss.NewStyle().Foreground(p.Muted).PaddingLeft(2),
		SidebarItemActive: lipgloss.NewStyle().Bold(true).Foreground(p.Primary),
		SidebarSection:    lipgloss.NewStyle().Foreground(p.Muted).Italic(true).PaddingTop(1),
		BodyBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.Border).
			Padding(0, 1),
	}
}

// themeIndexByName returns the index of the named theme in
// themePresets, or 0 (green default) if not found.
func themeIndexByName(name string) int {
	for i, p := range themePresets {
		if p.Name == name {
			return i
		}
	}
	return 0
}

// themeConfigPath returns $HOME/.weft/tui-theme — the file that
// holds the operator's chosen theme name across runs. Following
// the v0.3.3 ~/.weft/ convention split from ~/.weft-loom/.
func themeConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".weft", "tui-theme")
}

// loadSavedTheme reads the persisted theme name from disk. Returns
// "green" (the default) when the file is missing or unreadable.
// Errors are swallowed — the TUI must always boot, even when ~/.weft
// is not writable.
func loadSavedTheme() string {
	path := themeConfigPath()
	if path == "" {
		return themePresets[0].Name
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return themePresets[0].Name
	}
	name := string(b)
	// Strip a trailing newline if the operator edited the file.
	for len(name) > 0 && (name[len(name)-1] == '\n' || name[len(name)-1] == ' ') {
		name = name[:len(name)-1]
	}
	if themeIndexByName(name) == 0 && name != themePresets[0].Name {
		// Unknown name → fall back, don't reject. Helps when a
		// future theme is removed from the preset list.
		return themePresets[0].Name
	}
	return name
}

// saveTheme writes the chosen theme name to disk. Best-effort —
// failure is logged via the returned error so the app's status
// bar can show "couldn't persist theme" without aborting the
// rotation.
func saveTheme(name string) error {
	path := themeConfigPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(name+"\n"), 0o644)
}
