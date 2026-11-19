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
		{label: "Add a device", names: []string{"add-device"}, run: func(a *App) tea.Cmd { return a.main.openAddDevice(a, "") },
			withArg: func(a *App, code string) tea.Cmd { return a.main.openAddDevice(a, code) }},
		{label: "Show an approval code for a new device", names: []string{"approval-code"}, run: func(a *App) tea.Cmd { return a.main.openApprovalCode(a) }},
		{label: "Your devices", names: []string{"devices"}, run: func(a *App) tea.Cmd { return a.main.openDevices(a) }},
		argCommand("Set your presence: available, online, away, busy, or offline", "presence", "set-presence", "and a state, then a status message if you like", true),
		{label: "Set your status message", names: []string{"status"}, run: func(a *App) tea.Cmd { return a.main.act(a, "set-status", "", "", false) },
			withArg: func(a *App, s string) tea.Cmd { return a.main.act(a, "set-status", s, "", false) }},
		argCommand("Set your display name", "name", "set-name", "and your name", false),
		argCommand("Set your bio", "bio", "set-bio", "and a few words about you", false),
		argCommand("Set your links", "links", "set-links", "and up to five https:// links, separated by spaces", false),
		argCommand("Set your profile picture", "avatar", "set-avatar", "and the path to a PNG, JPEG, WebP, or GIF of at most 1 MB, or none", false),
		{label: "Show your profile", names: []string{"profile"}, run: func(a *App) tea.Cmd { return a.main.act(a, "show-profile", "", "", false) }},
		{label: "Send a file", names: []string{"attach"}, run: func(a *App) tea.Cmd { return a.main.openAttach(a) },
			withArg: func(a *App, path string) tea.Cmd { return a.main.act(a, "attach", path, "", false) }},
		{label: "Save the latest attachments", names: []string{"save"}, run: func(a *App) tea.Cmd { return a.main.act(a, "save", "", "", false) },
			withArg: func(a *App, dir string) tea.Cmd { return a.main.act(a, "save", dir, "", false) }},
		argCommand("Ask for read receipts on your messages: on or off", "ask-receipts", "ask-receipts", "on or off", false),
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
	cmds = append([]command{
		{label: "New group chat", names: []string{"new-group", "group"}, run: func(a *App) tea.Cmd { return a.main.openNewGroup(a, "") },
			withArg: func(a *App, name string) tea.Cmd { return a.main.openNewGroup(a, name) }},
		{label: "Create a community", names: []string{"create-community"}, run: func(a *App) tea.Cmd { return a.main.openNewCommunity(a) }},
		{label: "Join a community with a code", names: []string{"join"}, run: func(a *App) tea.Cmd {
			a.main.notice = "Type :join followed by the code, such as :join community.example.com::Kx82mPq1ZaR4vT9b"
			return nil
		}, withArg: func(a *App, code string) tea.Cmd { return a.main.act(a, "join-community", code, "", false) }},
	}, cmds...)
	if c, ok := m.activeChat(); ok && c.Kind == GroupChat {
		cmds = append(groupCommands(), cmds...)
	}
	if c, ok := m.activeChat(); ok && (c.Kind == GroupChat || c.Kind == DirectChat) {
		cmds = append([]command{argCommand("Send read receipts in this chat: on or off", "receipts", "read-receipts", "on or off", false)}, cmds...)
	}
	if c, ok := m.activeChat(); ok && c.IsChannel() {
		cmds = append(communityCommands(c), cmds...)
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

// argCommand is a command that needs text after its name.
func argCommand(label, name, kind, hint string, split bool) command {
	return command{
		label: label, names: []string{name},
		run: func(a *App) tea.Cmd { a.main.notice = "Type :" + name + " " + hint; return nil },
		withArg: func(a *App, arg string) tea.Cmd {
			arg2 := ""
			if split {
				arg, arg2, _ = strings.Cut(arg, " ")
				arg2 = strings.TrimSpace(arg2)
			}
			return a.main.act(a, kind, arg, arg2, false)
		},
	}
}

// groupCommands act on the open group chat (§11.7, §11.8, §12).
func groupCommands() []command {
	return []command{
		argCommand("Add someone to the group", "add-member", "add-member", "@handle", false),
		argCommand("Remove someone from the group", "remove-member", "remove-member", "@handle", false),
		argCommand("Make someone an admin", "make-admin", "make-admin", "@handle", false),
		argCommand("Stop someone being an admin", "revoke-admin", "revoke-admin", "@handle", false),
		argCommand("Rename the group", "rename", "rename", "and the new name", false),
		argCommand("Change the group's topic", "topic", "topic", "and the new topic", false),
		{label: "Approve the oldest add-request", names: []string{"approve"}, run: func(a *App) tea.Cmd { return a.main.act(a, "approve-add", "", "", false) }},
		{label: "Decline the oldest add-request", names: []string{"decline-add"}, run: func(a *App) tea.Cmd { return a.main.act(a, "decline-add", "", "", false) }},
		{label: "Leave the group", names: []string{"leave"}, run: func(a *App) tea.Cmd { return a.main.act(a, "leave", "", "", false) }},
	}
}

// communityCommands act on the open channel's community (§16).
func communityCommands(c Chat) []command {
	return []command{
		{label: "Make a join code for " + c.Community, names: []string{"code"}, run: func(a *App) tea.Cmd { return a.main.act(a, "make-code", "", "", true) }},
		{label: "Read this channel's history", names: []string{"history"}, run: func(a *App) tea.Cmd { return a.main.act(a, "history", "", "", false) }},
		argCommand("Add a text channel", "add-channel", "add-channel", "and its name", false),
		argCommand("Add a voice channel", "add-voice-channel", "add-voice-channel", "and its name", false),
		argCommand("Add a role", "add-role", "add-role", "Name, then its permissions: talk, invite, moderate, administrate", true),
		argCommand("Give someone a role", "give-role", "give-role", "@handle Role", true),
		argCommand("Kick someone from the community", "kick", "kick", "@handle", false),
		argCommand("Ban someone from the community", "ban", "ban", "@handle", false),
		argCommand("Time someone out", "timeout", "timeout", "@handle minutes", true),
		{label: "Leave " + c.Community, names: []string{"leave-community"}, run: func(a *App) tea.Cmd { return a.main.act(a, "leave-community", "", "", false) }},
	}
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
