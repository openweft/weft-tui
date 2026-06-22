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

// EditFn is the edit equivalent. Same shape as CreateFn except it
// also receives the row the form was opened on — typically used
// to extract the UUID for the UpdateXxx / SetXxx RPC. Returning a
// nil error closes the form + triggers a list refresh.
type EditFn func(ctx context.Context, c weftv1.WeftAgentClient, row map[string]any, values map[string]string) (msg string, err error)

// createFormModel is the per-row state of the modal. Owned by a
// ResourceListModel ; lifecycle is open/close, not persistent.
// Used for both Create (n) and Edit (e) ; editMode + editRow
// distinguish.
type createFormModel struct {
	fields   []FormField
	inputs   []textinput.Model
	focus    int
	errMsg   string
	cfg      ResourceConfig
	// editMode flips the form into Edit dispatch : on submit, the
	// editSubmitMsg fires instead of createSubmitMsg + the
	// ResourceListModel calls cfg.EditFn(row, values) rather than
	// cfg.CreateFn(values).
	editMode bool
	// editRow is the row the form was opened on. Used both to
	// pre-fill input values and to pass through to EditFn so the
	// closure can read row["uuid"] / row["name"] etc.
	editRow map[string]any
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

// newEditFormModel arms a form pre-filled from the highlighted row.
// EditFields[i].Key looks up row[key] for the initial value ; rows
// that don't carry the key (e.g. a sparse projection) start empty.
// Numeric values are formatted as decimal strings ; everything else
// is the string projection used in the table.
func newEditFormModel(cfg ResourceConfig, row map[string]any) *createFormModel {
	inputs := make([]textinput.Model, len(cfg.EditFields))
	for i, f := range cfg.EditFields {
		ti := textinput.New()
		ti.Placeholder = f.Placeholder
		ti.CharLimit = 256
		ti.Width = 48
		if row != nil {
			if v, ok := row[f.Key]; ok {
				ti.SetValue(rowValueAsString(v))
			}
		}
		if i == 0 {
			ti.Focus()
		}
		inputs[i] = ti
	}
	return &createFormModel{
		fields:   cfg.EditFields,
		inputs:   inputs,
		cfg:      cfg,
		editMode: true,
		editRow:  row,
	}
}

// rowValueAsString flattens a row's any-typed value to text the
// textinput can host. Mirrors the projection s()/iStr() use in
// catalogue_listers but kept here so the form stays self-contained.
func rowValueAsString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// Update routes key events. The submit path is signalled by returning
// a non-nil submitMsg.
type createSubmitMsg struct {
	cfg    string
	values map[string]string
}

// editSubmitMsg fires when an edit-mode form is submitted. The
// dispatcher in resources.go routes it to cfg.EditFn with editRow
// pre-fetched from the model.
type editSubmitMsg struct {
	cfg    string
	row    map[string]any
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
			if f.editMode {
				row := f.editRow
				return f, func() tea.Msg {
					return editSubmitMsg{cfg: f.cfg.ID, row: row, values: values}
				}
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
//
// Edit mode (f.editMode) softens the Required check : an empty field
// is interpreted as "keep current value" per the UpdateXxx RPCs that
// treat empty-string / -1 ints as no-op (memory : the proto3
// convention used by UpdateSubnet, UpdateDNSZone, UpdateLoadBalancer).
// Numeric validation still fires when the operator typed something.
func (f *createFormModel) collect() (map[string]string, error) {
	out := make(map[string]string, len(f.fields))
	for i, field := range f.fields {
		v := strings.TrimSpace(f.inputs[i].Value())
		if !f.editMode && field.Required && v == "" {
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
	header := "New " + f.cfg.Title
	if f.editMode {
		header = "Edit " + f.cfg.Title
	}
	b.WriteString(theme.Title.Render(header))
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
