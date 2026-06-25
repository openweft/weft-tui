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
	"sort"
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
	// CreateFields + CreateFn opt the resource into the `n` create
	// flow. CreateFields drives the form's textinputs ; CreateFn
	// turns the collected values into a CreateXxx RPC call. Leave
	// both nil to disable creation (operator falls back to the CLI
	// or webui for that resource).
	CreateFields []FormField
	CreateFn     CreateFn
	// EditFields + EditFn opt the resource into the `e` edit flow.
	// Same shape as CreateFields/CreateFn except the form is pre-
	// filled from the highlighted row and EditFn receives the row
	// alongside the user-edited values. Field.Key in EditFields
	// matches the row's map key for the pre-fill ; if a row doesn't
	// carry that key the input starts empty. Leave both nil to keep
	// the resource Create+Delete-only (still drops into the CLI for
	// edits — useful for resources whose Update RPCs we haven't
	// wired yet).
	EditFields []FormField
	EditFn     EditFn
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
	theme   Theme
	client  weftv1.WeftAgentClient
	cfg     ResourceConfig
	table   table.Model
	rows    []map[string]any
	loading bool
	err     error
	refresh time.Time

	// sortCol is the index of the column the table is currently
	// sorted on ; -1 = no explicit sort (preserve server order).
	// sortAsc reverses to descending when false. Keys: `<` cycles
	// the active column (prev), `>` cycles next, `Shift+S` toggles
	// direction. 2026-06-24 operator directive : sort par clic
	// sur la flèche du header.
	sortCol int
	sortAsc bool

	// confirmAction is the pending action when its Confirm field is
	// non-empty ; renders a one-line prompt the operator types into.
	confirmAction string
	confirmInput  string
	confirmRow    map[string]any

	// detailOpen + detailRow drive the inspector drawer that pops up
	// on `Enter`. The drawer renders every key/value of the selected
	// row in a sorted table — same level of detail the bespoke
	// Hosts + VMs drawers expose, but generic across the 21 palette
	// resources.
	detailOpen bool
	detailRow  map[string]any

	// create is the form model when the operator pressed `n` to
	// start a new entry. nil = no create flow in progress.
	create *createFormModel

	// relatedHosts resolves a detail row to the host records
	// logically attached to it. Used by AZ + Rack detail drawers
	// to surface their child Hosts under the row's properties.
	relatedHosts func(resourceID string, row map[string]any) []relatedHost

	// relatedProjects resolves a detail row to the projects
	// logically attached to it (used by Tenant detail drawer).
	relatedProjects func(resourceID string, row map[string]any) []relatedProject
}

// relatedHost is the small view of a Host the AZ + Rack detail
// drawers display under their properties. The fields match the
// `weft host ls` columns most operators look at when scoping a VM
// placement.
type relatedHost struct {
	Hostname, AZ, Rack, Hypervisor, State string
}

// relatedProject is the small view of a Project the Tenant detail
// drawer displays under tenant properties. Mirrors the
// `weft project ls` columns.
type relatedProject struct {
	Name, UUID string
}

// newResourceListModel builds a fresh model for a registered resource.
func newResourceListModel(theme Theme, client weftv1.WeftAgentClient, cfg ResourceConfig) *ResourceListModel {
	tbl := table.New(
		table.WithColumns(cfg.Columns),
		table.WithFocused(true),
		table.WithHeight(15),
	)
	s := table.DefaultStyles()
	// Padding(0, 0) : see hosts.go newHostsModel for the rationale.
	s.Header = s.Header.
		Padding(0, 0).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"}).
		BorderBottom(true).
		Bold(true)
	s.Cell = s.Cell.Padding(0, 0)
	s.Selected = theme.SelectedRow
	tbl.SetStyles(s)
	return &ResourceListModel{
		theme: theme, client: client, cfg: cfg, table: tbl, loading: true,
		// Default sort = first column ascending. Operator
		// directive 2026-06-24 : "il y'a forcement un ordre de
		// tri par defaut, la fleche doit le refleter".
		sortCol: 0, sortAsc: true,
	}
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

// applyRows refreshes the in-memory rows + the table. Applies the
// active sort key (m.sortCol / m.sortAsc) before pushing rows to
// the table, and decorates the column headers with `↑`/`↓` arrows
// so the operator sees which column drives the order.
func (m *ResourceListModel) applyRows(rows []map[string]any) {
	m.rows = rows
	m.applyRowsToTable()
	m.loading = false
	m.err = nil
	m.refresh = time.Now()
}

// applyRowsToTable performs the sort + table.SetRows. Pulled out
// so the sort handlers can re-trigger without re-fetching the data.
func (m *ResourceListModel) applyRowsToTable() {
	sorted := m.sortedRows()
	tableRows := make([]table.Row, 0, len(sorted))
	for _, r := range sorted {
		tableRows = append(tableRows, m.cfg.RowToCells(r))
	}
	m.table.SetRows(tableRows)
	m.table.SetColumns(m.columnsWithSortArrow())
}

// sortedRows returns m.rows sorted by the active column, ascending
// or descending. When sortCol is out of range or sentinel -1, the
// rows are returned unchanged (server order preserved).
func (m *ResourceListModel) sortedRows() []map[string]any {
	if m.sortCol < 0 || m.sortCol >= len(m.cfg.Columns) {
		return m.rows
	}
	out := make([]map[string]any, len(m.rows))
	copy(out, m.rows)
	sort.SliceStable(out, func(i, j int) bool {
		a := m.cfg.RowToCells(out[i])[m.sortCol]
		b := m.cfg.RowToCells(out[j])[m.sortCol]
		if m.sortAsc {
			return a < b
		}
		return a > b
	})
	return out
}

// columnsWithSortArrow returns a copy of the table's CURRENT
// columns (already rescaled by applyResize to span bodyInnerWidth)
// with ONLY the active sort column's title suffixed by ` ↑` /
// ` ↓`. Inactive columns stay clean — operator directive
// 2026-06-24. Reading from m.table.Columns() preserves the
// proportional widths the responsive rescale assigned ; using
// cfg.Columns (original widths) would shrink the table back to
// its compact form and truncate UUID columns unnecessarily.
func (m *ResourceListModel) columnsWithSortArrow() []table.Column {
	cur := m.table.Columns()
	if len(cur) == 0 {
		cur = m.cfg.Columns
	}
	out := make([]table.Column, len(cur))
	for i, c := range cur {
		out[i] = c
		// Restore the ORIGINAL title (strip any prior arrow) so
		// re-applying doesn't accumulate ` ↑` suffixes across
		// re-renders.
		if i < len(m.cfg.Columns) {
			out[i].Title = m.cfg.Columns[i].Title
		}
		if i == m.sortCol {
			arrow := " ↑"
			if !m.sortAsc {
				arrow = " ↓"
			}
			out[i].Title = out[i].Title + arrow
		}
	}
	return out
}

// cycleSort moves the active sort column by delta (+1 / -1) ;
// wraps. Triggered by `>` / `<` keys.
func (m *ResourceListModel) cycleSort(delta int) {
	if len(m.cfg.Columns) == 0 {
		return
	}
	if m.sortCol < 0 {
		m.sortCol = 0
		m.sortAsc = true
	} else {
		m.sortCol = (m.sortCol + delta + len(m.cfg.Columns)) % len(m.cfg.Columns)
	}
	m.applyRowsToTable()
}

// toggleSortDir flips ascending ↔ descending on the active column.
func (m *ResourceListModel) toggleSortDir() {
	if m.sortCol < 0 {
		m.sortCol = 0
	}
	m.sortAsc = !m.sortAsc
	m.applyRowsToTable()
}

// columnAtX maps an X offset (relative to the body's left edge, =
// click.X - sidebarWidth) to a column index. Returns -1 when X
// falls past the last column. Reads from m.table.Columns() (the
// LIVE rescaled widths) so clicks past the original cfg widths
// still hit the right column when the table got widened by
// applyResize — operator-reported 2026-06-24 "ton resize n'aurait
// pas decalé quelque chose ?".
func (m *ResourceListModel) columnAtX(x int) int {
	if x < 0 {
		return -1
	}
	cur := m.table.Columns()
	if len(cur) == 0 {
		cur = m.cfg.Columns
	}
	cumX := 0
	for i, c := range cur {
		if x >= cumX && x < cumX+c.Width {
			return i
		}
		cumX += c.Width
	}
	return -1
}

// selected returns the row under the cursor, or nil when the table
// is empty. Honours the active sort so the cursor index maps back
// to the right row in the SORTED order.
func (m *ResourceListModel) selected() map[string]any {
	if len(m.rows) == 0 {
		return nil
	}
	idx := m.table.Cursor()
	sorted := m.sortedRows()
	if idx < 0 || idx >= len(sorted) {
		return nil
	}
	return sorted[idx]
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

	case createSubmitMsg:
		if msg.cfg != m.cfg.ID || m.create == nil {
			return m, nil
		}
		cfg := m.cfg
		client := m.client
		values := msg.values
		// Close the form ; the action message will re-trigger a list
		// refresh on success.
		m.create = nil
		return m, func() tea.Msg {
			if cfg.CreateFn == nil {
				return resourceActionMsg{cfg: cfg.ID, action: "new", err: fmt.Errorf("create not wired")}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			out, err := cfg.CreateFn(ctx, client, values)
			return resourceActionMsg{cfg: cfg.ID, action: "new", msg: out, err: err}
		}

	case createCancelMsg:
		if msg.cfg == m.cfg.ID {
			m.create = nil
		}
		return m, nil

	case editSubmitMsg:
		if msg.cfg != m.cfg.ID || m.create == nil {
			return m, nil
		}
		cfg := m.cfg
		client := m.client
		row := msg.row
		values := msg.values
		// Same lifecycle as createSubmitMsg : close the form, fire
		// the RPC in the returned Cmd, surface success / error via
		// resourceActionMsg so the status bar + list-refresh path
		// stays unified.
		m.create = nil
		return m, func() tea.Msg {
			if cfg.EditFn == nil {
				return resourceActionMsg{cfg: cfg.ID, action: "edit", err: fmt.Errorf("edit not wired")}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			out, err := cfg.EditFn(ctx, client, row, values)
			return resourceActionMsg{cfg: cfg.ID, action: "edit", row: row, msg: out, err: err}
		}

	case tea.KeyMsg:
		// Create form open : route every key to the form's
		// textinput / submit logic. The form itself emits the
		// submit/cancel messages handled above.
		if m.create != nil {
			f, cmd := m.create.Update(msg)
			m.create = f
			return m, cmd
		}

		// Detail drawer open : Esc/q closes, everything else is a
		// no-op so the drawer doesn't accidentally consume action
		// keys meant for the underlying table.
		if m.detailOpen {
			switch msg.String() {
			case "esc", "q":
				m.detailOpen = false
				m.detailRow = nil
			}
			return m, nil
		}

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

		// Normal mode : action keys + r for refresh + Enter for the
		// detail drawer + `n` for new + table nav.
		key := msg.String()
		if key == "r" {
			return m, m.loadCmd()
		}
		// Sort cycling : `>` next column, `<` prev column, `S`
		// flips ascending/descending. Operator directive
		// 2026-06-24.
		if key == ">" {
			m.cycleSort(+1)
			return m, nil
		}
		if key == "<" {
			m.cycleSort(-1)
			return m, nil
		}
		if key == "S" {
			m.toggleSortDir()
			return m, nil
		}
		if key == "n" && m.cfg.CreateFn != nil {
			m.create = newCreateFormModel(m.cfg)
			return m, nil
		}
		if key == "e" && m.cfg.EditFn != nil {
			row := m.selected()
			if row == nil {
				m.err = fmt.Errorf("no row selected")
				return m, nil
			}
			m.create = newEditFormModel(m.cfg, row)
			return m, nil
		}
		if key == "enter" {
			row := m.selected()
			if row != nil {
				m.detailOpen = true
				m.detailRow = row
			}
			return m, nil
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

// View renders the header + table + confirmation prompt or drawer
// overlay (when active).
func (m *ResourceListModel) View(width int) string {
	var b strings.Builder
	// Title header dropped 2026-06-23 : the sidebar already shows
	// which view is active (▸ <Label>) so the panel title was
	// redundant. Interface uniformized across tabs.
	if m.loading {
		b.WriteString(m.theme.Faint.Render("loading…"))
		return b.String()
	}
	if m.err != nil {
		b.WriteString(m.theme.BadgeBad.Render("error: " + m.err.Error()))
		return b.String()
	}
	if m.create != nil {
		b.WriteString(m.create.View(m.theme))
		return b.String()
	}
	if m.detailOpen {
		b.WriteString(m.renderDetail(width))
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

// renderDetail draws the inspector drawer : every key/value of the
// selected row in a deterministic order. Keys are sorted alphabetically
// for stable diffing across refreshes (operators can mentally
// pin-point what changed without the table re-arranging itself).
func (m *ResourceListModel) renderDetail(width int) string {
	if m.detailRow == nil {
		return m.theme.Faint.Render("(no row selected)")
	}
	keys := make([]string, 0, len(m.detailRow))
	for k := range m.detailRow {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(m.theme.Title.Render(m.cfg.Title + " — detail"))
	b.WriteString("\n\n")
	// Render as a two-column "key  value" layout. Column widths are
	// derived from the longest key in the row so values line up.
	maxKey := 0
	for _, k := range keys {
		if len(k) > maxKey {
			maxKey = len(k)
		}
	}
	for _, k := range keys {
		v := m.detailRow[k]
		b.WriteString(m.theme.StatusKey.Render(padRight(k, maxKey)))
		b.WriteString("  ")
		b.WriteString(m.theme.StatusVal.Render(fmt.Sprintf("%v", v)))
		b.WriteString("\n")
	}
	// Related hosts section : AZ + Rack drawers list the hosts
	// attached to this row (the AZ → Rack → Host hierarchy made
	// flesh). Skipped silently for resources without a relatedHosts
	// callback wired or when the lookup returns nothing.
	if m.relatedHosts != nil {
		hosts := m.relatedHosts(m.cfg.ID, m.detailRow)
		if len(hosts) > 0 {
			b.WriteString("\n")
			b.WriteString(m.theme.Title.Render(fmt.Sprintf("Hosts (%d)", len(hosts))))
			b.WriteString("\n")
			for _, h := range hosts {
				line := fmt.Sprintf("  %-20s  %-8s  %-8s  %-6s  %s",
					h.Hostname, dashEmpty(h.AZ), dashEmpty(h.Rack),
					dashEmpty(h.Hypervisor), dashEmpty(h.State))
				b.WriteString(m.theme.StatusVal.Render(line))
				b.WriteString("\n")
			}
		}
	}
	// Related projects section : Tenant detail drawer lists the
	// projects attached to this tenant. Mirrors the AZ/Rack→Hosts
	// pattern.
	if m.relatedProjects != nil {
		projects := m.relatedProjects(m.cfg.ID, m.detailRow)
		if len(projects) > 0 {
			b.WriteString("\n")
			b.WriteString(m.theme.Title.Render(fmt.Sprintf("Projects (%d)", len(projects))))
			b.WriteString("\n")
			for _, p := range projects {
				// Full UUID — every catalogue view shows the 36-char
				// form since 2026-06-24. Audit 2026-06-25 removed
				// the [:8] truncation here for consistency.
				line := fmt.Sprintf("  %-24s  %-36s", p.Name, p.UUID)
				b.WriteString(m.theme.StatusVal.Render(line))
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("\n")
	b.WriteString(m.theme.Faint.Render("press Esc or q to close"))
	return b.String()
}

// padRight right-pads s with spaces to width n. No-op when len(s) >= n.
func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// Actions returns the help footer entries for this resource.
func (m *ResourceListModel) Actions() []ResourceAction { return m.cfg.Actions }
