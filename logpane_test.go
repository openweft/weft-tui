package main

// logpane_test.go covers the scrollable diagnostic strip rendered
// between the body and the status bar. The pane sees every
// connect/dial event from the resilient client + has its own scroll
// budget so a noisy failover doesn't move the rest of the TUI.

import (
	"strings"
	"testing"
	"time"
)

// TestLogPane_AppendRingBuffer locks down the ring-buffer eviction :
// once capacity is hit, the oldest entry drops off + the rest shifts.
// A regression that grew the buffer unbounded would leak memory in a
// long-running TUI session with a flapping link.
func TestLogPane_AppendRingBuffer(t *testing.T) {
	p := newLogPane(80)
	// Stamp deterministic timestamps so the test isn't time-sensitive.
	orig := now
	now = func() time.Time { return time.Date(2026, 6, 22, 18, 0, 0, 0, time.UTC) }
	defer func() { now = orig }()

	// Overflow the cap by 5.
	for i := 0; i < logPaneCapacity+5; i++ {
		p.append(ResilientEventInfo, "msg")
	}
	if len(p.entries) != logPaneCapacity {
		t.Fatalf("buffer size %d ; want %d", len(p.entries), logPaneCapacity)
	}
}

// TestLogPane_AppendRendersInViewport ensures the appended line is
// visible in the viewport content. A regression that forgot to call
// refresh() would leave the pane stuck on its initial empty state
// even though entries kept arriving.
func TestLogPane_AppendRendersInViewport(t *testing.T) {
	p := newLogPane(80)
	orig := now
	now = func() time.Time { return time.Date(2026, 6, 22, 18, 30, 0, 0, time.UTC) }
	defer func() { now = orig }()

	p.append(ResilientEventInfo, "connected to dc1-r1-h1")
	content := p.vp.View()
	if !strings.Contains(content, "connected to dc1-r1-h1") {
		t.Fatalf("appended line not in viewport content : %q", content)
	}
	if !strings.Contains(content, "18:30:00") {
		t.Fatalf("timestamp missing : %q", content)
	}
}

// TestLogPane_HeightAccountsForBorder confirms .height() ==
// content height + 2 (LogPaneBox top/bottom border) + 3 (framed
// tab strip : top border + label + bottom border). bodyHeight()
// uses this number to resize the table region ; a wrong value
// would either overlap or leave a gap.
func TestLogPane_HeightAccountsForBorder(t *testing.T) {
	p := newLogPane(80)
	// 2 outer LogPaneBox border + 2 tab strip (top border + label)
	// + 1 stitched rule (combines tab bottoms + pane separator)
	// = 5 lines of chrome.
	want := logPaneDefaultHeight + 5
	if p.height() != want {
		t.Fatalf("logPane.height() = %d ; want %d (content + 5 chrome)", p.height(), want)
	}
}

// TestLogPane_TabHitX maps X coordinates to tab IDs. Pinned so a
// future refactor of the tab box widths doesn't silently break the
// click-to-switch behaviour.
//
// Layout : 1 col lead-indent, tabs WELDED (no gap). Each shared
// boundary col carries the RIGHT tab's `╭` (top) and `┴` (bottom),
// the LEFT tab has no `╮` of its own. Only the LAST tab has the
// trailing `╮`. Per-tab span = `╭` + innerW (= label + 2 padding).
//
// Logs      : 1 + 6 = 7 cols (cols 1..7)
// Events    : 1 + 8 = 9 cols (cols 8..16)
// Terminal  : 1 + 10 = 11 cols (cols 17..27)
// Bookmarks : 1 + 11 + 1 (trailing `╮`) = 13 cols (cols 28..40)
func TestLogPane_TabHitX(t *testing.T) {
	p := newLogPane(80)
	cases := []struct {
		x    int
		want string
	}{
		{-1, ""},
		{0, ""}, // lead indent
		{1, "logs"},
		{7, "logs"},
		{8, "events"},
		{16, "events"},
		{17, "terminal"},
		{27, "terminal"},
		{28, "bookmarks"},
		{40, "bookmarks"},
		{41, ""}, // past
	}
	for _, tc := range cases {
		got := p.tabHitX(tc.x)
		if got != tc.want {
			t.Errorf("tabHitX(%d) = %q ; want %q", tc.x, got, tc.want)
		}
	}
}

// TestLogPane_SwitchTab verifies the click-driven activeTab update
// is idempotent + only honours known tab IDs.
func TestLogPane_SwitchTab(t *testing.T) {
	p := newLogPane(80)
	if p.activeTab != "logs" {
		t.Fatalf("default activeTab = %q ; want logs", p.activeTab)
	}
	p.switchTab("terminal")
	if p.activeTab != "terminal" {
		t.Errorf("after switchTab(terminal) : activeTab = %q", p.activeTab)
	}
	p.switchTab("nonsense") // unknown ID, no-op
	if p.activeTab != "terminal" {
		t.Errorf("switchTab(unknown) shouldn't change activeTab ; got %q", p.activeTab)
	}
	p.switchTab("logs")
	if p.activeTab != "logs" {
		t.Errorf("after switchTab(logs) : activeTab = %q", p.activeTab)
	}
}

// TestLogPane_ResizeUpdatesViewport tracks the WindowSizeMsg path :
// the parent layout resizes the pane, the viewport width must follow.
// Without this the pane content would clip on narrower terminals
// + wrap awkwardly on wider ones.
func TestLogPane_ResizeUpdatesViewport(t *testing.T) {
	p := newLogPane(80)
	p.resize(120)
	if p.vp.Width != 120 {
		t.Fatalf("viewport width after resize = %d ; want 120", p.vp.Width)
	}
	// Minimum guard : refuse to go below 10 cols (the floor in newLogPane).
	p.resize(2)
	if p.vp.Width < 10 {
		t.Fatalf("viewport width should be floored at 10, got %d", p.vp.Width)
	}
}
