package main

import (
	"context"
	"errors"
	"io"
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
