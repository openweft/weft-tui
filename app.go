// Package tui — the Bubble Tea entry point. The Model here is the
// top-level state machine : it owns the active tab, dispatches key
// events to the tab-specific sub-models (Hosts / VMs / Projects /
// Events all functional from V0.2 onward), runs the 5-second auto-
// refresh tick, and renders the chrome (tabs header + status bar)
// shared by every tab.
package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	weftv1 "github.com/openweft/weft-proto"
)

// tab identifies one of the top-level views. The integer is also
// the 1-indexed shortcut the user presses (`1` → tabHosts, …).
type tab int

const (
	tabHosts tab = iota
	tabVMs
	tabProjects
	tabEvents
	// tabResource = generic ResourceListModel reached via the
	// command palette (`:networks`, `:volumes`, …). Never a fixed
	// shortcut ; the palette is the only path in.
	tabResource
)

// tabLabels mirrors `tab` ; order MUST match the const block above.
// The tabResource label is dynamic (changes per resource) — handled
// in View() instead of relying on this slice.
var tabLabels = []string{"Hosts", "VMs", "Projects", "Events"}

// refreshInterval is the cadence the model auto-refreshes the current
// tab (hosts list, VMs list, projects list ; Events tab streams live
// so the ticker is a no-op there). 5s mirrors the CLI's `weft host
// ls` polling defaults — fresh enough for live drains, not so fast
// it hammers the agent socket.
const refreshInterval = 5 * time.Second

// errNoClient is returned by Cmd factories when the model was built
// without a gRPC client (e.g. dry-run / unit tests). The view surfaces
// it as a status-bar error so users notice the misconfiguration.
var errNoClient = errors.New("no agent client (dry-run mode)")

// Client is the full gRPC surface the TUI consumes. The production
// implementation is the `weftv1.WeftAgentClient` returned by
// weftclient.Client — its method set is a superset of what we need.
// The 4 narrower per-tab interfaces (HostsClient / VMsClient /
// ProjectsClient / EventsClient) all derive from this one so tests
// can keep using narrow fakes.
type Client interface {
	HostsClient
	VMsClient
	ProjectsClient
	EventsClient
}

// Model is the Bubble Tea state. Exported so the test suite can poke
// at it directly without going through tea.Program.
type Model struct {
	theme    Theme
	themeIdx int // index into themePresets ; cycled by the `T` key
	client   Client
	// clusterName is shown in the title bar to disambiguate which
	// federated cluster the operator is currently inspecting.
	// Sourced from --cluster-name flag or $WEFT_CLUSTER_NAME at
	// main() time ; empty hides the suffix and the title reads as
	// "weft tui" only (the pre-v0.3.6 behaviour).
	clusterName string

	active   tab
	hosts    hostsModel
	vms      vmsModel
	projects projectsModel
	events   eventsModel
	// resource is the active ResourceListModel when active == tabResource ;
	// nil otherwise. Reused across palette switches : creating a new
	// model on every switch would reset the cursor / cause flicker.
	resource map[string]*ResourceListModel
	// currentResource holds the slug of the resource being displayed
	// when active == tabResource.
	currentResource string
	palette         paletteModel
	width           int
	height          int
	showHelp        bool

	// eventsPump bridges the WatchEvents goroutine to the Update
	// loop. Allocated lazily the first time we open the Events tab,
	// then re-used across tab switches.
	eventsPump *eventStreamPump

	// Status bar.
	statusMsg string
	statusErr bool
}

// New builds a fresh top-level model. Pass nil for client in tests
// or dry-run builds — every Cmd factory degrades gracefully.
//
// Theme : reads the operator's persisted choice from
// ~/.weft/tui-theme (falls back to green) so a re-launch keeps the
// last selection. The `T` key cycles through themePresets at
// runtime ; saveTheme persists the new choice.
func New(client Client) Model {
	idx := themeIndexByName(loadSavedTheme())
	theme := NewThemeWith(themePresets[idx])
	return Model{
		theme:    theme,
		themeIdx: idx,
		client:   client,
		active:   tabHosts,
		hosts:    newHostsModel(theme),
		vms:      newVMsModel(theme),
		projects: newProjectsModel(theme),
		events:   newEventsModel(theme),
		resource: map[string]*ResourceListModel{},
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

// switchToResource flips the active view to the named resource
// (creating + initialising its ResourceListModel on first access).
// Called from the command palette's Enter handler. Returns the Init
// Cmd to kick off the initial list fetch.
func (m *Model) switchToResource(id string) tea.Cmd {
	cfg, ok := resourceByID(id)
	if !ok {
		m.setError("unknown resource: " + id)
		return nil
	}
	rm, exists := m.resource[id]
	if !exists {
		// Cast the narrow Client interface to the full WeftAgentClient.
		// In production this is the underlying weftv1.WeftAgentClient ;
		// in tests it's a fake that may or may not implement everything.
		var raw weftv1.WeftAgentClient
		if c, ok := m.client.(weftv1.WeftAgentClient); ok {
			raw = c
		}
		rm = newResourceListModel(m.theme, raw, cfg)
		// Apply the current terminal size at construction so a
		// resource opened AFTER the initial WindowSizeMsg doesn't
		// render at the default 15-row × default-column-width
		// layout. The size handler above also iterates the map on
		// every resize so subsequent terminal changes propagate.
		if m.width > 0 {
			h := m.height - 4
			if h < 5 {
				h = 5
			}
			applyResize(&rm.table, cfg.Columns, m.width, h)
		}
		m.resource[id] = rm
	}
	m.active = tabResource
	m.currentResource = id
	m.clearStatus()
	return rm.Init()
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
		// Table widgets : resize BOTH height (to fill the viewport)
		// AND column widths (proportionally to the declared widths
		// in their original definitions — captured by the per-tab
		// modeOriginalColumns helpers below). The bubbles/table
		// widget doesn't reflow on its own, so this is the single
		// source of responsive layout for the whole TUI.
		applyResize(&m.hosts.table, hostsColumns(), msg.Width, h)
		applyResize(&m.vms.table, vmsColumns(), msg.Width, h)
		applyResize(&m.projects.table, projectsColumns(), msg.Width, h)
		// Every previously-opened resource list inherits the new
		// size too. Lazily-created ones (palette opens a fresh
		// resource later) pick it up via newResourceListModel
		// reading m.width/m.height at construction time.
		for _, rm := range m.resource {
			applyResize(&rm.table, rm.cfg.Columns, msg.Width, h)
		}
		// The events viewport reserves one line for the header.
		evpH := h - 1
		if evpH < 3 {
			evpH = 3
		}
		m.events.vp.Width = msg.Width
		m.events.vp.Height = evpH
		// Logs viewport gets the same window minus the title + hint.
		logH := h - 2
		if logH < 3 {
			logH = 3
		}
		m.vms.logsVP.Width = msg.Width
		m.vms.logsVP.Height = logH
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case resourceLoadedMsg:
		if rm, ok := m.resource[msg.cfg]; ok {
			_, cmd := rm.Update(msg)
			return m, cmd
		}
		return m, nil

	case resourceActionMsg:
		if msg.err != nil {
			m.setError(fmt.Sprintf("%s : %s", msg.action, msg.err))
		} else if msg.msg != "" {
			m.setMsg(msg.msg)
		}
		if rm, ok := m.resource[msg.cfg]; ok {
			return m, rm.loadCmd()
		}
		return m, nil

	case refreshTickMsg:
		// Re-arm the ticker + refresh whichever tab is active.
		cmds := []tea.Cmd{tickRefresh()}
		switch m.active {
		case tabHosts:
			cmds = append(cmds, loadHostsCmd(m.client))
		case tabVMs:
			cmds = append(cmds, loadVMsCmd(m.client))
		case tabProjects:
			cmds = append(cmds, loadProjectsCmd(m.client))
		case tabResource:
			if rm, ok := m.resource[m.currentResource]; ok {
				cmds = append(cmds, rm.loadCmd())
			}
			// Events stream is self-driven ; nothing to do here.
		}
		return m, tea.Batch(cmds...)

	case hostsLoadedMsg:
		if msg.err != nil {
			m.hosts.err = msg.err
			m.hosts.loading = false
			m.setError("refresh failed: " + msg.err.Error())
		} else if msg.resp != nil {
			m.hosts.applyHosts(msg.resp)
			// Refresh the VMs HOST column with the now-populated
			// hosts cache so the resolved hostnames appear without
			// waiting for the next ListVMs tick.
			m.vms.refreshHostNames(m.hosts.hostnameByUUID)
		}
		return m, nil

	case hostActionMsg:
		if msg.err != nil {
			m.setError(fmt.Sprintf("%s %s failed: %s", msg.action, msg.host, msg.err))
		} else {
			m.setMsg(fmt.Sprintf("%s %s ok", msg.action, msg.host))
		}
		return m, loadHostsCmd(m.client)

	case vmsLoadedMsg:
		if msg.err != nil {
			m.vms.err = msg.err
			m.vms.loading = false
			m.setError("refresh failed: " + msg.err.Error())
		} else if msg.resp != nil {
			m.vms.applyVMs(msg.resp, m.hosts.hostnameByUUID)
		}
		return m, nil

	case vmActionMsg:
		if msg.err != nil {
			m.setError(fmt.Sprintf("%s %s failed: %s", msg.action, msg.name, msg.err))
		} else {
			m.setMsg(fmt.Sprintf("%s %s ok", msg.action, msg.name))
		}
		return m, loadVMsCmd(m.client)

	case vmLogsLoadedMsg:
		if msg.err != nil {
			m.vms.logsErr = msg.err
			m.vms.logsLoading = false
		} else {
			m.vms.logsErr = nil
			m.vms.logsLoading = false
			m.vms.logsVP.SetContent(msg.tail)
			m.vms.logsVP.GotoBottom()
		}
		return m, nil

	case projectsLoadedMsg:
		if msg.err != nil {
			m.projects.err = msg.err
			m.projects.loading = false
			m.setError("refresh failed: " + msg.err.Error())
		} else if msg.resp != nil {
			m.projects.applyProjects(msg.resp, msg.counts)
		}
		return m, nil

	case projectActionMsg:
		switch {
		case msg.err != nil:
			m.setError(fmt.Sprintf("%s project %s failed: %s", msg.action, msg.name, msg.err))
		case msg.action == "create" && !msg.created:
			m.setMsg(fmt.Sprintf("project %s already exists", msg.name))
		default:
			m.setMsg(fmt.Sprintf("%s project %s ok", msg.action, msg.name))
		}
		return m, loadProjectsCmd(m.client)

	case eventStreamStartedMsg:
		m.events.started = true
		m.events.err = nil
		return m, receiveNextEventCmd(m.eventsPump)

	case eventReceivedMsg:
		if !m.events.paused && msg.ev != nil {
			m.events.appendLine(m.events.formatEvent(msg.ev))
		}
		return m, receiveNextEventCmd(m.eventsPump)

	case eventStreamErrorMsg:
		if msg.err != nil {
			m.events.err = msg.err
		}
		m.events.started = false
		return m, nil
	}

	// Non-key events get forwarded to whichever tab is active so
	// scroll/animation messages still flow.
	return m.forwardToActiveTab(msg)
}

// forwardToActiveTab delegates non-key messages (animations, viewport
// scroll deltas) to the currently focused sub-model. Each tab returns
// its own Cmd, hoisted back into the top-level Cmd graph.
func (m Model) forwardToActiveTab(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.active {
	case tabHosts:
		m.hosts.table, cmd = m.hosts.table.Update(msg)
	case tabVMs:
		if m.vms.logsOpen {
			m.vms.logsVP, cmd = m.vms.logsVP.Update(msg)
		} else {
			m.vms.table, cmd = m.vms.table.Update(msg)
		}
	case tabProjects:
		if m.projects.creating {
			m.projects.input, cmd = m.projects.input.Update(msg)
		} else {
			m.projects.table, cmd = m.projects.table.Update(msg)
		}
	case tabEvents:
		m.events.vp, cmd = m.events.vp.Update(msg)
	}
	return m, cmd
}

// handleKey is the key-event dispatcher. Confirmation-modal keys
// short-circuit ; help-overlay keys short-circuit ; everything else
// goes through the normal global / tab-specific routes.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// --- Modals first : they capture every key until resolved. ---

	// Hosts confirm-remove modal.
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

	// VMs confirm-stop modal.
	if m.vms.confirmStop != "" {
		switch key {
		case "y", "Y":
			name := m.vms.confirmStop
			project := m.vms.confirmProject
			m.vms.confirmStop = ""
			m.vms.confirmProject = ""
			return m, stopVMCmd(m.client, name, project)
		case "n", "N", "esc", "ctrl+c":
			m.vms.confirmStop = ""
			m.vms.confirmProject = ""
			m.setMsg("stop cancelled")
			return m, nil
		}
		return m, nil
	}

	// VMs logs viewport — scroll keys go to the viewport ; Esc/q
	// closes. While open, no other tab-level key is honoured.
	if m.vms.logsOpen {
		switch key {
		case "esc", "q":
			m.vms.logsOpen = false
			return m, nil
		}
		var cmd tea.Cmd
		m.vms.logsVP, cmd = m.vms.logsVP.Update(msg)
		return m, cmd
	}

	// Projects create-form — Enter submits, Esc cancels, everything
	// else flows into the textinput.
	if m.projects.creating {
		switch key {
		case "esc", "ctrl+c":
			m.projects.creating = false
			m.projects.input.Blur()
			m.projects.input.Reset()
			m.setMsg("create cancelled")
			return m, nil
		case "enter":
			name := strings.TrimSpace(m.projects.input.Value())
			m.projects.creating = false
			m.projects.input.Blur()
			m.projects.input.Reset()
			if name == "" {
				m.setError("empty project name")
				return m, nil
			}
			return m, createProjectCmd(m.client, name)
		}
		var cmd tea.Cmd
		m.projects.input, cmd = m.projects.input.Update(msg)
		return m, cmd
	}

	// Projects confirm-delete modal.
	if m.projects.confirmDeleteUUID != "" {
		switch key {
		case "y", "Y":
			uuid := m.projects.confirmDeleteUUID
			name := m.projects.confirmDeleteName
			m.projects.confirmDeleteUUID = ""
			m.projects.confirmDeleteName = ""
			return m, deleteProjectCmd(m.client, uuid, name)
		case "n", "N", "esc", "ctrl+c":
			m.projects.confirmDeleteUUID = ""
			m.projects.confirmDeleteName = ""
			m.setMsg("delete cancelled")
			return m, nil
		}
		return m, nil
	}

	// Help overlay : ? toggles, anything else closes it.
	if m.showHelp {
		if key == "?" || key == "esc" || key == "q" {
			m.showHelp = false
		}
		return m, nil
	}

	// Command palette open : capture every key until Enter/Esc.
	if m.palette.open {
		_, switchTo := m.palette.handleKey(msg)
		if switchTo != "" {
			cmd := m.switchToResource(switchTo)
			return m, cmd
		}
		return m, nil
	}

	// --- Global keys. ---
	switch key {
	case ":":
		// Open the command palette. Lets the operator type a
		// resource id (`networks`, `volumes`, …) ; Enter switches
		// the active view to the matching ResourceListModel.
		m.palette.open = true
		m.palette.input = ""
		return m, nil
	case "q", "ctrl+c":
		if m.eventsPump != nil {
			m.eventsPump.stop()
		}
		return m, tea.Quit
	case "?":
		m.showHelp = true
		return m, nil
	case "T":
		// Cycle to the next theme preset + persist the choice so
		// the next launch picks it up. Capital T (not lowercase)
		// to avoid colliding with any future single-letter command
		// on the various tabs.
		m.themeIdx = (m.themeIdx + 1) % len(themePresets)
		preset := themePresets[m.themeIdx]
		m.theme = NewThemeWith(preset)
		// Per-tab models keep their own captured theme — refresh
		// them so the new colours take effect on the next render
		// without a tab switch.
		m.hosts.theme = m.theme
		m.vms.theme = m.theme
		m.projects.theme = m.theme
		m.events.theme = m.theme
		for _, r := range m.resource {
			r.theme = m.theme
		}
		if err := saveTheme(preset.Name); err != nil {
			m.statusErr = true
			m.statusMsg = "theme : couldn't persist (" + err.Error() + ")"
		} else {
			m.statusErr = false
			m.statusMsg = "theme → " + preset.Name
		}
		return m, nil
	case "1":
		m.active = tabHosts
		m.clearStatus()
		return m, loadHostsCmd(m.client)
	case "2":
		m.active = tabVMs
		m.clearStatus()
		return m, loadVMsCmd(m.client)
	case "3":
		m.active = tabProjects
		m.clearStatus()
		return m, loadProjectsCmd(m.client)
	case "4":
		m.active = tabEvents
		m.clearStatus()
		// Lazily open the event stream the first time the tab is
		// visited. Cached across tab switches.
		if m.eventsPump == nil && m.client != nil {
			m.eventsPump = newEventStreamPump()
			return m, startEventsStreamCmd(m.client, m.eventsPump)
		}
		return m, nil
	case "r":
		m.setMsg("refreshing…")
		switch m.active {
		case tabHosts:
			return m, loadHostsCmd(m.client)
		case tabVMs:
			return m, loadVMsCmd(m.client)
		case tabProjects:
			return m, loadProjectsCmd(m.client)
		}
		return m, nil
	}

	// --- Tab-specific keys. ---
	switch m.active {
	case tabHosts:
		return m.handleHostsKey(msg, key)
	case tabVMs:
		return m.handleVMsKey(msg, key)
	case tabProjects:
		return m.handleProjectsKey(msg, key)
	case tabEvents:
		return m.handleEventsKey(msg, key)
	case tabResource:
		if rm, ok := m.resource[m.currentResource]; ok {
			_, cmd := rm.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m Model) handleHostsKey(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	// Detail drawer open : Esc/Enter/q closes ; everything else is
	// swallowed so the action keys (c / u / d / x) don't fire while
	// the operator is reading the inspector.
	if m.hosts.detailOpen {
		switch key {
		case "esc", "enter", "q":
			m.hosts.detailOpen = false
			m.hosts.detailUUID = ""
		}
		return m, nil
	}
	switch key {
	case "enter":
		row, ok := m.hosts.selectedRow()
		if !ok {
			m.setError("no host selected")
			return m, nil
		}
		m.hosts.detailOpen = true
		m.hosts.detailUUID = row.UUID
		return m, nil
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
	var cmd tea.Cmd
	m.hosts.table, cmd = m.hosts.table.Update(msg)
	return m, cmd
}

func (m Model) handleVMsKey(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "s":
		name, project := m.vms.selected()
		if name == "" {
			m.setError("no VM selected")
			return m, nil
		}
		return m, startVMCmd(m.client, name, project)
	case "S":
		name, project := m.vms.selected()
		if name == "" {
			m.setError("no VM selected")
			return m, nil
		}
		m.vms.confirmStop = name
		m.vms.confirmProject = project
		return m, nil
	case "R":
		name, project := m.vms.selected()
		if name == "" {
			m.setError("no VM selected")
			return m, nil
		}
		// Restart : single atomic RPC (weft-proto v0.12.0+). The
		// agent rollbacks (restart on same host w/ same network)
		// if the start leg fails — something the prior client-side
		// stop+start chain couldn't offer.
		return m, restartVMCmd(m.client, name, project)
	case "l":
		name, project := m.vms.selected()
		if name == "" {
			m.setError("no VM selected")
			return m, nil
		}
		m.vms.logsOpen = true
		m.vms.logsLoading = true
		m.vms.logsErr = nil
		m.vms.logsTitle = name
		m.vms.logsVP.SetContent("")
		return m, loadVMLogsCmd(m.client, name, project)
	}
	var cmd tea.Cmd
	m.vms.table, cmd = m.vms.table.Update(msg)
	return m, cmd
}

func (m Model) handleProjectsKey(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "n":
		m.projects.creating = true
		m.projects.input.Reset()
		cmd := m.projects.input.Focus()
		return m, cmd
	case "D":
		uuid, name := m.projects.selected()
		if uuid == "" {
			m.setError("no project selected")
			return m, nil
		}
		m.projects.confirmDeleteUUID = uuid
		m.projects.confirmDeleteName = name
		return m, nil
	}
	var cmd tea.Cmd
	m.projects.table, cmd = m.projects.table.Update(msg)
	return m, cmd
}

func (m Model) handleEventsKey(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "p":
		m.events.paused = !m.events.paused
		if m.events.paused {
			m.setMsg("events paused")
		} else {
			m.setMsg("events live")
		}
		return m, nil
	case "c":
		m.events.clearBuffer()
		m.setMsg("events cleared")
		return m, nil
	case "G":
		m.events.vp.GotoBottom()
		m.events.userScrolled = false
		return m, nil
	case "g":
		m.events.vp.GotoTop()
		m.events.userScrolled = true
		return m, nil
	}
	// Scroll keys (j/k/PgUp/PgDn/arrows) flow into the viewport. We
	// flip userScrolled on once they move off the bottom so the
	// auto-scroll behaves.
	var cmd tea.Cmd
	prev := m.events.vp.AtBottom()
	m.events.vp, cmd = m.events.vp.Update(msg)
	if prev && !m.events.vp.AtBottom() {
		m.events.userScrolled = true
	}
	if m.events.vp.AtBottom() {
		m.events.userScrolled = false
	}
	return m, cmd
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
	parts := []string{header, body}
	if m.palette.open {
		parts = append(parts, m.palette.View(m.theme, m.width))
	}
	parts = append(parts, status)
	return strings.Join(parts, "\n")
}

func (m Model) renderTabs() string {
	parts := make([]string, 0, len(tabLabels)+1)
	for i, label := range tabLabels {
		text := fmt.Sprintf("%d %s", i+1, label)
		if tab(i) == m.active {
			parts = append(parts, m.theme.ActiveTab.Render(text))
		} else {
			parts = append(parts, m.theme.Tab.Render(text))
		}
	}
	if m.active == tabResource && m.currentResource != "" {
		parts = append(parts, m.theme.ActiveTab.Render(": "+m.currentResource))
	}
	head := "weft tui"
	if m.clusterName != "" {
		head += " · " + m.clusterName
	}
	title := m.theme.Title.Render(head)
	tabs := lipgloss.JoinHorizontal(lipgloss.Top, parts...)
	return lipgloss.JoinHorizontal(lipgloss.Top, title, tabs)
}

func (m Model) renderBody() string {
	switch m.active {
	case tabHosts:
		return m.hosts.View(m.width)
	case tabVMs:
		return m.vms.View(m.width)
	case tabProjects:
		return m.projects.View(m.width)
	case tabEvents:
		return m.events.View(m.width)
	case tabResource:
		if rm, ok := m.resource[m.currentResource]; ok {
			return rm.View(m.width)
		}
		return ""
	}
	return ""
}

func (m Model) renderStatusBar() string {
	var left string
	if m.active == tabResource {
		left = m.theme.StatusKey.Render(m.currentResource)
	} else {
		left = m.theme.StatusKey.Render(tabLabels[m.active])
	}
	mid := ""
	var ts time.Time
	switch m.active {
	case tabHosts:
		ts = m.hosts.lastRefresh
	case tabVMs:
		ts = m.vms.lastRefresh
	case tabProjects:
		ts = m.projects.lastRefresh
	case tabResource:
		if rm, ok := m.resource[m.currentResource]; ok {
			ts = rm.refresh
		}
	}
	if !ts.IsZero() {
		mid = m.theme.StatusVal.Render(" refreshed " + ts.Format("15:04:05"))
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
// modal is open (Hosts tab). Exported for tests.
func (m Model) ConfirmingRemove() bool { return m.hosts.confirmRemove != "" }

// ConfirmingStop reports whether the destructive-stop confirm modal
// is open (VMs tab). Exported for tests.
func (m Model) ConfirmingStop() bool { return m.vms.confirmStop != "" }

// ConfirmingDeleteProject reports whether the destructive-delete
// confirm modal is open (Projects tab). Exported for tests.
func (m Model) ConfirmingDeleteProject() bool { return m.projects.confirmDeleteUUID != "" }

// CreatingProject reports whether the create-project inline form is
// open. Exported for tests.
func (m Model) CreatingProject() bool { return m.projects.creating }

// LogsOpen reports whether the VM logs overlay is currently shown.
// Exported for tests.
func (m Model) LogsOpen() bool { return m.vms.logsOpen }

// EventsPaused reports the Events tab streaming pause flag. Exported
// for tests.
func (m Model) EventsPaused() bool { return m.events.paused }

// EventLineCount returns the number of lines currently buffered in
// the Events tab. Exported for tests.
func (m Model) EventLineCount() int { return len(m.events.lines) }

// StatusMessage returns the current status-bar message (msg, isError).
// Exported for tests.
func (m Model) StatusMessage() (string, bool) { return m.statusMsg, m.statusErr }
