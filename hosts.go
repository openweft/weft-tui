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
	"google.golang.org/grpc"
)

// HostsClient is the narrow gRPC surface the hosts view needs. Lifted
// to an interface so the test suite can stub it without dialling a
// real socket. The concrete production impl is just the
// `weftv1.WeftAgentClient` returned by shared.Client — its method
// signatures match this interface exactly.
type HostsClient interface {
	ListHosts(ctx context.Context, in *weftv1.ListHostsRequest, opts ...grpc.CallOption) (*weftv1.ListHostsResponse, error)
	SetHostCordoned(ctx context.Context, in *weftv1.SetHostCordonedRequest, opts ...grpc.CallOption) (*weftv1.SetHostCordonedResponse, error)
	SetHostState(ctx context.Context, in *weftv1.SetHostStateRequest, opts ...grpc.CallOption) (*weftv1.SetHostStateResponse, error)
	DeleteHost(ctx context.Context, in *weftv1.DeleteHostRequest, opts ...grpc.CallOption) (*weftv1.DeleteHostResponse, error)
}

// hostsRow is the in-memory copy of a Host the view needs ; lifted off
// the wire so the table render path is decoupled from the proto types
// (the wire structs carry a lot more fields than we display).
type hostsRow struct {
	UUID       string
	Hostname   string
	AZ         string
	Rack       string
	Hypervisor string
	State      string
	Cordoned   bool
	Connected  bool
	LastSeen   time.Time
}

// hostsModel owns the state of the Hosts tab : the bubbles/table, the
// in-memory host list, the refresh timestamp, and any confirm-modal
// state for the destructive `x` key.
type hostsModel struct {
	theme   Theme
	table   table.Model
	rows    []hostsRow
	loading bool
	err     error

	// confirmRemove is non-empty when the user pressed `x` ; holds
	// the UUID of the host pending confirmation. While set, the
	// view shows the modal and only y/n/Esc are honoured.
	confirmRemove   string
	confirmHostname string

	lastRefresh time.Time
}

func newHostsModel(theme Theme) hostsModel {
	cols := []table.Column{
		{Title: "UUID", Width: 8},
		{Title: "HOSTNAME", Width: 20},
		{Title: "AZ", Width: 6},
		{Title: "RACK", Width: 6},
		{Title: "HYP", Width: 10},
		{Title: "STATE", Width: 10},
		{Title: "CONN", Width: 5},
		{Title: "LAST-SEEN", Width: 20},
	}
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
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("0")).
		Background(lipgloss.AdaptiveColor{Light: "#A78BFA", Dark: "#A78BFA"}).
		Bold(true)
	tbl.SetStyles(s)
	return hostsModel{theme: theme, table: tbl, loading: true}
}

// selectedUUID returns the UUID column value of the currently
// highlighted row, or "" if the table is empty.
func (m *hostsModel) selectedUUID() string {
	if len(m.rows) == 0 {
		return ""
	}
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rows) {
		return ""
	}
	return m.rows[idx].UUID
}

func (m *hostsModel) selectedHostname() string {
	if len(m.rows) == 0 {
		return ""
	}
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rows) {
		return ""
	}
	return m.rows[idx].Hostname
}

// hostnameByUUID returns the operator-visible hostname for a given
// host UUID, "" if the UUID isn't in the current rows. Used by the
// VMs tab to resolve VMInfo.host_uuid into a friendly column value
// without a second ListHosts roundtrip.
func (m *hostsModel) hostnameByUUID(uuid string) string {
	for i := range m.rows {
		if m.rows[i].UUID == uuid {
			return m.rows[i].Hostname
		}
	}
	return ""
}

// applyHosts refreshes the in-memory rows + the underlying table.
// Called from the Bubble Tea Update handler when a hostsLoadedMsg
// arrives.
func (m *hostsModel) applyHosts(resp *weftv1.ListHostsResponse) {
	connected := make(map[string]bool, len(resp.ConnectedHostUuids))
	for _, u := range resp.ConnectedHostUuids {
		connected[u] = true
	}
	rows := make([]hostsRow, 0, len(resp.Hosts))
	tableRows := make([]table.Row, 0, len(resp.Hosts))
	for _, h := range resp.Hosts {
		hr := hostsRow{
			UUID:       h.Uuid,
			Hostname:   h.Hostname,
			AZ:         h.Az,
			Rack:       h.Rack,
			Hypervisor: h.Hypervisor,
			State:      h.State,
			Cordoned:   h.Cordoned,
			Connected:  connected[h.Uuid],
		}
		if h.LastSeenAtUnixNs != 0 {
			hr.LastSeen = time.Unix(0, h.LastSeenAtUnixNs)
		}
		rows = append(rows, hr)
		tableRows = append(tableRows, hr.tableRow(m.theme))
	}
	m.rows = rows
	m.table.SetRows(tableRows)
	m.loading = false
	m.err = nil
	m.lastRefresh = time.Now()
}

// tableRow renders one hostsRow as the slice of strings the
// bubbles/table widget consumes. The state column gets a badge tint
// based on cordoned / down / active so operators can scan a long list
// at a glance.
func (h hostsRow) tableRow(theme Theme) table.Row {
	state := dashEmpty(h.State)
	if h.Cordoned {
		state = theme.BadgeWarn.Render(state + "*")
	} else {
		switch strings.ToLower(h.State) {
		case "active":
			state = theme.BadgeOK.Render(state)
		case "down":
			state = theme.BadgeBad.Render(state)
		case "draining":
			state = theme.BadgeWarn.Render(state)
		}
	}
	conn := "no"
	if h.Connected {
		conn = theme.BadgeOK.Render("yes")
	} else {
		conn = theme.BadgeBad.Render("no")
	}
	last := "—"
	if !h.LastSeen.IsZero() {
		last = h.LastSeen.Format("2006-01-02 15:04:05")
	}
	uuidShort := h.UUID
	if len(uuidShort) > 8 {
		uuidShort = uuidShort[:8]
	}
	return table.Row{
		uuidShort,
		dashEmpty(h.Hostname),
		dashEmpty(h.AZ),
		dashEmpty(h.Rack),
		dashEmpty(h.Hypervisor),
		state,
		conn,
		last,
	}
}

// View renders the hosts tab. The confirm-remove modal, when active,
// is drawn instead of the table — saves us a separate overlay path.
func (m hostsModel) View(width int) string {
	if m.confirmRemove != "" {
		body := fmt.Sprintf(
			"Remove host %s (%s) ?\n\nThis does NOT stop its VMs — drain first.\n\n  y   confirm\n  n   cancel",
			m.confirmHostname, m.confirmRemove,
		)
		box := m.theme.ConfirmBox.Render(body)
		return lipgloss.Place(width, lipgloss.Height(box), lipgloss.Center, lipgloss.Top, box)
	}
	if m.loading && len(m.rows) == 0 {
		return m.theme.Faint.Render("  loading hosts…")
	}
	if m.err != nil {
		return m.theme.StatusErr.Render("  error: " + m.err.Error())
	}
	if len(m.rows) == 0 {
		return m.theme.Faint.Render("  no hosts registered. Run `weft host register` from a hypervisor agent.")
	}
	return m.table.View()
}

// hostsLoadedMsg is delivered to Update when an async ListHosts
// completes. carrying the response (or an error). The model swaps
// out its rows when it arrives.
type hostsLoadedMsg struct {
	resp *weftv1.ListHostsResponse
	err  error
}

// hostActionMsg is the result of a cordon / state / delete RPC. The
// status bar surfaces ok / err so the operator sees the outcome.
type hostActionMsg struct {
	action string // human-readable verb, e.g. "cordon", "remove"
	host   string // hostname (or UUID fallback) for the status line
	err    error
}

// loadHostsCmd dials the agent and fetches the host list. Returns a
// tea.Cmd that resolves to a hostsLoadedMsg, suitable for plumbing
// into the model's Init / refresh handlers.
func loadHostsCmd(client HostsClient) tea.Cmd {
	if client == nil {
		return func() tea.Msg {
			return hostsLoadedMsg{err: errNoClient}
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := client.ListHosts(ctx, &weftv1.ListHostsRequest{})
		return hostsLoadedMsg{resp: resp, err: err}
	}
}

// cordonCmd / setStateCmd / deleteCmd are thin tea.Cmd wrappers around
// the three mutating RPCs the Hosts tab can issue. Each returns a
// hostActionMsg the model handles uniformly.
func cordonCmd(client HostsClient, uuid, hostname string, cordoned bool) tea.Cmd {
	verb := "cordon"
	if !cordoned {
		verb = "uncordon"
	}
	return func() tea.Msg {
		if client == nil {
			return hostActionMsg{action: verb, host: hostname, err: errNoClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := client.SetHostCordoned(ctx, &weftv1.SetHostCordonedRequest{Uuid: uuid, Cordoned: cordoned})
		return hostActionMsg{action: verb, host: hostname, err: err}
	}
}

func setStateCmd(client HostsClient, uuid, hostname, state string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return hostActionMsg{action: "set-state " + state, host: hostname, err: errNoClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := client.SetHostState(ctx, &weftv1.SetHostStateRequest{Uuid: uuid, State: state})
		return hostActionMsg{action: "set-state " + state, host: hostname, err: err}
	}
}

func deleteHostCmd(client HostsClient, uuid, hostname string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return hostActionMsg{action: "remove", host: hostname, err: errNoClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := client.DeleteHost(ctx, &weftv1.DeleteHostRequest{Uuid: uuid})
		return hostActionMsg{action: "remove", host: hostname, err: err}
	}
}

func dashEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
