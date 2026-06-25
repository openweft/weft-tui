// weft-tui — interactive terminal UI for the weft cluster
// orchestrator. Standalone binary ; dials a running weft agent
// over its Unix-socket gRPC and presents a navigable dashboard.
//
// Sibling of `weft` (CLI) and `weft-webui` (HTTP+TS). Same data,
// different surface.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	weftclient "github.com/openweft/weft-client"
	weftv1 "github.com/openweft/weft-proto"
)

func main() {
	defaultSocket := defaultSocketPath()
	var (
		socket      = flag.String("socket", "", "Weft agent Unix socket path (legacy single-host mode ; overrides clusters.hcl when set). Default $HOME/.weft/weft.sock falls back from the config file when no clusters are configured.")
		sshSocket   = flag.String("ssh-socket", "", "Weft agent SSH socket path (enables SSH auth) ; default $HOME/.weft/weft-ssh.sock when --ssh-key is set")
		sshKey      = flag.String("ssh-key", "", "SSH private key for authentication (enables SSH transport)")
		clusterName = flag.String("cluster-name", os.Getenv("WEFT_CLUSTER_NAME"), "Federated cluster name shown in the title bar (e.g. 'prod-eu'). Defaults to $WEFT_CLUSTER_NAME ; empty hides the suffix.")
		clusterFlag = flag.String("cluster", os.Getenv("WEFT_TUI_CLUSTER"), "Cluster to connect to (matches a `cluster \"<name>\" {}` block in clusters.hcl). Default = first cluster in the file.")
	)
	flag.Parse()

	// Resolve ssh-socket default once ssh-key is set + the user
	// didn't override.
	if *sshKey != "" && *sshSocket == "" {
		*sshSocket = defaultSSHSocketPath()
	}

	// Endpoint resolution priority :
	//   1. --socket set → legacy single-host mode (existing flow).
	//   2. clusters.hcl present + has cluster → resilient connector
	//      cycles through endpoints + auto-fails over on disconnect.
	//   3. fallback → defaultSocket ($HOME/.weft/weft.sock).
	var (
		client weftv1.WeftAgentClient
		closer func() error
	)
	if *socket == "" {
		if endpoints, err := resolveEndpointsFromConfig(*clusterFlag); err != nil {
			fmt.Fprintf(os.Stderr, "weft-tui: %v\n", err)
		} else if len(endpoints) > 0 {
			rc, err := NewResilientClient(endpoints)
			if err != nil {
				fmt.Fprintf(os.Stderr, "weft-tui: %v\n", err)
				os.Exit(1)
			}
			client = rc
			closer = rc.Close
		}
	}
	if client == nil {
		// Legacy single-socket path.
		target := *socket
		if target == "" {
			target = defaultSocket
		}
		var opts []weftclient.Option
		if *sshKey != "" {
			opts = append(opts, weftclient.WithSSH(*sshSocket, *sshKey))
		}
		c, conn, err := weftclient.Client(target, opts...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "weft-tui: dial weft agent at %s: %v\n", target, err)
			os.Exit(1)
		}
		client = c
		closer = conn.Close
	}
	defer closer()

	model := New(client)
	// One GetClusterInfo RPC covers both the title-bar cluster name
	// AND the CP marker on the Hosts tab. Resolution order :
	//   1. --cluster-name flag (or $WEFT_CLUSTER_NAME) → wins for
	//      the title bar when explicitly set (ad-hoc override).
	//   2. GetClusterInfo → persisted cluster name + local_host_uuid
	//      (the agent serving this socket, used to mark the CP row).
	//   3. Empty + RPC error / unset → title bar shows just
	//      "weft tui" + no row gets the CP marker.
	rpcName, _, rpcCPSet := autoFetchClusterInfo(client)
	if *clusterName != "" {
		model.clusterName = *clusterName
	} else {
		model.clusterName = rpcName
	}
	model.hosts.controlPlaneUUIDs = rpcCPSet
	// tea.WithMouseCellMotion enables mouse button events + cell-
	// resolution motion (one MouseMsg per cell the cursor crosses).
	// Update handles MouseMsg in app.go : sidebar entries toggle the
	// active object type ; table rows move the cursor ; scroll-wheel
	// pages the table viewport ; the palette entries become click
	// targets when open.
	prog := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	// Wire the ResilientClient's lifecycle events to the TUI status
	// bar (instead of letting them scroll across the alt-screen via
	// os.Stderr). Only meaningful for the resilient path ; the
	// legacy single-socket flow never instantiates *ResilientClient.
	//
	// IMPORTANT : prog.Send blocks until the program's event loop is
	// running (it pushes to an unbuffered channel that Run drains).
	// Calling it before prog.Run() deadlocks the whole startup —
	// nothing renders, the user sees a blank terminal + interprets
	// it as "cannot connect". The seed-send is therefore deferred
	// to a goroutine that fires after Run starts ; SetOnSwitch /
	// SetOnEvent install the hooks synchronously so events that
	// arrive once Run is up reach the model.
	if rc, ok := client.(*ResilientClient); ok {
		rc.SetOnSwitch(func(ep Endpoint) {
			prog.Send(connSwitchMsg{active: ep})
		})
		rc.SetOnEvent(func(level, msg string) {
			prog.Send(connEventMsg{level: level, msg: msg})
		})
		// Seed the initial endpoint into the status bar so the
		// operator sees "● dc1" right away. Goroutine so Send
		// doesn't block on prog.Run starting ; the message is
		// safely queued once Run begins.
		go func() {
			prog.Send(connSwitchMsg{active: rc.Active()})
		}()
	}
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "weft-tui: %v\n", err)
		os.Exit(1)
	}
}

// autoFetchClusterInfo calls GetClusterInfo on the connected agent
// + returns (cluster_name, local_host_uuid, control_plane_uuid_set).
// Best-effort : RPC error or empty response → ("", "", nil) so the
// title bar / CP marker fall back to their pre-feature look. The
// flag/env path in main() short-circuits the cluster_name branch
// when the operator explicitly set a name ; the local UUID + CP set
// are always taken from the RPC.
//
// The CP set covers EVERY etcd quorum member (3 in a 3-DC HA cluster,
// 1 in single-host dev) — the TUI's Hosts tab marks every matching
// row's CP column with "*". The local UUID is kept separate (some
// future TUI flows might want to distinguish the locally-driven CP
// from the other quorum members).
//
// 3-second deadline so a slow agent at boot doesn't stall the TUI's
// alt-screen switch. Cheap RPC ; the operator notices a stall.
func autoFetchClusterInfo(client weftv1.WeftAgentClient) (clusterName, localHostUUID string, controlPlaneUUIDs map[string]struct{}) {
	if client == nil {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := client.GetClusterInfo(ctx, &weftv1.GetClusterInfoRequest{})
	if err != nil || resp == nil {
		return "", "", nil
	}
	cpSet := make(map[string]struct{}, len(resp.ControlPlaneHostUuids))
	for _, u := range resp.ControlPlaneHostUuids {
		if u == "" {
			continue
		}
		cpSet[u] = struct{}{}
	}
	// Belt-and-braces : if the server runs a build that doesn't yet
	// populate control_plane_host_uuids, fall back to the legacy
	// single-CP marker via local_host_uuid alone so the operator
	// still sees something.
	if len(cpSet) == 0 && resp.LocalHostUuid != "" {
		cpSet[resp.LocalHostUuid] = struct{}{}
	}
	return resp.ClusterName, resp.LocalHostUuid, cpSet
}

// autoFetchClusterName is the v0.3.7 entry point, kept for the
// existing test (TestAutoFetchClusterName_NilClient) and any
// external caller. Delegates to autoFetchClusterInfo + drops
// the local-host UUID + CP set.
func autoFetchClusterName(client weftv1.WeftAgentClient) string {
	name, _, _ := autoFetchClusterInfo(client)
	return name
}

// defaultSocketPath returns $HOME/.weft/weft.sock — the same default
// the weft CLI uses for the plain-socket transport.
func defaultSocketPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return "weft.sock"
	}
	return filepath.Join(home, ".weft", "weft.sock")
}

// defaultSSHSocketPath returns $HOME/.weft/weft-ssh.sock — the SSH
// transport's default that mirrors the CLI's `--ssh-socket` default.
func defaultSSHSocketPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return "weft-ssh.sock"
	}
	return filepath.Join(home, ".weft", "weft-ssh.sock")
}
