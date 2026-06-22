package main

// clusterconfig.go reads ~/.config/weft/clusters.hcl (or
// $WEFT_TUI_CONFIG) and resolves the active cluster's endpoint list.
// Two sources :
//
//   1. Static `endpoint` blocks — the operator types in
//      hostnames the TUI should try.
//   2. DNS SRV records — `srv_record = "_weft._tcp.weft.example.com"`
//      → the TUI does a net.LookupSRV, ordering targets by priority
//      then weight.
//
// Each endpoint can carry per-host SSH transport details
// (ssh_key, ssh_user, ssh_socket) so a tunnel-less connection works
// transparently. Missing fields inherit cluster-level defaults.
//
// Loader is forgiving : a missing file falls back to the legacy
// --socket flag in main.go ; a malformed block surfaces a warning
// on stderr but doesn't abort the TUI.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

// ClustersConfig is the top-level HCL schema.
//
//	cluster "prod" {
//	  default_ssh_key  = "~/.ssh/id_ed25519"
//	  default_ssh_user = "admin"
//	  default_socket   = "/home/admin/.weft/weft.sock"
//
//	  endpoint "h1" {
//	    address = "dc1-r1-h1"   # SSH target ; <user>@host syntax accepted
//	  }
//	  endpoint "h2" { address = "dc2-r1-h1" }
//	  endpoint "h3" { address = "dc3-r1-h1" }
//
//	  # OR — DNS SRV expansion :
//	  # srv_record = "_weft._tcp.cluster.example.com"
//	}
type ClustersConfig struct {
	Clusters []ClusterEntry `hcl:"cluster,block"`
}

type ClusterEntry struct {
	Name           string          `hcl:",label"`
	DefaultSSHKey  string          `hcl:"default_ssh_key,optional"`
	DefaultSSHUser string          `hcl:"default_ssh_user,optional"`
	DefaultSocket  string          `hcl:"default_socket,optional"`
	SRVRecord      string          `hcl:"srv_record,optional"`
	Endpoints      []EndpointEntry `hcl:"endpoint,block"`
}

type EndpointEntry struct {
	Name      string `hcl:",label"`
	Address   string `hcl:"address"`
	SSHKey    string `hcl:"ssh_key,optional"`
	SSHUser   string `hcl:"ssh_user,optional"`
	Socket    string `hcl:"socket,optional"`
	SSHSocket string `hcl:"ssh_socket,optional"`
}

// Endpoint is the resolved per-host shape the resilient connector
// consumes. Exactly one of (Socket, SSHKey+SSHSocket) is the
// effective transport ; both populated means SSH path wins.
type Endpoint struct {
	Name      string
	Address   string // <user>@host:port (user/port optional)
	Socket    string // local unix-socket path (legacy ; for in-process / pre-tunneled)
	SSHKey    string // PEM private key path for in-process SSH-to-gRPC
	SSHUser   string // SSH login user (defaults to current user when empty)
	SSHSocket string // remote unix socket path to forward through SSH
}

// LoadClustersConfig reads the operator's clusters.hcl. Lookup order :
//   1. $WEFT_TUI_CONFIG (operator override)
//   2. $XDG_CONFIG_HOME/weft/clusters.hcl
//   3. ~/.config/weft/clusters.hcl
//   4. ~/.weft/clusters.hcl
//
// Returns nil + nil error when no file is present so the caller can
// fall back to the --socket flag.
func LoadClustersConfig() (*ClustersConfig, error) {
	path := os.Getenv("WEFT_TUI_CONFIG")
	if path == "" {
		for _, p := range clusterConfigCandidates() {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path == "" {
		return nil, nil
	}
	var cfg ClustersConfig
	if err := hclsimple.DecodeFile(path, nil, &cfg); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &cfg, nil
}

// clusterConfigCandidates lists the default config-file paths in
// preference order. Empty entries (no $HOME) are filtered out.
func clusterConfigCandidates() []string {
	var paths []string
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		paths = append(paths, filepath.Join(x, "weft", "clusters.hcl"))
	}
	if home, _ := os.UserHomeDir(); home != "" {
		paths = append(paths,
			filepath.Join(home, ".config", "weft", "clusters.hcl"),
			filepath.Join(home, ".weft", "clusters.hcl"),
		)
	}
	return paths
}

// ResolveEndpoints expands a ClusterEntry into the concrete list the
// resilient connector iterates. Static endpoint blocks come first ;
// SRV-derived entries follow, ordered by (priority asc, weight desc).
// Per-endpoint SSH fields inherit the cluster's default_* values.
//
// SRV lookup failures are logged to stderr but don't abort — the
// static endpoints (if any) still work.
func ResolveEndpoints(c ClusterEntry) []Endpoint {
	out := make([]Endpoint, 0, len(c.Endpoints))
	for _, e := range c.Endpoints {
		out = append(out, mergeDefaults(c, e))
	}
	if c.SRVRecord != "" {
		srvs, err := lookupSRV(c.SRVRecord)
		if err != nil {
			fmt.Fprintf(os.Stderr, "weft-tui: SRV lookup %q failed (continuing with static endpoints): %v\n", c.SRVRecord, err)
		}
		for _, s := range srvs {
			out = append(out, mergeDefaults(c, EndpointEntry{
				Name:    fmt.Sprintf("srv:%s:%d", s.Target, s.Port),
				Address: fmt.Sprintf("%s:%d", strings.TrimSuffix(s.Target, "."), s.Port),
			}))
		}
	}
	return out
}

// mergeDefaults overlays cluster-level defaults onto an endpoint
// block where the per-host fields are empty.
func mergeDefaults(c ClusterEntry, e EndpointEntry) Endpoint {
	ep := Endpoint{
		Name:      e.Name,
		Address:   e.Address,
		Socket:    e.Socket,
		SSHKey:    e.SSHKey,
		SSHUser:   e.SSHUser,
		SSHSocket: e.SSHSocket,
	}
	if ep.SSHKey == "" {
		ep.SSHKey = c.DefaultSSHKey
	}
	if ep.SSHUser == "" {
		ep.SSHUser = c.DefaultSSHUser
	}
	if ep.SSHSocket == "" {
		ep.SSHSocket = c.DefaultSocket
	}
	// Expand ~ in path-like fields so the operator can use ~/.ssh/id_ed25519.
	ep.SSHKey = expandTilde(ep.SSHKey)
	ep.Socket = expandTilde(ep.Socket)
	return ep
}

// expandTilde rewrites a leading "~/" or "~" prefix to the user's
// home directory. Idempotent on absolute paths or "" inputs.
func expandTilde(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// lookupSRV is the net.LookupSRV thin wrapper that parses the
// _service._proto.name. shape and returns the records sorted by
// (priority asc, weight desc).
func lookupSRV(record string) ([]*net.SRV, error) {
	// "_weft._tcp.cluster.example.com" → service=weft, proto=tcp,
	// name=cluster.example.com.
	parts := strings.SplitN(record, ".", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed SRV record %q (want _service._proto.name)", record)
	}
	service := strings.TrimPrefix(parts[0], "_")
	proto := strings.TrimPrefix(parts[1], "_")
	name := parts[2]
	_, addrs, err := net.LookupSRV(service, proto, name)
	if err != nil {
		return nil, err
	}
	sort.Slice(addrs, func(i, j int) bool {
		if addrs[i].Priority != addrs[j].Priority {
			return addrs[i].Priority < addrs[j].Priority
		}
		return addrs[i].Weight > addrs[j].Weight
	})
	return addrs, nil
}

// String formats an endpoint for log lines. Hides the SSH key path
// (operator-private) but keeps everything else readable.
func (e Endpoint) String() string {
	parts := []string{}
	if e.Address != "" {
		parts = append(parts, e.Address)
	}
	if e.Socket != "" {
		parts = append(parts, "socket="+e.Socket)
	}
	if e.SSHSocket != "" {
		parts = append(parts, "ssh_socket="+e.SSHSocket)
	}
	if e.SSHUser != "" {
		parts = append(parts, "user="+e.SSHUser)
	}
	if e.Name != "" {
		return fmt.Sprintf("%s [%s]", e.Name, strings.Join(parts, " "))
	}
	return strings.Join(parts, " ")
}

// _ keeps strconv imported when the resolver doesn't need it (no
// integer parsing in the static-endpoint path).
var _ = strconv.Itoa

// resolveEndpointsFromConfig loads the operator's clusters.hcl,
// picks the requested cluster (or the first one when wanted is ""),
// and returns the resolved endpoint list. No-file or empty-config
// returns (nil, nil) so the caller falls back to the legacy
// --socket flow.
func resolveEndpointsFromConfig(wanted string) ([]Endpoint, error) {
	cfg, err := LoadClustersConfig()
	if err != nil {
		return nil, err
	}
	if cfg == nil || len(cfg.Clusters) == 0 {
		return nil, nil
	}
	chosen := cfg.Clusters[0]
	if wanted != "" {
		var found bool
		for _, c := range cfg.Clusters {
			if c.Name == wanted {
				chosen = c
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("cluster %q not found in clusters.hcl (have %s)", wanted, clusterNames(cfg.Clusters))
		}
	}
	return ResolveEndpoints(chosen), nil
}

func clusterNames(cs []ClusterEntry) string {
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return "<empty>"
	}
	out := names[0]
	for _, n := range names[1:] {
		out += ", " + n
	}
	return out
}
