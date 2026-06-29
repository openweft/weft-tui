package main

// logpane.go is a small scrollable strip rendered below the main
// body (sidebar+body row) and above the status bar. Its sole job is
// to surface diagnostic events the operator otherwise loses :
// resilient-client lifecycle (dial fail / failover), RPC errors that
// happen between refreshes, anything we used to fmt.Fprintf to
// os.Stderr (which bled through the alt-screen).
//
// Why a separate pane (instead of scrolling the body) ?
//
//   - The body content (Hosts / VMs / catalogue tables) sits in
//     bubbles/table. The table widget owns its own viewport AND its
//     own scrolling semantics ; mixing free-text logs into it is
//     out-of-band.
//   - A dedicated pane never moves the table cursor or steals
//     keystrokes from row selection.
//   - The viewport.Model handles wheel + PgUp/PgDn natively when the
//     mouse is inside its rect, so it scrolls independently from
//     the body table — exactly what the user asked for.
//
// Design : a fixed-height (5 lines) bordered box. Auto-scrolls to
// bottom on new appends ; PgUp / wheel-up pause auto-scroll until
// the operator returns to the bottom (operator-following pattern).
// In-memory ring buffer caps the entry count so a noisy failover
// loop doesn't OOM the TUI.

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

// logPaneCapacity is the number of log lines retained in memory.
// Each line is short (≤120 cols) so 200 lines ≈ 24 KiB peak — fine.
const logPaneCapacity = 200

// logPaneDefaultHeight is the visible row count for the pane. Picked
// so the table region still has the bulk of the screen. The operator
// can dump more with PgDn OR drag the horizontal handle between
// body + log pane to grow / shrink it.
const (
	logPaneDefaultHeight = 3 // slim drawer by default ; operator drags to grow
	logPaneMinHeight     = 0 // 0 viewport rows ; drawer collapses to just the tab strip + borders
	// High ceiling — operator can drag the divider up almost to the
	// body's content area ; bodyHeight()'s floor (3) is what actually
	// stops the rise, not this constant.
	logPaneMaxHeight = 200
)

// SetHeight sets the viewport's visible row count + refreshes the
// rendered content so the operator sees the new size immediately.
// Used by the drag-handle in app.go to grow / shrink the log pane.
func (p *logPane) SetHeight(h int) {
	if h < logPaneMinHeight {
		h = logPaneMinHeight
	}
	if h > logPaneMaxHeight {
		h = logPaneMaxHeight
	}
	p.vp.Height = h
	p.refresh()
}

// logEntry is one row in the pane. level matches the resilient
// client's ResilientEvent* constants ; the renderer maps it to a
// theme style.
type logEntry struct {
	ts    time.Time
	level string
	msg   string
}

// logPaneTab labels one tab of the log pane's strip. Today only
// "Logs" is functional ; the others are placeholders the operator
// can see in the strip so they know future panes are coming.
type logPaneTab struct {
	ID    string // stable identifier ; used to switch the active tab
	Label string // human-facing label rendered in the strip
}

// logPaneTabs is the ordered list rendered as the strip above the
// pane content. Logs is the only one wired up ; Terminal + Bookmarks
// are reserved placeholders.
var logPaneTabs = []logPaneTab{
	{ID: "logs", Label: "Logs"},
	{ID: "events", Label: "Events"},
	{ID: "terminal", Label: "Terminal"},
	{ID: "bookmarks", Label: "Bookmarks"},
}

// logPane owns the viewport + ring buffer. Held by Model so the
// Update loop can append + the View loop can render in one read.
type logPane struct {
	vp      viewport.Model
	entries []logEntry
	// follow=true → viewport auto-scrolls on every append. Cleared
	// when the operator scrolls up ; restored on a "G"/"end" gesture
	// or once they scroll back to the bottom.
	follow bool
	// activeTab is the currently selected tab in the strip (logs /
	// terminal / bookmarks). For V0.1 only "logs" is functional.
	activeTab string
}

// newLogPane builds the pane with sensible defaults. width gets
// re-applied on every WindowSizeMsg (see resizeLogPane below).
func newLogPane(width int) logPane {
	if width < 10 {
		width = 10
	}
	vp := viewport.New(width, logPaneDefaultHeight)
	vp.MouseWheelEnabled = true
	return logPane{
		vp:        vp,
		entries:   make([]logEntry, 0, logPaneCapacity),
		follow:    true,
		activeTab: "logs",
	}
}

// append adds a new entry + re-renders the viewport content. The
// ring buffer drops the oldest entry when the cap is hit.
func (p *logPane) append(level, msg string) {
	if len(p.entries) >= logPaneCapacity {
		p.entries = append(p.entries[1:], logEntry{ts: now(), level: level, msg: msg})
	} else {
		p.entries = append(p.entries, logEntry{ts: now(), level: level, msg: msg})
	}
	p.refresh()
}

// refresh re-renders the buffered entries into the viewport content.
// logLevelStyle maps a log level token to the colour the pane
// renders its tag in. Matches the operator's directive 2026-06-29 :
// red for error, green for info, orange for debug ; yellow for warn
// because that's the universal in-between cue. Unknown levels fall
// through to the default colour so a typo surfaces visually as
// "uncoloured" rather than crashing the renderer.
func logLevelStyle(level string) lipgloss.Style {
	switch level {
	case ResilientEventError, "ERROR":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#EF4444")).Bold(true)
	case ResilientEventWarn, "WARN":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#F59E0B"))
	case ResilientEventInfo, "INFO":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#22C55E"))
	case "DEBUG", "debug":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#FB923C"))
	default:
		return lipgloss.NewStyle()
	}
}

// Called after append and after a theme/resize change.
func (p *logPane) refresh() {
	var b strings.Builder
	for i, e := range p.entries {
		if i > 0 {
			b.WriteString("\n")
		}
		// Pad the level to 5 columns BEFORE colour-styling so the
		// ANSI escapes don't shift the msg column. Operator
		// directive 2026-06-29 : "met de la couleur sur le
		// type/tag de message. erreur en rouge, info en vert,
		// debug en orange".
		label := fmt.Sprintf("%-5s", e.level)
		fmt.Fprintf(&b, "%s  %s  %s", e.ts.Format("15:04:05"), logLevelStyle(e.level).Render(label), e.msg)
	}
	p.vp.SetContent(b.String())
	if p.follow {
		p.vp.GotoBottom()
	}
}

// resize tracks the parent layout's width changes.
func (p *logPane) resize(width int) {
	if width < 10 {
		width = 10
	}
	p.vp.Width = width
	p.refresh()
}

// View renders the pane wrapped in a bordered box so it's visually
// separate from the table region above + the status bar below. The
// FIRST row of the rendered content is the tab strip (Logs |
// Terminal | Bookmarks) so the operator can see what's reachable
// from this pane.
func (p logPane) View(theme Theme, width int, eventsBody string) string {
	if width < 10 {
		width = 10
	}
	// Tab strip at the TOP of the log pane (operator clarification
	// 2026-06-23 : only the FRAME stays anchored at the bottom of
	// the window — the tabs themselves don't ; they sit at the top
	// of the pane content). The LogPaneBox bottom border is
	// inherently anchored to the screen bottom (it's the last
	// thing rendered before the status bar) so the operator drags
	// the TOP border up to grow the pane.
	// Strip spans the FULL inside width of the pane (= width - 1
	// for the right border ; left border was dropped). Edge-to-
	// edge so the rule line's ends meet the LogPaneBox right
	// border, no 2-col gap. Operator directive 2026-06-24 "joint
	// le frame de la tab bar a droite et a gauche".
	strip := p.renderTabStrip(theme, width-1)
	var body string
	switch p.activeTab {
	case "events":
		// Events tab : the platform-event stream rendered by the
		// model.events viewport. Caller passes the rendered body
		// since the events sub-model is owned at the parent level
		// and we don't want to thread a pointer to it through.
		// Operator directive 2026-06-24 "passer la vue events
		// dans la zone tabbar".
		body = eventsBody
		if body == "" {
			body = theme.Faint.Render("  Events — connecting…")
		}
	case "terminal":
		body = padLogPaneBody(theme.Faint.Render("  Terminal — not wired yet (V0.2 : SSH multiplex)."), p.vp.Height)
	case "bookmarks":
		body = padLogPaneBody(theme.Faint.Render("  Bookmarks — not wired yet (V0.2 : pinned SSH targets)."), p.vp.Height)
	default:
		body = p.vp.View()
	}
	content := strip + "\n" + body
	// Drop the LEFT border : it would stack against the sidebar's
	// right border = a 2-col-thick divider. Collapsing into a single
	// shared column (the sidebar's `│`) reads as one frame
	// subdivision instead of two abutting boxes. The post-process
	// pass in app.View() injects a T-junction (`├` / `┤` / etc.)
	// where the log pane's top border crosses sidebar's right
	// border so the line reads continuously.
	// LogPaneBox keeps its top border : that line IS the divider
	// (drag target). The strip sits inside the box just below it.
	// Earlier 2026-06-24 attempt BorderTop(false) killed the drag
	// handle ; reverted. The strip's rule line still moves with
	// the drag — that's the open issue : actually anchoring the
	// strip bottom to the divider requires the strip to live
	// OUTSIDE the box, which is a bigger refactor (touches
	// bodyHeight + drag handler accounting).
	return theme.LogPaneBox.
		BorderLeft(false).
		PaddingLeft(0).
		PaddingRight(0).
		Width(width - 1).
		MaxHeight(p.height()).
		Render(content)
}

// padLogPaneBody right-pads a placeholder body (Terminal /
// Bookmarks single-line stubs) with empty lines so the rendered
// pane has the same total height as the Logs / Events tabs.
// Without this, switching to a placeholder tab shrinks the pane
// by N-1 rows and the LogPaneBox bottom border moves up — the
// operator-reported "le frame bas n'est pas correctement placcé".
func padLogPaneBody(body string, vpHeight int) string {
	if vpHeight <= 1 {
		return body
	}
	out := body
	for i := 1; i < vpHeight; i++ {
		out += "\n"
	}
	return out
}

// renderTabStrip draws the row of bordered tab boxes (Logs /
// Terminal / Bookmarks) plus a connecting rule line beneath. The
// tab styles have NO bottom border (see styles.go) ; this function
// manufactures the bottom edge by stitching `╰` / `╯` at each
// tab's corners and `─` everywhere else — so the rule line reads
// as the SHARED bottom of the tabs + the pane delimiter, like
// browser tabs anchored to their content.
//
// The strip occupies 3 lines :
//   1. Tab top borders (`╭──╮`)
//   2. Tab content (`│ Logs │`)
//   3. Stitched bottom rule (`╰──╯╰─...╯─────`)
//
// logPane.height() accounts for these 3 lines.
func (p logPane) renderTabStrip(theme Theme, width int) string {
	// Each tab has its OWN rounded top corners (`╭ ╮`), with a 1-col
	// gap between adjacent tabs so the rounded corners are visible.
	// Bottom corners are SQUARE (`└ ┘`) so the transition into the
	// continuous rule line below reads as a clean angle. The rule
	// line passes `─` through the gap between tabs so it stays
	// continuous edge-to-edge.
	//
	//   top   :  ╭──────╮ ╭──────────╮ ╭───────────╮
	//   label :  │ Logs │ │ Terminal │ │ Bookmarks │
	//   rule  :──└──────┘─└──────────┘─└───────────┘─────────
	const leadIndent = 1
	if len(logPaneTabs) == 0 {
		return ""
	}

	innerW := make([]int, len(logPaneTabs))
	for i, t := range logPaneTabs {
		innerW[i] = len(t.Label) + 2 // 1 padding + label + 1 padding
	}

	// Welded layout : at each shared boundary, only the RIGHT tab
	// carries the rounded `╭`. The LEFT tab has no `╮` of its own
	// — its top runs directly into the next tab's `╭`. Bottom uses
	// `┴` at every shared boundary so the rule line stays continuous
	// through the join.
	//
	//   top   : ╭──────╭──────────╭───────────╮
	//   label : │ Logs │ Terminal │ Bookmarks │
	//   rule  :─└──────┴──────────┴───────────┘──────────
	// Browser-tab semantics : the ACTIVE tab's bottom edge stays
	// open (rule line shows spaces under it) so it visually merges
	// with the body content below ; inactive tabs have a closed
	// bottom (rule line `───`) that reads as the tab's own frame.
	// Without this, dragging the horizontal divider during an
	// inactive tab made the whole rule line shift with the pane
	// while Logs (default active) looked anchored — operator-
	// reported 2026-06-24.
	activeIdx := -1
	for i, t := range logPaneTabs {
		if t.ID == p.activeTab {
			activeIdx = i
			break
		}
	}
	var top, rule strings.Builder
	var styledLabel strings.Builder
	for c := 0; c < leadIndent; c++ {
		top.WriteRune(' ')
		styledLabel.WriteRune(' ')
		rule.WriteRune('─')
	}
	for i, t := range logPaneTabs {
		isActive := i == activeIdx
		prevActive := i > 0 && i-1 == activeIdx
		// Left corner of THIS tab.
		top.WriteRune('╭')
		styledLabel.WriteString(theme.Faint.Render("│"))
		switch {
		case isActive && i == 0:
			rule.WriteRune('┘') // rule terminates here from the left
		case isActive:
			rule.WriteRune('┘') // rule comes from previous inactive, ends
		case prevActive:
			rule.WriteRune('└') // rule resumes after the active tab
		case i == 0:
			rule.WriteRune('└')
		default:
			rule.WriteRune('┴')
		}
		// Inner area : top always `─` ; bottom either `─` (inactive)
		// or space (active, "open" bottom — body shows through).
		for k := 0; k < innerW[i]; k++ {
			top.WriteRune('─')
			if isActive {
				rule.WriteRune(' ')
			} else {
				rule.WriteRune('─')
			}
		}
		// Label content.
		raw := " " + t.Label + " "
		for len(raw) < innerW[i] {
			raw += " "
		}
		if isActive {
			styledLabel.WriteString(theme.LogTabActive.UnsetBorderStyle().UnsetPadding().Render(raw))
		} else {
			styledLabel.WriteString(theme.LogTabInactive.UnsetBorderStyle().UnsetPadding().Render(raw))
		}
	}
	// Final right edge of the LAST tab.
	top.WriteRune('╮')
	styledLabel.WriteString(theme.Faint.Render("│"))
	// Rule's final corner depends on whether the last tab is active.
	lastActive := activeIdx == len(logPaneTabs)-1
	if lastActive {
		rule.WriteRune('└') // rule resumes to the right of the active last tab
	} else {
		rule.WriteRune('┘') // close on the inactive last tab
	}
	// Trailing `─` on the rule line so it reaches the pane edge.
	currentLen := lipgloss.Width(rule.String())
	for currentLen < width {
		rule.WriteRune('─')
		currentLen++
	}

	return theme.Faint.Render(top.String()) + "\n" +
		styledLabel.String() + "\n" +
		theme.Faint.Render(rule.String())
}

// tabHitX returns the tab ID whose rendered box contains the given
// X coordinate (relative to the log pane's left edge). Returns ""
// when X is outside any tab box.
//
// Layout : `leadIndent` spaces of blank, then each tab's box of
// width = visible-width(label) + 2 (rounded border) + 2 (padding) =
// len(label) + 4. Adjacent tabs are joined with no gap, so the
// cumulative X advances by the previous tab's width.
func (p logPane) tabHitX(x int) string {
	// Welded layout : each tab spans `╭` + innerW cols (no `╮` of
	// its own — the next tab's `╭` claims the next col). Only the
	// LAST tab has a trailing `╮`.
	const leadIndent = 1
	col := leadIndent
	for i, t := range logPaneTabs {
		innerW := len(t.Label) + 2
		w := 1 + innerW // `╭` + inner
		if i == len(logPaneTabs)-1 {
			w++ // trailing `╮` belongs to the last tab
		}
		if x >= col && x < col+w {
			return t.ID
		}
		col += 1 + innerW // advance past `╭` + inner ; next tab's `╭` follows immediately
	}
	return ""
}

// switchTab updates the active tab. Used by the click handler.
// Idempotent.
func (p *logPane) switchTab(id string) {
	for _, t := range logPaneTabs {
		if t.ID == id {
			p.activeTab = id
			return
		}
	}
}

// height is the row count the pane occupies in the parent layout :
//   1 (LogPaneBox top border)
//   + 2 (tab strip : top border + label row of the tab boxes ;
//        the bottom border is rendered by the stitched rule below)
//   + 1 (stitched rule : tabs' bottom edges + pane separator)
//   + vp.Height
//   + 1 (LogPaneBox bottom border)
// Centralised so bodyHeight() can subtract it cleanly.
func (p logPane) height() int {
	// 1 (top border = divider) + 3 (strip top+label+rule) + vp +
	// 1 (bottom border) = vp + 5.
	return p.vp.Height + 5
}

// now is a swappable clock so tests get deterministic timestamps.
var now = time.Now
