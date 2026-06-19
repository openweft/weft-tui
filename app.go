// Package tui — the Bubble Tea entry point. The Model here is the
// top-level state machine : it owns the active tab, dispatches key
// events to the tab-specific sub-models (only Hosts is functional in
// this first slice), runs the 5-second auto-refresh tick, and renders
// the chrome (tabs header + status bar) shared by every tab.
//
// Subsequent slices wire the VMs, Projects and Events tabs ; their
// stubs already register so the navigation chrome is complete.
package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// tab identifies one of the top-level views. The integer is also
// the 1-indexed shortcut the user presses (`1` → tabHosts, …).
type tab int

const (
	tabHosts tab = iota
	tabVMs
	tabProjects
	tabEvents
)

// tabLabels mirrors `tab` ; order MUST match the const block above.
var tabLabels = []string{"Hosts", "VMs", "Projects", "Events"}

// refreshInterval is the cadence the model auto-refreshes the current
// tab (hosts list, eventually VMs / events stream). 5s mirrors the
// CLI's `weft host ls` polling defaults — fresh enough for live
// drains, not so fast it hammers the agent socket.
const refreshInterval = 5 * time.Second

// errNoClient is returned by Cmd factories when the model was built
// without a gRPC client (e.g. dry-run / unit tests). The view surfaces
// it as a status-bar error so users notice the misconfiguration.
var errNoClient = errors.New("no agent client (dry-run mode)")

// Model is the Bubble Tea state. Exported so the test suite can poke
// at it directly without going through tea.Program.
type Model struct {
	theme  Theme
	client HostsClient

	active   tab
	hosts    hostsModel
	width    int
	height   int
	showHelp bool

	// Status bar.
	statusMsg string
	statusErr bool
}

// New builds a fresh top-level model. Pass nil for client in tests
// or dry-run builds — every Cmd factory degrades gracefully.
func New(client HostsClient) Model {
	theme := NewTheme()
	return Model{
		theme:  theme,
		client: client,
		active: tabHosts,
		hosts:  newHostsModel(theme),
	}
}

// Init returns the startup Cmd : kick off the first hosts fetch +
// arm the auto-refresh ticker.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		loadHostsCmd(m.client),
		tickRefresh(),
	)
}

// refreshTickMsg is the tea.Tick payload that fires every
// refreshInterval. The model uses it to re-fetch the active tab's
// data and to re-arm itself.
type refreshTickMsg struct{ t time.Time }

func tickRefresh() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return refreshTickMsg{t: t}
	})
}

// Update is the heart of the Bubble Tea loop. Returns the new model
// plus any side-effect Cmd to schedule (RPC, tick, quit).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Reserve 3 lines : tabs row + blank + status bar.
		h := msg.Height - 4
		if h < 5 {
			h = 5
		}
		m.hosts.table.SetHeight(h)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case refreshTickMsg:
		// Re-arm the ticker + refresh whichever tab is active.
		cmds := []tea.Cmd{tickRefresh()}
		if m.active == tabHosts {
			cmds = append(cmds, loadHostsCmd(m.client))
		}
		return m, tea.Batch(cmds...)

	case hostsLoadedMsg:
		if msg.err != nil {
			m.hosts.err = msg.err
			m.hosts.loading = false
			m.setError("refresh failed: " + msg.err.Error())
		} else if msg.resp != nil {
			m.hosts.applyHosts(msg.resp)
		}
		return m, nil

	case hostActionMsg:
		if msg.err != nil {
			m.setError(fmt.Sprintf("%s %s failed: %s", msg.action, msg.host, msg.err))
		} else {
			m.setMsg(fmt.Sprintf("%s %s ok", msg.action, msg.host))
		}
		// Refresh hosts so the table reflects the new state.
		return m, loadHostsCmd(m.client)
	}
	// Non-key events get forwarded to the table so scroll/select
	// animation messages still flow when we add them later.
	var cmd tea.Cmd
	m.hosts.table, cmd = m.hosts.table.Update(msg)
	return m, cmd
}

// handleKey is the key-event dispatcher. Confirmation-modal keys
// short-circuit ; help-overlay keys short-circuit ; everything else
// goes through the normal global / tab-specific routes.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Modal: confirm-remove. Only y/n/Esc/Ctrl+C are honoured.
	if m.hosts.confirmRemove != "" {
		switch key {
		case "y", "Y":
			uuid := m.hosts.confirmRemove
			host := m.hosts.confirmHostname
			m.hosts.confirmRemove = ""
			m.hosts.confirmHostname = ""
			return m, deleteHostCmd(m.client, uuid, host)
		case "n", "N", "esc", "ctrl+c":
			m.hosts.confirmRemove = ""
			m.hosts.confirmHostname = ""
			m.setMsg("remove cancelled")
			return m, nil
		}
		return m, nil
	}

	// Help overlay: ? toggles, anything else closes it without
	// dispatching the key further.
	if m.showHelp {
		if key == "?" || key == "esc" || key == "q" {
			m.showHelp = false
		}
		return m, nil
	}

	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.showHelp = true
		return m, nil
	case "1":
		m.active = tabHosts
		m.clearStatus()
		return m, loadHostsCmd(m.client)
	case "2":
		m.active = tabVMs
		m.clearStatus()
		return m, nil
	case "3":
		m.active = tabProjects
		m.clearStatus()
		return m, nil
	case "4":
		m.active = tabEvents
		m.clearStatus()
		return m, nil
	case "r":
		if m.active == tabHosts {
			m.setMsg("refreshing…")
			return m, loadHostsCmd(m.client)
		}
		return m, nil
	}

	// Hosts-tab-specific keys.
	if m.active == tabHosts {
		switch key {
		case "c":
			uuid := m.hosts.selectedUUID()
			host := m.hosts.selectedHostname()
			if uuid == "" {
				m.setError("no host selected")
				return m, nil
			}
			return m, cordonCmd(m.client, uuid, host, true)
		case "u":
			uuid := m.hosts.selectedUUID()
			host := m.hosts.selectedHostname()
			if uuid == "" {
				m.setError("no host selected")
				return m, nil
			}
			return m, cordonCmd(m.client, uuid, host, false)
		case "d":
			uuid := m.hosts.selectedUUID()
			host := m.hosts.selectedHostname()
			if uuid == "" {
				m.setError("no host selected")
				return m, nil
			}
			return m, setStateCmd(m.client, uuid, host, "down")
		case "x":
			uuid := m.hosts.selectedUUID()
			host := m.hosts.selectedHostname()
			if uuid == "" {
				m.setError("no host selected")
				return m, nil
			}
			m.hosts.confirmRemove = uuid
			m.hosts.confirmHostname = host
			return m, nil
		}
		// Forward to the table for navigation (↑/↓/j/k).
		var cmd tea.Cmd
		m.hosts.table, cmd = m.hosts.table.Update(msg)
		return m, cmd
	}

	return m, nil
}

// View renders the full UI : header (tabs), body (active tab), status
// bar. Help overlay supersedes the body when open.
func (m Model) View() string {
	if m.showHelp {
		return m.theme.helpView(m.width)
	}

	header := m.renderTabs()
	body := m.renderBody()
	status := m.renderStatusBar()

	return strings.Join([]string{header, body, status}, "\n")
}

func (m Model) renderTabs() string {
	parts := make([]string, len(tabLabels))
	for i, label := range tabLabels {
		text := fmt.Sprintf("%d %s", i+1, label)
		if tab(i) == m.active {
			parts[i] = m.theme.ActiveTab.Render(text)
		} else {
			parts[i] = m.theme.Tab.Render(text)
		}
	}
	title := m.theme.Title.Render("weft tui")
	tabs := lipgloss.JoinHorizontal(lipgloss.Top, parts...)
	return lipgloss.JoinHorizontal(lipgloss.Top, title, tabs)
}

func (m Model) renderBody() string {
	switch m.active {
	case tabHosts:
		return m.hosts.View(m.width)
	case tabVMs:
		return m.theme.Faint.Render("\n  VMs tab — coming soon.\n  Use `weft instance ls` / `weft microvm ls` meanwhile.")
	case tabProjects:
		return m.theme.Faint.Render("\n  Projects tab — coming soon.\n  Use `weft project ls` meanwhile.")
	case tabEvents:
		return m.theme.Faint.Render("\n  Events tab — coming soon.\n  Use `weft events tail` meanwhile.")
	}
	return ""
}

func (m Model) renderStatusBar() string {
	left := m.theme.StatusKey.Render(tabLabels[m.active])
	mid := ""
	if !m.hosts.lastRefresh.IsZero() && m.active == tabHosts {
		mid = m.theme.StatusVal.Render(" refreshed " + m.hosts.lastRefresh.Format("15:04:05"))
	}
	right := ""
	if m.statusMsg != "" {
		if m.statusErr {
			right = m.theme.StatusErr.Render(m.statusMsg)
		} else {
			right = m.theme.StatusMsg.Render(m.statusMsg)
		}
	} else {
		right = m.theme.Faint.Render("press ? for help")
	}
	content := left + "  " + mid + "  " + right
	return m.theme.StatusBar.Render(content)
}

// setMsg / setError / clearStatus are the tiny helpers the rest of
// the model uses to write into the status bar without sprinkling
// the field names everywhere.
func (m *Model) setMsg(s string) {
	m.statusMsg = s
	m.statusErr = false
}

func (m *Model) setError(s string) {
	m.statusMsg = s
	m.statusErr = true
}

func (m *Model) clearStatus() {
	m.statusMsg = ""
	m.statusErr = false
}

// ActiveTab is a tiny accessor exported for tests : Bubble Tea's
// `View()` output is hard to assert on, so tests transition state
// then read this directly.
func (m Model) ActiveTab() int { return int(m.active) }

// ShowHelp reports whether the help overlay is currently open.
// Exported for tests.
func (m Model) ShowHelp() bool { return m.showHelp }

// ConfirmingRemove reports whether the destructive-remove confirm
// modal is open. Exported for tests.
func (m Model) ConfirmingRemove() bool { return m.hosts.confirmRemove != "" }

// StatusMessage returns the current status-bar message (msg, isError).
// Exported for tests.
func (m Model) StatusMessage() (string, bool) { return m.statusMsg, m.statusErr }
