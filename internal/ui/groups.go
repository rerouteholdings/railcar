package ui

import (
	"context"
	"errors"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// groupForm is the box that creates a group chat (a name, then the
// contacts to put in it) or a community (a community server, a name, and
// the server's creation code).
type groupForm struct {
	community bool
	inputs    []textinput.Model
	field     int
	contacts  []Member
	picked    map[string]bool
	cursor    int
	err       string
	busy      bool
}

// Messages from the boxes in this file.
type (
	contactsMsg struct {
		list []Member
		err  error
	}
	groupMadeMsg struct {
		id  string
		err error
	}
	actedMsg struct {
		ok   string
		err  error
		copy bool
	}
)

func (m *mainView) newInput(a *App, placeholder, value string) textinput.Model {
	in := textinput.New()
	in.Prompt = "> "
	in.Placeholder = placeholder
	in.CharLimit = 200
	st := textinput.DefaultStyles(a.theme.dark)
	st.Focused.Prompt = a.theme.accent
	st.Focused.Text = a.theme.text
	st.Focused.Placeholder = a.theme.dim
	st.Cursor.Color = a.theme.p.accent
	in.SetStyles(st)
	in.SetValue(value)
	return in
}

// openNewGroup shows the create-group box, and loads the contacts.
func (m *mainView) openNewGroup(a *App, name string) tea.Cmd {
	m.grp = groupForm{inputs: []textinput.Model{m.newInput(a, "Group name", name)}, picked: map[string]bool{}}
	m.overlay = overlayNewGroup
	return tea.Batch(m.grp.inputs[0].Focus(), call(func(ctx context.Context) tea.Msg {
		l, err := a.b.Contacts(ctx)
		return contactsMsg{l, err}
	}))
}

// openNewCommunity shows the create-community box.
func (m *mainView) openNewCommunity(a *App) tea.Cmd {
	m.grp = groupForm{community: true, inputs: []textinput.Model{
		m.newInput(a, "community.example.com", ""),
		m.newInput(a, "Community name", ""),
		m.newInput(a, "The server's creation code, if it needs one", ""),
	}}
	m.overlay = overlayNewGroup
	return m.grp.inputs[0].Focus()
}

// groupKey handles a key in the create-group or create-community box.
func (m *mainView) groupKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	g := &m.grp
	if g.busy {
		return nil
	}
	onList := !g.community && g.field == len(g.inputs)
	switch msg.String() {
	case "esc":
		m.overlay = overlayNone
		return nil
	case "tab", "down":
		if onList && msg.String() == "down" {
			g.cursor = min(len(g.contacts)-1, g.cursor+1)
			return nil
		}
		return m.groupField(g.field + 1)
	case "shift+tab", "up":
		if onList && msg.String() == "up" && g.cursor > 0 {
			g.cursor--
			return nil
		}
		return m.groupField(g.field - 1)
	case "space":
		if onList && g.cursor < len(g.contacts) {
			h := g.contacts[g.cursor].Handle
			g.picked[h] = !g.picked[h]
			return nil
		}
	case "enter":
		if !onList && g.field < len(g.inputs)-1 || !g.community && !onList {
			return m.groupField(g.field + 1)
		}
		return m.submitGroup(a)
	}
	if onList || g.field >= len(g.inputs) {
		return nil
	}
	var cmd tea.Cmd
	g.inputs[g.field], cmd = g.inputs[g.field].Update(msg)
	return cmd
}

func (m *mainView) groupField(n int) tea.Cmd {
	g := &m.grp
	last := len(g.inputs) - 1
	if !g.community {
		last = len(g.inputs)
	}
	n = max(0, min(last, n))
	for i := range g.inputs {
		g.inputs[i].Blur()
	}
	g.field = n
	if n < len(g.inputs) {
		return g.inputs[n].Focus()
	}
	return nil
}

func (m *mainView) submitGroup(a *App) tea.Cmd {
	g := &m.grp
	if g.community {
		server, name, code := strings.TrimSpace(g.inputs[0].Value()), strings.TrimSpace(g.inputs[1].Value()), g.inputs[2].Value()
		if server == "" || name == "" {
			g.err = "Enter the community server and a name."
			return nil
		}
		g.busy, g.err = true, ""
		return call(func(ctx context.Context) tea.Msg {
			_, err := a.b.Act(ctx, "", Action{Kind: "create-community", Arg: server, Arg2: name, Arg3: code})
			return groupMadeMsg{err: err}
		})
	}
	name := strings.TrimSpace(g.inputs[0].Value())
	var handles []string
	for _, c := range g.contacts {
		if g.picked[c.Handle] {
			handles = append(handles, c.Handle)
		}
	}
	if name == "" || len(handles) == 0 {
		g.err = "Enter a name, and choose at least one contact with Space."
		return nil
	}
	g.busy, g.err = true, ""
	return call(func(ctx context.Context) tea.Msg {
		id, err := a.b.CreateGroup(ctx, name, handles)
		return groupMadeMsg{id, err}
	})
}

func (m *mainView) groupView(a *App) string {
	t := a.theme
	g := &m.grp
	w := min(a.width-6, 64)
	var b strings.Builder
	if g.community {
		b.WriteString(t.bold.Render("Create a community") + "\n\n")
		b.WriteString(t.muted.Width(w).Render("A community lives on a community server someone runs for it. Its channels are end-to-end encrypted (E2EE).") + "\n\n")
		for i, label := range []string{"Community server", "Name", "Creation code"} {
			g.inputs[i].SetWidth(w - 3)
			b.WriteString(t.dim.Render(label) + "\n" + g.inputs[i].View() + "\n")
		}
	} else {
		b.WriteString(t.bold.Render("New group chat") + "\n\n")
		g.inputs[0].SetWidth(w - 3)
		b.WriteString(g.inputs[0].View() + "\n\n")
		b.WriteString(t.dim.Render("Contacts (Space chooses, up to 24)") + "\n")
		if len(g.contacts) == 0 {
			b.WriteString(t.muted.Render("  No contacts yet") + "\n")
		}
		for i, c := range g.contacts {
			box := "[ ] "
			if g.picked[c.Handle] {
				box = "[x] "
			}
			row := box + "@" + c.Handle
			if g.field == len(g.inputs) && i == g.cursor {
				b.WriteString(t.accent.Render("> "+row) + "\n")
			} else {
				b.WriteString(t.text.Render("  "+row) + "\n")
			}
		}
	}
	b.WriteString("\n")
	switch {
	case g.busy:
		b.WriteString(a.spinner.View() + " " + t.muted.Render("Creating it") + "\n\n")
	case g.err != "":
		b.WriteString(t.danger.Width(w).Render(g.err) + "\n\n")
	}
	b.WriteString(t.accent.Render("tab") + t.muted.Render(" next   ") + t.accent.Render("enter") + t.muted.Render(" create   ") + t.accent.Render("esc") + t.muted.Render(" cancel"))
	content := b.String()
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}

// act runs an action on the open chat and reports it in the status bar.
// With copy, the answer, such as a join code, goes to the clipboard.
func (m *mainView) act(a *App, kind, arg, arg2 string, copy bool) tea.Cmd {
	chat := m.active
	m.notice = "Working on it"
	return call(func(ctx context.Context) tea.Msg {
		ok, err := a.b.Act(ctx, chat, Action{Kind: kind, Arg: arg, Arg2: arg2})
		return actedMsg{ok, err, copy}
	})
}

// handleGroupMsg handles the messages from this file's commands.
func (m *mainView) handleGroupMsg(a *App, msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case contactsMsg:
		m.grp.contacts = msg.list
		if msg.err != nil {
			m.grp.err = "Railcar couldn't list your contacts: " + msg.err.Error()
		}
		return nil, true
	case groupMadeMsg:
		m.grp.busy = false
		if msg.err != nil {
			m.grp.err = "Railcar couldn't create it: " + msg.err.Error()
			return nil, true
		}
		m.overlay = overlayNone
		cmds := []tea.Cmd{m.loadChats(a)}
		if msg.id != "" {
			cmds = append(cmds, m.openChat(a, msg.id, true))
		}
		m.notice = "Created."
		return tea.Batch(cmds...), true
	case actedMsg:
		if errors.Is(msg.err, ErrPassphraseNeeded) {
			return m.openPassphrase(a, "restore-backup"), true
		}
		if msg.err != nil {
			m.notice = msg.err.Error()
			return nil, true
		}
		m.notice = msg.ok
		if msg.copy {
			m.notice = msg.ok + " (copied)"
			return tea.Batch(tea.SetClipboard(msg.ok), m.loadChats(a)), true
		}
		return m.loadChats(a), true
	}
	return nil, false
}
