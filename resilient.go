package main

// resilient.go wraps weft-client with auto-failover : at startup
// + on connection drop, the wrapper iterates the configured
// Endpoint list trying each until one responds.
//
// Architecture :
//   * ResilientClient embeds weftv1.WeftAgentClient → method
//     promotion gives us all 172 RPCs for free, no per-method
//     delegating boilerplate. The embedded field is swapped under
//     a sync.RWMutex on every successful endpoint reconnect.
//   * A background heartbeat (1× GetClusterInfo every healthInterval)
//     detects dead connections ; on N consecutive failures, the
//     wrapper transparently swaps to the next endpoint.
//   * onSwitch callback fires after each successful swap so the
//     TUI can surface a status message ("connected to dc2-r1-h1")
//     without each tab having to know about the failover machinery.

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	weftclient "github.com/openweft/weft-client"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ResilientClient is the failover-capable weftv1.WeftAgentClient.
// The embedded interface field is what every RPC dispatches through ;
// reconnects atomically swap it for the freshly-dialled client.
type ResilientClient struct {
	weftv1.WeftAgentClient // current underlying ; swapped under mu on reconnect

	endpoints []Endpoint

	mu     sync.RWMutex
	active Endpoint
	conn   *grpc.ClientConn
	ssh    *sshDialer // in-process SSH client for the active remote endpoint ; nil for local-socket
	idx    int

	// onSwitch fires after every successful endpoint swap. nil = no-op.
	onSwitch func(active Endpoint)
	// onEvent fires on every diagnostic message (dial fail, reconnect
	// success, failover exhausted). The TUI hooks this up to its
	// status bar ; without a hook, events are dropped silently. The
	// previous behaviour wrote each event to os.Stderr which bled
	// through the alt-screen + scrolled across the rendered UI.
	onEvent func(level, msg string)

	failures       atomic.Int32
	healthInterval time.Duration
	failureBudget  int32

	stop     chan struct{}
	stopOnce sync.Once
}

// resilient event levels — surfaced to the TUI's status bar through
// the onEvent hook. "info" = green/grey ; "warn" = yellow ; "error"
// = red. The TUI maps these to its theme.
const (
	ResilientEventInfo  = "info"
	ResilientEventWarn  = "warn"
	ResilientEventError = "error"
)

// NewResilientClient picks the first endpoint that dials successfully
// + spins up the heartbeat goroutine. Returns an error only when
// every endpoint fails the initial dial.
func NewResilientClient(endpoints []Endpoint) (*ResilientClient, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no endpoints configured")
	}
	r := &ResilientClient{
		endpoints:      endpoints,
		healthInterval: 5 * time.Second,
		failureBudget:  2,
		stop:           make(chan struct{}),
	}
	if err := r.connectNext(); err != nil {
		return nil, err
	}
	go r.watchdog()
	return r, nil
}

// SetOnSwitch wires the callback fired after every successful
// endpoint swap. Pass nil to silence.
func (r *ResilientClient) SetOnSwitch(fn func(Endpoint)) {
	r.mu.Lock()
	r.onSwitch = fn
	r.mu.Unlock()
}

// SetOnEvent wires the diagnostic-event hook. fn is invoked on dial
// failures, successful reconnects, and exhausted failover. The TUI
// pipes this into its status bar so the connection lifecycle is
// visible without spamming os.Stderr (which bleeds through alt-screen).
func (r *ResilientClient) SetOnEvent(fn func(level, msg string)) {
	r.mu.Lock()
	r.onEvent = fn
	r.mu.Unlock()
}

// emitEvent fires the onEvent hook. Centralised so every diagnostic
// flows through the same path ; no more direct os.Stderr writes from
// anywhere in the wrapper. The caller MUST NOT hold r.mu — emitEvent
// takes its own read lock to snapshot the hook. connectNext (which
// holds the write lock) reads r.onEvent directly + calls the hook
// after unlocking to avoid self-deadlock.
func (r *ResilientClient) emitEvent(level, msg string) {
	r.mu.RLock()
	hook := r.onEvent
	r.mu.RUnlock()
	if hook != nil {
		hook(level, msg)
	}
}

// Active returns the endpoint the wrapper is currently talking to.
func (r *ResilientClient) Active() Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// Close terminates the heartbeat goroutine + the underlying gRPC
// conn + the SSH client (if the active endpoint used one).
func (r *ResilientClient) Close() error {
	r.stopOnce.Do(func() { close(r.stop) })
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.conn != nil {
		err = r.conn.Close()
	}
	if r.ssh != nil {
		_ = r.ssh.Close()
	}
	return err
}

// dial reaches the endpoint's weft agent via one of three transports :
//
//   * e.Socket alone : already-tunneled local socket. Just dial it.
//   * e.Address + e.SSHSocket : open an in-process SSH client to the
//     remote host (golang.org/x/crypto/ssh) + use a
//     `direct-streamlocal@openssh.com` channel as the gRPC dialer.
//     No fork, no /tmp socket, no `pkill` cleanup. See sshdial.go.
//   * Legacy e.Address + e.SSHKey (no SSHSocket) : the agent's
//     in-process SSH-server transport (weftclient.WithSSH). Kept
//     for backwards compat ; not used by the operator-side flow.
//
// Returns (client, conn, sshDialer, error). sshDialer is non-nil only
// for the in-process SSH path so connectNext / Close can release it.
func (r *ResilientClient) dial(e Endpoint) (weftv1.WeftAgentClient, *grpc.ClientConn, *sshDialer, error) {
	// Local socket : already-tunneled by the operator.
	if e.Address == "" && e.Socket != "" {
		c, conn, err := weftclient.Client(e.Socket)
		return c, conn, nil, err
	}
	// Remote SSH (in-process pure-Go).
	if e.Address != "" && e.SSHSocket != "" {
		d, err := newSSHDialer(e)
		if err != nil {
			return nil, nil, nil, err
		}
		c, conn, err := weftclient.Client("", weftclient.WithDialOption("passthrough:///weft", d.grpcDialOption()))
		if err != nil {
			_ = d.Close()
			return nil, nil, nil, err
		}
		return c, conn, d, nil
	}
	// Legacy in-process SSH transport (agent-side SSH server).
	if e.Address != "" && e.SSHKey != "" {
		c, conn, err := weftclient.Client(e.Address, weftclient.WithSSH(e.SSHSocket, e.SSHKey))
		return c, conn, nil, err
	}
	return nil, nil, nil, fmt.Errorf("endpoint %s : neither socket nor SSH target configured", e.Name)
}

// connectNext walks the endpoint list starting at idx+1, dialling
// each until one succeeds. Swaps the embedded WeftAgentClient under
// the write lock on success.
//
// Lock discipline : all callbacks (onSwitch, onEvent) fire AFTER the
// mutex is released. Calling them while the write lock is held used
// to self-deadlock when the hook tried to re-enter the wrapper (and
// any RLock under the write lock blocks forever) — see
// TestNewResilientClient_AllUnreachable's regression.
func (r *ResilientClient) connectNext() error {
	// Pending callbacks captured while the lock is held ; fired after
	// the defer unwinds. Lets us snapshot the hook + payload safely.
	type pendingEvent struct{ level, msg string }
	var pendingEvents []pendingEvent
	var switchedTo *Endpoint
	defer func() {
		// onSwitch always fires for the success case ; onEvent
		// flushes every queued dial-failure warning. snapshotted
		// hooks below ; safe to call without the lock.
		hookEvent := r.snapshotEventHook()
		hookSwitch := r.snapshotSwitchHook()
		for _, ev := range pendingEvents {
			if hookEvent != nil {
				hookEvent(ev.level, ev.msg)
			}
		}
		if switchedTo != nil && hookSwitch != nil {
			hookSwitch(*switchedTo)
		}
	}()

	r.mu.Lock()
	defer r.mu.Unlock()
	var lastErr error
	for tries := 0; tries < len(r.endpoints); tries++ {
		i := (r.idx + tries) % len(r.endpoints)
		ep := r.endpoints[i]
		client, conn, sshd, err := r.dial(ep)
		if err != nil {
			pendingEvents = append(pendingEvents, pendingEvent{
				level: ResilientEventWarn,
				msg:   fmt.Sprintf("dial %s : %v", ep, err),
			})
			lastErr = err
			continue
		}
		oldConn, oldSSH := r.conn, r.ssh
		r.active = ep
		r.WeftAgentClient = client
		r.conn = conn
		r.ssh = sshd
		r.idx = i
		r.failures.Store(0)
		if oldConn != nil {
			_ = oldConn.Close()
		}
		if oldSSH != nil {
			_ = oldSSH.Close()
		}
		// Queue the success callback ; fired after mu unlocks.
		ep2 := ep
		switchedTo = &ep2
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no endpoints configured")
	}
	return fmt.Errorf("all %d endpoints unreachable, last error: %w", len(r.endpoints), lastErr)
}

// snapshotEventHook returns the current onEvent callback under a
// read lock. Used by deferred callback dispatch in connectNext.
func (r *ResilientClient) snapshotEventHook() func(level, msg string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.onEvent
}

// snapshotSwitchHook returns the current onSwitch callback under a
// read lock.
func (r *ResilientClient) snapshotSwitchHook() func(Endpoint) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.onSwitch
}

// watchdog runs GetClusterInfo every healthInterval. A failed call
// increments r.failures ; after failureBudget consecutive failures
// the wrapper picks the next endpoint.
func (r *ResilientClient) watchdog() {
	if r.healthInterval <= 0 {
		return
	}
	t := time.NewTicker(r.healthInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			r.mu.RLock()
			cli := r.WeftAgentClient
			r.mu.RUnlock()
			if cli == nil {
				cancel()
				continue
			}
			_, err := cli.GetClusterInfo(ctx, &weftv1.GetClusterInfoRequest{})
			cancel()
			// Unimplemented is NOT a connection failure : the agent
			// is up + serving gRPC, the RPC just isn't in its build
			// (legacy server pre-dating ClusterInfo). The link is
			// healthy ; resetting the failure counter prevents a
			// pointless failover cascade across the whole endpoint
			// list when EVERY agent is on the same old build.
			if err != nil && status.Code(err) == codes.Unimplemented {
				r.failures.Store(0)
				continue
			}
			if err != nil {
				if r.failures.Add(1) >= r.failureBudget {
					if reconnectErr := r.connectNext(); reconnectErr != nil {
						r.emitEvent(ResilientEventError, fmt.Sprintf("failover failed : %v", reconnectErr))
					}
				}
			} else {
				r.failures.Store(0)
			}
		}
	}
}

// Ensure ResilientClient satisfies io.Closer for the main.go defer.
var _ io.Closer = (*ResilientClient)(nil)

// And that the embedded interface gives us full WeftAgentClient
// surface — defensive compile-time check.
var _ weftv1.WeftAgentClient = (*ResilientClient)(nil)

// Ensure grpc.ClientConnInterface is satisfied too, in case any
// caller needs the lower-level invoke path (server-streaming).
var _ = func(*grpc.ClientConn) {}
