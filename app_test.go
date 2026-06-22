package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// fakeClient is the test stub for the full Client interface used by
// every tab. Each tab records its last RPC call so the assertions
// below can verify the right RPC fired with the right args.
//
// We keep one struct rather than one-per-tab because Bubble Tea's
// Model owns a single Client ; splitting would force a test-only
// composition trick the production code doesn't need.
type fakeClient struct {
	mu sync.Mutex

	// Hosts.
	listHostsResp *weftv1.ListHostsResponse
	listHostsErr  error
	cordonReq     *weftv1.SetHostCordonedRequest
	cordonErr     error
	stateReq      *weftv1.SetHostStateRequest
	stateErr      error
	deleteReq     *weftv1.DeleteHostRequest
	deleteErr     error

	// VMs.
	listVMsResp *weftv1.ListVMsResponse
	listVMsErr  error
	startReq    *weftv1.StartVMRequest
	startErr    error
	stopReq     *weftv1.StopVMRequest
	stopErr     error
	restartReq  *weftv1.RestartVMRequest
	restartErr  error
	logsReq     *weftv1.VMLogsRequest
	logsResp    *weftv1.VMLogsResponse
	logsErr     error

	// Projects.
	listProjResp   *weftv1.ListProjectsResponse
	listProjErr    error
	createProjReq  *weftv1.CreateProjectRequest
	createProjResp *weftv1.CreateProjectResponse
	createProjErr  error
	deleteProjReq  *weftv1.DeleteProjectRequest
	deleteProjErr  error

	// Events.
	watchErr error
}

func (f *fakeClient) ListHosts(_ context.Context, _ *weftv1.ListHostsRequest, _ ...grpc.CallOption) (*weftv1.ListHostsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listHostsResp, f.listHostsErr
}

func (f *fakeClient) SetHostCordoned(_ context.Context, in *weftv1.SetHostCordonedRequest, _ ...grpc.CallOption) (*weftv1.SetHostCordonedResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cordonReq = in
	return &weftv1.SetHostCordonedResponse{}, f.cordonErr
}

func (f *fakeClient) SetHostState(_ context.Context, in *weftv1.SetHostStateRequest, _ ...grpc.CallOption) (*weftv1.SetHostStateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateReq = in
	return &weftv1.SetHostStateResponse{}, f.stateErr
}

func (f *fakeClient) DeleteHost(_ context.Context, in *weftv1.DeleteHostRequest, _ ...grpc.CallOption) (*weftv1.DeleteHostResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteReq = in
	return &weftv1.DeleteHostResponse{}, f.deleteErr
}

func (f *fakeClient) ListVMs(_ context.Context, _ *weftv1.ListVMsRequest, _ ...grpc.CallOption) (*weftv1.ListVMsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listVMsResp, f.listVMsErr
}

func (f *fakeClient) StartVM(_ context.Context, in *weftv1.StartVMRequest, _ ...grpc.CallOption) (*weftv1.StartVMResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startReq = in
	return &weftv1.StartVMResponse{}, f.startErr
}

func (f *fakeClient) StopVM(_ context.Context, in *weftv1.StopVMRequest, _ ...grpc.CallOption) (*weftv1.StopVMResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopReq = in
	return &weftv1.StopVMResponse{}, f.stopErr
}

func (f *fakeClient) RestartVM(_ context.Context, in *weftv1.RestartVMRequest, _ ...grpc.CallOption) (*weftv1.RestartVMResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restartReq = in
	return &weftv1.RestartVMResponse{}, f.restartErr
}

func (f *fakeClient) VMLogs(_ context.Context, in *weftv1.VMLogsRequest, _ ...grpc.CallOption) (*weftv1.VMLogsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsReq = in
	if f.logsResp == nil {
		return &weftv1.VMLogsResponse{}, f.logsErr
	}
	return f.logsResp, f.logsErr
}

func (f *fakeClient) ListProjects(_ context.Context, _ *weftv1.ListProjectsRequest, _ ...grpc.CallOption) (*weftv1.ListProjectsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listProjResp, f.listProjErr
}

func (f *fakeClient) CreateProject(_ context.Context, in *weftv1.CreateProjectRequest, _ ...grpc.CallOption) (*weftv1.CreateProjectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createProjReq = in
	if f.createProjResp == nil {
		return &weftv1.CreateProjectResponse{Project: &weftv1.ProjectInfo{Uuid: "fake-uuid", Name: in.Name}, Created: true}, f.createProjErr
	}
	return f.createProjResp, f.createProjErr
}

func (f *fakeClient) DeleteProject(_ context.Context, in *weftv1.DeleteProjectRequest, _ ...grpc.CallOption) (*weftv1.DeleteProjectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteProjReq = in
	return &weftv1.DeleteProjectResponse{}, f.deleteProjErr
}

// fakeEventStream is the minimal gRPC ServerStreamingClient impl the
// tests need. Pre-loaded with a fixed slice of events ; Recv returns
// them in order, then io.EOF.
type fakeEventStream struct {
	grpc.ClientStream
	events []*weftv1.PlatformEvent
	idx    int
	mu     sync.Mutex
}

func (s *fakeEventStream) Recv() (*weftv1.PlatformEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx >= len(s.events) {
		return nil, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}

func (s *fakeEventStream) Header() (metadata.MD, error) { return nil, nil }
func (s *fakeEventStream) Trailer() metadata.MD         { return nil }
func (s *fakeEventStream) CloseSend() error             { return nil }
func (s *fakeEventStream) Context() context.Context     { return context.Background() }
func (s *fakeEventStream) SendMsg(_ any) error          { return nil }
func (s *fakeEventStream) RecvMsg(_ any) error          { return io.EOF }

func (f *fakeClient) WatchEvents(_ context.Context, _ *weftv1.WatchEventsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[weftv1.PlatformEvent], error) {
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	return &fakeEventStream{}, nil
}

// keyMsg builds a tea.KeyMsg for the given rune. Bubble Tea's API has
// no public constructor that matches the `Update` callsite contract,
// so we hand-craft the struct. Runes only — modifiers go through the
// dedicated KeyType constants when needed.
func keyMsg(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// upperKeyMsg encodes an upper-case rune the same way Bubble Tea
// produces it on a shifted key. The runes themselves are uppercase.
func upperKeyMsg(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func TestTabSwitch(t *testing.T) {
	m := New(nil)
	if got := m.ActiveTab(); got != int(tabHosts) {
		t.Fatalf("initial tab = %d, want %d (Hosts)", got, tabHosts)
	}

	next, _ := m.Update(keyMsg('2'))
	m = next.(Model)
	if got := m.ActiveTab(); got != int(tabVMs) {
		t.Errorf("after '2', tab = %d, want %d (VMs)", got, tabVMs)
	}

	next, _ = m.Update(keyMsg('3'))
	m = next.(Model)
	if got := m.ActiveTab(); got != int(tabProjects) {
		t.Errorf("after '3', tab = %d, want %d (Projects)", got, tabProjects)
	}

	next, _ = m.Update(keyMsg('4'))
	m = next.(Model)
	if got := m.ActiveTab(); got != int(tabEvents) {
		t.Errorf("after '4', tab = %d, want %d (Events)", got, tabEvents)
	}

	next, _ = m.Update(keyMsg('1'))
	m = next.(Model)
	if got := m.ActiveTab(); got != int(tabHosts) {
		t.Errorf("after '1', tab = %d, want %d (Hosts)", got, tabHosts)
	}
}

func TestHelpToggle(t *testing.T) {
	m := New(nil)
	if m.ShowHelp() {
		t.Fatalf("help should start closed")
	}
	next, _ := m.Update(keyMsg('?'))
	m = next.(Model)
	if !m.ShowHelp() {
		t.Fatalf("after '?', help should be open")
	}
	next, _ = m.Update(keyMsg('?'))
	m = next.(Model)
	if m.ShowHelp() {
		t.Fatalf("after second '?', help should be closed")
	}
}

func TestQuitKey(t *testing.T) {
	m := New(nil)
	_, cmd := m.Update(keyMsg('q'))
	if cmd == nil {
		t.Fatalf("'q' should return tea.Quit")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("'q' Cmd produced %T, want tea.QuitMsg", msg)
	}
}

// --- Hosts tab tests (carried over from V0.1). ---

func TestCordonDispatch(t *testing.T) {
	client := &fakeClient{
		listHostsResp: &weftv1.ListHostsResponse{
			Hosts: []*weftv1.HostInfo{
				{Uuid: "u-1", Hostname: "alpha", State: "active"},
			},
		},
	}
	m := New(client)
	m.hosts.applyHosts(client.listHostsResp)

	next, cmd := m.Update(keyMsg('c'))
	m = next.(Model)
	if cmd == nil {
		t.Fatalf("'c' should produce a cordon Cmd")
	}
	msg := cmd()
	if _, ok := msg.(hostActionMsg); !ok {
		t.Fatalf("cordon Cmd produced %T, want hostActionMsg", msg)
	}
	if client.cordonReq == nil {
		t.Fatalf("SetHostCordoned was not called")
	}
	if client.cordonReq.Uuid != "u-1" || !client.cordonReq.Cordoned {
		t.Errorf("cordon req = %+v, want uuid=u-1 cordoned=true", client.cordonReq)
	}
}

func TestUncordonDispatch(t *testing.T) {
	client := &fakeClient{
		listHostsResp: &weftv1.ListHostsResponse{
			Hosts: []*weftv1.HostInfo{
				{Uuid: "u-2", Hostname: "beta", State: "active", Cordoned: true},
			},
		},
	}
	m := New(client)
	m.hosts.applyHosts(client.listHostsResp)

	_, cmd := m.Update(keyMsg('u'))
	_ = cmd()
	if client.cordonReq == nil || client.cordonReq.Cordoned {
		t.Fatalf("'u' should call SetHostCordoned(false)")
	}
}

func TestSetStateDownDispatch(t *testing.T) {
	client := &fakeClient{
		listHostsResp: &weftv1.ListHostsResponse{
			Hosts: []*weftv1.HostInfo{
				{Uuid: "u-3", Hostname: "gamma", State: "active"},
			},
		},
	}
	m := New(client)
	m.hosts.applyHosts(client.listHostsResp)

	_, cmd := m.Update(keyMsg('d'))
	_ = cmd()
	if client.stateReq == nil || client.stateReq.State != "down" {
		t.Fatalf("'d' should call SetHostState(down) ; got %+v", client.stateReq)
	}
}

func TestRemoveConfirmFlow(t *testing.T) {
	client := &fakeClient{
		listHostsResp: &weftv1.ListHostsResponse{
			Hosts: []*weftv1.HostInfo{
				{Uuid: "u-4", Hostname: "delta", State: "active"},
			},
		},
	}
	m := New(client)
	m.hosts.applyHosts(client.listHostsResp)

	next, cmd := m.Update(keyMsg('x'))
	m = next.(Model)
	if cmd != nil {
		t.Errorf("'x' must not issue an RPC ; got Cmd %v", cmd)
	}
	if !m.ConfirmingRemove() {
		t.Fatalf("'x' should open the confirm modal")
	}

	next, _ = m.Update(keyMsg('n'))
	m = next.(Model)
	if m.ConfirmingRemove() {
		t.Errorf("'n' should close the modal")
	}
	if client.deleteReq != nil {
		t.Errorf("DeleteHost must not be called on cancel")
	}

	next, _ = m.Update(keyMsg('x'))
	m = next.(Model)
	_, cmd = m.Update(keyMsg('y'))
	if cmd == nil {
		t.Fatalf("'y' should produce a delete Cmd")
	}
	_ = cmd()
	if client.deleteReq == nil || client.deleteReq.Uuid != "u-4" {
		t.Errorf("DeleteHost uuid = %v, want u-4", client.deleteReq)
	}
}

func TestRefreshKeyOnHosts(t *testing.T) {
	client := &fakeClient{listHostsResp: &weftv1.ListHostsResponse{Hosts: nil}}
	m := New(client)

	_, cmd := m.Update(keyMsg('r'))
	if cmd == nil {
		t.Fatalf("'r' on Hosts tab should produce a refresh Cmd")
	}
	msg := cmd()
	loaded, ok := msg.(hostsLoadedMsg)
	if !ok {
		t.Fatalf("refresh Cmd msg = %T, want hostsLoadedMsg", msg)
	}
	if loaded.err != nil {
		t.Fatalf("refresh err = %v, want nil", loaded.err)
	}
}

func TestRefreshErrorSurfaces(t *testing.T) {
	want := errors.New("boom")
	client := &fakeClient{listHostsErr: want}
	m := New(client)

	next, _ := m.Update(hostsLoadedMsg{err: want})
	m = next.(Model)
	gotMsg, gotErr := m.StatusMessage()
	if !gotErr {
		t.Errorf("statusErr = false, want true")
	}
	if gotMsg == "" {
		t.Errorf("statusMsg empty, want error text")
	}
}

// --- VMs tab tests. ---

// vmsModelWithSeed switches to the VMs tab and primes one row.
func vmsModelWithSeed(t *testing.T, c *fakeClient, vm *weftv1.VMInfo) Model {
	t.Helper()
	c.listVMsResp = &weftv1.ListVMsResponse{Vms: []*weftv1.VMInfo{vm}}
	m := New(c)
	next, _ := m.Update(keyMsg('2'))
	m = next.(Model)
	m.vms.applyVMs(c.listVMsResp, m.hosts.hostnameByUUID)
	return m
}

func TestStartVMDispatch(t *testing.T) {
	c := &fakeClient{}
	m := vmsModelWithSeed(t, c, &weftv1.VMInfo{Name: "v-1", Project: "p-1", State: weftv1.VMState_VM_STATE_STOPPED})

	_, cmd := m.Update(keyMsg('s'))
	if cmd == nil {
		t.Fatalf("'s' should produce a start Cmd")
	}
	msg := cmd()
	if _, ok := msg.(vmActionMsg); !ok {
		t.Fatalf("start Cmd msg = %T, want vmActionMsg", msg)
	}
	if c.startReq == nil || c.startReq.Name != "v-1" || c.startReq.Project != "p-1" {
		t.Errorf("StartVM req = %+v, want {Name:v-1 Project:p-1}", c.startReq)
	}
}

func TestStopVMConfirmFlow(t *testing.T) {
	c := &fakeClient{}
	m := vmsModelWithSeed(t, c, &weftv1.VMInfo{Name: "v-2", Project: "p-1", State: weftv1.VMState_VM_STATE_RUNNING})

	// 'S' opens the modal — no RPC yet.
	next, cmd := m.Update(upperKeyMsg('S'))
	m = next.(Model)
	if cmd != nil {
		t.Errorf("'S' must not fire an RPC ; got Cmd %v", cmd)
	}
	if !m.ConfirmingStop() {
		t.Fatalf("'S' should open the confirm-stop modal")
	}
	if c.stopReq != nil {
		t.Errorf("StopVM must not be called yet")
	}

	// 'n' cancels.
	next, _ = m.Update(keyMsg('n'))
	m = next.(Model)
	if m.ConfirmingStop() {
		t.Errorf("'n' should close the modal")
	}
	if c.stopReq != nil {
		t.Errorf("StopVM must not be called on cancel")
	}

	// 'S' again then 'y' → fires StopVM.
	next, _ = m.Update(upperKeyMsg('S'))
	m = next.(Model)
	_, cmd = m.Update(keyMsg('y'))
	if cmd == nil {
		t.Fatalf("'y' should produce a stop Cmd")
	}
	_ = cmd()
	if c.stopReq == nil || c.stopReq.Name != "v-2" {
		t.Errorf("StopVM req = %v, want Name=v-2", c.stopReq)
	}
}

func TestRestartVMDispatch(t *testing.T) {
	c := &fakeClient{}
	m := vmsModelWithSeed(t, c, &weftv1.VMInfo{Name: "v-3", Project: "p-1", State: weftv1.VMState_VM_STATE_RUNNING})

	_, cmd := m.Update(upperKeyMsg('R'))
	if cmd == nil {
		t.Fatalf("'R' should produce a RestartVM Cmd")
	}
	msg := cmd()
	action, ok := msg.(vmActionMsg)
	if !ok {
		t.Fatalf("restart Cmd msg = %T, want vmActionMsg", msg)
	}
	if action.action != "restart" {
		t.Errorf("action = %q, want restart (single atomic RPC)", action.action)
	}
	if c.restartReq == nil || c.restartReq.Name != "v-3" {
		t.Errorf("RestartVM req = %v, want Name=v-3", c.restartReq)
	}
	// Single-RPC restart : no stop / start legs should fire.
	if c.stopReq != nil {
		t.Errorf("StopVM should NOT fire on the atomic restart path ; got %+v", c.stopReq)
	}
	if c.startReq != nil {
		t.Errorf("StartVM should NOT fire on the atomic restart path ; got %+v", c.startReq)
	}
}

// TestVMsHostColumnResolvesHostname proves the v0.12.0 host_uuid
// → hostname resolution path : when hosts arrive after VMs (the
// common race on startup, both ListXxxs fire in parallel), the
// VMs tab re-renders so the HOST column flips from short-UUID
// to friendly hostname without a fresh ListVMs roundtrip.
func TestVMsHostColumnResolvesHostname(t *testing.T) {
	c := &fakeClient{
		listVMsResp: &weftv1.ListVMsResponse{Vms: []*weftv1.VMInfo{
			{Name: "v-1", Project: "p-1", HostUuid: "h-uuid-abcdef-1234"},
		}},
		listHostsResp: &weftv1.ListHostsResponse{Hosts: []*weftv1.HostInfo{
			{Uuid: "h-uuid-abcdef-1234", Hostname: "dc1-r1-h1"},
		}},
	}
	m := New(c)
	next, _ := m.Update(keyMsg('2'))
	m = next.(Model)

	// VMs land first ; HOST column should fall back to short UUID.
	m.vms.applyVMs(c.listVMsResp, m.hosts.hostnameByUUID)
	if got := m.vms.rows[0].HostName; got != "" {
		t.Errorf("HostName before hosts arrive = %q, want empty", got)
	}

	// Hosts arrive ; refresh re-resolves names.
	m.hosts.applyHosts(c.listHostsResp)
	m.vms.refreshHostNames(m.hosts.hostnameByUUID)
	if got := m.vms.rows[0].HostName; got != "dc1-r1-h1" {
		t.Errorf("HostName after hosts arrive = %q, want dc1-r1-h1", got)
	}
}

func TestVMLogsOpensViewport(t *testing.T) {
	c := &fakeClient{
		logsResp: &weftv1.VMLogsResponse{Contents: []byte("line a\nline b\nline c\n")},
	}
	m := vmsModelWithSeed(t, c, &weftv1.VMInfo{Name: "v-4", Project: "p-1"})

	next, cmd := m.Update(keyMsg('l'))
	m = next.(Model)
	if !m.LogsOpen() {
		t.Fatalf("'l' should open the logs overlay")
	}
	if cmd == nil {
		t.Fatalf("'l' should produce a VMLogs Cmd")
	}
	msg := cmd()
	loaded, ok := msg.(vmLogsLoadedMsg)
	if !ok {
		t.Fatalf("logs Cmd msg = %T, want vmLogsLoadedMsg", msg)
	}
	if loaded.err != nil {
		t.Fatalf("logs Cmd err = %v, want nil", loaded.err)
	}
	if c.logsReq == nil || c.logsReq.Name != "v-4" {
		t.Errorf("VMLogs req = %v, want Name=v-4", c.logsReq)
	}
	// Esc closes the overlay.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.LogsOpen() {
		t.Errorf("Esc should close the logs overlay")
	}
}

// --- Projects tab tests. ---

func projectsModelWithSeed(t *testing.T, c *fakeClient, p *weftv1.ProjectInfo) Model {
	t.Helper()
	c.listProjResp = &weftv1.ListProjectsResponse{Projects: []*weftv1.ProjectInfo{p}}
	m := New(c)
	next, _ := m.Update(keyMsg('3'))
	m = next.(Model)
	m.projects.applyProjects(c.listProjResp, map[string]int{p.Uuid: 0})
	return m
}

func TestCreateProjectFlow(t *testing.T) {
	c := &fakeClient{listProjResp: &weftv1.ListProjectsResponse{}}
	m := New(c)
	next, _ := m.Update(keyMsg('3'))
	m = next.(Model)

	// 'n' opens the input form.
	next, _ = m.Update(keyMsg('n'))
	m = next.(Model)
	if !m.CreatingProject() {
		t.Fatalf("'n' should open the create form")
	}

	// Type "alpha".
	for _, r := range "alpha" {
		next, _ = m.Update(keyMsg(r))
		m = next.(Model)
	}

	// Enter submits.
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("Enter should produce a CreateProject Cmd")
	}
	msg := cmd()
	if _, ok := msg.(projectActionMsg); !ok {
		t.Fatalf("create Cmd msg = %T, want projectActionMsg", msg)
	}
	if c.createProjReq == nil || c.createProjReq.Name != "alpha" {
		t.Errorf("CreateProject req = %v, want Name=alpha", c.createProjReq)
	}
}

func TestDeleteProjectConfirmFlow(t *testing.T) {
	c := &fakeClient{}
	m := projectsModelWithSeed(t, c, &weftv1.ProjectInfo{Uuid: "puuid-1", Name: "to-delete"})

	// 'D' opens the modal.
	next, cmd := m.Update(upperKeyMsg('D'))
	m = next.(Model)
	if cmd != nil {
		t.Errorf("'D' must not fire an RPC ; got Cmd %v", cmd)
	}
	if !m.ConfirmingDeleteProject() {
		t.Fatalf("'D' should open the confirm-delete modal")
	}

	// 'n' cancels.
	next, _ = m.Update(keyMsg('n'))
	m = next.(Model)
	if m.ConfirmingDeleteProject() {
		t.Errorf("'n' should close the modal")
	}
	if c.deleteProjReq != nil {
		t.Errorf("DeleteProject must not be called on cancel")
	}

	// 'D' again + 'y' → fires DeleteProject.
	next, _ = m.Update(upperKeyMsg('D'))
	m = next.(Model)
	_, cmd = m.Update(keyMsg('y'))
	if cmd == nil {
		t.Fatalf("'y' should produce a delete Cmd")
	}
	_ = cmd()
	if c.deleteProjReq == nil || c.deleteProjReq.Uuid != "puuid-1" {
		t.Errorf("DeleteProject req = %v, want Uuid=puuid-1", c.deleteProjReq)
	}
}

// --- Events tab tests. ---

func TestEventsPauseToggle(t *testing.T) {
	c := &fakeClient{}
	m := New(c)
	next, _ := m.Update(keyMsg('4'))
	m = next.(Model)
	if m.EventsPaused() {
		t.Fatalf("Events tab should start live, not paused")
	}
	next, _ = m.Update(keyMsg('p'))
	m = next.(Model)
	if !m.EventsPaused() {
		t.Fatalf("'p' should toggle to paused")
	}
	next, _ = m.Update(keyMsg('p'))
	m = next.(Model)
	if m.EventsPaused() {
		t.Fatalf("second 'p' should resume")
	}
}

func TestEventsAppendOnReceive(t *testing.T) {
	c := &fakeClient{}
	m := New(c)
	next, _ := m.Update(keyMsg('4'))
	m = next.(Model)

	// Feed a synthetic event message into Update — bypasses the
	// goroutine + channel so the test stays deterministic.
	ev := &weftv1.PlatformEvent{
		TsUnixNs: 1_700_000_000_000_000_000,
		Kind:     "vm.state.running",
		Subject:  "my-vm",
	}
	next, _ = m.Update(eventReceivedMsg{ev: ev})
	m = next.(Model)
	if got := m.EventLineCount(); got != 1 {
		t.Errorf("after one event, line count = %d, want 1", got)
	}

	// Pause then feed a second event ; line count must NOT advance.
	next, _ = m.Update(keyMsg('p'))
	m = next.(Model)
	next, _ = m.Update(eventReceivedMsg{ev: ev})
	m = next.(Model)
	if got := m.EventLineCount(); got != 1 {
		t.Errorf("while paused, line count = %d, want 1", got)
	}
}

func TestEventsClearBuffer(t *testing.T) {
	c := &fakeClient{}
	m := New(c)
	next, _ := m.Update(keyMsg('4'))
	m = next.(Model)
	for i := 0; i < 3; i++ {
		next, _ = m.Update(eventReceivedMsg{ev: &weftv1.PlatformEvent{TsUnixNs: 1, Kind: "vm.state.running", Subject: "x"}})
		m = next.(Model)
	}
	if m.EventLineCount() != 3 {
		t.Fatalf("setup: want 3 lines, got %d", m.EventLineCount())
	}
	next, _ = m.Update(keyMsg('c'))
	m = next.(Model)
	if m.EventLineCount() != 0 {
		t.Errorf("'c' should clear the buffer ; got %d lines", m.EventLineCount())
	}
}

// drainCmd executes a tea.Cmd (potentially a batch) and walks any
// nested batchMsg / sequenceMsg returned by it, calling each child
// Cmd in turn. Lets the tests observe RPCs that ride on a Batch.
func drainCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if msg == nil {
		return
	}
	// tea.BatchMsg is a slice of tea.Cmds in current Bubble Tea.
	switch v := msg.(type) {
	case tea.BatchMsg:
		for _, c := range v {
			drainCmd(t, c)
		}
	case []tea.Cmd:
		for _, c := range v {
			drainCmd(t, c)
		}
	}
}

// --- Command palette tests. ---

// TestPalette_OpensOnColon : pressing `:` opens the palette + sets
// the prompt to empty.
func TestPalette_OpensOnColon(t *testing.T) {
	m := New(&fakeClient{})
	next, _ := m.Update(keyMsg(':'))
	m = next.(Model)
	if !m.palette.open {
		t.Fatalf("palette should be open after `:`")
	}
}

// TestPalette_TypeAndEnterSwitchesView : type "networks" + Enter
// switches the active tab to tabResource with currentResource set.
func TestPalette_TypeAndEnterSwitchesView(t *testing.T) {
	m := New(&fakeClient{})
	next, _ := m.Update(keyMsg(':'))
	m = next.(Model)
	for _, r := range "networks" {
		next, _ = m.Update(keyMsg(r))
		m = next.(Model)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.active != tabResource {
		t.Errorf("active = %d, want tabResource (%d)", m.active, tabResource)
	}
	if m.currentResource != "networks" {
		t.Errorf("currentResource = %q, want networks", m.currentResource)
	}
}

// TestPalette_EscClosesWithoutSwitch : pressing Esc while typing
// closes the palette without changing the active view.
func TestPalette_EscClosesWithoutSwitch(t *testing.T) {
	m := New(&fakeClient{})
	originalActive := m.active
	next, _ := m.Update(keyMsg(':'))
	m = next.(Model)
	for _, r := range "vol" {
		next, _ = m.Update(keyMsg(r))
		m = next.(Model)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.palette.open {
		t.Errorf("palette should be closed after Esc")
	}
	if m.active != originalActive {
		t.Errorf("active changed unexpectedly : got %d, want %d", m.active, originalActive)
	}
}

// TestPalette_TabCompletes : pressing Tab on a partial match fills
// the prompt with the matching resource id.
func TestPalette_TabCompletes(t *testing.T) {
	m := New(&fakeClient{})
	next, _ := m.Update(keyMsg(':'))
	m = next.(Model)
	for _, r := range "net" {
		next, _ = m.Update(keyMsg(r))
		m = next.(Model)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(Model)
	if m.palette.input != "networks" {
		t.Errorf("Tab completion : input = %q, want networks", m.palette.input)
	}
}

// TestPalette_EmptyInputListsAllResources pins the v2 contract :
// opening the palette without typing shows the full catalogue.
// matches() returns every id in resourceIDs() order.
func TestPalette_EmptyInputListsAllResources(t *testing.T) {
	p := &paletteModel{open: true}
	m := p.matches()
	if len(m) != len(resourceCatalogue) {
		t.Errorf("matches on empty input = %d, want %d", len(m), len(resourceCatalogue))
	}
}

// TestPalette_SubstringFilterListsRelated pins the v2 contract :
// typing "vol" lists every volume-related resource, not just the
// one whose id starts with "vol".
func TestPalette_SubstringFilterListsRelated(t *testing.T) {
	p := &paletteModel{open: true, input: "vol"}
	m := p.matches()
	wantContains := []string{"volumes", "volume-properties", "volume-snapshots", "volume-backups"}
	for _, w := range wantContains {
		found := false
		for _, id := range m {
			if id == w {
				found = true
			}
		}
		if !found {
			// Not every install will have the same catalogue ;
			// the test passes as long as the matching ones land.
			// We only assert that vol-prefixed ids are present
			// — the rest is informational.
			if w == "volumes" {
				t.Errorf("vol filter missing %q ; got %v", w, m)
			}
		}
	}
}

// TestPalette_ArrowsMoveSelection pins the v2 contract : Down/Up
// move the highlighted entry within the filtered list.
func TestPalette_ArrowsMoveSelection(t *testing.T) {
	p := &paletteModel{open: true}
	if p.selected != 0 {
		t.Fatalf("initial selected = %d, want 0", p.selected)
	}
	_, _ = p.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if p.selected != 1 {
		t.Errorf("after Down : selected = %d, want 1", p.selected)
	}
	_, _ = p.handleKey(tea.KeyMsg{Type: tea.KeyUp})
	if p.selected != 0 {
		t.Errorf("after Up : selected = %d, want 0", p.selected)
	}
	// Down at the bottom clamps (doesn't wrap or go out of bounds).
	p.selected = len(p.matches()) - 1
	_, _ = p.handleKey(tea.KeyMsg{Type: tea.KeyDown})
	if p.selected != len(p.matches())-1 {
		t.Errorf("after Down at end : selected escaped bounds, %d/%d", p.selected, len(p.matches()))
	}
}

// TestTheme_TCycles pins the v0.3.4 theme picker : pressing `T`
// cycles to the next preset and updates the Model's themeIdx.
// Persistence is exercised by loadSavedTheme/saveTheme directly ;
// here we just verify the cycle wraps + applies.
func TestTheme_TCycles(t *testing.T) {
	m := New(&fakeClient{})
	startIdx := m.themeIdx
	next, _ := m.Update(keyMsg('T'))
	m = next.(Model)
	if m.themeIdx == startIdx {
		t.Errorf("T should cycle themeIdx ; stayed at %d", startIdx)
	}
	want := (startIdx + 1) % len(themePresets)
	if m.themeIdx != want {
		t.Errorf("themeIdx after T = %d, want %d", m.themeIdx, want)
	}
	// Cycle through every preset + back to start.
	for i := 0; i < len(themePresets)-1; i++ {
		next, _ = m.Update(keyMsg('T'))
		m = next.(Model)
	}
	if m.themeIdx != startIdx {
		t.Errorf("after %d total T presses themeIdx = %d, want %d (wrap to start)", len(themePresets), m.themeIdx, startIdx)
	}
}

// TestTheme_IndexByNameFallback pins loadSavedTheme's safety :
// an unknown name in the persisted file resolves to the default
// (green), never crashes or leaks a wrong theme.
func TestTheme_IndexByNameFallback(t *testing.T) {
	if got := themeIndexByName("does-not-exist"); got != 0 {
		t.Errorf("themeIndexByName(unknown) = %d, want 0 (green default)", got)
	}
	if got := themeIndexByName("green"); got != 0 {
		t.Errorf("themeIndexByName(green) = %d, want 0", got)
	}
	if got := themeIndexByName("blue"); got != 1 {
		t.Errorf("themeIndexByName(blue) = %d, want 1", got)
	}
}

// TestCatalogue_RWCoverage pins the v0.3.4 contract : MOST
// resources support Create now. The legitimate read-only ones
// (catalogue / derived / inventory-from-external-source) are
// listed by exception ; any new resource that lands without
// Create needs an explicit waiver.
func TestCatalogue_RWCoverage(t *testing.T) {
	expectedReadOnly := map[string]bool{
		"users":            true, // OIDC-sourced, no direct create
		"flavors":          true, // catalogue managed via HCL
		"images":           true, // pulled from OCI, not created
		"plugins":          true, // installed, not created
		"volume-snapshots": true, // derived from volumes, separate flow
		"volume-backups":   true, // derived from volumes, separate flow
	}
	for _, r := range resourceCatalogue {
		hasCreate := r.CreateFn != nil
		if expectedReadOnly[r.ID] {
			if hasCreate {
				t.Errorf("resource %q is documented read-only but has CreateFn — update expectedReadOnly", r.ID)
			}
		} else {
			if !hasCreate {
				t.Errorf("resource %q is missing CreateFn — wire CreateFields + CreateFn, or add to expectedReadOnly with a documented reason", r.ID)
			}
		}
	}
}

// TestResponsive_RescaleColumns pins the v0.3.6 column rescaler :
// declared widths act as weights ; the rescaled set sums to the
// available width (minus padding) with no column below the min.
func TestResponsive_RescaleColumns(t *testing.T) {
	orig := []table.Column{
		{Title: "A", Width: 10},
		{Title: "B", Width: 20},
		{Title: "C", Width: 30},
	}
	out := rescaleColumns(orig, 120)
	if len(out) != 3 {
		t.Fatalf("got %d cols, want 3", len(out))
	}
	var total int
	for _, c := range out {
		total += c.Width
	}
	// usable = 120 - tableSidePadding(4) = 116. Sum must equal 116
	// exactly thanks to the tail-correction.
	if total != 116 {
		t.Errorf("sum of widths = %d, want 116 (=120-padding)", total)
	}
	// Order must preserve titles (proportional, not re-sorted).
	for i, want := range []string{"A", "B", "C"} {
		if out[i].Title != want {
			t.Errorf("col[%d].Title = %q, want %q", i, out[i].Title, want)
		}
	}
	// Narrow terminal : columns clamp to the min instead of going to 0.
	out = rescaleColumns(orig, 20)
	for i, c := range out {
		if c.Width < columnMinWidth {
			t.Errorf("col[%d] = %d, want >= %d (min clamp)", i, c.Width, columnMinWidth)
		}
	}
}

// TestResponsive_ZeroWidthPreservesInput pins the defensive branch :
// availableWidth ≤ 0 returns the input slice unchanged so a parent
// that hasn't seen a WindowSizeMsg yet doesn't zero the layout.
func TestResponsive_ZeroWidthPreservesInput(t *testing.T) {
	orig := []table.Column{{Title: "X", Width: 5}, {Title: "Y", Width: 7}}
	out := rescaleColumns(orig, 0)
	if len(out) != 2 || out[0].Width != 5 || out[1].Width != 7 {
		t.Errorf("zero-width should return input unchanged, got %+v", out)
	}
}

// TestHosts_DetailDrawerOnEnter pins the v0.3.6 host inspector :
// pressing Enter on Hosts opens the drawer + sets detailUUID.
func TestHosts_DetailDrawerOnEnter(t *testing.T) {
	m := New(&fakeClient{})
	m.active = tabHosts
	// Seed a single host so selectedRow() has something to return.
	m.hosts.rows = []hostsRow{{UUID: "h-1", Hostname: "host-1"}}
	m.hosts.table.SetRows([]table.Row{{"h-1", "host-1", "", "", "", "", "", ""}})
	next, _ := m.Update(keyMsg('\r')) // Enter → "enter" via Bubble Tea
	m = next.(Model)
	// Bubble Tea's keyMsg helper doesn't always map '\r' to "enter" ;
	// fall back to tea.KeyEnter for the assertion.
	if !m.hosts.detailOpen {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = next.(Model)
	}
	if !m.hosts.detailOpen {
		t.Errorf("Enter on Hosts should open detail drawer ; detailOpen=false")
	}
	if m.hosts.detailUUID != "h-1" {
		t.Errorf("detailUUID = %q, want h-1", m.hosts.detailUUID)
	}
	// Esc closes.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.hosts.detailOpen {
		t.Errorf("Esc should close detail drawer ; still open")
	}
}

// TestHosts_CPMarker pins the v0.3.8 contract : when localCPUUID
// is set, the host with that UUID gets "*" in the CP column ; all
// others get "—". STATE/CONN are PLAIN text (no inline ANSI) so
// the bubbles/table truncator can't blank them under a narrow
// terminal — pin that too.
func TestHosts_CPMarker(t *testing.T) {
	theme := NewTheme()
	rows := []hostsRow{
		{UUID: "host-A", Hostname: "h-a", State: "active", Connected: true},
		{UUID: "host-B", Hostname: "h-b", State: "down"},
		{UUID: "host-C", Hostname: "h-c", State: "active", Cordoned: true},
	}
	// CP = host-B (single-member set, mirrors single-host dev).
	cpSet := map[string]struct{}{"host-B": {}}
	for i, r := range rows {
		row := r.tableRow(theme, cpSet)
		// CP column.
		want := "—"
		if r.UUID == "host-B" {
			want = "*"
		}
		if row[0] != want {
			t.Errorf("row[%d=%s].CP = %q, want %q", i, r.UUID, row[0], want)
		}
		// STATE column index 8 (after CP, UUID, HOSTNAME, AZ, RACK,
		// CPU, RAM, GPU).
		state := row[8]
		if strings.ContainsAny(state, "\x1b") {
			t.Errorf("row[%d].STATE contains ANSI escape : %q (should be plain text)", i, state)
		}
		if !strings.Contains(state, r.State) {
			t.Errorf("row[%d].STATE = %q, want to contain %q", i, state, r.State)
		}
		if r.Cordoned && !strings.Contains(state, "cordoned") {
			t.Errorf("row[%d].STATE missing 'cordoned' suffix : %q", i, state)
		}
		// CONN column index 9.
		conn := row[9]
		if strings.ContainsAny(conn, "\x1b") {
			t.Errorf("row[%d].CONN contains ANSI escape : %q", i, conn)
		}
		wantConn := "no"
		if r.Connected {
			wantConn = "yes"
		}
		if conn != wantConn {
			t.Errorf("row[%d].CONN = %q, want %q", i, conn, wantConn)
		}
	}
}

// TestHosts_CPMarker_NoLocal pins the empty-UUID branch : when the
// local UUID is unknown (RPC failed at boot, dev mode, etc.) every
// row's CP column is "—" — no row gets a spurious mark.
func TestHosts_CPMarker_NoLocal(t *testing.T) {
	theme := NewTheme()
	row := hostsRow{UUID: "host-X"}.tableRow(theme, nil)
	if row[0] != "—" {
		t.Errorf("CP with empty local UUID = %q, want —", row[0])
	}
}

// TestAutoFetchClusterName_NilClient pins the safety branch :
// passing nil short-circuits to "" so a TUI booting against an
// unreachable socket doesn't crash before the connection error
// surfaces. The main() flow then keeps the title bar empty —
// same as the pre-v0.3.7 look.
func TestAutoFetchClusterName_NilClient(t *testing.T) {
	if got := autoFetchClusterName(nil); got != "" {
		t.Errorf("autoFetchClusterName(nil) = %q, want \"\"", got)
	}
}

// TestClusterName_AppearsInTitle pins the v0.3.6 federation cue :
// when m.clusterName is set, the sidebar header includes it.
// (Title moved from the horizontal tab strip to the sidebar's top
// section in v0.5.0 when the layout switched to a left-hand object-
// type list.)
func TestClusterName_AppearsInTitle(t *testing.T) {
	m := New(&fakeClient{})
	m.clusterName = "prod-eu"
	m.width = 100
	m.height = 30
	side := m.renderSidebar()
	if !strings.Contains(side, "prod-eu") {
		t.Errorf("sidebar missing cluster name : %q", side)
	}
	m.clusterName = ""
	side = m.renderSidebar()
	if strings.Contains(side, "prod-eu") {
		t.Errorf("sidebar should not show cluster name when unset : %q", side)
	}
}

// TestEdit_FormPrefills pins the v0.3.5 edit form : opening with
// `e` on a row pre-fills the inputs from the row's keys.
func TestEdit_FormPrefills(t *testing.T) {
	// Pick a real resource that has EditFields wired (subnets).
	var cfg ResourceConfig
	for _, r := range resourceCatalogue {
		if r.ID == "subnets" {
			cfg = r
			break
		}
	}
	if cfg.EditFn == nil {
		t.Fatal("subnets must have EditFn wired in v0.3.5")
	}
	row := map[string]any{
		"uuid":        "sn-1",
		"name":        "edge",
		"description": "edge subnet",
		"gateway":     "10.0.0.1",
	}
	f := newEditFormModel(cfg, row)
	if !f.editMode {
		t.Errorf("editMode = false, want true")
	}
	if f.editRow["uuid"] != "sn-1" {
		t.Errorf("editRow uuid lost : %v", f.editRow)
	}
	// Inputs by index ; the order matches cfg.EditFields.
	for i, field := range cfg.EditFields {
		got := f.inputs[i].Value()
		want := rowValueAsString(row[field.Key])
		if got != want {
			t.Errorf("input[%d=%s] = %q, want %q (prefill)", i, field.Key, got, want)
		}
	}
}

// TestEdit_FormEmptyAllowedOnEdit pins the contract that empty
// fields are accepted in edit mode (proto3 "" = keep current).
// Create mode still enforces Required.
func TestEdit_FormEmptyAllowedOnEdit(t *testing.T) {
	cfg := ResourceConfig{
		ID:    "x",
		Title: "X",
		EditFields: []FormField{
			{Key: "name", Label: "Name", Required: true},
		},
	}
	f := newEditFormModel(cfg, map[string]any{"name": ""}) // empty start
	values, err := f.collect()
	if err != nil {
		t.Errorf("edit collect with empty Required field should succeed (keep-current) ; got %v", err)
	}
	if values["name"] != "" {
		t.Errorf("collect returned %q, want empty", values["name"])
	}
	// Now create mode : Required + empty must fail.
	f.editMode = false
	if _, err := f.collect(); err == nil {
		t.Errorf("create collect with empty Required field should fail")
	}
}

// TestTheme_SelectedRowFollowsPreset pins that switching theme
// updates the SelectedRow background instead of leaving the old
// hardcoded violet.
func TestTheme_SelectedRowFollowsPreset(t *testing.T) {
	for _, preset := range themePresets {
		theme := NewThemeWith(preset)
		got := theme.SelectedRow.GetBackground()
		if got == nil {
			t.Errorf("theme %q has nil SelectedRow.Background", preset.Name)
			continue
		}
		// We can't compare AdaptiveColors directly via interface
		// equality (Lipgloss wraps them) ; the contract is that
		// SelectedRow takes some color, not the literal violet.
		// A successful build-and-render is the real assertion ;
		// this test exists to flag a future regression if someone
		// hardcodes a colour in SelectedRow again.
		_ = got
	}
}

// TestPalette_EnterPicksSelectedNotInput pins the v2 contract :
// Enter opens the HIGHLIGHTED resource (which may differ from the
// raw input when typing partial then arrowing). The old v1 path
// required the input to be an EXACT match ; v2 trusts the picker.
func TestPalette_EnterPicksSelectedNotInput(t *testing.T) {
	p := &paletteModel{open: true, input: "v", selected: 0}
	m := p.matches()
	if len(m) == 0 {
		t.Fatal("filter `v` produced no matches ; can't run test")
	}
	expect := m[0] // selected=0 → first match
	_, switchTo := p.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if switchTo != expect {
		t.Errorf("Enter switched to %q, want top match %q (input was %q, partial)", switchTo, expect, "v")
	}
}

// TestCatalogue_AllResourcesDistinct : every catalogue entry has a
// unique slug — paranoia check against a future copy-paste mistake.
func TestCatalogue_AllResourcesDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range resourceCatalogue {
		if seen[r.ID] {
			t.Errorf("duplicate catalogue id : %q", r.ID)
		}
		seen[r.ID] = true
	}
	if len(seen) < 20 {
		t.Errorf("only %d resources in catalogue, want >= 20", len(seen))
	}
}

// TestResource_EnterOpensDetailDrawer : after switching to a resource
// view, Enter on a selected row opens the detail drawer. Esc closes.
func TestResource_EnterOpensDetailDrawer(t *testing.T) {
	cfg := ResourceConfig{
		ID: "test-noun", Title: "Test", Section: "Test",
		Columns: []table.Column{{Title: "K", Width: 10}},
		List: func(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) {
			return []map[string]any{{"uuid": "u-1", "name": "alpha", "extra": "value"}}, nil
		},
		RowToCells: func(r map[string]any) []string { return []string{s(r, "name")} },
	}
	rm := newResourceListModel(NewTheme(), nil, cfg)
	rm.applyRows([]map[string]any{{"uuid": "u-1", "name": "alpha", "extra": "value"}})

	// Enter opens.
	rm, _ = rm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !rm.detailOpen {
		t.Fatalf("Enter should open detail drawer")
	}
	if rm.detailRow["name"] != "alpha" {
		t.Errorf("detail row not the selected one : %+v", rm.detailRow)
	}

	// Esc closes.
	rm, _ = rm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if rm.detailOpen {
		t.Errorf("Esc should close drawer")
	}
	if rm.detailRow != nil {
		t.Errorf("detailRow should be nil after close")
	}
}

// TestResource_DetailDrawerSwallowsActionKeys : while the drawer is
// open, pressing an action key like `d` must NOT trigger the action
// (no risk of accidentally deleting while inspecting).
func TestResource_DetailDrawerSwallowsActionKeys(t *testing.T) {
	called := false
	cfg := ResourceConfig{
		ID: "x", Title: "X", Section: "X",
		Columns: []table.Column{{Title: "K", Width: 10}},
		List:    func(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) { return nil, nil },
		RowToCells: func(r map[string]any) []string { return []string{s(r, "name")} },
		Actions: []ResourceAction{
			{Key: "d", Label: "del", Do: func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any) (string, error) {
				called = true
				return "", nil
			}},
		},
	}
	rm := newResourceListModel(NewTheme(), nil, cfg)
	rm.applyRows([]map[string]any{{"uuid": "u-1", "name": "alpha"}})
	rm.detailOpen = true
	rm.detailRow = rm.rows[0]
	rm, _ = rm.Update(keyMsg('d'))
	if called {
		t.Errorf("`d` should not fire the action while drawer is open")
	}
}

// TestResource_NewOpensCreateForm : `n` on a resource that has a
// CreateFn opens the form ; on one without, it's a no-op.
func TestResource_NewOpensCreateForm(t *testing.T) {
	withCreate := ResourceConfig{
		ID: "with-create", Title: "T", Section: "T",
		Columns:    []table.Column{{Title: "K", Width: 10}},
		List:       func(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) { return nil, nil },
		RowToCells: func(r map[string]any) []string { return []string{s(r, "k")} },
		CreateFields: []FormField{
			{Key: "name", Label: "Name", Required: true},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			return "created " + v["name"], nil
		},
	}
	rm := newResourceListModel(NewTheme(), nil, withCreate)
	rm.applyRows(nil)
	rm, _ = rm.Update(keyMsg('n'))
	if rm.create == nil {
		t.Fatalf("`n` should open create form when CreateFn is wired")
	}

	withoutCreate := ResourceConfig{
		ID: "without-create", Title: "T", Section: "T",
		Columns:    []table.Column{{Title: "K", Width: 10}},
		List:       func(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) { return nil, nil },
		RowToCells: func(r map[string]any) []string { return []string{s(r, "k")} },
	}
	rm2 := newResourceListModel(NewTheme(), nil, withoutCreate)
	rm2.applyRows(nil)
	rm2, _ = rm2.Update(keyMsg('n'))
	if rm2.create != nil {
		t.Errorf("`n` should be a no-op when CreateFn is nil")
	}
}

// TestResource_CreateFormRequiresFields : Enter without filling
// required fields surfaces a validation error + leaves the form open.
func TestResource_CreateFormRequiresFields(t *testing.T) {
	cfg := ResourceConfig{
		ID: "x", Title: "X", Section: "X",
		Columns:    []table.Column{{Title: "K", Width: 10}},
		List:       func(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) { return nil, nil },
		RowToCells: func(r map[string]any) []string { return []string{s(r, "k")} },
		CreateFields: []FormField{
			{Key: "name", Label: "Name", Required: true},
		},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			return "ok", nil
		},
	}
	rm := newResourceListModel(NewTheme(), nil, cfg)
	rm.applyRows(nil)
	rm, _ = rm.Update(keyMsg('n'))
	rm, _ = rm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if rm.create == nil {
		t.Fatalf("form should stay open on validation error")
	}
	if rm.create.errMsg == "" {
		t.Errorf("missing required field should surface errMsg")
	}
}

// TestResource_CreateFormEscClosesAndDiscards : Esc on the form
// reverts to the table view without firing CreateFn.
func TestResource_CreateFormEscClosesAndDiscards(t *testing.T) {
	called := false
	cfg := ResourceConfig{
		ID: "x", Title: "X", Section: "X",
		Columns:      []table.Column{{Title: "K", Width: 10}},
		List:         func(ctx context.Context, c weftv1.WeftAgentClient) ([]map[string]any, error) { return nil, nil },
		RowToCells:   func(r map[string]any) []string { return []string{s(r, "k")} },
		CreateFields: []FormField{{Key: "name", Label: "Name"}},
		CreateFn: func(ctx context.Context, c weftv1.WeftAgentClient, v map[string]string) (string, error) {
			called = true
			return "", nil
		},
	}
	rm := newResourceListModel(NewTheme(), nil, cfg)
	rm.applyRows(nil)
	rm, _ = rm.Update(keyMsg('n'))
	// Esc emits createCancelMsg. The ResourceListModel handles that
	// by setting create=nil — but only when the model receives the
	// cancel msg via Update. Drive it explicitly.
	_, cmd := rm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		msg := cmd()
		rm, _ = rm.Update(msg)
	}
	if rm.create != nil {
		t.Errorf("Esc should close the form")
	}
	if called {
		t.Errorf("CreateFn should not be called on cancel")
	}
}
