package ui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The backup passphrase box (§7.5): it turns the backup on, or opens it
// for a restore. The passphrase shows as dots.

// passState is the passphrase box.
type passState struct {
	input textinput.Model
	// kind is the action the passphrase is for: backup-on or
	// restore-backup.
	kind string
	err  string
	busy bool
}

// passDoneMsg is the answer to the passphrase box's action.
type passDoneMsg struct {
	ok  string
	err error
}

// openPassphrase shows the passphrase box for an action.
func (m *mainView) openPassphrase(a *App, kind string) tea.Cmd {
	in := textinput.New()
	in.Prompt = "> "
	in.CharLimit = 1000
	in.EchoMode = textinput.EchoPassword
	st := textinput.DefaultStyles(a.theme.dark)
	st.Focused.Prompt = a.theme.accent
	st.Focused.Text = a.theme.text
	st.Cursor.Color = a.theme.p.accent
	in.SetStyles(st)
	m.pass = passState{input: in, kind: kind}
	m.overlay = overlayPassphrase
	return m.pass.input.Focus()
}

func (m *mainView) passphraseKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.overlay = overlayNone
		m.pass = passState{}
		return nil
	case "enter":
		p := m.pass.input.Value()
		if strings.TrimSpace(p) == "" || m.pass.busy {
			return nil
		}
		m.pass.busy, m.pass.err = true, ""
		kind, chat := m.pass.kind, m.active
		return call(func(ctx context.Context) tea.Msg {
			ok, err := a.b.Act(ctx, chat, Action{Kind: kind, Arg: p})
			return passDoneMsg{ok, err}
		})
	}
	var cmd tea.Cmd
	m.pass.input, cmd = m.pass.input.Update(msg)
	return cmd
}

// passDone closes the box once its action worked, or shows why it
// didn't.
func (m *mainView) passDone(a *App, msg passDoneMsg) tea.Cmd {
	m.pass.busy = false
	if msg.err != nil {
		m.pass.err = msg.err.Error()
		return nil
	}
	m.overlay = overlayNone
	m.pass = passState{}
	m.notice = msg.ok
	return m.loadChats(a)
}

func (m *mainView) passphraseView(a *App) string {
	t := a.theme
	w := min(a.width-6, 64)
	m.pass.input.SetWidth(w - 3)
	title, text, key := "Turn on the backup",
		"Railcar encrypts your message history and keeps it on your homeserver, which can't read it. Choose a passphrase of six or more random words, and write it down: you need it, or your recovery words, to restore the backup on a new device. Anyone with the passphrase and a copy of the backup can read it.",
		" turn on"
	if m.pass.kind == "restore-backup" {
		title, text, key = "Restore the backup", "Enter the backup passphrase.", " restore"
	}
	var b strings.Builder
	b.WriteString(t.bold.Render(title) + "\n\n")
	b.WriteString(t.muted.Width(w).Render(text) + "\n\n")
	b.WriteString(m.pass.input.View() + "\n\n")
	switch {
	case m.pass.busy:
		b.WriteString(a.spinner.View() + " " + t.muted.Render("Working on it") + "\n\n")
	case m.pass.err != "":
		b.WriteString(t.danger.Width(w).Render(m.pass.err) + "\n\n")
	}
	b.WriteString(t.accent.Render("enter") + t.muted.Render(key+"   ") + t.accent.Render("esc") + t.muted.Render(" cancel"))
	content := b.String()
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}
