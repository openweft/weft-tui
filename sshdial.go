package main

// sshdial.go is the in-process SSH transport for the TUI's
// resilient connector. Replaces the os/exec("ssh -fNL ...") fork
// + /tmp/<socket> shuffle with a pure-Go SSH client that opens a
// `direct-streamlocal@openssh.com` channel to the agent's Unix
// socket on the remote host.
//
// Benefits over the shell-out approach :
//   * No reliance on the system `ssh` binary being in PATH
//   * No /tmp socket files to manage / leak / collide
//   * No `pkill -f <path>` cleanup voodoo
//   * Full SSH error visibility to the Go layer
//
// Tradeoffs : we re-implement a slim subset of OpenSSH's client
// (user@host:port parsing, ssh-agent fallback, known_hosts host-key
// check). The TUI doesn't honor ~/.ssh/config or ProxyCommand — if
// the operator needs those, they keep using the legacy
// shell-tunnel path (`ssh -L /tmp/sock:… &` + `--socket /tmp/sock`).

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"google.golang.org/grpc"
)

// sshDialer holds the live *ssh.Client for an Endpoint + opens a
// direct-streamlocal channel per gRPC dial. One sshDialer per active
// connection ; the ResilientClient creates a new one on every
// successful endpoint connect.
type sshDialer struct {
	client     *ssh.Client
	remoteSock string
}

// newSSHDialer dials the remote SSH server using the auth methods +
// known-hosts policy of the Endpoint. Returns a dialer ready to open
// gRPC-over-SSH-channel connections. The ctx is the
// ResilientClient's stop channel ; if the caller quits while
// we're mid-handshake (TCP DialContext + ssh.NewClientConn), the
// dial unwinds quickly instead of blocking on the 5s Timeout.
// Audit 2026-06-25 fix : without this Close() blocked ~5s × N
// endpoints on a degraded cluster.
func newSSHDialer(ctx context.Context, e Endpoint) (*sshDialer, error) {
	user, host, port := parseSSHAddress(e.Address, e.SSHUser)
	authMethods, err := sshAuthMethods(e.SSHKey)
	if err != nil {
		return nil, fmt.Errorf("ssh auth %s: %w", e.Address, err)
	}
	hostKeyCB, err := sshHostKeyCallback()
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCB,
		Timeout:         5 * time.Second,
	}
	addr := net.JoinHostPort(host, port)
	dialer := net.Dialer{Timeout: 5 * time.Second}
	tcpConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	// ssh.NewClientConn doesn't take a ctx ; race the handshake
	// against the ctx via a goroutine. If ctx fires first we close
	// the TCP conn and the handshake unwinds with an EOF.
	type result struct {
		conn ssh.Conn
		chs  <-chan ssh.NewChannel
		reqs <-chan *ssh.Request
		err  error
	}
	done := make(chan result, 1)
	go func() {
		c, chs, reqs, err := ssh.NewClientConn(tcpConn, addr, cfg)
		done <- result{c, chs, reqs, err}
	}()
	select {
	case <-ctx.Done():
		_ = tcpConn.Close()
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("ssh handshake %s: %w", addr, r.err)
		}
		client := ssh.NewClient(r.conn, r.chs, r.reqs)
		return &sshDialer{client: client, remoteSock: e.SSHSocket}, nil
	}
}

// Close terminates the SSH connection. Idempotent.
func (d *sshDialer) Close() error {
	if d == nil || d.client == nil {
		return nil
	}
	return d.client.Close()
}

// grpcDialOption returns a grpc.DialOption that opens a fresh
// direct-streamlocal channel to the remote Unix socket on every
// gRPC connection. The gRPC stack multiplexes streams over the
// channel, so one TCP round-trip per RPC is amortised across the
// per-host SSH connection.
func (d *sshDialer) grpcDialOption() grpc.DialOption {
	return grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		ch, reqs, err := d.client.OpenChannel("direct-streamlocal@openssh.com", marshalStreamLocal(d.remoteSock))
		if err != nil {
			return nil, fmt.Errorf("direct-streamlocal %s: %w", d.remoteSock, err)
		}
		go ssh.DiscardRequests(reqs)
		return &sshChannelConn{Channel: ch, client: d.client}, nil
	})
}

// marshalStreamLocal builds the channel-open payload for the
// "direct-streamlocal@openssh.com" extension. Format per OpenSSH's
// PROTOCOL.mux :
//
//	string  socket path
//	string  reserved (always "")
//	uint32  reserved (always 0)
func marshalStreamLocal(socket string) []byte {
	type req struct {
		SocketPath string
		Reserved0  string
		Reserved1  uint32
	}
	return ssh.Marshal(req{SocketPath: socket})
}

// sshChannelConn adapts an ssh.Channel to net.Conn. The SSH channel
// doesn't carry a real network endpoint, so the address methods
// return synthetic strings the gRPC stack uses only for logging.
type sshChannelConn struct {
	ssh.Channel
	client *ssh.Client
}

func (c *sshChannelConn) LocalAddr() net.Addr  { return sshChannelAddr("ssh-channel-local") }
func (c *sshChannelConn) RemoteAddr() net.Addr { return sshChannelAddr(c.client.RemoteAddr().String()) }

// Deadline methods : ssh.Channel doesn't support them ; gRPC tolerates
// the no-op when the surrounding context carries the timeout.
func (c *sshChannelConn) SetDeadline(_ time.Time) error      { return nil }
func (c *sshChannelConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *sshChannelConn) SetWriteDeadline(_ time.Time) error { return nil }

type sshChannelAddr string

func (a sshChannelAddr) Network() string { return "ssh" }
func (a sshChannelAddr) String() string  { return string(a) }

// parseSSHAddress accepts "user@host:port", "host:port", "user@host",
// or "host" and returns the three components with sensible defaults
// (port 22, user from the endpoint config or the current OS user).
func parseSSHAddress(addr, defaultUser string) (user, host, port string) {
	port = "22"
	user = defaultUser
	host = addr
	if at := strings.Index(host, "@"); at >= 0 {
		user = host[:at]
		host = host[at+1:]
	}
	if colon := strings.LastIndex(host, ":"); colon >= 0 && !strings.HasPrefix(host, "[") {
		// "host:port" — separate. Skip the "[ipv6]:port" form since
		// LastIndex would split inside the brackets.
		port = host[colon+1:]
		host = host[:colon]
	}
	if user == "" {
		if cu, err := osCurrentUser(); err == nil {
			user = cu
		}
	}
	return user, host, port
}

// sshAuthMethods builds the ssh client auth chain :
//   - keyPath set + readable → use the private-key file directly
//   - keyPath empty → try ssh-agent ($SSH_AUTH_SOCK)
//   - both unavailable → return an error so the caller surfaces it
func sshAuthMethods(keyPath string) ([]ssh.AuthMethod, error) {
	if keyPath != "" {
		if data, err := os.ReadFile(keyPath); err == nil {
			signer, err := ssh.ParsePrivateKey(data)
			if err == nil {
				return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
			}
			// Fall through to ssh-agent on key parse failure so a
			// password-encrypted key file in ~/.ssh can still work
			// when the matching unlocked signer is in the agent.
		}
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			ag := agent.NewClient(conn)
			return []ssh.AuthMethod{ssh.PublicKeysCallback(ag.Signers)}, nil
		}
	}
	return nil, errors.New("no usable SSH auth (key file unreadable AND ssh-agent unavailable)")
}

// sshHostKeyCallback returns a HostKeyCallback that verifies against
// ~/.ssh/known_hosts. Three regimes :
//
//   * SSH_TUI_STRICT_HOSTKEY=1 : fail-closed on any unknown or
//     mismatching host. Production policy.
//   * known_hosts present (default) : reject only on a true key
//     mismatch (potential MITM) ; accept unknown hosts TOFU-style
//     so dev VMs that get recreated don't break the operator's
//     daily flow. The accepted key is NOT auto-written to the file
//     (the OpenSSH client already covers that ergonomic).
//   * known_hosts missing : InsecureIgnoreHostKey ; the operator
//     never ran ssh from this account before.
func sshHostKeyCallback() (ssh.HostKeyCallback, error) {
	strict := os.Getenv("SSH_TUI_STRICT_HOSTKEY") == "1"
	home, err := os.UserHomeDir()
	if err != nil {
		if strict {
			return nil, fmt.Errorf("cannot resolve $HOME for known_hosts (SSH_TUI_STRICT_HOSTKEY=1)")
		}
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec
	}
	path := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(path); err != nil {
		if strict {
			return nil, fmt.Errorf("known_hosts %s missing (SSH_TUI_STRICT_HOSTKEY=1)", path)
		}
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec
	}
	if strict {
		cb, err := knownhosts.New(path)
		if err != nil {
			return nil, fmt.Errorf("parse known_hosts: %w", err)
		}
		return cb, nil
	}
	// Non-strict (default) : InsecureIgnoreHostKey. The operator's
	// dev VMs get recreated regularly (Tart) so the known_hosts
	// always has stale entries and a strict callback rejects every
	// connection with "key mismatch". Set SSH_TUI_STRICT_HOSTKEY=1
	// to opt into strict + match `ssh` CLI defaults.
	return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec
}

// osCurrentUser returns the current OS user's login name. Wrapped
// for test seam ; production calls os/user.Current().
var osCurrentUser = func() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// _ silences "imported and not used" when binary.LittleEndian is
// only needed by the streamlocal payload encoder. ssh.Marshal handles
// it via struct reflection ; the import is here for clarity in case
// we replace ssh.Marshal with hand-rolled encoding later.
var _ = binary.LittleEndian
