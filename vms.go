package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// VMsClient is the narrow gRPC surface the VMs tab needs. Same
// interface trick as HostsClient — keeps the table tests free of a
// real socket. Mapped 1:1 onto the production weftv1.WeftAgentClient.
type VMsClient interface {
	ListVMs(ctx context.Context, in *weftv1.ListVMsRequest, opts ...grpc.CallOption) (*weftv1.ListVMsResponse, error)
	StartVM(ctx context.Context, in *weftv1.StartVMRequest, opts ...grpc.CallOption) (*weftv1.StartVMResponse, error)
	StopVM(ctx context.Context, in *weftv1.StopVMRequest, opts ...grpc.CallOption) (*weftv1.StopVMResponse, error)
	RestartVM(ctx context.Context, in *weftv1.RestartVMRequest, opts ...grpc.CallOption) (*weftv1.RestartVMResponse, error)
	VMLogs(ctx context.Context, in *weftv1.VMLogsRequest, opts ...grpc.CallOption) (*weftv1.VMLogsResponse, error)
}

// vmRow is the in-memory copy of one VM the table renders. We mirror
// just the columns we display — keeps the render path decoupled from
// the wire proto and lets us add badges per state without re-marshalling.
type vmRow struct {
	Name     string
	Project  string
	UUID     string
	HostUUID string // raw VMInfo.host_uuid from the wire
	HostName string // resolved via the hosts cache ; "" when unknown
	State    string
	Image    string
	CPU      uint32
	MemMB    uint64
	IP       string
}

// vmCountsByHost tallies how many of the given VM rows are placed
// on each host UUID. Rows without a host_uuid don't count anywhere
// (legacy / pre-placement VMs). Used by the Hosts tab to populate
// the VMS column.
func vmCountsByHost(rows []vmRow) map[string]int {
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		if r.HostUUID == "" {
			continue
		}
		out[r.HostUUID]++
	}
	return out
}

// vmsModel owns the VMs tab : table, in-memory rows, modal flags for
// stop-confirm + logs-viewport. The logs viewport is its own state
// because once opened it captures j/k/scroll keys and Esc closes it
// without affecting the underlying table.
type vmsModel struct {
	theme   Theme
	table   table.Model
	rows    []vmRow
	loading bool
	err     error

	// confirmStop is the VM name pending the destructive-stop modal ;
	// the project field disambiguates same-named VMs across projects.
	confirmStop    string
	confirmProject string

	// logsOpen + logsVP draw a viewport overlay with `VMLogs` output
	// when the operator presses `l`. While open, j/k/PgUp/PgDn scroll
	// the buffer ; Esc / q closes it.
	logsOpen    bool
	logsVP      viewport.Model
	logsTitle   string
	logsLoading bool
	logsErr     error

	lastRefresh time.Time
}

// vmsColumns returns the VMs table's canonical column layout (see
// hostsColumns for the responsive-layout contract).
func vmsColumns() []table.Column {
	return []table.Column{
		{Title: "NAME", Width: 18},
		{Title: "PROJECT", Width: 14},
		{Title: "HOST", Width: 10},
		{Title: "STATE", Width: 10},
		{Title: "IMAGE", Width: 22},
		{Title: "CPU", Width: 4},
		{Title: "MEM-MB", Width: 8},
	}
}

func newVMsModel(theme Theme) vmsModel {
	cols := vmsColumns()
	tbl := table.New(
		table.WithColumns(cols),
		table.WithFocused(true),
		table.WithHeight(15),
	)
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"}).
		BorderBottom(true).
		Bold(true)
	s.Selected = theme.SelectedRow
	tbl.SetStyles(s)
	vp := viewport.New(80, 15)
	return vmsModel{theme: theme, table: tbl, logsVP: vp, loading: true}
}

func (m *vmsModel) selected() (name, project string) {
	if len(m.rows) == 0 {
		return "", ""
	}
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rows) {
		return "", ""
	}
	return m.rows[idx].Name, m.rows[idx].Project
}

// refreshHostNames re-runs hostLookup over the current rows and
// re-renders the table. Cheap : no proto allocation, no agent
// roundtrip. Called when a hostsLoadedMsg lands after VMs were
// already on screen — we want the HOST column to surface the
// hostname immediately rather than waiting for the next ListVMs.
func (m *vmsModel) refreshHostNames(hostLookup func(uuid string) string) {
	if hostLookup == nil || len(m.rows) == 0 {
		return
	}
	tableRows := make([]table.Row, 0, len(m.rows))
	changed := false
	for i := range m.rows {
		if m.rows[i].HostUUID != "" {
			if name := hostLookup(m.rows[i].HostUUID); name != m.rows[i].HostName {
				m.rows[i].HostName = name
				changed = true
			}
		}
		tableRows = append(tableRows, m.rows[i].tableRow(m.theme))
	}
	if changed {
		m.table.SetRows(tableRows)
	}
}

// applyVMs refreshes the table + in-memory rows from a ListVMs
// response. The hostLookup callback resolves VMInfo.host_uuid into
// a friendly hostname for the HOST column ; pass nil when the hosts
// cache isn't populated yet (the column will fall back to a short
// UUID, or the IP if the agent hasn't filled host_uuid).
func (m *vmsModel) applyVMs(resp *weftv1.ListVMsResponse, hostLookup func(uuid string) string) {
	rows := make([]vmRow, 0, len(resp.Vms))
	tableRows := make([]table.Row, 0, len(resp.Vms))
	for _, v := range resp.Vms {
		state := vmStateString(v.State)
		hostName := ""
		if hostLookup != nil && v.HostUuid != "" {
			hostName = hostLookup(v.HostUuid)
		}
		row := vmRow{
			Name:     v.Name,
			Project:  v.Project,
			UUID:     v.Uuid,
			HostUUID: v.HostUuid,
			HostName: hostName,
			State:    state,
			Image:    v.Image,
			CPU:      v.Cpu,
			MemMB:    v.MemMb,
			IP:       v.Ip,
		}
		rows = append(rows, row)
		tableRows = append(tableRows, row.tableRow(m.theme))
	}
	m.rows = rows
	m.table.SetRows(tableRows)
	m.loading = false
	m.err = nil
	m.lastRefresh = time.Now()
}

// tableRow is the bubbles/table row for one VM. State gets a coloured
// badge so a running fleet visually pops vs. a stopped/errored one.
func (r vmRow) tableRow(theme Theme) table.Row {
	state := dashEmpty(r.State)
	switch strings.ToLower(r.State) {
	case "running":
		state = theme.BadgeOK.Render(state)
	case "stopped":
		state = theme.BadgeWarn.Render(state)
	case "error":
		state = theme.BadgeBad.Render(state)
	}
	// HOST column preference : hostname (operator-recognisable) →
	// short host UUID (8 chars, cross-references the Hosts tab) →
	// IP (legacy fallback for agents on weft-proto < v0.12.0).
	host := r.HostName
	if host == "" && r.HostUUID != "" {
		host = r.HostUUID
		if len(host) > 8 {
			host = host[:8]
		}
	}
	if host == "" {
		host = r.IP
	}
	if host == "" {
		host = "—"
	}
	if len(host) > 10 {
		host = host[:10]
	}
	return table.Row{
		dashEmpty(r.Name),
		dashEmpty(r.Project),
		host,
		state,
		dashEmpty(truncate(r.Image, 22)),
		fmt.Sprintf("%d", r.CPU),
		fmt.Sprintf("%d", r.MemMB),
	}
}

// View renders the VMs tab body. The logs viewport, when open, draws
// instead of the table — same pattern as the hosts confirm modal.
func (m vmsModel) View(width int) string {
	if m.logsOpen {
		title := m.theme.Title.Render("VM logs — " + m.logsTitle)
		hint := m.theme.Faint.Render("j/k scroll · PgUp/PgDn page · Esc close")
		body := m.logsVP.View()
		if m.logsLoading {
			body = m.theme.Faint.Render("  loading logs…")
		} else if m.logsErr != nil {
			body = m.theme.StatusErr.Render("  error: " + m.logsErr.Error())
		}
		return strings.Join([]string{title, body, hint}, "\n")
	}
	if m.confirmStop != "" {
		body := fmt.Sprintf(
			"Stop VM %s (project %s) ?\n\n  y   confirm\n  n   cancel",
			m.confirmStop, dashEmpty(m.confirmProject),
		)
		box := m.theme.ConfirmBox.Render(body)
		return lipgloss.Place(width, lipgloss.Height(box), lipgloss.Center, lipgloss.Top, box)
	}
	if m.loading && len(m.rows) == 0 {
		return m.theme.Faint.Render("  loading VMs…")
	}
	if m.err != nil {
		return m.theme.StatusErr.Render("  error: " + m.err.Error())
	}
	if len(m.rows) == 0 {
		return m.theme.Faint.Render("  no VMs. Create one with `weft microvm create`.")
	}
	return m.table.View()
}

// --- Cmd factories + messages ---

// vmsLoadedMsg is delivered to Update when an async ListVMs completes.
type vmsLoadedMsg struct {
	resp *weftv1.ListVMsResponse
	err  error
}

// vmActionMsg is the result of a Start / Stop / Restart RPC ; the
// status bar surfaces ok / err.
type vmActionMsg struct {
	action  string
	name    string
	project string
	err     error
}

// vmLogsLoadedMsg carries the result of a VMLogs fetch. tail is the
// last ~200 lines, trimmed by lineTailLast.
type vmLogsLoadedMsg struct {
	name    string
	project string
	tail    string
	err     error
}

func loadVMsCmd(client VMsClient) tea.Cmd {
	if client == nil {
		return func() tea.Msg { return vmsLoadedMsg{err: errNoClient} }
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := client.ListVMs(ctx, &weftv1.ListVMsRequest{})
		return vmsLoadedMsg{resp: resp, err: err}
	}
}

func startVMCmd(client VMsClient, name, project string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return vmActionMsg{action: "start", name: name, project: project, err: errNoClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := client.StartVM(ctx, &weftv1.StartVMRequest{Name: name, Project: project})
		return vmActionMsg{action: "start", name: name, project: project, err: err}
	}
}

func stopVMCmd(client VMsClient, name, project string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return vmActionMsg{action: "stop", name: name, project: project, err: errNoClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := client.StopVM(ctx, &weftv1.StopVMRequest{Name: name, Project: project})
		return vmActionMsg{action: "stop", name: name, project: project, err: err}
	}
}

// restartVMCmd uses the atomic RestartVM RPC (weft-proto v0.12.0+)
// instead of the sequential client-side StopVM → StartVM dance. The
// agent rollbacks (restarts on the same host with the same network
// attachments) when the start half fails — something the client-side
// chain couldn't offer.
func restartVMCmd(client VMsClient, name, project string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return vmActionMsg{action: "restart", name: name, project: project, err: errNoClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := client.RestartVM(ctx, &weftv1.RestartVMRequest{Name: name, Project: project})
		return vmActionMsg{action: "restart", name: name, project: project, err: err}
	}
}

// loadVMLogsCmd asks the agent for the tail of the VM's serial
// console.log. We pull a generous 32 KiB then trim to ~200 lines in
// memory — VMLogs has no native line API.
func loadVMLogsCmd(client VMsClient, name, project string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return vmLogsLoadedMsg{name: name, project: project, err: errNoClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := client.VMLogs(ctx, &weftv1.VMLogsRequest{Name: name, Project: project, TailBytes: 32 * 1024})
		if err != nil {
			return vmLogsLoadedMsg{name: name, project: project, err: err}
		}
		return vmLogsLoadedMsg{name: name, project: project, tail: lineTailLast(string(resp.Contents), 200)}
	}
}

// vmStateString maps a wire VMState into the short uppercase tag the
// table shows (RUNNING / STOPPED / ERROR / —). Falls back to the proto
// String() for anything weft adds in the future.
func vmStateString(s weftv1.VMState) string {
	switch s {
	case weftv1.VMState_VM_STATE_RUNNING:
		return "running"
	case weftv1.VMState_VM_STATE_STOPPED:
		return "stopped"
	case weftv1.VMState_VM_STATE_ERROR:
		return "error"
	case weftv1.VMState_VM_STATE_NOT_CREATED:
		return "not-created"
	}
	return ""
}

// lineTailLast keeps only the last n lines of s. Used to cap log
// dumps before they hit the viewport.
func lineTailLast(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
