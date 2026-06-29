// Package tui implements `weft tui` — an interactive terminal UI for
// cluster management. This file holds the shared Lip Gloss theme :
// adaptative colours so the same styles read well in both dark and
// light terminals.
package main

import "github.com/charmbracelet/lipgloss"

// Theme is the bundle of styles consumed by every view. Built once
// at process start (NewTheme) ; never mutated.
type Theme struct {
	Title lipgloss.Style
	// Tab + ActiveTab dead since the top-of-screen tabs header
	// was replaced by the sidebar (catalogue navigation now in
	// the left rail). Kept removed per audit 2026-06-25.
	StatusBar  lipgloss.Style
	StatusKey  lipgloss.Style
	StatusVal  lipgloss.Style
	StatusMsg  lipgloss.Style
	StatusErr  lipgloss.Style
	HelpBox    lipgloss.Style
	ConfirmBox lipgloss.Style
	BadgeOK    lipgloss.Style
	BadgeWarn  lipgloss.Style
	BadgeBad   lipgloss.Style
	Faint      lipgloss.Style
	// SelectedRow styles the highlighted row in every bubbles/table
	// widget. Picks up the primary hue + black foreground for
	// contrast — keeps the selected row visually consistent with the
	// active theme instead of the hard-coded violet that used to
	// ship across hosts/vms/projects/resources tables.
	SelectedRow lipgloss.Style
	// SidebarBox is the rounded-border container that wraps the
	// vertical object-type list on the left of the main view.
	SidebarBox lipgloss.Style
	// SidebarItem renders one inactive entry in the sidebar
	// (muted foreground). SidebarItemActive renders the selected
	// entry (primary hue + bold + leading bullet). SidebarSection
	// styles the section header ("core" / "resources").
	SidebarItem       lipgloss.Style
	SidebarItemActive lipgloss.Style
	SidebarSection    lipgloss.Style
	// BodyBox wraps the main body region in the same rounded
	// border as SidebarBox so the two side-by-side panels read
	// as a matched pair.
	BodyBox lipgloss.Style
	// LogPaneBox wraps the scrollable diagnostic pane below the body.
	// Same rounded-border family as SidebarBox / BodyBox so the three
	// panels read as a cohesive layout.
	LogPaneBox lipgloss.Style
	// TopbarBox wraps the topbar (product name + cluster left,
	// identity right). Same rounded-border family as the other
	// panels so the chrome stays cohesive.
	TopbarBox lipgloss.Style
	// LogTabActive / LogTabInactive style the individual log-pane
	// tabs (Logs / Terminal / Bookmarks). Each tab gets its own
	// rounded frame so the strip reads as a row of buttons rather
	// than a single text line.
	LogTabActive   lipgloss.Style
	LogTabInactive lipgloss.Style
}

// NewTheme builds the default theme. Adaptive colours are passed as
// `lipgloss.AdaptiveColor{Light, Dark}` so the same style produces a
// readable hue regardless of terminal background.
func NewTheme() Theme {
	// Primary = vivid green. Distinct from `ok` (softer green, used
	// for success badges) so titles + active tabs read as "weft
	// chrome" rather than "operation succeeded". 2026-06-21 :
	// switched away from violet per operator feedback ("ça serait
	// mieux en vert plutôt que violet").
	primary := lipgloss.AdaptiveColor{Light: "#16A34A", Dark: "#4ADE80"}
	muted := lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
	ok := lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#86EFAC"}
	warn := lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FCD34D"}
	bad := lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#FCA5A5"}
	border := lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"}

	return Theme{
		Title: lipgloss.NewStyle().
			Bold(true).
			Foreground(primary).
			Padding(0, 1),
		StatusBar: lipgloss.NewStyle().
			Foreground(muted).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			BorderTop(false).
			Padding(0, 1),
		StatusKey: lipgloss.NewStyle().Bold(true).Foreground(primary),
		StatusVal: lipgloss.NewStyle().Foreground(muted),
		StatusMsg: lipgloss.NewStyle().Foreground(ok),
		StatusErr: lipgloss.NewStyle().Foreground(bad),
		HelpBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(1, 2),
		ConfirmBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(bad).
			Padding(1, 2),
		BadgeOK:   lipgloss.NewStyle().Foreground(ok).Bold(true),
		BadgeWarn: lipgloss.NewStyle().Foreground(warn).Bold(true),
		BadgeBad:  lipgloss.NewStyle().Foreground(bad).Bold(true),
		Faint:     lipgloss.NewStyle().Foreground(muted),
		SidebarBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(1, 1),
		// 2026-06-23 : sidebar items + section headers aligned on the
		// topbar identity style (theme.Faint = muted, no italic, no
		// padding). Active row keeps its distinct bold+primary so
		// selection stays unmistakable.
		SidebarItem:       lipgloss.NewStyle().Foreground(muted),
		SidebarItemActive: lipgloss.NewStyle().Bold(true).Foreground(primary),
		SidebarSection:    lipgloss.NewStyle().Foreground(muted),
		BodyBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(0, 1),
		LogPaneBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(0, 1),
		TopbarBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(0, 1),
		// Tab boxes : NO bottom border. The bottom is drawn by a
		// custom rule line in renderTabStrip that connects the tab
		// edges to the pane's content below — like browser tabs.
		// Without this, lipgloss would draw a closing border under
		// each tab + a separate rule below = a visible gap.
		LogTabActive: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder(), true, true, false, true).
			BorderForeground(primary).
			Foreground(primary).
			Bold(true).
			Padding(0, 1),
		LogTabInactive: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder(), true, true, false, true).
			BorderForeground(border).
			Foreground(muted).
			Padding(0, 1),
	}
}
