package main

// clusterconfig_test.go covers the cluster-config loader + endpoint
// resolution paths that previously had zero coverage. These are the
// hot path for `weft-tui --cluster=…` so a regression here breaks the
// entire failover flow before the operator sees a single screen.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExpandTilde_Forms covers the four shapes of a leading-tilde path :
// empty, no-tilde, bare "~", "~/<rest>". A miss on any of these makes
// the SSH key resolver pass a literal "~" path to ReadFile + the dial
// fails with a confusing error.
func TestExpandTilde_Forms(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no HOME — skipping tilde expansion test")
	}
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/abs/path", "/abs/path"},
		{"./rel", "./rel"},
		{"~", home},
		{"~/", home},
		{"~/.ssh/id_ed25519", filepath.Join(home, ".ssh/id_ed25519")},
		// Leading ~ on a different user — current impl returns as-is
		// (we don't try to resolve ~other) ; assert the no-op.
		{"~root/x", "~root/x"},
	}
	for _, tc := range cases {
		got := expandTilde(tc.in)
		if got != tc.want {
			t.Errorf("expandTilde(%q) = %q ; want %q", tc.in, got, tc.want)
		}
	}
}

// TestMergeDefaults_Inheritance verifies the cluster-level defaults
// fill in the empty per-endpoint fields, and that per-endpoint values
// win when both are set. Without this, every endpoint would forget
// the default ssh_key/ssh_user/default_socket and dials would fail.
func TestMergeDefaults_Inheritance(t *testing.T) {
	c := ClusterEntry{
		DefaultSSHKey:  "/tmp/test-key",
		DefaultSSHUser: "admin",
		DefaultSocket:  "/home/admin/.weft/weft.sock",
	}
	// Endpoint with no overrides : everything should inherit.
	ep := mergeDefaults(c, EndpointEntry{Name: "h1", Address: "dc1-r1-h1"})
	if ep.SSHKey != "/tmp/test-key" || ep.SSHUser != "admin" || ep.SSHSocket != "/home/admin/.weft/weft.sock" {
		t.Fatalf("inheritance failed : %+v", ep)
	}
	// Endpoint with per-host override : the override must win.
	ep2 := mergeDefaults(c, EndpointEntry{
		Name: "h2", Address: "dc2", SSHUser: "root", SSHKey: "/etc/keys/k", SSHSocket: "/var/run/weft.sock",
	})
	if ep2.SSHUser != "root" || ep2.SSHKey != "/etc/keys/k" || ep2.SSHSocket != "/var/run/weft.sock" {
		t.Fatalf("override failed : %+v", ep2)
	}
}

// TestLoadClustersConfig_FileDecodes verifies the HCL decoder accepts
// the documented schema. A schema-change regression that broke field
// names would let the file load but produce empty endpoints — caught
// here by the field-by-field assertion.
func TestLoadClustersConfig_FileDecodes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.hcl")
	body := `
cluster "live" {
  default_ssh_key  = "/home/admin/.ssh/id_ed25519"
  default_ssh_user = "admin"
  default_socket   = "/home/admin/.weft/weft.sock"

  endpoint "dc1" { address = "admin@dc1-r1-h1" }
  endpoint "dc2" { address = "admin@dc2-r1-h1" }
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEFT_TUI_CONFIG", path)

	cfg, err := LoadClustersConfig()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg == nil || len(cfg.Clusters) != 1 {
		t.Fatalf("want 1 cluster, got %+v", cfg)
	}
	c := cfg.Clusters[0]
	if c.Name != "live" || c.DefaultSSHUser != "admin" || len(c.Endpoints) != 2 {
		t.Fatalf("schema regression : %+v", c)
	}
	if c.Endpoints[0].Address != "admin@dc1-r1-h1" {
		t.Fatalf("endpoint0 address : %q", c.Endpoints[0].Address)
	}
}

// TestLoadClustersConfig_NoFile returns (nil, nil) so main.go can fall
// back to the legacy --socket flag. A regression here would either
// crash the TUI with a stat-error or force the operator to create a
// dummy config to launch the legacy flow.
func TestLoadClustersConfig_NoFile(t *testing.T) {
	t.Setenv("WEFT_TUI_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // empty XDG dir
	t.Setenv("HOME", t.TempDir())            // empty HOME dir
	cfg, err := LoadClustersConfig()
	if err != nil {
		t.Fatalf("unexpected err on missing file : %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil cfg on missing file, got %+v", cfg)
	}
}

// TestResolveEndpointsFromConfig_ChoosesByName confirms the
// --cluster=<name> selector lands on the right block, and that an
// unknown name surfaces a helpful error (instead of silently picking
// cluster[0] like a regression would).
func TestResolveEndpointsFromConfig_ChoosesByName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.hcl")
	body := `
cluster "prod" {
  default_socket = "/tmp/p.sock"
  endpoint "p1" { address = "admin@prod-r1-h1" }
}
cluster "dev" {
  default_socket = "/tmp/d.sock"
  endpoint "d1" { address = "admin@dev-r1-h1" }
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEFT_TUI_CONFIG", path)

	eps, err := resolveEndpointsFromConfig("dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].Address != "admin@dev-r1-h1" {
		t.Fatalf("wrong cluster picked : %+v", eps)
	}
	if _, err := resolveEndpointsFromConfig("ghost"); err == nil {
		t.Fatal("expected error on unknown cluster name")
	}
}

// TestResolveEndpointsFromConfig_DefaultsToFirst is the empty-name
// case : without --cluster the TUI picks the first block. Operators
// rely on this so a single-cluster config doesn't need a flag.
func TestResolveEndpointsFromConfig_DefaultsToFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.hcl")
	body := `
cluster "first" {
  endpoint "a" { address = "admin@a" }
}
cluster "second" {
  endpoint "b" { address = "admin@b" }
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEFT_TUI_CONFIG", path)
	eps, err := resolveEndpointsFromConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].Address != "admin@a" {
		t.Fatalf("expected first cluster, got %+v", eps)
	}
}

// TestEndpoint_StringRedactsKey makes sure SSHKey is NOT printed
// (operator-private). A regression that started logging the key path
// could leak it to journals + screenshots.
func TestEndpoint_StringRedactsKey(t *testing.T) {
	ep := Endpoint{
		Name: "h1", Address: "admin@h1", SSHKey: "/home/admin/.ssh/secret_key", SSHUser: "admin",
	}
	s := ep.String()
	if strings.Contains(s, "secret_key") {
		t.Fatalf("SSH key leaked into String() : %q", s)
	}
}
