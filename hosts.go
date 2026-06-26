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
	// VMCount is the number of VMs currently placed on this host.
	// Refreshed by the cross-pollination in app.go's vmsLoadedMsg /
	// hostsLoadedMsg handlers — both sides recompute the map so a
	// fresh tab visit always sees an up-to-date count.
	VMCount   int
	CPUCount  int32
	MemoryMiB int64
	GPUs      []hostsGPU
}

// hostsGPU mirrors the wire GPU shape for the per-row render. One
// entry per physical accelerator ; the table cell summarises by
// counting same-(vendor,model) pairs (e.g. "H200×4").
type hostsGPU struct {
	Vendor     string
	Model      string
	MemoryGiB  int32
	MIGCapable bool
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
	// sortCol / sortAsc mirror ResourceListModel's contract so the
	// Hosts view inherits the same click-to-sort UX as the catalogue
	// views (Networks, Subnets, etc.). Default = first column,
	// ascending. Audit 2026-06-25 follow-up.
	sortCol int
	sortAsc bool
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

	// metricsRing is a lookup injected by the top-level Model so the
	// detail drawer can read the host's CPU/MEM/Net rolling buffer
	// (filled by hostMetricsBus from NATS). Returns nil when no
	// samples have arrived yet — renderHostMetricsBlock handles
	// that. Function-typed instead of an embedded *hostMetricsBus so
	// hosts.go stays free of NATS imports + the dependency points
	// in one direction only (app.go → hosts.go).
	metricsRing func(uuid string) *hostMetricsRing
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
			// Header line : interface name + link status only
			// (speed + operstate). Every other attribute lands
			// on its own indented line below, in a stable order :
			// mtu > mac > ipv4 > ipv6.
			b.WriteString("  ")
			b.WriteString(m.theme.StatusKey.Render(padKey(n.Name)))
			b.WriteString("  ")
			b.WriteString(nicHeader(n))
			b.WriteString("\n")
			if n.MTU > 0 {
				b.WriteString("    ")
				b.WriteString(m.theme.StatusKey.Render(padKey("mtu")))
				b.WriteString("  ")
				b.WriteString(strconv.Itoa(int(n.MTU)))
				b.WriteString("\n")
			}
			if n.MAC != "" {
				b.WriteString("    ")
				b.WriteString(m.theme.StatusKey.Render(padKey("mac")))
				b.WriteString("  ")
				b.WriteString(n.MAC)
				b.WriteString("\n")
			}
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
	// Append the metrics block (4 sparklines : CPU, MEM, Net rx,
	// Net tx) when the NATS bus has received at least one sample
	// for this host. The block is rendered with a max sparkline
	// width of (width - 40) so the label + live value columns stay
	// readable on narrow terminals. Empty string = no samples yet.
	if m.metricsRing != nil {
		w := width - 40
		if w < 16 {
			w = 16
		}
		if block := renderHostMetricsBlock(m.theme, m.metricsRing(r.UUID), w); block != "" {
			b.WriteString(block)
		}
	}
	b.WriteString("\n")
	b.WriteString(m.theme.Faint.Render("Esc / Enter close · c cordon · u uncordon · d state→down · o remove"))
	return m.theme.HelpBox.Render(b.String())
}

// nicHeader renders the per-NIC summary : link speed + operstate
// only. MAC / IPs / MTU each get their own line below — see the
// drawer renderer in detailView.
func nicHeader(n hostsNIC) string {
	switch {
	case n.LinkSpeedMbps > 0:
		return formatMbps(n.LinkSpeedMbps) + " " + dashEmpty(n.OperState)
	case n.OperState != "":
		return n.OperState
	}
	return "—"
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
		{Title: "UUID", Width: 36},
		{Title: "HOSTNAME", Width: 20},
		{Title: "RACK", Width: 10},
		{Title: "CPU", Width: 5},
		{Title: "RAM", Width: 9},
		{Title: "GPU", Width: 12},
		{Title: "STATE", Width: 10},
		{Title: "STATUS", Width: 10},
		{Title: "CONN", Width: 8},
		{Title: "VMS", Width: 5},
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
	// Padding(0, 0) on Header AND Cell : the default Padding(0, 1)
	// stretches every rendered cell by 2 extra cols, which made the
	// header line + its BorderBottom overflow the body box on
	// narrow (≤120 col) terminals + wrap onto a second line.
	// rescaleColumns assumes this zero-padding so the cell widths
	// sum to the rendered row width exactly.
	s.Header = s.Header.
		Padding(0, 0).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#4B5563"}).
		BorderBottom(true).
		Bold(true)
	s.Cell = s.Cell.Padding(0, 0)
	s.Selected = theme.SelectedRow
	tbl.SetStyles(s)
	return hostsModel{theme: theme, table: tbl, loading: true, sortCol: 0, sortAsc: true}
}

// columnsWithSortArrow mirrors ResourceListModel's helper : reads
// the table's CURRENT (rescaled) columns + suffixes only the
// active sort col's title with ↑ / ↓. Audit follow-up 2026-06-25.
func (m *hostsModel) columnsWithSortArrow() []table.Column {
	cur := m.table.Columns()
	src := hostsColumns()
	if len(cur) == 0 {
		cur = src
	}
	out := make([]table.Column, len(cur))
	for i, c := range cur {
		out[i] = c
		if i < len(src) {
			out[i].Title = src[i].Title
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

// columnAtX maps a body-relative X click coordinate to a hosts
// column index. Returns -1 when X falls past the last column.
func (m *hostsModel) columnAtX(x int) int {
	if x < 0 {
		return -1
	}
	cur := m.table.Columns()
	if len(cur) == 0 {
		cur = hostsColumns()
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

// applySort sorts m.rows by the active sort column + re-renders
// the table rows. Same shape as ResourceListModel.applyRowsToTable.
func (m *hostsModel) applySort() {
	if m.sortCol >= 0 {
		sort.SliceStable(m.rows, func(i, j int) bool {
			a := m.rows[i].tableRow(m.theme, m.controlPlaneUUIDs)
			b := m.rows[j].tableRow(m.theme, m.controlPlaneUUIDs)
			if m.sortCol >= len(a) || m.sortCol >= len(b) {
				return false
			}
			if m.sortAsc {
				return stripANSI(a[m.sortCol]) < stripANSI(b[m.sortCol])
			}
			return stripANSI(a[m.sortCol]) > stripANSI(b[m.sortCol])
		})
	}
	tableRows := make([]table.Row, 0, len(m.rows))
	for _, r := range m.rows {
		tableRows = append(tableRows, r.tableRow(m.theme, m.controlPlaneUUIDs))
	}
	m.table.SetRows(tableRows)
	m.table.SetColumns(m.columnsWithSortArrow())
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
	name, _, _ := m.placementByUUID(uuid)
	return name
}

// placementByUUID returns (hostname, az, rack) for a host UUID, with
// empty strings on miss. The VMs tab uses this to populate its
// HOST / AZ / RACK columns from a single lookup.
func (m *hostsModel) placementByUUID(uuid string) (string, string, string) {
	for i := range m.rows {
		if m.rows[i].UUID == uuid {
			return m.rows[i].Hostname, m.rows[i].AZ, m.rows[i].Rack
		}
	}
	return "", "", ""
}

// applyVMCounts stamps each in-memory row with the number of VMs
// currently placed on it (UUID-keyed). Re-renders the table so the
// new VMS column reflects the count without waiting for the next
// hostsLoadedMsg. Missing UUIDs map to 0 (no VM placed).
func (m *hostsModel) applyVMCounts(counts map[string]int) {
	for i := range m.rows {
		m.rows[i].VMCount = counts[m.rows[i].UUID]
	}
	m.applySort()
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
		hr.CPUCount = h.CpuCount
		hr.MemoryMiB = h.MemoryMib
		for _, g := range h.Gpus {
			if g == nil {
				continue
			}
			hr.GPUs = append(hr.GPUs, hostsGPU{
				Vendor:     g.Vendor,
				Model:      g.Model,
				MemoryGiB:  g.MemoryGib,
				MIGCapable: g.MigCapable,
			})
		}
		rows = append(rows, hr)
	}
	m.rows = rows
	m.applySort() // sorts + SetRows + SetColumns with arrow
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
	// State rendering : tint "down" red + prefix a "✖" so a sick
	// host pops visually. Cordoned suffix preserved. PLAIN text
	// otherwise — bubbles/table's truncator cuts ANSI mid-sequence
	// on narrow widths, so styling stays minimal + safe.
	stateText := dashEmpty(h.State)
	if h.Cordoned {
		stateText = stateText + " (cordoned)"
	}
	state := stateText
	if strings.EqualFold(h.State, "down") {
		// Plain text — no inline ANSI in a bubbles/table cell ;
		// the widget's truncator cuts ANSI mid-sequence on narrow
		// widths, blanking the cell. Audit 2026-06-25.
		state = "✖ " + stateText
	}
	// STATUS column = admin intent, derived from the State enum
	// today : HostStateInactive is the operator-marked "frozen"
	// signal that survives heartbeats (see hostRegistry.heartbeat).
	// Anything else reads as "active" administratively — the actual
	// runtime liveness is still visible in the STATE + CONN columns.
	// Matches the VM Status column's UX so the two views read the
	// same axis the same way (2026-06-24 operator directive).
	statusText := "active"
	if strings.EqualFold(h.State, "inactive") {
		statusText = "inactive"
	}
	conn := "no"
	if h.Connected {
		conn = "yes"
	}
	last := "—"
	if !h.LastSeen.IsZero() {
		last = h.LastSeen.Format("2006-01-02 15:04:05")
	}
	// Full UUID — symmetric with the other catalogue views
	// (Racks, AZs, Tenants…). Was 8 chars to fit a dense table ;
	// the column got widened to 36 in 2026-06-24.
	uuidFull := dashEmpty(h.UUID)
	cp := "—"
	if _, ok := controlPlaneUUIDs[h.UUID]; ok {
		cp = "*"
	}
	ver := dashEmpty(h.AgentVersion)
	drivers := dashEmpty(formatDriverVersions(h.DriverVersions))
	vms := strconv.Itoa(h.VMCount)
	cpu := dashEmptyInt(int(h.CPUCount))
	ram := dashEmpty(formatRAM(h.MemoryMiB))
	gpu := dashEmpty(formatGPUs(h.GPUs))
	// HYP dropped : DRIVERS already carries each loaded driver's
	// kind (e.g. "qemu:v0.6.0") so a separate column would be
	// redundant. Hypervisor is still in the detail drawer for
	// hosts that registered before the driver-versions feature
	// (DriverVersions empty, Hypervisor non-empty).
	// RACK column = "<AZ>:<RACK>" matching the Racks view ;
	// homonym racks across DCs stay distinguishable at a glance.
	// Operator directive 2026-06-24.
	rackLabel := dashEmpty(h.Rack)
	if h.Rack != "" && h.AZ != "" {
		rackLabel = h.AZ + ":" + h.Rack
	} else if h.AZ != "" && h.Rack == "" {
		rackLabel = h.AZ + ":—"
	}
	return table.Row{
		cp,
		uuidFull,
		dashEmpty(h.Hostname),
		rackLabel,
		cpu,
		ram,
		gpu,
		state,
		statusText,
		conn,
		vms,
		ver,
		drivers,
		last,
	}
}

// dashEmptyInt renders an int as "—" when zero, the decimal value
// otherwise. Mirrors dashEmpty's semantics for numeric columns
// (CPU count specifically) so missing data reads consistently
// across the table.
func dashEmptyInt(v int) string {
	if v <= 0 {
		return "—"
	}
	return strconv.Itoa(v)
}

// formatRAM renders a MiB value as a human-readable size :
// "32 GiB" past 1024 MiB, "768 MiB" below. 0 → "" so the dashEmpty
// caller can substitute "—".
func formatRAM(mib int64) string {
	if mib <= 0 {
		return ""
	}
	if mib >= 1024 {
		gib := float64(mib) / 1024.0
		return strconv.FormatFloat(gib, 'f', 0, 64) + " GiB"
	}
	return strconv.FormatInt(mib, 10) + " MiB"
}

// formatGPUs condenses a slice of GPUs into a single cell-friendly
// string. Same-(vendor,model) entries collapse to "model×count"
// (e.g. "H200×4") ; multi-SKU hosts join with " + ". Empty slice
// → "" (dashEmpty caller turns it into "—").
func formatGPUs(gpus []hostsGPU) string {
	if len(gpus) == 0 {
		return ""
	}
	type key struct{ vendor, model string }
	counts := map[key]int{}
	order := []key{}
	for _, g := range gpus {
		k := key{g.Vendor, g.Model}
		if _, seen := counts[k]; !seen {
			order = append(order, k)
		}
		counts[k]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		label := k.model
		if label == "" {
			label = k.vendor
		}
		if label == "" {
			label = "gpu"
		}
		if counts[k] > 1 {
			label += "×" + strconv.Itoa(counts[k])
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, " + ")
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
