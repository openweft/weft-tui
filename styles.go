// Package tui implements `weft tui` — an interactive terminal UI for
// cluster management. This file holds the shared Lip Gloss theme :
// adaptative colours so the same styles read well in both dark and
// light terminals.
package main

import "github.com/charmbracelet/lipgloss"

// Theme is the bundle of styles consumed by every view. Built once
// at process start (NewTheme) ; never mutated.
type Theme struct {
	Title      lipgloss.Style
	Tab        lipgloss.Style
	ActiveTab  lipgloss.Style
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
		Tab: lipgloss.NewStyle().
			Foreground(muted).
			Padding(0, 2),
		ActiveTab: lipgloss.NewStyle().
			Bold(true).
			Foreground(primary).
			Underline(true).
			Padding(0, 2),
		StatusBar: lipgloss.NewStyle().
			Foreground(muted).
			BorderTop(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(border).
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
		SidebarItem:       lipgloss.NewStyle().Foreground(muted).PaddingLeft(2),
		SidebarItemActive: lipgloss.NewStyle().Bold(true).Foreground(primary),
		SidebarSection:    lipgloss.NewStyle().Foreground(muted).Italic(true).PaddingTop(1),
		BodyBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(0, 1),
	}
}
