package main

// sshdial_test.go locks down the pure-data helpers behind the
// in-process SSH transport. The actual ssh.Dial path is integration-
// only (it needs a real sshd), but parseSSHAddress + marshalStreamLocal
// + sshAuthMethods are the bits that regressions most commonly break,
// and they're all reachable as plain Go functions.

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// TestParseSSHAddress_Forms covers the four documented address shapes :
// "host", "host:port", "user@host", "user@host:port". A regression
// that re-orders LastIndex/strings.Index would split "user@h:p" wrong
// (host="h:p" or user="user@h") + the dial dies with a useless error.
func TestParseSSHAddress_Forms(t *testing.T) {
	cases := []struct {
		in       string
		defUser  string
		wantUser string
		wantHost string
		wantPort string
	}{
		{"dc1-r1-h1", "admin", "admin", "dc1-r1-h1", "22"},
		{"dc1-r1-h1:2222", "admin", "admin", "dc1-r1-h1", "2222"},
		{"root@dc1-r1-h1", "admin", "root", "dc1-r1-h1", "22"},
		{"root@dc1-r1-h1:2200", "admin", "root", "dc1-r1-h1", "2200"},
		// Empty default user → osCurrentUser fallback. Stub it so the
		// test is hermetic across CI users.
		{"h", "", "stub-user", "h", "22"},
	}
	origLookup := osCurrentUser
	osCurrentUser = func() (string, error) { return "stub-user", nil }
	defer func() { osCurrentUser = origLookup }()

	for _, tc := range cases {
		u, h, p := parseSSHAddress(tc.in, tc.defUser)
		if u != tc.wantUser || h != tc.wantHost || p != tc.wantPort {
			t.Errorf("parseSSHAddress(%q,%q) = (%q,%q,%q) ; want (%q,%q,%q)",
				tc.in, tc.defUser, u, h, p, tc.wantUser, tc.wantHost, tc.wantPort)
		}
	}
}

// TestMarshalStreamLocal_Payload locks down the OpenSSH wire format
// for direct-streamlocal@openssh.com channel-open requests. Format
// per PROTOCOL.mux : <u32 len><string path><u32 0=reserved><u32 0>.
// A subtle regression (wrong byte order, missing reserved field)
// breaks the gRPC dial silently — openssh just refuses the open.
func TestMarshalStreamLocal_Payload(t *testing.T) {
	sock := "/home/admin/.weft/weft.sock"
	b := marshalStreamLocal(sock)

	// Expected layout : string-prefixed socket, then empty string, then uint32(0).
	if len(b) < 4+len(sock)+4+4 {
		t.Fatalf("payload too short (%d bytes) : %x", len(b), b)
	}
	sockLen := binary.BigEndian.Uint32(b[:4])
	if int(sockLen) != len(sock) {
		t.Fatalf("socket length prefix = %d ; want %d", sockLen, len(sock))
	}
	if string(b[4:4+sockLen]) != sock {
		t.Fatalf("socket path encoded wrong : got %q ; want %q", string(b[4:4+sockLen]), sock)
	}
	// Next : empty reserved string (4 bytes of zeroes = length 0).
	off := 4 + int(sockLen)
	if binary.BigEndian.Uint32(b[off:off+4]) != 0 {
		t.Fatalf("reserved string not empty")
	}
	off += 4
	if binary.BigEndian.Uint32(b[off:off+4]) != 0 {
		t.Fatalf("reserved uint32 not 0")
	}
}

// TestSSHAuthMethods_NoSourcesAvailable ensures the caller gets a
// clear error instead of a nil + nil slip-through when neither the
// key file is readable nor SSH_AUTH_SOCK is set. A regression that
// returned ([]ssh.AuthMethod{}, nil) would let ssh.Dial use no auth +
// fail with "ssh: handshake failed: ssh: unable to authenticate".
func TestSSHAuthMethods_NoSourcesAvailable(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "") // disable agent
	// /dev/null/missing : unreadable in every OS context.
	_, err := sshAuthMethods("/dev/null/missing/key.pem")
	if err == nil {
		t.Fatal("expected error when neither key file nor SSH_AUTH_SOCK is usable")
	}
	if !strings.Contains(err.Error(), "no usable SSH auth") {
		t.Fatalf("error message regressed : %q", err)
	}
}

// TestSSHHostKeyCallback_StrictRequiresKnownHosts covers the strict
// regime : SSH_TUI_STRICT_HOSTKEY=1 + missing known_hosts must error
// out instead of silently downgrading. Operators turn on strict to
// catch MITM ; a silent fallback to InsecureIgnoreHostKey would
// defeat that.
func TestSSHHostKeyCallback_StrictRequiresKnownHosts(t *testing.T) {
	t.Setenv("SSH_TUI_STRICT_HOSTKEY", "1")
	t.Setenv("HOME", t.TempDir()) // no ~/.ssh/known_hosts present
	_, err := sshHostKeyCallback()
	if err == nil {
		t.Fatal("expected error when strict is on + known_hosts missing")
	}
}

// TestSSHHostKeyCallback_NonStrictFallsBackToInsecure : default
// regime (env unset) returns a non-nil callback that accepts unknown
// hosts. Operators on dev Tart VMs rely on this — strict mode would
// reject every recreated VM's fresh host key.
func TestSSHHostKeyCallback_NonStrictFallsBackToInsecure(t *testing.T) {
	t.Setenv("SSH_TUI_STRICT_HOSTKEY", "")
	t.Setenv("HOME", t.TempDir())
	cb, err := sshHostKeyCallback()
	if err != nil {
		t.Fatalf("unexpected err : %v", err)
	}
	if cb == nil {
		t.Fatal("nil callback in non-strict mode")
	}
}

// _ keeps the os import alive in environments where Setenv is the
// only thing imported from os ; otherwise we'd get
// "imported and not used".
var _ = os.Getenv
