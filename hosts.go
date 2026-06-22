package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
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
	UUID              string
	Hostname          string
	AZ                string
	Rack              string
	Hypervisor        string
	State             string
	Cordoned          bool
	Connected         bool
	LastSeen          time.Time
	AgentVersion      string
	DriverVersions    map[string]string
	OSPretty          string
	OSID              string
	OSVersion         string
	KernelVersion     string
	NetworkInterfaces []hostsNIC
	StorageMounts     []hostsMount
}

// hostsNIC mirrors the wire NetworkInterface ; field names match the
// drawer rendering shorthand. LinkSpeedMbps == 0 means "unknown" ;
// the drawer renders "?" in that case.
type hostsNIC struct {
	Name          string
	MAC           string
	IPv4CIDRs     []string
	IPv6CIDRs     []string
	LinkSpeedMbps int64
	MTU           int32
	OperState     string
}

type hostsMount struct {
	Mountpoint string
	Device     string
	FSType     string
	TotalBytes int64
	FreeBytes  int64
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
	// controlPlaneUUIDs is the set of host UUIDs that are MEMBERS of
	// the control plane's etcd quorum — populated at boot from
	// GetClusterInfo.control_plane_host_uuids (see main.go's
	// autoFetchClusterInfo). Used in tableRow to mark the "CP" column
	// for every host belonging to the quorum (3 in a 3-DC HA cluster,
	// 1 in single-host dev). Nil/empty → no row is marked.
	controlPlaneUUIDs map[string]struct{}

	// confirmRemove is non-empty when the user pressed `x` ; holds
	// the UUID of the host pending confirmation. While set, the
	// view shows the modal and only y/n/Esc are honoured.
	confirmRemove   string
	confirmHostname string

	// detailOpen + detailUUID drive the inspector drawer that pops
	// up on `Enter`. Mirrors the ResourceListModel's detail flow so
	// the operator can read every field of a host (properties,
	// uptime, version, etc.) without dropping into the CLI.
	detailOpen bool
	detailUUID string

	lastRefresh time.Time
}

// selectedRow returns the currently-highlighted hostsRow + true, or
// (zero, false) when the table is empty. Used by the detail drawer.
func (m *hostsModel) selectedRow() (hostsRow, bool) {
	if len(m.rows) == 0 {
		return hostsRow{}, false
	}
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rows) {
		return hostsRow{}, false
	}
	return m.rows[idx], true
}

// rowByUUID looks up a host by UUID — used by the detail drawer's
// re-render path so a refresh that lands between Enter and the next
// frame doesn't lose the focus row.
func (m *hostsModel) rowByUUID(uuid string) (hostsRow, bool) {
	for _, r := range m.rows {
		if r.UUID == uuid {
			return r, true
		}
	}
	return hostsRow{}, false
}

// detailView renders the inspector drawer for the row identified by
// detailUUID. Every field of hostsRow appears on its own line ;
// state + connection get the same badge tint as the table cells.
func (m *hostsModel) detailView(width int) string {
	r, ok := m.rowByUUID(m.detailUUID)
	if !ok {
		return m.theme.HelpBox.Render("(host no longer in roster — press Esc to close)")
	}
	var b strings.Builder
	b.WriteString(m.theme.Title.Render("Host " + dashEmpty(r.Hostname)))
	b.WriteString("\n\n")
	pairs := []struct{ k, v string }{
		{"UUID", r.UUID},
		{"Hostname", dashEmpty(r.Hostname)},
		{"AZ", dashEmpty(r.AZ)},
		{"Rack", dashEmpty(r.Rack)},
		{"Hypervisor", dashEmpty(r.Hypervisor)},
		{"State", r.State},
		{"Cordoned", boolBadge(r.Cordoned, m.theme)},
		{"Connected", boolBadge(r.Connected, m.theme)},
		{"Agent version", dashEmpty(r.AgentVersion)},
		{"Last seen", lastSeenString(r.LastSeen)},
	}
	for _, p := range pairs {
		b.WriteString(m.theme.StatusKey.Render(padKey(p.k)))
		b.WriteString("  ")
		b.WriteString(p.v)
		b.WriteString("\n")
	}
	if len(r.DriverVersions) > 0 {
		b.WriteString("\n")
		b.WriteString(m.theme.StatusKey.Render(padKey("Drivers")))
		b.WriteString("\n")
		kinds := make([]string, 0, len(r.DriverVersions))
		for k := range r.DriverVersions {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			b.WriteString("  ")
			b.WriteString(m.theme.StatusKey.Render(padKey(k)))
			b.WriteString("  ")
			b.WriteString(r.DriverVersions[k])
			b.WriteString("\n")
		}
	}
	if r.OSPretty != "" || r.KernelVersion != "" {
		b.WriteString("\n")
		b.WriteString(m.theme.StatusKey.Render(padKey("OS")))
		b.WriteString("  ")
		b.WriteString(dashEmpty(r.OSPretty))
		b.WriteString("\n")
		b.WriteString(m.theme.StatusKey.Render(padKey("Kernel")))
		b.WriteString("  ")
		b.WriteString(dashEmpty(r.KernelVersion))
		b.WriteString("\n")
	}
	if len(r.NetworkInterfaces) > 0 {
		b.WriteString("\n")
		b.WriteString(m.theme.StatusKey.Render(padKey("Network")))
		b.WriteString("\n")
		for _, n := range r.NetworkInterfaces {
			// Header line : interface name + its link summary
			// (speed + operstate + mtu). The address details
			// follow indented on subsequent lines so a multi-IP
			// NIC reads as a vertical list, not a long wrap.
			b.WriteString("  ")
			b.WriteString(m.theme.StatusKey.Render(padKey(n.Name)))
			b.WriteString("  ")
			b.WriteString(nicHeader(n))
			b.WriteString("\n")
			for _, ip := range n.IPv4CIDRs {
				b.WriteString("    ")
				b.WriteString(m.theme.StatusKey.Render(padKey("ipv4")))
				b.WriteString("  ")
				b.WriteString(ip)
				b.WriteString("\n")
			}
			for _, ip := range n.IPv6CIDRs {
				b.WriteString("    ")
				b.WriteString(m.theme.StatusKey.Render(padKey("ipv6")))
				b.WriteString("  ")
				b.WriteString(ip)
				b.WriteString("\n")
			}
			if n.MAC != "" {
				b.WriteString("    ")
				b.WriteString(m.theme.StatusKey.Render(padKey("mac")))
				b.WriteString("  ")
				b.WriteString(n.MAC)
				b.WriteString("\n")
			}
		}
	}
	if len(r.StorageMounts) > 0 {
		b.WriteString("\n")
		b.WriteString(m.theme.StatusKey.Render(padKey("Storage")))
		b.WriteString("\n")
		for _, mt := range r.StorageMounts {
			b.WriteString("  ")
			b.WriteString(m.theme.StatusKey.Render(padKey(mt.Mountpoint)))
			b.WriteString("  ")
			b.WriteString(formatMount(mt))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(m.theme.Faint.Render("Esc / Enter close · c cordon · u uncordon · d state→down · x remove"))
	return m.theme.HelpBox.Render(b.String())
}

// nicHeader renders the per-NIC summary line (speed · state · mtu).
// The address details (ipv4 / ipv6 / mac) land on their own lines
// below, indented, so a NIC with multiple IPs reads as a vertical
// list rather than a wrapped paragraph.
func nicHeader(n hostsNIC) string {
	parts := make([]string, 0, 3)
	if n.LinkSpeedMbps > 0 {
		parts = append(parts, formatMbps(n.LinkSpeedMbps)+" "+dashEmpty(n.OperState))
	} else if n.OperState != "" {
		parts = append(parts, n.OperState)
	}
	if n.MTU > 0 {
		parts = append(parts, "mtu "+strconv.Itoa(int(n.MTU)))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}

// formatMount renders one storage mount : "ext4 18 GiB free / 32 GiB · /dev/vda1".
func formatMount(m hostsMount) string {
	parts := make([]string, 0, 3)
	if m.FSType != "" {
		parts = append(parts, m.FSType)
	}
	if m.TotalBytes > 0 {
		parts = append(parts, formatBytes(m.FreeBytes)+" free / "+formatBytes(m.TotalBytes))
	}
	if m.Device != "" {
		parts = append(parts, m.Device)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}

func formatMbps(m int64) string {
	switch {
	case m >= 1000:
		return strconv.FormatInt(m/1000, 10) + "Gbps"
	default:
		return strconv.FormatInt(m, 10) + "Mbps"
	}
}

// formatBytes renders one byte count as a human-readable size (GiB
// for >= 1GiB, MiB otherwise — operators don't care about kB).
func formatBytes(b int64) string {
	const (
		KiB = int64(1024)
		MiB = 1024 * KiB
		GiB = 1024 * MiB
		TiB = 1024 * GiB
	)
	switch {
	case b >= TiB:
		return strconv.FormatFloat(float64(b)/float64(TiB), 'f', 1, 64) + " TiB"
	case b >= GiB:
		return strconv.FormatFloat(float64(b)/float64(GiB), 'f', 1, 64) + " GiB"
	case b >= MiB:
		return strconv.FormatFloat(float64(b)/float64(MiB), 'f', 0, 64) + " MiB"
	default:
		return strconv.FormatInt(b/KiB, 10) + " KiB"
	}
}

// boolBadge renders true/false with the active theme's badge styles.
func boolBadge(v bool, theme Theme) string {
	if v {
		return theme.BadgeOK.Render("yes")
	}
	return theme.BadgeBad.Render("no")
}

// lastSeenString formats a last-seen timestamp ; zero → "—".
func lastSeenString(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04:05") + " (" + humanAge(t) + ")"
}

// humanAge renders a coarse "Nm ago" / "Nh ago" / "Nd ago" string —
// no time.Until below seconds, no fractional units. Enough resolution
// for the operator to spot a stale heartbeat.
func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	}
}

// hostsColumns returns the Hosts table's canonical column layout.
// Declared widths act as WEIGHTS the responsive layer (responsive.go)
// distributes proportionally on every WindowSizeMsg ; keep them
// relative to each other rather than absolute.
//
// "CP" is the control-plane marker : `*` when the host's UUID
// matches the local_host_uuid returned by GetClusterInfo (the host
// serving this TUI session's gRPC socket), `—` otherwise. Lets
// operators see which host is the CP they're driving without
// chasing the socket path.
//
// STATE / CONN dropped the badge styling that was hiding their
// content under narrow terminals — the bubbles/table truncator
// gets confused when ANSI sequences land inside the cut window.
// Plain text + wider weights restores visibility.
func hostsColumns() []table.Column {
	return []table.Column{
		{Title: "CP", Width: 3},
		{Title: "UUID", Width: 8},
		{Title: "HOSTNAME", Width: 20},
		{Title: "AZ", Width: 6},
		{Title: "RACK", Width: 6},
		{Title: "STATE", Width: 14},
		{Title: "CONN", Width: 8},
		{Title: "VERSION", Width: 10},
		{Title: "DRIVERS", Width: 22},
		{Title: "LAST-SEEN", Width: 20},
	}
}

func newHostsModel(theme Theme) hostsModel {
	cols := hostsColumns()
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
		hr.AgentVersion = h.AgentVersion
		if len(h.DriverVersions) > 0 {
			hr.DriverVersions = make(map[string]string, len(h.DriverVersions))
			for k, v := range h.DriverVersions {
				hr.DriverVersions[k] = v
			}
		}
		hr.OSPretty = h.OsPretty
		hr.OSID = h.OsId
		hr.OSVersion = h.OsVersion
		hr.KernelVersion = h.KernelVersion
		for _, n := range h.NetworkInterfaces {
			if n == nil {
				continue
			}
			hr.NetworkInterfaces = append(hr.NetworkInterfaces, hostsNIC{
				Name:          n.Name,
				MAC:           n.Mac,
				IPv4CIDRs:     append([]string(nil), n.Ipv4Cidrs...),
				IPv6CIDRs:     append([]string(nil), n.Ipv6Cidrs...),
				LinkSpeedMbps: n.LinkSpeedMbps,
				MTU:           n.Mtu,
				OperState:     n.Operstate,
			})
		}
		for _, m := range h.StorageMounts {
			if m == nil {
				continue
			}
			hr.StorageMounts = append(hr.StorageMounts, hostsMount{
				Mountpoint: m.Mountpoint,
				Device:     m.Device,
				FSType:     m.Fstype,
				TotalBytes: m.TotalBytes,
				FreeBytes:  m.FreeBytes,
			})
		}
		rows = append(rows, hr)
		tableRows = append(tableRows, hr.tableRow(m.theme, m.controlPlaneUUIDs))
	}
	m.rows = rows
	m.table.SetRows(tableRows)
	m.loading = false
	m.err = nil
	m.lastRefresh = time.Now()
}

// tableRow renders one hostsRow as the slice of strings the
// bubbles/table widget consumes. PLAIN text — no inline ANSI/badge
// styles in the cells. bubbles/table's truncator measures visible
// width via lipgloss but several widget versions still cut ANSI
// sequences mid-byte under narrow terminals, blanking the cell.
// Plain values are width-safe.
//
// controlPlaneUUIDs, when non-empty, marks each row whose UUID
// is a MEMBER of the control plane's etcd quorum with "*" in the
// leading "CP" column. Tip from main.go : the set comes from
// GetClusterInfo.control_plane_host_uuids (one entry per etcd
// member in HA, one entry in single-host dev).
func (h hostsRow) tableRow(theme Theme, controlPlaneUUIDs map[string]struct{}) table.Row {
	state := dashEmpty(h.State)
	if h.Cordoned {
		state = state + " (cordoned)"
	}
	conn := "no"
	if h.Connected {
		conn = "yes"
	}
	last := "—"
	if !h.LastSeen.IsZero() {
		last = h.LastSeen.Format("2006-01-02 15:04:05")
	}
	uuidShort := h.UUID
	if len(uuidShort) > 8 {
		uuidShort = uuidShort[:8]
	}
	cp := "—"
	if _, ok := controlPlaneUUIDs[h.UUID]; ok {
		cp = "*"
	}
	ver := dashEmpty(h.AgentVersion)
	drivers := dashEmpty(formatDriverVersions(h.DriverVersions))
	// HYP dropped : DRIVERS already carries each loaded driver's
	// kind (e.g. "qemu:v0.6.0") so a separate column would be
	// redundant. Hypervisor is still in the detail drawer for
	// hosts that registered before the driver-versions feature
	// (DriverVersions empty, Hypervisor non-empty).
	return table.Row{
		cp,
		uuidShort,
		dashEmpty(h.Hostname),
		dashEmpty(h.AZ),
		dashEmpty(h.Rack),
		state,
		conn,
		ver,
		drivers,
		last,
	}
}

// formatDriverVersions condenses a kind→version map into one cell-
// friendly string : "vz:v0.5.0 qemu:v0.6.0", kinds sorted so the
// rendering is stable across refreshes. Empty map → "".
func formatDriverVersions(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	kinds := make([]string, 0, len(m))
	for k := range m {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, k+":"+m[k])
	}
	return strings.Join(parts, " ")
}

// View renders the hosts tab. The confirm-remove modal AND the
// detail drawer, when active, are drawn instead of the table —
// saves us a separate overlay path. Order : detail drawer first
// (since it's the read-only inspector ; if a confirm-remove
// arrived underneath, Esc closes the drawer + the remove modal
// becomes visible on the next render).
func (m hostsModel) View(width int) string {
	if m.detailOpen {
		body := m.detailView(width)
		return lipgloss.Place(width, lipgloss.Height(body), lipgloss.Center, lipgloss.Top, body)
	}
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
