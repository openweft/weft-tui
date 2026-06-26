// hostmetrics.go wires the TUI side of the per-host CPU/mem/network
// graphs feature. The weft-agent publishes JSON Samples to NATS
// subject `weft.host.<uuid>.metrics` every 5s (cf. weft/hostmetrics/
// Phase 1). This file :
//
//   - dials NATS at TUI startup (gated on WEFT_NATS_URL ; nil-safe
//     when the env is unset so dev / non-cluster invocations work),
//   - subscribes to weft.host.> with a wildcard so a single sub
//     covers the whole cluster,
//   - keeps a per-host ring buffer of the most recent samples,
//   - emits a metricsSampleMsg into the bubbletea Update loop on
//     every new sample so views (Hosts detail drawer) can re-render.
//
// The Sample wire format MUST stay byte-identical with weft's
// hostmetrics.Sample — both ends are independent modules, so the
// type is duplicated rather than imported to keep the TUI free of
// any agent-side build deps.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nats-io/nats.go"
)

// hostMetricsSample mirrors weft/hostmetrics.Sample. Field tags MUST
// match — bumps to the wire format require coordination with the
// agent side.
type hostMetricsSample struct {
	TsUnixNs      int64   `json:"ts_unix_ns"`
	HostUUID      string  `json:"host_uuid"`
	Hostname      string  `json:"hostname"`
	CPUPct        float64 `json:"cpu_pct"`
	MemUsedBytes  uint64  `json:"mem_used_bytes"`
	MemTotalBytes uint64  `json:"mem_total_bytes"`
	NetRxBps      uint64  `json:"net_rx_bps"`
	NetTxBps      uint64  `json:"net_tx_bps"`
}

// metricsSampleMsg is the tea.Msg the Update loop receives whenever
// a new sample lands on NATS. The bus's pump() goroutine fans every
// decoded sample through the same channel ; the Cmd drains one msg
// at a time and re-arms itself so we don't stall the loop.
type metricsSampleMsg hostMetricsSample

// hostMetricsRing is the per-host bounded buffer. capacity samples
// (default 720 = 1h @ 5s) is enough for the default 5min view +
// some headroom for the [/] hotkey zoom-out to 1h in a later
// iteration. Older samples roll off the front.
type hostMetricsRing struct {
	cap     int
	samples []hostMetricsSample
}

func newHostMetricsRing(cap int) *hostMetricsRing {
	if cap <= 0 {
		cap = 720
	}
	return &hostMetricsRing{cap: cap, samples: make([]hostMetricsSample, 0, cap)}
}

// add appends s. Drops the oldest entry when the buffer is at cap so
// memory stays bounded regardless of how long the TUI has been up.
func (r *hostMetricsRing) add(s hostMetricsSample) {
	if len(r.samples) >= r.cap {
		copy(r.samples, r.samples[1:])
		r.samples = r.samples[:r.cap-1]
	}
	r.samples = append(r.samples, s)
}

// tail returns the last n samples (or all of them if fewer exist).
// Used by the sparkline renderer to size its window without copying
// the whole buffer when only the last 60 are visible.
func (r *hostMetricsRing) tail(n int) []hostMetricsSample {
	if n <= 0 || n >= len(r.samples) {
		return r.samples
	}
	return r.samples[len(r.samples)-n:]
}

// hostMetricsBus owns the NATS subscription + per-host ring buffers.
// One instance per Model ; lifetime = lifetime of the TUI process.
// Methods are safe for concurrent use ; rings are protected by mu
// since the NATS callback runs on a library goroutine while the
// bubbletea Update loop reads them from the main goroutine.
type hostMetricsBus struct {
	nc      *nats.Conn
	sub     *nats.Subscription
	samples chan hostMetricsSample

	mu    sync.Mutex
	rings map[string]*hostMetricsRing
}

// newHostMetricsBus dials NATS using the URL from WEFT_NATS_URL.
// Returns a non-nil bus even when the env is empty or the dial
// fails — in that case nc/sub stay nil and tea.Cmd just blocks on
// the (empty) channel, which never fires. Keeps the call site free
// of nil-checks.
func newHostMetricsBus(log *slog.Logger) *hostMetricsBus {
	b := &hostMetricsBus{
		samples: make(chan hostMetricsSample, 128),
		rings:   make(map[string]*hostMetricsRing),
	}
	url := os.Getenv("WEFT_NATS_URL")
	if url == "" {
		return b
	}
	nc, err := nats.Connect(url, nats.Name("weft-tui.hostmetrics"))
	if err != nil {
		log.Warn("hostmetrics bus: NATS dial failed", "url", url, "err", err)
		return b
	}
	sub, err := nc.Subscribe("weft.host.*.metrics", func(m *nats.Msg) {
		var s hostMetricsSample
		if err := json.Unmarshal(m.Data, &s); err != nil {
			return
		}
		// Drop on full channel rather than block the NATS callback —
		// the TUI is slow to drain (renders happen on user input),
		// and the ring buffer below catches up via direct insert.
		b.mu.Lock()
		r, ok := b.rings[s.HostUUID]
		if !ok {
			r = newHostMetricsRing(720)
			b.rings[s.HostUUID] = r
		}
		r.add(s)
		b.mu.Unlock()
		select {
		case b.samples <- s:
		default:
		}
	})
	if err != nil {
		log.Warn("hostmetrics bus: NATS subscribe failed", "err", err)
		nc.Close()
		return b
	}
	b.nc = nc
	b.sub = sub
	return b
}

// ring returns the rolling-window buffer for a host, or nil when no
// samples have arrived yet. Caller MUST treat the returned slice as
// read-only and copy if they need to retain it past the next add().
func (b *hostMetricsBus) ring(uuid string) *hostMetricsRing {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rings[uuid]
}

// close releases the NATS connection. Idempotent.
func (b *hostMetricsBus) close() {
	if b.sub != nil {
		_ = b.sub.Unsubscribe()
	}
	if b.nc != nil {
		b.nc.Close()
	}
}

// nextMetricsSample is the bubbletea Cmd that pumps one sample from
// the bus into the Update loop. The Cmd blocks until either a sample
// arrives or 2s elapses (so the loop doesn't sit forever on a quiet
// cluster — re-arming costs nothing). Update should ALWAYS re-arm
// this Cmd in its return tuple : otherwise samples accumulate in the
// channel without ever reaching a view.
func (b *hostMetricsBus) nextMetricsSample() tea.Cmd {
	return func() tea.Msg {
		select {
		case s := <-b.samples:
			return metricsSampleMsg(s)
		case <-time.After(2 * time.Second):
			return metricsSampleMsg{} // sentinel : empty UUID = no new sample
		}
	}
}

// ---- Sparkline renderer ------------------------------------------

// sparkChars is the 8-level UTF-8 block ramp every modern terminal
// handles. Index 0 = lowest, 7 = highest. Used by drawSparkline to
// map a normalised float to a char.
var sparkChars = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// drawSparkline renders n samples as a width-char-wide Unicode block
// sparkline normalised by max. Empty / all-zero input returns width
// space-padded so callers can use the result inline without
// alignment surprises. max=0 → auto-scale (use observed peak) ;
// otherwise the value clips at max so unusual spikes don't crush
// the rest of the curve.
func drawSparkline(values []float64, width int, max float64) string {
	if width <= 0 {
		return ""
	}
	if len(values) == 0 {
		return strings.Repeat(" ", width)
	}
	// Right-align : show the most recent N samples up to width.
	if len(values) > width {
		values = values[len(values)-width:]
	}
	if max <= 0 {
		for _, v := range values {
			if v > max {
				max = v
			}
		}
	}
	if max == 0 {
		// Every value is zero — render a flat baseline so the user
		// sees the line is "present but quiet" rather than "missing".
		return strings.Repeat(string(sparkChars[0]), len(values)) + strings.Repeat(" ", width-len(values))
	}
	var b strings.Builder
	b.Grow(width)
	// Left-pad with spaces if we have fewer samples than width so
	// the curve sticks to the RIGHT edge (= most recent on the right,
	// historical on the left). Matches Grafana / iftop ergonomics.
	for i := 0; i < width-len(values); i++ {
		b.WriteByte(' ')
	}
	for _, v := range values {
		if v < 0 || math.IsNaN(v) {
			v = 0
		}
		ratio := v / max
		if ratio > 1 {
			ratio = 1
		}
		idx := int(ratio * float64(len(sparkChars)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparkChars) {
			idx = len(sparkChars) - 1
		}
		b.WriteRune(sparkChars[idx])
	}
	return b.String()
}

// formatBps prints a rate in IEC-ish units with one decimal :
// 1234 → "1.2 KB/s", 1000000 → "976 KB/s", 12345678 → "11.8 MB/s".
// Keeps the column-width predictable (≤9 chars including suffix)
// so the sparkline grid doesn't drift between rows.
func formatBps(v uint64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case v >= gb:
		return fmt.Sprintf("%.1f GB/s", float64(v)/gb)
	case v >= mb:
		return fmt.Sprintf("%.1f MB/s", float64(v)/mb)
	case v >= kb:
		return fmt.Sprintf("%.1f KB/s", float64(v)/kb)
	default:
		return fmt.Sprintf("%d B/s", v)
	}
}

// ---- Detail-drawer integration helper ---------------------------

// renderHostMetricsBlock builds the four-row sparkline block that
// gets appended to the Hosts detail drawer. Returns "" when no
// samples have arrived yet for this host so callers can skip the
// section header. width is the available sparkline width in chars.
func renderHostMetricsBlock(theme Theme, ring *hostMetricsRing, width int) string {
	if ring == nil || len(ring.samples) == 0 {
		return ""
	}
	if width < 16 {
		width = 16
	}
	samples := ring.tail(width)
	cpu := make([]float64, len(samples))
	mem := make([]float64, len(samples))
	rxs := make([]float64, len(samples))
	txs := make([]float64, len(samples))
	for i, s := range samples {
		cpu[i] = s.CPUPct
		if s.MemTotalBytes > 0 {
			mem[i] = float64(s.MemUsedBytes) / float64(s.MemTotalBytes) * 100
		}
		rxs[i] = float64(s.NetRxBps)
		txs[i] = float64(s.NetTxBps)
	}
	last := samples[len(samples)-1]
	memPct := 0.0
	if last.MemTotalBytes > 0 {
		memPct = float64(last.MemUsedBytes) / float64(last.MemTotalBytes) * 100
	}
	// Per-row labels are fixed-width 16 chars so the sparkline grid
	// stays aligned no matter the live value.
	rows := []struct {
		label, val, line string
	}{
		{"CPU", fmt.Sprintf("%5.1f %%", last.CPUPct), drawSparkline(cpu, width, 100)},
		{"Memory", fmt.Sprintf("%5.1f %%", memPct), drawSparkline(mem, width, 100)},
		{"Net rx", formatBps(last.NetRxBps), drawSparkline(rxs, width, 0)},
		{"Net tx", formatBps(last.NetTxBps), drawSparkline(txs, width, 0)},
	}
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(theme.StatusKey.Render(padKey("Metrics")))
	b.WriteString("\n")
	for _, r := range rows {
		b.WriteString("  ")
		b.WriteString(theme.StatusKey.Render(padKey(r.label)))
		b.WriteString("  ")
		b.WriteString(theme.Faint.Render(r.line))
		b.WriteString("  ")
		b.WriteString(r.val)
		b.WriteString("\n")
	}
	return b.String()
}

// listenSamples is a slim wrapper that lets the Model start the
// pumping goroutine without referencing tea internals from the bus.
// Kept here for symmetry with other Cmd-returning helpers in the
// TUI ; the bus could also be plumbed via the bubbletea Manager
// pattern but the per-call Cmd reads cleaner.
func (b *hostMetricsBus) listenSamples(_ context.Context) tea.Cmd {
	return b.nextMetricsSample()
}
