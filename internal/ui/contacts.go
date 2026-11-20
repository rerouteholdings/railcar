package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// confirmState is a question that asks before an action that can't be
// undone, such as removing a contact.
type confirmState struct {
	title string
	text  string
	yes   string // the action's label, such as "remove"
	run   func(a *App) tea.Cmd
}

// ask opens a confirmation.
func (m *mainView) ask(c confirmState) tea.Cmd {
	m.confirm = c
	m.overlay = overlayConfirm
	return nil
}

func (m *mainView) confirmKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "enter", "y":
		m.overlay = overlayNone
		return m.confirm.run(a)
	case "esc", "n", "q":
		m.overlay = overlayNone
	}
	return nil
}

func (m *mainView) confirmView(a *App) string {
	t := a.theme
	w := min(a.width-6, 60)
	c := m.confirm
	content := t.bold.Render(c.title) + "\n\n" + t.muted.Width(w).Render(c.text) + "\n\n" +
		t.accent.Render("enter") + t.muted.Render(" "+c.yes+"   ") + t.accent.Render("esc") + t.muted.Render(" cancel")
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}

// contactCommands remove, block, and unblock people (§9.6). Removing and
// blocking take a handle, or mean the person in the open direct chat or
// contact request, and ask first.
func contactCommands(m *mainView) []command {
	target := func(arg string) string {
		if h := strings.TrimSpace(arg); h != "" {
			return "@" + strings.TrimPrefix(h, "@")
		}
		if c, ok := m.activeChat(); ok && (c.Kind == DirectChat || c.Kind == ContactRequest) && len(c.Members) > 0 {
			return "@" + c.Members[0].Handle
		}
		return ""
	}
	confirmed := func(kind, title, text, yes string) func(a *App, arg string) tea.Cmd {
		return func(a *App, arg string) tea.Cmd {
			who := target(arg)
			if who == "" {
				a.main.notice = "Type :" + kind + " @handle, or open your direct chat with them"
				return nil
			}
			return a.main.ask(confirmState{
				title: strings.ReplaceAll(title, "WHO", who), text: strings.ReplaceAll(text, "WHO", who), yes: yes,
				run: func(a *App) tea.Cmd { return a.main.act(a, kind, strings.TrimSpace(arg), "", false) },
			})
		}
	}
	remove := confirmed("remove-contact", "Remove WHO?",
		"WHO stops being a contact, and your direct chat with them ends, on all your devices. Their app sees you leave the chat. The messages stay here. To talk again, one of you needs a new invite code.",
		"remove")
	block := confirmed("block", "Block WHO?",
		"WHO stops being a contact, your direct chat with them ends, and your homeserver drops their requests from now on. They aren't told, but they may notice their messages go unanswered. You stay in any group chats you share.",
		"block")
	return []command{
		{label: "Remove a contact", names: []string{"remove-contact"}, run: func(a *App) tea.Cmd { return remove(a, "") }, withArg: remove},
		{label: "Block someone", names: []string{"block"}, run: func(a *App) tea.Cmd { return block(a, "") }, withArg: block},
		argCommand("Unblock someone", "unblock", "unblock", "@handle", false),
		{label: "List who you've blocked", names: []string{"blocked"}, run: func(a *App) tea.Cmd { return a.main.act(a, "blocked", "", "", false) }},
	}
}
