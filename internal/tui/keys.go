package tui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// runeKey is the key press of the printable rune r (as typed).
func runeKey(r rune) tea.KeyPressMsg {
	if r == ' ' {
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// styleInput styles ti in the theme and stops its cursor from blinking (a
// blinking cursor would redraw the whole screen twice a second).
func styleInput(ti *textinput.Model) {
	st := ti.Styles()
	for _, s := range []*textinput.StyleState{&st.Focused, &st.Blurred} {
		s.Prompt = keyStyle
		s.Placeholder = dimStyle
		s.Text = lipgloss.NewStyle()
	}
	st.Cursor.Color = primary
	st.Cursor.Blink = false
	ti.SetStyles(st)
}

// handlePaste routes a bracketed paste to the input that has the keyboard;
// normal mode and the dialogs ignore it (a paste never runs commands).
func (m *Model) handlePaste(msg tea.PasteMsg) tea.Cmd {
	switch m.mode {
	case modeForm:
		if m.form != nil {
			return tea.Batch(m.form.paste(msg), m.syncProject())
		}
	case modePicker:
		if m.picker != nil {
			m.picker.paste(msg.Content)
		}
	case modeFilter:
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return tea.Batch(cmd, m.applyFilter(m.filterInput.Value()))
	case modeRename:
		if m.statusErr {
			m.setStatus("")
		}
		var cmd tea.Cmd
		m.renameInput, cmd = m.renameInput.Update(msg)
		return cmd
	case modeNotes:
		if m.notes != nil && m.notes.ask == askNone {
			return m.notesUpdate(msg)
		}
	}
	return nil
}

// paste inserts pasted text into the focused text field.
func (f *newForm) paste(msg tea.PasteMsg) tea.Cmd {
	switch f.focus {
	case fieldTitle, fieldProject, fieldDesc:
		f.err, f.cycling = "", false
		var cmd tea.Cmd
		f.inputs[f.focus], cmd = f.inputs[f.focus].Update(msg)
		return cmd
	}
	return nil
}

// paste appends pasted text to the filter, as typing it would (spaces and
// control characters are dropped).
func (p *picker) paste(s string) {
	var b strings.Builder
	for _, r := range s {
		if r > ' ' && r != 0x7f {
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		p.setFilter(p.filter + b.String())
		p.clamp()
	}
}

// paste routes a bracketed paste to the selector's input that has the
// keyboard, if any.
func (m *WorkspacePicker) paste(msg tea.PasteMsg) tea.Cmd {
	if m.creating {
		return nil
	}
	var cmd tea.Cmd
	switch m.mode {
	case wsFilter:
		m.filterInput, cmd = m.filterInput.Update(msg)
		m.applyFilter(m.filterInput.Value())
	case wsNew:
		m.setStatus("", false)
		m.nameInput, cmd = m.nameInput.Update(msg)
	}
	return cmd
}

// restyleInputs applies the theme again to the inputs (after it changed).
func (m *Model) restyleInputs() {
	styleInput(&m.filterInput)
	styleInput(&m.renameInput)
	for _, f := range []*newForm{m.form, m.lastForm} {
		if f != nil {
			for i := range f.inputs {
				styleInput(&f.inputs[i])
			}
		}
	}
	if m.notes != nil {
		m.notes.restyle()
	}
}

// View implements tea.Model.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "atelier"
	if ws := clean(m.workspace); ws != "" {
		v.WindowTitle = "atelier · " + ws
	}
	return v
}

// View implements tea.Model.
func (m *WorkspacePicker) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "atelier · workspaces"
	return v
}
