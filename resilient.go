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
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	weftclient "github.com/openweft/weft-client"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// ResilientClient is the failover-capable weftv1.WeftAgentClient.
// The embedded interface field is what every RPC dispatches through ;
// reconnects atomically swap it for the freshly-dialled client.
type ResilientClient struct {
	weftv1.WeftAgentClient // current underlying ; swapped under mu on reconnect

	endpoints []Endpoint

	mu        sync.RWMutex
	active    Endpoint
	conn      *grpc.ClientConn
	tunnelCmd *exec.Cmd // ssh -fNL process owning the local tunnel socket
	tunnelSock string   // local Unix socket path that maps to the remote
	idx       int

	// onSwitch fires after every successful endpoint swap. nil = no-op.
	onSwitch func(active Endpoint)

	failures       atomic.Int32
	healthInterval time.Duration
	failureBudget  int32

	stop     chan struct{}
	stopOnce sync.Once
}

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

// Active returns the endpoint the wrapper is currently talking to.
func (r *ResilientClient) Active() Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// Close terminates the heartbeat goroutine + the underlying conn,
// plus the SSH tunnel child if any.
func (r *ResilientClient) Close() error {
	r.stopOnce.Do(func() { close(r.stop) })
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.conn != nil {
		err = r.conn.Close()
	}
	if r.tunnelSock != "" {
		_ = killTunnel(r.tunnelCmd, r.tunnelSock)
	}
	return err
}

// dial reaches the endpoint's weft agent. Three transports :
//
//   * e.Socket alone : the operator has a pre-existing tunnel
//     (`ssh -L /tmp/weft.sock:…`) — just dial the local socket.
//   * e.Address + e.SSHSocket : the TUI forks `ssh -fNL <tmp>:<remote>`
//     to establish a fresh tunnel itself, then dials the local
//     temp socket. The forked process gets terminated on the next
//     dial / on Close so failover cleans up after itself.
//   * legacy weftclient.WithSSH path (e.Address + e.SSHKey but no
//     remote socket spec) : preserved for the in-process SSH-server
//     model the agent exposes locally, not used by the operator
//     scenario.
//
// Returns (client, conn, tunnelCmd, tunnelSocket, error). tunnelCmd
// is nil for the legacy + local-socket paths.
func (r *ResilientClient) dial(e Endpoint) (weftv1.WeftAgentClient, *grpc.ClientConn, *exec.Cmd, string, error) {
	// Local socket : already-tunneled by the operator.
	if e.Address == "" && e.Socket != "" {
		c, conn, err := weftclient.Client(e.Socket)
		return c, conn, nil, "", err
	}
	// Remote SSH : fork `ssh -fNL <local>:<remote>` and dial the local
	// socket. The fork keeps the tunnel alive across multiple RPCs
	// without per-call SSH handshake cost.
	if e.Address != "" && e.SSHSocket != "" {
		localSock, cmd, err := openSSHTunnel(e)
		if err != nil {
			return nil, nil, nil, "", err
		}
		c, conn, err := weftclient.Client(localSock)
		if err != nil {
			_ = killTunnel(cmd, localSock)
			return nil, nil, nil, "", err
		}
		return c, conn, cmd, localSock, nil
	}
	// Legacy in-process SSH transport (agent's local SSH server).
	if e.Address != "" && e.SSHKey != "" {
		c, conn, err := weftclient.Client(e.Address, weftclient.WithSSH(e.SSHSocket, e.SSHKey))
		return c, conn, nil, "", err
	}
	return nil, nil, nil, "", fmt.Errorf("endpoint %s : neither socket nor SSH target configured", e.Name)
}

// openSSHTunnel forks `ssh -fN -L <local-unix-sock>:<remote-unix-sock>
// <user>@<host>` and returns the path of the local socket once it
// appears (ssh writes it from its child once the forward is bound).
// 5-second budget so a dead host doesn't stall the TUI's startup.
func openSSHTunnel(e Endpoint) (string, *exec.Cmd, error) {
	// Local socket path : per-endpoint slug so multiple tunnels can
	// coexist in /tmp without clashing.
	slug := e.Name
	if slug == "" {
		slug = "weft"
	}
	localSock := filepath.Join(os.TempDir(), fmt.Sprintf("weft-tui-%s-%d.sock", slug, os.Getpid()))
	_ = os.Remove(localSock)
	args := []string{
		"-fN",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "StreamLocalBindUnlink=yes",
		"-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3",
		"-L", localSock + ":" + e.SSHSocket,
	}
	if e.SSHKey != "" {
		args = append(args, "-i", e.SSHKey, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, e.Address)
	cmd := exec.Command("ssh", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("ssh -fNL: %w", err)
	}
	// Wait for the socket to materialise (ssh -f returns BEFORE the
	// child process has bound the forward in some cases).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(localSock); err == nil {
			return localSock, cmd, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", nil, fmt.Errorf("ssh tunnel %s : socket %s never appeared", e.Address, localSock)
}

// killTunnel removes the local socket + signals any lingering ssh
// child via `pkill` matching the socket path (the `-fN` ssh has
// no easily-reachable PID since the parent returned). Best-effort.
func killTunnel(_ *exec.Cmd, localSock string) error {
	if localSock == "" {
		return nil
	}
	// ssh -fN forks a detached child whose argv contains the socket
	// path — pkill -f matches it uniquely.
	exec.Command("pkill", "-f", localSock).Run()
	_ = os.Remove(localSock)
	return nil
}

// connectNext walks the endpoint list starting at idx+1, dialling
// each until one succeeds. Swaps the embedded WeftAgentClient under
// the write lock on success.
func (r *ResilientClient) connectNext() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var lastErr error
	for tries := 0; tries < len(r.endpoints); tries++ {
		i := (r.idx + tries) % len(r.endpoints)
		ep := r.endpoints[i]
		client, conn, tunCmd, tunSock, err := r.dial(ep)
		if err != nil {
			fmt.Fprintf(os.Stderr, "weft-tui: dial %s: %v\n", ep, err)
			lastErr = err
			continue
		}
		oldConn, oldCmd, oldSock := r.conn, r.tunnelCmd, r.tunnelSock
		r.active = ep
		r.WeftAgentClient = client
		r.conn = conn
		r.tunnelCmd = tunCmd
		r.tunnelSock = tunSock
		r.idx = i
		r.failures.Store(0)
		if oldConn != nil {
			_ = oldConn.Close()
		}
		if oldSock != "" {
			_ = killTunnel(oldCmd, oldSock)
		}
		if r.onSwitch != nil {
			r.onSwitch(ep)
		}
		fmt.Fprintf(os.Stderr, "weft-tui: connected to %s\n", ep)
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no endpoints configured")
	}
	return fmt.Errorf("all %d endpoints unreachable, last error: %w", len(r.endpoints), lastErr)
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
			if err != nil {
				if r.failures.Add(1) >= r.failureBudget {
					if reconnectErr := r.connectNext(); reconnectErr != nil {
						fmt.Fprintf(os.Stderr, "weft-tui: failover failed: %v\n", reconnectErr)
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
