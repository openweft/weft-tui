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

	mu     sync.RWMutex
	active Endpoint
	conn   *grpc.ClientConn
	idx    int

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

// Close terminates the heartbeat goroutine + the underlying conn.
// Idempotent.
func (r *ResilientClient) Close() error {
	r.stopOnce.Do(func() { close(r.stop) })
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// dial wraps weftclient.Client with the per-endpoint transport
// fields. Local socket : pass it as the dial target. SSH tunnel :
// use WithSSH(ssh_socket, key) so weft-client opens its own
// per-RPC SSH session.
func (r *ResilientClient) dial(e Endpoint) (weftv1.WeftAgentClient, *grpc.ClientConn, error) {
	var opts []weftclient.Option
	target := e.Socket
	if e.SSHKey != "" && e.Address != "" {
		opts = append(opts, weftclient.WithSSH(e.SSHSocket, e.SSHKey))
		target = e.Address
	}
	if target == "" {
		return nil, nil, fmt.Errorf("endpoint %s has neither socket nor SSH transport configured", e.Name)
	}
	return weftclient.Client(target, opts...)
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
		client, conn, err := r.dial(ep)
		if err != nil {
			fmt.Fprintf(os.Stderr, "weft-tui: dial %s: %v\n", ep, err)
			lastErr = err
			continue
		}
		oldConn := r.conn
		r.active = ep
		r.WeftAgentClient = client
		r.conn = conn
		r.idx = i
		r.failures.Store(0)
		if oldConn != nil {
			_ = oldConn.Close()
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
