package ui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Reports (§17.2): Railcar shows exactly what goes in a report, and who
// can read it, before it sends one.

// reportState is the report waiting for the user to confirm it.
type reportState struct {
	msg    Message
	reason string
}

// openReport asks to report the latest message from someone else in the
// open chat, for reason.
func (m *mainView) openReport(a *App, reason string) tea.Cmd {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		m.notice = "Type :report followed by the reason, such as :report spam"
		return nil
	}
	c, ok := m.activeChat()
	if !ok || c.IsRequest() || c.Kind == VoiceChannel {
		m.notice = "Open a chat first."
		return nil
	}
	msg, ok := m.lastMessage(false)
	if !ok || msg.System {
		m.notice = "There's no message from someone else to report."
		return nil
	}
	m.report = reportState{msg: msg, reason: reason}
	m.overlay = overlayReport
	return nil
}

// reportKey sends the report on Enter, or with f also to the sender's
// homeserver, and cancels on Esc.
func (m *mainView) reportKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	c, _ := m.activeChat()
	switch msg.String() {
	case "enter", "f":
		m.overlay = overlayNone
		forward := ""
		if msg.String() == "f" && !c.IsChannel() {
			forward = "forward"
		}
		r, chat := m.report, m.active
		m.notice = "Working on it"
		return call(func(ctx context.Context) tea.Msg {
			ok, err := a.b.Act(ctx, chat, Action{Kind: "report", Arg: r.reason, Arg2: r.msg.ID, Arg3: forward})
			return actedMsg{ok, err, false}
		})
	case "esc", "n":
		m.overlay = overlayNone
	}
	return nil
}

func (m *mainView) reportView(a *App) string {
	t := a.theme
	w := min(a.width-6, 64)
	c, _ := m.activeChat()
	r := m.report
	who := r.msg.Sender.Name
	if who == "" {
		who = "@" + r.msg.Sender.Handle
	}
	text := r.msg.Text
	if text == "" && len(r.msg.Files) > 0 {
		text = r.msg.Files[0].Name
	}
	readers := "Your homeserver's admins can read this message, and check that " + who + " sent it."
	keys := t.accent.Render("enter") + t.muted.Render(" report   ") + t.accent.Render("f") + t.muted.Render(" report, and send it to their homeserver too   ") + t.accent.Render("esc") + t.muted.Render(" cancel")
	if c.IsChannel() {
		readers = "The report goes to the community's moderators, through the community server, which can read it too. They can check that " + who + " sent it."
		keys = t.accent.Render("enter") + t.muted.Render(" report   ") + t.accent.Render("esc") + t.muted.Render(" cancel")
	}
	quote := ansi.Truncate(strings.ReplaceAll(text, "\n", " "), w-4, "…")
	content := t.bold.Render("Report this message from "+who+"?") + "\n\n" +
		t.text.Render("“"+quote+"”") + "\n" +
		t.muted.Render("Reason: "+r.reason) + "\n\n" +
		t.muted.Width(w).Render(readers+" Nothing else from this chat goes with it.") + "\n\n" + keys
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}
