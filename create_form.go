package main

// create_form.go — generic create-modal for the palette resources.
// Operators press `n` (for "new") on a resource view ; the form
// renders one textinput per field declared in CreateFields, Tab
// cycles focus, Enter submits, Esc cancels.
//
// Per-resource specifics (the RPC + field validation) live in the
// catalogue entry's CreateFn ; this file is purely the Bubble Tea
// state machine + rendering.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	weftv1 "github.com/openweft/weft-proto"
)

// FormField describes one input on a create form.
type FormField struct {
	Key         string // form field id (matches the map key passed to CreateFn)
	Label       string // user-visible label
	Placeholder string // greyed-out hint
	Required    bool
	Numeric     bool // when true, the form refuses non-int input on submit
}

// CreateFn is the per-resource closure that fires when the operator
// submits the form. Receives the values keyed by FormField.Key.
// Returns a status message + error ; nil error closes the form +
// triggers a list refresh.
type CreateFn func(ctx context.Context, c weftv1.WeftAgentClient, values map[string]string) (msg string, err error)

// createFormModel is the per-row state of the modal. Owned by a
// ResourceListModel ; lifecycle is open/close, not persistent.
type createFormModel struct {
	fields   []FormField
	inputs   []textinput.Model
	focus    int
	errMsg   string
	cfg      ResourceConfig
}

// newCreateFormModel arms a fresh form with one textinput per field.
func newCreateFormModel(cfg ResourceConfig) *createFormModel {
	inputs := make([]textinput.Model, len(cfg.CreateFields))
	for i, f := range cfg.CreateFields {
		ti := textinput.New()
		ti.Placeholder = f.Placeholder
		ti.CharLimit = 256
		ti.Width = 48
		if i == 0 {
			ti.Focus()
		}
		inputs[i] = ti
	}
	return &createFormModel{fields: cfg.CreateFields, inputs: inputs, cfg: cfg}
}

// Update routes key events. The submit path is signalled by returning
// a non-nil submitMsg.
type createSubmitMsg struct {
	cfg    string
	values map[string]string
}

type createCancelMsg struct{ cfg string }

func (f *createFormModel) Update(msg tea.Msg) (*createFormModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			return f, func() tea.Msg { return createCancelMsg{cfg: f.cfg.ID} }
		case "tab", "down":
			f.advance(1)
			return f, nil
		case "shift+tab", "up":
			f.advance(-1)
			return f, nil
		case "enter":
			values, err := f.collect()
			if err != nil {
				f.errMsg = err.Error()
				return f, nil
			}
			return f, func() tea.Msg { return createSubmitMsg{cfg: f.cfg.ID, values: values} }
		}
	}
	var cmd tea.Cmd
	f.inputs[f.focus], cmd = f.inputs[f.focus].Update(msg)
	return f, cmd
}

// advance moves focus by delta (wraps).
func (f *createFormModel) advance(delta int) {
	f.inputs[f.focus].Blur()
	f.focus = (f.focus + delta + len(f.inputs)) % len(f.inputs)
	f.inputs[f.focus].Focus()
}

// collect reads + validates every input value. Returns a map keyed by
// field id or the first validation error encountered.
func (f *createFormModel) collect() (map[string]string, error) {
	out := make(map[string]string, len(f.fields))
	for i, field := range f.fields {
		v := strings.TrimSpace(f.inputs[i].Value())
		if field.Required && v == "" {
			return nil, fmt.Errorf("%s is required", field.Label)
		}
		if field.Numeric && v != "" {
			if _, err := strconv.Atoi(v); err != nil {
				return nil, fmt.Errorf("%s must be numeric, got %q", field.Label, v)
			}
		}
		out[field.Key] = v
	}
	return out, nil
}

// View renders the form chrome + every input.
func (f *createFormModel) View(theme Theme) string {
	var b strings.Builder
	b.WriteString(theme.Title.Render("New " + f.cfg.Title))
	b.WriteString("\n\n")
	for i, field := range f.fields {
		label := field.Label
		if field.Required {
			label += " *"
		}
		b.WriteString(theme.StatusKey.Render(label))
		b.WriteString("\n")
		b.WriteString(f.inputs[i].View())
		b.WriteString("\n\n")
	}
	b.WriteString(theme.Faint.Render("Enter submit · Tab next field · Esc cancel"))
	if f.errMsg != "" {
		b.WriteString("\n")
		b.WriteString(theme.BadgeBad.Render("error: " + f.errMsg))
	}
	return b.String()
}
