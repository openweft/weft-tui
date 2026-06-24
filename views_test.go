package main

// views_test.go covers the pure-rendering helpers in hosts.go + vms.go
// + the sidebar plumbing in app.go. Each helper has at least one
// happy-path test + at least one regression-catching edge case.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	weftv1 "github.com/openweft/weft-proto"
)

// ---- vms.go ----

// TestShortImage_StripsRegistryPrefix locks down the OCI ref shortener
// the IMAGE column uses. A regression that returned the full ref would
// blow the column width budget + push columns off-screen on 80-col
// terminals.
func TestShortImage_StripsRegistryPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"ghcr.io/openweft/weft-etcd:v3.6.0", "weft-etcd:v3.6.0"},
		{"weft-etcd:v3.6.0", "weft-etcd:v3.6.0"},
		{"microvm/direct_linux", "direct_linux"},
		{"single", "single"},
		// Trailing slash : pass through unchanged.
		{"foo/bar/", "foo/bar/"},
	}
	for _, tc := range cases {
		got := shortImage(tc.in)
		if got != tc.want {
			t.Errorf("shortImage(%q) = %q ; want %q", tc.in, got, tc.want)
		}
	}
}

// TestVMStateString_CoversEveryState is the regression net for the
// state→label map. The user explicitly asked for "named states, never
// dashes". Every wire VMState value must produce a non-empty
// human-readable label.
func TestVMStateString_CoversEveryState(t *testing.T) {
	all := []weftv1.VMState{
		weftv1.VMState_VM_STATE_UNSPECIFIED,
		weftv1.VMState_VM_STATE_NOT_CREATED,
		weftv1.VMState_VM_STATE_STOPPED,
		weftv1.VMState_VM_STATE_RUNNING,
		weftv1.VMState_VM_STATE_ERROR,
		weftv1.VMState_VM_STATE_CREATED,
		weftv1.VMState_VM_STATE_STARTING,
		weftv1.VMState_VM_STATE_STOPPING,
		weftv1.VMState_VM_STATE_ZOMBIE,
		weftv1.VMState_VM_STATE_DELETING,
	}
	for _, s := range all {
		label := vmStateString(s)
		if label == "" {
			t.Errorf("VMState %v rendered as empty string", s)
		}
		if label == "-" || label == "—" {
			t.Errorf("VMState %v rendered as dash, regression on named-state contract", s)
		}
	}
}

// TestAZColor_DeterministicByName is the regression net for the AZ
// coloured marker : two VMs in the same AZ must always render the
// same colour, so the operator can scan the AZ column visually.
// FNV-1a is pure ; a regression that injected non-determinism (rand,
// map iteration order) would break this.
func TestAZColor_DeterministicByName(t *testing.T) {
	// Compare the underlying style's foreground spec, NOT the
	// rendered "●". Tests run without a TTY → lipgloss strips
	// colour codes + every Render() collapses to the same plain
	// string.
	a1 := azColor("dc1").GetForeground()
	a2 := azColor("dc1").GetForeground()
	if a1 != a2 {
		t.Fatal("azColor('dc1') not stable across calls — regression")
	}
	// And two distinct AZ names should usually produce different
	// colour specs. 6-colour palette ; with 3 standard names
	// ("dc1"/"dc2"/"dc3") at least one pair must differ.
	b := azColor("dc2").GetForeground()
	c := azColor("dc3").GetForeground()
	if a1 == b && b == c {
		t.Fatal("azColor collapsed dc1/dc2/dc3 to one colour spec — regression")
	}
}

// TestAZBadge_EmptyAZRendersDash : an empty AZ string must NOT emit a
// coloured bullet (it'd look like a "real" AZ on the row). The empty
// case should be the explicit em-dash placeholder.
func TestAZBadge_EmptyAZRendersDash(t *testing.T) {
	theme := NewTheme()
	got := azBadge(theme, "")
	if got != "—" {
		t.Errorf("azBadge(empty) = %q ; want em-dash", got)
	}
	got2 := azBadge(theme, "dc1")
	if !strings.Contains(got2, "dc1") {
		t.Errorf("azBadge(dc1) doesn't carry the AZ name : %q", got2)
	}
	if !strings.Contains(got2, "●") {
		t.Errorf("azBadge(dc1) doesn't carry the bullet : %q", got2)
	}
}

// ---- hosts.go ----

// TestFormatRAM_UnitsAndRounding covers the MiB→human pretty-print
// used in the RAM column. Threshold == 1024 ; below stays "M MiB",
// above flips to "G GiB". 0 / negative → "".
func TestFormatRAM_UnitsAndRounding(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, ""},
		{-1, ""},
		{512, "512 MiB"},
		{1023, "1023 MiB"},
		{1024, "1 GiB"},
		{8192, "8 GiB"},
		{32768, "32 GiB"},
	}
	for _, tc := range cases {
		got := formatRAM(tc.in)
		if got != tc.want {
			t.Errorf("formatRAM(%d) = %q ; want %q", tc.in, got, tc.want)
		}
	}
}

// TestFormatGPUs_CountAndSKU covers the GPU column collapsing logic.
// Same-(vendor,model) GPUs aggregate to "model×count" ; multi-SKU
// hosts join with " + ". Empty slice → "".
func TestFormatGPUs_CountAndSKU(t *testing.T) {
	cases := []struct {
		name string
		in   []hostsGPU
		want string
	}{
		{"empty", nil, ""},
		{"single", []hostsGPU{{Vendor: "NVIDIA", Model: "H200"}}, "H200"},
		{"quad-same", []hostsGPU{
			{Model: "H200"}, {Model: "H200"}, {Model: "H200"}, {Model: "H200"},
		}, "H200×4"},
		{"mixed", []hostsGPU{
			{Model: "H200"}, {Model: "H200"},
			{Model: "RTX-6000-Ada"},
		}, "H200×2 + RTX-6000-Ada"},
		{"vendor-only-fallback", []hostsGPU{{Vendor: "AMD"}}, "AMD"},
	}
	for _, tc := range cases {
		got := formatGPUs(tc.in)
		if got != tc.want {
			t.Errorf("%s : formatGPUs = %q ; want %q", tc.name, got, tc.want)
		}
	}
}

// TestFormatDriverVersions_StableOrder confirms the kind→version map
// is rendered with sorted keys. Map iteration order is random ; a
// regression that dropped the sort would make the DRIVERS column
// flicker across refreshes.
func TestFormatDriverVersions_StableOrder(t *testing.T) {
	in := map[string]string{
		"vz":   "v0.5.0",
		"qemu": "v0.6.0",
	}
	// Render twice ; both should be identical.
	a := formatDriverVersions(in)
	for i := 0; i < 10; i++ {
		if formatDriverVersions(in) != a {
			t.Fatal("formatDriverVersions order is non-deterministic")
		}
	}
	// And kinds should be sorted lexicographically : "qemu" before "vz".
	if !strings.HasPrefix(a, "qemu:") {
		t.Fatalf("expected qemu first (sorted) ; got %q", a)
	}
	if formatDriverVersions(nil) != "" {
		t.Fatal("empty map should render empty string")
	}
}

// ---- app.go : sidebar + helpers ----

// TestSidebarSections_NonEmpty verifies the catalogue is wired into
// the sidebar : it must surface at least the core entries + a
// non-trivial number of resource sections. A regression that broke
// the resourceCatalogue iteration would shrink the sidebar to the
// 4 core tabs alone.
func TestSidebarSections_NonEmpty(t *testing.T) {
	secs := sidebarSections()
	if len(secs) < 2 {
		t.Fatalf("expected at least 2 sections ; got %d", len(secs))
	}
	// Aggregate entries : must include Network / Storage / Compute /
	// Identity / Admin among the section headers. Core is no longer
	// expected — Hosts/VMs/Projects relocated long ago and Events
	// moved to the log-pane tabbar 2026-06-24, leaving Core empty
	// and elided.
	headers := map[string]bool{}
	for _, s := range secs {
		headers[s.Header] = true
	}
	for _, must := range []string{"Network", "Storage", "Compute"} {
		if !headers[must] {
			t.Errorf("sidebar missing section %q", must)
		}
	}
}

// TestEntryClickable_FiltersHints distinguishes targetable entries
// from keyboard-hint rows. A regression that returned true for "^P"
// (palette) or "?" (help) would route clicks to a no-op handler +
// confuse operators ; one that returned false for the Hosts tab (the
// iota=0 case) would block clicking it.
func TestEntryClickable_FiltersHints(t *testing.T) {
	cases := []struct {
		e    sidebarEntry
		want bool
		name string
	}{
		{sidebarEntry{tab: 0, shortcut: "1", label: "Hosts"}, true, "core Hosts (iota=0)"},
		{sidebarEntry{tab: 1, shortcut: "2", label: "VMs"}, true, "core VMs"},
		{sidebarEntry{resourceID: "vm", shortcut: "·", label: "VMs"}, true, "resource entry"},
		{sidebarEntry{shortcut: "^P", label: "Palette"}, false, "palette hint"},
		{sidebarEntry{shortcut: "?", label: "Help"}, false, "help hint"},
	}
	for _, tc := range cases {
		if got := entryClickable(tc.e); got != tc.want {
			t.Errorf("%s : entryClickable = %v ; want %v", tc.name, got, tc.want)
		}
	}
}

// TestStripANSI_RemovesCSI : sidebar hit detection scans rendered
// lines for label substrings ; the active row carries CSI colour
// codes around the text. A regression in stripANSI would make the
// hit-row map return nothing on the currently-active tab.
func TestStripANSI_RemovesCSI(t *testing.T) {
	in := "\x1b[1;36mHosts\x1b[0m"
	got := stripANSI(in)
	if got != "Hosts" {
		t.Fatalf("stripANSI(%q) = %q ; want 'Hosts'", in, got)
	}
	// No-ANSI input passes through untouched.
	if stripANSI("plain") != "plain" {
		t.Fatal("stripANSI corrupted plain string")
	}
	// Multi-CSI : strip them all.
	multi := "\x1b[31mr\x1b[32mg\x1b[34mb"
	if stripANSI(multi) != "rgb" {
		t.Fatalf("stripANSI multi-CSI = %q ; want 'rgb'", stripANSI(multi))
	}
}

// TestMouseClick_SidebarHonorsTopbarOffset is the regression net for
// the 2026-06-23 click-mis-targeting bug : when the topbar was added,
// the sidebar's Y coordinates stayed relative (renderSidebar starts
// at Y=0) but mouse events deliver ABSOLUTE terminal Y. Forgetting
// to subtract topbarHeight() in handleMouse made every click land
// one row above the row the operator pointed at.
//
// The test :
//   1. Builds a Model + drives a WindowSizeMsg so geometry is set.
//   2. Asks sidebarHitRows() for the relative Y of the VMs entry.
//   3. Sends a MouseMsg at ABSOLUTE Y = relativeY + topbarHeight().
//   4. Asserts the active tab flipped to VMs.
//
// A future regression that drops the topbarHeight() subtraction (or
// drops it again on log-pane wheel handling, etc.) flips this test
// to failure.
func TestMouseClick_SidebarHonorsTopbarOffset(t *testing.T) {
	m := New(nil)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = mm.(Model)
	// Find the absolute Y for the VMs sidebar entry.
	hits := m.sidebarHitRows()
	var vmsRelativeY int = -1
	for y, e := range hits {
		if e.label == "VMs" {
			vmsRelativeY = y
			break
		}
	}
	if vmsRelativeY < 0 {
		t.Fatal("VMs entry not in sidebarHitRows ; sidebar layout broken")
	}
	if m.ActiveTab() == int(tabVMs) {
		t.Fatal("test premise broken : starting on VMs already")
	}
	// Click at absolute Y = topbarHeight + relative ; X anywhere inside
	// the sidebar column (X=2 sits just past the rounded border).
	absoluteY := vmsRelativeY + m.topbarHeight()
	mm, _ = m.Update(tea.MouseMsg(tea.MouseEvent{
		X:      2,
		Y:      absoluteY,
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionRelease,
	}))
	m = mm.(Model)
	if got := m.ActiveTab(); got != int(tabVMs) {
		t.Errorf("click at absolute Y=%d landed on tab %d, want tabVMs (%d) — likely topbarHeight() was forgotten",
			absoluteY, got, tabVMs)
	}
}

// TestContextMenu_OverlaysOverBodyNotBottom pins the 2026-06-23
// directive : "le menu contextuel est a afficher en fenetre overflow,
// pas en bas". Renders View() with the menu open + asserts the menu
// items appear in the upper half (next to the selected row), not in
// the last few lines (where the previous "bottom strip" rendering
// placed them).
func TestContextMenu_OverlaysOverBodyNotBottom(t *testing.T) {
	m := New(&fakeClient{})
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = mm.(Model)
	// Switch to VMs tab + open menu.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = mm.(Model)
	// Inject a row so vms.selected() returns non-empty + the menu
	// builds.
	m.vms.rows = []vmRow{{Name: "vm-a", Project: "p"}}
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	m = mm.(Model)
	if !m.menu.open {
		t.Fatal("menu didn't open after `m` key")
	}

	view := m.View()
	lines := strings.Split(view, "\n")
	// Find the line carrying "Start  [s]" (the first menu item).
	startY := -1
	for i, l := range lines {
		if strings.Contains(stripANSI(l), "Start") && strings.Contains(stripANSI(l), "[s]") {
			startY = i
			break
		}
	}
	if startY < 0 {
		t.Fatal("menu's Start item not visible in View()")
	}
	// "Upper half" check : the menu should land in the top 60% of
	// the rendered output, not the bottom 30%. (For h=40, ≤24.)
	maxOK := len(lines) * 3 / 5
	if startY > maxOK {
		t.Errorf("menu's Start item at Y=%d ; expected ≤ %d (upper half) — menu is rendering as a bottom strip instead of an overlay",
			startY, maxOK)
	}
}

// TestTopbarHeight_MatchesRender pins the contract topbarHeight() ==
// lipgloss.Height(renderTopbar()). Once we wrap the topbar in a
// bordered box, topbarHeight() will return 3 instead of 1 — and
// every mouse-Y subtraction must follow. Locking this here so a
// silent border-wrap doesn't drift the contract.
func TestTopbarHeight_MatchesRender(t *testing.T) {
	m := New(nil)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = mm.(Model)
	want := strings.Count(m.renderTopbar(), "\n") + 1
	if got := m.topbarHeight(); got != want {
		t.Errorf("topbarHeight() = %d ; want %d (number of lines in renderTopbar)", got, want)
	}
}

// TestView_FitsTerminalHeight is the regression net for the height
// off-by-N bug : if the View() output exceeds m.height, the alt-screen
// scrolls + the top of the frame disappears. The Model has three
// stackable components (sidebar/body row, log pane, status bar) and
// each has subtle border accounting (StatusBar.BorderTop adds 1, the
// SidebarBox / BodyBox rounded borders add 2 each). Any future style
// change that grows the chrome must come with a bodyHeight() update,
// caught here.
func TestView_FitsTerminalHeight(t *testing.T) {
	for _, h := range []int{30, 40, 60, 80} {
		m := New(nil)
		mm, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: h})
		m = mm.(Model)
		out := m.View()
		lines := strings.Count(out, "\n") + 1
		if lines > h {
			t.Errorf("View at %dx%d rendered %d lines (overflows by %d) — alt-screen will scroll, top of TUI cut off",
				120, h, lines, lines-h)
		}
	}
}

// TestSidebarWidth_HonorsResize verifies the drag-resize state takes
// effect : Model.sidebarW != 0 wins over the default. A regression
// that ignored the field would render the sidebar at a fixed width
// regardless of the operator's drag.
func TestSidebarWidth_HonorsResize(t *testing.T) {
	m := New(nil)
	// Default — unset sidebarW must return the constant.
	if m.sidebarWidth() != defaultSidebarWidth {
		t.Fatalf("default sidebar width = %d ; want %d", m.sidebarWidth(), defaultSidebarWidth)
	}
	m.sidebarW = 40
	if m.sidebarWidth() != 40 {
		t.Fatalf("sidebarWidth after resize = %d ; want 40", m.sidebarWidth())
	}
}
