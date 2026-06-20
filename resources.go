// Package tui — generic resource-list view. Powers the 21 "secondary"
// views the operator reaches via the command palette (`:networks`,
// `:volumes`, …). The 4 primary tabs (Hosts / VMs / Projects /
// Events) keep their dedicated models because they need bespoke
// behaviour (drawers, log viewport, event streaming) ; everything
// else reuses ResourceListModel.
//
// Pattern : one `ResourceConfig` per noun describes how to list +
// what actions are available. ResourceListModel renders the table,
// runs the list Cmd, handles selection + per-action keypresses,
// then defers to user-confirmation modals when needed.
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	weftv1 "github.com/openweft/weft-proto"
)

// ResourceConfig is the per-noun description : how to list, what
// columns to render, what actions the operator can trigger.
type ResourceConfig struct {
	ID      string // url-slug, e.g. "networks"
	Title   string // header label, e.g. "Networks"
	Section string // sidebar group, e.g. "Network"
	Columns []table.Column
	// List runs the noun's ListXxx RPC + projects rows into a flat
	// slice. Returns the slice + a non-nil error to render in the
	// status bar.
	List func(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error)
	// RowToCells maps a list-returned row into the cell slice the
	// bubbles/table widget consumes. Order MUST match Columns.
	RowToCells func(r map[string]any) []string
	// Actions are the keypresses the user can trigger on the
	// selected row. Optional.
	Actions []ResourceAction
}

// ResourceAction is one operator-triggered command (delete, rename,
// drain, …) keyed on a single keystroke.
type ResourceAction struct {
	Key     string // "d", "x", "r" …
	Label   string // displayed in the help footer
	Confirm string // when non-empty, asks the operator to type this string before firing
	Do      func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (msg string, err error)
}

// ResourceListModel is the generic Bubble Tea model that backs every
// resource-listing view registered in the catalogue.
type ResourceListModel struct {
	theme    Theme
	client   weftv1.WeftAgentClient
	cfg      ResourceConfig
	table    table.Model
	rows     []map[string]any
	loading  bool
	err      error
	refresh  time.Time

	// confirmAction is the pending action when its Confirm field is
	// non-empty ; renders a one-line prompt the operator types into.
	confirmAction string
	confirmInput  string
	confirmRow    map[string]any
}

// newResourceListModel builds a fresh model for a registered resource.
func newResourceListModel(theme Theme, client weftv1.WeftAgentClient, cfg ResourceConfig) *ResourceListModel {
	tbl := table.New(
		table.WithColumns(cfg.Columns),
		table.WithFocused(true),
		table.WithHeight(15),
	)
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"}).
		BorderBottom(true).
		Bold(true)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("0")).
		Background(lipgloss.AdaptiveColor{Light: "#A78BFA", Dark: "#A78BFA"}).
		Bold(true)
	tbl.SetStyles(s)
	return &ResourceListModel{theme: theme, client: client, cfg: cfg, table: tbl, loading: true}
}

// Init triggers the initial list.
func (m *ResourceListModel) Init() tea.Cmd { return m.loadCmd() }

func (m *ResourceListModel) loadCmd() tea.Cmd {
	if m.client == nil {
		return func() tea.Msg { return resourceLoadedMsg{cfg: m.cfg.ID, err: errNoClient} }
	}
	cfg := m.cfg
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := cfg.List(ctx, client)
		return resourceLoadedMsg{cfg: cfg.ID, rows: rows, err: err}
	}
}

type resourceLoadedMsg struct {
	cfg  string
	rows []map[string]any
	err  error
}

type resourceActionMsg struct {
	cfg    string
	action string
	row    map[string]any
	msg    string
	err    error
}

// applyRows refreshes the in-memory rows + the table.
func (m *ResourceListModel) applyRows(rows []map[string]any) {
	m.rows = rows
	tableRows := make([]table.Row, 0, len(rows))
	for _, r := range rows {
		tableRows = append(tableRows, m.cfg.RowToCells(r))
	}
	m.table.SetRows(tableRows)
	m.loading = false
	m.err = nil
	m.refresh = time.Now()
}

// selected returns the row under the cursor, or nil when the table
// is empty.
func (m *ResourceListModel) selected() map[string]any {
	if len(m.rows) == 0 {
		return nil
	}
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rows) {
		return nil
	}
	return m.rows[idx]
}

// Update handles keypresses + resource-loaded messages.
func (m *ResourceListModel) Update(msg tea.Msg) (*ResourceListModel, tea.Cmd) {
	switch msg := msg.(type) {
	case resourceLoadedMsg:
		if msg.cfg != m.cfg.ID {
			return m, nil // stale, ignore
		}
		if msg.err != nil {
			m.err = msg.err
			m.loading = false
		} else {
			m.applyRows(msg.rows)
		}
		return m, nil

	case tea.KeyMsg:
		// In confirmation mode : type the confirmation string then
		// Enter fires the action. Esc cancels.
		if m.confirmAction != "" {
			switch msg.String() {
			case "esc":
				m.confirmAction = ""
				m.confirmInput = ""
				m.confirmRow = nil
				return m, nil
			case "enter":
				if m.confirmInput != m.confirmActionExpected() {
					// Wrong confirmation string ; reset.
					m.confirmInput = ""
					return m, nil
				}
				cfg := m.cfg
				client := m.client
				row := m.confirmRow
				action := m.confirmAction
				m.confirmAction = ""
				m.confirmInput = ""
				m.confirmRow = nil
				return m, func() tea.Msg {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					var doFn func(context.Context, weftv1.WeftAgentClient, map[string]any) (string, error)
					for _, a := range cfg.Actions {
						if a.Key == action {
							doFn = a.Do
							break
						}
					}
					if doFn == nil {
						return resourceActionMsg{cfg: cfg.ID, action: action, row: row, err: fmt.Errorf("unknown action %q", action)}
					}
					m2, err := doFn(ctx, client, row)
					return resourceActionMsg{cfg: cfg.ID, action: action, row: row, msg: m2, err: err}
				}
			case "backspace":
				if len(m.confirmInput) > 0 {
					m.confirmInput = m.confirmInput[:len(m.confirmInput)-1]
				}
				return m, nil
			default:
				if len(msg.String()) == 1 {
					m.confirmInput += msg.String()
				}
				return m, nil
			}
		}

		// Normal mode : action keys + r for refresh + table nav.
		key := msg.String()
		if key == "r" {
			return m, m.loadCmd()
		}
		for _, a := range m.cfg.Actions {
			if a.Key == key {
				row := m.selected()
				if row == nil {
					return m, nil
				}
				if a.Confirm != "" {
					m.confirmAction = a.Key
					m.confirmInput = ""
					m.confirmRow = row
					return m, nil
				}
				cfg := m.cfg
				client := m.client
				action := a
				return m, func() tea.Msg {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					mm, err := action.Do(ctx, client, row)
					return resourceActionMsg{cfg: cfg.ID, action: action.Key, row: row, msg: mm, err: err}
				}
			}
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// confirmActionExpected returns the string the operator must type to
// confirm the pending action. Pulled from the matching action's
// Confirm field.
func (m *ResourceListModel) confirmActionExpected() string {
	for _, a := range m.cfg.Actions {
		if a.Key == m.confirmAction {
			return a.Confirm
		}
	}
	return ""
}

// View renders the header + table + confirmation prompt (when active).
func (m *ResourceListModel) View(width int) string {
	var b strings.Builder
	b.WriteString(m.theme.Title.Render(m.cfg.Title))
	b.WriteString("\n")
	if m.loading {
		b.WriteString(m.theme.Faint.Render("loading…"))
		return b.String()
	}
	if m.err != nil {
		b.WriteString(m.theme.BadgeBad.Render("error: " + m.err.Error()))
		return b.String()
	}
	b.WriteString(m.table.View())
	if m.confirmAction != "" {
		b.WriteString("\n\n")
		expected := m.confirmActionExpected()
		prompt := fmt.Sprintf("type %q to confirm, Esc to cancel : ", expected)
		b.WriteString(m.theme.BadgeWarn.Render(prompt))
		b.WriteString(m.theme.Title.Render(m.confirmInput + "_"))
	}
	return b.String()
}

// Actions returns the help footer entries for this resource.
func (m *ResourceListModel) Actions() []ResourceAction { return m.cfg.Actions }
