package main

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// ProjectsClient is the narrow gRPC surface the Projects tab needs.
// Lifted for the same test-stub reasons as HostsClient / VMsClient.
type ProjectsClient interface {
	ListProjects(ctx context.Context, in *weftv1.ListProjectsRequest, opts ...grpc.CallOption) (*weftv1.ListProjectsResponse, error)
	CreateProject(ctx context.Context, in *weftv1.CreateProjectRequest, opts ...grpc.CallOption) (*weftv1.CreateProjectResponse, error)
	DeleteProject(ctx context.Context, in *weftv1.DeleteProjectRequest, opts ...grpc.CallOption) (*weftv1.DeleteProjectResponse, error)
	// ListVMs lets the tab show a per-project VM count — there's no
	// CountByProject RPC, so we fan out once per refresh against the
	// same ListVMs call the VMs tab uses, then group client-side.
	ListVMs(ctx context.Context, in *weftv1.ListVMsRequest, opts ...grpc.CallOption) (*weftv1.ListVMsResponse, error)
}

// projectRow is the table-side view of one project. VMCount is
// derived from a sibling ListVMs call ; -1 marks "unknown" so the
// renderer can show "?" while the first count call is in flight.
type projectRow struct {
	Name    string
	UUID    string
	Created time.Time
	VMCount int
}

// projectsModel owns the Projects tab : table, create-inline form
// (textinput, opened via `n`), and a confirm-delete modal (`D`).
type projectsModel struct {
	theme   Theme
	table   table.Model
	rows    []projectRow
	loading bool
	err     error

	// Create form. Active when input.Focused().
	creating bool
	input    textinput.Model

	// Confirm-delete modal. UUID + Name held so the post-action status
	// line can reference the project by its display name.
	confirmDeleteUUID string
	confirmDeleteName string

	lastRefresh time.Time
}

func newProjectsModel(theme Theme) projectsModel {
	cols := []table.Column{
		{Title: "NAME", Width: 24},
		{Title: "UUID", Width: 10},
		{Title: "CREATED", Width: 20},
		{Title: "VMS", Width: 6},
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
	s.Selected = theme.SelectedRow
	tbl.SetStyles(s)

	in := textinput.New()
	in.Placeholder = "new project name"
	in.CharLimit = 64
	in.Width = 40

	return projectsModel{theme: theme, table: tbl, input: in, loading: true}
}

func (m *projectsModel) selected() (uuid, name string) {
	if len(m.rows) == 0 {
		return "", ""
	}
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.rows) {
		return "", ""
	}
	return m.rows[idx].UUID, m.rows[idx].Name
}

// applyProjects refreshes the in-memory rows + table. counts is a
// VM-count map keyed by project_uuid that the caller fetches in
// parallel ; nil = leave the previous counts intact (so a transient
// ListVMs error doesn't blank the column).
func (m *projectsModel) applyProjects(resp *weftv1.ListProjectsResponse, counts map[string]int) {
	rows := make([]projectRow, 0, len(resp.Projects))
	tableRows := make([]table.Row, 0, len(resp.Projects))
	// Keep prior counts when the caller passed nil.
	prior := make(map[string]int, len(m.rows))
	for _, r := range m.rows {
		prior[r.UUID] = r.VMCount
	}
	for _, p := range resp.Projects {
		r := projectRow{
			Name:    p.Name,
			UUID:    p.Uuid,
			VMCount: -1,
		}
		if p.CreatedAtUnixNs != 0 {
			r.Created = time.Unix(0, p.CreatedAtUnixNs)
		}
		switch {
		case counts != nil:
			if c, ok := counts[p.Uuid]; ok {
				r.VMCount = c
			}
		default:
			if c, ok := prior[p.Uuid]; ok {
				r.VMCount = c
			}
		}
		rows = append(rows, r)
		tableRows = append(tableRows, r.tableRow())
	}
	m.rows = rows
	m.table.SetRows(tableRows)
	m.loading = false
	m.err = nil
	m.lastRefresh = time.Now()
}

func (r projectRow) tableRow() table.Row {
	uuidShort := r.UUID
	if len(uuidShort) > 8 {
		uuidShort = uuidShort[:8]
	}
	created := "—"
	if !r.Created.IsZero() {
		created = r.Created.Format("2006-01-02 15:04:05")
	}
	count := "?"
	if r.VMCount >= 0 {
		count = fmt.Sprintf("%d", r.VMCount)
	}
	return table.Row{
		dashEmpty(r.Name),
		uuidShort,
		created,
		count,
	}
}

// View renders the body. Create-form and confirm-delete modals
// supersede the table (same pattern as the Hosts confirm modal).
func (m projectsModel) View(width int) string {
	if m.creating {
		title := m.theme.Title.Render("Create project")
		hint := m.theme.Faint.Render("Enter to confirm · Esc to cancel")
		body := m.input.View()
		box := m.theme.HelpBox.Render(title + "\n\n" + body + "\n\n" + hint)
		return lipgloss.Place(width, lipgloss.Height(box), lipgloss.Center, lipgloss.Top, box)
	}
	if m.confirmDeleteUUID != "" {
		body := fmt.Sprintf(
			"Delete project %s (%s) ?\n\nProjects with attached VMs cannot be removed.\n\n  y   confirm\n  n   cancel",
			m.confirmDeleteName, m.confirmDeleteUUID,
		)
		box := m.theme.ConfirmBox.Render(body)
		return lipgloss.Place(width, lipgloss.Height(box), lipgloss.Center, lipgloss.Top, box)
	}
	if m.loading && len(m.rows) == 0 {
		return m.theme.Faint.Render("  loading projects…")
	}
	if m.err != nil {
		return m.theme.StatusErr.Render("  error: " + m.err.Error())
	}
	if len(m.rows) == 0 {
		return m.theme.Faint.Render("  no projects. Press `n` to create one.")
	}
	return m.table.View()
}

// --- Cmd factories + messages ---

// projectsLoadedMsg carries the result of the project+vms double
// fetch. counts is keyed by project_uuid so applyProjects can join
// without re-iterating both slices.
type projectsLoadedMsg struct {
	resp   *weftv1.ListProjectsResponse
	counts map[string]int
	err    error
}

// projectActionMsg surfaces a create / delete outcome to the status
// bar. created is true only for a successful CreateProject — lets the
// status line distinguish "ok, made it" from "already exists".
type projectActionMsg struct {
	action  string
	name    string
	uuid    string
	created bool
	err     error
}

func loadProjectsCmd(client ProjectsClient) tea.Cmd {
	if client == nil {
		return func() tea.Msg { return projectsLoadedMsg{err: errNoClient} }
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := client.ListProjects(ctx, &weftv1.ListProjectsRequest{})
		if err != nil {
			return projectsLoadedMsg{err: err}
		}
		// Fan-out the VM-count side query. A transient ListVMs error
		// shouldn't blank the project list — pass counts=nil so the
		// model retains its prior counts.
		var counts map[string]int
		if vmResp, vmErr := client.ListVMs(ctx, &weftv1.ListVMsRequest{}); vmErr == nil {
			counts = make(map[string]int, len(resp.Projects))
			for _, p := range resp.Projects {
				counts[p.Uuid] = 0
			}
			for _, v := range vmResp.Vms {
				if v.ProjectUuid != "" {
					counts[v.ProjectUuid]++
				} else if v.Project != "" {
					// Resolve by display name as a fallback.
					for _, p := range resp.Projects {
						if p.Name == v.Project {
							counts[p.Uuid]++
							break
						}
					}
				}
			}
		}
		return projectsLoadedMsg{resp: resp, counts: counts}
	}
}

func createProjectCmd(client ProjectsClient, name string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return projectActionMsg{action: "create", name: name, err: errNoClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := client.CreateProject(ctx, &weftv1.CreateProjectRequest{Name: name})
		msg := projectActionMsg{action: "create", name: name, err: err}
		if err == nil && resp != nil {
			msg.created = resp.Created
			if resp.Project != nil {
				msg.uuid = resp.Project.Uuid
			}
		}
		return msg
	}
}

func deleteProjectCmd(client ProjectsClient, uuid, name string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return projectActionMsg{action: "delete", uuid: uuid, name: name, err: errNoClient}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := client.DeleteProject(ctx, &weftv1.DeleteProjectRequest{Uuid: uuid})
		return projectActionMsg{action: "delete", uuid: uuid, name: name, err: err}
	}
}
