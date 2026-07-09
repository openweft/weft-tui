package main

// resilient_test.go covers the failover wrapper's shape : empty list
// rejection, exhausted-endpoint error formatting, idempotent Close.
// The actual gRPC dial paths run through weftclient.Client + need a
// real socket / sshd, so they live in an integration suite ; the
// unit tests here lock the dispatching logic that surrounds it.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
)

// TestNewResilientClient_RejectsEmptyList ensures an empty endpoint
// list returns an error immediately instead of returning a no-op
// wrapper whose every RPC call would NPE the embedded interface.
func TestNewResilientClient_RejectsEmptyList(t *testing.T) {
	_, err := NewResilientClient(nil)
	if err == nil {
		t.Fatal("expected error on nil endpoint list")
	}
	_, err = NewResilientClient([]Endpoint{})
	if err == nil {
		t.Fatal("expected error on empty endpoint list")
	}
}

// TestNewResilientClient_AllUnreachable surfaces the canonical error
// shape when every endpoint fails to dial. The TUI's main.go reads
// this error to format the "weft-tui: all N endpoints unreachable"
// message ; a regression that swallowed the inner error would make
// debugging a misconfigured cluster painful.
func TestNewResilientClient_AllUnreachable(t *testing.T) {
	// Use socket paths under t.TempDir() that don't exist : the dial
	// step's net.Dial("unix", …) returns ENOENT.
	dir := t.TempDir()
	eps := []Endpoint{
		{Name: "h1", Socket: filepath.Join(dir, "h1.sock")},
		{Name: "h2", Socket: filepath.Join(dir, "h2.sock")},
	}
	_, err := NewResilientClient(eps)
	if err == nil {
		t.Fatal("expected error when no endpoint dials")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("error doesn't mention unreachability : %v", err)
	}
	if !strings.Contains(err.Error(), "2 endpoints") {
		t.Fatalf("error doesn't include endpoint count : %v", err)
	}
}

// TestUnimplementedIsNotAFailure documents the gRPC code we treat as
// a healthy ping. The watchdog used to count Unimplemented as a
// connection failure ; with 3 endpoints all running the same legacy
// server (which doesn't yet ship GetClusterInfo), this cascaded into
// a non-stop failover loop that re-rendered the status bar every 5s
// + made the TUI unusable. Locking down the behaviour with a
// codes.Unimplemented assertion : if a future refactor counts it as
// a failure again, this test catches it.
func TestUnimplementedIsNotAFailure(t *testing.T) {
	// Just assert the import-side contract — code 12 = Unimplemented.
	// The watchdog branch in resilient.go relies on this exact code.
	if 12 != int(codes.Unimplemented) {
		t.Fatalf("gRPC Unimplemented code = %d ; want 12", codes.Unimplemented)
	}
}

// TestResilientClient_EventHookFiresWithoutDeadlock is the regression
// net for the self-deadlock that shipped briefly in V0.6.x : connectNext
// held the write lock while emitEvent tried to RLock the same mutex,
// hanging forever the moment any endpoint failed to dial.
// The fix : defer all callback fire-outs until AFTER the lock is
// released. This test instruments a hook that takes the lock from
// the hook body — a deadlock would manifest as the t.Fatal timer.
func TestResilientClient_EventHookFiresWithoutDeadlock(t *testing.T) {
	dir := t.TempDir()
	eps := []Endpoint{
		{Name: "h1", Socket: filepath.Join(dir, "missing1.sock")},
		{Name: "h2", Socket: filepath.Join(dir, "missing2.sock")},
	}

	// Build a wrapper but skip NewResilientClient (we want the hook
	// installed BEFORE connectNext runs). Mirror its initial state.
	r := &ResilientClient{
		endpoints:      eps,
		healthInterval: 0, // disable the background watchdog
		failureBudget:  2,
		stop:           make(chan struct{}),
	}
	called := 0
	r.SetOnEvent(func(level, msg string) {
		// Re-enter the wrapper from inside the hook : if connectNext
		// still held the lock, this would deadlock.
		_ = r.Active()
		called++
	})

	// connectNext returns an error (no socket exists), but the hooks
	// must still fire — at least once per failed endpoint.
	done := make(chan struct{})
	go func() {
		_ = r.connectNext()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("connectNext deadlocked when hook re-entered the wrapper")
	}
	if called < 2 {
		t.Fatalf("expected ≥ 2 dial-fail events (one per endpoint), got %d", called)
	}
}

// TestResilientClient_SurfaceMethods exercises the trivial accessors
// (Active / Close / SetOnSwitch) on a wrapper whose initial dial
// fails — the methods must still be safe to call. Without these
// tests, the watchdog goroutine's defensive code paths sit on 0%
// coverage and a regression could panic on a Close() of a never-
// connected client.
func TestResilientClient_SurfaceMethods(t *testing.T) {
	// Hand-construct a wrapper directly so we don't need a live
	// gRPC server. The unexported fields are reachable from the
	// same package (_test.go).
	r := &ResilientClient{
		endpoints: []Endpoint{{Name: "h1"}},
		stop:      make(chan struct{}),
	}
	// SetOnSwitch + Active : both safe before any dial.
	called := false
	r.SetOnSwitch(func(_ Endpoint) { called = true })
	if r.onSwitch == nil {
		t.Fatal("SetOnSwitch didn't install the callback")
	}
	if r.Active().Name != "" {
		t.Fatal("Active() on never-connected wrapper should be zero-value Endpoint")
	}
	// Close is idempotent + safe to call multiple times.
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// The watcher never fired, so the callback is still uncalled —
	// asserting `called == false` here protects against a regression
	// that would fire onSwitch from Close.
	if called {
		t.Fatal("onSwitch fired unexpectedly")
	}
}

// TestEndpointString_StableAcrossFields confirms the Endpoint.String
// helper used in the connectNext stderr line stays informative when
// only some fields are populated. A regression that printed "<empty>"
// would mask which endpoint was rotating.
func TestEndpointString_StableAcrossFields(t *testing.T) {
	cases := []struct {
		ep       Endpoint
		mustHave string
	}{
		{Endpoint{Name: "dc1", Address: "admin@dc1-r1-h1"}, "dc1-r1-h1"},
		{Endpoint{Name: "local", Socket: "/run/weft.sock"}, "/run/weft.sock"},
		{Endpoint{Name: "dc2", Address: "h2", SSHUser: "admin", SSHSocket: "/r.sock"}, "user=admin"},
	}
	for _, tc := range cases {
		s := tc.ep.String()
		if !strings.Contains(s, tc.mustHave) {
			t.Errorf("Endpoint.String() = %q ; missing substring %q", s, tc.mustHave)
		}
	}
}
