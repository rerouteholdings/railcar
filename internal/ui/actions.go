package ui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Messages from the commands in this file.
type (
	// actionMsg is the result of a call whose only answer is an error,
	// with the line to show either way.
	actionMsg struct {
		ok  string
		err error
		bad string
	}
	inviteMsg struct {
		code string
		err  error
	}
	addedMsg struct{ err error }
	// typingOffMsg ends a typing indicator this device sent, unless a later
	// keystroke started another (§13.3).
	typingOffMsg struct {
		chat string
		seq  int
	}
	// typingExpiredMsg hides someone's typing indicator after 10 seconds
	// without news (§13.3).
	typingExpiredMsg struct{}
)

// Typing indicators (§13.3).
const (
	typingEvery  = time.Second      // the most often "typing on" is sent
	typingIdle   = 7 * time.Second  // when "typing off" follows the last keystroke
	typingExpiry = 10 * time.Second // when a receiver hides one it heard nothing more about
)

// do runs a backend call and reports it in the status bar.
func do(ok, bad string, f func(ctx context.Context) error) tea.Cmd {
	return call(func(ctx context.Context) tea.Msg {
		return actionMsg{ok: ok, bad: bad, err: f(ctx)}
	})
}

// openAddContact shows the box to paste someone's code into.
func (m *mainView) openAddContact(a *App, code string) tea.Cmd {
	in := textinput.New()
	in.Prompt = "> "
	in.Placeholder = "@ravi.example.com::Kx82mPq1Za"
	in.CharLimit = 600
	st := textinput.DefaultStyles(a.theme.dark)
	st.Focused.Prompt = a.theme.accent
	st.Focused.Text = a.theme.text
	st.Focused.Placeholder = a.theme.dim
	st.Cursor.Color = a.theme.p.accent
	in.SetStyles(st)
	in.SetValue(code)
	m.addInput, m.addErr, m.addBusy = in, "", false
	m.overlay = overlayAddContact
	return m.addInput.Focus()
}

// addContactKey handles a key in the add-contact box.
func (m *mainView) addContactKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.overlay = overlayNone
		return nil
	case "enter":
		code := strings.TrimSpace(m.addInput.Value())
		if code == "" || m.addBusy {
			return nil
		}
		m.addBusy, m.addErr = true, ""
		return call(func(ctx context.Context) tea.Msg {
			return addedMsg{a.b.AddContact(ctx, code)}
		})
	}
	var cmd tea.Cmd
	m.addInput, cmd = m.addInput.Update(msg)
	return cmd
}

func (m *mainView) addContactView(a *App) string {
	t := a.theme
	w := min(a.width-6, 64)
	m.addInput.SetWidth(w - 3)
	var b strings.Builder
	b.WriteString(t.bold.Render("Add a contact") + "\n\n")
	b.WriteString(t.muted.Width(w).Render("Paste the code they gave you. Codes look like @handle::code, or a link to rerouteholdings.llc.") + "\n\n")
	b.WriteString(m.addInput.View() + "\n\n")
	switch {
	case m.addBusy:
		b.WriteString(a.spinner.View() + " " + t.muted.Render("Sending the request") + "\n\n")
	case m.addErr != "":
		b.WriteString(t.danger.Width(w).Render(m.addErr) + "\n\n")
	}
	b.WriteString(t.accent.Render("enter") + t.muted.Render(" send request   ") + t.accent.Render("esc") + t.muted.Render(" cancel"))
	content := b.String()
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}

// makeInvite makes an invite code and shows it.
func (m *mainView) makeInvite(a *App) tea.Cmd {
	m.notice = "Making an invite code"
	return call(func(ctx context.Context) tea.Msg {
		code, err := a.b.MakeInvite(ctx)
		return inviteMsg{code, err}
	})
}

func (m *mainView) inviteView(a *App) string {
	t := a.theme
	w := min(a.width-6, 64)
	content := t.bold.Render("Your invite code") + "\n\n" +
		t.accent.Render(m.inviteCode) + "\n\n" +
		t.muted.Width(w).Render("Give this code to someone you want to hear from. They paste it into their app to send you a contact request. Railcar copied it to the clipboard, if your terminal allows that.") + "\n\n" +
		t.dim.Render("Press Esc to close.")
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}

// requestView fills the conversation pane for a contact request.
func (m *mainView) requestView(a *App, c Chat, w, h int) string {
	t := a.theme
	who := c.Title
	var lines []string
	if c.Kind == ContactRequest {
		lines = []string{
			t.bold.Render(who + " wants to be your contact."),
			"",
			t.muted.Width(w - 4).Render("If you accept, you start an end-to-end encrypted (E2EE) chat with them. If you decline, they're told."),
			"",
			t.accent.Render("a") + t.muted.Render(" accept   ") + t.accent.Render("d") + t.muted.Render(" decline"),
		}
	} else if c.Stopped != "" {
		lines = []string{
			t.bold.Render(c.Stopped),
			"",
			t.muted.Width(w - 4).Render("You can send them another request in 30 days."),
		}
	} else {
		lines = []string{
			t.bold.Render("Waiting for " + who + " to accept."),
			"",
			t.muted.Width(w - 4).Render("Your chat starts when they accept your request."),
		}
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, strings.Join(lines, "\n"))
}

// acceptRequest and declineRequest answer the open contact request.
func (m *mainView) acceptRequest(a *App) tea.Cmd {
	c, ok := m.activeChat()
	if !ok || c.Kind != ContactRequest {
		return nil
	}
	m.notice = "Accepting " + c.Title
	return do("You and "+c.Title+" are contacts now.", "Railcar couldn't accept the request: ", func(ctx context.Context) error {
		return a.b.AcceptRequest(ctx, c.ID)
	})
}

func (m *mainView) declineRequest(a *App) tea.Cmd {
	c, ok := m.activeChat()
	if !ok || c.Kind != ContactRequest {
		return nil
	}
	return do("You declined "+c.Title+"'s request.", "Railcar couldn't decline the request: ", func(ctx context.Context) error {
		return a.b.DeclineRequest(ctx, c.ID)
	})
}

// lastMessage returns the open chat's latest message that isn't deleted,
// the identity's own when mine is set, or someone else's.
func (m *mainView) lastMessage(mine bool) (Message, bool) {
	msgs := m.messages[m.active]
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Mine == mine && !msgs[i].Deleted && !msgs[i].Unsupported && msgs[i].Status != StatusFailed {
			return msgs[i], true
		}
	}
	return Message{}, false
}

// startReply answers the latest message from someone else.
func (m *mainView) startReply(a *App) tea.Cmd {
	msg, ok := m.lastMessage(false)
	if !ok {
		m.notice = "There's no message to reply to."
		return nil
	}
	m.editing, m.replyTo = "", msg.ID
	m.updatePlaceholder()
	return m.setFocus(a, focusComposer)
}

// startEdit puts the identity's latest message in the composer to change.
func (m *mainView) startEdit(a *App) tea.Cmd {
	msg, ok := m.lastMessage(true)
	if !ok {
		m.notice = "You haven't sent a message here to edit."
		return nil
	}
	m.editing, m.replyTo = msg.ID, ""
	m.composer.SetValue(msg.Text)
	m.updatePlaceholder()
	return m.setFocus(a, focusComposer)
}

// deleteLast deletes the identity's latest message for everyone.
func (m *mainView) deleteLast(a *App) tea.Cmd {
	msg, ok := m.lastMessage(true)
	if !ok {
		m.notice = "You haven't sent a message here to delete."
		return nil
	}
	chat := m.active
	return do("Deleted your last message.", "Railcar couldn't delete the message: ", func(ctx context.Context) error {
		return a.b.Delete(ctx, chat, msg.ID)
	})
}

// reactLast reacts to the latest message from someone else.
func (m *mainView) reactLast(a *App, emoji string) tea.Cmd {
	if emoji == "" {
		emoji = "👍"
	}
	msg, ok := m.lastMessage(false)
	if !ok {
		m.notice = "There's no message to react to."
		return nil
	}
	chat := m.active
	return do("", "Railcar couldn't react: ", func(ctx context.Context) error {
		return a.b.React(ctx, chat, msg.ID, emoji)
	})
}

// cancelMode leaves reply or edit mode. It reports whether there was one.
func (m *mainView) cancelMode() bool {
	if m.replyTo == "" && m.editing == "" {
		return false
	}
	if m.editing != "" {
		m.composer.Reset()
	}
	m.replyTo, m.editing = "", ""
	m.updatePlaceholder()
	return true
}

// updatePlaceholder says what the composer will do.
func (m *mainView) updatePlaceholder() {
	c, ok := m.activeChat()
	if !ok {
		return
	}
	switch {
	case m.editing != "":
		m.composer.Placeholder = "Edit your message (esc cancels)"
	case m.replyTo != "":
		who := "the message"
		for _, msg := range m.messages[m.active] {
			if msg.ID == m.replyTo {
				who = msg.Sender.Name
				if who == "" {
					who = "@" + msg.Sender.Handle
				}
			}
		}
		m.composer.Placeholder = "Reply to " + who + " (esc cancels)"
	default:
		m.composer.Placeholder = "Message " + c.Title
	}
}

// typed is called after a key changes the composer: it sends "typing on"
// at most once a second, and schedules "typing off" (§13.3).
func (m *mainView) typed(a *App) tea.Cmd {
	c, ok := m.activeChat()
	if !ok || c.IsRequest() || a.opts.Demo {
		return nil
	}
	m.typingSeq++
	seq, chat := m.typingSeq, c.ID
	off := tea.Tick(typingIdle, func(time.Time) tea.Msg { return typingOffMsg{chat, seq} })
	now := a.opts.Now()
	if m.typingChat == chat && now.Sub(m.typingSent) < typingEvery {
		return off
	}
	m.typingChat, m.typingSent = chat, now
	return tea.Batch(off, call(func(ctx context.Context) tea.Msg {
		a.b.SetTyping(ctx, chat, true)
		return nil
	}))
}

// stopTyping sends "typing off" if this device said it was typing.
func (m *mainView) stopTyping(a *App) tea.Cmd {
	if m.typingChat == "" {
		return nil
	}
	chat := m.typingChat
	m.typingChat, m.typingSent = "", time.Time{}
	m.typingSeq++
	return call(func(ctx context.Context) tea.Msg {
		a.b.SetTyping(ctx, chat, false)
		return nil
	})
}

// typingLine names who's typing in a chat, or "".
func (m *mainView) typingLine(a *App, chat string) string {
	var names []string
	now := a.opts.Now()
	for _, t := range m.typing[chat] {
		if now.Sub(t.at) < typingExpiry {
			n := t.who.Name
			if n == "" {
				n = "@" + t.who.Handle
			}
			names = append(names, firstName(n))
		}
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0] + " is typing…"
	}
	return strings.Join(names, ", ") + " are typing…"
}

// typist is someone typing in a chat, and when that was last heard.
type typist struct {
	who Member
	at  time.Time
}

// setTyping records a Typing event.
func (m *mainView) setTyping(a *App, e Typing) tea.Cmd {
	if m.typing[e.Chat] == nil {
		m.typing[e.Chat] = map[string]typist{}
	}
	if !e.On {
		delete(m.typing[e.Chat], e.Who.Handle)
		return nil
	}
	m.typing[e.Chat][e.Who.Handle] = typist{e.Who, a.opts.Now()}
	return tea.Tick(typingExpiry, func(time.Time) tea.Msg { return typingExpiredMsg{} })
}
