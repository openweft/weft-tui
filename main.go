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
		socket      = flag.String("socket", defaultSocket, "Weft agent Unix socket path")
		sshSocket   = flag.String("ssh-socket", "", "Weft agent SSH socket path (enables SSH auth) ; default $HOME/.weft/weft-ssh.sock when --ssh-key is set")
		sshKey      = flag.String("ssh-key", "", "SSH private key for authentication (enables SSH transport)")
		clusterName = flag.String("cluster-name", os.Getenv("WEFT_CLUSTER_NAME"), "Federated cluster name shown in the title bar (e.g. 'prod-eu'). Defaults to $WEFT_CLUSTER_NAME ; empty hides the suffix.")
	)
	flag.Parse()

	// Resolve ssh-socket default once ssh-key is set + the user
	// didn't override.
	if *sshKey != "" && *sshSocket == "" {
		*sshSocket = defaultSSHSocketPath()
	}

	var opts []weftclient.Option
	if *sshKey != "" {
		opts = append(opts, weftclient.WithSSH(*sshSocket, *sshKey))
	}
	client, conn, err := weftclient.Client(*socket, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft-tui: dial weft agent at %s: %v\n", *socket, err)
		os.Exit(1)
	}
	defer conn.Close()

	model := New(client)
	// One GetClusterInfo RPC covers both the title-bar cluster name
	// AND the CP marker on the Hosts tab. Resolution order :
	//   1. --cluster-name flag (or $WEFT_CLUSTER_NAME) → wins for
	//      the title bar when explicitly set (ad-hoc override).
	//   2. GetClusterInfo → persisted cluster name + local_host_uuid
	//      (the agent serving this socket, used to mark the CP row).
	//   3. Empty + RPC error / unset → title bar shows just
	//      "weft tui" + no row gets the CP marker.
	rpcName, rpcLocalUUID := autoFetchClusterInfo(client)
	if *clusterName != "" {
		model.clusterName = *clusterName
	} else {
		model.clusterName = rpcName
	}
	model.hosts.localCPUUID = rpcLocalUUID
	prog := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "weft-tui: %v\n", err)
		os.Exit(1)
	}
}

// autoFetchClusterInfo calls GetClusterInfo on the connected agent
// + returns (cluster_name, local_host_uuid). Best-effort : RPC error
// or empty response → ("", "") so the title bar / CP marker fall
// back to their pre-feature look. The flag/env path in main()
// short-circuits the cluster_name branch when the operator
// explicitly set a name ; the local UUID is always taken from
// the RPC (it's the canonical "which host is the CP we're driving").
//
// 3-second deadline so a slow agent at boot doesn't stall the TUI's
// alt-screen switch. Cheap RPC ; the operator notices a stall.
func autoFetchClusterInfo(client weftv1.WeftAgentClient) (clusterName, localHostUUID string) {
	if client == nil {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := client.GetClusterInfo(ctx, &weftv1.GetClusterInfoRequest{})
	if err != nil || resp == nil {
		return "", ""
	}
	return resp.ClusterName, resp.LocalHostUuid
}

// autoFetchClusterName is the v0.3.7 entry point, kept for the
// existing test (TestAutoFetchClusterName_NilClient) and any
// external caller. Delegates to autoFetchClusterInfo + drops
// the local-host UUID.
func autoFetchClusterName(client weftv1.WeftAgentClient) string {
	name, _ := autoFetchClusterInfo(client)
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
