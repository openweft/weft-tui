package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// fakeHostsClient is a HostsClient stub that records every call and
// returns canned responses. Lets the tests below assert state
// transitions without dialling a real socket.
type fakeHostsClient struct {
	mu sync.Mutex

	listResp *weftv1.ListHostsResponse
	listErr  error

	cordonReq *weftv1.SetHostCordonedRequest
	cordonErr error

	stateReq *weftv1.SetHostStateRequest
	stateErr error

	deleteReq *weftv1.DeleteHostRequest
	deleteErr error
}

func (f *fakeHostsClient) ListHosts(_ context.Context, _ *weftv1.ListHostsRequest, _ ...grpc.CallOption) (*weftv1.ListHostsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listResp, f.listErr
}

func (f *fakeHostsClient) SetHostCordoned(_ context.Context, in *weftv1.SetHostCordonedRequest, _ ...grpc.CallOption) (*weftv1.SetHostCordonedResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cordonReq = in
	return &weftv1.SetHostCordonedResponse{}, f.cordonErr
}

func (f *fakeHostsClient) SetHostState(_ context.Context, in *weftv1.SetHostStateRequest, _ ...grpc.CallOption) (*weftv1.SetHostStateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateReq = in
	return &weftv1.SetHostStateResponse{}, f.stateErr
}

func (f *fakeHostsClient) DeleteHost(_ context.Context, in *weftv1.DeleteHostRequest, _ ...grpc.CallOption) (*weftv1.DeleteHostResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteReq = in
	return &weftv1.DeleteHostResponse{}, f.deleteErr
}

// keyMsg builds a tea.KeyMsg for the given rune. Bubble Tea's API has
// no public constructor that matches the `Update` callsite contract,
// so we hand-craft the struct. Runes only — modifiers go through the
// dedicated KeyType constants when needed.
func keyMsg(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func TestTabSwitch(t *testing.T) {
	m := New(nil)
	if got := m.ActiveTab(); got != int(tabHosts) {
		t.Fatalf("initial tab = %d, want %d (Hosts)", got, tabHosts)
	}

	// Press 2 → VMs.
	next, _ := m.Update(keyMsg('2'))
	m = next.(Model)
	if got := m.ActiveTab(); got != int(tabVMs) {
		t.Errorf("after '2', tab = %d, want %d (VMs)", got, tabVMs)
	}

	// Press 3 → Projects.
	next, _ = m.Update(keyMsg('3'))
	m = next.(Model)
	if got := m.ActiveTab(); got != int(tabProjects) {
		t.Errorf("after '3', tab = %d, want %d (Projects)", got, tabProjects)
	}

	// Press 4 → Events.
	next, _ = m.Update(keyMsg('4'))
	m = next.(Model)
	if got := m.ActiveTab(); got != int(tabEvents) {
		t.Errorf("after '4', tab = %d, want %d (Events)", got, tabEvents)
	}

	// Press 1 → back to Hosts.
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
	// Tea.Quit is a function that returns tea.QuitMsg{} when called.
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("'q' Cmd produced %T, want tea.QuitMsg", msg)
	}
}

// TestCordonDispatch primes the model with a host then presses 'c' ;
// the resulting Cmd should invoke the fake client's SetHostCordoned.
func TestCordonDispatch(t *testing.T) {
	client := &fakeHostsClient{
		listResp: &weftv1.ListHostsResponse{
			Hosts: []*weftv1.HostInfo{
				{Uuid: "u-1", Hostname: "alpha", State: "active"},
			},
		},
	}
	m := New(client)
	// Inject the hosts directly so the table has a row to select.
	m.hosts.applyHosts(client.listResp)

	next, cmd := m.Update(keyMsg('c'))
	m = next.(Model)
	if cmd == nil {
		t.Fatalf("'c' should produce a cordon Cmd")
	}
	// Drain the Cmd : it runs the gRPC call.
	msg := cmd()
	if _, ok := msg.(hostActionMsg); !ok {
		t.Fatalf("cordon Cmd produced %T, want hostActionMsg", msg)
	}
	if client.cordonReq == nil {
		t.Fatalf("SetHostCordoned was not called")
	}
	if client.cordonReq.Uuid != "u-1" {
		t.Errorf("cordon uuid = %q, want u-1", client.cordonReq.Uuid)
	}
	if !client.cordonReq.Cordoned {
		t.Errorf("cordon flag should be true on 'c'")
	}
}

// TestUncordonDispatch mirrors TestCordonDispatch for the 'u' key.
func TestUncordonDispatch(t *testing.T) {
	client := &fakeHostsClient{
		listResp: &weftv1.ListHostsResponse{
			Hosts: []*weftv1.HostInfo{
				{Uuid: "u-2", Hostname: "beta", State: "active", Cordoned: true},
			},
		},
	}
	m := New(client)
	m.hosts.applyHosts(client.listResp)

	_, cmd := m.Update(keyMsg('u'))
	_ = cmd()
	if client.cordonReq == nil || client.cordonReq.Cordoned {
		t.Fatalf("'u' should call SetHostCordoned(false)")
	}
}

// TestSetStateDownDispatch validates the 'd' shortcut → SetHostState(down).
func TestSetStateDownDispatch(t *testing.T) {
	client := &fakeHostsClient{
		listResp: &weftv1.ListHostsResponse{
			Hosts: []*weftv1.HostInfo{
				{Uuid: "u-3", Hostname: "gamma", State: "active"},
			},
		},
	}
	m := New(client)
	m.hosts.applyHosts(client.listResp)

	_, cmd := m.Update(keyMsg('d'))
	_ = cmd()
	if client.stateReq == nil || client.stateReq.State != "down" {
		t.Fatalf("'d' should call SetHostState(down) ; got %+v", client.stateReq)
	}
}

// TestRemoveConfirmFlow: 'x' opens the confirm modal ; 'n' cancels ;
// 'y' calls DeleteHost. Validates the modal short-circuits all other
// keys and the destructive RPC fires only on explicit consent.
func TestRemoveConfirmFlow(t *testing.T) {
	client := &fakeHostsClient{
		listResp: &weftv1.ListHostsResponse{
			Hosts: []*weftv1.HostInfo{
				{Uuid: "u-4", Hostname: "delta", State: "active"},
			},
		},
	}
	m := New(client)
	m.hosts.applyHosts(client.listResp)

	// 'x' opens the modal — no RPC yet.
	next, cmd := m.Update(keyMsg('x'))
	m = next.(Model)
	if cmd != nil {
		t.Errorf("'x' must not issue an RPC ; got Cmd %v", cmd)
	}
	if !m.ConfirmingRemove() {
		t.Fatalf("'x' should open the confirm modal")
	}

	// 'n' cancels.
	next, _ = m.Update(keyMsg('n'))
	m = next.(Model)
	if m.ConfirmingRemove() {
		t.Errorf("'n' should close the modal")
	}
	if client.deleteReq != nil {
		t.Errorf("DeleteHost must not be called on cancel")
	}

	// 'x' again, then 'y' → fires DeleteHost.
	next, _ = m.Update(keyMsg('x'))
	m = next.(Model)
	if !m.ConfirmingRemove() {
		t.Fatalf("'x' (second time) should open the modal")
	}
	_, cmd = m.Update(keyMsg('y'))
	if cmd == nil {
		t.Fatalf("'y' should produce a delete Cmd")
	}
	_ = cmd()
	if client.deleteReq == nil || client.deleteReq.Uuid != "u-4" {
		t.Errorf("DeleteHost uuid = %v, want u-4", client.deleteReq)
	}
}

// TestRefreshKeyOnHosts: 'r' on the Hosts tab issues a list RPC.
func TestRefreshKeyOnHosts(t *testing.T) {
	client := &fakeHostsClient{
		listResp: &weftv1.ListHostsResponse{Hosts: nil},
	}
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

// TestRefreshErrorSurfaces: when ListHosts fails, the model exposes
// the error through the status bar.
func TestRefreshErrorSurfaces(t *testing.T) {
	want := errors.New("boom")
	client := &fakeHostsClient{listErr: want}
	m := New(client)

	// Simulate the message that would be produced by the Cmd.
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
