package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// maxEventLines caps the in-memory event ring. 500 keeps a few
// minutes of a busy cluster while staying well under the viewport's
// reasonable rendering budget. Older lines drop off the top.
const maxEventLines = 500

// EventsClient is the narrow gRPC surface the Events tab needs : a
// server-streaming WatchEvents. Exported as an interface so the
// tests can pump synthetic events without a real socket.
type EventsClient interface {
	WatchEvents(ctx context.Context, in *weftv1.WatchEventsRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[weftv1.PlatformEvent], error)
}

// eventsModel owns the Events tab : a viewport showing a tail of
// formatted PlatformEvents, plus the goroutine bookkeeping for the
// long-running WatchEvents stream. Pause/resume is honoured *in the
// model* — the upstream gRPC stream stays open ; we just stop
// appending to the buffer so the operator can read.
type eventsModel struct {
	theme   Theme
	vp      viewport.Model
	lines   []string
	paused  bool
	started bool
	err     error

	// userScrolled is set the first time the user scrolls away from
	// the bottom. While true, new events still append but we don't
	// auto-scroll — let them read. Cleared on `c` (clear) and
	// whenever they manually jump back to the bottom.
	userScrolled bool
}

func newEventsModel(theme Theme) eventsModel {
	vp := viewport.New(80, 15)
	return eventsModel{theme: theme, vp: vp}
}

// View renders the body. Status + scroll hint sit above the
// viewport so the operator always knows whether the tail is live.
func (m eventsModel) View(width int) string {
	state := m.theme.BadgeOK.Render("LIVE")
	if m.paused {
		state = m.theme.BadgeWarn.Render("PAUSED")
	}
	if !m.started {
		state = m.theme.Faint.Render("connecting…")
	}
	if m.err != nil {
		state = m.theme.BadgeBad.Render("ERROR: " + m.err.Error())
	}
	header := state + "  " + m.theme.Faint.Render(fmt.Sprintf("%d lines · p pause · c clear · j/k scroll", len(m.lines)))
	body := m.vp.View()
	if len(m.lines) == 0 {
		// Keep the SAME line count as a full viewport — operator-
		// reported 2026-06-24 : the bottom border of the log pane
		// moved up by N-1 lines when switching from Logs to a
		// freshly-opened Events tab because the empty body had 1
		// line ("waiting for events…") instead of vp.Height lines.
		// Pad with blank lines so the pane's total height stays
		// constant across tab switches.
		blank := "  waiting for events…"
		body = m.theme.Faint.Render(blank)
		for i := 1; i < m.vp.Height; i++ {
			body += "\n"
		}
	}
	return header + "\n" + body
}

// appendLine pushes a formatted line into the ring buffer + viewport
// content. Drops the oldest line when we exceed maxEventLines.
func (m *eventsModel) appendLine(line string) {
	m.lines = append(m.lines, line)
	if len(m.lines) > maxEventLines {
		m.lines = m.lines[len(m.lines)-maxEventLines:]
	}
	m.vp.SetContent(strings.Join(m.lines, "\n"))
	if !m.userScrolled {
		m.vp.GotoBottom()
	}
}

func (m *eventsModel) clearBuffer() {
	m.lines = nil
	m.vp.SetContent("")
	m.userScrolled = false
}

// formatEvent renders one PlatformEvent into the colourised line the
// viewport displays : `[HH:MM:SS] component verb subject  meta…`. The
// component prefix (vm / project / host / guest / error) drives the
// colour so operators can scan a busy bus.
func (m eventsModel) formatEvent(e *weftv1.PlatformEvent) string {
	ts := time.Unix(0, e.TsUnixNs).Format("15:04:05")
	component, verb := splitKind(e.Kind)
	tinted := component + " " + verb
	switch component {
	case "host":
		tinted = m.theme.StatusKey.Render(component) + " " + verb
	case "vm":
		tinted = m.theme.BadgeOK.Render(component) + " " + verb
	case "project":
		tinted = m.theme.BadgeWarn.Render(component) + " " + verb
	case "guest":
		tinted = m.theme.StatusVal.Render(component) + " " + verb
	case "error", "err":
		tinted = m.theme.BadgeBad.Render(component) + " " + verb
	}
	subject := dashEmpty(e.Subject)
	if e.ProjectUuid != "" && len(e.ProjectUuid) >= 8 {
		subject = subject + " (" + e.ProjectUuid[:8] + ")"
	}
	if isErrorKind(e.Kind) {
		return m.theme.StatusErr.Render(fmt.Sprintf("[%s] %s %s", ts, tinted, subject))
	}
	return fmt.Sprintf("[%s] %s %s", ts, tinted, subject)
}

// splitKind splits "vm.state.running" → ("vm", "state.running").
// Component-less events return ("event", kind) so the formatter
// still has something to render.
func splitKind(kind string) (string, string) {
	idx := strings.Index(kind, ".")
	if idx <= 0 {
		return "event", kind
	}
	return kind[:idx], kind[idx+1:]
}

func isErrorKind(kind string) bool {
	return strings.Contains(kind, "error") || strings.Contains(kind, "failed")
}

// --- streaming Cmd + messages ---

// eventReceivedMsg is delivered when the WatchEvents goroutine reads
// one PlatformEvent off the stream.
type eventReceivedMsg struct{ ev *weftv1.PlatformEvent }

// eventStreamErrorMsg is delivered when the WatchEvents stream
// terminates with an error (or EOF after the agent closes the conn).
type eventStreamErrorMsg struct{ err error }

// eventStreamStartedMsg signals the goroutine successfully opened
// the stream — flips the model out of the "connecting…" state.
type eventStreamStartedMsg struct{}

// startEventsStreamCmd opens a WatchEvents server-stream in a
// goroutine ; every received PlatformEvent reaches the Bubble Tea
// loop as an eventReceivedMsg via the returned channel that tea.Cmd
// pumps with tea.Batch / repeated wake-ups.
//
// Bubble Tea is single-threaded ; the canonical pattern for a
// long-lived stream is a sequence of "wait for next event" Cmds
// that each block on the same goroutine-owned channel. We implement
// that here with a goroutine that pushes into a buffered channel +
// a receiveNextCmd that drains one item at a time.
func startEventsStreamCmd(client EventsClient, p *eventStreamPump) tea.Cmd {
	if client == nil {
		return func() tea.Msg { return eventStreamErrorMsg{err: errNoClient} }
	}
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		p.setCancel(cancel)
		stream, err := client.WatchEvents(ctx, &weftv1.WatchEventsRequest{})
		if err != nil {
			cancel()
			return eventStreamErrorMsg{err: err}
		}
		go func() {
			defer close(p.ch)
			for {
				ev, recvErr := stream.Recv()
				if recvErr != nil {
					p.errOnce.Do(func() { p.errCh <- recvErr })
					return
				}
				select {
				case <-ctx.Done():
					return
				case p.ch <- ev:
				}
			}
		}()
		return eventStreamStartedMsg{}
	}
}

// receiveNextEventCmd produces one eventReceivedMsg per call by
// reading the next item off the pump. Re-issued by Update after each
// delivery to keep the pipeline flowing until the stream closes.
func receiveNextEventCmd(p *eventStreamPump) tea.Cmd {
	if p == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case ev, ok := <-p.ch:
			if !ok {
				// Channel closed → stream ended. Surface whatever
				// error landed on errCh (or a generic EOF).
				select {
				case e := <-p.errCh:
					return eventStreamErrorMsg{err: e}
				default:
					return eventStreamErrorMsg{err: nil}
				}
			}
			return eventReceivedMsg{ev: ev}
		case e := <-p.errCh:
			return eventStreamErrorMsg{err: e}
		}
	}
}

// eventStreamPump bridges the gRPC goroutine and the Bubble Tea
// Update loop. ch is the event firehose ; errCh carries the
// terminal stream error (sent exactly once). cancel tears the
// gRPC context down when the user quits the app.
type eventStreamPump struct {
	mu      sync.Mutex
	ch      chan *weftv1.PlatformEvent
	errCh   chan error
	errOnce sync.Once
	cancel  context.CancelFunc
}

func newEventStreamPump() *eventStreamPump {
	return &eventStreamPump{
		ch:    make(chan *weftv1.PlatformEvent, 64),
		errCh: make(chan error, 1),
	}
}

func (p *eventStreamPump) setCancel(c context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cancel = c
}

func (p *eventStreamPump) stop() {
	p.mu.Lock()
	c := p.cancel
	p.mu.Unlock()
	if c != nil {
		c()
	}
}
