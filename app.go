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

	"github.com/charmbracelet/bubbles/table"
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

	// Context menu (right-click). When open, the status bar is
	// replaced with a strip of available actions for the currently-
	// selected row. Click an item or press its shortcut to fire ;
	// Esc closes.
	menu contextMenu
}

// contextMenu holds the right-click menu state. items is rebuilt on
// every right-click based on the active tab + the selected row, so
// the menu always reflects what's actionable. cursor highlights one
// item for keyboard navigation ; left-arrow / right-arrow move it.
type contextMenu struct {
	open   bool
	items  []contextMenuItem
	cursor int
}

// contextMenuItem is one row in the menu. action is the tea.Cmd to
// dispatch on activation (Enter / click / shortcut). Build lazily
// so each click captures the current selection.
type contextMenuItem struct {
	label    string
	shortcut string
	action   tea.Cmd
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
			applyResize(&rm.table, cfg.Columns, m.bodyInnerWidth(), m.bodyHeight()-2)
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
	case tea.MouseMsg:
		return m.handleMouse(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Reserve : status bar (2 lines) + 1 line breather + sidebar
		// occupies its own horizontal slot, so the body width must
		// exclude it. Match bodyHeight()/bodyWidth() so applyResize
		// + renderBody agree on the viewport.
		h := m.bodyHeight() - 2 // account for BodyBox border top+bottom
		bw := m.bodyInnerWidth()
		// Table widgets : resize BOTH height (to fill the viewport)
		// AND column widths (proportionally to the declared widths
		// in their original definitions — captured by the per-tab
		// modeOriginalColumns helpers below). The bubbles/table
		// widget doesn't reflow on its own, so this is the single
		// source of responsive layout for the whole TUI.
		applyResize(&m.hosts.table, hostsColumns(), bw, h)
		applyResize(&m.vms.table, vmsColumns(), bw, h)
		applyResize(&m.projects.table, projectsColumns(), bw, h)
		// Every previously-opened resource list inherits the new
		// size too. Lazily-created ones (palette opens a fresh
		// resource later) pick it up via newResourceListModel
		// reading m.width/m.height at construction time.
		for _, rm := range m.resource {
			applyResize(&rm.table, rm.cfg.Columns, bw, h)
		}
		// The events viewport reserves one line for the header.
		evpH := h - 1
		if evpH < 3 {
			evpH = 3
		}
		m.events.vp.Width = bw
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
			m.vms.refreshHostNames(m.hosts.placementByUUID)
			// Cross-pollinate VM counts so the new hosts table
			// reflects the prior VM listing's placement.
			m.hosts.applyVMCounts(vmCountsByHost(m.vms.rows))
		}
		return m, nil

	case openHostDetailMsg:
		row, ok := m.hosts.rowByUUID(msg.uuid)
		if !ok {
			return m, nil
		}
		_ = row
		m.hosts.detailOpen = true
		m.hosts.detailUUID = msg.uuid
		return m, nil

	case openHostConfirmRemoveMsg:
		m.hosts.confirmRemove = msg.uuid
		m.hosts.confirmHostname = msg.hostname
		return m, nil

	case openVMConfirmStopMsg:
		m.vms.confirmStop = msg.name
		m.vms.confirmProject = msg.project
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
			m.vms.applyVMs(msg.resp, m.hosts.placementByUUID)
			// Push fresh per-host counts to the hosts model so the
			// "VMS" column reflects the new placement immediately —
			// no need to wait for the next hostsLoadedMsg.
			m.hosts.applyVMCounts(vmCountsByHost(m.vms.rows))
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

	// --- Context menu first : navigation captures everything. ---
	if m.menu.open {
		switch key {
		case "esc", "ctrl+c":
			m.menu.open = false
			return m, nil
		case "up", "k":
			if m.menu.cursor > 0 {
				m.menu.cursor--
			}
			return m, nil
		case "down", "j":
			if m.menu.cursor < len(m.menu.items)-1 {
				m.menu.cursor++
			}
			return m, nil
		case "enter":
			return m.menuActivate()
		}
		// Any matching item shortcut triggers it directly.
		for _, it := range m.menu.items {
			if it.shortcut == key {
				m.menu.open = false
				return m, it.action
			}
		}
		// Other keys fall through to the regular tab handlers so
		// muscle-memory shortcuts (refresh, etc.) still work even
		// with the menu open.
	}

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

// View renders the full UI : sidebar (object types) on the left,
// body (active tab content) on the right, status bar at the
// bottom. Help overlay supersedes the body when open.
func (m Model) View() string {
	if m.showHelp {
		return m.theme.helpView(m.width)
	}
	sidebar := m.renderSidebar()
	body := m.renderBody()
	main := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, body)
	parts := []string{main}
	if m.palette.open {
		parts = append(parts, m.palette.View(m.theme, m.width))
	}
	if m.menu.open {
		// Context menu pre-empts the status bar : the items + the
		// nav hint are more useful right after a right-click than
		// the timestamp / refresh chrome. Esc / item-click /
		// shortcut all close the menu, after which the status bar
		// returns on the next render.
		parts = append(parts, m.renderContextMenu())
	} else {
		parts = append(parts, m.renderStatusBar())
	}
	return strings.Join(parts, "\n")
}

// sidebarWidth is the fixed horizontal slot the sidebar occupies,
// including border + padding. Tuned so the longest catalogue entry
// ("Scheduling Rules" / "Installed Plugins" / "Availability Zones"
// / "SSH Keys (catalogue)" — all ~17-20 chars) fits with its
// shortcut prefix + the active "▸" marker. 28 col leaves a body
// width of 52 col on an 80-col terminal — still readable.
const sidebarWidth = 28

// sidebarEntry is one clickable row in the sidebar. Either tab is
// set (a core tab) or resourceID is set (a catalogue resource).
// Mutually exclusive ; sidebarRows tags each.
type sidebarEntry struct {
	tab        tab
	resourceID string
	label      string
	// shortcut shows the keyboard hint for the row. "1..4" for the
	// core tabs ; "·" for catalogue entries (no global shortcut —
	// click or palette).
	shortcut string
}

// sidebarSections is the ordered list of sections + their entries
// the sidebar renders. Built once from tabLabels (core) +
// resourceCatalogue (grouped by their Section attribute, sorted by
// title within each group). Sections list section.* of the
// catalogue in the same order operators expect : Network, Storage,
// Compute, Identity, Admin.
func sidebarSections() []sidebarSection {
	sections := []sidebarSection{{
		Header:  "core",
		Entries: coreEntries(),
	}}
	// Group the catalogue by Section.
	groups := map[string][]ResourceConfig{}
	order := []string{}
	for _, r := range resourceCatalogue {
		if _, seen := groups[r.Section]; !seen {
			order = append(order, r.Section)
		}
		groups[r.Section] = append(groups[r.Section], r)
	}
	// Stable section ordering : Network → Storage → Compute →
	// Identity → Admin → any unknown bucket last. Matches the
	// catalogue's declaration order today ; if the catalogue grows
	// a new bucket it appears below the known ones.
	preferred := []string{"Network", "Storage", "Compute", "Identity", "Admin"}
	ordered := make([]string, 0, len(order))
	seen := map[string]bool{}
	for _, s := range preferred {
		if _, ok := groups[s]; ok {
			ordered = append(ordered, s)
			seen[s] = true
		}
	}
	for _, s := range order {
		if !seen[s] {
			ordered = append(ordered, s)
		}
	}
	for _, sec := range ordered {
		entries := make([]sidebarEntry, 0, len(groups[sec]))
		for _, r := range groups[sec] {
			entries = append(entries, sidebarEntry{
				resourceID: r.ID,
				label:      r.Title,
				shortcut:   "·",
			})
		}
		sections = append(sections, sidebarSection{
			Header:  strings.ToLower(sec),
			Entries: entries,
		})
	}
	sections = append(sections, sidebarSection{
		Header: "more",
		Entries: []sidebarEntry{
			{label: "palette", shortcut: "^P"},
			{label: "help", shortcut: "?"},
		},
	})
	return sections
}

type sidebarSection struct {
	Header  string
	Entries []sidebarEntry
}

// coreEntries materialises the 4 fixed top-level tabs as sidebar
// entries. The shortcut digit doubles as the keyboard accelerator
// the legacy 1..4 keymap already drives.
func coreEntries() []sidebarEntry {
	out := make([]sidebarEntry, len(tabLabels))
	for i, label := range tabLabels {
		out[i] = sidebarEntry{
			tab:      tab(i),
			label:    label,
			shortcut: fmt.Sprintf("%d", i+1),
		}
	}
	return out
}

// renderSidebar draws the vertical object-type list on the left.
// Core tabs (Hosts/VMs/Projects/Events) sit at the top with their
// numeric shortcut ; the full resource catalogue follows below,
// grouped by section (Network / Storage / Compute / Identity /
// Admin). Click any entry to jump to it ; the active entry picks
// up the SidebarItemActive style.
func (m Model) renderSidebar() string {
	var b strings.Builder
	head := "weft"
	if m.clusterName != "" {
		head += "\n" + m.clusterName
	}
	b.WriteString(m.theme.Title.Render(head))
	b.WriteString("\n")
	for _, sec := range sidebarSections() {
		b.WriteString(m.theme.SidebarSection.Render(sec.Header))
		b.WriteString("\n")
		for _, e := range sec.Entries {
			active := m.isSidebarEntryActive(e)
			b.WriteString(sidebarRow(m.theme, e.shortcut, e.label, active))
			b.WriteString("\n")
		}
	}
	return m.theme.SidebarBox.
		Width(sidebarWidth - 2).
		Height(m.bodyHeight()).
		Render(b.String())
}

// isSidebarEntryActive returns true when the entry corresponds to
// the currently-active view : core tab match for tab-typed entries,
// resource-ID match for catalogue entries.
func (m Model) isSidebarEntryActive(e sidebarEntry) bool {
	if e.resourceID != "" {
		return m.active == tabResource && m.currentResource == e.resourceID
	}
	// "more" rows have no associated tab AND no resource ; they're
	// never "active" — operators use ^P / ? to invoke them.
	if e.shortcut == "^P" || e.shortcut == "?" {
		return false
	}
	return m.active == e.tab
}

// sidebarRow renders one entry : "<shortcut> <label>". Active rows
// pick up the SidebarItemActive style ; inactive rows pad with two
// spaces so the label column lines up regardless of selection.
func sidebarRow(theme Theme, shortcut, label string, active bool) string {
	if active {
		return theme.SidebarItemActive.Render("▸ " + shortcut + " " + label)
	}
	return theme.SidebarItem.Render(shortcut + " " + label)
}

// bodyHeight is the vertical slot the body + sidebar region uses.
// Subtracts the status bar (border + content = 2 lines) from the
// terminal height ; matches the legacy "Reserve 3 lines : tabs row
// + blank + status bar" budget so per-tab tables still get the
// same usable rows.
func (m Model) bodyHeight() int {
	h := m.height - 4
	if h < 5 {
		h = 5
	}
	return h
}

func (m Model) renderBody() string {
	// Body content gets the inner width — the BodyBox below wraps
	// in a rounded border + 1-col horizontal padding, eating 4 cols
	// total. Tables don't reflow on their own so the width budget
	// has to be right at applyResize time, not at render.
	inner := m.bodyInnerWidth()
	var content string
	switch m.active {
	case tabHosts:
		content = m.hosts.View(inner)
	case tabVMs:
		content = m.vms.View(inner)
	case tabProjects:
		content = m.projects.View(inner)
	case tabEvents:
		content = m.events.View(inner)
	case tabResource:
		if rm, ok := m.resource[m.currentResource]; ok {
			content = rm.View(inner)
		}
	}
	return m.theme.BodyBox.
		Width(m.bodyWidth() - 2).
		Height(m.bodyHeight() - 2).
		Render(content)
}

// bodyInnerWidth is what the body content actually has to draw on,
// once the BodyBox border (2 cols) + padding (2 cols) is subtracted
// from bodyWidth. Tables use this for column width allocation.
func (m Model) bodyInnerWidth() int {
	w := m.bodyWidth() - 4
	if w < 16 {
		w = 16
	}
	return w
}

// bodyWidth is the horizontal slot the body region uses — total
// terminal width minus the sidebar slot. Floors at 20 so the table
// stays renderable on absurdly narrow terminals (caller will then
// scroll horizontally inside the table).
func (m Model) bodyWidth() int {
	w := m.width - sidebarWidth
	if w < 20 {
		w = 20
	}
	return w
}

// renderContextMenu draws the menu strip that replaces the status
// bar when m.menu.open. One line per item, prefixed with "▸ " on
// the selected row + the shortcut key in brackets. Footer reminds
// the operator of the keyboard navigation contract.
func (m Model) renderContextMenu() string {
	if !m.menu.open || len(m.menu.items) == 0 {
		return ""
	}
	var b strings.Builder
	for i, it := range m.menu.items {
		if i == m.menu.cursor {
			b.WriteString(m.theme.SidebarItemActive.Render("▸ " + it.label + "  [" + it.shortcut + "]"))
		} else {
			b.WriteString(m.theme.SidebarItem.Render(it.label + "  [" + it.shortcut + "]"))
		}
		b.WriteString("\n")
	}
	b.WriteString(m.theme.Faint.Render("↑↓ select · ↵ run · click · Esc close"))
	return m.theme.HelpBox.Width(m.bodyWidth() - 4).Render(b.String())
}

// menuHitRow translates a click Y coordinate to a menu item index
// when the click lands inside the rendered menu's vertical extent.
// Returns (idx, false) when y is outside the menu, so the caller
// closes the menu instead of dispatching.
//
// Layout (relative to terminal Y) :
//   row N-K to N-1 : menu items + footer + border
// where N is the terminal height + the menu has K=len(items)+1
// content rows + 2 border rows (HelpBox has padding 1,2).
func (m Model) menuHitRow(y int) (int, bool) {
	if !m.menu.open {
		return 0, false
	}
	// Menu spans the bottom of the screen. The HelpBox style adds
	// 1 border + 1 padding row top & bottom. So content rows live
	// in [m.height - bottomChrome - len(items) - 1, m.height - bottomChrome - 2).
	// Simpler : render the menu, count its lines, and map by index.
	rendered := m.renderContextMenu()
	lines := strings.Split(rendered, "\n")
	menuTop := m.height - len(lines)
	rel := y - menuTop
	if rel < 0 || rel >= len(lines) {
		return 0, false
	}
	// Within the menu : the first 2 lines are border+padding, then
	// one line per item, then footer, then border+padding.
	itemY := rel - 2
	if itemY < 0 || itemY >= len(m.menu.items) {
		return 0, false
	}
	return itemY, true
}

// menuActivate dispatches the currently-selected menu item's action
// + closes the menu. Returns (m, action-cmd). Defensive : guards
// against an out-of-range cursor + a nil action.
func (m Model) menuActivate() (tea.Model, tea.Cmd) {
	if !m.menu.open || m.menu.cursor < 0 || m.menu.cursor >= len(m.menu.items) {
		m.menu.open = false
		return m, nil
	}
	cmd := m.menu.items[m.menu.cursor].action
	m.menu.open = false
	return m, cmd
}

// buildContextMenu assembles the right-click action list for the
// currently-active tab + selected row. Returns an empty list when
// nothing is selected, so callers can no-op cleanly. Each item's
// action is a pre-built tea.Cmd capturing the selection at click
// time — moving the cursor afterwards doesn't change what the menu
// fires.
func (m Model) buildContextMenu() []contextMenuItem {
	switch m.active {
	case tabHosts:
		uuid := m.hosts.selectedUUID()
		host := m.hosts.selectedHostname()
		if uuid == "" {
			return nil
		}
		row, ok := m.hosts.selectedRow()
		if !ok {
			return nil
		}
		items := []contextMenuItem{
			{label: "Detail", shortcut: "↵", action: openHostDetailCmd(uuid)},
		}
		if row.Cordoned {
			items = append(items, contextMenuItem{label: "Uncordon", shortcut: "u",
				action: cordonCmd(m.client, uuid, host, false)})
		} else {
			items = append(items, contextMenuItem{label: "Cordon", shortcut: "c",
				action: cordonCmd(m.client, uuid, host, true)})
		}
		items = append(items, contextMenuItem{label: "Mark Down", shortcut: "d",
			action: setStateCmd(m.client, uuid, host, "down")})
		items = append(items, contextMenuItem{label: "Remove…", shortcut: "x",
			action: openHostConfirmRemoveCmd(uuid, host)})
		return items
	case tabVMs:
		name, project := m.vms.selected()
		if name == "" {
			return nil
		}
		return []contextMenuItem{
			{label: "Start", shortcut: "s", action: startVMCmd(m.client, name, project)},
			{label: "Restart", shortcut: "R", action: restartVMCmd(m.client, name, project)},
			{label: "Stop…", shortcut: "S", action: openVMConfirmStopCmd(name, project)},
			{label: "Logs", shortcut: "l", action: loadVMLogsCmd(m.client, name, project)},
		}
	}
	return nil
}

// openHostDetailCmd / openHostConfirmRemoveCmd / openVMConfirmStopCmd
// are no-arg tea.Cmds that just emit a tagged message the Update
// loop interprets to flip a modal flag on the right sub-model.
// Keeps buildContextMenu pure (no model mutation) so the View / Cmd
// boundaries stay clean.
type openHostDetailMsg struct{ uuid string }
type openHostConfirmRemoveMsg struct{ uuid, hostname string }
type openVMConfirmStopMsg struct{ name, project string }

func openHostDetailCmd(uuid string) tea.Cmd {
	return func() tea.Msg { return openHostDetailMsg{uuid: uuid} }
}
func openHostConfirmRemoveCmd(uuid, hostname string) tea.Cmd {
	return func() tea.Msg { return openHostConfirmRemoveMsg{uuid: uuid, hostname: hostname} }
}
func openVMConfirmStopCmd(name, project string) tea.Cmd {
	return func() tea.Msg { return openVMConfirmStopMsg{name: name, project: project} }
}

// entryClickable returns true when the sidebar entry has an actual
// target (core tab or catalogue resource). The "more" rows (palette
// / help, shortcuts "^P" / "?") are pure keyboard hints — clicking
// them is a no-op so we skip them in the hit map.
func entryClickable(e sidebarEntry) bool {
	return e.resourceID != "" || (e.shortcut != "^P" && e.shortcut != "?")
}

// sidebarHitRows maps each rendered Y coordinate inside the sidebar
// to the target the operator activates by clicking that row. Rather
// than predict lipgloss border / padding / section padding offsets
// (which drift whenever the theme tweaks them), we render the
// sidebar once and scan the resulting lines for each entry label.
// Cheap (~1ms) and immune to layout refactors.
//
// Strips ANSI escape sequences before substring-matching so the
// terminal color codes on active rows don't break the lookup.
func (m Model) sidebarHitRows() map[int]sidebarEntry {
	rendered := m.renderSidebar()
	lines := strings.Split(rendered, "\n")
	out := make(map[int]sidebarEntry, 32)
	for _, sec := range sidebarSections() {
		for _, e := range sec.Entries {
			// "more" rows (palette / help) have non-targetable
			// shortcuts ; skip them so a click doesn't no-op
			// silently. Core entries (tab=0 for Hosts via iota,
			// shortcut "1") + resource entries (shortcut "·")
			// stay clickable.
			if !entryClickable(e) {
				continue
			}
			needle := e.shortcut + " " + e.label
			for y, line := range lines {
				if _, taken := out[y]; taken {
					continue
				}
				plain := stripANSI(line)
				if strings.Contains(plain, needle) {
					out[y] = e
					break
				}
			}
		}
	}
	return out
}

// stripANSI removes CSI escape sequences from s. lipgloss writes
// these to colour active sidebar rows, and the substring search
// in sidebarHitRows needs the raw label text to match.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\x1b' || i+1 >= len(s) || s[i+1] != '[' {
			b.WriteByte(s[i])
			continue
		}
		// Skip "\x1b[" + parameters + final byte (in 0x40..0x7e).
		i += 2
		for i < len(s) {
			c := s[i]
			if c >= 0x40 && c <= 0x7e {
				break
			}
			i++
		}
	}
	return b.String()
}

// handleMouse routes mouse events to the right surface :
//
//   - Left-click in the sidebar column → switch the active tab to
//     whichever entry the operator clicked. Triggers the same
//     loadCmd path the keyboard shortcut does.
//   - Left-click in the body column → move the bubbles/table
//     cursor to the clicked row (when the active tab owns a table).
//   - Wheel up/down anywhere over the body → scroll the table's
//     viewport one row (matches every other terminal table widget).
//
// Motion + non-left buttons are ignored — they would only add
// noise (hover highlighting isn't worth the redraw cost here).
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.scrollActiveTable(-1)
		return m, nil
	case tea.MouseButtonWheelDown:
		m.scrollActiveTable(1)
		return m, nil
	case tea.MouseButtonRight:
		// Right-click acts on the release event so a click-drag-
		// release doesn't open the menu off-row.
		if msg.Action != tea.MouseActionRelease {
			return m, nil
		}
		// Outside the body region : ignore. The sidebar has its
		// own click semantics ; right-clicking it would be
		// surprising.
		if msg.X < sidebarWidth {
			m.menu.open = false
			return m, nil
		}
		// Move the table cursor to the clicked row first so the
		// menu reflects the right selection — then build the menu.
		row := msg.Y - 2
		if row < 0 {
			return m, nil
		}
		m.setActiveTableCursor(row)
		items := m.buildContextMenu()
		if len(items) == 0 {
			m.menu.open = false
			return m, nil
		}
		m.menu.open = true
		m.menu.items = items
		m.menu.cursor = 0
		return m, nil
	case tea.MouseButtonLeft:
		// Single click = MouseActionPress ; we only act on the
		// release so motion-while-pressed doesn't drag-select.
		if msg.Action != tea.MouseActionRelease {
			return m, nil
		}
		if msg.X < sidebarWidth {
			if e, ok := m.sidebarHitRows()[msg.Y]; ok {
				m.menu.open = false
				if e.resourceID != "" {
					cmd := m.switchToResource(e.resourceID)
					return m, cmd
				}
				return m.activateTab(e.tab)
			}
			return m, nil
		}
		// Context menu open : map the click to a menu item.
		if m.menu.open {
			if y, ok := m.menuHitRow(msg.Y); ok {
				m.menu.cursor = y
				return m.menuActivate()
			}
			// Click outside the menu lines closes it.
			m.menu.open = false
			return m, nil
		}
		// Body click → table-row cursor move. The bubbles/table
		// reserves 1 line for the header and 1 line at the top
		// for the styled border ; the first data row sits at
		// rendered row 2 (relative to the body region's top).
		row := msg.Y - 2
		if row < 0 {
			return m, nil
		}
		m.setActiveTableCursor(row)
		return m, nil
	}
	return m, nil
}

// activateTab switches the model to the requested tab + arms the
// corresponding refresh Cmd so the table reflects current data
// immediately. Single source of truth shared with the keyboard
// "1..4" shortcuts (callers in handleKey use the same path).
func (m Model) activateTab(t tab) (tea.Model, tea.Cmd) {
	m.active = t
	switch t {
	case tabHosts:
		return m, loadHostsCmd(m.client)
	case tabVMs:
		return m, loadVMsCmd(m.client)
	case tabProjects:
		return m, loadProjectsCmd(m.client)
	}
	return m, nil
}

// scrollActiveTable bumps the cursor of the active tab's table by
// delta rows. -1 = wheel up, +1 = wheel down. No-op for tabs that
// don't own a table (events viewport scrolls itself via its own
// mouse handling — bubbles/viewport supports the wheel natively).
func (m *Model) scrollActiveTable(delta int) {
	tbl := m.activeTable()
	if tbl == nil {
		return
	}
	cursor := tbl.Cursor() + delta
	if cursor < 0 {
		cursor = 0
	}
	tbl.SetCursor(cursor)
}

// setActiveTableCursor moves the bubbles/table cursor on the active
// tab to a specific row. row is 0-indexed against the visible rows
// (the table widget translates that into the underlying data row).
func (m *Model) setActiveTableCursor(row int) {
	tbl := m.activeTable()
	if tbl == nil {
		return
	}
	if row < 0 {
		row = 0
	}
	tbl.SetCursor(row)
}

// activeTable returns the bubbles/table widget the active tab
// owns, or nil for tabs that don't have one (events). Pointer
// receiver so callers mutate the widget in place.
func (m *Model) activeTable() *table.Model {
	switch m.active {
	case tabHosts:
		return &m.hosts.table
	case tabVMs:
		return &m.vms.table
	case tabProjects:
		return &m.projects.table
	case tabResource:
		if rm, ok := m.resource[m.currentResource]; ok {
			return &rm.table
		}
	}
	return nil
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
