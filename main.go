// weft-tui — interactive terminal UI for the weft cluster
// orchestrator. Standalone binary ; dials a running weft agent
// over its Unix-socket gRPC and presents a navigable dashboard.
//
// Sibling of `weft` (CLI) and `weft-webui` (HTTP+TS). Same data,
// different surface.

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	weftclient "github.com/openweft/weft-client"
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
	model.clusterName = *clusterName
	prog := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "weft-tui: %v\n", err)
		os.Exit(1)
	}
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
