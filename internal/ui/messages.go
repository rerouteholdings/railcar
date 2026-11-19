package ui

import (
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// groupGap is how long a sender can pause and still have the next message
// shown under the same name.
const groupGap = 5 * time.Minute

// renderMessages draws a chat's messages, oldest first, for a pane of the
// given width. Messages are grouped by day, and by sender within a day.
func renderMessages(t *theme, msgs []Message, width int, now time.Time, loc *time.Location) string {
	if len(msgs) == 0 {
		return t.dim.Render("No messages yet. Write the first one below.")
	}
	byID := make(map[string]Message, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
	}
	bodyW := max(10, width-2)
	var b strings.Builder
	var prev *Message
	for i := range msgs {
		m := msgs[i]
		sent := m.Sent.In(loc)
		newDay := prev == nil || daysBetween(prev.Sent.In(loc), sent) != 0
		if newDay {
			if prev != nil {
				b.WriteString("\n")
			}
			b.WriteString(daySeparator(t, dayLabel(sent, now, loc), width) + "\n")
		}
		if m.System {
			if !newDay {
				b.WriteString("\n")
			}
			b.WriteString(systemLine(t, m.Text, width) + "\n")
			prev = &msgs[i]
			continue
		}
		newGroup := newDay || prev.System || prev.Sender.Handle != m.Sender.Handle || m.Sent.Sub(prev.Sent) > groupGap
		if newGroup {
			if !newDay {
				b.WriteString("\n")
			}
			name := t.sender.Render(m.Sender.Name)
			if m.Mine {
				name = t.senderMine.Render("You")
			} else if m.Sender.Name == "" {
				name = t.sender.Render("@" + m.Sender.Handle)
			}
			b.WriteString(name + "  " + t.dim.Render(sent.Format("3:04 PM")) + "\n")
		}
		if m.ReplyTo != "" {
			b.WriteString("  " + replyLine(t, m.ReplyTo, byID, bodyW) + "\n")
		}
		b.WriteString(messageBody(t, m, bodyW) + "\n")
		prev = &msgs[i]
	}
	return strings.TrimRight(b.String(), "\n")
}

// messageBody wraps a message's text and adds its markers: edited, sending,
// or not sent.
func messageBody(t *theme, m Message, width int) string {
	switch {
	case m.Deleted:
		return "  " + t.dim.Render("Message deleted")
	case m.Unsupported:
		return "  " + t.dim.Render("Unsupported message. A newer app may show it.")
	}
	text := lipgloss.NewStyle().Width(width).Render(m.Text)
	var marks []string
	if m.Edited {
		marks = append(marks, t.dim.Render("(edited)"))
	}
	if m.Read {
		marks = append(marks, t.dim.Render("Read"))
	}
	switch m.Status {
	case StatusSending:
		marks = append(marks, t.dim.Render("Sending…"))
	case StatusFailed:
		marks = append(marks, t.danger.Render("! Not sent"))
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	if len(marks) > 0 {
		last := len(lines) - 1
		suffix := strings.Join(marks, " ")
		if lipgloss.Width(lines[last])+1+lipgloss.Width(suffix) <= width {
			lines[last] += " " + suffix
		} else {
			lines = append(lines, suffix)
		}
	}
	for i, l := range lines {
		lines[i] = "  " + t.text.Render(l)
	}
	if len(m.Reactions) > 0 {
		var rs []string
		for _, r := range m.Reactions {
			label := r.Emoji + " " + strconv.Itoa(r.Count)
			if r.Mine {
				rs = append(rs, t.accent.Render(label))
			} else {
				rs = append(rs, t.muted.Render(label))
			}
		}
		lines = append(lines, "  "+strings.Join(rs, "  "))
	}
	return strings.Join(lines, "\n")
}

// systemLine centers a line Railcar shows on its own.
func systemLine(t *theme, text string, width int) string {
	text = ansi.Truncate(text, max(4, width-2), "…")
	pad := max(0, (width-lipgloss.Width(text))/2)
	return strings.Repeat(" ", pad) + t.dim.Render(text)
}

// replyLine names the message being answered, with a short quote.
func replyLine(t *theme, id string, byID map[string]Message, width int) string {
	orig, ok := byID[id]
	if !ok {
		return t.reply.Render("↳ Replying to an earlier message")
	}
	who := orig.Sender.Name
	if orig.Mine {
		who = "you"
	}
	text := orig.Text
	if orig.Deleted {
		text = "a deleted message"
	}
	quote := ansi.Truncate(previewOf(text), max(8, width-len(who)-16), "…")
	return t.reply.Render("↳ Replying to " + who + ": " + quote)
}

// daySeparator is a centered day label between two rules.
func daySeparator(t *theme, label string, width int) string {
	label = " " + label + " "
	side := max(2, (width-lipgloss.Width(label))/2)
	right := max(2, width-side-lipgloss.Width(label))
	return t.daySep.Render(strings.Repeat("─", side) + label + strings.Repeat("─", right))
}

// dayLabel names a day: Today, Yesterday, or the date in full.
func dayLabel(d, now time.Time, loc *time.Location) string {
	d, now = d.In(loc), now.In(loc)
	switch daysBetween(d, now) {
	case 0:
		return "Today"
	case 1:
		return "Yesterday"
	}
	if d.Year() == now.Year() {
		return d.Format("Monday, January 2")
	}
	return d.Format("Monday, January 2, 2006")
}
