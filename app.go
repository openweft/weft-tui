// Package tui — the Bubble Tea entry point. The Model here is the
// top-level state machine : it owns the active tab, dispatches key
// events to the tab-specific sub-models (Hosts / VMs / Projects /
// Events all functional from V0.2 onward), runs the 5-second auto-
// refresh tick, and renders the chrome (tabs header + status bar)
// shared by every tab.
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
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
	FlavorsClient
}

// FlavorsClient is the narrow ListFlavors surface needed to fill
// the VMs FLAVOR column's lookup cache.
type FlavorsClient interface {
	ListFlavors(ctx context.Context, in *weftv1.ListFlavorsRequest, opts ...grpc.CallOption) (*weftv1.ListFlavorsResponse, error)
}

// Model is the Bubble Tea state. Exported so the test suite can poke
// at it directly without going through tea.Program.
type Model struct {
	theme    Theme
	themeIdx int // index into themePresets ; cycled by the `T` key
	client   Client
	// flavorByShape caches the (vcpu, mem_mib) → flavor name map.
	// Populated by flavorsLoadedMsg (best-effort, refreshed alongside
	// vmsLoadedMsg). The vmsModel reads it via the flavorLookup
	// closure to fill the FLAVOR column.
	flavorByShape map[flavorShapeKey]string
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

	// sidebarW is the dynamic sidebar width in cells. Starts at
	// defaultSidebarWidth ; the operator can resize it by clicking
	// on the right-edge column and dragging. Persisted across
	// renders within a session ; reset on next launch.
	sidebarW int

	// dragSidebar is true while the operator is currently dragging
	// the sidebar boundary. Set on MouseActionPress at the boundary
	// column, cleared on MouseActionRelease.
	dragSidebar bool

	// logPaneH is the operator-overridden log pane viewport height
	// (set by dragging the horizontal handle between body and log
	// pane). 0 = use default (logPaneDefaultHeight). dragLogPane is
	// true while a drag is in progress.
	logPaneH    int
	dragLogPane bool

	// sidebarCollapsed = true → render the sidebar in icon-only
	// mode (narrow column, shortcuts as glyphs, labels hidden).
	// Toggle via Ctrl+B. Useful on small terminals where the
	// catalogue + labels eat too much horizontal real estate.
	sidebarCollapsed bool

	// sidebarOffset is the row index of the first visible sidebar
	// entry. Increments on wheel-down over the sidebar (or PgDn
	// when the sidebar is focused) so the operator can reach
	// entries past the bottom edge on short terminals. The
	// catalogue is ~34 lines tall — clipped by ~5-10 lines on a
	// 24-30 row terminal, which is what the operator hit when
	// they reported "AZ and Racks still missing".
	sidebarOffset int

	// identity is the connection identity rendered in the topbar's
	// right side : "user@host" for an SSH endpoint, "local" for a
	// Unix-socket endpoint. Updated by connSwitchMsg whenever the
	// ResilientClient swaps endpoints. Empty before the first
	// switch arrives.
	identity string

	// conn is the latest connection-lifecycle line surfaced in the
	// status bar : "● dc1-r1-h1" on a healthy link, "✖ failover failed"
	// on an exhausted retry. Replaces the os.Stderr scrolling that
	// used to bleed through the alt-screen. Empty = silent (initial
	// boot before the first event).
	conn connStatus

	// logPane is the scrollable diagnostic strip rendered between
	// the body and the status bar. Captures connEventMsg /
	// connSwitchMsg + any other operator-facing log line. Auto-scroll
	// follows the tail ; PgUp / wheel-up inside the pane pauses
	// following so the operator can read older entries.
	logPane logPane
}

// connStatus is the snapshot of the resilient connection's latest
// event. level is one of ResilientEventInfo / Warn / Error so the
// status bar can theme it ; msg is the operator-facing string.
type connStatus struct {
	level string
	msg   string
}

// connSwitchMsg + connEventMsg are tea.Msg payloads bridged from the
// ResilientClient callbacks. Without them the callback fires from a
// background goroutine + we'd need to coordinate Model mutation by
// hand ; sending through the Bubble Tea event loop keeps state
// changes serialised through Update.
type connSwitchMsg struct{ active Endpoint }
type connEventMsg struct{ level, msg string }

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
		logPane:  newLogPane(80),
	}
}

// Init returns the startup Cmd : kick off the first hosts fetch +
// arm the auto-refresh ticker.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		loadHostsCmd(m.client),
		// Seed the tenant + project lookup so the VMs tab's
		// "<tenant>:<project>" prefix is populated before the
		// operator switches to it. Without this, the first VMs
		// render shows bare project names until the projects tab
		// is visited or the periodic ticker fires.
		loadProjectsCmd(m.client),
		tickRefresh(),
	)
}

// hostsForResource returns the host rows logically attached to the
// given resource detail row. Matches by :
//
//   - "azs"   : host.AZ == row["code"] (AZ entries use code as their
//               canonical short name, mirrored in HostInfo.AZ)
//   - "racks" : host.Rack == row["code"] (same convention)
//
// Any other resource ID returns nil so the resource detail drawer
// stays unchanged for non-Host-bearing catalogue rows.
func (m *Model) hostsForResource(resourceID string, row map[string]any) []relatedHost {
	if row == nil {
		return nil
	}
	wantCode, _ := row["code"].(string)
	if wantCode == "" {
		return nil
	}
	var match func(h hostsRow) bool
	switch resourceID {
	case "azs":
		match = func(h hostsRow) bool { return h.AZ == wantCode }
	case "racks":
		match = func(h hostsRow) bool { return h.Rack == wantCode }
	default:
		return nil
	}
	out := make([]relatedHost, 0)
	for _, h := range m.hosts.rows {
		if !match(h) {
			continue
		}
		out = append(out, relatedHost{
			Hostname:   h.Hostname,
			AZ:         h.AZ,
			Rack:       h.Rack,
			Hypervisor: h.Hypervisor,
			State:      h.State,
		})
	}
	return out
}

// projectsForResource returns the project rows logically attached
// to the given resource detail row. Today only "tenants" is
// supported : matches by row["uuid"] == project.TenantUUID. The
// projects tab's model holds the list ; the closure called from
// the Tenant detail drawer reads it directly.
func (m *Model) projectsForResource(resourceID string, row map[string]any) []relatedProject {
	if resourceID != "tenants" || row == nil {
		return nil
	}
	tenantUUID, _ := row["uuid"].(string)
	if tenantUUID == "" {
		return nil
	}
	out := make([]relatedProject, 0)
	for _, p := range m.projects.rows {
		if p.TenantUUID != tenantUUID {
			continue
		}
		out = append(out, relatedProject{Name: p.Name, UUID: p.UUID})
	}
	return out
}

// formatActionHint styles one action label so the shortcut key
// pops without bracket clutter. The matching letter in the label
// (case-insensitive) is rendered bold + underlined ; the rest
// stays Faint. When the key letter isn't in the label (e.g. `x`
// for "remove", `R` for "restart"), the key is appended in
// parentheses : "remove (x)". Operator directive 2026-06-24
// "mettre en bold et/ou souligné la lettre concernée plutôt
// qu'ajouter entre crochets".
func formatActionHint(theme Theme, key, label string) string {
	if key == "" || label == "" {
		return label
	}
	// Match the FIRST occurrence of the key in the label, case-
	// insensitive. Preserve the label's original casing in the
	// output (`Activate` stays `Activate`, not `activate`).
	keyLow := strings.ToLower(key)
	labelLow := strings.ToLower(label)
	idx := strings.Index(labelLow, keyLow)
	emph := lipgloss.NewStyle().Bold(true).Underline(true).Foreground(theme.SidebarItemActive.GetForeground())
	if idx < 0 || len(key) != 1 {
		// Fallback : key not present in the label (or multi-rune
		// key like `^P`). Append it in parentheses, bold.
		return theme.Faint.Render(label+" (") + emph.Render(key) + theme.Faint.Render(")")
	}
	prefix := label[:idx]
	letter := label[idx : idx+1]
	suffix := label[idx+1:]
	return theme.Faint.Render(prefix) + emph.Render(letter) + theme.Faint.Render(suffix)
}

// renderActionBar returns a 1-line faint-styled hint with the
// keyboard shortcuts available on the active view. Empty string
// when no actions apply (e.g. modal overlays). The bar is non-
// clickable — keys / context menu (`m`) are the real input paths
// — but it surfaces the verbs operators would otherwise have to
// memorise. 2026-06-24 operator directive.
func (m Model) renderActionBar(width int) string {
	type hint struct{ key, label string }
	// Single "Actions" button (opens the context menu via `m`) +
	// "refresh" (`r`). Operator directive 2026-06-24 : "mettre un
	// bouton Actions qui ouvre le menu avec toutes les actions
	// dedans plutot que de les repeter dans la tabbar. il faut
	// laisser refresh par contre". Per-tab key listings (start /
	// restart / cordon / activate / …) are still keyboard-bound
	// AND discoverable via the menu.
	var hints []hint
	switch m.active {
	case tabHosts, tabVMs, tabProjects:
		hints = []hint{{"a", "Actions"}, {"r", "Refresh"}}
	case tabResource:
		if _, ok := m.resource[m.currentResource]; !ok {
			return ""
		}
		hints = []hint{{"a", "Actions"}, {"r", "Refresh"}}
	default:
		return ""
	}
	if len(hints) == 0 {
		return ""
	}
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		parts = append(parts, formatActionHint(m.theme, h.key, h.label))
	}
	bar := strings.Join(parts, "  ")
	if width > 0 && lipgloss.Width(bar) > width {
		// Truncate so the bar never spills past the body's right
		// edge ; the table widget assumes its allotted width is
		// the full visible area.
		runes := []rune(stripANSI(bar))
		if len(runes) > width {
			bar = string(runes[:width-1]) + "…"
		}
	}
	return m.theme.Faint.Render(bar)
}

// flavorShapeKey indexes the flavor cache by the (vcpu, mem_mib)
// tuple a VM was sized at. Comparable so map[...]string works.
type flavorShapeKey struct {
	vcpu  uint32
	memMb uint64
}

// flavorLookup is the closure vmsModel.applyVMs uses to fill the
// FLAVOR column. Empty cache or no match → empty string ; the
// table widget renders "—". 2026-06-24 VM flavor MVP.
func (m *Model) flavorLookup(cpu uint32, memMb uint64) string {
	if m.flavorByShape == nil {
		return ""
	}
	return m.flavorByShape[flavorShapeKey{vcpu: cpu, memMb: memMb}]
}

// flavorsLoadedMsg carries the result of a one-shot ListFlavors
// load. Update populates m.flavorByShape from it so subsequent
// vmsLoadedMsg + applyVMs hits resolve flavor names via the cache.
type flavorsLoadedMsg struct {
	flavors []*weftv1.Flavor
	err     error
}

// loadFlavorsCmd fires a ListFlavors call ; harmless if the
// catalogue is empty (cache stays nil, FLAVOR column blanks).
func loadFlavorsCmd(client Client) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return flavorsLoadedMsg{err: errNoClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := client.ListFlavors(ctx, &weftv1.ListFlavorsRequest{})
		if err != nil || resp == nil {
			return flavorsLoadedMsg{err: err}
		}
		return flavorsLoadedMsg{flavors: resp.Flavors}
	}
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
		// Wire the AZ ↔ Hosts + Rack ↔ Hosts lookup so the detail
		// drawer surfaces the attached hosts (operator directive
		// 2026-06-23 "si les hosts sont rattaché a des az et des
		// racks, ils devraient aparaitre dans les panneau
		// correspondants"). The closure captures `m` (the outer
		// Model) so it sees the latest hosts list at lookup time.
		rm.relatedHosts = func(resourceID string, row map[string]any) []relatedHost {
			return m.hostsForResource(resourceID, row)
		}
		rm.relatedProjects = func(resourceID string, row map[string]any) []relatedProject {
			return m.projectsForResource(resourceID, row)
		}
		// Apply the current terminal size at construction so a
		// resource opened AFTER the initial WindowSizeMsg doesn't
		// render at the default 15-row × default-column-width
		// layout. The size handler above also iterates the map on
		// every resize so subsequent terminal changes propagate.
		if m.width > 0 {
			applyResize(&rm.table, cfg.Columns, m.bodyInnerWidth(), m.bodyHeight()-1)
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
		// Log pane width tracks the full terminal width ; resize it
		// FIRST so bodyHeight() (which subtracts logPane.height())
		// stays in sync.
		m.logPane.resize(m.width)
		// Apply any drag-set height override.
		if m.logPaneH > 0 {
			m.logPane.SetHeight(m.logPaneH)
		}
		// Reserve : status bar (2 lines) + 1 line breather + sidebar
		// occupies its own horizontal slot, so the body width must
		// exclude it. Match bodyHeight()/bodyWidth() so applyResize
		// + renderBody agree on the viewport.
		h := m.bodyHeight() - 1 // account for BodyBox border top+bottom
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

	case connSwitchMsg:
		// Endpoint just connected (or rotated). Render the active host
		// in the status bar ; clear any prior error/warn since the new
		// link works.
		name := msg.active.Name
		if name == "" {
			name = msg.active.Address
		}
		if name == "" {
			name = msg.active.Socket
		}
		m.conn = connStatus{level: ResilientEventInfo, msg: "● " + name}
		m.logPane.append(ResilientEventInfo, "connected to "+name)
		// Compute the identity rendered in the topbar's right side.
		// SSH endpoint → "user@host" ; local-socket → "local".
		switch {
		case msg.active.Address != "":
			user := msg.active.SSHUser
			if user == "" {
				// Strip any inline user@ prefix from Address so we
				// reach the host part for the display.
				user = "?"
			}
			host := msg.active.Address
			if at := strings.IndexByte(host, '@'); at >= 0 {
				if user == "?" {
					user = host[:at]
				}
				host = host[at+1:]
			}
			m.identity = user + "@" + host
		case msg.active.Socket != "":
			m.identity = "local"
		}
		return m, nil

	case connEventMsg:
		// Dial failure / failover-exhausted. Replace the success line
		// with the warning so the operator sees a degraded link.
		m.conn = connStatus{level: msg.level, msg: msg.msg}
		m.logPane.append(msg.level, msg.msg)
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

	case openResourceConfirmMsg:
		// Context menu picked a Confirm-gated action — flip the
		// target ResourceListModel into its 2-step confirm
		// prompt, mirroring the keyboard path in resources.go.
		if rm, ok := m.resource[msg.cfg]; ok {
			rm.confirmAction = msg.action
			rm.confirmInput = ""
			rm.confirmRow = msg.row
			m.resource[msg.cfg] = rm
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
			m.vms.applyVMs(msg.resp, m.hosts.placementByUUID, m.projects.tenantNameForProject, m.flavorLookup)
			// Push fresh per-host counts to the hosts model so the
			// "VMS" column reflects the new placement immediately —
			// no need to wait for the next hostsLoadedMsg.
			m.hosts.applyVMCounts(vmCountsByHost(m.vms.rows))
		}
		// Side-load flavors so the next applyVMs (refresh tick or
		// any tab switch) resolves the FLAVOR column via the cache.
		return m, loadFlavorsCmd(m.client)

	case flavorsLoadedMsg:
		if msg.err == nil && msg.flavors != nil {
			cache := make(map[flavorShapeKey]string, len(msg.flavors))
			for _, f := range msg.flavors {
				gib := uint64(ramToGiB(f.Ram))
				cache[flavorShapeKey{vcpu: uint32(f.Vcpu), memMb: gib * 1024}] = f.Name
			}
			m.flavorByShape = cache
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
			m.projects.applyProjects(msg.resp, msg.counts, msg.tenants)
			// Re-render the VMs tab so the new tenant lookup is
			// applied to existing rows (first projects fetch
			// arrives AFTER the first VMs fetch ; without this
			// the PROJECT column stays unprefixed until the next
			// refresh tick).
			m.vms.refreshProjectColumn(m.projects.tenantNameForProject)
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

	// --- Context menu : keyboard-triggered. ---
	// "m" on a table row opens the per-row action menu. Keyboard
	// is the PRIMARY trigger (terminals don't all forward
	// Ctrl+Shift+Click reliably — many strip the modifiers, some
	// fall back to right-click for paste). The mouse path in
	// handleMouse remains as a bonus where it works.
	// Operator directive 2026-06-24 : "Actions" button is bound to
	// `a` (was `m`). Lowercase 'a' previously triggered direct
	// Activate on VMs/AZ/Rack ; the menu intercept here now takes
	// priority, so Activate is reached via the menu (which lists
	// it explicitly).
	if !m.menu.open && !m.palette.open && key == "a" {
		items := m.buildContextMenu()
		if len(items) > 0 {
			m.menu.open = true
			m.menu.items = items
			m.menu.cursor = 0
			return m, nil
		}
	}

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
	case "ctrl+b":
		// Toggle the sidebar's collapsed (icon-only) mode. Useful
		// on small terminals where the full catalogue + labels eat
		// too much horizontal real estate. Mirrors VSCode's Ctrl+B
		// muscle memory.
		m.sidebarCollapsed = !m.sidebarCollapsed
		// Re-resize tables since bodyInnerWidth changed.
		h := m.bodyHeight() - 1
		bw := m.bodyInnerWidth()
		applyResize(&m.hosts.table, hostsColumns(), bw, h)
		applyResize(&m.vms.table, vmsColumns(), bw, h)
		applyResize(&m.projects.table, projectsColumns(), bw, h)
		for _, rm := range m.resource {
			applyResize(&rm.table, rm.cfg.Columns, bw, h)
		}
		return m, nil
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
	case "o":
		// "o" for remove — picks the 4th letter of "remove" so
		// the action-bar hint underlines a letter actually in
		// the label. Was "x" pre-2026-06-24.
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
	case "a":
		// Activate : flip admin status to active. Orthogonal to
		// runtime state — the VM may already be `running`.
		uuid := m.vms.selectedUUID()
		name, project := m.vms.selected()
		if uuid == "" {
			m.setError("no VM selected")
			return m, nil
		}
		return m, setVMStatusCmd(m.client, uuid, name, project, "active")
	case "i":
		// Inactivate : freeze the VM admin-side. Respawn skips,
		// scheduler avoids ; runtime keeps going until operator
		// explicitly stops.
		uuid := m.vms.selectedUUID()
		name, project := m.vms.selected()
		if uuid == "" {
			m.setError("no VM selected")
			return m, nil
		}
		return m, setVMStatusCmd(m.client, uuid, name, project, "inactive")
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
	topbar := m.renderTopbar()
	sidebar := m.renderSidebar()
	body := m.renderBody()
	// Resize the events viewport to fit the log-pane drawer ; the
	// vp was sized for the BODY by handleResize, which would
	// overflow + clip when the events tab is hosted in the log
	// pane (operator-reported 2026-06-24 "le panneau events ne
	// semble pas actif" — the LIVE header showed but lines were
	// clipped). Inner rows = log-pane vp height minus 1 for the
	// events sub-header.
	eventsBody := ""
	if m.logPane.activeTab == "events" {
		innerRows := m.logPane.vp.Height - 1
		if innerRows < 1 {
			innerRows = 1
		}
		m.events.vp.Width = m.bodyInnerWidth()
		m.events.vp.Height = innerRows
		m.events.vp.SetContent(strings.Join(m.events.lines, "\n"))
		if !m.events.userScrolled {
			m.events.vp.GotoBottom()
		}
		eventsBody = m.events.View(m.bodyInnerWidth())
	}
	logPaneView := m.logPane.View(m.theme, m.bodyWidth(), eventsBody)
	rightCol := lipgloss.JoinVertical(lipgloss.Left, body, logPaneView)
	main := lipgloss.JoinHorizontal(lipgloss.Top, sidebar, rightCol)
	// Inject the drag grips AFTER JoinHorizontal so both sides of
	// each boundary land on the same absolute Y / X — fixes the
	// "grips droite et gauche ne sont pas alignés" the operator
	// hit when each component computed its own middle.
	//
	// Frame junctions : the body lost its LEFT border + the log
	// pane lost its LEFT border so the sidebar's right border `│`
	// now carries the divider. Where the body's / log pane's
	// horizontal borders cross it, swap `╮ │ ╯` for T-junctions
	// (`┬ ├ ┴`) so the chrome reads as one frame with subdivisions.
	// Also patch the log pane's top-right corner (`╮` → `┤`) so
	// body's right border `│` flows down into the log pane's.
	sidebarRightX := m.sidebarWidth() - 1
	mainRightX := m.width - 1
	logTopY := m.bodyHeight()
	lastY := m.bodyHeight() + m.logPane.height() - 1
	main = injectFrameJunctions(main, sidebarRightX, logTopY, lastY, mainRightX)
	// Vertical drag (sidebar ↔ body) : single grip on the sidebar's
	// right border (now the shared divider — body's left border was
	// dropped) centered on the JOINED middle row. Length = 1/5 of
	// the joined height.
	if !m.sidebarCollapsed {
		main = injectDragGripAtJoinedMid(main, sidebarRightX)
	}
	// Horizontal drag (body ↔ log pane) : single grip on the shared
	// divider row. The body no longer carries its own bottom border
	// (collapsed into the log pane's top border) so the divider is a
	// single row at Y = bodyHeight() — the log pane's top border.
	bodyMidX := m.sidebarWidth() + m.bodyWidth()/2
	main = injectHorizontalDragGripAtY(main, m.bodyHeight(), bodyMidX)
	parts := []string{topbar, main}
	if m.palette.open {
		parts = append(parts, m.palette.View(m.theme, m.width))
	}
	parts = append(parts, m.renderStatusBar())
	base := strings.Join(parts, "\n")
	// Context menu : overlay near the selected row instead of
	// replacing the status bar. The previous "bottom strip"
	// rendering was a layout shortcut ; the operator's directive
	// 2026-06-23 : "le menu contextuel est a afficher en fenetre
	// overflow, pas en bas". Splice the menu lines into the base
	// at a computed (x, y) anchor so it visually floats over the
	// body table.
	if m.menu.open {
		base = m.overlayContextMenu(base)
	}
	return base
}

// topbarHeight returns how many terminal rows the topbar occupies.
// Drives mouse-coordinate translation : clicks deliver absolute
// terminal Y, but sidebarHitRows / menuHitRow / body-row-math all
// work in coordinates RELATIVE to their region. Every mouse Y must
// have topbarHeight() subtracted before being compared.
//
// Today the topbar is exactly 1 line ("weft  <cluster>      user@host"
// rendered with the Title style — no border yet). When we wrap it
// in a bordered box, this returns 3 (top border + content + bottom).
// Centralising the count here is the only thing that keeps the
// mouse handlers honest across that change — a regression test in
// app_test.go pins the contract.
func (m Model) topbarHeight() int {
	// renderTopbar produces a single styled line ; lipgloss.Height
	// of the rendered string is the authoritative count + survives
	// any future border-wrap refactor.
	return lipgloss.Height(m.renderTopbar())
}

// renderTopbar draws the 1-line bar that sits above the sidebar +
// body. Left side carries the product name + the cluster name (so
// the operator always knows which cluster they're poking at) ; right
// side surfaces the connection identity ("user@host" for SSH, "local"
// for a Unix-socket endpoint) so the operator can't accidentally act
// on the wrong cluster.
//
// Width-budget : exactly m.width cols. Trims left/right halves
// proportionally on narrow terminals.
func (m Model) renderTopbar() string {
	// lipgloss.Style.Width(N) sets the OUTER width including padding
	// but excluding border. TopbarBox = Border(1+1) + Padding(1+1) →
	// content area visible width = Width - 2 (the padding). For the
	// box's total output to match m.width, pass Width(m.width - 2)
	// (excludes border) ; the inner content (the bar) must then
	// target m.width - 4 visible cols (excludes border + padding).
	innerForLipgloss := m.width - 2
	if innerForLipgloss < 8 {
		innerForLipgloss = 8
	}
	contentWidth := m.width - 4
	if contentWidth < 6 {
		contentWidth = 6
	}

	leftPlain := "weft"
	if m.clusterName != "" {
		leftPlain += "  " + m.clusterName
	}
	// Refresh timestamp moves out of the status bar (operator
	// directive 2026-06-23, reiterated : "il y'a un affichage
	// 'refreshed ...' sous les frames, a deplacer dans la top
	// bar"). Format kept identical (`refreshed HH:MM:SS`) so the
	// operator's habit doesn't change.
	refreshedPlain := ""
	if ts := m.activeRefreshTS(); !ts.IsZero() {
		refreshedPlain = "refreshed " + ts.Format("15:04:05")
	}
	// Connection state dot : `●` coloured by the cp's health. Always
	// rendered (operator directive 2026-06-23 "affiche le point avec
	// pc en permanence avec une couleur"). 2 visible cols (`● `) so
	// the truncation budget accounts for it.
	connDotPlain := "● "
	rightPlain := m.identity
	// Compose the right cluster : [refreshed]  ●  [identity]. The
	// conn dot is always present (just 2 cols) so we account for it
	// here as part of the right-side width budget.
	rightCombinedPlain := refreshedPlain
	if rightCombinedPlain != "" {
		rightCombinedPlain += "  " + connDotPlain
	} else {
		rightCombinedPlain = connDotPlain
	}
	if rightPlain != "" {
		rightCombinedPlain += rightPlain
	}
	// Truncate left first if it would push right off the edge.
	maxLeft := contentWidth - len(rightCombinedPlain) - 1
	if maxLeft < len("weft") {
		maxLeft = len("weft")
	}
	if len(leftPlain) > maxLeft {
		if maxLeft > 1 {
			leftPlain = leftPlain[:maxLeft-1] + "…"
		} else {
			leftPlain = leftPlain[:maxLeft]
		}
	}
	// Truncate the combined right side if it still doesn't fit. We
	// drop the refresh portion FIRST (less critical than identity).
	rightBudget := contentWidth - len(leftPlain) - 1
	if rightBudget < 0 {
		rightBudget = 0
	}
	if len(rightCombinedPlain) > rightBudget {
		// Try without refreshed first.
		if refreshedPlain != "" && len(rightPlain) <= rightBudget {
			refreshedPlain = ""
			rightCombinedPlain = rightPlain
		} else if rightBudget > 1 {
			rightCombinedPlain = "…" + rightCombinedPlain[len(rightCombinedPlain)-(rightBudget-1):]
		} else {
			rightCombinedPlain = rightCombinedPlain[:rightBudget]
		}
	}

	left := m.theme.Title.Render("weft")
	if m.clusterName != "" {
		cluster := strings.TrimPrefix(leftPlain, "weft")
		cluster = strings.TrimLeft(cluster, " ")
		if cluster != "" {
			left += "  " + m.theme.StatusVal.Render(cluster)
		}
	}
	right := ""
	if refreshedPlain != "" {
		right = m.theme.StatusVal.Render(refreshedPlain)
	}
	// Connection dot styled by level — green-ish (BadgeOK) when
	// Info, amber (BadgeWarn) on warn, red (BadgeBad) on error or
	// when there is no event yet (so the operator sees a hint that
	// the link hasn't come up). Bold so the colour reads through.
	var dotStyle = m.theme.BadgeBad
	switch m.conn.level {
	case ResilientEventInfo:
		dotStyle = m.theme.BadgeOK
	case ResilientEventWarn:
		dotStyle = m.theme.BadgeWarn
	case ResilientEventError:
		dotStyle = m.theme.BadgeBad
	}
	if right != "" {
		right += "  "
	}
	right += dotStyle.Render("●") + " "
	if rightPlain != "" {
		right += m.theme.Faint.Render(rightPlain)
	}

	used := lipgloss.Width(left) + lipgloss.Width(right)
	pad := contentWidth - used
	if pad < 0 {
		pad = 0
	}
	bar := left + strings.Repeat(" ", pad) + right
	// Drop the topbar's BOTTOM border so it doesn't stack on top of
	// the main region's TOP border (= sidebar.top + body.top) —
	// same trick as body↔log pane + sidebar↔body. injectFrameJunctions
	// patches the corners where topbar's left/right `│` meets main's
	// top `─` (`╭`→`├` and `╮`→`┤`) so the chrome reads as one frame.
	return m.theme.TopbarBox.BorderBottom(false).Width(innerForLipgloss).Render(bar)
}

// sidebarWidth is the fixed horizontal slot the sidebar occupies,
// including border + padding. Tuned so the longest catalogue entry
// ("Scheduling Rules" / "Installed Plugins" / "Availability Zones"
// / "SSH Keys (catalogue)" — all ~17-20 chars) fits with its
// shortcut prefix + the active "▸" marker. 28 col leaves a body
// width of 52 col on an 80-col terminal — still readable.
// defaultSidebarWidth is the initial sidebar slot. The operator can
// drag the right-edge column to resize ; the new value lives on
// Model.sidebarW. Floor at minSidebarWidth so labels stay legible ;
// cap at maxSidebarWidth so the body region keeps a usable budget.
const (
	defaultSidebarWidth   = 28
	minSidebarWidth       = 16
	maxSidebarWidth       = 60
	collapsedSidebarWidth = 5 // icon-only mode : border + 1 char + border + padding
)

// sidebarWidth returns the current sidebar width, honoring the
// operator's drag-resize when set or falling back to the default.
// When sidebarCollapsed is true, returns the narrow icon-only width
// regardless of any drag-set value (drag is disabled in collapsed
// mode).
func (m Model) sidebarWidth() int {
	if m.sidebarCollapsed {
		return collapsedSidebarWidth
	}
	if m.sidebarW > 0 {
		return m.sidebarW
	}
	return defaultSidebarWidth
}

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
// reorderEntries applies a per-section preferred ID order, with
// unknown IDs preserved at the tail in their original catalogue
// position. Only sections present in `entryOrder` are reshuffled.
func reorderEntries(in []ResourceConfig, section string) []ResourceConfig {
	entryOrder := map[string][]string{
		"Storage": {"images", "volumes", "shares", "buckets", "volume-snapshots"},
	}
	pref, ok := entryOrder[section]
	if !ok || len(in) == 0 {
		return in
	}
	pos := map[string]int{}
	for i, id := range pref {
		pos[id] = i
	}
	known := make([]ResourceConfig, 0, len(in))
	unknown := make([]ResourceConfig, 0, len(in))
	for _, r := range in {
		if _, ok := pos[r.ID]; ok {
			known = append(known, r)
		} else {
			unknown = append(unknown, r)
		}
	}
	sort.SliceStable(known, func(i, j int) bool {
		return pos[known[i].ID] < pos[known[j].ID]
	})
	return append(known, unknown...)
}

// title within each group). Sections list section.* of the
// catalogue in the same order operators expect : Network, Storage,
// Compute, Identity, Admin.
func sidebarSections() []sidebarSection {
	sections := []sidebarSection{{
		Header:  "Core",
		Entries: coreEntries(),
	}}
	// Drop the Core section header when all its core entries got
	// relocated elsewhere (Hosts → admin, VMs → compute, Projects
	// → identity, Events → log-pane tabbar). Leaves the sidebar
	// starting on the first real section.
	if len(sections) == 1 && len(sections[0].Entries) == 0 {
		sections = nil
	}
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
		entries := make([]sidebarEntry, 0, len(groups[sec])+2)
		// VMs belongs to the COMPUTE section. First entry.
		if strings.ToLower(sec) == "compute" {
			entries = append(entries, sidebarEntry{
				tab:      tabVMs,
				label:    "VMs",
				shortcut: "2",
			})
		}
		// Per-section preferred entry order. Today only Storage
		// needs it ("images" above "volumes" per 2026-06-23 operator
		// directive — base templates read before the snapshots
		// they spawn). Unknown IDs land at the end in catalogue
		// order, same as the section ordering above.
		groups[sec] = reorderEntries(groups[sec], sec)
		for _, r := range groups[sec] {
			entries = append(entries, sidebarEntry{
				resourceID: r.ID,
				label:      r.Title,
				shortcut:   "·",
			})
			// Hosts belongs under Racks (AZ → Rack → Host hierarchy).
			// Insert inline right after the Racks entry. Shortcut
			// "1" still binds to tabHosts.
			if strings.ToLower(sec) == "admin" && r.ID == "racks" {
				entries = append(entries, sidebarEntry{
					tab:      tabHosts,
					label:    "Hosts",
					shortcut: "1",
				})
			}
			// Projects belongs under Tenants (Tenant → Project
			// hierarchy) — operator directive 2026-06-23 "les
			// projets devraient etre classé sous les tenants".
			// Insert inline right after the Tenants entry, in the
			// identity section. Shortcut "3" still binds to it.
			if strings.ToLower(sec) == "identity" && r.ID == "tenants" {
				entries = append(entries, sidebarEntry{
					tab:      tabProjects,
					label:    "Projects",
					shortcut: "3",
				})
			}
		}
		sections = append(sections, sidebarSection{
			// Section headers keep the catalogue's CamelCase
			// (Network / Storage / Compute / Identity / Admin)
			// per the 2026-06-23 directive : "met une lettre
			// majuscule aux catégories dans la sidebar".
			Header:  sec,
			Entries: entries,
		})
	}
	sections = append(sections, sidebarSection{
		Header: "More",
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
	out := make([]sidebarEntry, 0, len(tabLabels))
	for i, label := range tabLabels {
		// Hosts → admin (under Racks) ; VMs → compute (first) ;
		// Projects → identity (under Tenants) ; Events → log pane
		// tabbar (operator 2026-06-24). Keyboard shortcuts
		// 1/2/3/4 still bind to their respective tabs even though
		// they're no longer in the `core` section.
		if tab(i) == tabHosts || tab(i) == tabVMs || tab(i) == tabProjects || tab(i) == tabEvents {
			continue
		}
		out = append(out, sidebarEntry{
			tab:      tab(i),
			label:    label,
			shortcut: fmt.Sprintf("%d", i+1),
		})
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
	// Build the full sidebar content (every section + every entry)
	// into a slice of lines first, then slice by sidebarOffset
	// before passing it to lipgloss.
	allLines := make([]string, 0, 40)
	for _, sec := range sidebarSections() {
		if m.sidebarCollapsed {
			allLines = append(allLines, "")
		} else {
			allLines = append(allLines, m.theme.SidebarSection.Render(sec.Header))
		}
		for _, e := range sec.Entries {
			active := m.isSidebarEntryActive(e)
			if m.sidebarCollapsed {
				allLines = append(allLines, sidebarRowCollapsed(m.theme, e.shortcut, active))
			} else {
				allLines = append(allLines, sidebarRow(m.theme, e.shortcut, e.label, active))
			}
		}
	}

	// Available content rows inside the SidebarBox : the box's
	// total Height is sidebarInnerHeight, minus 2 for vertical
	// padding (Padding(1,1)) — that's how many rows of payload
	// fit before lipgloss starts clipping.
	visibleRows := m.sidebarInnerHeight() - 2
	if visibleRows < 1 {
		visibleRows = 1
	}

	offset := m.sidebarOffset
	if offset < 0 {
		offset = 0
	}
	maxOffset := len(allLines) - visibleRows
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	end := offset + visibleRows
	if end > len(allLines) {
		end = len(allLines)
	}
	window := allLines[offset:end]

	// Scroll indicators : "↑ N more" / "↓ N more" rendered on
	// the first/last row when content is clipped. Tells the
	// operator there's catalogue beyond the visible window —
	// without this the report "AZ and Racks still missing" would
	// look like a real bug instead of a viewport overflow.
	if offset > 0 && len(window) > 0 {
		window[0] = m.theme.Faint.Render(fmt.Sprintf("↑ %d more", offset))
	}
	if offset < maxOffset && len(window) > 0 {
		window[len(window)-1] = m.theme.Faint.Render(fmt.Sprintf("↓ %d more", maxOffset-offset))
	}

	var b strings.Builder
	for i, l := range window {
		b.WriteString(l)
		if i < len(window)-1 {
			b.WriteString("\n")
		}
	}
	// MaxHeight clips overflow ; Height alone only PADS UP (lipgloss
	// quirk verified by probe). Without MaxHeight, a sidebar whose
	// catalogue + section headers exceed bodyHeight would render
	// at full content size + push the terminal to scroll, losing
	// the top of the frame (the operator-reported regression).
	// MaxHeight includes the border ; +2 to leave room for the
	// rounded top/bottom.
	// The drag grip is injected by View() AFTER JoinHorizontal so
	// the sidebar's right grip + body's left grip align on the
	// same absolute Y. See injectDragGripAtJoinedMid.
	return m.theme.SidebarBox.
		Width(m.sidebarWidth() - 2).
		Height(m.sidebarInnerHeight()).
		MaxHeight(m.sidebarInnerHeight() + 2).
		Render(b.String())
}

// injectDragGripAtJoinedMid overwrites column x of the middle
// ROWS in `rendered` with the heavy `┃` glyph. Grip length = 1/5
// of the rendered height (centered) — scales with terminal size.
// Floored at 3 rows.
//
// MUST be called AFTER JoinHorizontal so both grips of a shared
// boundary (sidebar right col + body left col) land on the same
// absolute Y range — the operator's "grips droite et gauche ne
// sont pas alignés" was caused by each component computing its
// own midpoint pre-join.
func injectDragGripAtJoinedMid(rendered string, x int) string {
	lines := strings.Split(rendered, "\n")
	if len(lines) < 5 {
		return rendered
	}
	gripLen := len(lines) / 5
	if gripLen < 3 {
		gripLen = 3
	}
	half := gripLen / 2
	mid := len(lines) / 2
	for offset := -half; offset <= half; offset++ {
		y := mid + offset
		if y < 0 || y >= len(lines) {
			continue
		}
		lines[y] = overwriteRuneAt(lines[y], x, '┃')
	}
	return strings.Join(lines, "\n")
}

// injectFrameJunctions makes the sidebar's right border + log
// pane's top-right corner read as T-junctions where horizontal
// borders cross — otherwise the body's and log pane's top borders
// look like floating fragments next to the sidebar's closed
// rounded box. Sites :
//
//	(0,             0)         `╭` → `├`   topbar's left `│` continues down
//	(sidebarRightX, 0)         `╮` → `┬`   body top horizontal joins
//	(sidebarRightX, logTopY)   `│` → `├`   log pane top horizontal
//	(sidebarRightX, lastY)     `╯` → `┴`   log pane bottom horizontal
//	(mainRightX,    0)         `╮` → `┤`   topbar's right `│` continues down
//	(mainRightX,   logTopY)    `╮` → `┤`   body right vertical joins
//
// Caller passes the rendered "main" region (sidebar+body+log pane
// already joined). Coordinates are within main, NOT the full View.
//
// Junction glyphs are square ; the surrounding frame uses rounded
// corners. The mix is intentional — there is no rounded variant of
// `┬ ┴ ├ ┤` in Unicode, and at terminal-font sizes the difference
// is imperceptible.
func injectFrameJunctions(rendered string, sidebarRightX, logTopY, lastY, mainRightX int) string {
	lines := strings.Split(rendered, "\n")
	put := func(y, x int, swap rune) {
		if y < 0 || y >= len(lines) {
			return
		}
		lines[y] = overwriteRuneAt(lines[y], x, swap)
	}
	put(0, 0, '├')
	put(0, sidebarRightX, '┬')
	put(0, mainRightX, '┤')
	put(logTopY, sidebarRightX, '├')
	put(lastY, sidebarRightX, '┴')
	put(logTopY, mainRightX, '┤')
	return strings.Join(lines, "\n")
}

// injectHorizontalDragGripAtY overwrites a horizontal span of the
// row at line index y in `rendered` with the heavy `━` glyph.
// Grip width = 1/5 of the line's visible width (centered on midX),
// floored at 3 cols. Scales with terminal width.
func injectHorizontalDragGripAtY(rendered string, y, midX int) string {
	lines := strings.Split(rendered, "\n")
	if y < 0 || y >= len(lines) {
		return rendered
	}
	visibleW := len([]rune(stripANSI(lines[y])))
	gripLen := visibleW / 5
	if gripLen < 3 {
		gripLen = 3
	}
	half := gripLen / 2
	for offset := -half; offset <= half; offset++ {
		lines[y] = overwriteRuneAt(lines[y], midX+offset, '━')
	}
	return strings.Join(lines, "\n")
}

// overwriteRuneAt swaps the rune at visible column x of line for
// the given rune. ANSI escape sequences are copied verbatim ;
// multi-byte UTF-8 runes are walked safely.
func overwriteRuneAt(line string, x int, swap rune) string {
	plain := stripANSI(line)
	plainRunes := []rune(plain)
	if x < 0 || x >= len(plainRunes) {
		return line
	}
	var b strings.Builder
	visible := 0
	for i := 0; i < len(line); {
		if line[i] == '\x1b' && i+1 < len(line) && line[i+1] == '[' {
			b.WriteByte(line[i])
			b.WriteByte(line[i+1])
			i += 2
			for i < len(line) {
				c := line[i]
				b.WriteByte(c)
				i++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			continue
		}
		r, sz := utf8DecodeRune(line[i:])
		if visible == x {
			b.WriteRune(swap)
		} else {
			b.WriteRune(r)
		}
		visible++
		i += sz
	}
	return b.String()
}

// utf8DecodeRune is a thin wrapper over utf8.DecodeRuneInString that
// keeps the call site terse + lets us swap in test seams later.
func utf8DecodeRune(s string) (rune, int) {
	return utf8.DecodeRuneInString(s)
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
// sidebarRow renders one entry. Numeric shortcuts (core entries
// "1"/"2"/"3"/"4") render as a bullet `·` like the catalogue
// entries — all sidebar rows share the same visual class. The
// 1..4 keyboard shortcuts still work, they're just not displayed.
//
// Active rows REPLACE the bullet with `▸` so the label X position
// stays identical between active + inactive states (the operator
// directive 2026-06-23 : "ca evite de deplacer l'item vers la
// droite").
func sidebarRow(theme Theme, shortcut, label string, active bool) string {
	bullet := shortcut + " "
	if isNumericShortcut(shortcut) {
		bullet = "· "
	} else if shortcut == "" {
		bullet = "  "
	}
	if active {
		return theme.SidebarItemActive.Render("▸ " + label)
	}
	return theme.SidebarItem.Render(bullet + label)
}

// sidebarRowCollapsed renders a 1-col icon-only row used when the
// sidebar is in collapsed mode (Ctrl+B toggle). Just the shortcut
// glyph ; active rows pick up the active colour so the operator
// still knows which view they're on. Numeric shortcuts stay visible
// here because they ARE the icon — without them the collapsed row
// would be empty.
func sidebarRowCollapsed(theme Theme, shortcut string, active bool) string {
	if active {
		return theme.SidebarItemActive.Render(shortcut)
	}
	return theme.SidebarItem.Render(shortcut)
}

// isNumericShortcut reports whether the shortcut is a single ASCII
// digit ("1".."9"). Drives the hide-numbering logic in sidebarRow.
func isNumericShortcut(s string) bool {
	return len(s) == 1 && s[0] >= '0' && s[0] <= '9'
}

// bodyHeight is the TOTAL rendered height of the body region —
// including its rounded BodyBox border. Despite the name "inner"
// historically, renderBody passes Height(bodyHeight() - 1) into
// BodyBox so the output (border + content) ends up bodyHeight()
// lines exactly. Same convention for downstream sites.
//
// View() total = topbarHeight + sidebar/body row + statusBar (2).
// sidebar/body row = max(sidebar rendered, right column rendered).
// right column = bodyHeight() + logPane.height() (both already
// include their own borders).
//
// Solving total == m.height :
//   topbarHeight + bodyHeight + logPane.height + 2 == m.height
//   bodyHeight = m.height - 2 - logPane.height() - topbarHeight()
//
// topbarHeight() reads renderTopbar()'s line count at runtime so any
// future change to the topbar's framing automatically reflows the
// math.
//
// Floor at 3 so the body always has at least 1 content row when
// the operator drags the divider almost all the way up — the user
// wants the horizontal drawer to be able to rise up to (just under)
// the main panel's content.
//
// Chrome budget : topbarHeight() + bodyHeight + logPane.height() + 1
// (status bar = single line, no border anymore) == m.height.
func (m Model) bodyHeight() int {
	h := m.height - 1 - m.logPane.height() - m.topbarHeight()
	if h < 3 {
		h = 3
	}
	return h
}

// sidebarInnerHeight returns the Height the sidebar wants so its
// TOTAL rendered output equals the right column (body+logpane).
// Right column = bodyHeight + logPane.height (both already include
// their borders). Sidebar rendered = sidebarInner + 2 (rounded
// border). Solving for parity :
//
//   sidebarInner + 2 = bodyHeight + logPane.height
//   sidebarInner    = bodyHeight + logPane.height - 2
func (m Model) sidebarInnerHeight() int {
	h := m.bodyHeight() + m.logPane.height() - 2
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
	// Action toolbar at the top of the body : faint key-hint
	// strip listing the action keys available on the current view.
	// Followed by a faint horizontal rule separating it from the
	// table's header — operator directive 2026-06-24 "met un
	// frame pour separrer la barre d'action du tableau".
	if bar := m.renderActionBar(inner); bar != "" {
		sep := m.theme.Faint.Render(strings.Repeat("─", inner))
		content = bar + "\n" + sep + "\n" + content
	}
	// MaxHeight defensive cap : the table widget normally honours its
	// applyResize'd height, but the create-form / detail-drawer
	// overlays can exceed it. Without MaxHeight an overflow pushes
	// the layout down + cuts the top of the sidebar's frame.
	// Drop the body's BOTTOM border (so it doesn't stack on top of
	// the log pane's TOP border) AND its LEFT border (so it doesn't
	// stack against the sidebar's RIGHT border) — same operator
	// request, applied symmetrically to both drawers : the divider
	// reads as a single line on each side, and the body reclaims
	// 1 row + 1 column of usable space. Height = bodyHeight - 1 (only
	// top border kept) ; Width = bodyWidth - 1 (only right border
	// kept) — totals stay (bodyWidth × bodyHeight).
	// Drop horizontal padding so the action bar (and its separator
	// rule line) reaches the body's right border edge-to-edge.
	// Operator directive 2026-06-24 "joint le frame de la tab bar
	// a droite et a gauche. idem pour l'icon/button barre".
	return m.theme.BodyBox.
		BorderBottom(false).
		BorderLeft(false).
		PaddingLeft(0).
		PaddingRight(0).
		Width(m.bodyWidth() - 1).
		Height(m.bodyHeight() - 1).
		MaxHeight(m.bodyHeight()).
		Render(content)
}

// bodyInnerWidth is what the body content actually has to draw on,
// once the BodyBox border (1 col — only right is kept) is
// subtracted from bodyWidth. Horizontal padding was dropped so the
// action bar reaches edge-to-edge (operator 2026-06-24).
func (m Model) bodyInnerWidth() int {
	w := m.bodyWidth() - 1
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
	w := m.width - m.sidebarWidth()
	if w < 20 {
		w = 20
	}
	return w
}

// overlayContextMenu splices the rendered context menu into base
// at a computed anchor near the selected row. Returns base
// unchanged when the menu is closed or empty.
//
// Anchor :
//   - X : just past the sidebar + 4 cols of body padding so the menu
//     sits inside the body region but doesn't cover its leftmost
//     content (UUID / NAME column).
//   - Y : terminal Y of the active table's selected row + 1 (so the
//     menu drops "below" the cursor like a desktop context menu).
//     When the row is near the bottom of the body, the menu flips
//     upward so it stays visible.
//
// Implementation : the base View is a string of lines ; the menu is
// also a multi-line string. We split both, overlay the menu lines
// into the base lines at (anchorX, anchorY) by character position
// — keeping the ANSI styles intact via stripANSI math for the
// length count, then write the menu bytes verbatim.
func (m Model) overlayContextMenu(base string) string {
	if !m.menu.open || len(m.menu.items) == 0 {
		return base
	}
	menu := m.renderContextMenuFloating()
	if menu == "" {
		return base
	}
	menuLines := strings.Split(menu, "\n")
	baseLines := strings.Split(base, "\n")
	// Anchor : selected-row Y + 1, or flip upward if too close to
	// the bottom of the body region.
	tbl := (&m).activeTable()
	rowIdx := 0
	if tbl != nil {
		rowIdx = tbl.Cursor()
	}
	// Body region starts at terminal Y = topbarHeight() ; the table
	// header consumes 2 rows (top border + header line), so the
	// first data row sits at topbarHeight + 2.
	bodyTop := m.topbarHeight()
	anchorY := bodyTop + 2 + rowIdx + 1 // just below the selected row
	menuHeight := len(menuLines)
	bodyBottom := bodyTop + m.bodyHeight()
	if anchorY+menuHeight > bodyBottom {
		// Flip upward so the menu fits.
		anchorY = bodyTop + 2 + rowIdx - menuHeight
		if anchorY < bodyTop+1 {
			anchorY = bodyTop + 1
		}
	}
	// X anchor inside the body region. sidebarWidth + 4 puts the
	// menu past the BodyBox border + 1 col of padding.
	anchorX := m.sidebarWidth() + 4

	for i, line := range menuLines {
		y := anchorY + i
		if y < 0 || y >= len(baseLines) {
			continue
		}
		baseLines[y] = overlayLineAt(baseLines[y], line, anchorX, m.width)
	}
	return strings.Join(baseLines, "\n")
}

// renderContextMenuFloating produces a small, self-contained menu
// box — same content as the legacy renderContextMenu but sized as a
// floating window rather than a full-width strip. Uses BodyBox's
// rounded border so the overlay reads as a "panel" rather than a
// banner. Width is computed from actual content + chrome so labels
// + shortcuts always fit cleanly (no cropped text).
func (m Model) renderContextMenuFloating() string {
	if !m.menu.open || len(m.menu.items) == 0 {
		return ""
	}
	// Visible width of one row = label + "  [" (3) + shortcut + "]"
	// (1) = label + shortcut + 4. Constant chrome ; no marker on the
	// active row (colour conveys selection).
	contentWidth := 0
	for _, it := range m.menu.items {
		l := lipgloss.Width(it.label) + lipgloss.Width(it.shortcut) + 4
		if l > contentWidth {
			contentWidth = l
		}
	}
	footer := "↑↓ select · ↵ run · Esc close"
	if w := lipgloss.Width(footer); w > contentWidth {
		contentWidth = w
	}
	// Floor : 20 cols so the box never collapses on a 1-item menu
	// with a short label.
	if contentWidth < 20 {
		contentWidth = 20
	}

	var b strings.Builder
	for i, it := range m.menu.items {
		// No indentation marker : the active row is distinguished
		// by colour alone (per the operator's directive 2026-06-23
		// "le changement de couleur suffit a avoir ou on est").
		// Each row is padded to contentWidth so the colour highlight
		// of the active row covers the full box width.
		row := it.label + "  [" + it.shortcut + "]"
		if w := lipgloss.Width(row); w < contentWidth {
			row += strings.Repeat(" ", contentWidth-w)
		}
		if i == m.menu.cursor {
			b.WriteString(m.theme.SidebarItemActive.Render(row))
		} else {
			b.WriteString(m.theme.SidebarItem.Render(row))
		}
		b.WriteString("\n")
	}
	b.WriteString(m.theme.Faint.Render(footer))
	// BodyBox adds Padding(0, 1) → +2 cols outer ; Border → +2.
	// Pass Width that includes the padding (lipgloss convention) so
	// the rendered output is contentWidth + 4 cols total.
	return m.theme.BodyBox.Width(contentWidth + 2).Render(b.String())
}

// overlayLineAt writes overlay over base starting at column anchorX.
//
// Robust algorithm — the previous splice tried to keep base's ANSI
// styles around the menu, but the math drifted under accumulated
// CSI sequences and the menu ended up shifted horizontally (the
// operator-reported regression "ne va toujours pas pour le menu
// contextuel, il doit etre afficher au dessus du reste sans
// decallage"). This version trades style fidelity on the 6 overlaid
// lines for ROCK-SOLID alignment :
//
//   1. Strip ANSI from base → plain text.
//   2. Pad plain to `width` so anchorX + overlayWidth always fits.
//   3. Re-render the line as : plain[0:anchorX] + overlay +
//      plain[anchorX+overlayWidth:].
//
// The 6 lines under the menu lose their original colors (they
// re-render as plain text + the menu's styled box on top). Operators
// see solid menu placement instead of a kaleidoscope of mis-aligned
// columns ; the original styles return as soon as the menu closes.
func overlayLineAt(base, overlay string, anchorX, width int) string {
	plain := stripANSI(base)
	overlayPlain := stripANSI(overlay)
	ow := len([]rune(overlayPlain))

	// Pad plain base to at least width cols so the slicing math
	// below is always valid.
	plainRunes := []rune(plain)
	if len(plainRunes) < width {
		plainRunes = append(plainRunes, []rune(strings.Repeat(" ", width-len(plainRunes)))...)
	}

	if anchorX < 0 {
		anchorX = 0
	}
	if anchorX > len(plainRunes) {
		anchorX = len(plainRunes)
	}
	end := anchorX + ow
	if end > len(plainRunes) {
		end = len(plainRunes)
	}
	prefix := string(plainRunes[:anchorX])
	suffix := string(plainRunes[end:])

	out := prefix + overlay + suffix
	// Cap to width — strip any padding past the terminal's right
	// edge. Use rune count for the cap on the plain side.
	plainOut := stripANSI(out)
	if width > 0 && len([]rune(plainOut)) > width {
		// Re-walk to truncate while keeping ANSI codes from overlay
		// (they're in the middle ; the trailing plain runes go
		// last). Simpler : just rune-slice the plain output and
		// re-inject the overlay block as-is.
		plainOutRunes := []rune(plainOut)
		plainOutRunes = plainOutRunes[:width]
		// Plain truncated output ; the styled overlay is inside it
		// already since we built `out` with overlay verbatim.
		// Re-construct : take the styled prefix bytes up to anchorX
		// (plain), then overlay, then trimmed suffix.
		newSuffixLen := width - anchorX - ow
		if newSuffixLen < 0 {
			newSuffixLen = 0
		}
		trimSuffix := ""
		if len([]rune(suffix)) > newSuffixLen {
			trimSuffix = string([]rune(suffix)[:newSuffixLen])
		} else {
			trimSuffix = suffix
		}
		out = prefix + overlay + trimSuffix
		_ = plainOutRunes
	}
	return out
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
		items = append(items, contextMenuItem{label: "Remove…", shortcut: "o",
			action: openHostConfirmRemoveCmd(uuid, host)})
		return items
	case tabVMs:
		name, project := m.vms.selected()
		uuid := m.vms.selectedUUID()
		if name == "" {
			return nil
		}
		return []contextMenuItem{
			{label: "Start", shortcut: "s", action: startVMCmd(m.client, name, project)},
			{label: "Restart", shortcut: "R", action: restartVMCmd(m.client, name, project)},
			{label: "Stop…", shortcut: "S", action: openVMConfirmStopCmd(name, project)},
			{label: "Activate", shortcut: "a", action: setVMStatusCmd(m.client, uuid, name, project, "active")},
			{label: "Inactivate", shortcut: "i", action: setVMStatusCmd(m.client, uuid, name, project, "inactive")},
			{label: "Logs", shortcut: "l", action: loadVMLogsCmd(m.client, name, project)},
		}
	case tabResource:
		// Uniformise : every resource panel surfaces its catalogue
		// Actions in the context menu. Operator directive
		// 2026-06-24 "uniformise la tui pour avoir le menu
		// contextuel actif partout".
		rm, ok := m.resource[m.currentResource]
		if !ok {
			return nil
		}
		row := rm.selected()
		if row == nil {
			return nil
		}
		items := make([]contextMenuItem, 0, len(rm.cfg.Actions))
		for _, a := range rm.cfg.Actions {
			items = append(items, contextMenuItem{
				label:    actionLabel(a),
				shortcut: a.Key,
				action:   resourceActionCmd(rm.client, rm.cfg, a, row),
			})
		}
		return items
	}
	return nil
}

// actionLabel renders the menu entry text — adds "…" when the
// action requires a confirmation step so the operator sees the
// 2-step contract at a glance.
func actionLabel(a ResourceAction) string {
	if a.Confirm != "" {
		return strings.Title(a.Label) + "…"
	}
	return strings.Title(a.Label)
}

// resourceActionCmd wraps a catalogue ResourceAction into a tea.Cmd
// that runs the Do closure with a 20s context + emits the same
// resourceActionMsg the ResourceListModel's keyboard path uses.
// Confirm-gated actions go through openResourceConfirmCmd instead
// so the existing 2-step UX kicks in.
func resourceActionCmd(client weftv1.WeftAgentClient, cfg ResourceConfig, a ResourceAction, row map[string]any) tea.Cmd {
	if a.Confirm != "" {
		return openResourceConfirmCmd(cfg.ID, a.Key, row)
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		mm, err := a.Do(ctx, client, row)
		return resourceActionMsg{cfg: cfg.ID, action: a.Key, row: row, msg: mm, err: err}
	}
}

// openResourceConfirmMsg flips ResourceListModel into its confirm
// prompt for the named action / row — used by the context menu when
// the picked action is Confirm-gated.
type openResourceConfirmMsg struct {
	cfg    string
	action string
	row    map[string]any
}

func openResourceConfirmCmd(cfg, action string, row map[string]any) tea.Cmd {
	return func() tea.Msg { return openResourceConfirmMsg{cfg: cfg, action: action, row: row} }
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
			// Needle mirrors sidebarRow : inactive rows render as
			// "· <label>" (or "<shortcut> <label>" for catalogue) ;
			// active rows render as "▸ <label>" (bullet replaced,
			// not prepended). Search for the LABEL substring — it
			// appears in both forms, no need to branch on active
			// state.
			var needle string
			switch {
			case m.sidebarCollapsed:
				needle = e.shortcut
			default:
				needle = e.label
			}
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
	// Wheel events inside the log pane scroll the pane's viewport
	// instead of the table. The pane sits at the bottom : its top
	// row = (bodyHeight + 1 sidebar/body row gap). Detect by Y range.
	// Log pane bounds in ABSOLUTE terminal Y : topbar first, then
	// body (bodyHeight + 2 lines for BodyBox border = body region
	// total), then the log pane stacked under the body in the right
	// column. Forgetting topbarHeight() here misses wheel events
	// on the pane on small terminals.
	topOfPane := m.topbarHeight() + m.bodyHeight()
	bottomOfPane := topOfPane + m.logPane.height() - 1
	inPane := msg.Y >= topOfPane && msg.Y <= bottomOfPane
	// Tab-strip rectangle inside the pane : 3 lines for the tab
	// boxes, starting 1 line below the pane's top border. X 0..N
	// relative to the pane's left edge (which is sidebarWidth()).
	tabStripTop := topOfPane + 1
	tabStripBottom := tabStripTop + 2 // 3 lines : top border, label, bottom border
	inTabStrip := msg.Y >= tabStripTop && msg.Y <= tabStripBottom
	// Sidebar wheel handling : when the pointer is over the
	// sidebar (X < sidebarWidth, Y inside the body row), the wheel
	// scrolls the sidebar offset so entries past the visible
	// bottom (AZs, Racks, Hosts, Plugins on a short terminal)
	// stay reachable. Operator directive 2026-06-23 "dans la TUI
	// il manque toujours les AZ et les racks".
	inSidebar := !m.sidebarCollapsed && msg.X < m.sidebarWidth()-1 && msg.Y >= m.topbarHeight() && msg.Y < m.topbarHeight()+m.bodyHeight()
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if inSidebar {
			if m.sidebarOffset > 0 {
				m.sidebarOffset--
			}
			return m, nil
		}
		if inPane {
			m.logPane.vp.LineUp(2)
			m.logPane.follow = false
			return m, nil
		}
		m.scrollActiveTable(-1)
		return m, nil
	case tea.MouseButtonWheelDown:
		if inSidebar {
			m.sidebarOffset++
			return m, nil
		}
		if inPane {
			m.logPane.vp.LineDown(2)
			if m.logPane.vp.AtBottom() {
				m.logPane.follow = true
			}
			return m, nil
		}
		m.scrollActiveTable(1)
		return m, nil
	case tea.MouseButtonRight:
		// Right-click context menu. macOS touchpad maps the two-
		// finger tap to a right-click — the natural gesture for a
		// context menu. Terminals that bind right-click to paste
		// will conflict, but the keyboard `m` shortcut + the
		// Ctrl+Shift+Left fallback below cover that case.
		if msg.Action != tea.MouseActionRelease {
			return m, nil
		}
		if msg.X < m.sidebarWidth() {
			m.menu.open = false
			return m, nil
		}
		row := msg.Y - m.topbarHeight() - 2
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
		// Action bar click → "Actions" or "Refresh" button.
		// Bar Y inside body = top border (1). Absolute Y = topbar + 1.
		// Bar layout : "Actions" (7 cols) + "  " (2) + "Refresh" (7).
		if msg.Action == tea.MouseActionRelease && msg.X >= m.sidebarWidth() {
			actionBarY := m.topbarHeight() + 1
			if msg.Y == actionBarY {
				localX := msg.X - m.sidebarWidth()
				const actionsLen = 7   // "Actions"
				const sepLen = 2       // "  "
				const refreshLen = 7   // "Refresh"
				switch {
				case localX >= 0 && localX < actionsLen:
					// Open the context menu — same effect as `a`.
					items := m.buildContextMenu()
					if len(items) > 0 {
						m.menu.open = true
						m.menu.items = items
						m.menu.cursor = 0
					}
					return m, nil
				case localX >= actionsLen+sepLen && localX < actionsLen+sepLen+refreshLen:
					// Refresh the active view — same effect as `r`.
					switch m.active {
					case tabHosts:
						return m, loadHostsCmd(m.client)
					case tabVMs:
						return m, loadVMsCmd(m.client)
					case tabProjects:
						return m, loadProjectsCmd(m.client)
					case tabResource:
						if rm, ok := m.resource[m.currentResource]; ok {
							return m, rm.loadCmd()
						}
					}
					return m, nil
				}
			}
		}
		// Table header click → toggle column sort (tabResource only
		// for now ; bespoke views land in a follow-up). Header Y
		// inside body = top border (1) + action bar (1) + sep (1)
		// = row 3 inside body. Absolute Y = topbar + 3.
		if m.active == tabResource && msg.Action == tea.MouseActionRelease && msg.X >= m.sidebarWidth() {
			headerY := m.topbarHeight() + 3
			if msg.Y == headerY {
				if rm, ok := m.resource[m.currentResource]; ok {
					if col := rm.columnAtX(msg.X - m.sidebarWidth()); col >= 0 {
						if col == rm.sortCol {
							rm.toggleSortDir()
						} else {
							rm.sortCol = col
							rm.sortAsc = true
							rm.applyRowsToTable()
						}
						m.resource[m.currentResource] = rm
						return m, nil
					}
				}
			}
		}
		// Log-pane tab click : switch the active tab when the
		// operator clicks one of the bordered tab boxes at the top
		// of the log pane. Y is in the strip's 3-line rect ; X is
		// relative to the pane's left edge (which begins at
		// sidebarWidth()).
		if inTabStrip && msg.Action == tea.MouseActionRelease && msg.X >= m.sidebarWidth() {
			localX := msg.X - m.sidebarWidth() - 2 // -2 for LogPaneBox border+padding
			if id := m.logPane.tabHitX(localX); id != "" {
				m.logPane.switchTab(id)
				// Switching to "events" lazily opens the
				// platform-event stream — same trigger as the
				// keypath but now from the log-pane tabbar
				// (operator directive 2026-06-24 "passer la
				// vue events dans la zone tabbar").
				if id == "events" && m.eventsPump == nil && m.client != nil {
					m.eventsPump = newEventStreamPump()
					return m, startEventsStreamCmd(m.client, m.eventsPump)
				}
				return m, nil
			}
		}
		// Context menu : Ctrl+Shift+Left-click on a body row opens
		// the per-row action menu (was right-click pre-2026-06-23,
		// switched to avoid conflicts with the terminal's own
		// right-click handling). Trigger on release so a drag-
		// click-release doesn't open the menu mid-selection.
		if msg.Ctrl && msg.Shift && msg.Action == tea.MouseActionRelease {
			if msg.X < m.sidebarWidth() {
				m.menu.open = false
				return m, nil
			}
			row := msg.Y - m.topbarHeight() - 2
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
		}
		// Sidebar drag-resize : a Press exactly on the sidebar's
		// right-border column enters drag mode ; Motion updates the
		// width live ; Release exits. Has to be checked BEFORE the
		// "release ignored" guard so motion events get through.
		//
		// Log-pane drag-resize : a Press exactly on the top-border
		// row of the log pane (terminal Y = topbarHeight +
		// bodyHeight) enters vertical drag mode. The handle is
		// visible as the LogPaneBox's top `─` line ; the operator
		// pulls it up to grow the log pane / down to shrink.
		sw := m.sidebarWidth()
		boundaryX := sw - 1
		logHandleY := m.topbarHeight() + m.bodyHeight() // first row of log pane
		switch msg.Action {
		case tea.MouseActionPress:
			if msg.X == boundaryX {
				m.dragSidebar = true
				return m, nil
			}
			if msg.Y == logHandleY && msg.X >= m.sidebarWidth() {
				m.dragLogPane = true
				return m, nil
			}
		case tea.MouseActionMotion:
			if m.dragSidebar {
				newW := msg.X + 1
				if newW < minSidebarWidth {
					newW = minSidebarWidth
				}
				if newW > maxSidebarWidth {
					newW = maxSidebarWidth
				}
				if m.width > 0 && newW > m.width-20 {
					newW = m.width - 20
				}
				m.sidebarW = newW
				// Re-resize tables to the new body width.
				h := m.bodyHeight() - 1
				bw := m.bodyInnerWidth()
				applyResize(&m.hosts.table, hostsColumns(), bw, h)
				applyResize(&m.vms.table, vmsColumns(), bw, h)
				applyResize(&m.projects.table, projectsColumns(), bw, h)
				for _, rm := range m.resource {
					applyResize(&rm.table, rm.cfg.Columns, bw, h)
				}
				return m, nil
			}
			if m.dragLogPane {
				// New log pane top row = msg.Y (where the handle
				// has been pulled). New log pane total height =
				// terminal-Y of statusbar - msg.Y - 2.
				bottomY := m.height - 2 // status bar starts here (incl. its top border)
				newPaneHeight := bottomY - msg.Y
				// newPaneHeight is the TOTAL log pane lines ; the
				// viewport content height = total - chrome (5).
				newVPHeight := newPaneHeight - 5
				m.logPaneH = newVPHeight
				m.logPane.SetHeight(newVPHeight)
				// Re-resize tables since bodyHeight changed.
				h := m.bodyHeight() - 1
				bw := m.bodyInnerWidth()
				applyResize(&m.hosts.table, hostsColumns(), bw, h)
				applyResize(&m.vms.table, vmsColumns(), bw, h)
				applyResize(&m.projects.table, projectsColumns(), bw, h)
				for _, rm := range m.resource {
					applyResize(&rm.table, rm.cfg.Columns, bw, h)
				}
				return m, nil
			}
		case tea.MouseActionRelease:
			if m.dragSidebar {
				m.dragSidebar = false
				return m, nil
			}
			if m.dragLogPane {
				m.dragLogPane = false
				return m, nil
			}
		}
		// Single click = MouseActionPress ; we only act on the
		// release so motion-while-pressed doesn't drag-select.
		if msg.Action != tea.MouseActionRelease {
			return m, nil
		}
		// Mouse Y → region-relative Y. The topbar lives above the
		// sidebar + body row, so every comparison below must
		// subtract its height. A regression here (forgetting the
		// subtraction) makes the operator's click hit the row above
		// the one they pointed at — the user-reported bug post-
		// topbar-introduction. Pinned by sidebar-click test.
		regionY := msg.Y - m.topbarHeight()
		if msg.X < sw {
			if e, ok := m.sidebarHitRows()[regionY]; ok {
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
			if y, ok := m.menuHitRow(regionY); ok {
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
		row := regionY - 2
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
	case tabEvents:
		// Lazily open the event stream the first time the tab is
		// reached — mirror the keypath at handleKey "4". The
		// sidebar click path used to skip this entirely
		// (operator-reported 2026-06-24 "la vue events ne
		// semble pas cablée").
		if m.eventsPump == nil && m.client != nil {
			m.eventsPump = newEventStreamPump()
			return m, startEventsStreamCmd(m.client, m.eventsPump)
		}
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

// activeRefreshTS returns the lastRefresh timestamp for whichever
// view is currently active, or zero if the view has never loaded.
// Used by the topbar to surface "refreshed HH:MM:SS" — moved out
// of the status bar per 2026-06-23 directive.
func (m Model) activeRefreshTS() time.Time {
	switch m.active {
	case tabHosts:
		return m.hosts.lastRefresh
	case tabVMs:
		return m.vms.lastRefresh
	case tabProjects:
		return m.projects.lastRefresh
	case tabResource:
		if rm, ok := m.resource[m.currentResource]; ok {
			return rm.refresh
		}
	}
	return time.Time{}
}

func (m Model) renderStatusBar() string {
	// The active-tab indicator AND the "refreshed HH:MM:SS" stamp
	// both moved up to the topbar (2026-06-23 directive, reiterated
	// today). The status bar now carries just the connection status
	// + status message / help hint.
	// Status bar : transient status messages only (success / error
	// from a recent action). The conn ● indicator moved up to the
	// topbar (operator directive 2026-06-23 "affiche le point avec
	// pc en permanence avec une couleur ... dans la topbar"). The
	// status bar's top border was also dropped — it was the "bout de
	// ligne orphelin sous les frames" the operator flagged.
	if m.statusMsg == "" {
		// Render a single padded space line so the layout's height
		// budget (- 1 row for status bar in bodyHeight()) stays
		// constant whether or not there's a message.
		return m.theme.StatusBar.Render("")
	}
	if m.statusErr {
		return m.theme.StatusBar.Render(m.theme.StatusErr.Render(m.statusMsg))
	}
	return m.theme.StatusBar.Render(m.theme.StatusMsg.Render(m.statusMsg))
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
