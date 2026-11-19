package ui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// command is one entry in the command palette.
type command struct {
	label string
	// names are what you can type after : to run the command directly.
	names []string
	run   func(a *App) tea.Cmd
	// withArg, when set, runs the command typed by name with the rest of
	// the line, such as :react 🎉.
	withArg func(a *App, arg string) tea.Cmd
}

// commandPalette lists commands and filters them as you type. Opened with
// :, it also runs a command typed by name, such as :quit.
type commandPalette struct {
	input    textinput.Model
	commands []command
	shown    []command
	cursor   int
}

func newCommandPalette(a *App, m *mainView) *commandPalette {
	in := textinput.New()
	in.Prompt = ": "
	in.Placeholder = "Type a command or a chat's name"
	in.CharLimit = 200
	st := textinput.DefaultStyles(a.theme.dark)
	st.Focused.Prompt = a.theme.accent
	st.Focused.Text = a.theme.text
	st.Focused.Placeholder = a.theme.dim
	st.Cursor.Color = a.theme.p.accent
	in.SetStyles(st)
	in.Focus()

	details := "Show details"
	if m.showDetails {
		details = "Hide details"
	}
	cmds := []command{
		{label: "Add a contact", names: []string{"add"}, run: func(a *App) tea.Cmd { return a.main.openAddContact(a, "") },
			withArg: func(a *App, code string) tea.Cmd { return a.main.openAddContact(a, code) }},
		{label: "Make an invite code", names: []string{"invite"}, run: func(a *App) tea.Cmd { return a.main.makeInvite(a) }},
		{label: "Find a chat", names: []string{"find"}, run: func(a *App) tea.Cmd { return a.main.findChat(a) }},
		{label: details, names: []string{"details", "info"}, run: func(a *App) tea.Cmd { a.main.toggleDetails(a); return nil }},
		{label: "Reply to the last message", names: []string{"reply"}, run: func(a *App) tea.Cmd { return a.main.startReply(a) }},
		{label: "Edit your last message", names: []string{"edit"}, run: func(a *App) tea.Cmd { return a.main.startEdit(a) }},
		{label: "Delete your last message", names: []string{"delete"}, run: func(a *App) tea.Cmd { return a.main.deleteLast(a) }},
		{label: "React to the last message", names: []string{"react"}, run: func(a *App) tea.Cmd { return a.main.reactLast(a, "") },
			withArg: func(a *App, emoji string) tea.Cmd { return a.main.reactLast(a, emoji) }},
		{label: "Show keys and help", names: []string{"help", "h"}, run: func(a *App) tea.Cmd { a.main.overlay = overlayHelp; return nil }},
		{label: "Sign out", names: []string{"sign-out", "signout"}, run: func(a *App) tea.Cmd { a.main.overlay = overlaySignOut; return nil }},
		{label: "Quit Railcar", names: []string{"quit", "q"}, run: func(*App) tea.Cmd { return tea.Quit }},
	}
	if c, ok := m.activeChat(); ok && c.Kind == ContactRequest {
		cmds = append([]command{
			{label: "Accept " + c.Title + "'s request", names: []string{"accept"}, run: func(a *App) tea.Cmd { return a.main.acceptRequest(a) }},
			{label: "Decline " + c.Title + "'s request", names: []string{"decline"}, run: func(a *App) tea.Cmd { return a.main.declineRequest(a) }},
		}, cmds...)
	}
	for _, c := range m.chats {
		id := c.ID
		cmds = append(cmds, command{
			label: "Open " + c.Title,
			run:   func(a *App) tea.Cmd { return a.main.openChat(a, id, true) },
		})
	}
	p := &commandPalette{input: in, commands: cmds}
	p.filter()
	return p
}

// filter keeps the commands whose label has every typed word.
func (p *commandPalette) filter() {
	words := strings.Fields(strings.ToLower(p.input.Value()))
	p.shown = p.shown[:0]
	for _, c := range p.commands {
		label := strings.ToLower(c.label)
		ok := true
		for _, w := range words {
			if !strings.Contains(label, w) && !hasName(c, w) {
				ok = false
				break
			}
		}
		if ok {
			p.shown = append(p.shown, c)
		}
	}
	p.cursor = min(p.cursor, max(0, len(p.shown)-1))
}

func hasName(c command, s string) bool {
	for _, n := range c.names {
		if n == s {
			return true
		}
	}
	return false
}

// update handles a key while the palette is open. It returns done when the
// palette should close.
func (p *commandPalette) update(a *App, msg tea.KeyPressMsg) (cmd tea.Cmd, done bool) {
	switch msg.String() {
	case "esc":
		return nil, true
	case "up", "ctrl+p", "shift+tab":
		p.cursor = max(0, p.cursor-1)
		return nil, false
	case "down", "ctrl+n", "tab":
		p.cursor = min(max(0, len(p.shown)-1), p.cursor+1)
		return nil, false
	case "enter":
		line := strings.TrimSpace(p.input.Value())
		typed := strings.ToLower(line)
		for _, c := range p.commands {
			if hasName(c, typed) {
				return c.run(a), true
			}
		}
		if name, arg, ok := strings.Cut(line, " "); ok {
			for _, c := range p.commands {
				if c.withArg != nil && hasName(c, strings.ToLower(name)) {
					return c.withArg(a, strings.TrimSpace(arg)), true
				}
			}
		}
		if len(p.shown) == 0 {
			return nil, false
		}
		return p.shown[p.cursor].run(a), true
	}
	before := p.input.Value()
	p.input, cmd = p.input.Update(msg)
	if p.input.Value() != before {
		p.cursor = 0
		p.filter()
	}
	return cmd, false
}

// view draws the palette's box contents.
func (p *commandPalette) view(t *theme, width, rows int) string {
	p.input.SetWidth(width - 3)
	var b strings.Builder
	b.WriteString(p.input.View() + "\n")
	b.WriteString(t.dim.Render(strings.Repeat("─", width)) + "\n")
	if len(p.shown) == 0 {
		b.WriteString(t.dim.Render("No matching commands"))
		return b.String()
	}
	start := 0
	if p.cursor >= rows {
		start = p.cursor - rows + 1
	}
	end := min(len(p.shown), start+rows)
	for i := start; i < end; i++ {
		label := ansi.Truncate(p.shown[i].label, width-2, "…")
		pad := strings.Repeat(" ", max(0, width-2-ansi.StringWidth(label)))
		if i == p.cursor {
			b.WriteString(t.paletteSel.Render("> " + label + pad))
		} else {
			b.WriteString(t.paletteRow.Render("  " + label + pad))
		}
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}
