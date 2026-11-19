package ui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Read receipts (§13.3): the first time someone asks for them in a chat,
// Railcar asks the user once whether to send them there.

// askReceipts opens the read receipt question for the open chat, if
// someone asked there, the user hasn't answered, and this run hasn't
// asked yet.
func (m *mainView) askReceipts() {
	c, ok := m.activeChat()
	// The palette closes once the command that opened the chat returns,
	// and leaves this box open.
	if !ok || c.ReceiptAsk == nil || m.overlay != overlayNone && m.overlay != overlayPalette || m.receiptAsked[c.ID] {
		return
	}
	m.receiptAsked[c.ID] = true
	m.overlay = overlayReceipts
}

// receiptsKey answers the question: y sends read receipts in the chat, n
// doesn't, and Esc leaves it for the next time Railcar starts.
func (m *mainView) receiptsKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "y", "enter":
		m.overlay = overlayNone
		return m.act(a, "read-receipts", "on", "", false)
	case "n":
		m.overlay = overlayNone
		return m.act(a, "read-receipts", "off", "", false)
	case "esc":
		m.overlay = overlayNone
	}
	return nil
}

func (m *mainView) receiptsView(a *App) string {
	t := a.theme
	w := min(a.width-6, 56)
	name := "them"
	if c, ok := m.activeChat(); ok && c.ReceiptAsk != nil {
		name = firstName(c.ReceiptAsk.Name)
		if name == "" {
			name = "@" + c.ReceiptAsk.Handle
		}
	}
	content := t.bold.Render("Send read receipts to "+name+"?") + "\n\n" +
		t.muted.Width(w).Render("They asked to know when you've read their messages. Railcar remembers your answer for this chat. To change it later, type :receipts on or :receipts off.") + "\n\n" +
		t.accent.Render("y") + t.muted.Render(" send   ") + t.accent.Render("n") + t.muted.Render(" don't send   ") + t.accent.Render("esc") + t.muted.Render(" decide later")
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}
